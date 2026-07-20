package afk

// Autoland poller + lander-reaping engine tests (issue #181): the same fixture
// as engine_test.go — fakes for tmux/tracker/provider, a REAL store, REAL git
// fixtures. The store does not police the (binding, autoland) pair — that is
// reposvc's API guard — so the fixture's fake-tracker repo can be re-bound
// "forge" to satisfy the poller's forge-only gate.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// autolandOn flips repo to a forge binding with autoland enabled.
func autolandOn(f *fixture, repo store.Repo) {
	f.t.Helper()
	if _, err := f.st.UpdateRepoSettings(f.t.Context(), repo.ID, store.RepoSettingsUpdate{
		TrackerBinding:  store.Set(store.TrackerBindingForge),
		AutolandEnabled: store.Set(true),
	}); err != nil {
		f.t.Fatal(err)
	}
}

// originClaimBranch creates branch at the fixture repo's ORIGIN — the adopt-
// branch launch (AddWorktreeExisting) fetches and requires origin/<branch>,
// exactly what a pushed claim looks like.
func originClaimBranch(f *fixture, repo store.Repo, branch string) {
	f.t.Helper()
	origin := strings.TrimPrefix(repo.RemoteURL, "file://")
	gitCmd(f.t, f.home, origin, "branch", branch, "main")
}

func landerRuns(f *fixture, repo store.Repo) []store.Run {
	f.t.Helper()
	runs, err := f.st.ActiveRunsByRepo(f.t.Context(), repo.ID)
	if err != nil {
		f.t.Fatalf("ActiveRunsByRepo: %v", err)
	}
	out := runs[:0]
	for _, r := range runs {
		if r.Kind == store.RunKindLander {
			out = append(out, r)
		}
	}
	return out
}

// --- the poller ----------------------------------------------------------------

// The headline: a virgin claim PR on an autoland-enabled repo gets its lander
// within one sweep — and a second sweep while it lives spawns nothing (the
// runs-store gate makes the poller idempotent by state, not by memory).
func TestAutolandOnce_spawnsLanderOnVirginClaimPR(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen) // pull #1, no reviews, no comments

	f.svc.AutolandOnce(t.Context())

	runs := landerRuns(f, f.repo)
	if len(runs) != 1 {
		t.Fatalf("lander runs = %d, want exactly 1", len(runs))
	}
	run := runs[0]
	if run.Branch != "afk/7" || run.SessionName != "proj~lander-7" {
		t.Errorf("run identity = %s/%s, want afk/7 / proj~lander-7", run.Branch, run.SessionName)
	}
	if run.IssueNumber == nil || *run.IssueNumber != 7 {
		t.Errorf("issue = %v, want 7 (parsed from the claim head)", run.IssueNumber)
	}
	if run.BudgetDeadline == nil {
		t.Error("lander run has no persisted budget deadline")
	}

	// Idempotent: the live lander occupies the branch, nothing else spawns.
	f.svc.AutolandOnce(t.Context())
	if runs := landerRuns(f, f.repo); len(runs) != 1 {
		t.Errorf("second sweep grew the lander count to %d, want still 1", len(runs))
	}
}

// The poller's suppressions at the wiring level (the predicate rows live in
// TestShouldSpawnLander; these prove the sweep gathers the right facts).
func TestAutolandOnce_suppressions(t *testing.T) {
	t.Run("autoland disabled never reads the tracker", func(t *testing.T) {
		f := newFixture(t)
		// Forge-bound but NOT enabled: the cheap pre-filter must veto before
		// any tracker call.
		if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
			TrackerBinding: store.Set(store.TrackerBindingForge),
		}); err != nil {
			t.Fatal(err)
		}
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.svc.AutolandOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Error("disabled repo spawned a lander")
		}
		if f.trk.pullsCallCount() != 0 {
			t.Error("disabled repo's pulls were listed (the pre-filter must veto first)")
		}
	})
	t.Run("builtin binding vetoes even when enabled", func(t *testing.T) {
		f := newFixture(t)
		// The store does not police the pair; the poller must.
		if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
			AutolandEnabled: store.Set(true),
		}); err != nil {
			t.Fatal(err)
		}
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.svc.AutolandOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Error("builtin-bound repo spawned a lander (the poller cannot read its verdict state)")
		}
	})
	t.Run("three-strikes pause suppresses lander spawns too", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		f.setFailures(f.repo, PauseThreshold)
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.svc.AutolandOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Error("paused repo spawned a lander")
		}
	})
	t.Run("human-branch head never touched", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		f.trk.addPull("fix/typo", tracker.PullOpen)
		f.svc.AutolandOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Error("human-branch PR spawned a lander")
		}
	})
	t.Run("live review suppresses, dismissed-only does not", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		originClaimBranch(f, f.repo, "afk/7")
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.trk.addReview(1, tracker.ReviewCommented, false)
		f.svc.AutolandOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Fatal("PR with a live review spawned a lander")
		}
		// The same review dismissed is a superseded verdict: virgin again.
		f.trk.mu.Lock()
		f.trk.reviews[1][0].Dismissed = true
		f.trk.mu.Unlock()
		f.svc.AutolandOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 1 {
			t.Error("dismissed-only review still suppressed the spawn")
		}
	})
	t.Run("any verdict marker suppresses, fix-done included", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.trk.addPullComment(1, tracker.VerdictFixDone)
		f.svc.AutolandOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Error("PR with verdict state spawned a #181 lander (that PR is the fix-forward loop's, #182)")
		}
	})
	t.Run("live run on the branch suppresses", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		f.trk.setReady(7)
		if _, err := f.svc.StartManualAFK(t.Context(), f.repo.ID); err != nil {
			t.Fatalf("StartManualAFK: %v", err)
		}
		f.trk.addPull("afk/7", tracker.PullOpen) // the authoring run's own PR
		f.svc.AutolandOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Error("lander spawned while the authoring AFK run still idles on the branch")
		}
	})
	t.Run("at cap spawns nothing", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		f.trk.addPull("afk/7", tracker.PullOpen)
		if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
			t.Fatal(err)
		}
		f.runner.AddLive("other~existing")
		f.svc.AutolandOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Error("at-cap sweep spawned a lander")
		}
	})
	t.Run("logged out does not spawn", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.prov.SetLoggedIn(false)
		f.svc.AutolandOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Error("logged-out sweep spawned a lander doomed at the login wall")
		}
	})
	t.Run("comment read failure fails closed", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		originClaimBranch(f, f.repo, "afk/7")
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.trk.failPullComments(errors.New("forge is down"))
		f.svc.AutolandOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Fatal("sweep spawned on unreadable verdict state (must fail closed)")
		}
		// Next tick the forge answers: the same PR spawns.
		f.trk.failPullComments(nil)
		f.svc.AutolandOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 1 {
			t.Error("recovered sweep did not spawn")
		}
	})
}

// --- the reaper on lander runs ---------------------------------------------------

// launchLanderFixture spawns a lander for pull #1 on afk/7 through the real
// engine entry point (the branch created at origin first).
func launchLanderFixture(f *fixture) store.Run {
	f.t.Helper()
	originClaimBranch(f, f.repo, "afk/7")
	if err := f.svc.LaunchLander(f.t.Context(), f.repo.ID, 1, "afk/7", 7); err != nil {
		f.t.Fatalf("LaunchLander: %v", err)
	}
	run, err := f.st.RunBySession(f.t.Context(), "proj~lander-7")
	if err != nil {
		f.t.Fatalf("RunBySession: %v", err)
	}
	return run
}

// The bug the kind-switch exists for: a lander's PR pre-exists the run, so
// the AFK PR-presence rule would success-reap it on its FIRST tick. An open
// verdict-less PR must leave the lander running; the verdict comment (here a
// reject — a reject IS a completed lander) then ends it as success.
func TestReap_landerNotDoneOnPreexistingOpenPR(t *testing.T) {
	f := newFixture(t)
	run := launchLanderFixture(f)
	f.trk.addPull("afk/7", tracker.PullOpen)

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeActive {
		t.Fatalf("outcome = %q after the first tick, want still active (the PR pre-exists the run)", got.Outcome)
	}

	f.trk.addPullComment(1, tracker.VerdictReject+"\n\nfindings: broken tests")
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(2*time.Minute))
	got := f.runRow(run.ID)
	if got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success (a reject verdict completes the lander)", got.Outcome)
	}
	if n := f.failures(f.repo); n != 0 {
		t.Errorf("failures = %d, want 0 (a reject verdict is a completed run, not a strike)", n)
	}
}

// A merged PR is the done-signal all by itself: no comment read is spent.
func TestReap_landerMergedPRIsDoneWithoutCommentRead(t *testing.T) {
	f := newFixture(t)
	run := launchLanderFixture(f)
	f.trk.addPull("afk/7", tracker.PullMerged)

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success off the merged PR", got.Outcome)
	}
	if calls := f.trk.pullCommentsCallCount(); calls != 0 {
		t.Errorf("PullComments called %d times for a merged PR, want 0 (bounded read)", calls)
	}
}

// A fix-done marker is a FIX run's signal (#182) — never a lander's: an open
// PR carrying only fix-done leaves the lander running.
func TestReap_landerFixDoneIsNotItsDoneSignal(t *testing.T) {
	f := newFixture(t)
	run := launchLanderFixture(f)
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.trk.addPullComment(1, tracker.VerdictFixDone)

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeActive {
		t.Errorf("outcome = %q, want still active (fix-done is not a lander done-signal)", got.Outcome)
	}
}

// A PullComments failure skips the run this tick — never classified on
// missing data; a dead lander without a done-signal is then a real death that
// feeds the three-strikes pause (issue #181 AC: landers count toward it).
func TestReap_landerCommentsErrorSkipsThenDeathStrikes(t *testing.T) {
	f := newFixture(t)
	run := launchLanderFixture(f)
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.runner.Kill(run.SessionName)
	f.trk.failPullComments(errors.New("forge is down"))

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeActive {
		t.Fatalf("outcome = %q — classified on an unreadable comment thread", got.Outcome)
	}

	f.trk.failPullComments(nil)
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(2*time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
		t.Fatalf("outcome = %q, want death (session gone, no verdict, PR not merged)", got.Outcome)
	}
	if n := f.failures(f.repo); n != 1 {
		t.Errorf("failures = %d, want 1 (a lander death feeds the three-strikes pause)", n)
	}
}

// The AFK-kind classification must not change by a single behavior: an AFK
// run's done-signal stays bare PR presence — no comment read, ever.
func TestReap_afkKindNeverReadsPullComments(t *testing.T) {
	f := newFixture(t)
	f.trk.setReady(7)
	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.commitInWorktree(run.WorktreePath)
	f.trk.addPull("afk/7", tracker.PullOpen)

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success on bare PR presence", got.Outcome)
	}
	if calls := f.trk.pullCommentsCallCount(); calls != 0 {
		t.Errorf("AFK reap read PullComments %d times, want 0", calls)
	}
}

// A lander's success reap sends NO done-signal push: the pinned copy is
// "<run> opened PR #n" (CONTEXT.md) and a lander opened nothing — its forge-
// observable outcome is its notification surface for now.
func TestReap_landerSuccessDoesNotNotify(t *testing.T) {
	f := newFixture(t)
	cap := &captureNotifier{}
	f.svc.notify = cap.notify
	run := launchLanderFixture(f)
	f.trk.addPull("afk/7", tracker.PullMerged)

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success", got.Outcome)
	}
	if cap.count() != 0 {
		t.Errorf("lander success reap sent %d pushes, want 0", cap.count())
	}
}

// The reaper tick carries the poller (ReaperLoop wiring): a virgin claim PR
// gets its lander within one tick of the loop, with no scheduler involved.
func TestReaperLoop_carriesAutolandSweep(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen)
	if err := f.st.SetSetting(t.Context(), store.SettingAFKTickSeconds, "1"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.svc.ReaperLoop(ctx)
	}()
	// Wait for the run row AND a cleared startguard: the row is written
	// mid-Launch, so cancelling on the row alone races the in-flight spawn
	// (the cancel would roll the launch — and the row — back). The guard
	// clears only once the session is live or the rollback completed (§4b).
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if len(landerRuns(f, f.repo)) == 1 && !f.guard.Has("proj~lander-7") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	if len(landerRuns(f, f.repo)) != 1 {
		t.Fatal("reaper loop never spawned the lander for a virgin claim PR")
	}
}
