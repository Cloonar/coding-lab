package agentapi

// Secret-leak guard integration (issue #107). These tests wire the server the
// way cmd/lab does — the run-token resolver wrapped by secretscan.NewResolver —
// and drive the real HTTP handlers, so they pin the whole path: a run-token
// PR/issue/comment create whose content carries a repo secret value (in
// plaintext or a common encoding) is rejected with a 400 that names the secret
// and never echoes the value, the underlying tracker never sees the write, and
// a clean write with secrets seeded reaches the tracker byte-identical. Both
// bindings resolve through the one wrapped seam, so the builtin binding is
// exercised too, and a decrypt/store failure fails closed (a non-400 refusal,
// never a silent delegation).

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/secretscan"
)

// scanForgeServer mirrors production wiring for a forge-bound repo: the fake
// tracker's resolver wrapped by secretscan.NewResolver, exactly as cmd/lab
// wraps the registry handed to agentapi. Non-incogni + nil scrub, so
// sanitizeBody is a pass-through and the tracker sees content unaltered but for
// the PR path's Closes injection.
func (f *testFixture) scanForgeServer(fk tracker.Tracker) *Server {
	inner := resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) {
		return fk, nil
	})
	return New(f.st, f.vlt, secretscan.NewResolver(inner, f.st, f.vlt), nil, discard(), func() time.Time { return f.now }, nil)
}

// scanBuiltinServer mirrors production wiring for a builtin-bound repo: the
// store-backed builtin resolver wrapped by secretscan.NewResolver.
func (f *testFixture) scanBuiltinServer() *Server {
	return New(f.st, f.vlt, secretscan.NewResolver(builtinResolver(f.st), f.st, f.vlt), nil, discard(), func() time.Time { return f.now }, nil)
}

// secretEncodings is the set of forms a value can leak through — the same
// derivations secretscan recognises. The 400-body leak assertion checks that
// NONE of them appears in the rejection message.
func secretEncodings(v string) []string {
	b := []byte(v)
	return []string{
		v,
		base64.StdEncoding.EncodeToString(b),
		base64.RawStdEncoding.EncodeToString(b),
		base64.URLEncoding.EncodeToString(b),
		base64.RawURLEncoding.EncodeToString(b),
		hex.EncodeToString(b),
		strings.ToUpper(hex.EncodeToString(b)),
		url.QueryEscape(v),
		url.PathEscape(v),
	}
}

// assertNoLeak fails if the rejection body carries the value in any encoding —
// the message must name the secret ONLY, never the material it matched.
func assertNoLeak(t *testing.T, body, value string) {
	t.Helper()
	for _, form := range secretEncodings(value) {
		if form == "" {
			continue
		}
		if strings.Contains(body, form) {
			t.Errorf("400 body leaked a secret encoding %q:\n%s", form, body)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return string(b)
}

// TestSecretScan_PRExactValueBlocked (criteria a, f): a PR whose body carries a
// secret's exact plaintext is a 400 naming the secret, the value never appears
// in the response in any encoding, and CreatePull is never called.
func TestSecretScan_PRExactValueBlocked(t *testing.T) {
	f := newFixture(t)
	f.seedRepoBinding(t, "repo_f", "forge", "forgejo")
	f.seedRunKind(t, "run_f", "repo_f", "afk_auto", "active", intp(7), "afk/7")
	token := f.seedToken(t, "run_f", nil)
	const value = "sk-live-4f8a2b9c1d3e5f70"
	f.seedSecret(t, "repo_f", "DEPLOY_KEY", "", value)

	fk := &fakeTracker{pullRef: tracker.PullRef{Number: 12, URL: "https://forge/pr/12"}}
	handler := f.scanForgeServer(fk).Handler()

	body := mustJSON(t, map[string]string{"title": "feat: ship it", "body": "Rolls out using " + value + " for auth."})
	rr := doJSON(t, handler, "POST", "/agent/v1/prs", token, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "DEPLOY_KEY") {
		t.Errorf("400 body = %s, want it to name DEPLOY_KEY", rr.Body.String())
	}
	assertNoLeak(t, rr.Body.String(), value)
	if fk.createdPull != nil {
		t.Errorf("CreatePull reached the tracker despite the rejected leak: %+v", fk.createdPull)
	}
}

// TestSecretScan_IssueBase64TitleBlocked (criteria b, f): base64-of-value in
// the issue title is a 400 naming the secret; no CreateIssue.
func TestSecretScan_IssueBase64TitleBlocked(t *testing.T) {
	f := newFixture(t)
	f.seedRepoBinding(t, "repo_f", "forge", "forgejo")
	f.seedRunKind(t, "run_f", "repo_f", "afk_auto", "active", intp(7), "afk/7")
	token := f.seedToken(t, "run_f", nil)
	const value = "tok_9Z8y7X6w5V4u3T2s"
	f.seedSecret(t, "repo_f", "API_TOKEN", "", value)

	fk := &fakeTracker{}
	handler := f.scanForgeServer(fk).Handler()

	encoded := base64.StdEncoding.EncodeToString([]byte(value))
	body := mustJSON(t, map[string]any{"title": "chore: rotate " + encoded, "body": "clean body"})
	rr := doJSON(t, handler, "POST", "/agent/v1/issues", token, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "API_TOKEN") {
		t.Errorf("400 body = %s, want it to name API_TOKEN", rr.Body.String())
	}
	assertNoLeak(t, rr.Body.String(), value)
	if fk.createdIssue != nil {
		t.Errorf("CreateIssue reached the tracker despite the rejected leak: %+v", fk.createdIssue)
	}
}

// TestSecretScan_CommentURLEncodedBlocked (criteria c, f): a URL-encoded secret
// (value chosen so its QueryEscape differs — it carries a slash and a space) in
// a comment body is a 400 naming the secret; no CreateComment.
func TestSecretScan_CommentURLEncodedBlocked(t *testing.T) {
	f := newFixture(t)
	f.seedRepoBinding(t, "repo_f", "forge", "forgejo")
	f.seedRunKind(t, "run_f", "repo_f", "afk_auto", "active", intp(7), "afk/7")
	token := f.seedToken(t, "run_f", nil)
	const value = "postgres://u:p@db.host/app data"
	f.seedSecret(t, "repo_f", "DB_URL", "", value)

	fk := &fakeTracker{}
	handler := f.scanForgeServer(fk).Handler()

	encoded := url.QueryEscape(value)
	if encoded == value {
		t.Fatalf("QueryEscape did not alter the value; pick a value with a space or slash")
	}
	body := mustJSON(t, map[string]string{"body": "connecting to " + encoded + " now"})
	rr := doJSON(t, handler, "POST", "/agent/v1/issues/7/comments", token, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "DB_URL") {
		t.Errorf("400 body = %s, want it to name DB_URL", rr.Body.String())
	}
	assertNoLeak(t, rr.Body.String(), value)
	if len(fk.comments) != 0 {
		t.Errorf("CreateComment reached the tracker despite the rejected leak: %+v", fk.comments)
	}
}

// TestSecretScan_CleanWritesSucceed (criterion d): with a secret seeded, clean
// PR/issue/comment writes all succeed and reach the tracker byte-identical. The
// PR body is asserted against what the handler actually sends — after the AFK
// Closes injection and the (pass-through) sanitizer.
func TestSecretScan_CleanWritesSucceed(t *testing.T) {
	f := newFixture(t)
	f.seedRepoBinding(t, "repo_f", "forge", "forgejo")
	f.seedRunKind(t, "run_f", "repo_f", "afk_auto", "active", intp(7), "afk/7")
	token := f.seedToken(t, "run_f", nil)
	f.seedSecret(t, "repo_f", "DEPLOY_KEY", "", "sk-live-4f8a2b9c1d3e5f70")

	fk := &fakeTracker{pullRef: tracker.PullRef{Number: 12, URL: "https://forge/pr/12"}}
	handler := f.scanForgeServer(fk).Handler()

	// PR: the handler appends "\n\nCloses #7" (AFK run, issue 7); sanitizeBody
	// is a pass-through here, so that is exactly what CreatePull must receive.
	prBody := mustJSON(t, map[string]string{"title": "feat: widget", "body": "Implements the widget."})
	rr := doJSON(t, handler, "POST", "/agent/v1/prs", token, prBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("PR create: status = %d, body %s", rr.Code, rr.Body.String())
	}
	if fk.createdPull == nil {
		t.Fatal("CreatePull not called on a clean PR write")
	}
	wantPull := pullArgs{head: "afk/7", base: "main", title: "feat: widget", body: "Implements the widget.\n\nCloses #7"}
	if *fk.createdPull != wantPull {
		t.Errorf("CreatePull got %+v, want %+v", *fk.createdPull, wantPull)
	}

	// Issue create.
	issueBody := mustJSON(t, map[string]any{"title": "found: flake", "body": "Teardown wobbles."})
	rr = doJSON(t, handler, "POST", "/agent/v1/issues", token, issueBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("issue create: status = %d, body %s", rr.Code, rr.Body.String())
	}
	if fk.createdIssue == nil || fk.createdIssue.title != "found: flake" || fk.createdIssue.body != "Teardown wobbles." {
		t.Errorf("CreateIssue got %+v, want title/body verbatim", fk.createdIssue)
	}

	// Comment.
	commentBody := mustJSON(t, map[string]string{"body": "Working on it."})
	rr = doJSON(t, handler, "POST", "/agent/v1/issues/7/comments", token, commentBody)
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment: status = %d, body %s", rr.Code, rr.Body.String())
	}
	if len(fk.comments) != 1 || fk.comments[0] != (commentArgs{number: 7, body: "Working on it."}) {
		t.Errorf("CreateComment got %+v, want the clean body verbatim", fk.comments)
	}
}

// TestSecretScan_BuiltinBindingBlocked (criteria e, f): the SAME scan site
// covers the builtin binding — a run-token comment carrying a secret through
// the real store-backed tracker is a 400 naming the secret, and no comment row
// is written (proving the scan gated the store, not just a fake).
func TestSecretScan_BuiltinBindingBlocked(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a") // builtin binding
	f.seedIssue(t, "iss7", "repo_a", 7, "Fix it", "")
	f.seedRun(t, "run_afk", "repo_a", "active")
	token := f.seedToken(t, "run_afk", nil)
	const value = "sk-live-4f8a2b9c1d3e5f70"
	f.seedSecret(t, "repo_a", "DEPLOY_KEY", "", value)

	handler := f.scanBuiltinServer().Handler()

	body := mustJSON(t, map[string]string{"body": "pushed with " + value})
	rr := doJSON(t, handler, "POST", "/agent/v1/issues/7/comments", token, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "DEPLOY_KEY") {
		t.Errorf("400 body = %s, want it to name DEPLOY_KEY", rr.Body.String())
	}
	assertNoLeak(t, rr.Body.String(), value)

	// The store must carry no comment for issue 7 — the write never landed.
	is, err := f.st.IssueByRepoNumber(context.Background(), "repo_a", 7)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if len(is.Comments) != 0 {
		t.Errorf("issue 7 gained a comment despite the rejected leak: %+v", is.Comments)
	}
}

// TestSecretScan_FailClosed (criterion g): an undecryptable secret blob makes
// the scan fail CLOSED — the write is refused with a non-400 error (500 on the
// builtin binding, via internalError, since the failure is not a BlockedError)
// and never delegated. The opaque response carries neither the blob nor a 2xx.
func TestSecretScan_FailClosed(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a") // builtin binding
	f.seedIssue(t, "iss7", "repo_a", 7, "Fix it", "")
	f.seedRun(t, "run_afk", "repo_a", "active")
	token := f.seedToken(t, "run_afk", nil)

	// A garbage blob stored directly: CreateRepoSecret validates the NAME, not
	// the ciphertext, so this seeds a value the vault cannot decrypt.
	const blob = "garble-not-a-valid-ciphertext-blob"
	if _, err := f.st.CreateRepoSecret(context.Background(), "sec_garbage", "repo_a", "GARBAGE", "", []byte(blob), f.now); err != nil {
		t.Fatalf("CreateRepoSecret: %v", err)
	}

	handler := f.scanBuiltinServer().Handler()

	// Even a clean write is refused — the decrypt fails before any match.
	body := mustJSON(t, map[string]string{"body": "totally clean text"})
	rr := doJSON(t, handler, "POST", "/agent/v1/issues/7/comments", token, body)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (fail-closed on the builtin binding)", rr.Code)
	}
	if rr.Code == http.StatusOK || rr.Code == http.StatusCreated {
		t.Fatalf("fail-closed returned a success status %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), blob) {
		t.Errorf("fail-closed response leaked the blob: %s", rr.Body.String())
	}

	// And the write never landed.
	is, err := f.st.IssueByRepoNumber(context.Background(), "repo_a", 7)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if len(is.Comments) != 0 {
		t.Errorf("issue 7 gained a comment despite the fail-closed refusal: %+v", is.Comments)
	}
}
