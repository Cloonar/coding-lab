package afk

// Acceptance-criteria pins for issue #185, at the integration level: SpawnOnce
// driven end-to-end — real gathers over the scripted tracker, real launches
// through the engine's locked paths — where spawn_test.go pins the pass core
// over synthetic candidates. Two criteria live here: at cap the ONE free slot
// goes to the lander candidate in preference to the new-work candidate
// (asserted, not incidental), and concurrent scheduler/reaper tick bodies can
// never over-subscribe the cap (the pre-#185 structure was two loops racing
// one bounded resource — a race by construction). Same fixture as
// engine_test.go / autoland_test.go: fakes for tmux/tracker/provider, a REAL
// store, REAL git fixtures.

import (
	"context"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// The priority AC (#185): with BOTH a virgin claim PR (lander candidate) and a
// claimable ready issue (new-work candidate) in the same pass, and exactly ONE
// free slot under the cap, the slot goes to the lander — drain the pipeline
// before filling it. The second half proves the losing candidate was DEFERRED,
// not lost: the claim PR merges, the reap frees the slot the natural way, and
// the next pass spends it on the new-work candidate.
func TestSpawnOnce_lastSlotGoesToLanderNotNewWork(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	autoOn(f, f.repo)
	// The lander candidate: a virgin claim PR (open, claim-branch head, no
	// review, no verdict, nobody on the branch).
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen) // pull #1
	// The new-work candidate: a claimable ready issue. Issue 8, not 7 — 7 is
	// parked behind its claim branch and could never be claimed anyway, so the
	// two candidates are genuinely independent.
	f.trk.setReady(8)
	// Exactly one free slot: cap 1, zero live sessions.
	if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
		t.Fatal(err)
	}

	f.svc.SpawnOnce(t.Context())

	active, err := f.st.ActiveRuns(t.Context())
	if err != nil {
		t.Fatalf("ActiveRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("pass spawned %d runs, want exactly 1 (one free slot under cap 1)", len(active))
	}
	if active[0].Kind != store.RunKindLander {
		t.Fatalf("the one slot went to a %q run, want %q (at cap the lander must outrank new work)",
			active[0].Kind, store.RunKindLander)
	}
	// Deferred WITHOUT side effects: the claim happens inside the launch
	// closure, and the pass never called the vetoed candidate's closure — so
	// no claim branch may exist for the deferred issue.
	if f.branchExists(f.repo, "afk/8") {
		t.Error("the deferred new-work candidate claimed issue 8 anyway (a claim with no run)")
	}

	// Free the slot the natural way: the claim PR merges, the reap ends the
	// lander (merged PR is its done-signal) and kills its session.
	f.trk.mu.Lock()
	f.trk.pulls[0].State = tracker.PullMerged
	f.trk.mu.Unlock()
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if runs := landerRuns(f, f.repo); len(runs) != 0 {
		t.Fatalf("lander still active after its merged-PR reap (%d runs) — no freed slot to prove deferral with", len(runs))
	}

	// The next pass spends the freed slot on the deferred new-work candidate.
	f.svc.SpawnOnce(t.Context())
	active, err = f.st.ActiveRuns(t.Context())
	if err != nil {
		t.Fatalf("ActiveRuns: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("second pass spawned %d runs, want exactly 1 (the freed slot)", len(active))
	}
	run := active[0]
	if run.Kind != store.RunKindAFKAuto || run.IssueNumber == nil || *run.IssueNumber != 8 {
		t.Fatalf("second pass run = kind %q issue %v, want afk_auto on issue 8 (deferred, not lost)",
			run.Kind, run.IssueNumber)
	}
}

// The no-over-subscription AC (#185): the two long-lived loops' tick bodies —
// the reaper tick (ReapOnce then SpawnOnce, the ReaperLoop body) and the
// scheduler tick (SpawnOnce, the SchedulerLoop body) — race against one repo
// with cap 1 and BOTH candidate kinds spawnable. The interleave is free; the
// ASSERTION is the deterministic total: live instances and active runs never
// exceed the cap, however the ticks fell. And not starved either — work was
// available and a slot was free, so the winning pass must have spent it, on
// the lander (the stage order holds inside every pass, so the winner's choice
// is interleave-independent). Run under -race as well: the pre-#185 structure
// was exactly this pair of goroutines racing one bounded resource.
func TestSpawnOnce_concurrentReaperAndSchedulerTicksCannotOversubscribeCap(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	autoOn(f, f.repo)
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen) // pull #1: the lander candidate
	f.trk.setReady(8)                        // the new-work candidate
	if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // the reaper tick body (ReaperLoop)
		defer wg.Done()
		f.svc.ReapOnce(context.Background(), clockTime.Add(time.Minute))
		f.svc.SpawnOnce(context.Background())
	}()
	go func() { // the scheduler tick body (SchedulerLoop)
		defer wg.Done()
		f.svc.SpawnOnce(context.Background())
	}()
	wg.Wait()

	// The bounded resource stayed bounded: never more live instances — or
	// active run rows — than the cap.
	live, err := f.runner.List(t.Context())
	if err != nil {
		t.Fatalf("runner.List: %v", err)
	}
	if n := instance.LiveInstanceCount(live); n > 1 {
		t.Fatalf("live instances = %d, want <= cap 1: concurrent ticks over-subscribed the cap", n)
	}
	active, err := f.st.ActiveRuns(t.Context())
	if err != nil {
		t.Fatalf("ActiveRuns: %v", err)
	}
	if len(active) > 1 {
		t.Fatalf("active runs = %d, want <= cap 1: concurrent ticks over-subscribed the cap", len(active))
	}
	if len(active) != 1 {
		t.Fatalf("active runs = %d, want exactly 1 (a slot was free and work was spawnable — bounded, not starved)", len(active))
	}
	if active[0].Kind != store.RunKindLander {
		t.Errorf("the slot went to a %q run, want %q (the stage order holds in whichever pass won)",
			active[0].Kind, store.RunKindLander)
	}
}

// Bonus end-to-end floor pin (#185): a repo sitting at ITS per-repo cap must
// not starve another repo's candidate in the same pass. Cross-producer, on
// purpose — repo A's would-be candidate is a LANDER (vetoed at A's own cap,
// before any forge read is spent on it), repo B's is NEW WORK with default-cap
// headroom, and B still launches in the same pass. The pass-core floor-raise
// rows live in spawn_test.go; the new-work-only atCap variant is
// TestSchedule_perRepoCapDoesNotAbortTick.
func TestSpawnOnce_atCapLanderRepoDoesNotStarveOtherRepoSamePass(t *testing.T) {
	f := newFixture(t)
	// Repo A ("proj"): autoland on with a virgin claim PR — a lander candidate
	// in any other pass — but a per-repo cap of 1 already consumed by an
	// unrelated live session.
	autolandOn(f, f.repo)
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen)
	one := 1
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		MaxInstancesOverride: store.Set(&one),
	}); err != nil {
		t.Fatal(err)
	}
	f.runner.AddLive("other~existing") // global live count 1: == A's cap, < B's default cap

	// Repo B ("zzz" — listed after A): AFK auto on with a claimable ready
	// issue and headroom under the default cap.
	repoB, trkB := f.addRepo("zzz", "afk/<N>")
	autoOn(f, repoB)
	trkB.setReady(9)

	f.svc.SpawnOnce(t.Context())

	// A: vetoed at its own cap — no lander, and zero forge reads spent on it
	// (the veto sits before the pull listing).
	if runs := landerRuns(f, f.repo); len(runs) != 0 {
		t.Errorf("at-cap repo A spawned %d lander runs, want 0", len(runs))
	}
	if calls := f.trk.pullsCallCount(); calls != 0 {
		t.Errorf("at-cap repo A's pulls were listed %d times, want 0 (the cap veto precedes the forge reads)", calls)
	}
	// B: launched in the SAME pass — one repo's cap is a per-repo floor, never
	// a whole-pass stop.
	runsB, err := f.st.ActiveRunsByRepo(t.Context(), repoB.ID)
	if err != nil {
		t.Fatalf("ActiveRunsByRepo: %v", err)
	}
	if len(runsB) != 1 || runsB[0].Kind != store.RunKindAFKAuto {
		t.Fatalf("repo B active runs = %+v, want exactly one afk_auto run launched in the same pass", runsB)
	}
	if runsB[0].IssueNumber == nil || *runsB[0].IssueNumber != 9 {
		t.Errorf("repo B claimed issue %v, want 9", runsB[0].IssueNumber)
	}
}
