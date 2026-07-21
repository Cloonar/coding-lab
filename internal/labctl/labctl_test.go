package labctl

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/agentapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/builtin"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

func run(t *testing.T, args []string, env map[string]string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errw strings.Builder
	code = Run(args, Env{
		Getenv:  func(k string) string { return env[k] },
		Stdout:  &out,
		Stderr:  &errw,
		Version: "1.2.3-test",
	})
	return code, out.String(), errw.String()
}

var agentEnv = map[string]string{
	"LAB_URL":   "http://127.0.0.1:8080",
	"LAB_TOKEN": "lab_run_x",
}

// TestRunCommandSurface pins the argument parsing and the usage exit code (2)
// with no server in play.
func TestRunCommandSurface(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		env        map[string]string
		wantCode   int
		wantStdout string // substring
		wantStderr string // substring
	}{
		{"no args", nil, nil, 2, "", "Usage"},
		{"version", []string{"--version"}, nil, 0, "labctl 1.2.3-test", ""},
		{"help", []string{"help"}, nil, 0, "Usage", ""},
		{"unknown command", []string{"bogus"}, nil, 2, "", `unknown command "bogus"`},
		{"issue no sub", []string{"issue"}, nil, 2, "", "Usage"},
		{"issue unknown sub", []string{"issue", "bogus"}, nil, 2, "", `unknown subcommand "bogus"`},

		{"issue view bad n", []string{"issue", "view", "twelve"}, agentEnv, 2, "", "not an integer"},
		{"issue view too many", []string{"issue", "view", "1", "2"}, agentEnv, 2, "", "too many arguments"},
		{"issue list too many", []string{"issue", "list", "x"}, agentEnv, 2, "", "too many arguments"},
		{"issue comment missing body", []string{"issue", "comment", "5"}, agentEnv, 2, "", "want <n> <body>"},
		{"issue comment bad n", []string{"issue", "comment", "five", "hi"}, agentEnv, 2, "", "not an integer"},
		{"issue comment empty body", []string{"issue", "comment", "5", ""}, agentEnv, 2, "", "body must not be empty"},
		{"pr create missing title", []string{"pr", "create", "--body", "b"}, agentEnv, 2, "", "--title is required"},
		{"pr create missing body", []string{"pr", "create", "--title", "t"}, agentEnv, 2, "", "--body is required"},
		{"pr without create", []string{"pr"}, agentEnv, 2, "", "Usage"},
		{"pr unknown sub", []string{"pr", "bogus"}, agentEnv, 2, "", `unknown subcommand "bogus"`},
		{"pr view missing n", []string{"pr", "view"}, agentEnv, 2, "", "want <n>"},
		{"pr view too many", []string{"pr", "view", "1", "2"}, agentEnv, 2, "", "want <n>"},
		{"pr view bad n", []string{"pr", "view", "twelve"}, agentEnv, 2, "", "not an integer"},
		{"pr list too many", []string{"pr", "list", "x"}, agentEnv, 2, "", "too many arguments"},

		// `pr checks` OVERRIDES the binary-wide usage code: a usage error is 1,
		// not 2 (2 is reserved for a red aggregate), and still prints usage.
		{"pr checks no n", []string{"pr", "checks"}, agentEnv, 1, "", "Usage"},
		{"pr checks bad n", []string{"pr", "checks", "twelve"}, agentEnv, 1, "", "not a positive integer"},
		{"pr checks too many", []string{"pr", "checks", "1", "2"}, agentEnv, 1, "", "want <n>"},

		// secret surface: every mis-shape is the usage code 2 (the CHILD's exit
		// passes through only once a well-formed exec actually runs).
		{"secret no sub", []string{"secret"}, agentEnv, 2, "", "Usage"},
		{"secret unknown sub", []string{"secret", "bogus"}, agentEnv, 2, "", `unknown subcommand "bogus"`},
		{"secret list too many", []string{"secret", "list", "extra"}, agentEnv, 2, "", "too many arguments"},
		{"secret exec no args", []string{"secret", "exec"}, agentEnv, 2, "", "separator is required"},
		{"secret exec no sep", []string{"secret", "exec", "FOO", "echo"}, agentEnv, 2, "", "separator is required"},
		{"secret exec no names", []string{"secret", "exec", "--", "echo"}, agentEnv, 2, "", "at least one secret NAME"},
		{"secret exec no cmd", []string{"secret", "exec", "FOO", "--"}, agentEnv, 2, "", "want a command after --"},
		{"secret exec missing env", []string{"secret", "exec", "FOO", "--", "echo", "hi"}, nil, 2, "", "LAB_URL and LAB_TOKEN must be set"},
		// `secret scan` deviates AFTER usage (any failure or finding is 1, fail
		// closed), but its shape/env errors keep the binary-wide usage code 2.
		{"secret scan no args", []string{"secret", "scan"}, agentEnv, 2, "", "want <rev-arg>"},
		{"secret scan missing env", []string{"secret", "scan", "HEAD"}, nil, 2, "", "LAB_URL and LAB_TOKEN must be set"},

		{"issue create missing title", []string{"issue", "create", "--body", "b"}, agentEnv, 2, "", "--title is required"},
		{"issue create missing body", []string{"issue", "create", "--title", "t"}, agentEnv, 2, "", "--body is required"},
		{"issue create stray args", []string{"issue", "create", "--title", "t", "--body", "b", "x"}, agentEnv, 2, "", "unexpected arguments"},
		{"issue edit missing n", []string{"issue", "edit"}, agentEnv, 2, "", "want <n>"},
		{"issue edit bad n", []string{"issue", "edit", "one", "--title", "t"}, agentEnv, 2, "", "not an integer"},
		{"issue edit no flags", []string{"issue", "edit", "1"}, agentEnv, 2, "", "want at least one of --title or --body"},
		{"issue edit empty title", []string{"issue", "edit", "1", "--title", ""}, agentEnv, 2, "", "title must not be empty"},
		{"issue edit stray args", []string{"issue", "edit", "1", "--title", "t", "x"}, agentEnv, 2, "", "unexpected arguments"},
		{"issue label no op", []string{"issue", "label"}, agentEnv, 2, "", "want add|remove"},
		{"issue label bad op", []string{"issue", "label", "toggle", "1", "bug"}, agentEnv, 2, "", "want add|remove"},
		{"issue label add missing labels", []string{"issue", "label", "add", "1"}, agentEnv, 2, "", "want <n> <labels>"},
		{"issue label add bad n", []string{"issue", "label", "add", "one", "bug"}, agentEnv, 2, "", "not an integer"},
		{"issue label add empty labels", []string{"issue", "label", "add", "1", " , "}, agentEnv, 2, "", "labels must not be empty"},
		{"issue close missing n", []string{"issue", "close"}, agentEnv, 2, "", "want <n>"},
		{"issue close bad n", []string{"issue", "close", "seven"}, agentEnv, 2, "", "not an integer"},
		{"label no sub", []string{"label"}, agentEnv, 2, "", "Usage"},
		{"label unknown sub", []string{"label", "bogus"}, agentEnv, 2, "", `unknown subcommand "bogus"`},
		{"label list too many", []string{"label", "list", "x"}, agentEnv, 2, "", "too many arguments"},
		{"label create missing name", []string{"label", "create", "--color", "#fff"}, agentEnv, 2, "", "--name is required"},

		{"missing env", []string{"issue", "view"}, nil, 2, "", "LAB_URL and LAB_TOKEN must be set"},
		{"missing env shows usage", []string{"issue", "view"}, nil, 2, "", "Usage"},
		{"missing token only", []string{"issue", "list"}, map[string]string{"LAB_URL": "http://x"}, 2, "", "LAB_URL and LAB_TOKEN must be set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := run(t, tt.args, tt.env)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, tt.wantCode, stderr)
			}
			if tt.wantStdout != "" && !strings.Contains(stdout, tt.wantStdout) {
				t.Fatalf("stdout = %q, want it to contain %q", stdout, tt.wantStdout)
			}
			if tt.wantStderr != "" && !strings.Contains(stderr, tt.wantStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
			if tt.wantStderr == "" && stderr != "" {
				t.Fatalf("unexpected stderr output: %q", stderr)
			}
		})
	}
}

// --- transport tests against a real agentapi over httptest ------------------

var fixedNow = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

// agentFixture is a real store + real agent API served over httptest; labctl
// talks to it exactly like a session would (LAB_URL/LAB_TOKEN + Bearer).
type agentFixture struct {
	st    *store.Store
	vlt   *vault.Vault
	repo  string
	url   string
	token string
	runID string
}

type resolverFunc func(ctx context.Context, repo store.Repo) (tracker.Tracker, error)

func (f resolverFunc) TrackerFor(ctx context.Context, repo store.Repo) (tracker.Tracker, error) {
	return f(ctx, repo)
}

// fakeForge is a minimal recording tracker for the forge-bound PR path.
type fakeForge struct {
	pullRef      tracker.PullRef
	createdPull  *[4]string // head, base, title, body
	pulls        []tracker.PullRef
	pullDetail   *tracker.PullDetail // served by Pull when the number matches
	mergeRef     tracker.PullRef     // returned by MergePull on success
	mergeErr     error               // returned by MergePull when set
	mergedNumber int                 // records the last MergePull argument
	checks       []tracker.Check     // returned by Checks (single-shot tests)
	checksSeq    [][]tracker.Check   // consecutive Checks() results (--wait); overrides checks when set
	checksCall   int                 // index into checksSeq

	reviews []tracker.Review // returned by Reviews

	rerequestErr  error  // returned by RerequestReview when set
	rerequestedN  int    // records the last RerequestReview argument
	commentErr    error  // returned by CommentPull when set
	commentedN    int    // records the last CommentPull number argument
	commentedBody string // records the last CommentPull body argument
}

func (f *fakeForge) ReadyIssues(context.Context) ([]tracker.Issue, error) {
	return []tracker.Issue{}, nil
}
func (f *fakeForge) Issues(context.Context, string) ([]tracker.Issue, error) {
	return []tracker.Issue{}, nil
}
func (f *fakeForge) Issue(context.Context, int) (tracker.Issue, error) {
	return tracker.Issue{}, tracker.ErrNotFound
}
func (f *fakeForge) CreateComment(context.Context, int, string) error { return nil }
func (f *fakeForge) Pulls(context.Context) ([]tracker.PullRef, error) {
	if f.pulls == nil {
		return []tracker.PullRef{}, nil
	}
	return f.pulls, nil
}

// PullsForHead filters the scripted pulls by head; the rows carry no base, so
// base always matches.
func (f *fakeForge) PullsForHead(_ context.Context, head, _ string) ([]tracker.PullRef, error) {
	out := make([]tracker.PullRef, 0, len(f.pulls))
	for _, p := range f.pulls {
		if p.HeadBranch == head {
			out = append(out, p)
		}
	}
	return out, nil
}
func (f *fakeForge) Pull(_ context.Context, number int) (tracker.PullDetail, error) {
	if f.pullDetail != nil && f.pullDetail.Number == number {
		return *f.pullDetail, nil
	}
	// Shaped like the real client's upstream-404: wraps the typed sentinel.
	return tracker.PullDetail{}, fmt.Errorf("forgejo GET /repos/o/r/pulls/%d: unexpected status 404: %w", number, tracker.ErrNotFound)
}
func (f *fakeForge) Checks(context.Context, int) ([]tracker.Check, error) {
	if f.checksSeq != nil {
		// Serve the next scripted result; clamp to the last so extra polls keep
		// seeing the terminal state.
		i := f.checksCall
		if i >= len(f.checksSeq) {
			i = len(f.checksSeq) - 1
		}
		f.checksCall++
		return f.checksSeq[i], nil
	}
	return f.checks, nil
}
func (f *fakeForge) CreatePull(_ context.Context, head, base, title, body string) (tracker.PullRef, error) {
	f.createdPull = &[4]string{head, base, title, body}
	return f.pullRef, nil
}
func (f *fakeForge) MergePull(_ context.Context, number int) (tracker.PullRef, error) {
	f.mergedNumber = number
	if f.mergeErr != nil {
		return tracker.PullRef{}, f.mergeErr
	}
	return f.mergeRef, nil
}
func (f *fakeForge) Reviews(context.Context, int) ([]tracker.Review, error) {
	if f.reviews == nil {
		return []tracker.Review{}, nil
	}
	return f.reviews, nil
}
func (f *fakeForge) RerequestReview(_ context.Context, number int) error {
	f.rerequestedN = number
	return f.rerequestErr
}
func (f *fakeForge) CommentPull(_ context.Context, number int, body string) error {
	f.commentedN, f.commentedBody = number, body
	return f.commentErr
}
func (f *fakeForge) PullComments(context.Context, int) ([]tracker.Comment, error) {
	return nil, nil
}
func (f *fakeForge) CloseIssue(context.Context, int) error { return nil }
func (f *fakeForge) CreateIssue(context.Context, string, string, []string) (tracker.Issue, error) {
	return tracker.Issue{}, nil
}
func (f *fakeForge) EditIssue(context.Context, int, tracker.IssueEdit) (tracker.Issue, error) {
	return tracker.Issue{}, nil
}
func (f *fakeForge) AddIssueLabels(context.Context, int, []string) error    { return nil }
func (f *fakeForge) RemoveIssueLabels(context.Context, int, []string) error { return nil }
func (f *fakeForge) Labels(context.Context) ([]tracker.Label, error)        { return nil, nil }
func (f *fakeForge) EnsureLabel(context.Context, string, string, string) (tracker.Label, error) {
	return tracker.Label{}, nil
}

// newAgentFixture seeds one repo (binding + resolver as given), one issue
// (#1, with an operator comment and the ready-for-agent label), one active
// AFK run claiming it, and that run's token.
func newAgentFixture(t *testing.T, binding string, resolver agentapi.TrackerResolver) *agentFixture {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lab.db")
	st, err := store.Open(ctx, "sqlite:"+path, logx.New(io.Discard))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	forgeKind := "none"
	if binding == store.TrackerBindingForge {
		forgeKind = "forgejo"
	}
	repo, err := st.CreateRepo(ctx, store.Repo{
		ID:                 ids.NewID("repo"),
		Name:               "proj",
		RemoteURL:          "https://example.invalid/r.git",
		TrackerBinding:     binding,
		ForgeKind:          forgeKind,
		DefaultBranch:      "main",
		AFKBranchPattern:   "afk/<N>",
		ManualBranchPrefix: "lab/",
		CloneStatus:        store.CloneStatusReady,
		CreatedAt:          fixedNow,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}

	labels, err := st.LabelsByRepo(ctx, repo.ID)
	if err != nil {
		t.Fatalf("LabelsByRepo: %v", err)
	}
	var readyID string
	for _, l := range labels {
		if l.Name == tracker.ReadyLabel {
			readyID = l.ID
		}
	}
	is, err := st.CreateIssueWithLabels(ctx, repo.ID, "Fix the frobnicator", "It wobbles.", []string{readyID}, store.CommentAuthorOperator, nil, fixedNow)
	if err != nil {
		t.Fatalf("CreateIssueWithLabels: %v", err)
	}
	if _, err := st.CreateIssueComment(ctx, is.ID, store.CommentAuthorOperator, nil, "please fix", fixedNow); err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}

	runID := ids.NewID("run")
	n := is.Number
	if _, err := st.CreateRun(ctx, store.Run{
		ID:           runID,
		RepoID:       repo.ID,
		Kind:         store.RunKindAFKManual,
		Provider:     "claude-code",
		IssueNumber:  &n,
		Branch:       "afk/1",
		WorktreePath: "/tmp/wt",
		SessionName:  "proj~afk-1",
		Model:        "opus[1m]",
		Effort:       "max",
		StartedAt:    fixedNow,
		Outcome:      store.RunOutcomeActive,
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	token, hash := ids.NewToken("run")
	if err := st.CreateRunToken(ctx, runID, hash, nil, fixedNow); err != nil {
		t.Fatalf("CreateRunToken: %v", err)
	}

	vlt := newTestVault(t)
	handler := agentapi.New(st, vlt, resolver, nil, nil, func() time.Time { return fixedNow }, nil).Handler()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return &agentFixture{st: st, vlt: vlt, repo: repo.ID, url: ts.URL, token: token, runID: runID}
}

// newTestVault seals a Vault under a random 32-byte key — the same key the
// fixture's agentapi Server decrypts secret blobs with.
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

// seedSecret encrypts value with the fixture's vault and stores it under repo,
// returning the secret's id (for a later rotation). The store only ever holds
// ciphertext.
func (f *agentFixture) seedSecret(t *testing.T, name, description, value string) string {
	t.Helper()
	blob, err := f.vlt.Encrypt([]byte(value))
	if err != nil {
		t.Fatalf("encrypt %s: %v", name, err)
	}
	sec, err := f.st.CreateRepoSecret(context.Background(), ids.NewID("sec"), f.repo, name, description, blob, fixedNow)
	if err != nil {
		t.Fatalf("CreateRepoSecret %s: %v", name, err)
	}
	return sec.ID
}

// testShell returns a POSIX sh usable as an absolute exec target, or skips the
// test if none is available. `sh` is not on the sandbox PATH, so LookPath is
// tried first and /bin/sh is the fallback (it is a real symlink here).
func testShell(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("sh"); err == nil {
		return p
	}
	if _, err := exec.LookPath("/bin/sh"); err == nil {
		return "/bin/sh"
	}
	t.Skip("no sh available for secret exec test")
	return ""
}

func builtinResolver(st **store.Store) agentapi.TrackerResolver {
	return resolverFunc(func(_ context.Context, repo store.Repo) (tracker.Tracker, error) {
		return builtin.New(tracker.BuiltinConfig{Store: *st, RepoID: repo.ID}), nil
	})
}

func newBuiltinFixture(t *testing.T) *agentFixture {
	var st *store.Store
	f := newAgentFixture(t, store.TrackerBindingBuiltin, builtinResolver(&st))
	st = f.st
	return f
}

func (f *agentFixture) env() map[string]string {
	return map[string]string{"LAB_URL": f.url, "LAB_TOKEN": f.token}
}

const wantIssueView = `#1 Fix the frobnicator
state: open
labels: ready-for-agent

It wobbles.

--- comment by operator (2026-07-06T12:00:00.000Z)
please fix
`

func TestIssueViewClaimed(t *testing.T) {
	f := newBuiltinFixture(t)
	code, stdout, stderr := run(t, []string{"issue", "view"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != wantIssueView {
		t.Errorf("stdout =\n%q\nwant\n%q", stdout, wantIssueView)
	}
}

func TestIssueViewByNumber(t *testing.T) {
	f := newBuiltinFixture(t)
	code, stdout, stderr := run(t, []string{"issue", "view", "1"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != wantIssueView {
		t.Errorf("stdout =\n%q\nwant\n%q", stdout, wantIssueView)
	}

	// A number that names no issue: exit 1 with the API's message.
	code, stdout, stderr = run(t, []string{"issue", "view", "99"}, f.env())
	if code != 1 || stdout != "" {
		t.Fatalf("exit = %d, stdout %q, want 1 with empty stdout", code, stdout)
	}
	if !strings.Contains(stderr, "labctl issue view: not found") {
		t.Errorf("stderr = %q, want the not-found message", stderr)
	}
}

func TestIssueListOutput(t *testing.T) {
	f := newBuiltinFixture(t)
	// A second open issue; the list is number-DESC (store contract).
	repo := f.repoID(t)
	if _, err := f.st.CreateIssue(context.Background(), repo, "Add the sprocket", "", fixedNow); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}

	code, stdout, stderr := run(t, []string{"issue", "list"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	// number, state, created-at, comma-joined labels (empty column when
	// unlabeled), title — the triage buckets come from this one call.
	want := "#2\topen\t2026-07-06T12:00:00.000Z\t\tAdd the sprocket\n" +
		"#1\topen\t2026-07-06T12:00:00.000Z\tready-for-agent\tFix the frobnicator\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func (f *agentFixture) repoID(t *testing.T) string {
	t.Helper()
	repos, err := f.st.Repos(context.Background())
	if err != nil || len(repos) != 1 {
		t.Fatalf("Repos: %v (%d)", err, len(repos))
	}
	return repos[0].ID
}

func TestIssueCommentPostsAsRun(t *testing.T) {
	f := newBuiltinFixture(t)
	code, stdout, stderr := run(t, []string{"issue", "comment", "1", "looks good"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty (parseable silence on success)", stdout)
	}

	is, err := f.st.IssueByRepoNumber(context.Background(), f.repoID(t), 1)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	var runComment *store.IssueComment
	for i := range is.Comments {
		if is.Comments[i].AuthorKind == store.CommentAuthorRun {
			runComment = &is.Comments[i]
		}
	}
	if runComment == nil {
		t.Fatalf("no run-authored comment landed; comments = %+v", is.Comments)
	}
	if runComment.RunID == nil || *runComment.RunID != f.runID {
		t.Errorf("run comment RunID = %v, want %s", runComment.RunID, f.runID)
	}
	if runComment.Body != "looks good" {
		t.Errorf("comment body = %q", runComment.Body)
	}
}

// TestIssueCommentJoinsMultiWordBody pins that an unquoted multi-word body is
// joined with spaces and posted verbatim (an agent typing natural phrasing,
// not a single quoted argument).
func TestIssueCommentJoinsMultiWordBody(t *testing.T) {
	f := newBuiltinFixture(t)
	code, _, stderr := run(t, []string{"issue", "comment", "1", "tests", "are", "green"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	is, err := f.st.IssueByRepoNumber(context.Background(), f.repoID(t), 1)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	var runComment *store.IssueComment
	for i := range is.Comments {
		if is.Comments[i].AuthorKind == store.CommentAuthorRun {
			runComment = &is.Comments[i]
		}
	}
	if runComment == nil {
		t.Fatalf("no run-authored comment landed; comments = %+v", is.Comments)
	}
	if runComment.Body != "tests are green" {
		t.Errorf("comment body = %q, want %q", runComment.Body, "tests are green")
	}
}

// TestIssueCreateFilesRunAttributedIssue drives the mid-run follow-up flow:
// `labctl issue create` files a labelled issue, prints its number, and the
// builtin store attributes it to the run.
func TestIssueCreateFilesRunAttributedIssue(t *testing.T) {
	f := newBuiltinFixture(t)
	code, stdout, stderr := run(t, []string{"issue", "create",
		"--title", "found: flaky teardown", "--body", "Discovered mid-run.",
		"--labels", "needs-triage, wontfix"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "2\n" {
		t.Errorf("stdout = %q, want the created issue number", stdout)
	}

	is, err := f.st.IssueByRepoNumber(context.Background(), f.repoID(t), 2)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if is.Title != "found: flaky teardown" || is.Body != "Discovered mid-run." {
		t.Errorf("stored issue = %+v", is)
	}
	if len(is.Labels) != 2 || is.Labels[0] != "needs-triage" || is.Labels[1] != "wontfix" {
		t.Errorf("stored labels = %v, want the two comma-split names", is.Labels)
	}
	if is.AuthorKind != store.CommentAuthorRun || is.RunID == nil || *is.RunID != f.runID {
		t.Errorf("author = (%q, %v), want (run, %s)", is.AuthorKind, is.RunID, f.runID)
	}

	// A typo'd label: the API's 400 → exit 1 naming the label.
	code, _, stderr = run(t, []string{"issue", "create",
		"--title", "doomed", "--body", "b", "--labels", "no-such"}, f.env())
	if code != 1 || !strings.Contains(stderr, "no-such") {
		t.Fatalf("unknown label: exit = %d, stderr %q, want 1 naming the label", code, stderr)
	}
}

// TestIssueEditCommand drives `labctl issue edit` against the builtin store: a
// title-only patch and a body-only patch each print the updated issue through
// printIssue and leave the omitted field untouched, `--body ""` clears the body
// (a follow-up view shows it empty), and an unknown issue number surfaces the
// API's 404 as exit 1.
func TestIssueEditCommand(t *testing.T) {
	f := newBuiltinFixture(t)
	ctx := context.Background()

	// Title only: printIssue renders the renamed issue; the seeded body stays.
	code, stdout, stderr := run(t, []string{"issue", "edit", "1", "--title", "Renamed"}, f.env())
	if code != 0 {
		t.Fatalf("title edit: exit = %d, stderr %q", code, stderr)
	}
	if !strings.HasPrefix(stdout, "#1 Renamed\n") || !strings.Contains(stdout, "It wobbles.") {
		t.Errorf("stdout = %q, want the renamed issue with the kept body", stdout)
	}
	is, _ := f.st.IssueByRepoNumber(ctx, f.repoID(t), 1)
	if is.Title != "Renamed" || is.Body != "It wobbles." {
		t.Errorf("stored = title %q body %q, want Renamed / It wobbles.", is.Title, is.Body)
	}

	// Body only: the title from the prior edit is kept.
	code, stdout, stderr = run(t, []string{"issue", "edit", "1", "--body", "Now settled."}, f.env())
	if code != 0 {
		t.Fatalf("body edit: exit = %d, stderr %q", code, stderr)
	}
	if !strings.Contains(stdout, "Now settled.") {
		t.Errorf("stdout = %q, want the new body", stdout)
	}
	is, _ = f.st.IssueByRepoNumber(ctx, f.repoID(t), 1)
	if is.Title != "Renamed" || is.Body != "Now settled." {
		t.Errorf("stored = title %q body %q, want Renamed / Now settled.", is.Title, is.Body)
	}

	// Clear the body with --body "": a follow-up view shows an empty body.
	code, _, stderr = run(t, []string{"issue", "edit", "1", "--body", ""}, f.env())
	if code != 0 {
		t.Fatalf("clear body: exit = %d, stderr %q", code, stderr)
	}
	is, _ = f.st.IssueByRepoNumber(ctx, f.repoID(t), 1)
	if is.Body != "" || is.Title != "Renamed" {
		t.Errorf("after clear = title %q body %q, want body cleared and title kept", is.Title, is.Body)
	}
	code, stdout, stderr = run(t, []string{"issue", "view", "1"}, f.env())
	if code != 0 {
		t.Fatalf("view after clear: exit = %d, stderr %q", code, stderr)
	}
	// Empty body renders as the blank region between the labels line and the
	// seeded operator comment (printIssue's "\n%s\n" over ""); the old body text
	// is gone.
	if !strings.HasPrefix(stdout, "#1 Renamed\nstate: open\nlabels: ready-for-agent\n\n\n") {
		t.Errorf("view after clear = %q, want the renamed issue with an empty body", stdout)
	}
	if strings.Contains(stdout, "It wobbles.") || strings.Contains(stdout, "Now settled.") {
		t.Errorf("view after clear = %q, still carries an old body", stdout)
	}

	// Unknown issue number → the API's 404 → exit 1 with the message.
	code, stdout, stderr = run(t, []string{"issue", "edit", "99", "--title", "x"}, f.env())
	if code != 1 || stdout != "" {
		t.Fatalf("unknown issue: exit = %d, stdout %q, want 1 with empty stdout", code, stdout)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr = %q, want the not-found message", stderr)
	}
}

// TestIssueLabelAddRemoveAndClose drives the triage moves end-to-end: role
// labels on, role swap off, close after the explanation comment.
func TestIssueLabelAddRemoveAndClose(t *testing.T) {
	f := newBuiltinFixture(t)
	repo := f.repoID(t)
	ctx := context.Background()

	code, stdout, stderr := run(t, []string{"issue", "label", "add", "1", "needs-triage"}, f.env())
	if code != 0 || stdout != "" {
		t.Fatalf("label add: exit = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	is, _ := f.st.IssueByRepoNumber(ctx, repo, 1)
	if len(is.Labels) != 2 || is.Labels[0] != "needs-triage" || is.Labels[1] != "ready-for-agent" {
		t.Fatalf("labels after add = %v", is.Labels)
	}

	code, _, stderr = run(t, []string{"issue", "label", "remove", "1", "needs-triage,ready-for-agent"}, f.env())
	if code != 0 {
		t.Fatalf("label remove: exit = %d, stderr %q", code, stderr)
	}
	is, _ = f.st.IssueByRepoNumber(ctx, repo, 1)
	if len(is.Labels) != 0 {
		t.Fatalf("labels after remove = %v, want none", is.Labels)
	}

	code, stdout, stderr = run(t, []string{"issue", "close", "1"}, f.env())
	if code != 0 || stdout != "" {
		t.Fatalf("close: exit = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	is, _ = f.st.IssueByRepoNumber(ctx, repo, 1)
	if is.State != store.IssueStateClosed {
		t.Errorf("state after close = %q, want closed", is.State)
	}

	code, _, stderr = run(t, []string{"issue", "close", "99"}, f.env())
	if code != 1 || !strings.Contains(stderr, "not found") {
		t.Fatalf("close missing: exit = %d, stderr %q, want 1 not found", code, stderr)
	}
}

// TestLabelListAndCreate pins the label surface: the ensure prints the label
// that exists afterwards (retry-safe), and list renders name/color/description
// rows.
func TestLabelListAndCreate(t *testing.T) {
	f := newBuiltinFixture(t)

	code, stdout, stderr := run(t, []string{"label", "create", "--name", "bug", "--description", "confirmed defect"}, f.env())
	if code != 0 {
		t.Fatalf("label create: exit = %d, stderr %q", code, stderr)
	}
	if stdout != "bug\t"+store.LabelDefaultColor+"\n" {
		t.Errorf("stdout = %q, want name<TAB>color", stdout)
	}
	// The unconditional re-ensure: same exit 0, the existing label.
	code, stdout, _ = run(t, []string{"label", "create", "--name", "bug", "--color", "#ff0000"}, f.env())
	if code != 0 || stdout != "bug\t"+store.LabelDefaultColor+"\n" {
		t.Fatalf("re-ensure: exit = %d, stdout %q, want the existing label untouched", code, stdout)
	}

	code, stdout, stderr = run(t, []string{"label", "list"}, f.env())
	if code != 0 {
		t.Fatalf("label list: exit = %d, stderr %q", code, stderr)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 6 { // five seeded triage labels + bug
		t.Fatalf("label list = %d lines (%q), want 6", len(lines), stdout)
	}
	if lines[0] != "bug\t"+store.LabelDefaultColor+"\tconfirmed defect" {
		t.Errorf("first row = %q, want the bug label row", lines[0])
	}
}

func TestPRCreateOutput(t *testing.T) {
	fk := &fakeForge{pullRef: tracker.PullRef{Number: 12, URL: "https://forge.example/pr/12"}}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "create", "--title", "feat: fix", "--body", "Did it."}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "12\thttps://forge.example/pr/12\n" {
		t.Errorf("stdout = %q, want number<TAB>url", stdout)
	}
	if fk.createdPull == nil {
		t.Fatal("CreatePull not called")
	}
	if fk.createdPull[0] != "afk/1" || fk.createdPull[1] != "main" {
		t.Errorf("head/base = %q/%q, want afk/1/main", fk.createdPull[0], fk.createdPull[1])
	}
	// The server injects the pinned Closes trailer for AFK runs.
	if fk.createdPull[3] != "Did it.\n\nCloses #1" {
		t.Errorf("PR body = %q, want the injected Closes #1", fk.createdPull[3])
	}
}

// TestPRCreateBuiltinChangeRequest: since M6 a builtin-bound `labctl pr
// create` opens a change request — same number<TAB>url output as the forge
// path, with the lab-relative CR route as the URL, and the server-injected
// `Closes #<claimed issue>` persisted as a cr_closes row.
func TestPRCreateBuiltinChangeRequest(t *testing.T) {
	f := newBuiltinFixture(t)
	code, stdout, stderr := run(t, []string{"pr", "create", "--title", "feat: fix", "--body", "Did it."}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if !strings.HasPrefix(stdout, "1\t/repos/") || !strings.HasSuffix(stdout, "/crs/1\n") {
		t.Fatalf("stdout = %q, want 1<TAB>/repos/<id>/crs/1", stdout)
	}
	repoID := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(stdout), "1\t/repos/"), "/crs/1")

	cr, err := f.st.CRByRepoNumber(context.Background(), repoID, 1)
	if err != nil {
		t.Fatalf("CRByRepoNumber: %v", err)
	}
	if cr.State != store.CRStateOpen || cr.HeadBranch != "afk/1" || cr.BaseBranch != "main" {
		t.Fatalf("cr = state %q head %q base %q, want open afk/1 main", cr.State, cr.HeadBranch, cr.BaseBranch)
	}
	if cr.Body != "Did it.\n\nCloses #1" {
		t.Errorf("CR body = %q, want the injected Closes #1", cr.Body)
	}
	if len(cr.Closes) != 1 || cr.Closes[0] != 1 {
		t.Errorf("closes = %v, want [1]", cr.Closes)
	}
}

// TestPRViewOutput pins the plain-text PR view (issue #8): number+title,
// state/head/url metadata lines, then the FULL body — the captured-card-YAML
// retrieval path, no raw forge fallback. An unknown number is the server's
// 404 envelope → message on stderr, exit 1 (never 2, never a panic).
// TestPRMergeOutput: `labctl pr merge <n>` prints one parseable line
// (#number<TAB>state<TAB>url) and passes the number through to MergePull.
func TestPRMergeOutput(t *testing.T) {
	fk := &fakeForge{mergeRef: tracker.PullRef{
		Number: 12, HeadBranch: "afk/1", State: tracker.PullMerged, URL: "https://forge.example/pr/12",
	}}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "merge", "12"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "#12\tmerged\thttps://forge.example/pr/12\n" {
		t.Errorf("stdout = %q, want #12<TAB>merged<TAB>url", stdout)
	}
	if fk.mergedNumber != 12 {
		t.Errorf("MergePull arg = %d, want 12", fk.mergedNumber)
	}
}

// TestPRMergeRejectedExit1: a backend rejection prints the message on stderr
// and exits 1 (the pinned API-error exit code).
func TestPRMergeRejectedExit1(t *testing.T) {
	fk := &fakeForge{mergeErr: fmt.Errorf("%w: required status check \"ci\" is expected", tracker.ErrMergeRejected)}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "merge", "12"}, f.env())
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %q)", code, stdout)
	}
	if !strings.Contains(stderr, "required status check") {
		t.Errorf("stderr = %q, want the backend's own words", stderr)
	}
}

// TestPRMergeUsage: a missing or non-integer number is a usage error (exit 2).
func TestPRMergeUsage(t *testing.T) {
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return &fakeForge{}, nil }))
	for _, args := range [][]string{{"pr", "merge"}, {"pr", "merge", "abc"}, {"pr", "merge", "1", "2"}} {
		if code, _, _ := run(t, args, f.env()); code != 2 {
			t.Errorf("run %v: exit = %d, want 2", args, code)
		}
	}
}

func TestPRViewOutput(t *testing.T) {
	fk := &fakeForge{pullDetail: &tracker.PullDetail{
		Number: 12, Title: "feat: capture card", Body: "card: |\n  kind: capture",
		State: tracker.PullOpen, HeadBranch: "afk/7", URL: "https://forge.example/pr/12",
	}}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "view", "12"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	// No reviews on this fixture: output stays exactly what it was before
	// reviews existed.
	want := "#12 feat: capture card\n" +
		"state: open\n" +
		"head: afk/7\n" +
		"url: https://forge.example/pr/12\n" +
		"\ncard: |\n  kind: capture\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	code, stdout, stderr = run(t, []string{"pr", "view", "999"}, f.env())
	if code != 1 || stdout != "" {
		t.Fatalf("unknown PR: exit = %d, stdout %q, want 1 with empty stdout", code, stdout)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr = %q, want the 404 message", stderr)
	}
}

// TestPRViewWithReviews extends TestPRViewOutput (issue #180): two submitted
// reviews (one dismissed) each render under a "--- review by <reviewer>
// (<state>)" separator following the body, dismissed adding ", dismissed" to
// the parenthetical.
func TestPRViewWithReviews(t *testing.T) {
	fk := &fakeForge{
		pullDetail: &tracker.PullDetail{
			Number: 12, Title: "feat: capture card", Body: "card: |\n  kind: capture",
			State: tracker.PullOpen, HeadBranch: "afk/7", URL: "https://forge.example/pr/12",
		},
		reviews: []tracker.Review{
			{Reviewer: "alice", State: tracker.ReviewChangesRequested, Body: "fix the tests", Dismissed: false},
			{Reviewer: "bob", State: tracker.ReviewApproved, Body: "lgtm", Dismissed: true},
		},
	}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "view", "12"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	want := "#12 feat: capture card\n" +
		"state: open\n" +
		"head: afk/7\n" +
		"url: https://forge.example/pr/12\n" +
		"\ncard: |\n  kind: capture\n" +
		"\n--- review by alice (changes_requested)\nfix the tests\n" +
		"\n--- review by bob (approved, dismissed)\nlgtm\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestPRViewWithComments extends TestPRViewWithReviews (issue #191): PR
// comments render AFTER the reviews block, each under printIssue's own
// "--- comment by <author> (<time>)" separator, oldest first (slice order,
// no sorting). The server-side wiring of GET /agent/v1/prs/{n}'s comments
// array is issue #191's other half (internal/agentapi, done in parallel), so
// this drives the Client straight against a raw JSON httptest server — like
// TestPRChecksUnknownAggregateExitsOne below — rather than through
// newAgentFixture's fakeForge, exercising labctl's decode-and-render path on
// its own.
func TestPRViewWithComments(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{
			"number": 12, "title": "feat: capture card", "body": "card: |\n  kind: capture",
			"state": "open", "head": "afk/7", "url": "https://forge.example/pr/12",
			"reviews": [
				{"reviewer": "alice", "state": "changes_requested", "body": "fix the tests", "dismissed": false}
			],
			"comments": [
				{"author": "alice", "body": "pushed a fix for the tests", "created_at": "2026-07-06T12:00:00Z"},
				{"author": "bob", "body": "looks good now", "created_at": "2026-07-06T13:00:00Z"}
			]
		}`)
	}))
	defer ts.Close()

	env := map[string]string{"LAB_URL": ts.URL, "LAB_TOKEN": "lab_run_x"}
	code, stdout, stderr := run(t, []string{"pr", "view", "12"}, env)
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	want := "#12 feat: capture card\n" +
		"state: open\n" +
		"head: afk/7\n" +
		"url: https://forge.example/pr/12\n" +
		"\ncard: |\n  kind: capture\n" +
		"\n--- review by alice (changes_requested)\nfix the tests\n" +
		"\n--- comment by alice (2026-07-06T12:00:00Z)\npushed a fix for the tests\n" +
		"\n--- comment by bob (2026-07-06T13:00:00Z)\nlooks good now\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// --- pr review verbs (issue #180) -------------------------------------------

// TestPRRejectOutput: `labctl pr reject <n> <body>` records the rejection
// verdict through the real agentapi — end to end, the tracker sees ONE PR
// comment whose first line is the reject marker with the findings below
// (composed server-side; the CLI speaks verbs only) — and prints
// #<n>\trejected.
func TestPRRejectOutput(t *testing.T) {
	fk := &fakeForge{}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "reject", "12", "fix the tests"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "#12\trejected\n" {
		t.Errorf("stdout = %q, want %q", stdout, "#12\trejected\n")
	}
	wantComment := "[autoland] verdict: reject\n\nfix the tests"
	if fk.commentedN != 12 || fk.commentedBody != wantComment {
		t.Errorf("CommentPull args = (%d, %q), want (12, %q)", fk.commentedN, fk.commentedBody, wantComment)
	}
}

// TestPRRejectErrorExit1: a tracker failure on the verdict comment surfaces
// on stderr and exits 1 — the verdict-verb twin of TestPRMergeRejectedExit1.
func TestPRRejectErrorExit1(t *testing.T) {
	fk := &fakeForge{commentErr: fmt.Errorf("forge comments unreachable")}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "reject", "12", "fix it"}, f.env())
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %q)", code, stdout)
	}
	if !strings.Contains(stderr, "forge comments unreachable") {
		t.Errorf("stderr = %q, want the backend's own words", stderr)
	}
}

// TestPRRejectUsage: a missing/non-integer PR number or an empty positional
// body is a usage error (exit 2) — the CLI never round-trips a blank body to
// the server's 400.
func TestPRRejectUsage(t *testing.T) {
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return &fakeForge{}, nil }))
	for _, args := range [][]string{
		{"pr", "reject"},
		{"pr", "reject", "12"},
		{"pr", "reject", "abc", "issues found"},
		{"pr", "reject", "12", ""},
		{"pr", "reject", "12", "issues found", "extra"},
	} {
		if code, _, _ := run(t, args, f.env()); code != 2 {
			t.Errorf("run %v: exit = %d, want 2", args, code)
		}
	}
}

// TestPREscalateOutput: `labctl pr escalate <n> <body>` records the
// escalate-mode lander's terminal marker through the real agentapi — end to
// end, the tracker sees ONE PR comment whose first line is the escalate
// marker with the digest below (composed server-side) — and prints
// #<n>\tescalated. Unlike rerequest, there is no native ping to observe.
func TestPREscalateOutput(t *testing.T) {
	fk := &fakeForge{}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "escalate", "12", "round 1: missing tests; round 2: still flaky"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "#12\tescalated\n" {
		t.Errorf("stdout = %q, want %q", stdout, "#12\tescalated\n")
	}
	wantComment := "[autoland] verdict: escalate\n\nround 1: missing tests; round 2: still flaky"
	if fk.commentedN != 12 || fk.commentedBody != wantComment {
		t.Errorf("CommentPull args = (%d, %q), want (12, %q)", fk.commentedN, fk.commentedBody, wantComment)
	}
}

// TestPREscalateErrorExit1: a tracker failure on the verdict comment surfaces
// on stderr and exits 1 — the verdict-verb twin of TestPRRejectErrorExit1.
func TestPREscalateErrorExit1(t *testing.T) {
	fk := &fakeForge{commentErr: fmt.Errorf("forge comments unreachable")}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "escalate", "12", "digest"}, f.env())
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %q)", code, stdout)
	}
	if !strings.Contains(stderr, "forge comments unreachable") {
		t.Errorf("stderr = %q, want the backend's own words", stderr)
	}
}

// TestPREscalateUsage mirrors TestPRRejectUsage: a missing/non-integer PR
// number or an empty positional body is a usage error (exit 2) — the CLI
// never round-trips a blank digest to the server's 400.
func TestPREscalateUsage(t *testing.T) {
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return &fakeForge{}, nil }))
	for _, args := range [][]string{
		{"pr", "escalate"},
		{"pr", "escalate", "12"},
		{"pr", "escalate", "abc", "digest"},
		{"pr", "escalate", "12", ""},
		{"pr", "escalate", "12", "digest", "extra"},
	} {
		if code, _, _ := run(t, args, f.env()); code != 2 {
			t.Errorf("run %v: exit = %d, want 2", args, code)
		}
	}
}

// TestPRApproveOutput: `labctl pr approve <n> [body]` records the pass verdict
// through the real agentapi — end to end, the tracker sees ONE PR comment
// whose first line is the pass marker (bare when no body was given, CONCERNS
// prose below it otherwise) — and prints #<n>\tapproved.
func TestPRApproveOutput(t *testing.T) {
	fk := &fakeForge{}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "approve", "12", "looks good"}, f.env())
	if code != 0 {
		t.Fatalf("with body: exit = %d, stderr %q", code, stderr)
	}
	if stdout != "#12\tapproved\n" {
		t.Errorf("with body: stdout = %q, want %q", stdout, "#12\tapproved\n")
	}
	wantComment := "[autoland] verdict: pass\n\nlooks good"
	if fk.commentedN != 12 || fk.commentedBody != wantComment {
		t.Errorf("CommentPull args = (%d, %q), want (12, %q)", fk.commentedN, fk.commentedBody, wantComment)
	}

	code, stdout, stderr = run(t, []string{"pr", "approve", "12"}, f.env())
	if code != 0 {
		t.Fatalf("without body: exit = %d, stderr %q", code, stderr)
	}
	if stdout != "#12\tapproved\n" {
		t.Errorf("without body: stdout = %q, want %q", stdout, "#12\tapproved\n")
	}
	if fk.commentedBody != "[autoland] verdict: pass" {
		t.Errorf("without body: CommentPull body = %q, want the bare pass marker", fk.commentedBody)
	}
}

// TestPRApproveUsage mirrors TestPRRejectUsage: a missing/non-integer number
// or trailing junk is a usage error (exit 2); the body stays optional.
func TestPRApproveUsage(t *testing.T) {
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return &fakeForge{}, nil }))
	for _, args := range [][]string{
		{"pr", "approve"},
		{"pr", "approve", "abc"},
		{"pr", "approve", "12", "body", "extra"},
	} {
		if code, _, _ := run(t, args, f.env()); code != 2 {
			t.Errorf("run %v: exit = %d, want 2", args, code)
		}
	}
}

// TestPRRerequestOutput: `labctl pr rerequest <n>` signals fix-done — end to
// end, the tracker sees the bare fix-done marker as a PR comment AND the native
// reviewer re-request — and prints #<n>\trerequested.
func TestPRRerequestOutput(t *testing.T) {
	fk := &fakeForge{}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "rerequest", "12"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "#12\trerequested\n" {
		t.Errorf("stdout = %q, want %q", stdout, "#12\trerequested\n")
	}
	if fk.commentedN != 12 || fk.commentedBody != "[autoland] verdict: fix-done" {
		t.Errorf("CommentPull args = (%d, %q), want the bare fix-done marker on 12", fk.commentedN, fk.commentedBody)
	}
	if fk.rerequestedN != 12 {
		t.Errorf("RerequestReview arg = %d, want 12", fk.rerequestedN)
	}
}

// TestPRRerequestPingFailureWarnsExit0: a refused native reviewer ping is
// NON-FATAL — the fix-done comment already landed (it is the done-signal), so
// labctl prints the warning on stderr, keeps the success line, and exits 0.
func TestPRRerequestPingFailureWarnsExit0(t *testing.T) {
	fk := &fakeForge{rerequestErr: fmt.Errorf("%w: the forge declined the re-request", tracker.ErrReviewRejected)}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "rerequest", "12"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (stderr %q)", code, stderr)
	}
	if stdout != "#12\trerequested\n" {
		t.Errorf("stdout = %q, want %q", stdout, "#12\trerequested\n")
	}
	if !strings.Contains(stderr, "warning") || !strings.Contains(stderr, "declined the re-request") {
		t.Errorf("stderr = %q, want a warning carrying the forge's own words", stderr)
	}
	if fk.commentedN != 12 || fk.commentedBody != "[autoland] verdict: fix-done" {
		t.Errorf("CommentPull args = (%d, %q), want the fix-done marker despite the failed ping", fk.commentedN, fk.commentedBody)
	}
}

// TestPRRerequestUsage: a missing/non-integer/extra number is a usage error
// (exit 2) — `pr rerequest` takes exactly one argument.
func TestPRRerequestUsage(t *testing.T) {
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return &fakeForge{}, nil }))
	for _, args := range [][]string{
		{"pr", "rerequest"},
		{"pr", "rerequest", "abc"},
		{"pr", "rerequest", "12", "extra"},
	} {
		if code, _, _ := run(t, args, f.env()); code != 2 {
			t.Errorf("run %v: exit = %d, want 2", args, code)
		}
	}
}

// TestPRCommentOutput: `labctl pr comment <n> <body>` posts a plain
// discussion comment and prints nothing on success — mirroring `labctl issue
// comment`'s parseable-silence confirmation shape exactly.
func TestPRCommentOutput(t *testing.T) {
	fk := &fakeForge{}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "comment", "12", "tests are green"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty (parseable silence on success)", stdout)
	}
	if fk.commentedN != 12 || fk.commentedBody != "tests are green" {
		t.Errorf("CommentPull args = (%d, %q), want (12, %q)", fk.commentedN, fk.commentedBody, "tests are green")
	}
}

// TestPRCommentUsage mirrors TestPRRejectUsage: `pr comment` requires exactly
// <n> <body>, and an empty positional body is a usage error too.
func TestPRCommentUsage(t *testing.T) {
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return &fakeForge{}, nil }))
	for _, args := range [][]string{
		{"pr", "comment"},
		{"pr", "comment", "12"},
		{"pr", "comment", "abc", "hi"},
		{"pr", "comment", "12", ""},
		{"pr", "comment", "12", "hi", "extra"},
	} {
		if code, _, _ := run(t, args, f.env()); code != 2 {
			t.Errorf("run %v: exit = %d, want 2", args, code)
		}
	}
}

// TestPRListOutput pins the one-row-per-PR list: number, state, head, url —
// tab-separated like every labctl list. The rows are whatever Pulls()
// answers (since issue #176: the open set plus the recent-closed window; the
// bound lives in the tracker backends, so the fake's rows render verbatim) —
// the row format is agent-parsed API surface and must not change.
func TestPRListOutput(t *testing.T) {
	fk := &fakeForge{pulls: []tracker.PullRef{
		{Number: 12, HeadBranch: "afk/7", State: tracker.PullOpen, URL: "https://forge.example/pr/12"},
		{Number: 3, HeadBranch: "afk/2", State: tracker.PullMerged, URL: "https://forge.example/pr/3"},
	}}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "list"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	want := "#12\topen\tafk/7\thttps://forge.example/pr/12\n" +
		"#3\tmerged\tafk/2\thttps://forge.example/pr/3\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestPRViewBuiltinRoundTrip proves the end-to-end acceptance path on a
// builtin repo: the body a run filed with `pr create` (a card YAML) comes
// back verbatim through `pr view` — plus the server-injected Closes — with
// no forge/tea/gh fallback anywhere.
func TestPRViewBuiltinRoundTrip(t *testing.T) {
	f := newBuiltinFixture(t)

	code, _, stderr := run(t, []string{"pr", "create", "--title", "feat: capture card", "--body", "card: |\n  kind: capture"}, f.env())
	if code != 0 {
		t.Fatalf("pr create: exit = %d, stderr %q", code, stderr)
	}

	code, stdout, stderr := run(t, []string{"pr", "view", "1"}, f.env())
	if code != 0 {
		t.Fatalf("pr view: exit = %d, stderr %q", code, stderr)
	}
	if !strings.HasPrefix(stdout, "#1 feat: capture card\nstate: open\nhead: afk/1\n") {
		t.Errorf("stdout = %q, want the CR header lines", stdout)
	}
	if !strings.Contains(stdout, "card: |\n  kind: capture") || !strings.Contains(stdout, "Closes #1") {
		t.Errorf("stdout = %q, want the card YAML body and the injected Closes", stdout)
	}

	code, stdout, stderr = run(t, []string{"pr", "list"}, f.env())
	if code != 0 {
		t.Fatalf("pr list: exit = %d, stderr %q", code, stderr)
	}
	if !strings.HasPrefix(stdout, "#1\topen\tafk/1\t/repos/") {
		t.Errorf("stdout = %q, want the CR row", stdout)
	}
}

// TestPRChecksRed pins the red path (issue #72): the rows render in the wire
// column order name<TAB>state<TAB>rawState<TAB>summary<TAB>url under a
// `state: <aggregate>` line, and a red aggregate exits 2 — the one code that
// means "checks are red", distinct from every other failure (which is 1).
func TestPRChecksRed(t *testing.T) {
	fk := &fakeForge{checks: []tracker.Check{
		{Name: "build", State: tracker.CheckSuccess, RawState: "success", Summary: "ok", URL: "https://ci/build"},
		{Name: "test", State: tracker.CheckFailure, RawState: "failure", Summary: "3 failing", URL: "https://ci/test"},
	}}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "checks", "12"}, f.env())
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (red aggregate); stderr %q", code, stderr)
	}
	want := "state: failure\n" +
		"build\tsuccess\tsuccess\tok\thttps://ci/build\n" +
		"test\tfailure\tfailure\t3 failing\thttps://ci/test\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestPRChecksGreen: an all-success aggregate exits 0 (green — the loop is done).
func TestPRChecksGreen(t *testing.T) {
	fk := &fakeForge{checks: []tracker.Check{
		{Name: "build", State: tracker.CheckSuccess, RawState: "success", Summary: "", URL: "https://ci/build"},
	}}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "checks", "12"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (green); stderr %q", code, stderr)
	}
	if stdout != "state: success\nbuild\tsuccess\tsuccess\t\thttps://ci/build\n" {
		t.Errorf("stdout = %q", stdout)
	}
}

// TestPRChecksPendingExit3: a single-shot fetch that finds the aggregate still
// pending exits 3 ("still pending at exit" covers the single-shot case too).
func TestPRChecksPendingExit3(t *testing.T) {
	fk := &fakeForge{checks: []tracker.Check{
		{Name: "build", State: tracker.CheckPending, RawState: "in_progress", Summary: "", URL: "https://ci/build"},
	}}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "checks", "12"}, f.env())
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (still pending); stderr %q", code, stderr)
	}
	if stdout != "state: pending\nbuild\tpending\tin_progress\t\thttps://ci/build\n" {
		t.Errorf("stdout = %q", stdout)
	}
}

// TestPRChecksBuiltinNone: a builtin repo truthfully has no CI, so a CR's
// checks aggregate to none — printed as just the state line, exit 0. The CR is
// seeded end-to-end via `pr create`, the way the builtin PR-view test does.
func TestPRChecksBuiltinNone(t *testing.T) {
	f := newBuiltinFixture(t)
	if code, _, stderr := run(t, []string{"pr", "create", "--title", "feat: x", "--body", "Did it."}, f.env()); code != 0 {
		t.Fatalf("pr create: exit = %d, stderr %q", code, stderr)
	}

	code, stdout, stderr := run(t, []string{"pr", "checks", "1"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (none); stderr %q", code, stderr)
	}
	if stdout != "state: none\n" {
		t.Errorf("stdout = %q, want just the state line (zero rows)", stdout)
	}
}

// TestPRChecksWaitSucceeds: --wait polls CLIENT-SIDE (the harness blocks
// foreground sleep) until the aggregate leaves pending. The fake returns
// pending then success; labctl prints ONLY the final success report, once, and
// exits 0. Poll knobs are shrunk to milliseconds so the loop is sub-second.
func TestPRChecksWaitSucceeds(t *testing.T) {
	origInterval, origCap := checksPollInterval, checksWaitCap
	checksPollInterval = time.Millisecond
	checksWaitCap = time.Second
	t.Cleanup(func() { checksPollInterval, checksWaitCap = origInterval, origCap })

	fk := &fakeForge{checksSeq: [][]tracker.Check{
		{{Name: "ci", State: tracker.CheckPending, RawState: "queued", Summary: "", URL: "https://ci/1"}},
		{{Name: "ci", State: tracker.CheckSuccess, RawState: "success", Summary: "", URL: "https://ci/1"}},
	}}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "checks", "12", "--wait"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr %q", code, stderr)
	}
	if stdout != "state: success\nci\tsuccess\tsuccess\t\thttps://ci/1\n" {
		t.Errorf("stdout = %q, want a single success report", stdout)
	}
	if strings.Count(stdout, "state:") != 1 {
		t.Errorf("stdout has %d state lines, want exactly 1 (report printed once, never per poll)", strings.Count(stdout, "state:"))
	}
}

// TestPRChecksWaitTimesOut: --wait that stays pending past the (shrunken) cap
// prints the still-pending report once and exits 3.
func TestPRChecksWaitTimesOut(t *testing.T) {
	origInterval, origCap := checksPollInterval, checksWaitCap
	checksPollInterval = time.Millisecond
	checksWaitCap = 5 * time.Millisecond
	t.Cleanup(func() { checksPollInterval, checksWaitCap = origInterval, origCap })

	fk := &fakeForge{checks: []tracker.Check{
		{Name: "ci", State: tracker.CheckPending, RawState: "queued", Summary: "", URL: "https://ci/1"},
	}}
	f := newAgentFixture(t, store.TrackerBindingForge,
		resolverFunc(func(context.Context, store.Repo) (tracker.Tracker, error) { return fk, nil }))

	code, stdout, stderr := run(t, []string{"pr", "checks", "12", "--wait"}, f.env())
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (pending past cap); stderr %q", code, stderr)
	}
	if stdout != "state: pending\nci\tpending\tqueued\t\thttps://ci/1\n" {
		t.Errorf("stdout = %q, want the still-pending report", stdout)
	}
	if strings.Count(stdout, "state:") != 1 {
		t.Errorf("stdout has %d state lines, want exactly 1", strings.Count(stdout, "state:"))
	}
}

// TestPRChecksUnknownAggregateExitsOne pins the malformed-answer edge: an
// aggregate word outside the four-word vocabulary (a non-lab server, or
// version skew — a real lab server computes it via tracker.ChecksState and
// can only emit the four) is an API error, exit 1 with a diagnostic — never a
// silent 0 an agent would read as green. Needs a raw server: the in-process
// fixture cannot produce an out-of-vocabulary aggregate.
func TestPRChecksUnknownAggregateExitsOne(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"state":"weird","checks":[]}`)
	}))
	defer ts.Close()

	env := map[string]string{"LAB_URL": ts.URL, "LAB_TOKEN": "lab_run_x"}
	code, _, stderr := run(t, []string{"pr", "checks", "1"}, env)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, `unexpected aggregate state "weird"`) {
		t.Errorf("stderr = %q, want the unexpected-aggregate diagnostic", stderr)
	}
}

func TestAPIErrorsExitOne(t *testing.T) {
	f := newBuiltinFixture(t)

	// Bad token → the API's opaque 401 → exit 1.
	env := map[string]string{"LAB_URL": f.url, "LAB_TOKEN": "lab_run_bogusbogusbogus"}
	code, stdout, stderr := run(t, []string{"issue", "list"}, env)
	if code != 1 || stdout != "" {
		t.Fatalf("exit = %d, stdout %q, want 1", code, stdout)
	}
	if !strings.Contains(stderr, "invalid run token") {
		t.Errorf("stderr = %q, want the 401 message", stderr)
	}

	// Unreachable server → exit 1 (transport error, not usage).
	env = map[string]string{"LAB_URL": "http://127.0.0.1:1", "LAB_TOKEN": "lab_run_x"}
	code, _, stderr = run(t, []string{"issue", "list"}, env)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stderr %q)", code, stderr)
	}
	if stderr == "" {
		t.Error("stderr empty, want a transport error message")
	}
}

// TestRedirectSurfacesActionableError checks that labctl does NOT follow a 3xx
// from the agent API — the signature of an SSO/auth proxy bouncing machine
// traffic to a login page — and instead reports the redirect target and the
// "may be pointed at an auth proxy" hint, rather than choking on the login
// HTML with a JSON-decode error (issue #30).
func TestRedirectSurfacesActionableError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://sso.example.com/login?rd="+r.URL.Path, http.StatusFound)
	}))
	defer ts.Close()

	env := map[string]string{"LAB_URL": ts.URL, "LAB_TOKEN": "lab_run_x"}
	code, stdout, stderr := run(t, []string{"issue", "list"}, env)
	if code != 1 || stdout != "" {
		t.Fatalf("exit = %d, stdout %q, want 1", code, stdout)
	}
	for _, want := range []string{"302", "sso.example.com/login", "auth proxy"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
	if strings.Contains(stderr, "invalid character") || strings.Contains(stderr, "decoding response") {
		t.Errorf("stderr = %q, still a JSON-decode error", stderr)
	}
}

// TestHTMLBodySurfacesProxyHint checks the redirect-less variants: a proxy that
// answers with its login/error page as text/html at various statuses. The
// decoder would fail with "invalid character '<'" (or, worse, a mutating
// command with no decode step would silently exit 0); labctl instead names the
// HTML/auth-proxy cause and fails for every command shape (issue #30).
func TestHTMLBodySurfacesProxyHint(t *testing.T) {
	const htmlBody = "<!DOCTYPE html><html><body>Please sign in</body></html>"
	tests := []struct {
		name   string
		status int
		args   []string // a read command (out != nil) vs a mutation (out == nil)
	}{
		{"200 read", http.StatusOK, []string{"issue", "list"}},
		{"200 mutation", http.StatusOK, []string{"issue", "close", "1"}},
		{"401 read", http.StatusUnauthorized, []string{"issue", "list"}},
		{"502 mutation", http.StatusBadGateway, []string{"issue", "close", "1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, htmlBody)
			}))
			defer ts.Close()

			env := map[string]string{"LAB_URL": ts.URL, "LAB_TOKEN": "lab_run_x"}
			code, stdout, stderr := run(t, tt.args, env)
			if code != 1 {
				t.Fatalf("exit = %d, want 1 (stdout %q, stderr %q)", code, stdout, stderr)
			}
			if !strings.Contains(stderr, "HTML") || !strings.Contains(stderr, "auth proxy") {
				t.Errorf("stderr = %q, want the HTML/auth-proxy hint", stderr)
			}
			if strings.Contains(stderr, "invalid character") || strings.Contains(stderr, "decoding response") {
				t.Errorf("stderr = %q, still a JSON-decode error", stderr)
			}
		})
	}
}

// --- secret surface ---------------------------------------------------------

// TestSecretListOutput pins the `NAME<TAB>description` rows and that no value
// crosses the list surface.
func TestSecretListOutput(t *testing.T) {
	f := newBuiltinFixture(t)
	f.seedSecret(t, "API_KEY", "prod key", "secret-value-never-shown")
	f.seedSecret(t, "DB_URL", "", "postgres://who:cares@h/db")

	code, stdout, stderr := run(t, []string{"secret", "list"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	// Ordered by name (store contract): API_KEY then DB_URL.
	want := "API_KEY\tprod key\nDB_URL\t\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if strings.Contains(stdout, "secret-value") || strings.Contains(stdout, "postgres://") {
		t.Errorf("list leaked a value: %q", stdout)
	}
}

// TestSecretExecInjectsValue pins the core exec path: the named secret
// reaches the child as $NAME. The value can no longer be proven by printing
// it — output redaction (issue #105) means the plaintext never survives to
// stdout — so injection is proven by an IN-CHILD comparison against the
// expected value (passed as an argv operand, which is never scanned), and the
// child also prints the value to prove the printed side arrives, by
// contract, as the redaction token rather than the plaintext.
func TestSecretExecInjectsValue(t *testing.T) {
	f := newBuiltinFixture(t)
	f.seedSecret(t, "FOO", "", "plaintext-injected")
	sh := testShell(t)

	code, stdout, stderr := run(t, []string{"secret", "exec", "FOO", "--",
		sh, "-c", `[ "$FOO" = "$1" ] && printf %s "$FOO"`, "sh", "plaintext-injected"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "[REDACTED:FOO]" {
		t.Errorf("stdout = %q, want the redaction token", stdout)
	}
}

// TestSecretExecLiveRotation pins that values are fetched per exec, never
// cached: rotating the stored ciphertext (no client/fixture rebuild) changes
// what the very next exec sees, with the same env. Rotation can no longer be
// observed by printing the value — both v1 and v2 would redact to the same
// "[REDACTED:FOO]" token — so instead the child compares $FOO against an
// expected literal given as an argv operand (never scanned) and prints only
// "match"/"mismatch", never the value itself.
func TestSecretExecLiveRotation(t *testing.T) {
	f := newBuiltinFixture(t)
	id := f.seedSecret(t, "FOO", "", "v1")
	sh := testShell(t)
	cmpScript := `if [ "$FOO" = "$1" ]; then printf match; else printf mismatch; fi`
	argsFor := func(expect string) []string {
		return []string{"secret", "exec", "FOO", "--", sh, "-c", cmpScript, "sh", expect}
	}

	code, stdout, stderr := run(t, argsFor("v1"), f.env())
	if code != 0 || stdout != "match" {
		t.Fatalf("first exec (compare v1) = %d %q, stderr %q, want 0 match", code, stdout, stderr)
	}

	// Rotate the stored blob in place; no new client, no new server.
	blob, err := f.vlt.Encrypt([]byte("v2"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := f.st.RotateRepoSecret(context.Background(), id, blob, fixedNow); err != nil {
		t.Fatalf("RotateRepoSecret: %v", err)
	}

	code, stdout, stderr = run(t, argsFor("v1"), f.env())
	if code != 0 || stdout != "mismatch" {
		t.Fatalf("post-rotation exec (compare stale v1) = %d %q, stderr %q, want 0 mismatch", code, stdout, stderr)
	}

	code, stdout, stderr = run(t, argsFor("v2"), f.env())
	if code != 0 || stdout != "match" {
		t.Fatalf("post-rotation exec (compare v2) = %d %q, stderr %q, want 0 match", code, stdout, stderr)
	}
}

// TestSecretExecPreservesExitCode pins the transparent exit passthrough: the
// child's non-zero code is labctl's exit code.
func TestSecretExecPreservesExitCode(t *testing.T) {
	f := newBuiltinFixture(t)
	f.seedSecret(t, "FOO", "", "irrelevant")
	sh := testShell(t)

	code, _, _ := run(t, []string{"secret", "exec", "FOO", "--", sh, "-c", "exit 7"}, f.env())
	if code != 7 {
		t.Fatalf("exit = %d, want the child's 7", code)
	}
}

// TestSecretExecUnknownName pins that an unknown secret is an API error (exit
// 1) whose message names the secret, and that the child NEVER runs (empty
// stdout) — nothing half-executes on a miss.
func TestSecretExecUnknownName(t *testing.T) {
	f := newBuiltinFixture(t)
	sh := testShell(t)

	code, stdout, stderr := run(t, []string{"secret", "exec", "NOPE", "--", sh, "-c", `printf ran`}, f.env())
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (API error before exec)", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty (command must not run on a miss)", stdout)
	}
	if !strings.Contains(stderr, "NOPE") {
		t.Errorf("stderr = %q, want it to name the missing secret", stderr)
	}
}

// TestSecretExecArgvIsExactCommand pins that argv is passed through EXACT and
// value-free: the child echoes its own argv ($0 then $@) byte-identical (a
// passthrough check, since none of those bytes match the secret), then proves
// injection with an in-child comparison (never printing the value), then
// prints $FOO itself — which by contract arrives as the redaction token, not
// the plaintext.
func TestSecretExecArgvIsExactCommand(t *testing.T) {
	f := newBuiltinFixture(t)
	f.seedSecret(t, "FOO", "", "smuggled-plain")
	sh := testShell(t)

	// `sh -c SCRIPT $0 $@`: with trailing operands "sh" "alpha" "beta", the
	// child sees $0=sh, $@=(alpha beta) — exactly the argv we passed. $2 below
	// carries the expected value for the in-child comparison; it is an argv
	// operand, never scanned, and never echoed.
	script := `printf '%s\n' "$0" "$1" "$2"; [ "$FOO" = "$3" ] && printf 'match\n'; printf 'env=%s\n' "$FOO"`
	code, stdout, stderr := run(t, []string{"secret", "exec", "FOO", "--",
		sh, "-c", script, "sh", "alpha", "beta", "smuggled-plain"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	want := "sh\nalpha\nbeta\nmatch\nenv=[REDACTED:FOO]\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if strings.Contains(stdout, "smuggled-plain") || strings.Contains(stderr, "smuggled-plain") {
		t.Errorf("plaintext value leaked: stdout %q, stderr %q", stdout, stderr)
	}
}

// --- output redaction (issue #105) ------------------------------------------

// TestSecretExecRedactsPrintedValue pins the headline behaviour: a child that
// prints the injected value verbatim to stdout never gets it through — the
// broker's redactor replaces it with the "[REDACTED:NAME]" token.
func TestSecretExecRedactsPrintedValue(t *testing.T) {
	f := newBuiltinFixture(t)
	f.seedSecret(t, "FOO", "", "plaintext-injected")
	sh := testShell(t)

	code, stdout, stderr := run(t, []string{"secret", "exec", "FOO", "--", sh, "-c", `printf %s "$FOO"`}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "[REDACTED:FOO]" {
		t.Errorf("stdout = %q, want [REDACTED:FOO]", stdout)
	}
}

// TestSecretExecRedactsEncodedForms pins that redaction catches every derived
// form the Matcher recognises, not just the exact plaintext: base64, hex, and
// URL percent-encoding. The value is chosen so its encodings differ from the
// plaintext and from each other (space, '+', '=', '!' all need escaping or
// encoding). Each encoded string is passed as an argv operand — never
// scanned — and the child prints it verbatim; the printed output must still
// come out as the token, proving the Matcher derived and matched that form.
func TestSecretExecRedactsEncodedForms(t *testing.T) {
	f := newBuiltinFixture(t)
	const value = "hello world+/=!"
	f.seedSecret(t, "FOO", "", value)
	sh := testShell(t)

	forms := map[string]string{
		"base64": base64.StdEncoding.EncodeToString([]byte(value)),
		"hex":    hex.EncodeToString([]byte(value)),
		"url":    url.QueryEscape(value),
	}
	for form, encoded := range forms {
		t.Run(form, func(t *testing.T) {
			code, stdout, stderr := run(t, []string{"secret", "exec", "FOO", "--", sh, "-c", `printf %s "$1"`, "sh", encoded}, f.env())
			if code != 0 {
				t.Fatalf("exit = %d, stderr %q", code, stderr)
			}
			if stdout != "[REDACTED:FOO]" {
				t.Errorf("stdout = %q, want [REDACTED:FOO] (form %s, encoded %q)", stdout, form, encoded)
			}
			if strings.Contains(stdout, value) || strings.Contains(stderr, value) {
				t.Errorf("plaintext value leaked: stdout %q, stderr %q", stdout, stderr)
			}
			if strings.Contains(stdout, encoded) {
				t.Errorf("encoded value leaked unredacted: stdout %q", stdout)
			}
		})
	}
}

// TestSecretExecPrintenvShowsPlaceholder is the issue's literal acceptance
// criterion: `labctl secret exec FOO -- printenv FOO` must print the
// redaction placeholder, never the value.
func TestSecretExecPrintenvShowsPlaceholder(t *testing.T) {
	f := newBuiltinFixture(t)
	f.seedSecret(t, "FOO", "", "plaintext-injected")

	printenv, err := exec.LookPath("printenv")
	if err != nil {
		// Fall back to the shell's printenv utility (skip is inside testShell
		// if even that is unavailable); printenv exists in this sandbox's
		// coreutils, so this branch is not expected to run.
		sh := testShell(t)
		code, stdout, stderr := run(t, []string{"secret", "exec", "FOO", "--", sh, "-c", "printenv FOO"}, f.env())
		if code != 0 {
			t.Fatalf("exit = %d, stderr %q", code, stderr)
		}
		if stdout != "[REDACTED:FOO]\n" {
			t.Fatalf("stdout = %q, want [REDACTED:FOO]\\n", stdout)
		}
		return
	}

	code, stdout, stderr := run(t, []string{"secret", "exec", "FOO", "--", printenv, "FOO"}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "[REDACTED:FOO]\n" {
		t.Fatalf("stdout = %q, want [REDACTED:FOO]\\n", stdout)
	}
}

// TestSecretExecStderrRedactedAndSeparate pins that redaction applies to
// stderr independently of stdout, and that the two streams stay separate: a
// stdout marker survives byte-identical while the value written to stderr
// comes out only as the token.
func TestSecretExecStderrRedactedAndSeparate(t *testing.T) {
	f := newBuiltinFixture(t)
	f.seedSecret(t, "FOO", "", "plaintext-injected")
	sh := testShell(t)

	code, stdout, stderr := run(t, []string{"secret", "exec", "FOO", "--", sh, "-c", `printf out-ok; printf %s "$FOO" >&2`}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if stdout != "out-ok" {
		t.Errorf("stdout = %q, want out-ok (byte-identical, no token, no value)", stdout)
	}
	if stderr != "[REDACTED:FOO]" {
		t.Errorf("stderr = %q, want [REDACTED:FOO]", stderr)
	}
}

// TestSecretExecRedactionPreservesExitCode pins flush-before-return on the
// ExitError path: a child that prints the secret and then exits non-zero must
// still get its output redacted (not dropped, not left raw) AND its exit code
// passed through verbatim.
func TestSecretExecRedactionPreservesExitCode(t *testing.T) {
	f := newBuiltinFixture(t)
	f.seedSecret(t, "FOO", "", "plaintext-injected")
	sh := testShell(t)

	code, stdout, stderr := run(t, []string{"secret", "exec", "FOO", "--", sh, "-c", `printf %s "$FOO"; exit 5`}, f.env())
	if code != 5 {
		t.Fatalf("exit = %d, want the child's 5 (stderr %q)", code, stderr)
	}
	if stdout != "[REDACTED:FOO]" {
		t.Errorf("stdout = %q, want [REDACTED:FOO]", stdout)
	}
}

// TestSecretExecMultipleSecretsRedacted pins that every injected secret is
// redacted under its own name in the same stream, and that surrounding
// non-secret bytes (the marker) pass through byte-identical.
func TestSecretExecMultipleSecretsRedacted(t *testing.T) {
	f := newBuiltinFixture(t)
	f.seedSecret(t, "FOO", "", "foo-value")
	f.seedSecret(t, "BAR", "", "bar-value")
	sh := testShell(t)

	code, stdout, stderr := run(t, []string{"secret", "exec", "FOO", "BAR", "--", sh, "-c", `printf %s "$FOO"; printf MARKER; printf %s "$BAR"`}, f.env())
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	want := "[REDACTED:FOO]MARKER[REDACTED:BAR]"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}
