package httpapi

// M4 issue-surface suite (httptest against the real store on a t.TempDir
// sqlite DB). The builtin binding runs end-to-end through the real registry +
// builtin tracker + store. The forge binding runs through the REAL registry —
// forge-kind gate, credential load, vault decryption, RepoPath — with only the
// Forgejo REST client stubbed at the injected ForgejoFactory seam (tests
// cannot reach a real Forgejo; the REST client has its own httptest suite in
// internal/tracker/forgejo).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/builtin"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// newTrackerServer builds a test server with the tracker registry mounted:
// real store, real vault, real builtin factory. forge/github stub the Forgejo
// and GitHub REST clients; a nil factory panics if ever invoked (a binding a
// given test does not exercise).
func newTrackerServer(t *testing.T, forge tracker.ForgejoFactory, github tracker.GitHubFactory) (*testServer, *vault.Vault) {
	t.Helper()
	vlt, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	if forge == nil {
		forge = func(tracker.ForgejoConfig) tracker.Tracker {
			panic("forgejo factory invoked in a test that did not expect it")
		}
	}
	if github == nil {
		github = func(tracker.GitHubConfig) tracker.Tracker {
			panic("github factory invoked in a test that did not expect it")
		}
	}
	x := newTestServer(t, func(o *Options) {
		o.Tracker = tracker.NewRegistry(o.Store, vlt, nil, builtin.New, forge, github)
	})
	x.setup("op", "password123")
	return x, vlt
}

// seedTrackerRepo inserts a repo row directly (the clone lifecycle is the
// repos suite's business); CreateRepo seeds the five triage labels.
func seedTrackerRepo(t *testing.T, x *testServer, name string, mod func(*store.Repo)) store.Repo {
	t.Helper()
	repo := store.Repo{
		ID:             ids.NewID("repo"),
		Name:           name,
		RemoteURL:      "git@git.cloonar.com:Cloonar/" + name + ".git",
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none",
		DefaultBranch:    "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
	}
	if mod != nil {
		mod(&repo)
	}
	created, err := x.st.CreateRepo(context.Background(), repo)
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return created
}

// issuesOf pulls the issues array out of a decoded list response.
func issuesOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["issues"].([]any)
	if !ok {
		t.Fatalf("no issues array in %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

func TestBuiltinIssueLifecycle(t *testing.T) {
	x, _ := newTrackerServer(t, nil, nil)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID

	// Create: numbers run 1, 2, … per repo.
	resp := x.do("POST", base+"/issues", map[string]any{"title": "first", "body": "b1"}, h)
	wantStatus(t, resp, http.StatusCreated)
	one := decodeBody(t, resp)
	if one["number"] != float64(1) || one["state"] != "open" || one["title"] != "first" {
		t.Fatalf("first issue = %v", one)
	}
	if labels := one["labels"].([]any); len(labels) != 0 {
		t.Fatalf("fresh issue labels = %v", labels)
	}
	if comments := one["comments"].([]any); len(comments) != 0 {
		t.Fatalf("fresh issue comments = %v", comments)
	}

	resp = x.do("POST", base+"/issues", map[string]any{"title": "second", "labels": []string{"needs-triage"}}, h)
	wantStatus(t, resp, http.StatusCreated)
	two := decodeBody(t, resp)
	if two["number"] != float64(2) {
		t.Fatalf("second issue number = %v", two["number"])
	}
	if labels := two["labels"].([]any); len(labels) != 1 || labels[0] != "needs-triage" {
		t.Fatalf("second issue labels = %v", labels)
	}

	// List (state defaults to open): newest number first, comment counts.
	resp = x.do("GET", base+"/issues", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	if list["binding"] != "builtin" {
		t.Fatalf("binding = %v", list["binding"])
	}
	items := issuesOf(t, list)
	if len(items) != 2 || items[0]["number"] != float64(2) || items[1]["number"] != float64(1) {
		t.Fatalf("open list = %v", items)
	}
	if items[0]["comments_count"] != float64(0) {
		t.Fatalf("comments_count = %v", items[0]["comments_count"])
	}

	// Label filter on the list view.
	resp = x.do("GET", base+"/issues?label=needs-triage", nil, nil)
	filtered := issuesOf(t, decodeBody(t, resp))
	if len(filtered) != 1 || filtered[0]["number"] != float64(2) {
		t.Fatalf("label-filtered list = %v", filtered)
	}

	// Comment as the operator.
	resp = x.do("POST", base+"/issues/1/comments", map[string]any{"body": "hi"}, h)
	wantStatus(t, resp, http.StatusCreated)
	if c := decodeBody(t, resp); c["author"] != "operator" || c["body"] != "hi" || c["created_at"] == "" {
		t.Fatalf("comment = %v", c)
	}

	// The count shows in the list, the thread in the detail.
	resp = x.do("GET", base+"/issues", nil, nil)
	for _, it := range issuesOf(t, decodeBody(t, resp)) {
		if it["number"] == float64(1) && it["comments_count"] != float64(1) {
			t.Fatalf("comments_count after comment = %v", it["comments_count"])
		}
	}
	resp = x.do("GET", base+"/issues/1", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	detail := decodeBody(t, resp)
	comments := detail["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("detail comments = %v", comments)
	}
	if c := comments[0].(map[string]any); c["author"] != "operator" || c["body"] != "hi" {
		t.Fatalf("detail comment = %v", c)
	}

	// Close: closed_at stamped (store witness — the pinned JSON carries no
	// closed_at key).
	resp = x.do("PATCH", base+"/issues/1", map[string]any{"state": "closed"}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["state"] != "closed" {
		t.Fatalf("patched state = %v", got["state"])
	}
	stored, err := x.st.IssueByRepoNumber(context.Background(), repo.ID, 1)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if stored.ClosedAt == nil {
		t.Fatal("closed_at not stamped on close")
	}

	// State filters.
	resp = x.do("GET", base+"/issues", nil, nil)
	if items := issuesOf(t, decodeBody(t, resp)); len(items) != 1 || items[0]["number"] != float64(2) {
		t.Fatalf("open list after close = %v", items)
	}
	resp = x.do("GET", base+"/issues?state=closed", nil, nil)
	if items := issuesOf(t, decodeBody(t, resp)); len(items) != 1 || items[0]["number"] != float64(1) {
		t.Fatalf("closed list = %v", items)
	}
	resp = x.do("GET", base+"/issues?state=all", nil, nil)
	if items := issuesOf(t, decodeBody(t, resp)); len(items) != 2 {
		t.Fatalf("all list = %v", items)
	}

	// Reopen clears closed_at.
	resp = x.do("PATCH", base+"/issues/1", map[string]any{"state": "open"}, h)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
	stored, err = x.st.IssueByRepoNumber(context.Background(), repo.ID, 1)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if stored.ClosedAt != nil {
		t.Fatal("closed_at not cleared on reopen")
	}

	// Title/body patch.
	resp = x.do("PATCH", base+"/issues/1", map[string]any{"title": "renamed", "body": "nb"}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["title"] != "renamed" || got["body"] != "nb" {
		t.Fatalf("patched issue = %v", got)
	}

	// Label replace (PUT semantics; names come back sorted from the store).
	resp = x.do("PUT", base+"/issues/1/labels", map[string]any{"labels": []string{"ready-for-agent", "needs-info"}}, h)
	wantStatus(t, resp, http.StatusOK)
	got := decodeBody(t, resp)
	lbls := got["labels"].([]any)
	if len(lbls) != 2 || lbls[0] != "needs-info" || lbls[1] != "ready-for-agent" {
		t.Fatalf("replaced labels = %v", lbls)
	}
	resp = x.do("PUT", base+"/issues/1/labels", map[string]any{"labels": []string{"needs-info"}}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); len(got["labels"].([]any)) != 1 {
		t.Fatalf("labels after shrink = %v", got["labels"])
	}

	// Validation and misses.
	for _, tt := range []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"unknown label on PUT", "PUT", base + "/issues/1/labels", map[string]any{"labels": []string{"nope"}}, 400},
		{"invalid state", "PATCH", base + "/issues/1", map[string]any{"state": "parked"}, 400},
		{"unknown patch field", "PATCH", base + "/issues/1", map[string]any{"assignee": "x"}, 400},
		{"empty title create", "POST", base + "/issues", map[string]any{"title": "   "}, 400},
		{"unknown create label", "POST", base + "/issues", map[string]any{"title": "t", "labels": []string{"nope"}}, 400},
		{"empty comment", "POST", base + "/issues/1/comments", map[string]any{"body": " "}, 400},
		{"missing issue get", "GET", base + "/issues/999", nil, 404},
		{"missing issue patch", "PATCH", base + "/issues/999", map[string]any{"title": "x"}, 404},
		{"missing issue comment", "POST", base + "/issues/999/comments", map[string]any{"body": "x"}, 404},
		{"missing issue labels", "PUT", base + "/issues/999/labels", map[string]any{"labels": []string{}}, 404},
		{"non-numeric issue", "GET", base + "/issues/abc", nil, 404},
		{"bad state filter", "GET", base + "/issues?state=bogus", nil, 400},
	} {
		resp := x.do(tt.method, tt.path, tt.body, h)
		if resp.StatusCode != tt.want {
			t.Fatalf("%s: status = %d, want %d", tt.name, resp.StatusCode, tt.want)
		}
		_ = resp.Body.Close()
	}
}

func TestBuiltinReadyQueue(t *testing.T) {
	x, _ := newTrackerServer(t, nil, nil)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID

	// #1 ready, #2 unlabeled, #3 ready but closed → only #1 queues.
	for _, title := range []string{"ready", "plain", "closed-ready"} {
		resp := x.do("POST", base+"/issues", map[string]any{"title": title}, h)
		wantStatus(t, resp, http.StatusCreated)
		_ = resp.Body.Close()
	}
	for _, n := range []int{1, 3} {
		resp := x.do("PUT", fmt.Sprintf("%s/issues/%d/labels", base, n),
			map[string]any{"labels": []string{tracker.ReadyLabel}}, h)
		wantStatus(t, resp, http.StatusOK)
		_ = resp.Body.Close()
	}
	resp := x.do("PATCH", base+"/issues/3", map[string]any{"state": "closed"}, h)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()

	// A comment on the queued issue shows up as its real count in /ready
	// (the builtin tracker carries the store count through the vocabulary).
	resp = x.do("POST", base+"/issues/1/comments", map[string]any{"body": "context"}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = resp.Body.Close()

	resp = x.do("GET", base+"/ready", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	items := issuesOf(t, decodeBody(t, resp))
	if len(items) != 1 || items[0]["number"] != float64(1) {
		t.Fatalf("ready queue = %v", items)
	}
	if items[0]["comments_count"] != float64(1) {
		t.Fatalf("ready comments_count = %v, want 1", items[0]["comments_count"])
	}
}

// stubForgeTracker is the canned Tracker the forge tests inject at the
// ForgejoFactory seam.
type stubForgeTracker struct {
	ready    []tracker.Issue
	issues   []tracker.Issue
	details  map[int]tracker.Issue
	gotState string
	gotEdits []tracker.IssueEdit
}

func (s *stubForgeTracker) ReadyIssues(context.Context) ([]tracker.Issue, error) {
	return s.ready, nil
}

func (s *stubForgeTracker) Issues(_ context.Context, state string) ([]tracker.Issue, error) {
	s.gotState = state
	return s.issues, nil
}

func (s *stubForgeTracker) Issue(_ context.Context, number int) (tracker.Issue, error) {
	is, ok := s.details[number]
	if !ok {
		// Shaped like the real client's upstream-404 error: operation context,
		// never the token, wrapping the typed tracker.ErrNotFound sentinel.
		return tracker.Issue{}, fmt.Errorf("forgejo GET /repos/o/r/issues/%d: unexpected status 404: not found: %w", number, tracker.ErrNotFound)
	}
	return is, nil
}

func (s *stubForgeTracker) CreateComment(context.Context, int, string) error { return nil }

func (s *stubForgeTracker) Pulls(context.Context) ([]tracker.PullRef, error) {
	return []tracker.PullRef{}, nil
}

func (s *stubForgeTracker) PullsForHead(context.Context, string, string) ([]tracker.PullRef, error) {
	return []tracker.PullRef{}, nil
}

func (s *stubForgeTracker) Pull(_ context.Context, number int) (tracker.PullDetail, error) {
	return tracker.PullDetail{}, fmt.Errorf("forgejo GET /repos/o/r/pulls/%d: unexpected status 404: not found: %w", number, tracker.ErrNotFound)
}

func (s *stubForgeTracker) Checks(context.Context, int) ([]tracker.Check, error) {
	return []tracker.Check{}, nil
}

func (s *stubForgeTracker) CreatePull(context.Context, string, string, string, string) (tracker.PullRef, error) {
	return tracker.PullRef{}, nil
}
func (s *stubForgeTracker) MergePull(context.Context, int) (tracker.PullRef, error) {
	return tracker.PullRef{}, nil
}
func (s *stubForgeTracker) Reviews(context.Context, int) ([]tracker.Review, error) {
	return nil, nil
}
func (s *stubForgeTracker) RerequestReview(context.Context, int) error     { return nil }
func (s *stubForgeTracker) CommentPull(context.Context, int, string) error { return nil }
func (s *stubForgeTracker) PullComments(context.Context, int) ([]tracker.Comment, error) {
	return nil, nil
}

func (s *stubForgeTracker) CloseIssue(context.Context, int) error { return nil }
func (s *stubForgeTracker) CreateIssue(context.Context, string, string, []string) (tracker.Issue, error) {
	return tracker.Issue{}, nil
}
func (s *stubForgeTracker) EditIssue(_ context.Context, number int, edit tracker.IssueEdit) (tracker.Issue, error) {
	s.gotEdits = append(s.gotEdits, edit)
	is, ok := s.details[number]
	if !ok {
		// Shaped like the real client's upstream-404: operation context, never
		// the token, wrapping the typed tracker.ErrNotFound sentinel.
		return tracker.Issue{}, fmt.Errorf("forgejo PATCH /repos/o/r/issues/%d: unexpected status 404: not found: %w", number, tracker.ErrNotFound)
	}
	if edit.Title != nil {
		is.Title = *edit.Title
	}
	if edit.Body != nil {
		is.Body = *edit.Body
	}
	s.details[number] = is
	// Keep the list entry in sync so a follow-up list read agrees with detail.
	for i := range s.issues {
		if s.issues[i].Number == number {
			s.issues[i].Title = is.Title
			s.issues[i].Body = is.Body
		}
	}
	// Return the updated issue in LIST shape (an edit is a write, not a thread
	// read): no comment bodies, the count carried through the vocabulary.
	list := is
	list.Comments = nil
	list.CommentsCount = len(is.Comments)
	return list, nil
}
func (s *stubForgeTracker) AddIssueLabels(context.Context, int, []string) error    { return nil }
func (s *stubForgeTracker) RemoveIssueLabels(context.Context, int, []string) error { return nil }
func (s *stubForgeTracker) Labels(context.Context) ([]tracker.Label, error)        { return nil, nil }
func (s *stubForgeTracker) EnsureLabel(context.Context, string, string, string) (tracker.Label, error) {
	return tracker.Label{}, nil
}

func TestForgeBoundRepo(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	stub := &stubForgeTracker{
		ready: []tracker.Issue{
			{Number: 4, Title: "queued", State: "open", Labels: []string{tracker.ReadyLabel}, CreatedAt: now, UpdatedAt: now},
		},
		issues: []tracker.Issue{
			{Number: 7, Title: "forge issue", State: "open", Labels: []string{"bug"}, CommentsCount: 5, CreatedAt: now, UpdatedAt: now},
			{Number: 4, Title: "queued", State: "open", Labels: []string{tracker.ReadyLabel}, CreatedAt: now, UpdatedAt: now},
		},
		details: map[int]tracker.Issue{
			7: {Number: 7, Title: "forge issue", Body: "b", State: "open", Labels: []string{"bug"},
				Comments:  []tracker.Comment{{Author: "alice", Body: "from the forge", CreatedAt: now}},
				CreatedAt: now, UpdatedAt: now},
		},
	}
	var gotCfg tracker.ForgejoConfig
	x, vlt := newTrackerServer(t, func(c tracker.ForgejoConfig) tracker.Tracker {
		gotCfg = c
		return stub
	}, nil)

	// A real forge_token credential, vault-encrypted — the registry decrypts
	// it on every TrackerFor.
	blob, err := vlt.EncryptPayload(vault.ForgeTokenPayload{Host: "forge.example.com", Token: "sekret-tok"})
	if err != nil {
		t.Fatalf("EncryptPayload: %v", err)
	}
	credID := ids.NewID("cred")
	if _, err := x.st.CreateCredential(context.Background(), credID, "forge", store.CredentialKindForgeToken, blob, time.Now()); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	repo := seedTrackerRepo(t, x, "forged", func(r *store.Repo) {
		r.TrackerBinding = store.TrackerBindingForge
		r.ForgeKind = "forgejo"
		r.ForgeCredentialID = &credID
	})
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID

	// Read views proxy the stubbed REST client through the real registry.
	resp := x.do("GET", base+"/issues?state=all", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	list := decodeBody(t, resp)
	if list["binding"] != "forge" {
		t.Fatalf("binding = %v", list["binding"])
	}
	items := issuesOf(t, list)
	if len(items) != 2 || items[0]["number"] != float64(7) {
		t.Fatalf("forge list = %v", items)
	}
	// The forge list carries the tracker vocabulary's real comment count
	// (list views load no comment bodies).
	if items[0]["comments_count"] != float64(5) {
		t.Fatalf("forge list comments_count = %v, want 5", items[0]["comments_count"])
	}
	if stub.gotState != "all" {
		t.Fatalf("state passed to tracker = %q, want all", stub.gotState)
	}

	// The registry built the client from the decrypted credential + remote.
	if gotCfg.BaseURL != "https://forge.example.com/api/v1" {
		t.Fatalf("BaseURL = %q", gotCfg.BaseURL)
	}
	if gotCfg.Token != "sekret-tok" || gotCfg.Owner != "Cloonar" || gotCfg.Repo != "forged" {
		t.Fatalf("config = %+v", gotCfg)
	}

	// Client-side label filter on the forge list.
	resp = x.do("GET", base+"/issues?label="+tracker.ReadyLabel, nil, nil)
	if items := issuesOf(t, decodeBody(t, resp)); len(items) != 1 || items[0]["number"] != float64(4) {
		t.Fatalf("filtered forge list = %v", items)
	}

	// Detail with comments; ready queue.
	resp = x.do("GET", base+"/issues/7", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	detail := decodeBody(t, resp)
	cs := detail["comments"].([]any)
	if len(cs) != 1 || cs[0].(map[string]any)["author"] != "alice" {
		t.Fatalf("forge detail comments = %v", cs)
	}
	resp = x.do("GET", base+"/ready", nil, nil)
	if items := issuesOf(t, decodeBody(t, resp)); len(items) != 1 || items[0]["number"] != float64(4) {
		t.Fatalf("forge ready = %v", items)
	}

	// A forge miss is a 404 — the client's typed upstream-404 sentinel maps
	// to not-found, same contract as the builtin binding (a stale issue link
	// is not a forge outage).
	resp = x.do("GET", base+"/issues/404", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	if got := decodeBody(t, resp); got["error"] != "not found" {
		t.Fatalf("forge miss error = %q, want opaque not found", got["error"])
	}

	// Every builtin-only mutation (issue create, comment, label ops) answers the
	// pinned 409. The title/body PATCH is deliberately NOT here — it rides the
	// tracker seam on every binding (ADR-0046); see TestForgeBoundIssueEdit.
	for _, tt := range []struct {
		method, path string
		body         any
	}{
		{"POST", base + "/issues", map[string]any{"title": "t"}},
		{"POST", base + "/issues/7/comments", map[string]any{"body": "b"}},
		{"PUT", base + "/issues/7/labels", map[string]any{"labels": []string{}}},
		{"POST", base + "/labels", map[string]any{"name": "x"}},
		{"PATCH", base + "/labels/lbl_x", map[string]any{"name": "x"}},
		{"DELETE", base + "/labels/lbl_x", nil},
	} {
		resp := x.do(tt.method, tt.path, tt.body, h)
		wantStatus(t, resp, http.StatusConflict)
		if got := decodeBody(t, resp); got["error"] != forgeTrackerMessage {
			t.Fatalf("%s %s error = %q, want pinned forge message", tt.method, tt.path, got["error"])
		}
	}

	// The label LIST stays readable (every repo carries the seeded five).
	resp = x.do("GET", base+"/labels", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); len(got["labels"].([]any)) != 5 {
		t.Fatalf("forge repo labels = %v", got["labels"])
	}
}

// TestForgeBoundIssueEdit pins the operator title/body PATCH riding
// Tracker.EditIssue on a FORGE-bound repo (ADR-0046): the edit reaches the seam,
// the pinned response is the DETAIL shape via the follow-up read, and a STATE
// change is refused with the pinned 400 because the seam has no state op. Same
// seeding shape as TestForgeBoundRepo, in a fresh server so gotEdits starts empty.
func TestForgeBoundIssueEdit(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	stub := &stubForgeTracker{
		issues: []tracker.Issue{
			{Number: 7, Title: "forge issue", State: "open", Labels: []string{"bug"}, CommentsCount: 1, CreatedAt: now, UpdatedAt: now},
		},
		details: map[int]tracker.Issue{
			7: {Number: 7, Title: "forge issue", Body: "b", State: "open", Labels: []string{"bug"},
				Comments:  []tracker.Comment{{Author: "alice", Body: "from the forge", CreatedAt: now}},
				CreatedAt: now, UpdatedAt: now},
		},
	}
	x, vlt := newTrackerServer(t, func(tracker.ForgejoConfig) tracker.Tracker { return stub }, nil)
	blob, err := vlt.EncryptPayload(vault.ForgeTokenPayload{Host: "forge.example.com", Token: "sekret-tok"})
	if err != nil {
		t.Fatalf("EncryptPayload: %v", err)
	}
	credID := ids.NewID("cred")
	if _, err := x.st.CreateCredential(context.Background(), credID, "forge", store.CredentialKindForgeToken, blob, time.Now()); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	repo := seedTrackerRepo(t, x, "forged", func(r *store.Repo) {
		r.TrackerBinding = store.TrackerBindingForge
		r.ForgeKind = "forgejo"
		r.ForgeCredentialID = &credID
	})
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID

	// A title+body edit rides the seam and answers 200 with the DETAIL shape:
	// the new title/body AND the comment thread from the follow-up read.
	resp := x.do("PATCH", base+"/issues/7", map[string]any{"title": "renamed", "body": "nb"}, h)
	wantStatus(t, resp, http.StatusOK)
	got := decodeBody(t, resp)
	if got["title"] != "renamed" || got["body"] != "nb" {
		t.Fatalf("edited issue = %v", got)
	}
	cs := got["comments"].([]any)
	if len(cs) != 1 || cs[0].(map[string]any)["author"] != "alice" {
		t.Fatalf("edit response thread = %v, want alice's comment from the follow-up read", cs)
	}
	if len(stub.gotEdits) != 1 {
		t.Fatalf("seam saw %d edits, want 1", len(stub.gotEdits))
	}
	if e := stub.gotEdits[0]; e.Title == nil || *e.Title != "renamed" || e.Body == nil || *e.Body != "nb" {
		t.Fatalf("seam saw edit = %+v, want Title and Body pointers both set", e)
	}

	// A body-only clear: non-nil empty Body reaches the seam, Title stays nil.
	resp = x.do("PATCH", base+"/issues/7", map[string]any{"body": ""}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["body"] != "" {
		t.Fatalf("body after clear = %v, want empty", got["body"])
	}
	if e := stub.gotEdits[len(stub.gotEdits)-1]; e.Title != nil || e.Body == nil || *e.Body != "" {
		t.Fatalf("clear edit = %+v, want nil Title and non-nil empty Body", e)
	}

	// A state change has no seam op → the pinned 400, and nothing reaches the seam.
	edits := len(stub.gotEdits)
	resp = x.do("PATCH", base+"/issues/7", map[string]any{"state": "closed"}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if body := decodeBody(t, resp); body["error"] != forgeStateMessage {
		t.Fatalf("forge state PATCH error = %q, want pinned forgeStateMessage", body["error"])
	}
	if len(stub.gotEdits) != edits {
		t.Fatalf("state PATCH reached the seam (%d→%d edits)", edits, len(stub.gotEdits))
	}

	// A mixed title+state patch is refused whole — the state clause wins, and
	// the title is NOT edited.
	resp = x.do("PATCH", base+"/issues/7", map[string]any{"title": "t", "state": "closed"}, h)
	wantStatus(t, resp, http.StatusBadRequest)
	if body := decodeBody(t, resp); body["error"] != forgeStateMessage {
		t.Fatalf("mixed PATCH error = %q, want pinned forgeStateMessage", body["error"])
	}
	if len(stub.gotEdits) != edits {
		t.Fatalf("mixed PATCH reached the seam (%d→%d edits)", edits, len(stub.gotEdits))
	}

	// Field validation runs before the binding split: on the forge binding too a
	// whitespace title and an unknown field are 400s with the pinned wording,
	// and neither reaches the seam.
	for _, tt := range []struct {
		name string
		body any
		want string
	}{
		{"whitespace title", map[string]any{"title": "  "}, "title must not be empty"},
		{"unknown field", map[string]any{"bogus": "x"}, `unknown field "bogus"`},
	} {
		resp := x.do("PATCH", base+"/issues/7", tt.body, h)
		wantStatus(t, resp, http.StatusBadRequest)
		if body := decodeBody(t, resp); body["error"] != tt.want {
			t.Fatalf("%s: error = %q, want %q", tt.name, body["error"], tt.want)
		}
	}
	if len(stub.gotEdits) != edits {
		t.Fatalf("a validation-rejected PATCH reached the seam")
	}

	// An edit on an unknown issue number is a 404 — the seam's typed not-found.
	resp = x.do("PATCH", base+"/issues/404", map[string]any{"title": "x"}, h)
	wantStatus(t, resp, http.StatusNotFound)
	if body := decodeBody(t, resp); body["error"] != "not found" {
		t.Fatalf("unknown-issue edit error = %q, want opaque not found", body["error"])
	}
}

// TestForgeFlavorMismatch: a github.com-detected repo (forge_kind 'github')
// bound to a FORGEJO-flavored credential is a loud configuration conflict
// (ADR-0015 mismatch tripwire) — a 409 naming the disagreement, not a silent
// 404 on first use.
func TestForgeFlavorMismatch(t *testing.T) {
	x, vlt := newTrackerServer(t, nil, nil)
	blob, err := vlt.EncryptPayload(vault.ForgeTokenPayload{Host: "git.cloonar.com", Token: "tok", Forge: vault.ForgeForgejo})
	if err != nil {
		t.Fatalf("EncryptPayload: %v", err)
	}
	credID := ids.NewID("cred")
	if _, err := x.st.CreateCredential(context.Background(), credID, "forge", store.CredentialKindForgeToken, blob, time.Now()); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	repo := seedTrackerRepo(t, x, "hub", func(r *store.Repo) {
		r.TrackerBinding = store.TrackerBindingForge
		r.ForgeKind = "github"
		r.ForgeCredentialID = &credID
		r.RemoteURL = "git@github.com:Cloonar/hub.git"
	})
	// Resolution fails on the tripwire before any wire call: the neither
	// factory the server was built with is ever invoked.
	resp := x.do("GET", "/api/v1/repos/"+repo.ID+"/issues", nil, nil)
	wantStatus(t, resp, http.StatusConflict)
	if got := decodeBody(t, resp); !strings.Contains(got["error"].(string), "does not match") {
		t.Fatalf("error = %q, want the flavor-mismatch diagnostic", got["error"])
	}
}

func TestForgeBadCredentialHostIs409(t *testing.T) {
	x, vlt := newTrackerServer(t, nil, nil)
	// Plain-http host: normalizeForgeHost rejects it (lab does not send forge
	// tokens over cleartext) with ErrForgeHost, wrapped with the credential
	// name. That is a configuration conflict pointing at the credential — a
	// 409 with the diagnostic, not an opaque logged 500.
	blob, err := vlt.EncryptPayload(vault.ForgeTokenPayload{Host: "http://forge.example.com", Token: "tok"})
	if err != nil {
		t.Fatalf("EncryptPayload: %v", err)
	}
	credID := ids.NewID("cred")
	if _, err := x.st.CreateCredential(context.Background(), credID, "forge", store.CredentialKindForgeToken, blob, time.Now()); err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	repo := seedTrackerRepo(t, x, "badhost", func(r *store.Repo) {
		r.TrackerBinding = store.TrackerBindingForge
		r.ForgeKind = "forgejo"
		r.ForgeCredentialID = &credID
	})
	resp := x.do("GET", "/api/v1/repos/"+repo.ID+"/issues", nil, nil)
	wantStatus(t, resp, http.StatusConflict)
	got := decodeBody(t, resp)
	msg, _ := got["error"].(string)
	if !strings.Contains(msg, "invalid forge host") || !strings.Contains(msg, `"forge"`) {
		t.Fatalf("error = %q, want ErrForgeHost diagnostic naming the credential", msg)
	}
}

func TestIssueChangedSSE(t *testing.T) {
	x, _ := newTrackerServer(t, nil, nil)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)

	stream := x.do("GET", "/api/v1/events", nil, nil)
	wantStatus(t, stream, http.StatusOK)
	defer func() { _ = stream.Body.Close() }()

	type frame struct {
		data string
		err  error
	}
	done := make(chan frame, 1)
	go func() {
		scanner := bufio.NewScanner(stream.Body)
		sawEvent := false
		for scanner.Scan() {
			line := scanner.Text()
			if line == "event: "+EventIssueChanged {
				sawEvent = true
				continue
			}
			if sawEvent && strings.HasPrefix(line, "data: ") {
				done <- frame{data: strings.TrimPrefix(line, "data: ")}
				return
			}
		}
		done <- frame{err: scanner.Err()}
	}()

	resp := x.do("POST", "/api/v1/repos/"+repo.ID+"/issues", map[string]any{"title": "evented"}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = resp.Body.Close()

	select {
	case f := <-done:
		if f.err != nil {
			t.Fatalf("reading stream: %v", f.err)
		}
		if !strings.Contains(f.data, repo.ID) || !strings.Contains(f.data, EventIssueChanged) {
			t.Fatalf("issue.changed payload = %q, want repoID %q", f.data, repo.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for issue.changed")
	}
}

func TestTrackerMutationsRequireCSRF(t *testing.T) {
	x, _ := newTrackerServer(t, nil, nil)
	repo := seedTrackerRepo(t, x, "proj", nil)
	base := "/api/v1/repos/" + repo.ID

	// Cookie-authenticated (ambient) mutations without the CSRF header are
	// rejected before any handler runs.
	for _, tt := range []struct {
		method, path string
		body         any
	}{
		{"POST", base + "/issues", map[string]any{"title": "t"}},
		{"PATCH", base + "/issues/1", map[string]any{"title": "t"}},
		{"POST", base + "/issues/1/comments", map[string]any{"body": "b"}},
		{"PUT", base + "/issues/1/labels", map[string]any{"labels": []string{}}},
		{"POST", base + "/labels", map[string]any{"name": "x"}},
		{"PATCH", base + "/labels/lbl_x", map[string]any{"name": "x"}},
		{"DELETE", base + "/labels/lbl_x", nil},
	} {
		resp := x.do(tt.method, tt.path, tt.body, nil)
		wantStatus(t, resp, http.StatusForbidden)
		if got := decodeBody(t, resp); got["error"] != "missing X-Lab-Csrf header" {
			t.Fatalf("%s %s error = %q", tt.method, tt.path, got["error"])
		}
	}

	// Reads need no CSRF.
	resp := x.do("GET", base+"/issues", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
}

// TestIssueLabelsPutBodyValidation pins that the destructive label PUT never
// treats a missing "labels" key (or a typoed field) as "clear everything":
// absent key, unknown key, and null are 400s; an explicit [] still clears.
func TestIssueLabelsPutBodyValidation(t *testing.T) {
	x, _ := newTrackerServer(t, nil, nil)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID

	resp := x.do("POST", base+"/issues", map[string]any{"title": "t", "labels": []string{"needs-triage"}}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = resp.Body.Close()

	labelsOfIssue := func() []any {
		t.Helper()
		resp := x.do("GET", base+"/issues/1", nil, nil)
		wantStatus(t, resp, http.StatusOK)
		return decodeBody(t, resp)["labels"].([]any)
	}

	for _, tt := range []struct {
		name string
		body any
		want string
	}{
		{"empty object", map[string]any{}, "labels is required"},
		{"typoed key", map[string]any{"lables": []string{"needs-info"}}, `unknown field "lables"`},
		{"null labels", map[string]any{"labels": nil}, "labels must be an array of strings"},
	} {
		resp := x.do("PUT", base+"/issues/1/labels", tt.body, h)
		wantStatus(t, resp, http.StatusBadRequest)
		if got := decodeBody(t, resp); got["error"] != tt.want {
			t.Fatalf("%s: error = %q, want %q", tt.name, got["error"], tt.want)
		}
		// The rejected request must not have touched the label set.
		if got := labelsOfIssue(); len(got) != 1 || got[0] != "needs-triage" {
			t.Fatalf("%s: labels after rejected PUT = %v, want [needs-triage]", tt.name, got)
		}
	}

	// The explicit empty array keeps its clear semantics.
	resp = x.do("PUT", base+"/issues/1/labels", map[string]any{"labels": []string{}}, h)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); len(got["labels"].([]any)) != 0 {
		t.Fatalf("labels after explicit clear = %v, want none", got["labels"])
	}
}

// TestLabelIDsByName_errorClassification pins the 400-vs-500 split behind
// issue create / label PUT: only the unknown-name USER error carries the
// errUnknownLabel sentinel (→ 400); a store failure does not (→ the handlers'
// opaque logged 500), so internal diagnostics never ride the error envelope.
func TestLabelIDsByName_errorClassification(t *testing.T) {
	x, _ := newTrackerServer(t, nil, nil)
	repo := seedTrackerRepo(t, x, "proj", nil)

	// Unknown name → the sentinel, carrying the offending name.
	_, err := x.srv.labelIDsByName(context.Background(), repo.ID, []string{"nope"})
	if !errors.Is(err, errUnknownLabel) {
		t.Fatalf("unknown-name err = %v, want errUnknownLabel", err)
	}
	if !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("unknown-name err %q does not carry the name", err)
	}

	// A store failure (canceled context) is NOT the user error: the handlers
	// route it to internalError instead of a 400.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = x.srv.labelIDsByName(canceled, repo.ID, []string{"needs-triage"})
	if err == nil {
		t.Fatal("labelIDsByName with canceled context returned nil error")
	}
	if errors.Is(err, errUnknownLabel) {
		t.Fatalf("store failure classified as unknown label: %v", err)
	}
}

// TestOperatorWriteNotSecretScanned pins the run-token-only line of the #107
// leak guard from the operator side: the secret scan wraps ONLY the resolver
// handed to the agent API (secretscan.NewResolver in cmd/lab), so an operator
// write carrying a repo secret's value is NOT second-guessed — it succeeds and
// reaches the store byte-identical. The operator API resolves the bare registry
// (Config{Tracker: trackerReg}); this test would fail if the scan ever leaked
// onto that path.
func TestOperatorWriteNotSecretScanned(t *testing.T) {
	x, vlt := newTrackerServer(t, nil, nil)
	repo := seedTrackerRepo(t, x, "proj", nil)
	h := csrfHeaders(x.ts.URL)
	base := "/api/v1/repos/" + repo.ID

	// Seed a repo secret exactly as the operator's `secret set` would: the
	// store only ever sees ciphertext, encrypted under the server's own vault.
	const secretValue = "op-secret-do-not-scan-8842"
	blob, err := vlt.Encrypt([]byte(secretValue))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if _, err := x.st.CreateRepoSecret(context.Background(), ids.NewID("sec"), repo.ID, "DEPLOY_KEY", "", blob, time.Now()); err != nil {
		t.Fatalf("CreateRepoSecret: %v", err)
	}

	resp := x.do("POST", base+"/issues", map[string]any{"title": "deploy notes"}, h)
	wantStatus(t, resp, http.StatusCreated)
	_ = resp.Body.Close()

	// The operator comment carries the secret verbatim — and is accepted, the
	// value never scanned.
	commentBody := "rolled out with " + secretValue
	resp = x.do("POST", base+"/issues/1/comments", map[string]any{"body": commentBody}, h)
	wantStatus(t, resp, http.StatusCreated)
	if got := decodeBody(t, resp); got["body"] != commentBody {
		t.Fatalf("operator comment body = %q, want the value passed through verbatim", got["body"])
	}

	// It landed in the store unchanged — the operator path is not gated.
	is, err := x.st.IssueByRepoNumber(context.Background(), repo.ID, 1)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if len(is.Comments) != 1 || is.Comments[0].Body != commentBody {
		t.Fatalf("stored comments = %+v, want the operator write verbatim", is.Comments)
	}
}

func TestTrackerUnknownRepo404(t *testing.T) {
	x, _ := newTrackerServer(t, nil, nil)
	h := csrfHeaders(x.ts.URL)
	for _, tt := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/repos/repo_missing/issues", nil},
		{"GET", "/api/v1/repos/repo_missing/issues/1", nil},
		{"GET", "/api/v1/repos/repo_missing/ready", nil},
		{"GET", "/api/v1/repos/repo_missing/labels", nil},
		{"POST", "/api/v1/repos/repo_missing/issues", map[string]any{"title": "t"}},
		{"POST", "/api/v1/repos/repo_missing/labels", map[string]any{"name": "x"}},
	} {
		resp := x.do(tt.method, tt.path, tt.body, h)
		wantStatus(t, resp, http.StatusNotFound)
		_ = resp.Body.Close()
	}
}
