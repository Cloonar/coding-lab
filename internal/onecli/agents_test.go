package onecli

// EnsureAgent's decision table — the most load-bearing behavior in the
// package. Lab calls it once per lab repo at spawn, concurrently, forever, and
// the per-repo agent mapping the whole gateway design rests on (issue #23) is
// only as good as the "never create a duplicate, never silently return
// nothing" rules asserted here.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// agentsAPI is a scriptable stand-in for OneCLI's /v1/agents endpoints: a
// mutable listing, a programmable POST outcome, programmable PATCH/DELETE
// statuses, and — the part the race cases need — an optional listing
// REPLACEMENT applied at the moment the POST is handled, which is exactly when
// a competing caller would have won.
//
// The PATCH mutates the listing the way the real one does, so a case can list
// again and observe the rename rather than only the request that asked for it.
type agentsAPI struct {
	mu sync.Mutex

	agents       []wireAgent // the current listing
	postStatus   int         // status the POST answers with (0 ⇒ 201 Created)
	postBody     string      // body the POST answers with ("" ⇒ the created agent)
	afterPost    []wireAgent // when non-nil, replaces the listing when a POST arrives
	patchStatus  int         // status the PATCH answers with (0 ⇒ 200 OK)
	deleteStatus int         // status the DELETE answers with (0 ⇒ 204 No Content)
	deleteError  string      // message a failing DELETE answers with

	gets, posts, patches, deletes int
}

func (a *agentsAPI) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		switch r.Method {
		case http.MethodGet:
			a.gets++
			_, _ = io.WriteString(w, jsonEncode(t, map[string]any{"agents": a.agents}))

		case http.MethodPost:
			a.posts++
			if a.afterPost != nil {
				a.agents = a.afterPost
			}
			if a.postStatus >= 300 {
				w.WriteHeader(a.postStatus)
				_, _ = io.WriteString(w, `{"error":"an agent with that identifier already exists"}`)
				return
			}
			if a.postBody != "" {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, a.postBody)
				return
			}
			var req struct {
				Name       string `json:"name"`
				Identifier string `json:"identifier"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decoding POST /agents body: %v", err)
			}
			// The listing row carries the agent's access token; the create
			// ANSWER does not (wire.go point 5) — modeling that asymmetry is
			// what forces the client's create path to resolve by re-listing.
			a.agents = append(a.agents, wireAgent{ID: "ag_created", Name: req.Name, Identifier: req.Identifier, AccessToken: "oc_agent_created"})
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, jsonEncode(t, map[string]string{
				"id": "ag_created", "name": req.Name, "identifier": req.Identifier,
			}))

		case http.MethodPatch:
			a.patches++
			if a.patchStatus >= 300 {
				w.WriteHeader(a.patchStatus)
				_, _ = io.WriteString(w, `{"error":"the rename failed"}`)
				return
			}
			var req struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decoding PATCH /agents/{id} body: %v", err)
			}
			// Upstream renames in place and keeps the identifier (wire.go point
			// 10); so does this, or a second ensure would not see the heal.
			for i := range a.agents {
				if a.agents[i].ID == path.Base(r.URL.Path) {
					a.agents[i].Name = req.Name
				}
			}
			_, _ = io.WriteString(w, `{"success":true}`)

		case http.MethodDelete:
			a.deletes++
			if a.deleteStatus >= 300 {
				w.WriteHeader(a.deleteStatus)
				_, _ = io.WriteString(w, jsonEncode(t, map[string]string{"error": a.deleteError}))
				return
			}
			id := path.Base(r.URL.Path)
			kept := a.agents[:0]
			for _, agent := range a.agents {
				if agent.ID != id {
					kept = append(kept, agent)
				}
			}
			a.agents = kept
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Errorf("unexpected method %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func (a *agentsAPI) counts() (gets, posts, patches, deletes int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gets, a.posts, a.patches, a.deletes
}

func TestEnsureAgent(t *testing.T) {
	// The identifier is the match key (a repo's derived slug); the display name
	// is what an operator reads in OneCLI's dashboard. They are deliberately
	// different strings here so no case can pass by conflating them.
	const (
		identifier = "coding-lab"
		display    = "Coding Lab"
	)

	// The row describes the SERVER's state; the subtest builds the agentsAPI
	// from it (agentsAPI holds a mutex and must never be copied out of a table).
	for _, tc := range []struct {
		name string

		agents      []wireAgent // the project's listing before the call
		postStatus  int         // status the POST answers with (0 ⇒ 201 Created)
		postBody    string      // body the POST answers with ("" ⇒ the created agent)
		afterPost   []wireAgent // listing replacement applied when the POST arrives
		patchStatus int         // status the PATCH answers with (0 ⇒ 200 OK)

		want        Agent
		wantErr     string // substring the error must carry ("" ⇒ success expected)
		wantGets    int
		wantPosts   int
		wantPatches int
	}{
		{
			// The steady state: every touchpoint after the first, on a repo
			// nobody renamed. One list, no write of any kind — and the listing
			// row's access token rides along, because the spawn path reads it
			// from here and never regenerates.
			name: "existing agent with a current name is returned without a write",
			agents: []wireAgent{
				{ID: "ag_1", Name: "Other", Identifier: "other", AccessToken: "oc_agent_other"},
				{ID: "ag_2", Name: display, Identifier: identifier, AccessToken: "oc_agent_2"},
			},
			want:     Agent{ID: "ag_2", Identifier: identifier, Name: display, Token: "oc_agent_2"},
			wantGets: 1,
		},
		{
			// The rename heal: matched by identifier, renamed in place. The agent
			// is reported under the name lab just set, because that is the name
			// upstream now carries.
			name: "existing agent with a stale name is renamed in place",
			agents: []wireAgent{
				{ID: "ag_2", Name: "the old repo name", Identifier: identifier, AccessToken: "oc_agent_2"},
			},
			want:        Agent{ID: "ag_2", Identifier: identifier, Name: display, Token: "oc_agent_2"},
			wantGets:    1,
			wantPatches: 1,
		},
		{
			// Issue #35 decision 7: the rename is cosmetic and the launch path is
			// fail-closed on this call, so a failed PATCH must degrade to the
			// stale name rather than take a spawn down.
			name: "failed rename returns the found agent with its stale name",
			agents: []wireAgent{
				{ID: "ag_2", Name: "the old repo name", Identifier: identifier, AccessToken: "oc_agent_2"},
			},
			patchStatus: http.StatusInternalServerError,
			want:        Agent{ID: "ag_2", Identifier: identifier, Name: "the old repo name", Token: "oc_agent_2"},
			wantGets:    1,
			wantPatches: 1,
		},
		{
			// First touchpoint for a repo: create, then re-list — the create
			// answer carries no access token, so the token can only come from the
			// second GET.
			name:      "missing agent is created and resolved with its token",
			agents:    []wireAgent{{ID: "ag_1", Name: "Other", Identifier: "other"}},
			want:      Agent{ID: "ag_created", Identifier: identifier, Name: display, Token: "oc_agent_created"},
			wantGets:  2,
			wantPosts: 1,
		},
		{
			// Two lab goroutines raced; we lost. The 409 must resolve to the
			// WINNER's agent, with exactly one POST from us and no duplicate.
			name:       "409 conflict re-lists and returns the winner",
			postStatus: http.StatusConflict,
			afterPost:  []wireAgent{{ID: "ag_winner", Name: display, Identifier: identifier, AccessToken: "oc_agent_winner"}},
			want:       Agent{ID: "ag_winner", Identifier: identifier, Name: display, Token: "oc_agent_winner"},
			wantGets:   2,
			wantPosts:  1,
		},
		{
			// A 409 for an identifier that then does not exist breaks the
			// assumption the whole mapping rests on. It is an error — never a
			// zero Agent and a nil error, which would wire a run to no identity
			// at all.
			name:       "409 conflict with the agent still absent is an error",
			postStatus: http.StatusConflict,
			wantErr:    "still absent from the project listing",
			wantGets:   2,
			wantPosts:  1,
		},
		{
			// The create answer's body is ignored entirely (only the re-list
			// matters), so an unreadable answer changes nothing.
			name:      "create answer body is ignored in favor of the re-list",
			postBody:  `{"created":true}`,
			afterPost: []wireAgent{{ID: "ag_resolved", Name: display, Identifier: identifier, AccessToken: "oc_agent_resolved"}},
			want:      Agent{ID: "ag_resolved", Identifier: identifier, Name: display, Token: "oc_agent_resolved"},
			wantGets:  2,
			wantPosts: 1,
		},
		{
			// …but a create whose agent then never appears in the listing is a
			// broken assumption and must be loud, exactly like the 409 variant.
			name:      "create with the agent still absent from the listing is an error",
			postBody:  `{"created":true}`,
			wantErr:   "still absent from the project listing",
			wantGets:  2,
			wantPosts: 1,
		},
		{
			// Any non-409 failure is the caller's problem, surfaced as-is: a 403
			// must not be mistaken for a race and retried into a second write.
			name:       "non-conflict create failure is surfaced",
			postStatus: http.StatusForbidden,
			wantErr:    "unexpected status 403",
			wantGets:   1,
			wantPosts:  1,
		},
		{
			// The identifier is upstream's unique key, so the match is exact:
			// a row differing only in case is a DIFFERENT agent, and treating it
			// as this repo's would hand one repo another's credentials.
			name:      "identifier match is case-sensitive",
			agents:    []wireAgent{{ID: "ag_1", Name: display, Identifier: "Coding-Lab"}},
			want:      Agent{ID: "ag_created", Identifier: identifier, Name: display, Token: "oc_agent_created"},
			wantGets:  2,
			wantPosts: 1,
		},
		{
			// The inversion's other half: a row carrying the right NAME and the
			// wrong identifier is not this repo's agent, however convincing it
			// looks in the dashboard.
			name:      "a matching name with a different identifier is not a match",
			agents:    []wireAgent{{ID: "ag_1", Name: display, Identifier: "some-other-repo", AccessToken: "oc_agent_other"}},
			want:      Agent{ID: "ag_created", Identifier: identifier, Name: display, Token: "oc_agent_created"},
			wantGets:  2,
			wantPosts: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &agentsAPI{
				agents:      tc.agents,
				postStatus:  tc.postStatus,
				postBody:    tc.postBody,
				afterPost:   tc.afterPost,
				patchStatus: tc.patchStatus,
			}
			s := newStub(t, api.handler(t))
			c := newTestClient(t, s.URL)

			got, err := c.EnsureAgent(context.Background(), identifier, display)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("EnsureAgent: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("EnsureAgent succeeded with %#v, want an error containing %q", got, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
			if tc.wantErr == "" && got != tc.want {
				t.Errorf("agent = %#v, want %#v", got, tc.want)
			}
			if tc.wantErr != "" && got != (Agent{}) {
				t.Errorf("agent = %#v on the error path, want the zero Agent", got)
			}

			gets, posts, patches, deletes := api.counts()
			if gets != tc.wantGets {
				t.Errorf("GET /agents count = %d, want %d", gets, tc.wantGets)
			}
			if posts != tc.wantPosts {
				t.Errorf("POST /agents count = %d, want %d", posts, tc.wantPosts)
			}
			if patches != tc.wantPatches {
				t.Errorf("PATCH /agents/{id} count = %d, want %d", patches, tc.wantPatches)
			}
			if deletes != 0 {
				t.Errorf("EnsureAgent issued %d DELETEs; it must never delete an agent", deletes)
			}
		})
	}
}

// TestEnsureAgentCreateBody pins the create body verified against a real
// OneCLI build: {"name": …, "identifier": …} and nothing else. A bare
// {"name": …} body is 400-rejected with "Invalid input: expected string,
// received undefined", which is the regression this test exists to catch, and
// since issue #35 the two fields are independent inputs rather than one string
// and its slug — so the case sends a display name that could not possibly
// derive the identifier being sent with it.
func TestEnsureAgentCreateBody(t *testing.T) {
	const (
		identifier = "repo-0123456789abcdef0123456789abcdef"
		display    = "Coding Lab"
	)

	api := &agentsAPI{}
	s := newStub(t, api.handler(t))
	c := newTestClient(t, s.URL)

	if _, err := c.EnsureAgent(context.Background(), identifier, display); err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	reqs := s.requests()
	if len(reqs) != 3 {
		t.Fatalf("stub saw %d requests, want 3 (list, create, token-resolving re-list): %+v", len(reqs), reqs)
	}
	post := reqs[1]
	if post.Method != http.MethodPost || post.Path != "/v1/agents" {
		t.Errorf("create request = %s %s, want POST /v1/agents", post.Method, post.Path)
	}
	if got := post.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	want := `{"name":"` + display + `","identifier":"` + identifier + `"}`
	if got := strings.TrimSpace(post.Body); got != want {
		t.Errorf("create body = %s, want %s", got, want)
	}
}

// TestEnsureAgentAdoptsALegacyAgent is the zero-migration claim of issue #35,
// asserted end to end: an agent created by the OLD code carries the repo store
// ID as its name and the byte-identical derived slug as its identifier, so the
// new code matches it by identifier and renames it in place. Nothing is
// created, nothing is deleted, and the grants and access token that hang off
// that agent id are therefore untouched — which is the entire reason the
// inversion was possible without a migration step.
func TestEnsureAgentAdoptsALegacyAgent(t *testing.T) {
	const (
		storeID = "repo_0123456789abcdef0123456789abcdef"
		display = "coding-lab"
	)
	identifier := AgentIdentifier(storeID)

	api := &agentsAPI{agents: []wireAgent{
		{ID: "ag_legacy", Name: storeID, Identifier: identifier, AccessToken: "oc_agent_legacy"},
	}}
	s := newStub(t, api.handler(t))
	c := newTestClient(t, s.URL)

	got, err := c.EnsureAgent(context.Background(), identifier, display)
	if err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	want := Agent{ID: "ag_legacy", Identifier: identifier, Name: display, Token: "oc_agent_legacy"}
	if got != want {
		t.Errorf("agent = %#v, want %#v", got, want)
	}

	reqs := s.requests()
	if len(reqs) != 2 {
		t.Fatalf("stub saw %d requests, want 2 (list, rename): %+v", len(reqs), reqs)
	}
	patch := reqs[1]
	if patch.Method != http.MethodPatch || patch.Path != "/v1/agents/ag_legacy" {
		t.Errorf("rename request = %s %s, want PATCH /v1/agents/ag_legacy", patch.Method, patch.Path)
	}
	// The name and NOTHING else: the identifier is immutable upstream, and a
	// body that carried one would be lab trying to move its own match key.
	if want := `{"name":"` + display + `"}`; strings.TrimSpace(patch.Body) != want {
		t.Errorf("rename body = %s, want %s", patch.Body, want)
	}
}

// TestEnsureAgentDistinguishesReposSharingADisplayName is the regression the
// inversion could otherwise introduce: two lab repos may legitimately carry the
// same name (different owners, different forges), and the identifier is what
// keeps them apart. If either one ever resolved to the other's agent, one repo
// would be spawning with another repo's gateway credential.
func TestEnsureAgentDistinguishesReposSharingADisplayName(t *testing.T) {
	const display = "coding-lab"
	var (
		first  = AgentIdentifier("repo_0123456789abcdef0123456789abcdef")
		second = AgentIdentifier("repo_fedcba9876543210fedcba9876543210")
	)

	// Two agents the dashboard renders identically; only the identifier tells
	// them apart, and each carries its own gateway credential.
	api := &agentsAPI{agents: []wireAgent{
		{ID: "ag_first", Name: display, Identifier: first, AccessToken: "oc_agent_first"},
		{ID: "ag_second", Name: display, Identifier: second, AccessToken: "oc_agent_second"},
	}}
	s := newStub(t, api.handler(t))
	c := newTestClient(t, s.URL)

	a, err := c.EnsureAgent(context.Background(), first, display)
	if err != nil {
		t.Fatalf("EnsureAgent(%q): %v", first, err)
	}
	b, err := c.EnsureAgent(context.Background(), second, display)
	if err != nil {
		t.Fatalf("EnsureAgent(%q): %v", second, err)
	}
	if want := (Agent{ID: "ag_first", Identifier: first, Name: display, Token: "oc_agent_first"}); a != want {
		t.Errorf("agent for %q = %#v, want %#v", first, a, want)
	}
	if want := (Agent{ID: "ag_second", Identifier: second, Name: display, Token: "oc_agent_second"}); b != want {
		t.Errorf("agent for %q = %#v, want %#v", second, b, want)
	}
	// A name-based match would have found the same first row twice, created
	// nothing, and handed the second repo the first one's token — so the write
	// counts are part of the assertion, not decoration.
	if _, posts, patches, _ := api.counts(); posts != 0 || patches != 0 {
		t.Errorf("POSTs = %d, PATCHes = %d; want neither — both agents already exist under their own identifiers", posts, patches)
	}
}

// TestEnsureAgentRejectsBadInput: every input rule is enforced LOCALLY, before
// a request exists. A caller that forgot to derive the identifier must fail
// where its mistake is (an opaque upstream 400 explains nothing, and an agent
// created under an underived identifier would be invisible to every later
// ensure), and a blank display name is a 400 upstream anyway.
func TestEnsureAgentRejectsBadInput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		identifier string
		display    string
		wantErr    string
	}{
		{"empty identifier", "", "coding-lab", "identifier must not be empty"},
		// The store ID itself, handed over underived — the mistake the rule
		// exists to catch, since its underscores are illegal in a slug.
		{"underived identifier", "Repo_ABC", "coding-lab", "identifier slug"},
		{"unsluggable identifier", "___", "coding-lab", "identifier slug"},
		{"empty display name", "coding-lab", "", "display name"},
		{"blank display name", "coding-lab", "   ", "display name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &agentsAPI{}
			s := newStub(t, api.handler(t))
			c := newTestClient(t, s.URL)

			_, err := c.EnsureAgent(context.Background(), tc.identifier, tc.display)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
			if reqs := s.requests(); len(reqs) != 0 {
				t.Errorf("%d requests were issued for input that cannot be valid: %+v", len(reqs), reqs)
			}
		})
	}
}

// TestAgentIdentifier pins the slug derivation: every output must match
// OneCLI's ^[a-z0-9][a-z0-9-]{0,49}$ and be deterministic in its input,
// because it is now the MATCH KEY every lab call site derives independently —
// two call sites disagreeing about a repo's identifier is two agents for one
// repo — and because EnsureAgent's 409-race resolution relies on "same store
// ID ⇒ same identifier".
func TestAgentIdentifier(t *testing.T) {
	slug := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`)
	for _, tc := range []struct {
		in   string
		want string
	}{
		// The only shape lab actually derives from: a repo store ID, whose
		// underscore is the character that makes the raw ID an illegal slug.
		{"repo_0123456789abcdef0123456789abcdef", "repo-0123456789abcdef0123456789abcdef"},
		{"coding-lab", "coding-lab"},
		{"Coding Lab", "coding-lab"},
		{"__repo", "repo"},                                 // a slug must start alphanumeric
		{"grüße", "gr--e"},                                 // non-ASCII maps per rune, not per byte
		{strings.Repeat("a", 60), strings.Repeat("a", 50)}, // capped at the slug maximum
	} {
		got := AgentIdentifier(tc.in)
		if got != tc.want {
			t.Errorf("AgentIdentifier(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !slug.MatchString(got) {
			t.Errorf("AgentIdentifier(%q) = %q, which is not a valid OneCLI identifier slug", tc.in, got)
		}
		// Idempotence is what lets EnsureAgent validate its input by
		// re-deriving it: a derived identifier must survive a second pass.
		if again := AgentIdentifier(got); again != got {
			t.Errorf("AgentIdentifier(%q) = %q, want the already-derived slug unchanged", got, again)
		}
	}
}

// TestDeleteAgent: the repo-delete path. Resolution is by identifier through
// the same matcher the ensure path uses, so a delete can never address an agent
// an ensure would not have addressed.
func TestDeleteAgent(t *testing.T) {
	const identifier = "repo-0123456789abcdef0123456789abcdef"

	t.Run("existing agent is deleted", func(t *testing.T) {
		api := &agentsAPI{agents: []wireAgent{
			{ID: "ag_other", Name: "Other", Identifier: "other", AccessToken: "oc_agent_other"},
			{ID: "ag_repo", Name: "Coding Lab", Identifier: identifier, AccessToken: "oc_agent_repo"},
		}}
		s := newStub(t, api.handler(t))
		c := newTestClient(t, s.URL)

		deleted, err := c.DeleteAgent(context.Background(), identifier)
		if err != nil || !deleted {
			t.Fatalf("DeleteAgent = (%t, %v), want (true, nil)", deleted, err)
		}
		reqs := s.requests()
		if len(reqs) != 2 {
			t.Fatalf("stub saw %d requests, want 2 (list, delete): %+v", len(reqs), reqs)
		}
		// Addressed by the UPSTREAM ID resolved from the listing, never by the
		// identifier: the identifier is what lab matches on, not what upstream
		// routes on.
		if del := reqs[1]; del.Method != http.MethodDelete || del.Path != "/v1/agents/ag_repo" {
			t.Errorf("delete request = %s %s, want DELETE /v1/agents/ag_repo", del.Method, del.Path)
		}
	})

	t.Run("absent agent is not an error and not a request", func(t *testing.T) {
		// The ordinary case for a repo created before OneCLI was configured:
		// there is no agent, the delete has nothing to do, and reporting an
		// error here would turn a clean repo deletion into a warning forever.
		api := &agentsAPI{agents: []wireAgent{{ID: "ag_other", Name: "Other", Identifier: "other"}}}
		s := newStub(t, api.handler(t))
		c := newTestClient(t, s.URL)

		deleted, err := c.DeleteAgent(context.Background(), identifier)
		if err != nil || deleted {
			t.Fatalf("DeleteAgent = (%t, %v), want (false, nil)", deleted, err)
		}
		for _, req := range s.requests() {
			if req.Method == http.MethodDelete {
				t.Errorf("a DELETE was issued for an identifier no agent carries: %+v", req)
			}
		}
	})

	t.Run("the default-agent guardrail is surfaced", func(t *testing.T) {
		// Lab-created agents are never the project default, so this 400 means
		// the identifier resolved to an agent lab did not create. The caller
		// reports it; nothing here tries to route around it.
		api := &agentsAPI{
			agents:       []wireAgent{{ID: "ag_repo", Name: "Coding Lab", Identifier: identifier}},
			deleteStatus: http.StatusBadRequest,
			deleteError:  "Cannot delete the default agent",
		}
		s := newStub(t, api.handler(t))
		c := newTestClient(t, s.URL)

		deleted, err := c.DeleteAgent(context.Background(), identifier)
		if deleted {
			t.Errorf("DeleteAgent reported a deletion after a %d", http.StatusBadRequest)
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
			t.Fatalf("error = %v, want an *APIError with status 400", err)
		}
		if !strings.Contains(apiErr.Error(), "Cannot delete the default agent") {
			t.Errorf("error = %q, want it to carry the server's guardrail message", apiErr)
		}
	})

	t.Run("an empty identifier is refused without a request", func(t *testing.T) {
		api := &agentsAPI{agents: []wireAgent{{ID: "ag_nameless", Name: "Nameless"}}}
		s := newStub(t, api.handler(t))
		c := newTestClient(t, s.URL)

		if _, err := c.DeleteAgent(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "identifier must not be empty") {
			t.Fatalf("error = %v, want the empty-identifier refusal", err)
		}
		if reqs := s.requests(); len(reqs) != 0 {
			t.Errorf("%d requests were issued for an empty identifier: %+v", len(reqs), reqs)
		}
	})
}

// TestEnsureAgentListFailureIsNotAWrite: if the first list fails there is no
// evidence about whether the agent exists, so the only safe move is to fail —
// creating on an unknown listing is how duplicates get made.
func TestEnsureAgentListFailureIsNotAWrite(t *testing.T) {
	s := newStub(t, jsonHandler(http.StatusInternalServerError, `{"error":"database down"}`))
	c := newTestClient(t, s.URL)

	_, err := c.EnsureAgent(context.Background(), "coding-lab", "Coding Lab")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Fatalf("error = %v, want an *APIError with status 500", err)
	}
	for _, req := range s.requests() {
		if req.Method == http.MethodPost {
			t.Errorf("a POST was issued after a failed listing: %+v", req)
		}
	}
}

// TestEnsureAgentConcurrentCreatesExactlyOne is the concurrency assertion the
// whole mapping rests on: many goroutines ensuring the same identifier against
// one OneCLI project must converge on one agent. The stub enforces uniqueness
// the way a real server would — the first POST wins, every later one is a 409
// — so a client that did not re-list after a conflict would fail here rather
// than quietly returning nothing.
//
// Every caller passes the same display name, which is also the point: the
// winner creates the agent already carrying it, so no goroutine finds a stale
// name and none of them issues a rename. A PATCH here would mean the heal
// fires on a name that is already current, i.e. a write on every concurrent
// spawn of every repo — hence the stub's refusal to answer one.
func TestEnsureAgentConcurrentCreatesExactlyOne(t *testing.T) {
	const (
		identifier = "coding-lab"
		display    = "Coding Lab"
		n          = 12
	)

	var (
		mu      sync.Mutex
		agents  []wireAgent
		created int
	)
	s := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, jsonEncode(t, map[string]any{"agents": agents}))
		case http.MethodPost:
			if len(agents) > 0 {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"error":"an agent with that identifier already exists"}`)
				return
			}
			created++
			// The listing row carries the token; the create answer does not.
			agents = append(agents, wireAgent{ID: "ag_only", Name: display, Identifier: identifier, AccessToken: "oc_agent_only"})
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, jsonEncode(t, map[string]string{"id": "ag_only", "name": display, "identifier": identifier}))
		default:
			t.Errorf("unexpected %s %s: the steady state is a list, and the create is the only write", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	c := newTestClient(t, s.URL)

	results := make(chan Agent, n)
	errs := make(chan error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			agent, err := c.EnsureAgent(context.Background(), identifier, display)
			if err != nil {
				errs <- err
				return
			}
			results <- agent
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("EnsureAgent: %v", err)
	}
	got := 0
	for agent := range results {
		got++
		if (agent != Agent{ID: "ag_only", Identifier: identifier, Name: display, Token: "oc_agent_only"}) {
			t.Errorf("agent = %#v, want the single created agent with its stable token", agent)
		}
	}
	if got != n {
		t.Errorf("%d of %d callers got an agent, want all of them", got, n)
	}
	mu.Lock()
	defer mu.Unlock()
	if created != 1 {
		t.Errorf("the server created %d agents, want exactly 1", created)
	}
}

// TestHealthDecodeFailureIsLoud: a health endpoint answering something that is
// not the assumed object must not read as healthy.
func TestHealthDecodeFailureIsLoud(t *testing.T) {
	for _, body := range []string{"", "<html>nginx</html>"} {
		s := newStub(t, jsonHandler(http.StatusOK, body))
		c := newTestClient(t, s.URL)
		if got, err := c.Health(context.Background()); err == nil {
			t.Errorf("Health(%q) succeeded with %#v, want an error", body, got)
		}
	}
}
