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
	run, err := st.CreateRun(context.Background(), Run{
		ID: ids.NewID("run"), RepoID: repoID, Kind: kind, Provider: "claude-code",
		Branch: "afk/7", WorktreePath: "/wt/x", SessionName: session,
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

		// One manual run (excluded), two AFK runs and a lander (returned sorted
		// by session name — the reaper owns all three), one reaped AFK run
		// (excluded — terminal).
		afkFixtureRun(t, st, repo.ID, RunKindManual, "proj~20260706-1200", afkClock)
		afkFixtureRun(t, st, repo.ID, RunKindAFKAuto, "proj~afk-auto-9", afkClock.Add(time.Minute))
		afkFixtureRun(t, st, repo.ID, RunKindAFKManual, "proj~afk-12", afkClock.Add(2*time.Minute))
		afkFixtureRun(t, st, repo.ID, RunKindLander, "proj~lander-7", afkClock.Add(3*time.Minute))
		reaped := afkFixtureRun(t, st, repo.ID, RunKindAFKManual, "proj~afk-3", afkClock)
		if err := st.EndRun(ctx, reaped.ID, RunOutcomeSuccess, afkClock.Add(time.Hour), ""); err != nil {
			t.Fatalf("EndRun: %v", err)
		}

		runs, err := st.ActiveAFKRuns(ctx)
		if err != nil {
			t.Fatalf("ActiveAFKRuns: %v", err)
		}
		if len(runs) != 3 {
			t.Fatalf("ActiveAFKRuns = %d runs, want 3", len(runs))
		}
		if runs[0].SessionName != "proj~afk-12" || runs[1].SessionName != "proj~afk-auto-9" || runs[2].SessionName != "proj~lander-7" {
			t.Errorf("order = [%s, %s, %s], want sorted by session name", runs[0].SessionName, runs[1].SessionName, runs[2].SessionName)
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
