package secretscan

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// --- fixture: migrated sqlite store + a real vault, mirroring agentapi's ---

type fixture struct {
	st  *store.Store
	db  *sql.DB
	vlt *vault.Vault
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lab.db")
	st, err := store.Open(context.Background(), "sqlite:"+path,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return &fixture{st: st, db: db, vlt: newTestVault(t), now: time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)}
}

func newTestVault(t *testing.T) *vault.Vault {
	t.Helper()
	key := make([]byte, vault.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	v, err := vault.New(key)
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	return v
}

func (f *fixture) seedRepo(t *testing.T, id string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO repos
		(id, name, remote_url, tracker_binding, forge_kind, default_branch, afk_branch_pattern, manual_branch_prefix, incogni, created_at)
		VALUES (?, ?, ?, 'builtin', 'none', 'main', 'afk/<N>', 'lab/', 0, ?)`,
		id, "repo-"+id, "https://example.invalid/r.git", f.now.Format("2006-01-02T15:04:05.000Z07:00"))
	if err != nil {
		t.Fatalf("seed repo: %v", err)
	}
}

// seedSecret encrypts value with the fixture vault and stores it — the operator
// `secret set` path (the store only ever sees ciphertext).
func (f *fixture) seedSecret(t *testing.T, repoID, name, value string) {
	t.Helper()
	blob, err := f.vlt.Encrypt([]byte(value))
	if err != nil {
		t.Fatalf("encrypt %s: %v", name, err)
	}
	if _, err := f.st.CreateRepoSecret(context.Background(), ids.NewID("sec"), repoID, name, "", blob, f.now); err != nil {
		t.Fatalf("CreateRepoSecret %s: %v", name, err)
	}
}

// seedGarbageSecret stores a blob that is NOT a valid vault ciphertext, so a
// later Decrypt fails — the fail-closed path.
func (f *fixture) seedGarbageSecret(t *testing.T, repoID, name string, blob []byte) {
	t.Helper()
	if _, err := f.st.CreateRepoSecret(context.Background(), ids.NewID("sec"), repoID, name, "", blob, f.now); err != nil {
		t.Fatalf("CreateRepoSecret %s: %v", name, err)
	}
}

func (f *fixture) resolver(inner Inner) *Resolver { return NewResolver(inner, f.st, f.vlt) }

func (f *fixture) trackerFor(t *testing.T, inner Inner, repoID string) tracker.Tracker {
	t.Helper()
	trk, err := f.resolver(inner).TrackerFor(context.Background(), store.Repo{ID: repoID, TrackerBinding: "builtin"})
	if err != nil {
		t.Fatalf("TrackerFor: %v", err)
	}
	return trk
}

// --- fakes ---

type commentCall struct {
	number int
	body   string
}
type issueCall struct {
	title, body string
	labels      []string
}
type editCall struct {
	number int
	edit   tracker.IssueEdit
}
type pullCall struct{ head, base, title, body string }

// recTracker records write args and returns canned values. It does NOT
// implement RunScoper.
type recTracker struct {
	comments     []commentCall
	issues       []issueCall
	edits        []editCall
	pulls        []pullCall
	pullComments []commentCall // CommentPull(number, body)

	issueRet tracker.Issue
	pullRet  tracker.PullRef
	issueGot tracker.Issue // returned by Issue()
}

func (r *recTracker) ReadyIssues(context.Context) ([]tracker.Issue, error) { return nil, nil }
func (r *recTracker) Issues(context.Context, string) ([]tracker.Issue, error) {
	return nil, nil
}
func (r *recTracker) Issue(context.Context, int) (tracker.Issue, error) { return r.issueGot, nil }
func (r *recTracker) CreateComment(_ context.Context, number int, body string) error {
	r.comments = append(r.comments, commentCall{number, body})
	return nil
}
func (r *recTracker) Pulls(context.Context) ([]tracker.PullRef, error) { return nil, nil }
func (r *recTracker) PullsForHead(context.Context, string, string) ([]tracker.PullRef, error) {
	return nil, nil
}
func (r *recTracker) Pull(context.Context, int) (tracker.PullDetail, error) {
	return tracker.PullDetail{}, nil
}
func (r *recTracker) Checks(context.Context, int) ([]tracker.Check, error)  { return nil, nil }
func (r *recTracker) CheckLog(context.Context, int, string) ([]byte, error) { return nil, nil }
func (r *recTracker) CreatePull(_ context.Context, head, base, title, body string) (tracker.PullRef, error) {
	r.pulls = append(r.pulls, pullCall{head, base, title, body})
	return r.pullRet, nil
}
func (r *recTracker) MergePull(context.Context, int) (tracker.PullRef, error) {
	return tracker.PullRef{}, nil
}
func (r *recTracker) Reviews(context.Context, int) ([]tracker.Review, error) { return nil, nil }
func (r *recTracker) RerequestReview(context.Context, int) error             { return nil }
func (r *recTracker) CommentPull(_ context.Context, number int, body string) error {
	r.pullComments = append(r.pullComments, commentCall{number, body})
	return nil
}
func (r *recTracker) PullComments(context.Context, int) ([]tracker.Comment, error) {
	return nil, nil
}
func (r *recTracker) CloseIssue(context.Context, int) error { return nil }
func (r *recTracker) CreateIssue(_ context.Context, title, body string, labels []string) (tracker.Issue, error) {
	r.issues = append(r.issues, issueCall{title, body, labels})
	return r.issueRet, nil
}
func (r *recTracker) EditIssue(_ context.Context, number int, edit tracker.IssueEdit) (tracker.Issue, error) {
	r.edits = append(r.edits, editCall{number, edit})
	return r.issueRet, nil
}
func (r *recTracker) AddIssueLabels(context.Context, int, []string) error    { return nil }
func (r *recTracker) RemoveIssueLabels(context.Context, int, []string) error { return nil }
func (r *recTracker) Labels(context.Context) ([]tracker.Label, error)        { return nil, nil }
func (r *recTracker) EnsureLabel(_ context.Context, name, color, description string) (tracker.Label, error) {
	return tracker.Label{Name: name, Color: color, Description: description}, nil
}

// scoper wraps a base recorder and implements RunScoper: ForRun records the run
// id and hands back a distinct rescoped recorder the test can inspect.
type scoper struct {
	*recTracker
	rescoped  *recTracker
	lastRunID string
}

func (s *scoper) ForRun(runID string) tracker.Tracker {
	s.lastRunID = runID
	return s.rescoped
}

// fakeInner adapts a fixed Tracker (and optional error) to Inner.
type fakeInner struct {
	trk tracker.Tracker
	err error
}

func (fi fakeInner) TrackerFor(context.Context, store.Repo) (tracker.Tracker, error) {
	return fi.trk, fi.err
}

// blockedErr type-asserts err to *BlockedError or fails the test.
func blockedErr(t *testing.T, err error) *BlockedError {
	t.Helper()
	if err == nil {
		t.Fatal("want *BlockedError, got nil")
	}
	be, ok := err.(*BlockedError)
	if !ok {
		t.Fatalf("want *BlockedError, got %T: %v", err, err)
	}
	return be
}

// --- tests ---

// 1. Zero-secret repo: all three writes delegate with byte-identical args.
func TestZeroSecretRepoDelegates(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_z")
	rec := &recTracker{}
	trk := f.trackerFor(t, fakeInner{trk: rec}, "repo_z")
	ctx := context.Background()

	if err := trk.CreateComment(ctx, 7, "just a status note"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if _, err := trk.CreateIssue(ctx, "a title", "a body", []string{"bug"}); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := trk.CreatePull(ctx, "afk/7", "main", "pr title", "pr body"); err != nil {
		t.Fatalf("pull: %v", err)
	}

	if len(rec.comments) != 1 || rec.comments[0] != (commentCall{7, "just a status note"}) {
		t.Errorf("comment args = %+v", rec.comments)
	}
	if len(rec.issues) != 1 || rec.issues[0].title != "a title" || rec.issues[0].body != "a body" ||
		len(rec.issues[0].labels) != 1 || rec.issues[0].labels[0] != "bug" {
		t.Errorf("issue args = %+v", rec.issues)
	}
	if len(rec.pulls) != 1 || rec.pulls[0] != (pullCall{"afk/7", "main", "pr title", "pr body"}) {
		t.Errorf("pull args = %+v", rec.pulls)
	}
}

// 2. Repo with secrets, clean writes: delegate byte-identical.
func TestCleanWritesDelegate(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedSecret(t, "repo_a", "DEPLOY_KEY", "s3cr3t-deploy-value")
	rec := &recTracker{}
	trk := f.trackerFor(t, fakeInner{trk: rec}, "repo_a")
	ctx := context.Background()

	if err := trk.CreateComment(ctx, 3, "no secrets here"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if _, err := trk.CreateIssue(ctx, "clean title", "clean body", nil); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := trk.CreatePull(ctx, "afk/3", "main", "clean pr", "clean pr body"); err != nil {
		t.Fatalf("pull: %v", err)
	}

	if len(rec.comments) != 1 || rec.comments[0].body != "no secrets here" {
		t.Errorf("comment args = %+v", rec.comments)
	}
	if len(rec.issues) != 1 || rec.issues[0].title != "clean title" || rec.issues[0].body != "clean body" {
		t.Errorf("issue args = %+v", rec.issues)
	}
	if len(rec.pulls) != 1 || rec.pulls[0] != (pullCall{"afk/3", "main", "clean pr", "clean pr body"}) {
		t.Errorf("pull args = %+v", rec.pulls)
	}
}

// 2b. EditIssue scans only the fields the patch SETS: a clean edit delegates
// byte-identical; a secret in a set title or body blocks and never reaches the
// inner tracker; an unset (nil) field is not scanned.
func TestEditIssueScansSetFields(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedSecret(t, "repo_a", "DEPLOY_KEY", "s3cr3t-deploy-value")
	rec := &recTracker{}
	trk := f.trackerFor(t, fakeInner{trk: rec}, "repo_a")
	ctx := context.Background()

	str := func(v string) *string { return &v }

	// Clean edit delegates byte-identical.
	if _, err := trk.EditIssue(ctx, 5, tracker.IssueEdit{Title: str("clean title"), Body: str("clean body")}); err != nil {
		t.Fatalf("clean edit: %v", err)
	}
	if len(rec.edits) != 1 || rec.edits[0].number != 5 ||
		rec.edits[0].edit.Title == nil || *rec.edits[0].edit.Title != "clean title" ||
		rec.edits[0].edit.Body == nil || *rec.edits[0].edit.Body != "clean body" {
		t.Fatalf("clean edit args = %+v", rec.edits)
	}

	// Secret in the set title blocks; inner untouched.
	_, err := trk.EditIssue(ctx, 5, tracker.IssueEdit{Title: str("leak s3cr3t-deploy-value")})
	be := blockedErr(t, err)
	if len(be.Matches) != 1 || be.Matches[0].Field != "title" || be.Matches[0].Secret != "DEPLOY_KEY" {
		t.Errorf("title matches = %+v", be.Matches)
	}

	// Secret in the set body blocks; inner untouched.
	_, err = trk.EditIssue(ctx, 5, tracker.IssueEdit{Body: str("here it is s3cr3t-deploy-value")})
	be = blockedErr(t, err)
	if len(be.Matches) != 1 || be.Matches[0].Field != "body" {
		t.Errorf("body matches = %+v", be.Matches)
	}

	// No blocked write ever reached the inner tracker.
	if len(rec.edits) != 1 {
		t.Errorf("inner EditIssue called on a blocked write: %+v", rec.edits)
	}
}

// 3. Comment body containing the exact value → BlockedError; inner NEVER called.
func TestCommentExactBlocks(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedSecret(t, "repo_a", "DEPLOY_KEY", "s3cr3t-deploy-value")
	rec := &recTracker{}
	trk := f.trackerFor(t, fakeInner{trk: rec}, "repo_a")

	err := trk.CreateComment(context.Background(), 1, "here it is: s3cr3t-deploy-value do not use")
	be := blockedErr(t, err)
	if len(be.Matches) != 1 || be.Matches[0].Secret != "DEPLOY_KEY" ||
		be.Matches[0].Field != "body" || be.Matches[0].Form != "exact" {
		t.Errorf("matches = %+v", be.Matches)
	}
	if len(rec.comments) != 0 {
		t.Errorf("inner CreateComment was called: %+v", rec.comments)
	}
}

// 3b. PR comment bodies (CommentPull — the seam the verdict verbs compose
// over, ADR-0048) are content-bearing writes: a clean body delegates
// byte-identical; a body carrying the secret blocks and never reaches the
// inner tracker. RerequestReview (no body) delegates untouched.
func TestReviewWritesScanBody(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedSecret(t, "repo_a", "DEPLOY_KEY", "s3cr3t-deploy-value")
	rec := &recTracker{}
	trk := f.trackerFor(t, fakeInner{trk: rec}, "repo_a")
	ctx := context.Background()

	// A clean PR comment delegates byte-identical.
	if err := trk.CommentPull(ctx, 4, "a discussion note"); err != nil {
		t.Fatalf("clean pull comment: %v", err)
	}
	if err := trk.RerequestReview(ctx, 4); err != nil {
		t.Fatalf("rerequest: %v", err)
	}
	if len(rec.pullComments) != 1 || rec.pullComments[0] != (commentCall{4, "a discussion note"}) {
		t.Errorf("pull comment args = %+v", rec.pullComments)
	}

	// A secret in the comment body blocks; inner never called for it.
	err := trk.CommentPull(ctx, 4, "fyi s3cr3t-deploy-value")
	be := blockedErr(t, err)
	if len(be.Matches) != 1 || be.Matches[0].Field != "body" || be.Matches[0].Secret != "DEPLOY_KEY" {
		t.Errorf("pull comment matches = %+v", be.Matches)
	}
	if len(rec.pullComments) != 1 {
		t.Errorf("a blocked pull comment reached the inner tracker: %+v", rec.pullComments)
	}
}

// 4. Encoded forms across the three writes and both fields: base64 in a PR body,
// exact in a PR title, hex in an issue title, url-encoded in an issue body.
func TestEncodedFormsBlockPerFieldAndWrite(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	// A value whose url-encoding differs from plaintext (has a slash/space) so
	// the url-encoded form is a distinct pattern.
	f.seedSecret(t, "repo_a", "TOKEN", "abc/def ghi=jkl")
	rec := &recTracker{}
	trk := f.trackerFor(t, fakeInner{trk: rec}, "repo_a")
	ctx := context.Background()

	value := "abc/def ghi=jkl"

	// PR body carries the base64 form.
	b64 := base64.StdEncoding.EncodeToString([]byte(value))
	err := func() error {
		_, e := trk.CreatePull(ctx, "afk/1", "main", "safe title", "deploy uses "+b64+" token")
		return e
	}()
	be := blockedErr(t, err)
	if len(be.Matches) != 1 || be.Matches[0].Field != "body" || be.Matches[0].Form != "base64" {
		t.Errorf("PR base64 body matches = %+v", be.Matches)
	}

	// PR title carries the exact value → Field "title".
	_, e := trk.CreatePull(ctx, "afk/1", "main", "leak "+value, "safe body")
	be = blockedErr(t, e)
	if len(be.Matches) != 1 || be.Matches[0].Field != "title" || be.Matches[0].Form != "exact" {
		t.Errorf("PR exact title matches = %+v", be.Matches)
	}

	// Issue title carries the hex form.
	hx := hex.EncodeToString([]byte(value))
	_, e = trk.CreateIssue(ctx, "config "+hx, "safe body", nil)
	be = blockedErr(t, e)
	if len(be.Matches) != 1 || be.Matches[0].Field != "title" || be.Matches[0].Form != "hex" {
		t.Errorf("issue hex title matches = %+v", be.Matches)
	}

	// Issue body carries the url-encoded form.
	ue := url.QueryEscape(value)
	_, e = trk.CreateIssue(ctx, "safe title", "value is "+ue+" here", nil)
	be = blockedErr(t, e)
	if len(be.Matches) != 1 || be.Matches[0].Field != "body" || be.Matches[0].Form != "urlencoded" {
		t.Errorf("issue url-encoded body matches = %+v", be.Matches)
	}

	if len(rec.pulls) != 0 || len(rec.issues) != 0 {
		t.Errorf("inner called on a blocked write: pulls=%+v issues=%+v", rec.pulls, rec.issues)
	}
}

// 5. Two different secrets in one body → both named, deterministic order.
func TestTwoSecretsDeterministicOrder(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedSecret(t, "repo_a", "DEPLOY_KEY", "zzz-deploy-value")
	f.seedSecret(t, "repo_a", "API_TOKEN", "aaa-api-value")
	rec := &recTracker{}
	trk := f.trackerFor(t, fakeInner{trk: rec}, "repo_a")

	// Body mentions DEPLOY_KEY's value first, API_TOKEN's second — the message
	// must still sort by secret name (API_TOKEN before DEPLOY_KEY).
	body := "first zzz-deploy-value then aaa-api-value"
	err := trk.CreateComment(context.Background(), 1, body)
	be := blockedErr(t, err)
	if len(be.Matches) != 2 {
		t.Fatalf("matches = %+v, want 2", be.Matches)
	}
	if be.Matches[0].Secret != "API_TOKEN" || be.Matches[1].Secret != "DEPLOY_KEY" {
		t.Errorf("order = %q, %q; want API_TOKEN, DEPLOY_KEY", be.Matches[0].Secret, be.Matches[1].Secret)
	}
	msg := be.Error()
	if !strings.Contains(msg, "API_TOKEN (body, exact)") || !strings.Contains(msg, "DEPLOY_KEY (body, exact)") {
		t.Errorf("message = %q, want both secrets named", msg)
	}
	if strings.Index(msg, "API_TOKEN") > strings.Index(msg, "DEPLOY_KEY") {
		t.Errorf("message order not deterministic: %q", msg)
	}
}

// 6. The rendered Error() never contains the plaintext value nor any of its
// base64/hex/url-encoded encodings.
func TestErrorMessageLeaksNoValue(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	value := "abc/def ghi=jkl"
	f.seedSecret(t, "repo_a", "TOKEN", value)
	rec := &recTracker{}
	trk := f.trackerFor(t, fakeInner{trk: rec}, "repo_a")

	encodings := []string{
		value,
		base64.StdEncoding.EncodeToString([]byte(value)),
		base64.RawStdEncoding.EncodeToString([]byte(value)),
		base64.URLEncoding.EncodeToString([]byte(value)),
		hex.EncodeToString([]byte(value)),
		strings.ToUpper(hex.EncodeToString([]byte(value))),
		url.QueryEscape(value),
		url.PathEscape(value),
	}

	// Exercise every field/write so each Error() rendering is checked.
	msgs := []string{}
	collect := func(err error) {
		be := blockedErr(t, err)
		msgs = append(msgs, be.Error())
	}
	collect(trk.CreateComment(context.Background(), 1, "x "+value))
	_, e := trk.CreateIssue(context.Background(), "t "+value, "b "+base64.StdEncoding.EncodeToString([]byte(value)), nil)
	collect(e)
	_, e = trk.CreatePull(context.Background(), "afk/1", "main", "t "+hex.EncodeToString([]byte(value)), "b "+url.QueryEscape(value))
	collect(e)

	for _, msg := range msgs {
		for _, enc := range encodings {
			if strings.Contains(msg, enc) {
				t.Errorf("Error() %q leaked an encoding of the value: %q", msg, enc)
			}
		}
		if !strings.Contains(msg, "TOKEN") {
			t.Errorf("Error() %q does not name the secret", msg)
		}
	}
}

// 7a. ForRun with a RunScoper inner: the rescoped scanner still blocks dirty
// writes and delegates clean ones to the RESCOPED inner.
func TestForRunRescopesAndScans(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedSecret(t, "repo_a", "DEPLOY_KEY", "s3cr3t-deploy-value")
	rescoped := &recTracker{}
	sc := &scoper{recTracker: &recTracker{}, rescoped: rescoped}
	trk := f.trackerFor(t, fakeInner{trk: sc}, "repo_a")

	rs, ok := trk.(tracker.RunScoper)
	if !ok {
		t.Fatal("scanner does not implement RunScoper")
	}
	scoped := rs.ForRun("run_xyz")
	if sc.lastRunID != "run_xyz" {
		t.Errorf("inner ForRun run id = %q, want run_xyz", sc.lastRunID)
	}

	// Clean write on the rescoped scanner delegates to the rescoped inner.
	if err := scoped.CreateComment(context.Background(), 5, "all clear"); err != nil {
		t.Fatalf("clean rescoped comment: %v", err)
	}
	if len(rescoped.comments) != 1 || rescoped.comments[0] != (commentCall{5, "all clear"}) {
		t.Errorf("rescoped inner comments = %+v", rescoped.comments)
	}
	if len(sc.comments) != 0 {
		t.Errorf("base (non-rescoped) inner got the comment: %+v", sc.comments)
	}

	// Dirty write on the rescoped scanner still blocks, inner untouched.
	err := scoped.CreateComment(context.Background(), 5, "leak s3cr3t-deploy-value")
	_ = blockedErr(t, err)
	if len(rescoped.comments) != 1 {
		t.Errorf("rescoped inner got a blocked write: %+v", rescoped.comments)
	}
}

// 7b. ForRun with an inner WITHOUT RunScoper: ForRun returns a scanner that
// still blocks dirty writes.
func TestForRunWithoutRunScoperStillScans(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedSecret(t, "repo_a", "DEPLOY_KEY", "s3cr3t-deploy-value")
	rec := &recTracker{} // no ForRun
	trk := f.trackerFor(t, fakeInner{trk: rec}, "repo_a")

	rs, ok := trk.(tracker.RunScoper)
	if !ok {
		t.Fatal("scanner does not implement RunScoper")
	}
	scoped := rs.ForRun("run_abc")

	err := scoped.CreateComment(context.Background(), 1, "leak s3cr3t-deploy-value")
	_ = blockedErr(t, err)
	if len(rec.comments) != 0 {
		t.Errorf("inner got a blocked write: %+v", rec.comments)
	}

	// And a clean write still delegates.
	if err := scoped.CreateComment(context.Background(), 1, "fine"); err != nil {
		t.Fatalf("clean write: %v", err)
	}
	if len(rec.comments) != 1 {
		t.Errorf("clean write not delegated: %+v", rec.comments)
	}
}

// 8. A read delegates untouched (never scanned, never blocked).
func TestReadDelegates(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedSecret(t, "repo_a", "DEPLOY_KEY", "s3cr3t-deploy-value")
	// Even an issue whose body IS the secret comes back untouched on a read.
	rec := &recTracker{issueGot: tracker.Issue{Number: 9, Title: "t", Body: "s3cr3t-deploy-value"}}
	trk := f.trackerFor(t, fakeInner{trk: rec}, "repo_a")

	got, err := trk.Issue(context.Background(), 9)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if got.Number != 9 || got.Body != "s3cr3t-deploy-value" {
		t.Errorf("issue = %+v, want the inner's value verbatim", got)
	}
}

// 9. Decrypt failure → fail closed: a non-BlockedError error, inner never
// called, message carries no blob bytes.
func TestDecryptFailureFailsClosed(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	garbage := []byte("this-is-not-a-valid-vault-blob-XYZ")
	f.seedGarbageSecret(t, "repo_a", "BROKEN", garbage)
	rec := &recTracker{}
	trk := f.trackerFor(t, fakeInner{trk: rec}, "repo_a")

	err := trk.CreateComment(context.Background(), 1, "harmless body")
	if err == nil {
		t.Fatal("want a fail-closed error, got nil")
	}
	if _, ok := err.(*BlockedError); ok {
		t.Fatalf("got *BlockedError, want a plain fail-closed error: %v", err)
	}
	if len(rec.comments) != 0 {
		t.Errorf("inner was called on a fail-closed write: %+v", rec.comments)
	}
	if strings.Contains(err.Error(), string(garbage)) {
		t.Errorf("error leaked blob bytes: %q", err.Error())
	}
}

// Resolver propagates an inner resolution error unwrapped (nothing to scan).
func TestResolverPropagatesInnerError(t *testing.T) {
	f := newFixture(t)
	_, err := f.resolver(fakeInner{err: context.Canceled}).
		TrackerFor(context.Background(), store.Repo{ID: "repo_a"})
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
