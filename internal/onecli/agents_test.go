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
	"regexp"
	"strings"
	"sync"
	"testing"
)

// agentsAPI is a scriptable stand-in for OneCLI's /v1/agents endpoint: a
// mutable listing, a programmable POST outcome, and — the part the race cases
// need — an optional listing REPLACEMENT applied at the moment the POST is
// handled, which is exactly when a competing caller would have won.
type agentsAPI struct {
	mu sync.Mutex

	agents     []wireAgent // the current listing
	postStatus int         // status the POST answers with (0 ⇒ 201 Created)
	postBody   string      // body the POST answers with ("" ⇒ the created agent)
	afterPost  []wireAgent // when non-nil, replaces the listing when a POST arrives

	gets, posts int
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
				_, _ = io.WriteString(w, `{"error":"an agent with that name already exists"}`)
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
			a.agents = append(a.agents, wireAgent{ID: "ag_created", Name: req.Name, AccessToken: "oc_agent_created"})
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, jsonEncode(t, map[string]string{
				"id": "ag_created", "name": req.Name, "identifier": req.Identifier,
			}))

		default:
			t.Errorf("unexpected method %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func (a *agentsAPI) counts() (gets, posts int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gets, a.posts
}

func TestEnsureAgent(t *testing.T) {
	const name = "coding-lab"

	// The row describes the SERVER's state; the subtest builds the agentsAPI
	// from it (agentsAPI holds a mutex and must never be copied out of a table).
	for _, tc := range []struct {
		name string

		agents     []wireAgent // the project's listing before the call
		postStatus int         // status the POST answers with (0 ⇒ 201 Created)
		postBody   string      // body the POST answers with ("" ⇒ the created agent)
		afterPost  []wireAgent // listing replacement applied when the POST arrives

		want      Agent
		wantErr   string // substring the error must carry ("" ⇒ success expected)
		wantGets  int
		wantPosts int
	}{
		{
			// The steady state: every spawn after the first. One list, no write —
			// and the listing row's access token rides along, because the spawn
			// path reads it from here and never regenerates.
			name:      "existing agent is returned without a POST",
			agents:    []wireAgent{{ID: "ag_1", Name: "other", AccessToken: "oc_agent_other"}, {ID: "ag_2", Name: name, AccessToken: "oc_agent_2"}},
			want:      Agent{ID: "ag_2", Name: name, Token: "oc_agent_2"},
			wantGets:  1,
			wantPosts: 0,
		},
		{
			// First spawn for a repo: create, then re-list — the create answer
			// carries no access token, so the token can only come from the
			// second GET.
			name:      "missing agent is created and resolved with its token",
			agents:    []wireAgent{{ID: "ag_1", Name: "other"}},
			want:      Agent{ID: "ag_created", Name: name, Token: "oc_agent_created"},
			wantGets:  2,
			wantPosts: 1,
		},
		{
			// Two lab goroutines raced; we lost. The 409 must resolve to the
			// WINNER's agent, with exactly one POST from us and no duplicate.
			name:       "409 conflict re-lists and returns the winner",
			postStatus: http.StatusConflict,
			afterPost:  []wireAgent{{ID: "ag_winner", Name: name, AccessToken: "oc_agent_winner"}},
			want:       Agent{ID: "ag_winner", Name: name, Token: "oc_agent_winner"},
			wantGets:   2,
			wantPosts:  1,
		},
		{
			// A 409 for a name that then does not exist breaks the assumption the
			// whole mapping rests on. It is an error — never a zero Agent and a
			// nil error, which would wire a run to no identity at all.
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
			afterPost: []wireAgent{{ID: "ag_resolved", Name: name, AccessToken: "oc_agent_resolved"}},
			want:      Agent{ID: "ag_resolved", Name: name, Token: "oc_agent_resolved"},
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
			// Case matters: lab repo names differing only in case are different
			// repos, and folding them together would hand one repo another's
			// credentials.
			name:      "name match is case-sensitive",
			agents:    []wireAgent{{ID: "ag_1", Name: "Coding-Lab"}},
			want:      Agent{ID: "ag_created", Name: name, Token: "oc_agent_created"},
			wantGets:  2,
			wantPosts: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &agentsAPI{
				agents:     tc.agents,
				postStatus: tc.postStatus,
				postBody:   tc.postBody,
				afterPost:  tc.afterPost,
			}
			s := newStub(t, api.handler(t))
			c := newTestClient(t, s.URL)

			got, err := c.EnsureAgent(context.Background(), name)
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

			gets, posts := api.counts()
			if gets != tc.wantGets {
				t.Errorf("GET /agents count = %d, want %d", gets, tc.wantGets)
			}
			if posts != tc.wantPosts {
				t.Errorf("POST /agents count = %d, want %d", posts, tc.wantPosts)
			}
		})
	}
}

// TestEnsureAgentCreateBody pins wire.go assumption 4 — the one assumption
// verified against a real OneCLI build: the create body is {"name": …,
// "identifier": …} and nothing else. The name is sent VERBATIM (lab's agent
// identity IS the name, and listing resolution matches it exactly); the
// identifier is the derived slug OneCLI's validation requires — a bare
// {"name": …} body is 400-rejected with "Invalid input: expected string,
// received undefined", which is the regression this test exists to catch. The
// underscore in lab's repo_<32 hex> names is illegal in the slug, so the two
// fields genuinely differ.
func TestEnsureAgentCreateBody(t *testing.T) {
	const name = "repo_0123456789abcdef0123456789abcdef"

	api := &agentsAPI{}
	s := newStub(t, api.handler(t))
	c := newTestClient(t, s.URL)

	if _, err := c.EnsureAgent(context.Background(), name); err != nil {
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
	want := `{"name":"` + name + `","identifier":"repo-0123456789abcdef0123456789abcdef"}`
	if got := strings.TrimSpace(post.Body); got != want {
		t.Errorf("create body = %s, want %s", got, want)
	}
}

// TestAgentIdentifier pins the slug derivation: every output must match
// OneCLI's ^[a-z0-9][a-z0-9-]{0,49}$ and be deterministic in the name, because
// EnsureAgent's 409-race resolution relies on "same name ⇒ same identifier".
func TestAgentIdentifier(t *testing.T) {
	slug := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`)
	for _, tc := range []struct {
		name string
		want string
	}{
		// The only shape lab actually sends: a repo store ID, whose underscore
		// is the character that made sending the name verbatim impossible.
		{"repo_0123456789abcdef0123456789abcdef", "repo-0123456789abcdef0123456789abcdef"},
		{"coding-lab", "coding-lab"},
		{"Coding Lab", "coding-lab"},
		{"__repo", "repo"},                                 // a slug must start alphanumeric
		{"grüße", "gr--e"},                                 // non-ASCII maps per rune, not per byte
		{strings.Repeat("a", 60), strings.Repeat("a", 50)}, // capped at the slug maximum
	} {
		got := agentIdentifier(tc.name)
		if got != tc.want {
			t.Errorf("agentIdentifier(%q) = %q, want %q", tc.name, got, tc.want)
		}
		if !slug.MatchString(got) {
			t.Errorf("agentIdentifier(%q) = %q, which is not a valid OneCLI identifier slug", tc.name, got)
		}
	}
}

// TestEnsureAgentRefusesAnUnsluggableName: a name deriving an empty slug must
// fail locally and loudly, before OneCLI turns it into an opaque 400 — and
// must not issue the doomed POST.
func TestEnsureAgentRefusesAnUnsluggableName(t *testing.T) {
	api := &agentsAPI{}
	s := newStub(t, api.handler(t))
	c := newTestClient(t, s.URL)

	_, err := c.EnsureAgent(context.Background(), "___")
	if err == nil || !strings.Contains(err.Error(), "identifier slug") {
		t.Fatalf("error = %v, want the identifier-slug refusal", err)
	}
	for _, req := range s.requests() {
		if req.Method == http.MethodPost {
			t.Errorf("a POST was issued for an unsluggable name: %+v", req)
		}
	}
}

// TestEnsureAgentListFailureIsNotAWrite: if the first list fails there is no
// evidence about whether the agent exists, so the only safe move is to fail —
// creating on an unknown listing is how duplicates get made.
func TestEnsureAgentListFailureIsNotAWrite(t *testing.T) {
	s := newStub(t, jsonHandler(http.StatusInternalServerError, `{"error":"database down"}`))
	c := newTestClient(t, s.URL)

	_, err := c.EnsureAgent(context.Background(), "coding-lab")
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
// whole mapping rests on: many goroutines ensuring the same name against one
// OneCLI project must converge on one agent. The stub enforces uniqueness the
// way a real server would — the first POST wins, every later one is a 409 —
// so a client that did not re-list after a conflict would fail here rather
// than quietly returning nothing.
func TestEnsureAgentConcurrentCreatesExactlyOne(t *testing.T) {
	const (
		name = "coding-lab"
		n    = 12
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
			agents = append(agents, wireAgent{ID: "ag_only", Name: name, AccessToken: "oc_agent_only"})
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, jsonEncode(t, map[string]string{"id": "ag_only", "name": name}))
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
			agent, err := c.EnsureAgent(context.Background(), name)
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
		if (agent != Agent{ID: "ag_only", Name: name, Token: "oc_agent_only"}) {
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
