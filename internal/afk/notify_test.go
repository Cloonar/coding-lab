package afk

// Done-signal notification tests (issue #100): drive real success/failure
// reaps through the engine fixture and assert the injected Notify seam fired
// (or stayed silent) with the exact payload. The seam is injected
// post-construction the same way metrics_test wires the metrics handle
// (f.svc.notify = cap.notify); the entire rest of the suite runs with a nil
// notifier, which is the nil-safety proof.

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// captureNotifier records every done-signal Notification the reaper fires.
type captureNotifier struct {
	mu  sync.Mutex
	got []Notification
}

func (c *captureNotifier) notify(n Notification) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, n)
}

func (c *captureNotifier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.got)
}

func (c *captureNotifier) last() Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.got) == 0 {
		return Notification{}
	}
	return c.got[len(c.got)-1]
}

func TestReap_successNotifiesDoneSignal(t *testing.T) {
	f := newFixture(t)
	cap := &captureNotifier{}
	f.svc.notify = cap.notify
	f.trk.setReady(7)

	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	f.commitInWorktree(run.WorktreePath)
	f.trk.addPull("afk/7", tracker.PullOpen)
	pull := f.trk.pulls[0]
	f.trk.setPullTitle(pull.Number, "Resolve issue 7")

	f.clock.Advance(10 * time.Minute)
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	// The reap's own side effects still happen.
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success", got.Outcome)
	}
	// Exactly one notification, all four fields (builtin fixture → "change request").
	if cap.count() != 1 {
		t.Fatalf("notifications = %d, want exactly 1", cap.count())
	}
	want := Notification{
		Title: fmt.Sprintf("proj~afk-7 opened change request #%d", pull.Number),
		Body:  "Resolve issue 7",
		Tag:   run.ID,
		Route: "/runs/" + run.ID,
	}
	if got := cap.last(); got != want {
		t.Errorf("notification = %+v; want %+v", got, want)
	}
	// The heavier detail call happened (the title came through, not the degraded form).
	if f.trk.detailCallCount() == 0 {
		t.Error("pull detail never fetched (body came from the hot-path PullRef?)")
	}

	// A second sweep is idempotent: the claim is the once-guarantee.
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if cap.count() != 1 {
		t.Errorf("second reap notified again: count = %d, want 1", cap.count())
	}
}

func TestReap_forgeBindingSaysPR(t *testing.T) {
	f := newFixture(t)
	cap := &captureNotifier{}
	f.svc.notify = cap.notify
	f.trk.setReady(7)

	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	f.commitInWorktree(run.WorktreePath)

	// Flip the repo to a forge binding before the reap — the reaper re-reads the
	// row each tick, so the done noun becomes "PR".
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		TrackerBinding: store.Set(store.TrackerBindingForge),
	}); err != nil {
		t.Fatalf("flip tracker binding: %v", err)
	}
	f.trk.addPull("afk/7", tracker.PullMerged)
	pull := f.trk.pulls[0]
	f.trk.setPullTitle(pull.Number, "Ship it")

	f.clock.Advance(time.Minute)
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success", got.Outcome)
	}
	if cap.count() != 1 {
		t.Fatalf("notifications = %d, want exactly 1", cap.count())
	}
	want := Notification{
		Title: fmt.Sprintf("proj~afk-7 opened PR #%d", pull.Number),
		Body:  "Ship it",
		Tag:   run.ID,
		Route: "/runs/" + run.ID,
	}
	if got := cap.last(); got != want {
		t.Errorf("notification = %+v; want %+v", got, want)
	}
}

// A user-set run title replaces the session name in the push title (issue
// #111); the session-name form in the tests above is the title-unset fallback.
func TestReap_notifyUsesTitleOverlay(t *testing.T) {
	f := newFixture(t)
	cap := &captureNotifier{}
	f.svc.notify = cap.notify
	f.trk.setReady(7)

	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	f.commitInWorktree(run.WorktreePath)
	title := "Payment retries"
	if err := f.st.UpdateRunTitle(t.Context(), run.ID, &title); err != nil {
		t.Fatalf("UpdateRunTitle: %v", err)
	}
	f.trk.addPull("afk/7", tracker.PullOpen)
	pull := f.trk.pulls[0]
	f.trk.setPullTitle(pull.Number, "Resolve issue 7")

	f.clock.Advance(time.Minute)
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	if cap.count() != 1 {
		t.Fatalf("notifications = %d, want exactly 1", cap.count())
	}
	want := fmt.Sprintf("Payment retries opened change request #%d", pull.Number)
	if got := cap.last().Title; got != want {
		t.Errorf("Title = %q, want %q (the title overlay replaces the session name)", got, want)
	}
}

func TestReap_notifyDetailFetchDegrades(t *testing.T) {
	f := newFixture(t)
	cap := &captureNotifier{}
	f.svc.notify = cap.notify
	f.trk.setReady(7)

	run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	f.commitInWorktree(run.WorktreePath)
	f.trk.addPull("afk/7", tracker.PullOpen)
	pull := f.trk.pulls[0]
	f.trk.failPullDetail(errors.New("forge detail 500"))

	f.clock.Advance(time.Minute)
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	// The reap is unaffected by the detail failure.
	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success (detail failure must not affect the reap)", got.Outcome)
	}
	if cap.count() != 1 {
		t.Fatalf("notifications = %d, want exactly 1", cap.count())
	}
	if got := cap.last().Body; got != fmt.Sprintf("change request #%d", pull.Number) {
		t.Errorf("degraded body = %q, want the bare number form", got)
	}
}

func TestReap_failuresDoNotNotify(t *testing.T) {
	t.Run("death does not notify", func(t *testing.T) {
		f := newFixture(t)
		cap := &captureNotifier{}
		f.svc.notify = cap.notify
		f.trk.setReady(7)
		run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.runner.Kill(run.SessionName) // crashed, no pull
		f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Minute))
		if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
			t.Fatalf("outcome = %q, want death", got.Outcome)
		}
		if cap.count() != 0 {
			t.Errorf("death notified: count = %d, want 0", cap.count())
		}
	})

	t.Run("timeout does not notify", func(t *testing.T) {
		f := newFixture(t)
		cap := &captureNotifier{}
		f.svc.notify = cap.notify
		f.trk.setReady(7)
		run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.commitInWorktree(run.WorktreePath) // alive but over budget, no pull
		f.svc.ReapOnce(t.Context(), clockTime.Add(120*time.Minute))
		if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeTimeout {
			t.Fatalf("outcome = %q, want timeout", got.Outcome)
		}
		if cap.count() != 0 {
			t.Errorf("timeout notified: count = %d, want 0", cap.count())
		}
	})

	t.Run("three strikes to pause do not notify", func(t *testing.T) {
		f := newFixture(t)
		cap := &captureNotifier{}
		f.svc.notify = cap.notify
		f.trk.setReady(7, 8, 9)
		for i := 0; i < PauseThreshold; i++ {
			run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
			if err != nil {
				t.Fatalf("strike %d start: %v", i, err)
			}
			f.runner.Kill(run.SessionName)
			f.svc.ReapOnce(t.Context(), f.clock.Now().Add(time.Duration(i+1)*time.Minute))
		}
		if n := f.failures(f.repo); n != PauseThreshold {
			t.Fatalf("failures = %d, want %d (paused)", n, PauseThreshold)
		}
		if cap.count() != 0 {
			t.Errorf("three-strikes reaps notified: count = %d, want 0", cap.count())
		}
	})

	t.Run("neutral stop does not notify", func(t *testing.T) {
		f := newFixture(t)
		cap := &captureNotifier{}
		f.svc.notify = cap.notify
		f.trk.setReady(7)
		run, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.svc.StopAFK(t.Context(), run.SessionName); err != nil {
			t.Fatalf("StopAFK: %v", err)
		}
		if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeStopped {
			t.Fatalf("outcome = %q, want stopped", got.Outcome)
		}
		if cap.count() != 0 {
			t.Errorf("neutral stop notified: count = %d, want 0", cap.count())
		}
	})
}
