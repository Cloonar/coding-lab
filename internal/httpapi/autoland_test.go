package httpapi

// httptest suite for the human re-arm surface (issue #188 / ADR-0048's
// amendment): POST .../autoland/pulls/{pull}/rearm. Same bar as afk_test.go's
// TestAPI_AFKResetClearsFailures — real store, real AFK engine, builtin
// tracker — since re-arm is modeled directly on the three-strikes reset.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// TestAPI_AutolandRearmHappyPath is the worst-failure-mode check the issue
// calls out by name: a re-arm that clears terminality but leaves the fix
// budget spent re-escalates on the very next rejection and reads to the
// human as "the re-arm silently did not work." So this seeds BOTH the fix
// and escalate counters non-zero first, then asserts — cross-checked
// directly against the store, never just the response body — that both read
// back 0 AND that PullRearmedAt actually moved.
func TestAPI_AutolandRearmHappyPath(t *testing.T) {
	x := newAFKServer(t)
	h := csrfHeaders(x.ts.URL)
	log := recordBus(t, x.bus)
	ctx := context.Background()
	const pull = 41

	for range 2 {
		if err := x.st.RecordAutolandAttempt(ctx, x.repo.ID, pull, store.RunKindFix); err != nil {
			t.Fatalf("seed fix attempt: %v", err)
		}
	}
	if err := x.st.RecordAutolandAttempt(ctx, x.repo.ID, pull, store.RunKindEscalate); err != nil {
		t.Fatalf("seed escalate attempt: %v", err)
	}
	if n, err := x.st.AutolandAttempts(ctx, x.repo.ID, pull, store.RunKindFix); err != nil || n != 2 {
		t.Fatalf("precondition: fix attempts = %d (err %v), want 2", n, err)
	}

	before := time.Now()
	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/autoland/pulls/41/rearm", nil, h)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)

	if body["repo_id"] != x.repo.ID {
		t.Errorf("repo_id = %v, want %v", body["repo_id"], x.repo.ID)
	}
	if body["pull_number"] != float64(pull) {
		t.Errorf("pull_number = %v, want %v", body["pull_number"], pull)
	}
	rearmedAt, _ := body["rearmed_at"].(string)
	if rearmedAt == "" {
		t.Fatal("rearmed_at missing or not a string")
	}
	respTime, err := time.Parse(time.RFC3339Nano, rearmedAt)
	if err != nil {
		t.Fatalf("rearmed_at %q does not parse: %v", rearmedAt, err)
	}
	if respTime.Before(before.Add(-time.Second)) {
		t.Errorf("rearmed_at %v looks stale relative to the call (before %v)", respTime, before)
	}

	// rearmed_at is store.FormatTime's rendering of the STORED value, byte
	// for byte — cross-checked against PullRearmedAt directly, not just
	// decoded back out of the same response.
	stored, err := x.st.PullRearmedAt(ctx, x.repo.ID, pull)
	if err != nil {
		t.Fatalf("PullRearmedAt: %v", err)
	}
	if stored.IsZero() {
		t.Fatal("PullRearmedAt still zero after a successful re-arm")
	}
	if want := store.FormatTime(stored); rearmedAt != want {
		t.Errorf("rearmed_at = %q, want %q (the stored value byte-for-byte)", rearmedAt, want)
	}

	// The worst failure mode: budgets left spent while terminality clears.
	for _, kind := range []string{store.RunKindFix, store.RunKindEscalate} {
		if n, err := x.st.AutolandAttempts(ctx, x.repo.ID, pull, kind); err != nil || n != 0 {
			t.Errorf("%s attempts after re-arm = %d (err %v), want 0", kind, n, err)
		}
	}

	if !sawEvent(log, "repo.changed") {
		t.Error("no repo.changed event on re-arm")
	}
}

// TestAPI_AutolandRearmUnknownRepo: a repo that does not exist is a 404, the
// same as every other repo-scoped action (loadRepo).
func TestAPI_AutolandRearmUnknownRepo(t *testing.T) {
	x := newAFKServer(t)
	resp := x.do("POST", "/api/v1/repos/ghost/autoland/pulls/41/rearm", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
}

// TestAPI_AutolandRearmBadPullNumber: a non-integer or non-positive {pull} is
// caller input shaped wrong — a 400, never a 404 (which would suggest the
// repo, not the pull number, is the problem) and never a panic.
func TestAPI_AutolandRearmBadPullNumber(t *testing.T) {
	x := newAFKServer(t)
	h := csrfHeaders(x.ts.URL)
	for _, bad := range []string{"abc", "0", "-1"} {
		t.Run(bad, func(t *testing.T) {
			resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/autoland/pulls/"+bad+"/rearm", nil, h)
			wantStatus(t, resp, http.StatusBadRequest)
			_ = resp.Body.Close()
		})
	}
}

// TestAPI_AutolandRearmRequiresHumanAuth pins the acceptance criterion the
// issue states in the loudest terms: "no labctl (run-token) verb exists for
// it." Two layers of proof:
//
//  1. An unauthenticated request (no session cookie) — what a run-token
//     caller would look like AT the operator surface — is refused with the
//     same 401 requireAuth answers everywhere else (see TestLogout in
//     server_test.go for the identical idiom: doWith + http.DefaultClient,
//     a fresh client the setup/login flow never touched).
//  2. The structural guarantee behind that 401: this test lives in
//     internal/httpapi and only imports internal/httpapi's own server, which
//     mounts the run-token surface as `root.Handle("/agent/v1/", s.agent)`
//     from the wholly separate internal/agentapi package (server.go,
//     Handler()) — a disjoint http.ServeMux this package never registers
//     handleAutolandRearm on and has no way to reach into. internal/agentapi
//     is out of this issue's file scope and is never imported here, so
//     there is no route table to inspect from this side; the guarantee is
//     the absence of any registration, not a filtered-out one. A real
//     run-token request against /agent/v1/... is internal/agentapi's own
//     test suite's concern (agentapi_test.go's route table), not this
//     package's — nothing in that table names an autoland/rearm path
//     because internal/agentapi contains no such handler at all.
func TestAPI_AutolandRearmRequiresHumanAuth(t *testing.T) {
	x := newAFKServer(t)
	resp := doWith(t, http.DefaultClient, x.ts.URL, "POST",
		"/api/v1/repos/"+x.repo.ID+"/autoland/pulls/41/rearm", nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
}

// TestAPI_AutolandRearmIdempotent: re-arming an already-re-armed PR is a
// legal no-op that moves the moment forward, never a 409 — the issue is
// explicit that re-arm has no invalid prior state to reject.
func TestAPI_AutolandRearmIdempotent(t *testing.T) {
	x := newAFKServer(t)
	h := csrfHeaders(x.ts.URL)

	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/autoland/pulls/41/rearm", nil, h)
	wantStatus(t, resp, http.StatusOK)
	first := decodeBody(t, resp)["rearmed_at"].(string)
	firstAt, err := time.Parse(time.RFC3339Nano, first)
	if err != nil {
		t.Fatalf("first rearmed_at %q does not parse: %v", first, err)
	}

	resp = x.do("POST", "/api/v1/repos/"+x.repo.ID+"/autoland/pulls/41/rearm", nil, h)
	wantStatus(t, resp, http.StatusOK)
	second := decodeBody(t, resp)["rearmed_at"].(string)
	secondAt, err := time.Parse(time.RFC3339Nano, second)
	if err != nil {
		t.Fatalf("second rearmed_at %q does not parse: %v", second, err)
	}

	if secondAt.Before(firstAt) {
		t.Errorf("second rearmed_at %v is BEFORE the first %v", secondAt, firstAt)
	}
}

// TestAPI_AutolandRunJSON_PullNumber pins runJSON's newest field (issue
// #188): a lander/fix/escalate run carries the PR it works; every other run
// kind renders the key as JSON null. The key is ALWAYS present — runResponse
// pins "every key, always" for its nullable columns, and the SPA's Run type
// mirrors that with `number | null` rather than an optional field.
func TestAPI_AutolandRunJSON_PullNumber(t *testing.T) {
	x := newAFKServer(t)
	ctx := context.Background()
	pull := 55
	landerRun, err := x.st.CreateRun(ctx, store.Run{
		ID: ids.NewID("run"), RepoID: x.repo.ID, Kind: store.RunKindLander, Provider: "claude-code",
		Branch: "pr-55-head", WorktreePath: "/wt/x", SessionName: "proj~pr-55",
		Model: "opus[1m]", Effort: "max", StartedAt: afkClock, Outcome: store.RunOutcomeActive,
		PullNumber: &pull,
	})
	if err != nil {
		t.Fatalf("CreateRun (lander, with pull): %v", err)
	}
	manualRun, err := x.st.CreateRun(ctx, store.Run{
		ID: ids.NewID("run"), RepoID: x.repo.ID, Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/scratch", WorktreePath: "/wt/y", SessionName: "proj~manual",
		Model: "opus[1m]", Effort: "max", StartedAt: afkClock.Add(time.Minute), Outcome: store.RunOutcomeActive,
	})
	if err != nil {
		t.Fatalf("CreateRun (manual, no pull): %v", err)
	}

	resp := x.do("GET", "/api/v1/runs?repo="+x.repo.ID, nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	runs, _ := body["runs"].([]any)
	byID := map[string]map[string]any{}
	for _, raw := range runs {
		row, _ := raw.(map[string]any)
		byID[row["id"].(string)] = row
	}

	landerRow, ok := byID[landerRun.ID]
	if !ok {
		t.Fatalf("lander run %s missing from runs list", landerRun.ID)
	}
	if landerRow["pull_number"] != float64(pull) {
		t.Errorf("lander run pull_number = %v, want %v", landerRow["pull_number"], pull)
	}

	manualRow, ok := byID[manualRun.ID]
	if !ok {
		t.Fatalf("manual run %s missing from runs list", manualRun.ID)
	}
	got, present := manualRow["pull_number"]
	if !present {
		t.Error("manual run is missing the pull_number key, want it present as null")
	}
	if got != nil {
		t.Errorf("manual run pull_number = %v, want null", got)
	}
}
