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

// afkFixtureRunOn is afkFixtureRun with an explicit branch — the fix and
// escalate tests below need per-row branch control that afkFixtureRun's fixed
// "afk/7" doesn't give.
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

// afkFixtureRunForPull is afkFixtureRunOn with runs.pull_number set (issue
// #188 / migration 0022) — every row the terminality gate can see carries the
// PR it worked. All these rows share ONE branch on purpose: the requeue case
// this issue fixes is several PRs on the same reused afk/<N> claim branch, so
// a test that varied the branch alongside the pull could not tell a
// PR-scoped query from the branch-scoped one it replaced.
func afkFixtureRunForPull(t *testing.T, st *Store, repoID, kind string, pull int, session string, startedAt time.Time) Run {
	t.Helper()
	run, err := st.CreateRun(context.Background(), Run{
		ID: ids.NewID("run"), RepoID: repoID, Kind: kind, Provider: "claude-code",
		Branch: "afk/5", WorktreePath: "/wt/x", SessionName: session,
		Model: "opus[1m]", Effort: "max", StartedAt: startedAt, Outcome: RunOutcomeActive,
		PullNumber: &pull,
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

// TestAutolandAttempts pins the fix-forward attempt bound's source of truth
// (issue #182 / ADR-0048) and its PR scoping (issue #188 / migration 0022):
// the counter is spawn INTENT keyed on (repo, pull, kind), an absent row reads
// zero, and no other kind, pull, or repo leaks in. The two-pulls-one-repo case
// is the requeue regression itself — pull 41 and pull 42 sit on the SAME
// afk/<N> claim branch, and under the old branch key the second PR was born
// owing the first PR's spent budget.
func TestAutolandAttempts(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		other := afkFixtureRepo(t, st, "other")
		ctx := context.Background()

		// Absent row reads zero — the most permissive value, so a PR that has
		// never been attempted needs no seeding.
		if n, err := st.AutolandAttempts(ctx, repo.ID, 41, RunKindFix); err != nil || n != 0 {
			t.Fatalf("virgin pull: n=%d err=%v, want 0", n, err)
		}

		for range 2 {
			if err := st.RecordAutolandAttempt(ctx, repo.ID, 41, RunKindFix); err != nil {
				t.Fatalf("RecordAutolandAttempt: %v", err)
			}
		}
		if n, err := st.AutolandAttempts(ctx, repo.ID, 41, RunKindFix); err != nil || n != 2 {
			t.Errorf("after two attempts: n=%d err=%v, want 2", n, err)
		}

		// The counter is keyed on all three of (repo, pull, kind): the other
		// kind on the same pull, the same kind on another pull of the same
		// repo, and the same pair in another repo are each their own budget.
		if err := st.RecordAutolandAttempt(ctx, repo.ID, 41, RunKindEscalate); err != nil {
			t.Fatalf("RecordAutolandAttempt escalate: %v", err)
		}
		for _, tc := range []struct {
			name   string
			repoID string
			pull   int
			kind   string
			want   int
		}{
			{"fix on the pull", repo.ID, 41, RunKindFix, 2},
			{"escalate on the pull", repo.ID, 41, RunKindEscalate, 1},
			{"fix on the requeued issue's new pull", repo.ID, 42, RunKindFix, 0},
			{"escalate on the requeued issue's new pull", repo.ID, 42, RunKindEscalate, 0},
			{"fix in another repo", other.ID, 41, RunKindFix, 0},
		} {
			n, err := st.AutolandAttempts(ctx, tc.repoID, tc.pull, tc.kind)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if n != tc.want {
				t.Errorf("%s: n=%d, want %d", tc.name, n, tc.want)
			}
		}

		// The counter is spawn-INTENT, decoupled from runs rows entirely: a
		// pull carrying no fix run at all still reports its burned attempts
		// (the launch-pad-failure case that a row count cannot see).
		if runs, err := st.RunsByRepo(ctx, repo.ID, 0); err != nil {
			t.Fatalf("RunsByRepo: %v", err)
		} else if len(runs) != 0 {
			t.Fatalf("runs = %d, want 0 — attempts must not depend on run rows", len(runs))
		}
	})
}

// TestEscalatedRunForPull pins the poller's terminality gate after issue #188
// re-scoped it from the branch to the PR and made it return a MOMENT rather
// than a bool: the newest outcome='escalated' run's ended_at for the pull,
// ok=false when none exists, and nothing else — not an active escalate run,
// not one that ended success, not a NULL-pull_number row from before 0022, not
// another pull on the same claim branch, not another repo. Round-trips both
// CHECK values (kind='escalate', outcome='escalated') through the normal store
// API alongside the new runs.pull_number column.
func TestEscalatedRunForPull(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		other := afkFixtureRepo(t, st, "other")
		ctx := context.Background()

		if at, ok, err := st.EscalatedRunForPull(ctx, repo.ID, 41); err != nil || ok || !at.IsZero() {
			t.Fatalf("virgin pull: at=%v ok=%v err=%v, want zero/false", at, ok, err)
		}

		// An active escalate run is not yet the terminal signal.
		active := afkFixtureRunForPull(t, st, repo.ID, RunKindEscalate, 41, "proj~escalate-1", afkClock)
		if _, ok, _ := st.EscalatedRunForPull(ctx, repo.ID, 41); ok {
			t.Error("active escalate run reported terminal")
		}
		// A merged escalate run (Classify's merged-first rule) ends success,
		// not escalated — also not the terminal signal.
		if err := st.EndRun(ctx, active.ID, RunOutcomeSuccess, afkClock.Add(time.Hour), ""); err != nil {
			t.Fatalf("EndRun: %v", err)
		}
		if _, ok, _ := st.EscalatedRunForPull(ctx, repo.ID, 41); ok {
			t.Error("success-outcome escalate run reported terminal")
		}

		// A pre-0022 row — pull_number NULL — can never match. That is the
		// documented upgrade path, not an oversight: the escalate MARKER
		// comment keeps those PRs terminal through the poller's other half.
		legacy := afkFixtureRunOn(t, st, repo.ID, RunKindEscalate, "afk/5", "proj~escalate-legacy", afkClock)
		if err := st.EndRun(ctx, legacy.ID, RunOutcomeEscalated, afkClock.Add(time.Hour), ""); err != nil {
			t.Fatalf("EndRun legacy: %v", err)
		}
		if _, ok, _ := st.EscalatedRunForPull(ctx, repo.ID, 41); ok {
			t.Error("a NULL-pull_number run matched the PR-scoped gate")
		}

		// Only outcome='escalated' on the pull flips the gate, and the answer
		// is that run's ended_at.
		esc := afkFixtureRunForPull(t, st, repo.ID, RunKindEscalate, 41, "proj~escalate-2", afkClock.Add(time.Minute))
		endedAt := afkClock.Add(time.Hour)
		if err := st.EndRun(ctx, esc.ID, RunOutcomeEscalated, endedAt, ""); err != nil {
			t.Fatalf("EndRun: %v", err)
		}
		at, ok, err := st.EscalatedRunForPull(ctx, repo.ID, 41)
		if err != nil || !ok {
			t.Fatalf("EscalatedRunForPull: at=%v ok=%v err=%v, want the escalation moment", at, ok, err)
		}
		if !at.Equal(storedTime(endedAt)) {
			t.Errorf("moment = %v, want the escalated run's ended_at %v", at, storedTime(endedAt))
		}

		// Scoped: the requeued issue's next PR on the SAME claim branch, and
		// another repo, both read clear — terminality is per-PR, never a
		// branch-wide or repo-wide lock. (afkFixtureRunForPull pins branch
		// afk/5 for every row above, so branch cannot be doing this work.)
		if _, ok, _ := st.EscalatedRunForPull(ctx, repo.ID, 42); ok {
			t.Error("the next PR on the reused claim branch inherited terminality")
		}
		if _, ok, _ := st.EscalatedRunForPull(ctx, other.ID, 41); ok {
			t.Error("another repo's escalated run leaked into the query")
		}
	})
}

// TestEscalatedRunForPull_newestWins is the reason the max is computed in Go
// rather than with ORDER BY ... LIMIT 1. Three escalated runs on one PR, laid
// out so the two tempting SQL shortcuts each pick a DIFFERENT, wrong row:
//
//   - ORDER BY started_at DESC LIMIT 1 picks the run that started last but
//     ended first, reporting 12:45 — an escalation moment three quarters of an
//     hour before the real one, which a re-arm at 12:50 would then wrongly
//     supersede.
//   - the winning row's ended_at differs from the runner-up's only in the
//     fractional second, so any comparison that stops at whole seconds — or
//     that orders text rendered by a formatter which drops trailing zeros —
//     picks the older row.
//
// The answer must be the newest ended_at, 13:00:00.500.
func TestEscalatedRunForPull_newestWins(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		ctx := context.Background()

		newest := afkClock.Add(time.Hour).Add(500 * time.Millisecond) // 13:00:00.500
		for i, tc := range []struct {
			session          string
			started, endedAt time.Time
		}{
			{"proj~escalate-a", afkClock, newest},
			{"proj~escalate-b", afkClock.Add(30 * time.Minute), afkClock.Add(45 * time.Minute)},
			{"proj~escalate-c", afkClock.Add(10 * time.Minute), afkClock.Add(time.Hour)}, // 13:00:00.000
		} {
			run := afkFixtureRunForPull(t, st, repo.ID, RunKindEscalate, 41, tc.session, tc.started)
			if err := st.EndRun(ctx, run.ID, RunOutcomeEscalated, tc.endedAt, ""); err != nil {
				t.Fatalf("EndRun %d: %v", i, err)
			}
		}

		at, ok, err := st.EscalatedRunForPull(ctx, repo.ID, 41)
		if err != nil || !ok {
			t.Fatalf("EscalatedRunForPull: at=%v ok=%v err=%v", at, ok, err)
		}
		if !at.Equal(storedTime(newest)) {
			t.Errorf("moment = %v, want the NEWEST escalation %v — the max is over "+
				"COALESCE(ended_at, started_at) parsed as time, not a SQL ORDER BY", at, storedTime(newest))
		}
	})
}

// TestPullRearmedAt pins the supersession record's read side (issue #188 /
// ADR-0048's amendment): never re-armed is a zero Time and not an error (the
// AutolandAttempts absent-row rule — every real escalation instant is after
// the zero Time, so the gate's comparison needs no special case), a re-arm
// reads back the exact stored instant, and a second re-arm OVERWRITES rather
// than accumulating, because only the newest gesture matters.
func TestPullRearmedAt(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		other := afkFixtureRepo(t, st, "other")
		ctx := context.Background()

		got, err := st.PullRearmedAt(ctx, repo.ID, 41)
		if err != nil {
			t.Fatalf("never re-armed must not be an error: %v", err)
		}
		if !got.IsZero() {
			t.Errorf("never re-armed = %v, want the zero Time", got)
		}

		first := afkClock.Add(2 * time.Hour)
		if err := st.RearmPull(ctx, repo.ID, 41, first); err != nil {
			t.Fatalf("RearmPull: %v", err)
		}
		if got, err = st.PullRearmedAt(ctx, repo.ID, 41); err != nil || !got.Equal(storedTime(first)) {
			t.Errorf("after re-arm = %v (err %v), want %v", got, err, storedTime(first))
		}

		// Scoped per (repo, pull): neither the next PR nor another repo is
		// re-armed by this gesture.
		if got, _ := st.PullRearmedAt(ctx, repo.ID, 42); !got.IsZero() {
			t.Errorf("another pull reads re-armed at %v", got)
		}
		if got, _ := st.PullRearmedAt(ctx, other.ID, 41); !got.IsZero() {
			t.Errorf("another repo reads re-armed at %v", got)
		}

		// Re-arm is indefinitely repeatable: the second gesture supersedes the
		// first in place — one row per PR, no history to prune.
		second := first.Add(90 * time.Minute)
		if err := st.RearmPull(ctx, repo.ID, 41, second); err != nil {
			t.Fatalf("second RearmPull: %v", err)
		}
		if got, err = st.PullRearmedAt(ctx, repo.ID, 41); err != nil || !got.Equal(storedTime(second)) {
			t.Errorf("after second re-arm = %v (err %v), want %v", got, err, storedTime(second))
		}
		if n := count(t, st, "autoland_rearms"); n != 1 {
			t.Errorf("autoland_rearms rows = %d, want 1 — the upsert must overwrite, not append", n)
		}
	})
}

// TestRearmPull_zeroesBudgetsAtomically pins the half this operation exists
// for (issue #188 / ADR-0048's amendment). Clearing terminality while leaving
// the fix budget spent would escalate again on the first rejection — the worst
// outcome, because it reads as "the re-arm silently did not work" — so the
// supersession record and BOTH attempt budgets move together, in one
// transaction, and no half-applied state is ever observable. Zeroing is a
// DELETE of the pull's rows: an absent row already means zero.
func TestRearmPull_zeroesBudgetsAtomically(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		repo := afkFixtureRepo(t, st, "proj")
		ctx := context.Background()

		// Pull 41 has spent both budgets; pull 42 — the requeued issue's next
		// PR on the same claim branch — has spent a fix attempt of its own.
		for range 2 {
			if err := st.RecordAutolandAttempt(ctx, repo.ID, 41, RunKindFix); err != nil {
				t.Fatalf("RecordAutolandAttempt fix: %v", err)
			}
		}
		if err := st.RecordAutolandAttempt(ctx, repo.ID, 41, RunKindEscalate); err != nil {
			t.Fatalf("RecordAutolandAttempt escalate: %v", err)
		}
		if err := st.RecordAutolandAttempt(ctx, repo.ID, 42, RunKindFix); err != nil {
			t.Fatalf("RecordAutolandAttempt other pull: %v", err)
		}

		at := afkClock.Add(3 * time.Hour)
		if err := st.RearmPull(ctx, repo.ID, 41, at); err != nil {
			t.Fatalf("RearmPull: %v", err)
		}

		// Both halves landed: full budgets AND the supersession moment. Either
		// one missing is the silent-failure mode the atomicity exists to rule
		// out, so they are asserted together.
		for _, kind := range []string{RunKindFix, RunKindEscalate} {
			if n, err := st.AutolandAttempts(ctx, repo.ID, 41, kind); err != nil || n != 0 {
				t.Errorf("%s attempts after re-arm = %d (err %v), want 0 — a re-armed PR "+
					"that re-escalates on the first rejection looks like the re-arm failed", kind, n, err)
			}
		}
		if got, err := st.PullRearmedAt(ctx, repo.ID, 41); err != nil || !got.Equal(storedTime(at)) {
			t.Errorf("rearmed_at after re-arm = %v (err %v), want %v — budgets restored on a PR "+
				"the poller still cannot see are budgets that are never spent", got, err, storedTime(at))
		}

		// The other PR's budget is untouched: re-arm is per-PR, not a repo-wide
		// amnesty.
		if n, err := st.AutolandAttempts(ctx, repo.ID, 42, RunKindFix); err != nil || n != 1 {
			t.Errorf("another pull's fix attempts = %d (err %v), want 1", n, err)
		}

		// A vanished repo is refused, not silently recorded against nothing:
		// the autoland_rearms FK surfaces as ErrNotFound, the package's
		// convention for a write against a missing parent.
		if err := st.RearmPull(ctx, "repo_missing", 41, at); !errors.Is(err, ErrNotFound) {
			t.Errorf("RearmPull on a missing repo = %v, want ErrNotFound", err)
		}
		if n := count(t, st, "autoland_rearms"); n != 1 {
			t.Errorf("autoland_rearms rows = %d, want 1 — the refused re-arm wrote a row", n)
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
