package afk

// Autoland gather + verdict-kind reaping engine tests (issues #181/#182; the
// gather feeds the spawn pass, #185): the same fixture as engine_test.go —
// fakes for tmux/tracker/provider, a REAL store, REAL git fixtures. The store
// does not police the (binding, autoland) pair — that is reposvc's API guard
// — so the fixture's fake-tracker repo can be re-bound "forge" to satisfy the
// gather's forge-only gate. The pure decision rows live in TestDecideAutoland
// (decide_test.go); the tests here prove the producer assembles the right
// facts and the launch/reap paths honour them.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// autolandOn flips repo to a forge binding with autoland enabled, at the
// production attempt bound (max_fix_attempts 2, the migration default — the
// fixture's CreateRepo writes the zero value, which would escalate on the
// first rejection and mask the fix stage entirely).
func autolandOn(f *fixture, repo store.Repo) {
	f.t.Helper()
	if _, err := f.st.UpdateRepoSettings(f.t.Context(), repo.ID, store.RepoSettingsUpdate{
		TrackerBinding:  store.Set(store.TrackerBindingForge),
		AutolandEnabled: store.Set(true),
		MaxFixAttempts:  store.Set(2),
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

// runsOfKind lists repo's runs of one kind, ANY outcome — the fix-forward
// tests read terminal rows too (a burned fix attempt is a terminal row that
// still counts against the bound).
func runsOfKind(f *fixture, repo store.Repo, kind string) []store.Run {
	f.t.Helper()
	runs, err := f.st.RunsByRepo(f.t.Context(), repo.ID, 0)
	if err != nil {
		f.t.Fatalf("RunsByRepo: %v", err)
	}
	var out []store.Run
	for _, r := range runs {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// seedTerminalFixRun writes one already-ended fix run row for (repo, branch)
// — a burned attempt: FixRunCountForBranch counts SPAWNS with any outcome,
// so tests seed the bound without driving whole fix rounds.
func seedTerminalFixRun(f *fixture, repo store.Repo, branch, session, outcome string) {
	f.t.Helper()
	id := ids.NewID("run")
	if _, err := f.st.CreateRun(f.t.Context(), store.Run{
		ID: id, RepoID: repo.ID, Kind: store.RunKindFix, Provider: "claude-code",
		Branch: branch, WorktreePath: "/wt/" + session, SessionName: session,
		Model: "m", Effort: "e", StartedAt: clockTime, Outcome: store.RunOutcomeActive,
	}); err != nil {
		f.t.Fatalf("CreateRun: %v", err)
	}
	if err := f.st.EndRun(f.t.Context(), id, outcome, clockTime, ""); err != nil {
		f.t.Fatalf("EndRun: %v", err)
	}
	// The row is the run's history; the ATTEMPT is what the bound counts (a
	// row can be rolled back out of existence, an attempt cannot). A seeded
	// past fix run therefore has to burn its attempt too.
	if err := f.st.RecordAutolandAttempt(f.t.Context(), repo.ID, branch, store.RunKindFix); err != nil {
		f.t.Fatalf("RecordAutolandAttempt: %v", err)
	}
}

// --- the lander gather through the spawn pass ------------------------------------

// The headline: a virgin claim PR on an autoland-enabled repo gets its lander
// within one spawn pass — and a second pass while it lives spawns nothing (the
// runs-store gate makes the gather idempotent by state, not by memory).
func TestSpawnOnce_spawnsLanderOnVirginClaimPR(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen) // pull #1, no reviews, no comments

	f.svc.SpawnOnce(t.Context())

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
	f.svc.SpawnOnce(t.Context())
	if runs := landerRuns(f, f.repo); len(runs) != 1 {
		t.Errorf("second pass grew the lander count to %d, want still 1", len(runs))
	}
}

// The lander suppressions at the wiring level (the decision rows live in
// TestDecideAutoland; these prove the gather assembles the right facts — and,
// for the at-cap row, that the pass enforces what the decision never weighs,
// #185).
func TestSpawnOnce_landerSuppressions(t *testing.T) {
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
		f.svc.SpawnOnce(t.Context())
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
		f.svc.SpawnOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Error("builtin-bound repo spawned a lander (the poller cannot read its verdict state)")
		}
	})
	t.Run("three-strikes pause suppresses lander spawns too", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		f.setFailures(f.repo, PauseThreshold)
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.svc.SpawnOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Error("paused repo spawned a lander")
		}
	})
	t.Run("human-branch head never touched", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		f.trk.addPull("fix/typo", tracker.PullOpen)
		f.svc.SpawnOnce(t.Context())
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
		f.svc.SpawnOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Fatal("PR with a live review spawned a lander")
		}
		// The same review dismissed is a superseded verdict: virgin again.
		f.trk.mu.Lock()
		f.trk.reviews[1][0].Dismissed = true
		f.trk.mu.Unlock()
		f.svc.SpawnOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 1 {
			t.Error("dismissed-only review still suppressed the spawn")
		}
	})
	t.Run("pass verdict with no live rejection spawns nothing", func(t *testing.T) {
		// #181 read ANY marker as "not mine"; #182's decision table refines
		// that: a validated PR (last word pass, no outstanding rejection) is
		// DecideAutoland's nothing — for every kind, not just the lander.
		f := newFixture(t)
		autolandOn(f, f.repo)
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.trk.addPullComment(1, tracker.VerdictPass)
		f.svc.SpawnOnce(t.Context())
		if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
			t.Errorf("validated PR spawned %d runs, want 0", len(active))
		}
	})
	t.Run("merged pull is never a candidate", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		f.trk.addPull("afk/7", tracker.PullMerged)
		f.svc.SpawnOnce(t.Context())
		if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
			t.Errorf("merged PR spawned %d runs, want 0", len(active))
		}
		if calls := f.trk.pullCommentsCallCount(); calls != 0 {
			t.Errorf("merged PR's comments were read %d times, want 0 (the open gate precedes the per-pull reads)", calls)
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
		f.svc.SpawnOnce(t.Context())
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
		f.svc.SpawnOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Error("at-cap pass spawned a lander")
		}
	})
	t.Run("logged out does not spawn", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.prov.SetLoggedIn(false)
		f.svc.SpawnOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Error("logged-out pass spawned a lander doomed at the login wall")
		}
	})
	t.Run("comment read failure fails closed", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		originClaimBranch(f, f.repo, "afk/7")
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.trk.failPullComments(errors.New("forge is down"))
		f.svc.SpawnOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 0 {
			t.Fatal("pass spawned on unreadable verdict state (must fail closed)")
		}
		// Next pass the forge answers: the same PR spawns.
		f.trk.failPullComments(nil)
		f.svc.SpawnOnce(t.Context())
		if len(landerRuns(f, f.repo)) != 1 {
			t.Error("recovered pass did not spawn")
		}
	})
}

// --- the reaper on lander runs ---------------------------------------------------

// launchLanderFixture spawns a lander for pull #1 on afk/7 through the real
// engine entry point (the branch created at origin first).
func launchLanderFixture(f *fixture) store.Run {
	f.t.Helper()
	originClaimBranch(f, f.repo, "afk/7")
	if err := f.svc.LaunchLander(f.t.Context(), f.repo.ID, 1, "afk/7", 7, false); err != nil {
		f.t.Fatalf("LaunchLander: %v", err)
	}
	run, err := f.st.RunBySession(f.t.Context(), "proj~lander-7")
	if err != nil {
		f.t.Fatalf("RunBySession: %v", err)
	}
	return run
}

// launchFixFixture spawns a fix run for pull #1 on afk/7 through the real
// engine entry point, carrying rejection as the seed's work order.
func launchFixFixture(f *fixture, rejection string) store.Run {
	f.t.Helper()
	originClaimBranch(f, f.repo, "afk/7")
	if err := f.svc.LaunchFix(f.t.Context(), f.repo.ID, 1, "afk/7", 7, rejection); err != nil {
		f.t.Fatalf("LaunchFix: %v", err)
	}
	// Production burns the attempt in autolandLaunch, the pass's chokepoint —
	// not inside LaunchFix — so a direct-entry fixture must burn it too, or the
	// bound reads one attempt short of what the poller would see.
	if err := f.st.RecordAutolandAttempt(f.t.Context(), f.repo.ID, "afk/7", store.RunKindFix); err != nil {
		f.t.Fatalf("RecordAutolandAttempt: %v", err)
	}
	run, err := f.st.RunBySession(f.t.Context(), "proj~fix-7")
	if err != nil {
		f.t.Fatalf("RunBySession: %v", err)
	}
	return run
}

// launchEscalateFixture spawns an escalate run for pull #1 on afk/7 through
// the real engine entry point, carrying history below its seed's separator.
func launchEscalateFixture(f *fixture, history string) store.Run {
	f.t.Helper()
	originClaimBranch(f, f.repo, "afk/7")
	if err := f.svc.LaunchEscalate(f.t.Context(), f.repo.ID, 1, "afk/7", 7, history); err != nil {
		f.t.Fatalf("LaunchEscalate: %v", err)
	}
	if err := f.st.RecordAutolandAttempt(f.t.Context(), f.repo.ID, "afk/7", store.RunKindEscalate); err != nil {
		f.t.Fatalf("RecordAutolandAttempt: %v", err)
	}
	run, err := f.st.RunBySession(f.t.Context(), "proj~escalate-7")
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
// missing data; a dead lander without a done-signal is then a real death. That
// death is a terminal failure outcome, but a lander outcome never moves the
// AFK counter (issue #185): the strike stays at zero.
func TestReap_landerCommentsErrorSkipsThenDeath(t *testing.T) {
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
	if n := f.failures(f.repo); n != 0 {
		t.Errorf("failures = %d, want 0 (a lander death never strikes the AFK counter, #185)", n)
	}
}

// Issue #185, the counter separation. A lander death is a terminal FAILURE
// outcome, yet it must move the shared AFK counter in NEITHER direction —
// lander flakiness may never pause a repo's unrelated AFK work. Seeded both
// non-zero (the "not 0==0" proof) and zero.
func TestReap_landerDeathDoesNotStrike(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed int
	}{
		{"seeded non-zero stays put", 1},
		{"zero stays zero", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.setFailures(f.repo, tc.seed)
			run := launchLanderFixture(f)
			f.trk.addPull("afk/7", tracker.PullOpen)
			f.runner.Kill(run.SessionName)

			f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
			if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
				t.Fatalf("outcome = %q, want death (session gone, PR open, no verdict)", got.Outcome)
			}
			if n := f.failures(f.repo); n != tc.seed {
				t.Errorf("failures = %d, want %d unchanged (a lander death never increments the AFK counter, #185)", n, tc.seed)
			}
		})
	}
}

// Issue #185, the backwards half. A lander posting `reject` reaps as SUCCESS,
// yet must NOT reset the counter — the strikes broken AFK runs earned stand,
// because a reject is evidence FOR the pause, not against it. Seeded 2, still 2.
func TestReap_landerRejectDoesNotResetCounter(t *testing.T) {
	f := newFixture(t)
	f.setFailures(f.repo, 2) // strikes from broken AFK runs — the reject must not clear them
	run := launchLanderFixture(f)
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.trk.addPullComment(1, tracker.VerdictReject+"\n\nfindings: broken tests")

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success (a reject verdict completes the lander)", got.Outcome)
	}
	if n := f.failures(f.repo); n != 2 {
		t.Errorf("failures = %d, want 2 unchanged (a lander reject must not re-arm the brake, #185)", n)
	}
}

// Issue #185: the same non-reset rule for a lander that succeeds by MERGE —
// the merged PR is the done-signal, and it too leaves the AFK counter alone.
func TestReap_landerMergeDoesNotResetCounter(t *testing.T) {
	f := newFixture(t)
	f.setFailures(f.repo, 2)
	run := launchLanderFixture(f)
	f.trk.addPull("afk/7", tracker.PullMerged)

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success off the merged PR", got.Outcome)
	}
	if n := f.failures(f.repo); n != 2 {
		t.Errorf("failures = %d, want 2 unchanged (a lander merge-success must not reset the AFK counter, #185)", n)
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

// --- the fix-forward producer (issue #182) ---------------------------------------

// The headline: a reject marker on a lander-engaged PR (no live run) yields a
// fix run within one spawn pass — carrying the rejection as its work order
// and inheriting provider/model/effort from the PERSISTED authoring run row,
// not from a fresh resolution.
func TestSpawnOnce_rejectSpawnsFixRunInheritingAuthoringRow(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	// Distinct AFK defaults so the authoring run ROW records values the base
	// chain would not re-derive.
	sonnet, high := "sonnet", "high"
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		AFKModelDefault: store.Set(&sonnet), AFKEffortDefault: store.Set(&high),
	}); err != nil {
		t.Fatal(err)
	}
	f.trk.setReady(7)
	authoring, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	if authoring.Model != "sonnet" || authoring.Effort != "high" {
		t.Fatalf("authoring run model/effort = %s/%s, want sonnet/high (the AFK defaults)", authoring.Model, authoring.Effort)
	}
	f.commitInWorktree(authoring.WorktreePath)
	f.trk.addPull("afk/7", tracker.PullOpen) // pull #1 — the authoring run's done-signal
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(authoring.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("authoring outcome = %q, want success before the fix round", got.Outcome)
	}
	// Clear the AFK defaults: a fresh empty-request resolution would now pick
	// the catalog heads, so matching values below prove ROW inheritance.
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		AFKModelDefault: store.Set[*string](nil), AFKEffortDefault: store.Set[*string](nil),
	}); err != nil {
		t.Fatal(err)
	}
	// The lander round rejected; the PR head exists at origin (the detached
	// adopt fetches it — the shape a pushed claim has).
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPullComment(1, tracker.VerdictReject+"\n\nfindings: broken tests")

	f.svc.SpawnOnce(t.Context())

	fixes := runsOfKind(f, f.repo, store.RunKindFix)
	if len(fixes) != 1 {
		t.Fatalf("fix runs = %d, want exactly 1 within one pass", len(fixes))
	}
	run := fixes[0]
	if run.Branch != "afk/7" || run.SessionName != "proj~fix-7" {
		t.Errorf("run identity = %s/%s, want afk/7 / proj~fix-7", run.Branch, run.SessionName)
	}
	if run.IssueNumber == nil || *run.IssueNumber != 7 {
		t.Errorf("issue = %v, want 7", run.IssueNumber)
	}
	if run.BudgetDeadline == nil {
		t.Error("fix run has no persisted budget deadline")
	}
	// The #182 inheritance pin: provider/model/effort equal the authoring
	// row's, surviving the cleared defaults.
	if run.Provider != authoring.Provider || run.Model != "sonnet" || run.Effort != "high" {
		t.Errorf("fix run provider/model/effort = %s/%s/%s, want the authoring row's %s/sonnet/high",
			run.Provider, run.Model, run.Effort, authoring.Provider)
	}
	// The seed carries the rejection work order below the separator — exactly
	// FixSeedPrompt over RejectionContext's rendering.
	sess, live := f.runner.Session(run.SessionName)
	if !live {
		t.Fatal("fix session not live")
	}
	want := FixSeedPrompt(7, 1, "afk/7", false, "Lander rejection:\nfindings: broken tests")
	if last := sess.Argv[len(sess.Argv)-1]; last != want {
		t.Errorf("seed argv = %q, want the exact FixSeedPrompt with the rejection work order", last)
	}

	// Idempotent by state: the live fix run occupies the branch.
	f.svc.SpawnOnce(t.Context())
	if n := len(runsOfKind(f, f.repo, store.RunKindFix)); n != 1 {
		t.Errorf("second pass grew the fix count to %d, want still 1", n)
	}
}

// The hybrid trigger (ADR-0048): a live human changes-requested native
// review, with NO markers at all, is rejected-state — a fix run spawns, its
// work order quoting the review. No authoring run row exists here, so the
// provider falls back through the repo chain (warned, never fatal — the loop
// stays alive).
func TestSpawnOnce_humanRejectionAloneSpawnsFixRun(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.trk.addReviewFrom(1, "human", tracker.ReviewChangesRequested, false, "please split the migration")

	f.svc.SpawnOnce(t.Context())

	fixes := runsOfKind(f, f.repo, store.RunKindFix)
	if len(fixes) != 1 {
		t.Fatalf("fix runs = %d, want exactly 1 (a native changes-requested is rejected-state)", len(fixes))
	}
	if fixes[0].Provider != "claude-code" {
		t.Errorf("provider = %q, want the repo-chain fallback claude-code (no authoring row exists)", fixes[0].Provider)
	}
	sess, live := f.runner.Session(fixes[0].SessionName)
	if !live {
		t.Fatal("fix session not live")
	}
	want := FixSeedPrompt(7, 1, "afk/7", false, "Review by human (changes requested):\nplease split the migration")
	if last := sess.Argv[len(sess.Argv)-1]; last != want {
		t.Errorf("seed argv = %q, want the human rejection as the work order", last)
	}
}

// At the attempt bound (FixSpawns >= MaxFixAttempts) rejected-state spawns
// the terminal escalate run instead of a third fix run — gated on the LANDER
// provider chain, strict on a named lander_provider.
func TestSpawnOnce_atFixBoundSpawnsEscalate(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo) // max_fix_attempts 2
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.trk.addPullComment(1, tracker.VerdictReject+"\n\nround 1: broken")
	f.trk.addPullComment(1, tracker.VerdictReject+"\n\nround 2: still broken")
	// Two attempts already burned — SPAWNS with any outcome count, so a dead
	// fix run burns one exactly like a rejected one.
	seedTerminalFixRun(f, f.repo, "afk/7", "proj~fix-7-r1", store.RunOutcomeDeath)
	seedTerminalFixRun(f, f.repo, "afk/7", "proj~fix-7-r2", store.RunOutcomeSuccess)

	// A bogus lander_provider vetoes the escalate candidate: the gate is the
	// lander chain, strict on the operator-named id.
	bogus := "no-such-provider"
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		LanderProvider: store.Set(&bogus),
	}); err != nil {
		t.Fatal(err)
	}
	f.svc.SpawnOnce(t.Context())
	if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
		t.Fatalf("bogus lander provider still spawned %d runs", len(active))
	}

	// Cleared, the escalate run spawns — with the full round history below
	// its seed's separator.
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		LanderProvider: store.Set[*string](nil),
	}); err != nil {
		t.Fatal(err)
	}
	f.svc.SpawnOnce(t.Context())
	escs := runsOfKind(f, f.repo, store.RunKindEscalate)
	if len(escs) != 1 {
		t.Fatalf("escalate runs = %d, want exactly 1 at the bound", len(escs))
	}
	run := escs[0]
	if run.Branch != "afk/7" || run.SessionName != "proj~escalate-7" {
		t.Errorf("run identity = %s/%s, want afk/7 / proj~escalate-7", run.Branch, run.SessionName)
	}
	if n := len(runsOfKind(f, f.repo, store.RunKindFix)); n != 2 {
		t.Errorf("fix runs = %d, want the 2 burned attempts only — never a third at the bound", n)
	}
	sess, live := f.runner.Session(run.SessionName)
	if !live {
		t.Fatal("escalate session not live")
	}
	history := "Verdict comments (oldest first):\n\n" +
		tracker.VerdictReject + "\n\nround 1: broken" + "\n\n" +
		tracker.VerdictReject + "\n\nround 2: still broken"
	if last := sess.Argv[len(sess.Argv)-1]; last != EscalateSeedPrompt(7, 1, "afk/7", history) {
		t.Errorf("seed argv = %q, want the exact EscalateSeedPrompt with the round history", last)
	}
}

// Escalated terminality: after escalation the PR is invisible to autoland
// FOREVER — by the durable run row even if the marker comment was deleted,
// and by the marker at ANY comment position even without a row.
// An escalate run that dies BEFORE posting its marker writes no 'escalated'
// row, so the poller re-derives the identical rejected-at-bound state and
// escalates again. Nothing else brakes that: escalate is excluded from the
// three-strikes counter, and the candidate outranks new work at StageFix. The
// escalate arm is therefore bounded in its own right — MaxEscalateAttempts
// spawns, then autoland goes quiet on the PR instead of spinning forever.
func TestSpawnOnce_escalateArmIsBounded(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		MaxFixAttempts: store.Set(0), // straight to the escalate arm
	}); err != nil {
		t.Fatal(err)
	}
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.trk.addPullComment(1, tracker.VerdictReject+"\n\nfindings")

	// Each round the escalate run fails to deliver — killed before it can post
	// its marker, or unable to launch at all once round 1's worktree survives
	// the un-merged teardown. Either way no 'escalated' row is ever written, so
	// the PR keeps re-deriving rejected-at-bound. The attempt counter is what
	// the bound reads, so it is what this asserts on: a surviving runs row is
	// exactly the thing that cannot be relied upon here.
	for round := 1; round <= MaxEscalateAttempts; round++ {
		f.svc.SpawnOnce(t.Context())
		n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, "afk/7", store.RunKindEscalate)
		if err != nil {
			t.Fatalf("round %d: AutolandAttempts: %v", round, err)
		}
		if n != round {
			t.Fatalf("round %d: escalate attempts = %d, want %d", round, n, round)
		}
		for _, run := range activeRuns(f) {
			f.runner.Kill(run.SessionName)
		}
		f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Duration(round)*time.Minute))
	}
	if esc, err := f.st.EscalatedRunOnBranch(t.Context(), f.repo.ID, "afk/7"); err != nil || esc {
		t.Fatalf("EscalatedRunOnBranch = %v (err %v), want false — no marker ever landed", esc, err)
	}

	// The bound is spent: the PR still reads rejected-at-bound, but the poller
	// must go quiet rather than spawn escalate attempt MaxEscalateAttempts+1.
	f.svc.SpawnOnce(t.Context())
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, "afk/7", store.RunKindEscalate); err != nil || n != MaxEscalateAttempts {
		t.Errorf("escalate attempts = %d (err %v), want %d — the arm must not respawn past its bound", n, err, MaxEscalateAttempts)
	}
	if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
		t.Errorf("active runs = %d, want 0 — autoland is silent on the PR past the bound", len(active))
	}
}

// activeRuns is the fixture's live-run snapshot for the repo under test.
func activeRuns(f *fixture) []store.Run {
	f.t.Helper()
	runs, err := f.st.ActiveRunsByRepo(f.t.Context(), f.repo.ID)
	if err != nil {
		f.t.Fatalf("ActiveRunsByRepo: %v", err)
	}
	return runs
}

// A fix launch that fails on the launch pad — before any runs row survives —
// must still burn an attempt. Start's rollback deletes the row on every
// post-CreateRun failure and the pre-CreateRun failures never write one, so a
// bound counting runs rows would let the same failing launch retry every tick
// forever. Here the claim branch is missing from origin, so LaunchFix fails
// with no row left behind.
func TestSpawnOnce_failedFixLaunchBurnsAttempt(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		MaxFixAttempts: store.Set(1),
	}); err != nil {
		t.Fatal(err)
	}
	// Deliberately NO originClaimBranch: the launch cannot make a worktree.
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.trk.addPullComment(1, tracker.VerdictReject+"\n\nfindings")

	f.svc.SpawnOnce(t.Context())
	if n := len(runsOfKind(f, f.repo, store.RunKindFix)); n != 0 {
		t.Fatalf("fix run rows = %d, want 0 — the launch failed, so no row survives", n)
	}
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, "afk/7", store.RunKindFix); err != nil || n != 1 {
		t.Fatalf("AutolandAttempts(fix) = %d (err %v), want 1 — the ATTEMPT burns it, not the surviving row", n, err)
	}

	// The bound is 1 and it is spent: the next pass must escalate, never retry
	// the same failing fix launch.
	f.svc.SpawnOnce(t.Context())
	if n := len(runsOfKind(f, f.repo, store.RunKindFix)); n != 0 {
		t.Errorf("fix run rows = %d, want still 0 — the bound is spent, no respawn", n)
	}
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, "afk/7", store.RunKindEscalate); err != nil || n != 1 {
		t.Errorf("AutolandAttempts(escalate) = %d (err %v), want 1 — the loop moved on to the hand-off", n, err)
	}
}

func TestSpawnOnce_escalatedIsTerminal(t *testing.T) {
	t.Run("escalated-outcome run row blocks forever", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		originClaimBranch(f, f.repo, "afk/7")
		f.trk.addPull("afk/7", tracker.PullOpen)
		// Rejected-state on the thread (the marker itself deleted by a
		// human): without the row this would spawn a fix run.
		f.trk.addPullComment(1, tracker.VerdictReject+"\n\nfindings")
		id := ids.NewID("run")
		if _, err := f.st.CreateRun(t.Context(), store.Run{
			ID: id, RepoID: f.repo.ID, Kind: store.RunKindEscalate, Provider: "claude-code",
			Branch: "afk/7", WorktreePath: "/wt/proj-escalate-7", SessionName: "proj~escalate-7-old",
			Model: "m", Effort: "e", StartedAt: clockTime, Outcome: store.RunOutcomeActive,
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.st.EndRun(t.Context(), id, store.RunOutcomeEscalated, clockTime, ""); err != nil {
			t.Fatal(err)
		}

		f.svc.SpawnOnce(t.Context())
		if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
			t.Errorf("escalated PR spawned %d runs, want 0 forever (the row is the durable gate)", len(active))
		}
	})
	t.Run("escalate marker blocks at any position", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		originClaimBranch(f, f.repo, "afk/7")
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.trk.addPullComment(1, tracker.VerdictEscalate+"\n\ndigest")
		// A later rejection does not resurrect the loop: escalate wins from
		// ANY position, not only as the last word.
		f.trk.addPullComment(1, tracker.VerdictReject+"\n\na human re-rejected")

		f.svc.SpawnOnce(t.Context())
		if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
			t.Errorf("escalate-marked PR spawned %d runs, want 0 (marker alone is terminal)", len(active))
		}
	})
}

// fix-done as the last word spawns the re-validation lander (DecideAutoland
// case 4) — with the repo's own AutoMerge honoured when no human rejection is
// outstanding, and the approve-only seed forced when one is (never merge over
// a live human changes-requested; the round still runs to move the marker
// state, and a fix run here would instant-reap on its own done-signal).
func TestSpawnOnce_fixDoneSpawnsRevalidationLander(t *testing.T) {
	t.Run("plain re-validation honours AutoMerge", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
			AutoMerge: store.Set(true),
		}); err != nil {
			t.Fatal(err)
		}
		originClaimBranch(f, f.repo, "afk/7")
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.trk.addPullComment(1, tracker.VerdictReject+"\n\nround 1")
		f.trk.addPullComment(1, tracker.VerdictFixDone)

		f.svc.SpawnOnce(t.Context())
		landers := landerRuns(f, f.repo)
		if len(landers) != 1 {
			t.Fatalf("lander runs = %d, want exactly 1 (fix-done is the re-validation trigger)", len(landers))
		}
		sess, _ := f.runner.Session("proj~lander-7")
		if last := sess.Argv[len(sess.Argv)-1]; last != LanderSeedPrompt(1, "afk/7", true, false) {
			t.Errorf("seed argv = %q, want the AutoMerge variant (no human rejection outstanding)", last)
		}
	})
	t.Run("outstanding human rejection forces the approve-only seed", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
			AutoMerge: store.Set(true),
		}); err != nil {
			t.Fatal(err)
		}
		originClaimBranch(f, f.repo, "afk/7")
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.trk.addPullComment(1, tracker.VerdictReject+"\n\nround 1")
		f.trk.addPullComment(1, tracker.VerdictFixDone)
		f.trk.addReviewFrom(1, "human", tracker.ReviewChangesRequested, false, "still wrong")

		f.svc.SpawnOnce(t.Context())
		landers := landerRuns(f, f.repo)
		if len(landers) != 1 {
			t.Fatalf("lander runs = %d, want exactly 1 (the round must run — never a fix run at fix-done)", len(landers))
		}
		if n := len(runsOfKind(f, f.repo, store.RunKindFix)); n != 0 {
			t.Fatalf("fix runs = %d, want 0 (the instant-reap trap: fix-done is already a fix done-signal)", n)
		}
		sess, _ := f.runner.Session("proj~lander-7")
		if last := sess.Argv[len(sess.Argv)-1]; last != LanderSeedPrompt(1, "afk/7", false, false) {
			t.Errorf("seed argv = %q, want the approve-only variant despite AutoMerge=true", last)
		}
	})
}

// --- the reaper on fix and escalate runs -----------------------------------------

// A fix run ends on its fix-done marker as the LAST word — the stale reject
// it spawned from does not end it — and its success RESETS the failure
// counter (fix is AFK-class unattended work, #182) without sending any push
// (the marker plus the native reviewer ping is its surface).
func TestReap_fixRunEndsOnFixDoneAndResetsCounter(t *testing.T) {
	f := newFixture(t)
	notes := &captureNotifier{}
	f.svc.notify = notes.notify
	f.setFailures(f.repo, 2)
	run := launchFixFixture(f, "findings: broken tests")
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.trk.addPullComment(1, tracker.VerdictReject+"\n\nfindings: broken tests")

	// Last word reject: the run's own signal has not landed — still active.
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeActive {
		t.Fatalf("outcome = %q on the stale reject, want still active", got.Outcome)
	}

	// `labctl pr rerequest` landed: fix-done becomes the last word.
	f.trk.addPullComment(1, tracker.VerdictFixDone)
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(2*time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success off the fix-done marker", got.Outcome)
	}
	if n := f.failures(f.repo); n != 0 {
		t.Errorf("failures = %d, want 0 (a fix success resets the counter, #182)", n)
	}
	if notes.count() != 0 {
		t.Errorf("fix success sent %d pushes, want 0 (its marker + native ping is the surface)", notes.count())
	}
}

// A dead fix session with no marker is a death: the attempt is burned (the
// spawn already counted — FixRunCountForBranch), the failure counter strikes,
// and the NEXT pass at the bound escalates instead of respawning.
func TestReap_fixDeathBurnsAttemptStrikesAndEscalatesNext(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		MaxFixAttempts: store.Set(1),
	}); err != nil {
		t.Fatal(err)
	}
	run := launchFixFixture(f, "findings")
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.trk.addPullComment(1, tracker.VerdictReject+"\n\nround 1")
	f.runner.Kill(run.SessionName)

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
		t.Fatalf("outcome = %q, want death (session gone, no fix-done, PR not merged)", got.Outcome)
	}
	if n := f.failures(f.repo); n != 1 {
		t.Errorf("failures = %d, want 1 (a fix death strikes — three-strikes accounting applies, #182)", n)
	}
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, "afk/7", store.RunKindFix); err != nil || n != 1 {
		t.Errorf("AutolandAttempts(fix) = %d (err %v), want 1 — the dead spawn burned the attempt", n, err)
	}

	// The next pass: still rejected, FixSpawns 1 >= bound 1 → escalate, never
	// a second fix run.
	f.svc.SpawnOnce(t.Context())
	if n := len(runsOfKind(f, f.repo, store.RunKindEscalate)); n != 1 {
		t.Fatalf("escalate runs after the burned attempt = %d, want 1", n)
	}
	if n := len(runsOfKind(f, f.repo, store.RunKindFix)); n != 1 {
		t.Errorf("fix runs = %d, want still 1 (the bound is spent — no respawn)", n)
	}
}

// An escalate run whose marker lands ends outcome 'escalated' — not success —
// fires the escalation push exactly once off the idempotent claim, tears down
// like any reap, leaves the failure counter alone in BOTH directions, and its
// terminal row then blocks the poller forever.
func TestReap_escalateMarkerEndsEscalatedAndNotifiesOnce(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo) // forge binding: the push noun is "PR"
	notes := &captureNotifier{}
	f.svc.notify = notes.notify
	f.setFailures(f.repo, 2)
	// The parked-claim shape: the origin branch one commit past main AND its
	// local claim ref in the bare, so the guarded teardown's merged check has
	// a branch to judge (clean worktree removed, unmerged branch kept).
	origin := strings.TrimPrefix(f.repo.RemoteURL, "file://")
	gitCmd(t, f.home, origin, "branch", "afk/7", "main")
	gitCmd(t, f.home, origin, "checkout", "-q", "afk/7")
	gitCmd(t, f.home, origin, "commit", "-q", "--allow-empty", "-m", "claim work")
	gitCmd(t, f.home, origin, "checkout", "-q", "main")
	gitCmd(t, f.home, f.bare(f.repo), "fetch", "-q", "origin")
	gitCmd(t, f.home, f.bare(f.repo), "branch", "afk/7", "origin/afk/7")
	if err := f.svc.LaunchEscalate(t.Context(), f.repo.ID, 1, "afk/7", 7, "history"); err != nil {
		t.Fatalf("LaunchEscalate: %v", err)
	}
	run, err := f.st.RunBySession(t.Context(), "proj~escalate-7")
	if err != nil {
		t.Fatalf("RunBySession: %v", err)
	}
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.trk.setPullTitle(1, "Resolve issue 7")
	f.trk.addPullComment(1, tracker.VerdictReject+"\n\nround 1")

	// Last word reject: the hand-off has not landed — still active, no push.
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeActive {
		t.Fatalf("outcome = %q before the marker, want still active", got.Outcome)
	}
	if notes.count() != 0 {
		t.Fatalf("push fired before the escalate marker landed")
	}

	// `labctl pr escalate` landed LAST: the terminal marker.
	f.trk.addPullComment(1, tracker.VerdictEscalate+"\n\ndigest")
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(2*time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeEscalated {
		t.Fatalf("outcome = %q, want escalated (the marker, not success)", got.Outcome)
	}
	if _, live := f.runner.Session(run.SessionName); live {
		t.Error("escalate session still live after the reap")
	}
	if dirExists(run.WorktreePath) {
		t.Error("clean escalate worktree not removed (the guarded teardown applies as usual)")
	}
	if !f.branchExists(f.repo, "afk/7") {
		t.Error("unmerged claim branch deleted on the escalated reap (the guarded teardown keeps it)")
	}
	if n := f.failures(f.repo); n != 2 {
		t.Errorf("failures = %d, want 2 unchanged (an escalate outcome moves the counter in neither direction)", n)
	}
	if notes.count() != 1 {
		t.Fatalf("escalation pushes = %d, want exactly 1", notes.count())
	}
	want := Notification{
		Title: "proj~escalate-7 escalated PR #1",
		Body:  "Resolve issue 7",
		Tag:   run.ID,
		Route: "/runs/" + run.ID,
	}
	if got := notes.last(); got != want {
		t.Errorf("notification = %+v; want %+v", got, want)
	}

	// Idempotent: a second sweep neither reclassifies nor re-sends.
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(3*time.Minute))
	if notes.count() != 1 {
		t.Errorf("second sweep re-sent the escalation push: count = %d, want 1", notes.count())
	}
	// And the escalated row is the poller's permanent gate.
	f.svc.SpawnOnce(t.Context())
	if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
		t.Errorf("escalated PR spawned %d runs after the terminal reap, want 0", len(active))
	}
}

// A PR merged mid-escalate is moot: plain success (merged-first mirrors
// LanderDone), no escalation push, and no comment read spent on a merged PR.
func TestReap_escalateMergedMidRunIsPlainSuccess(t *testing.T) {
	f := newFixture(t)
	notes := &captureNotifier{}
	f.svc.notify = notes.notify
	run := launchEscalateFixture(f, "history")
	f.trk.addPull("afk/7", tracker.PullMerged)

	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want plain success off the merged PR", got.Outcome)
	}
	if notes.count() != 0 {
		t.Errorf("merged-mid-escalate sent %d pushes, want 0", notes.count())
	}
	if calls := f.trk.pullCommentsCallCount(); calls != 0 {
		t.Errorf("PullComments called %d times for a merged PR, want 0 (bounded read)", calls)
	}
}

// The reaper tick carries the spawn pass (ReaperLoop wiring, #185): a virgin
// claim PR gets its lander within one tick of the loop, with no scheduler
// involved.
func TestReaperLoop_carriesSpawnPass(t *testing.T) {
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
