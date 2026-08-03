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

// seedTerminalFixRun writes one already-ended fix run row for (repo, pull,
// branch) — a burned attempt: the bound counts SPAWNS with any outcome, so
// tests seed it without driving whole fix rounds. The row carries the pull
// number a real fix launch stamps on it (issue #188).
func seedTerminalFixRun(f *fixture, repo store.Repo, pullNumber int, branch, session, outcome string) {
	f.t.Helper()
	id := ids.NewID("run")
	if _, err := f.st.CreateRun(f.t.Context(), store.Run{
		ID: id, RepoID: repo.ID, Kind: store.RunKindFix, Provider: "claude-code",
		Branch: branch, PullNumber: &pullNumber, WorktreePath: "/wt/" + session, SessionName: session,
		Model: "m", Effort: "e", StartedAt: clockTime, Outcome: store.RunOutcomeActive,
	}); err != nil {
		f.t.Fatalf("CreateRun: %v", err)
	}
	if err := f.st.EndRun(f.t.Context(), id, outcome, clockTime, ""); err != nil {
		f.t.Fatalf("EndRun: %v", err)
	}
	// The row is the run's history; the ATTEMPT is what the bound counts (a
	// row can be rolled back out of existence, an attempt cannot). A seeded
	// past fix run therefore has to burn its attempt too — against the PULL,
	// which is what the counter is keyed on since issue #188.
	if err := f.st.RecordAutolandAttempt(f.t.Context(), repo.ID, pullNumber, store.RunKindFix); err != nil {
		f.t.Fatalf("RecordAutolandAttempt: %v", err)
	}
}

// seedEscalatedRun writes one already-ended outcome='escalated' run row for
// (repo, pull, branch) — the DURABLE half of terminality, exactly the row an
// escalate run leaves behind once its marker lands (issue #182), stamped with
// the pull number the gate now keys on (issue #188). endedAt is the moment
// terminality was recorded: the instant EscalatedRunForPull hands back and the
// re-arm comparison is made against.
func seedEscalatedRun(f *fixture, repo store.Repo, pullNumber int, branch, session string, endedAt time.Time) {
	f.t.Helper()
	id := ids.NewID("run")
	if _, err := f.st.CreateRun(f.t.Context(), store.Run{
		ID: id, RepoID: repo.ID, Kind: store.RunKindEscalate, Provider: "claude-code",
		Branch: branch, PullNumber: &pullNumber, WorktreePath: "/wt/" + session, SessionName: session,
		Model: "m", Effort: "e", StartedAt: clockTime, Outcome: store.RunOutcomeActive,
	}); err != nil {
		f.t.Fatalf("CreateRun: %v", err)
	}
	if err := f.st.EndRun(f.t.Context(), id, store.RunOutcomeEscalated, endedAt, ""); err != nil {
		f.t.Fatalf("EndRun: %v", err)
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
	if err := f.st.RecordAutolandAttempt(f.t.Context(), f.repo.ID, 1, store.RunKindFix); err != nil {
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
	if err := f.st.RecordAutolandAttempt(f.t.Context(), f.repo.ID, 1, store.RunKindEscalate); err != nil {
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
// fix run within one spawn pass — carrying the rejection as its work order and
// resolving provider/model/effort through the NORMAL AFK chain (issue #189
// reversed the ADR-0048 authoring-row inheritance). The authoring run row is
// present recording a DISTINCT value, and the repo's AFK overrides point
// somewhere else again: the fix run carries the AFK-chain values, proving the
// persisted authoring row has zero influence.
func TestSpawnOnce_rejectSpawnsFixRunOnAFKChain(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	// The authoring run records sonnet/high (the AFK defaults at ITS launch) —
	// a distinctive value neither the base heads nor the later override match.
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
		t.Fatalf("authoring run model/effort = %s/%s, want sonnet/high (the AFK defaults at its launch)", authoring.Model, authoring.Effort)
	}
	f.commitInWorktree(authoring.WorktreePath)
	f.trk.addPull("afk/7", tracker.PullOpen) // pull #1 — the authoring run's done-signal
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
	if got := f.runRow(authoring.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("authoring outcome = %q, want success before the fix round", got.Outcome)
	}
	// Repoint the AFK overrides to fable/xhigh — DIFFERENT from both the
	// authoring row (sonnet/high) AND the base heads (opus[1m]/max). This is
	// the AFK-chain value the fix run must resolve to; the authoring row's
	// recorded sonnet/high must NOT leak in.
	fable, xhigh := "fable", "xhigh"
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		AFKModelDefault: store.Set(&fable), AFKEffortDefault: store.Set(&xhigh),
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
	// The #189 pin: model/effort are the AFK-chain override (fable/xhigh), NOT
	// the authoring row's sonnet/high — the persisted authoring row has zero
	// influence. Provider stays claude-code: the fixture registers no second
	// provider, so the AFK chain and the authoring row happen to agree there.
	if run.Provider != "claude-code" || run.Model != "fable" || run.Effort != "xhigh" {
		t.Errorf("fix run provider/model/effort = %s/%s/%s, want the AFK-chain claude-code/fable/xhigh (authoring row recorded sonnet/high)",
			run.Provider, run.Model, run.Effort)
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
// work order quoting the review. Its provider resolves through the normal AFK
// chain (issue #189) — the base default claude-code here — exactly as every
// fix run now resolves, authoring row or not: there is no longer a "missing
// row" branch, only the one chain.
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
		t.Errorf("provider = %q, want the AFK-chain base default claude-code", fixes[0].Provider)
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
	seedTerminalFixRun(f, f.repo, 1, "afk/7", "proj~fix-7-r1", store.RunOutcomeDeath)
	seedTerminalFixRun(f, f.repo, 1, "afk/7", "proj~fix-7-r2", store.RunOutcomeSuccess)

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
		n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, 1, store.RunKindEscalate)
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
	if _, esc, err := f.st.EscalatedRunForPull(t.Context(), f.repo.ID, 1); err != nil || esc {
		t.Fatalf("EscalatedRunForPull ok = %v (err %v), want false — no marker ever landed", esc, err)
	}

	// The bound is spent: the PR still reads rejected-at-bound, but the poller
	// must go quiet rather than spawn escalate attempt MaxEscalateAttempts+1.
	f.svc.SpawnOnce(t.Context())
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, 1, store.RunKindEscalate); err != nil || n != MaxEscalateAttempts {
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
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, 1, store.RunKindFix); err != nil || n != 1 {
		t.Fatalf("AutolandAttempts(fix) = %d (err %v), want 1 — the ATTEMPT burns it, not the surviving row", n, err)
	}

	// The bound is 1 and it is spent: the next pass must escalate, never retry
	// the same failing fix launch.
	f.svc.SpawnOnce(t.Context())
	if n := len(runsOfKind(f, f.repo, store.RunKindFix)); n != 0 {
		t.Errorf("fix run rows = %d, want still 0 — the bound is spent, no respawn", n)
	}
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, 1, store.RunKindEscalate); err != nil || n != 1 {
		t.Errorf("AutolandAttempts(escalate) = %d (err %v), want 1 — the loop moved on to the hand-off", n, err)
	}
}

// Escalated terminality, both sources: an escalated PR is invisible to
// autoland by the durable run row even when a human deleted the marker
// comment, and by the marker at ANY comment position even without a row.
// Terminal until superseded (issue #188) — the re-arm cases are
// TestSpawnOnce_rearmLiftsTerminality; here nothing has re-armed, so the
// suppression stands pass after pass.
func TestSpawnOnce_escalatedIsTerminal(t *testing.T) {
	t.Run("escalated-outcome run row blocks until re-armed", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		originClaimBranch(f, f.repo, "afk/7")
		f.trk.addPull("afk/7", tracker.PullOpen)
		// Rejected-state on the thread (the marker itself deleted by a
		// human): without the row this would spawn a fix run.
		f.trk.addPullComment(1, tracker.VerdictReject+"\n\nfindings")
		seedEscalatedRun(f, f.repo, 1, "afk/7", "proj~escalate-7-old", clockTime)

		f.svc.SpawnOnce(t.Context())
		if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
			t.Errorf("escalated PR spawned %d runs, want 0 (the row is the durable gate)", len(active))
		}
	})
	t.Run("escalate marker blocks at any position", func(t *testing.T) {
		f := newFixture(t)
		autolandOn(f, f.repo)
		originClaimBranch(f, f.repo, "afk/7")
		f.trk.addPull("afk/7", tracker.PullOpen)
		f.trk.addPullComment(1, tracker.VerdictEscalate+"\n\ndigest")
		// A later rejection does not resurrect the loop: an un-superseded
		// escalate marker wins from ANY position, not only as the last word.
		f.trk.addPullComment(1, tracker.VerdictReject+"\n\na human re-rejected")

		f.svc.SpawnOnce(t.Context())
		if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
			t.Errorf("escalate-marked PR spawned %d runs, want 0 (marker alone is terminal)", len(active))
		}
	})
}

// --- terminality is PR-scoped and supersedable (issue #188) -----------------------

// The case that motivated the whole issue. afk/<N> claim branches derive from
// the ISSUE number, so discarding an escalated run and letting a fresh AFK run
// re-claim issue 7 REUSES afk/7 — and under the old branch-keyed gate the
// brand-new PR was invisible from birth and born owing the discarded PR's
// spent budget, without ever having been validated once. No re-arm is involved
// or needed: PR #101 has simply never escalated.
func TestSpawnOnce_requeuedPRIsVirginDespiteEscalatedPredecessor(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo) // max_fix_attempts 2
	originClaimBranch(f, f.repo, "afk/7")
	// The discarded predecessor: PR #100 on afk/7, escalated, both fix
	// attempts spent. Its history stays exactly as it was — supersession is
	// never erasure — it simply does not speak for a different PR.
	seedEscalatedRun(f, f.repo, 100, "afk/7", "proj~escalate-7-old", clockTime)
	seedTerminalFixRun(f, f.repo, 100, "afk/7", "proj~fix-7-r1", store.RunOutcomeSuccess)
	seedTerminalFixRun(f, f.repo, 100, "afk/7", "proj~fix-7-r2", store.RunOutcomeDeath)
	// The requeue: a brand-new, verdict-less PR on the SAME branch.
	f.trk.addPullNumbered(101, "afk/7", tracker.PullOpen)

	f.svc.SpawnOnce(t.Context())

	landers := landerRuns(f, f.repo)
	if len(landers) != 1 {
		t.Fatalf("lander runs = %d, want exactly 1 — a virgin PR gets its round-1 lander whatever the branch's history", len(landers))
	}
	if landers[0].PullNumber == nil || *landers[0].PullNumber != 101 {
		t.Errorf("lander pull_number = %v, want 101 (the gate needs the PR stamped on the row)", landers[0].PullNumber)
	}
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, 101, store.RunKindFix); err != nil || n != 0 {
		t.Errorf("fix attempts on the new PR = %d (err %v), want 0 — a requeued PR is born with a full budget", n, err)
	}
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, 100, store.RunKindFix); err != nil || n != 2 {
		t.Errorf("fix attempts on the discarded PR = %d (err %v), want the 2 it really spent — history is never rewritten", n, err)
	}
}

// Re-arm lifts terminality for each source independently and for both at once:
// the run row alone (the human deleted the marker comment), the marker comment
// alone (an escalate run that posted its marker but whose row was never
// written, or a pre-migration row), and both together. In every shape the PR is
// suppressed before the gesture and a fix candidate after it — the escalation
// signal is not erased, it is simply older than the re-arm.
func TestSpawnOnce_rearmLiftsTerminality(t *testing.T) {
	rearmAt := clockTime.Add(time.Minute)
	for _, tc := range []struct {
		name  string
		setup func(f *fixture)
	}{
		{"run row only, marker deleted from the thread", func(f *fixture) {
			seedEscalatedRun(f, f.repo, 1, "afk/7", "proj~escalate-7-old", clockTime)
			f.trk.addPullComment(1, tracker.VerdictReject+"\n\nfindings")
		}},
		{"marker comment only, no escalated row", func(f *fixture) {
			f.trk.addPullComment(1, tracker.VerdictEscalate+"\n\ndigest")
			f.trk.addPullComment(1, tracker.VerdictReject+"\n\nfindings")
		}},
		{"both sources present", func(f *fixture) {
			seedEscalatedRun(f, f.repo, 1, "afk/7", "proj~escalate-7-old", clockTime)
			f.trk.addPullComment(1, tracker.VerdictEscalate+"\n\ndigest")
			f.trk.addPullComment(1, tracker.VerdictReject+"\n\nfindings")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			autolandOn(f, f.repo)
			originClaimBranch(f, f.repo, "afk/7")
			f.trk.addPull("afk/7", tracker.PullOpen)
			tc.setup(f)

			f.svc.SpawnOnce(t.Context())
			if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
				t.Fatalf("escalated PR spawned %d runs before the re-arm, want 0", len(active))
			}

			if err := f.st.RearmPull(t.Context(), f.repo.ID, 1, rearmAt); err != nil {
				t.Fatalf("RearmPull: %v", err)
			}
			f.svc.SpawnOnce(t.Context())
			// Exactly one live run, and a FIX one: the rejection is visible
			// again AND the restored budget is what pays for the round (an
			// escalate run here would mean the budget stayed spent).
			active := activeRuns(f)
			if len(active) != 1 || active[0].Kind != store.RunKindFix {
				t.Fatalf("active runs after the re-arm = %+v, want exactly one %s run", active, store.RunKindFix)
			}
		})
	}
}

// The NATURAL post-escalation thread, which every other re-arm test dodges by
// putting the rejection last: `labctl pr escalate` posts its marker LAST (the
// escalate seed's step 5), so the digest is the last verdict word and the
// reject that caused it sits underneath. The supersession filter has to drop
// the digest from the COMMENT slice, not merely from the word list — because
// RejectionContext re-derives the words itself and renders its lander section
// only while the last known word is reject. Filtering one and not the other
// spawns a fix run carrying an EMPTY work order: a run told to repair findings
// it was never given. So this asserts the work order's CONTENT, not the action.
func TestSpawnOnce_rearmOnEscalateLastThreadCarriesTheWorkOrder(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.trk.addPullCommentAt(1, clockTime, tracker.VerdictReject+"\n\nfindings: broken tests")
	f.trk.addPullCommentAt(1, clockTime.Add(time.Minute), tracker.VerdictEscalate+"\n\nround history digest")

	f.svc.SpawnOnce(t.Context())
	if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
		t.Fatalf("escalated PR spawned %d runs before the re-arm, want 0", len(active))
	}

	if err := f.st.RearmPull(t.Context(), f.repo.ID, 1, clockTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("RearmPull: %v", err)
	}
	f.svc.SpawnOnce(t.Context())

	fixes := runsOfKind(f, f.repo, store.RunKindFix)
	if len(fixes) != 1 {
		t.Fatalf("fix runs = %d, want 1 — dropping the digest resurfaces the reject as the last word", len(fixes))
	}
	sess, live := f.runner.Session(fixes[0].SessionName)
	if !live {
		t.Fatal("fix session not live")
	}
	want := FixSeedPrompt(7, 1, "afk/7", false, "Lander rejection:\nfindings: broken tests")
	if last := sess.Argv[len(sess.Argv)-1]; last != want {
		t.Errorf("seed argv = %q,\nwant the rejection findings as the work order — an empty one means the filter reached the words but not the comments", last)
	}
}

// The one shape where the escalate digest is a PR's ONLY marker: rejected-state
// reached purely through a human changes-requested review. Filtering leaves
// zero verdict words, which must NOT read as virgin — the virgin gate is a
// conjunction (no words AND no live review), and the human's review is live, so
// HumanRejected carries the PR into rejected-state instead. A round-1 lander
// here would re-validate ground a human has already judged.
func TestSpawnOnce_rearmOnDigestOnlyThreadWithHumanRejection(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.trk.addReviewFrom(1, "human", tracker.ReviewChangesRequested, false, "please split the migration")
	f.trk.addPullCommentAt(1, clockTime, tracker.VerdictEscalate+"\n\ndigest")

	f.svc.SpawnOnce(t.Context())
	if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
		t.Fatalf("escalated PR spawned %d runs before the re-arm, want 0", len(active))
	}

	if err := f.st.RearmPull(t.Context(), f.repo.ID, 1, clockTime.Add(time.Minute)); err != nil {
		t.Fatalf("RearmPull: %v", err)
	}
	f.svc.SpawnOnce(t.Context())

	if n := len(landerRuns(f, f.repo)); n != 0 {
		t.Errorf("lander runs = %d, want 0 — an emptied word list must not read as a VIRGIN PR", n)
	}
	fixes := runsOfKind(f, f.repo, store.RunKindFix)
	if len(fixes) != 1 {
		t.Fatalf("fix runs = %d, want 1 (the live human changes-requested is rejected-state)", len(fixes))
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

// The filter is a supersession, never an amnesty: an escalate marker dated
// AFTER the re-arm survives it and still suppresses. Pinned separately from
// TestSpawnOnce_rearmIsRepeatable's sequence so a filter that started dropping
// escalate markers unconditionally fails one test that says exactly that.
func TestSpawnOnce_escalationAfterRearmStillSuppresses(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen)
	// A rejection the PR would unambiguously act on without the marker, so
	// "nothing spawned" can only mean the fresh escalation held.
	f.trk.addPullCommentAt(1, clockTime, tracker.VerdictReject+"\n\nfindings")
	if err := f.st.RearmPull(t.Context(), f.repo.ID, 1, clockTime.Add(time.Minute)); err != nil {
		t.Fatalf("RearmPull: %v", err)
	}
	f.trk.addPullCommentAt(1, clockTime.Add(2*time.Minute), tracker.VerdictEscalate+"\n\nfresh digest")

	f.svc.SpawnOnce(t.Context())
	if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
		t.Errorf("PR escalated AFTER its re-arm spawned %d runs, want 0 — the filter must not be a blanket amnesty", len(active))
	}
}

// Repeatable in both directions, indefinitely: a fresh escalation dated AFTER
// a re-arm is terminal again (the gate re-checks a relation, it does not
// consume a one-shot clear), and a second re-arm lifts that one too.
func TestSpawnOnce_rearmIsRepeatable(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo)
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen)
	// Round 1's hand-off, then the live rejection it handed off. Every
	// assertion below turns on that rejection: without terminality this PR is
	// unambiguously a fix candidate, so "nothing spawned" can only mean the
	// gate held.
	f.trk.addPullCommentAt(1, clockTime, tracker.VerdictEscalate+"\n\nround 1 digest")
	f.trk.addPullCommentAt(1, clockTime.Add(time.Minute), tracker.VerdictReject+"\n\nfindings")

	f.svc.SpawnOnce(t.Context())
	if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
		t.Fatalf("escalated PR spawned %d runs before any re-arm, want 0", len(active))
	}

	// Re-arm #1 supersedes the round-1 marker: the PR is a fix candidate.
	if err := f.st.RearmPull(t.Context(), f.repo.ID, 1, clockTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("RearmPull: %v", err)
	}
	f.svc.SpawnOnce(t.Context())
	if n := len(runsOfKind(f, f.repo, store.RunKindFix)); n != 1 {
		t.Fatalf("fix runs after re-arm #1 = %d, want 1", n)
	}
	for _, run := range activeRuns(f) {
		f.runner.Kill(run.SessionName)
	}
	f.svc.ReapOnce(t.Context(), f.clock.Now().Add(3*time.Minute))

	// The agents failed again and escalated again — a marker NEWER than the
	// re-arm, with the rejection re-stated after it. Terminal once more: the
	// gate re-evaluates a relation every pass, it does not consume a one-shot
	// clear that a second escalation would then have to re-arm around.
	f.trk.addPullCommentAt(1, clockTime.Add(4*time.Minute), tracker.VerdictEscalate+"\n\nround 2 digest")
	f.trk.addPullCommentAt(1, clockTime.Add(5*time.Minute), tracker.VerdictReject+"\n\nstill broken")
	f.svc.SpawnOnce(t.Context())
	if active, _ := f.st.ActiveRunsByRepo(t.Context(), f.repo.ID); len(active) != 0 {
		t.Fatalf("re-escalated PR spawned %d runs, want 0 — a fresh escalation after a re-arm is terminal again", len(active))
	}
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, 1, store.RunKindFix); err != nil || n != 1 {
		t.Fatalf("fix attempts across the terminal pass = %d (err %v), want still round 1's 1 — nothing reached the launch pad", n, err)
	}

	// Re-arm #2 lifts it again — the upsert keeps only the newest gesture, so
	// re-arming is indefinitely repeatable with no history to prune. Asserted
	// on the ATTEMPT counter, not on a surviving runs row: round 1's dead fix
	// run left its unmerged worktree behind (the guarded teardown keeps it),
	// so round 2's launch fails on the launch pad — which is exactly the case
	// the intent counter exists for, and it still proves the poller decided to
	// spawn. RearmPull zeroed the counter, so reading 1 here can only be this
	// pass's burn.
	if err := f.st.RearmPull(t.Context(), f.repo.ID, 1, clockTime.Add(6*time.Minute)); err != nil {
		t.Fatalf("RearmPull: %v", err)
	}
	f.svc.SpawnOnce(t.Context())
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, 1, store.RunKindFix); err != nil || n != 1 {
		t.Errorf("fix attempts after re-arm #2 = %d (err %v), want 1 — the second gesture lifts the second escalation", n, err)
	}
}

// The trap the issue names as the worst outcome: clearing terminality while
// leaving the budget spent would re-escalate on the very first rejection and
// read to the human as "the re-arm silently did not work". RearmPull zeroes
// both halves in one operation — this proves that is visible THROUGH the
// poller, not just in the store: at a fully spent bound, the re-armed PR's
// live rejection spawns a FIX run, never a second escalate run.
func TestSpawnOnce_rearmRestoresTheFixBudget(t *testing.T) {
	f := newFixture(t)
	autolandOn(f, f.repo) // max_fix_attempts 2
	originClaimBranch(f, f.repo, "afk/7")
	f.trk.addPull("afk/7", tracker.PullOpen)
	f.trk.addPullComment(1, tracker.VerdictReject+"\n\nround 2: still broken")
	// Both fix attempts spent and the hand-off already made: rejected-at-bound
	// with an escalated row, exactly where the fix-forward loop leaves a PR.
	seedTerminalFixRun(f, f.repo, 1, "afk/7", "proj~fix-7-r1", store.RunOutcomeSuccess)
	seedTerminalFixRun(f, f.repo, 1, "afk/7", "proj~fix-7-r2", store.RunOutcomeSuccess)
	seedEscalatedRun(f, f.repo, 1, "afk/7", "proj~escalate-7-old", clockTime)

	if err := f.st.RearmPull(t.Context(), f.repo.ID, 1, clockTime.Add(time.Minute)); err != nil {
		t.Fatalf("RearmPull: %v", err)
	}
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, 1, store.RunKindFix); err != nil || n != 0 {
		t.Fatalf("fix attempts after the re-arm = %d (err %v), want 0", n, err)
	}

	f.svc.SpawnOnce(t.Context())
	if n := len(runsOfKind(f, f.repo, store.RunKindFix)); n != 3 {
		t.Fatalf("fix run rows = %d, want 3 (the 2 seeded history rows plus a fresh one) — the restored budget must be spendable", n)
	}
	if n := len(runsOfKind(f, f.repo, store.RunKindEscalate)); n != 1 {
		t.Errorf("escalate run rows = %d, want still the 1 seeded — re-escalating on the first rejection is the silent-failure trap", n)
	}
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, 1, store.RunKindFix); err != nil || n != 1 {
		t.Errorf("fix attempts after the round = %d (err %v), want 1 — the budget restarted from zero", n, err)
	}
}

// The diagnosis path (issue #188): the gate used to `continue` silently, so a
// suppressed PR looked like "autoland just ignores this one" with no way in
// short of reading the runs table. One Info line per pass per escalated PR,
// naming the repo, the pull, the branch, and WHICH source tripped it.
func TestSpawnOnce_suppressionIsLogged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		setup  func(f *fixture)
	}{
		{"marker source", "marker", func(f *fixture) {
			f.trk.addPullComment(1, tracker.VerdictEscalate+"\n\ndigest")
		}},
		{"run-row source", "run", func(f *fixture) {
			seedEscalatedRun(f, f.repo, 1, "afk/7", "proj~escalate-7-old", clockTime)
			f.trk.addPullComment(1, tracker.VerdictReject+"\n\nfindings")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			autolandOn(f, f.repo)
			originClaimBranch(f, f.repo, "afk/7")
			f.trk.addPull("afk/7", tracker.PullOpen)
			tc.setup(f)
			rec := recordLogs(f)

			f.svc.SpawnOnce(t.Context())

			got := rec.matching("autoland suppressed")
			if len(got) != 1 {
				t.Fatalf("suppression log lines = %d, want exactly 1", len(got))
			}
			for key, want := range map[string]string{
				"repo": "proj", "pull": "1", "branch": "afk/7", "source": tc.source,
			} {
				if got[0].attrs[key] != want {
					t.Errorf("log attr %q = %q, want %q", key, got[0].attrs[key], want)
				}
			}
		})
	}
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
	if n, err := f.st.AutolandAttempts(t.Context(), f.repo.ID, 1, store.RunKindFix); err != nil || n != 1 {
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
// terminal row then blocks the poller until a human re-arms the PR.
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
	// And the escalated row — stamped with the PR the run worked — is the
	// durable half of the poller's terminality gate.
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
