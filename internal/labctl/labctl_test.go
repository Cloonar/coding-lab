package labctl

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

		{"issue create missing title", []string{"issue", "create", "--body", "b"}, agentEnv, 2, "", "--title is required"},
		{"issue create missing body", []string{"issue", "create", "--title", "t"}, agentEnv, 2, "", "--body is required"},
		{"issue create stray args", []string{"issue", "create", "--title", "t", "--body", "b", "x"}, agentEnv, 2, "", "unexpected arguments"},
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
	pullRef     tracker.PullRef
	createdPull *[4]string // head, base, title, body
	pulls       []tracker.PullRef
	pullDetail  *tracker.PullDetail // served by Pull when the number matches
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
func (f *fakeForge) Pull(_ context.Context, number int) (tracker.PullDetail, error) {
	if f.pullDetail != nil && f.pullDetail.Number == number {
		return *f.pullDetail, nil
	}
	// Shaped like the real client's upstream-404: wraps the typed sentinel.
	return tracker.PullDetail{}, fmt.Errorf("forgejo GET /repos/o/r/pulls/%d: unexpected status 404: %w", number, tracker.ErrNotFound)
}
func (f *fakeForge) CreatePull(_ context.Context, head, base, title, body string) (tracker.PullRef, error) {
	f.createdPull = &[4]string{head, base, title, body}
	return f.pullRef, nil
}
func (f *fakeForge) CloseIssue(context.Context, int) error { return nil }
func (f *fakeForge) CreateIssue(context.Context, string, string, []string) (tracker.Issue, error) {
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
		Provider:           "claude-code",
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

	handler := agentapi.New(st, resolver, nil, nil, func() time.Time { return fixedNow }).Handler()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return &agentFixture{st: st, url: ts.URL, token: token, runID: runID}
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

// TestPRListOutput pins the one-row-per-PR list: number, state, head, url —
// across all states, tab-separated like every labctl list.
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
