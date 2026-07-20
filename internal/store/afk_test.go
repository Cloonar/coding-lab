package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

var afkClock = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

func afkFixtureRepo(t *testing.T, st *Store, name string) Repo {
	t.Helper()
	repo, err := st.CreateRepo(context.Background(), Repo{
		ID: ids.NewID("repo"), Name: name, RemoteURL: "https://example.invalid/" + name + ".git",
		TrackerBinding: TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: CloneStatusReady, CreatedAt: afkClock,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return repo
}

func afkFixtureRun(t *testing.T, st *Store, repoID, kind, session string, startedAt time.Time) Run {
	t.Helper()
	return afkFixtureRunOn(t, st, repoID, kind, "afk/7", session, startedAt)
}

// afkFixtureRunOn is afkFixtureRun with an explicit branch — the fix,
// escalate, and authoring-run tests below need per-row branch control that
// afkFixtureRun's fixed "afk/7" doesn't give.
func afkFixtureRunOn(t *testing.T, st *Store, repoID, kind, branch, session string, startedAt time.Time) Run {
	t.Helper()
	run, err := st.CreateRun(context.Background(), Run{
		ID: ids.NewID("run"), RepoID: repoID, Kind: kind, Provider: "claude-code",
		Branch: branch, WorktreePath: "/wt/x", SessionName: session,
		Model: "opus[1m]", Effort: "max", StartedAt: startedAt, Outcome: RunOutcomeActive,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run
}

func TestActiveAFKRuns_filtersAndOrders(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		ctx := context.Background()

		// One manual run (excluded), five unattended-kind runs — two AFK, a
		// lander, a fix, and an escalate (returned sorted by session name —
		// the reaper owns the classification of all five, issue #182), one
		// reaped AFK run (excluded — terminal).
		afkFixtureRun(t, st, repo.ID, RunKindManual, "proj~20260706-1200", afkClock)
		afkFixtureRun(t, st, repo.ID, RunKindAFKAuto, "proj~afk-auto-9", afkClock.Add(time.Minute))
		afkFixtureRun(t, st, repo.ID, RunKindAFKManual, "proj~afk-12", afkClock.Add(2*time.Minute))
		afkFixtureRun(t, st, repo.ID, RunKindLander, "proj~lander-7", afkClock.Add(3*time.Minute))
		afkFixtureRun(t, st, repo.ID, RunKindFix, "proj~fix-5", afkClock.Add(4*time.Minute))
		afkFixtureRun(t, st, repo.ID, RunKindEscalate, "proj~escalate-4", afkClock.Add(5*time.Minute))
		reaped := afkFixtureRun(t, st, repo.ID, RunKindAFKManual, "proj~afk-3", afkClock)
		if err := st.EndRun(ctx, reaped.ID, RunOutcomeSuccess, afkClock.Add(time.Hour), ""); err != nil {
			t.Fatalf("EndRun: %v", err)
		}

		runs, err := st.ActiveAFKRuns(ctx)
		if err != nil {
			t.Fatalf("ActiveAFKRuns: %v", err)
		}
		if len(runs) != 5 {
			t.Fatalf("ActiveAFKRuns = %d runs, want 5", len(runs))
		}
		wantOrder := []string{"proj~afk-12", "proj~afk-auto-9", "proj~escalate-4", "proj~fix-5", "proj~lander-7"}
		for i, want := range wantOrder {
			if runs[i].SessionName != want {
				t.Errorf("order[%d] = %s, want %s (sorted by session name)", i, runs[i].SessionName, want)
			}
		}
	})
}

func TestActiveRunsByRepo(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		other := afkFixtureRepo(t, st, "other")
		ctx := context.Background()

		afkFixtureRun(t, st, repo.ID, RunKindManual, "proj~20260706-1200", afkClock)
		newest := afkFixtureRun(t, st, repo.ID, RunKindAFKManual, "proj~afk-7", afkClock.Add(time.Minute))
		afkFixtureRun(t, st, other.ID, RunKindAFKManual, "other~afk-1", afkClock)
		ended := afkFixtureRun(t, st, repo.ID, RunKindAFKManual, "proj~afk-2", afkClock)
		if err := st.EndRun(ctx, ended.ID, RunOutcomeStopped, afkClock.Add(time.Hour), ""); err != nil {
			t.Fatalf("EndRun: %v", err)
		}

		runs, err := st.ActiveRunsByRepo(ctx, repo.ID)
		if err != nil {
			t.Fatalf("ActiveRunsByRepo: %v", err)
		}
		if len(runs) != 2 {
			t.Fatalf("ActiveRunsByRepo = %d runs, want 2 (repo-scoped, active only)", len(runs))
		}
		if runs[0].ID != newest.ID {
			t.Errorf("first run = %s, want the newest (%s)", runs[0].ID, newest.ID)
		}
	})
}

func TestActiveAutoRunForRepo(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		other := afkFixtureRepo(t, st, "other")
		ctx := context.Background()

		if _, found, err := st.ActiveAutoRunForRepo(ctx, repo.ID); err != nil || found {
			t.Fatalf("empty repo: found=%v err=%v, want no auto run", found, err)
		}

		// A manual AFK run does not count as the repo's auto run.
		afkFixtureRun(t, st, repo.ID, RunKindAFKManual, "proj~afk-7", afkClock)
		if _, found, _ := st.ActiveAutoRunForRepo(ctx, repo.ID); found {
			t.Fatal("manual AFK run reported as the repo's auto run")
		}

		auto := afkFixtureRun(t, st, repo.ID, RunKindAFKAuto, "proj~afk-auto-8", afkClock)
		got, found, err := st.ActiveAutoRunForRepo(ctx, repo.ID)
		if err != nil || !found || got.ID != auto.ID {
			t.Fatalf("auto run: found=%v got=%q err=%v", found, got.ID, err)
		}
		// Scoped per repo.
		if _, found, _ := st.ActiveAutoRunForRepo(ctx, other.ID); found {
			t.Error("another repo's auto run leaked into the query")
		}
		// A terminal auto run stops counting.
		if err := st.EndRun(ctx, auto.ID, RunOutcomeDeath, afkClock.Add(time.Hour), "x"); err != nil {
			t.Fatalf("EndRun: %v", err)
		}
		if _, found, _ := st.ActiveAutoRunForRepo(ctx, repo.ID); found {
			t.Error("reaped auto run still reported in flight")
		}
	})
}

func TestActiveRunOnBranch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		other := afkFixtureRepo(t, st, "other")
		ctx := context.Background()

		// Empty repo: nothing on the branch.
		if got, err := st.ActiveRunOnBranch(ctx, repo.ID, "afk/7"); err != nil || got {
			t.Fatalf("empty repo: got=%v err=%v, want no active run", got, err)
		}

		// ANY kind counts — a manual instance parked on the branch suppresses
		// a lander exactly like an AFK run (afkFixtureRun pins branch afk/7).
		run := afkFixtureRun(t, st, repo.ID, RunKindManual, "proj~20260706-1200", afkClock)
		if got, _ := st.ActiveRunOnBranch(ctx, repo.ID, "afk/7"); !got {
			t.Error("active manual run on the branch not reported")
		}
		// Scoped: another branch and another repo both read clear.
		if got, _ := st.ActiveRunOnBranch(ctx, repo.ID, "afk/8"); got {
			t.Error("a different branch reported the run")
		}
		if got, _ := st.ActiveRunOnBranch(ctx, other.ID, "afk/7"); got {
			t.Error("another repo's run leaked into the query")
		}
		// A terminal run stops counting — the reaped authoring run frees the
		// branch for its lander within one tick.
		if err := st.EndRun(ctx, run.ID, RunOutcomeSuccess, afkClock.Add(time.Hour), ""); err != nil {
			t.Fatalf("EndRun: %v", err)
		}
		if got, _ := st.ActiveRunOnBranch(ctx, repo.ID, "afk/7"); got {
			t.Error("terminal run still reported as active on the branch")
		}
	})
}

// TestFixRunCountForBranch pins the fix-forward attempt bound's source of
// truth (issue #182 / ADR-0048): every kind='fix' row on the branch counts,
// regardless of outcome — a fix run that dies on the launch pad still burns
// an attempt — and no other kind or branch/repo leaks in.
func TestFixRunCountForBranch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		other := afkFixtureRepo(t, st, "other")
		ctx := context.Background()

		if n, err := st.FixRunCountForBranch(ctx, repo.ID, "afk/7"); err != nil || n != 0 {
			t.Fatalf("empty branch: n=%d err=%v, want 0", n, err)
		}

		// One active fix run and one dead fix run — both count.
		afkFixtureRunOn(t, st, repo.ID, RunKindFix, "afk/7", "proj~fix-1", afkClock)
		dead := afkFixtureRunOn(t, st, repo.ID, RunKindFix, "afk/7", "proj~fix-2", afkClock.Add(time.Minute))
		if err := st.EndRun(ctx, dead.ID, RunOutcomeDeath, afkClock.Add(time.Hour), "x"); err != nil {
			t.Fatalf("EndRun: %v", err)
		}
		// A non-fix kind on the same branch, a fix run on a different branch,
		// and a fix run in a different repo must never count.
		afkFixtureRunOn(t, st, repo.ID, RunKindAFKAuto, "afk/7", "proj~afk-9", afkClock)
		afkFixtureRunOn(t, st, repo.ID, RunKindFix, "afk/8", "proj~fix-other-branch", afkClock)
		afkFixtureRunOn(t, st, other.ID, RunKindFix, "afk/7", "other~fix-1", afkClock)

		n, err := st.FixRunCountForBranch(ctx, repo.ID, "afk/7")
		if err != nil {
			t.Fatalf("FixRunCountForBranch: %v", err)
		}
		if n != 2 {
			t.Errorf("count = %d, want 2 (active and death both count as spawns)", n)
		}
	})
}

// TestAuthoringRunForBranch pins the provider/model/effort inheritance
// source for a fix run (issue #182 / ADR-0048): the LATEST afk_manual/
// afk_auto row on the branch, ignoring manual/lander/fix rows entirely, even
// ones that started later.
func TestAuthoringRunForBranch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		ctx := context.Background()

		if _, found, err := st.AuthoringRunForBranch(ctx, repo.ID, "afk/9"); err != nil || found {
			t.Fatalf("empty branch: found=%v err=%v, want none", found, err)
		}

		afkFixtureRunOn(t, st, repo.ID, RunKindManual, "afk/9", "proj~manual", afkClock)
		afkFixtureRunOn(t, st, repo.ID, RunKindAFKAuto, "afk/9", "proj~afk-auto", afkClock.Add(time.Minute))
		newer := afkFixtureRunOn(t, st, repo.ID, RunKindAFKManual, "afk/9", "proj~afk-manual", afkClock.Add(2*time.Minute))
		// The authoring run is normally already success-reaped by the time a
		// fix run spawns (its PR already exists) — outcome is deliberately
		// not filtered, so a terminal authoring run must still be found.
		if err := st.EndRun(ctx, newer.ID, RunOutcomeSuccess, afkClock.Add(time.Hour), ""); err != nil {
			t.Fatalf("EndRun: %v", err)
		}
		// Later lander and fix rows on the same branch are not afk kinds and
		// must be ignored even though they started after the authoring run.
		afkFixtureRunOn(t, st, repo.ID, RunKindLander, "afk/9", "proj~lander", afkClock.Add(3*time.Minute))
		afkFixtureRunOn(t, st, repo.ID, RunKindFix, "afk/9", "proj~fix-1", afkClock.Add(4*time.Minute))

		got, found, err := st.AuthoringRunForBranch(ctx, repo.ID, "afk/9")
		if err != nil || !found {
			t.Fatalf("AuthoringRunForBranch: found=%v err=%v", found, err)
		}
		if got.ID != newer.ID {
			t.Errorf("authoring run = %q, want the latest afk-kind row %q", got.ID, newer.ID)
		}
	})
}

// TestEscalatedRunOnBranch pins the poller's permanent-terminality gate
// (issue #182 / ADR-0048): true only once an outcome='escalated' row exists
// for the branch, never on an active or otherwise-terminal escalate run.
// Round-trips both new CHECK values (kind='escalate', outcome='escalated')
// through the normal store API, proving the widened migration constraints.
func TestEscalatedRunOnBranch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		other := afkFixtureRepo(t, st, "other")
		ctx := context.Background()

		if got, err := st.EscalatedRunOnBranch(ctx, repo.ID, "afk/5"); err != nil || got {
			t.Fatalf("empty branch: got=%v err=%v, want false", got, err)
		}

		// An active escalate run is not yet the terminal signal.
		active := afkFixtureRunOn(t, st, repo.ID, RunKindEscalate, "afk/5", "proj~escalate-1", afkClock)
		if got, _ := st.EscalatedRunOnBranch(ctx, repo.ID, "afk/5"); got {
			t.Error("active escalate run reported terminal")
		}
		// A merged escalate run (Classify's merged-first rule) ends success,
		// not escalated — also not the terminal signal.
		if err := st.EndRun(ctx, active.ID, RunOutcomeSuccess, afkClock.Add(time.Hour), ""); err != nil {
			t.Fatalf("EndRun: %v", err)
		}
		if got, _ := st.EscalatedRunOnBranch(ctx, repo.ID, "afk/5"); got {
			t.Error("success-outcome escalate run reported terminal")
		}

		// Only outcome='escalated' flips the gate.
		esc := afkFixtureRunOn(t, st, repo.ID, RunKindEscalate, "afk/5", "proj~escalate-2", afkClock.Add(time.Minute))
		if err := st.EndRun(ctx, esc.ID, RunOutcomeEscalated, afkClock.Add(time.Hour), ""); err != nil {
			t.Fatalf("EndRun: %v", err)
		}
		got, err := st.EscalatedRunOnBranch(ctx, repo.ID, "afk/5")
		if err != nil || !got {
			t.Fatalf("EscalatedRunOnBranch: got=%v err=%v, want true", got, err)
		}
		// Scoped: a different branch and a different repo both read clear —
		// escalation is per-branch, never a repo-wide lock.
		if got, _ := st.EscalatedRunOnBranch(ctx, repo.ID, "afk/6"); got {
			t.Error("a different branch reported escalated")
		}
		if got, _ := st.EscalatedRunOnBranch(ctx, other.ID, "afk/5"); got {
			t.Error("another repo's escalated run leaked into the query")
		}
	})
}

func TestIncrementAndResetRepoFailures(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		ctx := context.Background()

		for want := 1; want <= 3; want++ {
			n, err := st.IncrementRepoFailures(ctx, repo.ID)
			if err != nil {
				t.Fatalf("IncrementRepoFailures: %v", err)
			}
			if n != want {
				t.Errorf("counter = %d, want %d", n, want)
			}
		}
		got, err := st.RepoByID(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ConsecutiveFailures != 3 {
			t.Errorf("persisted counter = %d, want 3", got.ConsecutiveFailures)
		}

		changed, err := st.ResetRepoFailures(ctx, repo.ID)
		if err != nil || !changed {
			t.Fatalf("ResetRepoFailures: changed=%v err=%v, want a real transition", changed, err)
		}
		if got, _ := st.RepoByID(ctx, repo.ID); got.ConsecutiveFailures != 0 {
			t.Errorf("counter after reset = %d, want 0", got.ConsecutiveFailures)
		}
		// Resetting an already-zero counter is not a change (no repo.changed).
		changed, err = st.ResetRepoFailures(ctx, repo.ID)
		if err != nil || changed {
			t.Errorf("second reset: changed=%v err=%v, want no-op", changed, err)
		}

		if _, err := st.IncrementRepoFailures(ctx, "repo_missing"); !errors.Is(err, ErrNotFound) {
			t.Errorf("increment on missing repo = %v, want ErrNotFound", err)
		}
	})
}
