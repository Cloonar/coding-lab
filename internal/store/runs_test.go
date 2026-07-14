package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/migrations"
)

// seedRepoForRuns creates a minimal ready repo so runs (FK repo_id) can be
// inserted.
func seedRepoForRuns(t *testing.T, st *Store) Repo {
	t.Helper()
	r, err := st.CreateRepo(context.Background(), Repo{
		ID: ids.NewID("repo"), Name: "proj-" + ids.NewID("x")[:8], RemoteURL: "/tmp/x",
		TrackerBinding: TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: CloneStatusReady, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return r
}

func manualRun(repoID, session, branch string, at time.Time) Run {
	return Run{
		ID: ids.NewID("run"), RepoID: repoID, Kind: RunKindManual, Provider: "claude-code",
		Branch: branch, WorktreePath: "/wt/" + branch, SessionName: session,
		Model: "opus[1m]", Effort: "max", StartedAt: at, Outcome: RunOutcomeActive,
	}
}

func TestRunLifecycle(t *testing.T) {
	st := openTestSQLite(t)
	ctx := context.Background()
	repo := seedRepoForRuns(t, st)
	now := time.Date(2026, 6, 8, 15, 30, 0, 0, time.UTC)

	created, err := st.CreateRun(ctx, manualRun(repo.ID, "proj~20260608-1530", "lab/20260608-1530", now))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if created.Outcome != RunOutcomeActive {
		t.Errorf("outcome = %q, want active", created.Outcome)
	}

	// RunBySession returns the active row.
	got, err := st.RunBySession(ctx, "proj~20260608-1530")
	if err != nil {
		t.Fatalf("RunBySession: %v", err)
	}
	if got.ID != created.ID || got.Branch != "lab/20260608-1530" {
		t.Errorf("RunBySession = %+v", got)
	}

	// ActiveRuns includes it.
	active, err := st.ActiveRuns(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("ActiveRuns = %v (err %v), want 1", active, err)
	}

	// UpdateRunDeepLink then read it back.
	if err := st.UpdateRunDeepLink(ctx, created.ID, "https://claude.ai/code/session_x"); err != nil {
		t.Fatalf("UpdateRunDeepLink: %v", err)
	}

	// EndRun moves to a terminal outcome; the active scan no longer sees it.
	ended := now.Add(time.Hour)
	if err := st.EndRun(ctx, created.ID, RunOutcomeStopped, ended, ""); err != nil {
		t.Fatalf("EndRun: %v", err)
	}
	if _, err := st.RunBySession(ctx, "proj~20260608-1530"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RunBySession after EndRun err = %v, want ErrNotFound", err)
	}
	active, _ = st.ActiveRuns(ctx)
	if len(active) != 0 {
		t.Errorf("ActiveRuns after EndRun = %d, want 0", len(active))
	}

	// EndRun is idempotent against a double-stop: a second call touches no
	// active row.
	if err := st.EndRun(ctx, created.ID, RunOutcomeDeath, ended, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second EndRun err = %v, want ErrNotFound (only active rows transition)", err)
	}

	// History survives and carries the terminal outcome + deep link.
	hist, err := st.RunsByRepo(ctx, repo.ID, 50)
	if err != nil || len(hist) != 1 {
		t.Fatalf("RunsByRepo = %v (err %v)", hist, err)
	}
	if hist[0].Outcome != RunOutcomeStopped || hist[0].EndedAt == nil {
		t.Errorf("history outcome=%q endedAt=%v", hist[0].Outcome, hist[0].EndedAt)
	}
	if hist[0].DeepLinkURL == nil || *hist[0].DeepLinkURL != "https://claude.ai/code/session_x" {
		t.Errorf("history deep link = %v", hist[0].DeepLinkURL)
	}
}

// TestUpdateRunTitle pins the title overlay (issue #111): set round-trips,
// nil clears back to NULL, an unknown id is ErrNotFound.
func TestUpdateRunTitle(t *testing.T) {
	st := openTestSQLite(t)
	ctx := context.Background()
	repo := seedRepoForRuns(t, st)
	run, err := st.CreateRun(ctx, manualRun(repo.ID, "proj~title", "lab/title", time.Now()))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	title := "Fix the flaky reaper"
	if err := st.UpdateRunTitle(ctx, run.ID, &title); err != nil {
		t.Fatalf("UpdateRunTitle: %v", err)
	}
	got, err := st.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatalf("RunByID: %v", err)
	}
	if got.Title == nil || *got.Title != title {
		t.Errorf("title = %v, want %q", got.Title, title)
	}

	// nil clears back to NULL, read back as nil.
	if err := st.UpdateRunTitle(ctx, run.ID, nil); err != nil {
		t.Fatalf("UpdateRunTitle(nil): %v", err)
	}
	got, err = st.RunByID(ctx, run.ID)
	if err != nil {
		t.Fatalf("RunByID: %v", err)
	}
	if got.Title != nil {
		t.Errorf("cleared title = %q, want nil", *got.Title)
	}

	if err := st.UpdateRunTitle(ctx, "run_missing", &title); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id err = %v, want ErrNotFound", err)
	}
}

// TestRunRemoteRoundTrip pins runs.remote (issue #163): the resolved value the
// launcher stamped, read back verbatim in both directions. It is a plain bool,
// not a tri-state — a run row records what WAS resolved, so there is nothing to
// inherit — but false must still survive the NOT NULL DEFAULT FALSE column
// without being confused for "the column was never written".
func TestRunRemoteRoundTrip(t *testing.T) {
	st := openTestSQLite(t)
	ctx := context.Background()
	repo := seedRepoForRuns(t, st)
	now := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)

	remote := manualRun(repo.ID, "proj~remote", "lab/remote", now)
	remote.Remote = true
	if _, err := st.CreateRun(ctx, remote); err != nil {
		t.Fatalf("CreateRun remote: %v", err)
	}
	local := manualRun(repo.ID, "proj~local", "lab/local", now.Add(time.Minute))
	local.Remote = false
	if _, err := st.CreateRun(ctx, local); err != nil {
		t.Fatalf("CreateRun local: %v", err)
	}

	got, err := st.RunByID(ctx, remote.ID)
	if err != nil {
		t.Fatalf("RunByID remote: %v", err)
	}
	if !got.Remote {
		t.Error("remote run read back Remote=false")
	}
	// Every read path scans the same column list, so check the re-adoption one
	// too: it is the reader that arms deep-link capture after a restart.
	if got, err = st.RunBySession(ctx, "proj~local"); err != nil {
		t.Fatalf("RunBySession local: %v", err)
	}
	if got.Remote {
		t.Error("non-remote run read back Remote=true")
	}
	active, err := st.ActiveRuns(ctx)
	if err != nil || len(active) != 2 {
		t.Fatalf("ActiveRuns = %v (err %v), want 2", active, err)
	}
	for _, r := range active {
		want := r.SessionName == "proj~remote"
		if r.Remote != want {
			t.Errorf("ActiveRuns %s Remote = %v, want %v", r.SessionName, r.Remote, want)
		}
	}
}

// TestRunRemoteBackfill is the migration proof the DoD asks for: rows written
// under the PRE-0011 schema — when every run was spawned with a hardcoded
// --remote-control — must come out of the migration with remote = TRUE, or
// their claude.ai "Open" deep links would go dead. It exercises the real thing:
// goose up to 0010, insert a run through the old schema, then apply 0011 and
// read the row back through the normal store accessors.
func TestRunRemoteBackfill(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lab.db")
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	p, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.SQLite)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}

	// The schema as it stood before this issue: no runs.remote column at all.
	if _, err := p.UpTo(ctx, 10); err != nil {
		t.Fatalf("migrate to 0010: %v", err)
	}
	var hasRemote int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = 'remote'`).Scan(&hasRemote); err != nil {
		t.Fatalf("inspect pre-migration runs table: %v", err)
	}
	if hasRemote != 0 {
		t.Fatalf("runs.remote already exists at version 0010 — the fixture is not pre-migration")
	}

	repoID, runID := ids.NewID("repo"), ids.NewID("run")
	const deepLink = "https://claude.ai/code/session_legacy"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO repos (id, name, remote_url, tracker_binding, forge_kind,
		     afk_branch_pattern, manual_branch_prefix, clone_status, created_at)
		 VALUES (?, ?, ?, 'builtin', 'none', 'afk/<N>', 'lab/', 'ready', ?)`,
		repoID, "legacy", "/tmp/legacy", fmtTime(time.Now())); err != nil {
		t.Fatalf("insert pre-migration repo: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO runs (id, repo_id, kind, provider, branch, worktree_path,
		     session_name, model, effort, deep_link_url, started_at, outcome)
		 VALUES (?, ?, 'manual', 'claude-code', 'lab/legacy', '/wt/legacy',
		     'legacy~1200', 'opus[1m]', 'max', ?, ?, 'active')`,
		runID, repoID, deepLink, fmtTime(time.Now())); err != nil {
		t.Fatalf("insert pre-migration run: %v", err)
	}

	// Apply 0011: the ALTER adds remote with DEFAULT FALSE, the backfill UPDATE
	// then tells the truth about how that run was actually spawned.
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	st := &Store{db: db, dia: dialectSQLite, log: discardLogger(), Now: time.Now}
	got, err := st.RunByID(ctx, runID)
	if err != nil {
		t.Fatalf("RunByID after migration: %v", err)
	}
	if !got.Remote {
		t.Error("pre-migration run has Remote=false after 0011: the backfill did not run, " +
			"and its claude.ai Open link is now dead")
	}
	if got.DeepLinkURL == nil || *got.DeepLinkURL != deepLink {
		t.Errorf("deep link after migration = %v, want %q", got.DeepLinkURL, deepLink)
	}

	// The DEFAULT FALSE that the ALTER needed must not leak into new rows: they
	// carry whatever the resolver stamped, false included.
	fresh := manualRun(repoID, "legacy~1300", "lab/fresh", time.Now())
	if _, err := st.CreateRun(ctx, fresh); err != nil {
		t.Fatalf("CreateRun after migration: %v", err)
	}
	if got, err = st.RunByID(ctx, fresh.ID); err != nil {
		t.Fatalf("RunByID fresh: %v", err)
	}
	if got.Remote {
		t.Error("a run created with Remote=false after the migration read back true")
	}
}

func TestRunsByRepo_newestFirstAndLimit(t *testing.T) {
	st := openTestSQLite(t)
	ctx := context.Background()
	repo := seedRepoForRuns(t, st)
	base := time.Date(2026, 6, 8, 15, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		r := manualRun(repo.ID, "proj~s"+ids.NewID("x")[:6], "lab/b", base.Add(time.Duration(i)*time.Minute))
		if _, err := st.CreateRun(ctx, r); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
	}
	runs, err := st.RunsByRepo(ctx, repo.ID, 3)
	if err != nil {
		t.Fatalf("RunsByRepo: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("limit not applied: got %d, want 3", len(runs))
	}
	for i := 1; i < len(runs); i++ {
		if runs[i-1].StartedAt.Before(runs[i].StartedAt) {
			t.Errorf("runs not newest-first at %d: %v before %v", i, runs[i-1].StartedAt, runs[i].StartedAt)
		}
	}
}

func TestDeleteRun_cascadesTokens(t *testing.T) {
	st := openTestSQLite(t)
	ctx := context.Background()
	repo := seedRepoForRuns(t, st)
	run, err := st.CreateRun(ctx, manualRun(repo.ID, "proj~del", "lab/del", time.Now()))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	token, hash := ids.NewToken("run")
	if err := st.CreateRunToken(ctx, run.ID, hash, nil, time.Now()); err != nil {
		t.Fatalf("CreateRunToken: %v", err)
	}
	if err := st.DeleteRun(ctx, run.ID); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if _, err := st.RunTokenByHash(ctx, ids.HashToken(token)); !errors.Is(err, ErrNotFound) {
		t.Errorf("token survived run delete: err = %v, want ErrNotFound (FK cascade)", err)
	}
}

// TestRunTokenValidityRule pins the §3a auth rule: a token is valid only while
// its joined run is active and unexpired. DeleteRunTokens (the stop/reap
// chokepoint) makes it invalid at once.
func TestRunTokenValidityRule(t *testing.T) {
	st := openTestSQLite(t)
	ctx := context.Background()
	repo := seedRepoForRuns(t, st)
	run, err := st.CreateRun(ctx, manualRun(repo.ID, "proj~tok", "lab/tok", time.Now()))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	token, hash := ids.NewToken("run")
	// Manual run: NULL expiry (no wall clock).
	if err := st.CreateRunToken(ctx, run.ID, hash, nil, time.Now()); err != nil {
		t.Fatalf("CreateRunToken: %v", err)
	}

	info, err := st.RunTokenByHash(ctx, ids.HashToken(token))
	if err != nil {
		t.Fatalf("RunTokenByHash: %v", err)
	}
	if info.Outcome != RunOutcomeActive || info.ExpiresAt != nil {
		t.Errorf("token info = outcome %q expires %v, want active + NULL", info.Outcome, info.ExpiresAt)
	}

	// The stop/reap chokepoint deletes the tokens: lookup now 401s (ErrNotFound).
	if err := st.DeleteRunTokens(ctx, run.ID); err != nil {
		t.Fatalf("DeleteRunTokens: %v", err)
	}
	if _, err := st.RunTokenByHash(ctx, ids.HashToken(token)); !errors.Is(err, ErrNotFound) {
		t.Errorf("token still resolvable after DeleteRunTokens: err = %v, want ErrNotFound", err)
	}
}
