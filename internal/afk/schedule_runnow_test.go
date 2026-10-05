package afk

// Run now (issue #61): a Schedule's on-demand scheduled run, driven through
// the same fixture as the cadence suite. The bar is the issue's: an ORDINARY
// scheduled run (kind, ScheduleID, budget clock, failure accounting) that
// goes through the ONE spawn pass, refuses with a typed reason instead of
// queueing, works on a switched-off Schedule, and leaves the cadence's own
// state — the in-memory pending/high-water memo and last_fired_at — exactly
// where it was.

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// cadenceMemo snapshots the Schedule due-ness memo so a test can prove a Run
// now left it untouched.
func (f *fixture) cadenceMemo() (pending, checked map[string]time.Time) {
	return maps.Clone(f.svc.schedulePending), maps.Clone(f.svc.scheduleChecked)
}

func (f *fixture) atCap() {
	f.t.Helper()
	if err := f.st.SetSetting(f.t.Context(), store.SettingMaxInstances, "1"); err != nil {
		f.t.Fatal(err)
	}
	f.runner.AddLive("other~existing")
}

// The success path: an ordinary scheduled run with the pinned identity, the
// Schedule link, and the ordinary 30-minute budget clock — for an enabled
// Schedule, for a switched-off one (testing a prompt before arming the
// cadence), and for a repo whose own AFK three-strikes pause is tripped
// (that pause never stops Schedules).
func TestRunScheduleNow_launchesOrdinaryScheduledRun(t *testing.T) {
	for _, tc := range []struct {
		name        string
		enabled     bool
		repoStrikes int
	}{
		{name: "enabled", enabled: true},
		{name: "switched off", enabled: false},
		{name: "repo AFK-paused", enabled: true, repoStrikes: PauseThreshold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.setFailures(f.repo, tc.repoStrikes)
			sched := f.addSchedule("deps", func(sc *store.Schedule) { sc.Enabled = tc.enabled })

			run, err := f.svc.RunScheduleNow(t.Context(), sched.ID)
			if err != nil {
				t.Fatalf("RunScheduleNow: %v", err)
			}
			if run.Kind != store.RunKindScheduled {
				t.Errorf("kind = %q, want scheduled", run.Kind)
			}
			if run.ScheduleID == nil || *run.ScheduleID != sched.ID {
				t.Errorf("ScheduleID = %v, want %q", run.ScheduleID, sched.ID)
			}
			if run.IssueNumber != nil {
				t.Errorf("IssueNumber = %v, want nil", *run.IssueNumber)
			}
			wantLabel := ScheduleLabel(sched.ID, f.clock.Now())
			if run.SessionName != "proj~"+wantLabel || run.Branch != f.repo.ManualBranchPrefix+wantLabel {
				t.Errorf("session/branch = %q/%q, want the scheduled identity for label %q", run.SessionName, run.Branch, wantLabel)
			}
			if want := f.clock.Now().Add(30 * time.Minute); run.BudgetDeadline == nil || !run.BudgetDeadline.Equal(want) {
				t.Errorf("budget deadline = %v, want %v (the ordinary scheduled-run budget)", run.BudgetDeadline, want)
			}
			sess, live := f.runner.Session(run.SessionName)
			if !live {
				t.Fatalf("session %q not live", run.SessionName)
			}
			if want := ComposeSchedulePrompt(sched.Prompt, sched.Flows, sched.Name); sess.Argv[len(sess.Argv)-1] != want {
				t.Errorf("seed = %q, want the composed schedule prompt %q", sess.Argv[len(sess.Argv)-1], want)
			}
			// The returned run is the stored row.
			if runs := f.scheduledRuns(); len(runs) != 1 || runs[0].ID != run.ID || runs[0].Outcome != store.RunOutcomeActive {
				t.Fatalf("scheduled runs = %+v, want exactly the returned active run", runs)
			}
			// last_fired_at is cadence bookkeeping: a Run now never stamps it.
			if row := f.scheduleRow(sched.ID); row.LastFiredAt != nil {
				t.Errorf("last_fired_at = %v, want nil (a Run now is not a cadence firing)", row.LastFiredAt)
			}
		})
	}
}

// The per-Schedule budget override applies to a Run now like to any firing.
func TestRunScheduleNow_budgetOverride(t *testing.T) {
	f := newFixture(t)
	mins := 45
	sched := f.addSchedule("deps", func(sc *store.Schedule) { sc.BudgetMinutes = &mins })
	run, err := f.svc.RunScheduleNow(t.Context(), sched.ID)
	if err != nil {
		t.Fatalf("RunScheduleNow: %v", err)
	}
	if want := f.clock.Now().Add(45 * time.Minute); run.BudgetDeadline == nil || !run.BudgetDeadline.Equal(want) {
		t.Errorf("budget deadline = %v, want %v", run.BudgetDeadline, want)
	}
}

// Every refusal is typed, and the SPECIFIC reason wins over at-cap when
// several apply.
func TestRunScheduleNow_refusals(t *testing.T) {
	t.Run("paused", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("deps", nil)
		if _, err := f.st.SetSchedulePaused(t.Context(), sched.ID, true); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, ErrSchedulePaused) {
			t.Fatalf("err = %v, want ErrSchedulePaused", err)
		}
		if runs := f.scheduledRuns(); len(runs) != 0 {
			t.Errorf("paused Run now launched %d runs", len(runs))
		}
	})
	t.Run("paused and at cap answers paused", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("deps", nil)
		if _, err := f.st.SetSchedulePaused(t.Context(), sched.ID, true); err != nil {
			t.Fatal(err)
		}
		f.atCap()
		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, ErrSchedulePaused) {
			t.Fatalf("err = %v, want ErrSchedulePaused (the specific reason wins over at-cap)", err)
		}
	})
	t.Run("previous Run now still live", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("deps", nil)
		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); err != nil {
			t.Fatalf("first Run now: %v", err)
		}
		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, ErrScheduleRunLive) {
			t.Fatalf("second Run now err = %v, want ErrScheduleRunLive", err)
		}
		if runs := f.scheduledRuns(); len(runs) != 1 {
			t.Errorf("scheduled runs = %d, want 1", len(runs))
		}
	})
	t.Run("cadence run still live", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("deps", nil)
		f.sightSchedules()                // 12:00
		f.clock.Advance(16 * time.Minute) // 12:16 — the 12:15 slot fires
		f.svc.SpawnOnce(t.Context())
		if runs := f.scheduledRuns(); len(runs) != 1 {
			t.Fatalf("cadence runs = %d, want 1", len(runs))
		}
		// One live run fills a cap of 1 too: run-live must still win.
		if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, ErrScheduleRunLive) {
			t.Fatalf("err = %v, want ErrScheduleRunLive", err)
		}
		if runs := f.scheduledRuns(); len(runs) != 1 {
			t.Errorf("scheduled runs = %d, want still 1", len(runs))
		}
	})
	t.Run("logged out", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("deps", nil)
		f.prov.SetLoggedIn(false)
		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, instance.ErrLoggedOut) {
			t.Fatalf("err = %v, want instance.ErrLoggedOut", err)
		}
		if runs := f.scheduledRuns(); len(runs) != 0 {
			t.Errorf("logged-out Run now launched %d runs", len(runs))
		}
	})
	t.Run("repo not ready", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("deps", nil)
		if err := f.st.UpdateRepoCloneStatus(t.Context(), f.repo.ID, store.CloneStatusCloning, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, instance.ErrRepoNotReady) {
			t.Fatalf("err = %v, want instance.ErrRepoNotReady", err)
		}
	})
	t.Run("unknown schedule", func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.svc.RunScheduleNow(t.Context(), "sched_missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want store.ErrNotFound", err)
		}
	})
	t.Run("empty composed prompt", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("ghostly", func(sc *store.Schedule) {
			sc.Prompt = ""
			sc.Flows = []string{"retired-flow"}
		})
		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, ErrScheduleEmptyPrompt) {
			t.Fatalf("err = %v, want ErrScheduleEmptyPrompt", err)
		}
	})
}

// At cap a Run now is refused with ErrOverCap — and NOT queued: once the cap
// frees, later passes launch nothing for it (enabled or switched off alike).
func TestRunScheduleNow_atCapRefusedNeverQueued(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		f := newFixture(t)
		sched := f.addSchedule("daily", func(sc *store.Schedule) {
			sc.Cadence = "0 6 * * *" // no slot inside this test's window
			sc.Enabled = enabled
		})
		f.sightSchedules()
		f.atCap()

		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, instance.ErrOverCap) {
			t.Fatalf("enabled=%v: err = %v, want instance.ErrOverCap", enabled, err)
		}

		f.runner.Kill("other~existing") // the cap frees
		for range 3 {
			f.clock.Advance(time.Minute)
			f.svc.SpawnOnce(t.Context())
		}
		if runs := f.scheduledRuns(); len(runs) != 0 {
			t.Fatalf("enabled=%v: a refused Run now fired later: %d scheduled runs, want 0", enabled, len(runs))
		}
	}
}

// The Run now candidate rides the pass AFTER the producers: when the same
// Schedule's cadence slot is due in that very pass, the cadence firing
// launches first (stamping last_fired_at, consuming its pending) and the Run
// now is refused as run-live — never the reverse, which would overlap-skip
// and so consume the cadence firing. With exactly one free slot the Run now
// is snapshot-vetoed instead of called, and the specific reason must still
// win over at-cap.
func TestRunScheduleNow_cadenceDueInSamePassLaunchesFirst(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  string // "" = the seeded default (plenty of headroom)
	}{
		{name: "headroom"},
		{name: "one free slot", cap: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sched := f.addSchedule("deps", nil)
			f.sightSchedules() // 12:00
			if tc.cap != "" {
				if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, tc.cap); err != nil {
					t.Fatal(err)
				}
				f.runner.AddLive("other~existing")
			}
			f.clock.Advance(16 * time.Minute) // 12:16 — the 12:15 slot is due in the Run now's pass

			if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, ErrScheduleRunLive) {
				t.Fatalf("err = %v, want ErrScheduleRunLive (the cadence firing launched first)", err)
			}
			runs := f.scheduledRuns()
			if len(runs) != 1 {
				t.Fatalf("scheduled runs = %d, want 1 (the cadence firing)", len(runs))
			}
			if row := f.scheduleRow(sched.ID); row.LastFiredAt == nil || !row.LastFiredAt.Equal(f.clock.Now()) {
				t.Errorf("last_fired_at = %v, want %v (the run is the cadence firing)", row.LastFiredAt, f.clock.Now())
			}
			if _, held := f.svc.schedulePending[sched.ID]; held {
				t.Error("cadence firing still pending after it launched")
			}
		})
	}
}

// A Run now never reads or writes the cadence memo: the pending at-cap
// firing and the high-water marks are identical before and after a refused
// Run now and a launched one, and the held firing still launches on its own
// once the cap frees.
func TestRunScheduleNow_leavesCadenceMemoUntouched(t *testing.T) {
	t.Run("refused at cap with a held firing", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("deps", nil)
		f.sightSchedules() // 12:00
		f.atCap()
		f.clock.Advance(16 * time.Minute) // 12:16 — the 12:15 firing is owed but at cap
		f.svc.SpawnOnce(t.Context())
		if due, held := f.svc.schedulePending[sched.ID]; !held || !due.Equal(clockTime.Add(15*time.Minute)) {
			t.Fatalf("pending = %v/%v, want the 12:15 firing held at cap", due, held)
		}
		pending, checked := f.cadenceMemo()

		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, instance.ErrOverCap) {
			t.Fatalf("err = %v, want instance.ErrOverCap", err)
		}
		if p, c := f.cadenceMemo(); !maps.Equal(p, pending) || !maps.Equal(c, checked) {
			t.Fatalf("memo moved: pending %v → %v, checked %v → %v", pending, p, checked, c)
		}

		// The held cadence firing launches on its own once the cap frees —
		// one run, the cadence's.
		f.runner.Kill("other~existing")
		f.clock.Advance(time.Minute) // 12:17
		f.svc.SpawnOnce(t.Context())
		if runs := f.scheduledRuns(); len(runs) != 1 {
			t.Fatalf("scheduled runs after the cap freed = %d, want 1 (the held cadence firing)", len(runs))
		}
		if row := f.scheduleRow(sched.ID); row.LastFiredAt == nil {
			t.Error("the held cadence firing did not stamp last_fired_at")
		}
	})
	t.Run("launched", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("deps", nil)
		f.sightSchedules()               // 12:00
		f.clock.Advance(5 * time.Minute) // 12:05
		f.svc.SpawnOnce(t.Context())     // the tick a live engine would run here
		pending, checked := f.cadenceMemo()

		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); err != nil {
			t.Fatalf("RunScheduleNow: %v", err)
		}
		if p, c := f.cadenceMemo(); !maps.Equal(p, pending) || !maps.Equal(c, checked) {
			t.Fatalf("memo moved: pending %v → %v, checked %v → %v", pending, p, checked, c)
		}
	})
	t.Run("switched off", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("deps", func(sc *store.Schedule) { sc.Enabled = false })
		f.sightSchedules()
		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); err != nil {
			t.Fatalf("RunScheduleNow: %v", err)
		}
		if _, ok := f.svc.scheduleChecked[sched.ID]; ok {
			t.Error("a Run now armed a switched-off Schedule's cadence")
		}
		if _, ok := f.svc.schedulePending[sched.ID]; ok {
			t.Error("a Run now left a pending firing for a switched-off Schedule")
		}
	})
}

// The next cadence firing is neither moved nor consumed: sight at 12:00, Run
// now at 12:05, the run dies and is reaped, and the 12:15 slot fires on its
// own — with last_fired_at untouched until then.
func TestRunScheduleNow_nextCadenceFiringUnchanged(t *testing.T) {
	f := newFixture(t)
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00

	f.clock.Advance(5 * time.Minute) // 12:05
	now, err := f.svc.RunScheduleNow(t.Context(), sched.ID)
	if err != nil {
		t.Fatalf("RunScheduleNow: %v", err)
	}
	if row := f.scheduleRow(sched.ID); row.LastFiredAt != nil {
		t.Fatalf("last_fired_at = %v after a Run now, want nil", row.LastFiredAt)
	}

	f.runner.Kill(now.SessionName)
	f.clock.Advance(time.Minute) // 12:06
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if got := f.runRow(now.ID); got.Outcome != store.RunOutcomeDeath {
		t.Fatalf("Run now outcome = %q, want death", got.Outcome)
	}

	f.clock.Advance(10 * time.Minute) // 12:16 — past the 12:15 slot
	f.svc.SpawnOnce(t.Context())
	runs := f.scheduledRuns()
	if len(runs) != 2 {
		t.Fatalf("scheduled runs = %d, want 2 (the Run now and the 12:15 cadence firing)", len(runs))
	}
	if cadence := runs[1]; cadence.Outcome != store.RunOutcomeActive ||
		cadence.SessionName != "proj~"+ScheduleLabel(sched.ID, f.clock.Now()) {
		t.Errorf("cadence run = %s (%s), want the live 12:16 firing", cadence.SessionName, cadence.Outcome)
	}
	if row := f.scheduleRow(sched.ID); row.LastFiredAt == nil || !row.LastFiredAt.Equal(f.clock.Now()) {
		t.Errorf("last_fired_at = %v, want %v (stamped by the cadence firing alone)", row.LastFiredAt, f.clock.Now())
	}
}

// Failure accounting is the ordinary scheduled run's: a Run now run that
// dies before its deadline strikes its Schedule, never the repo's AFK
// three-strikes counter — here one strike short of the repo pause, which it
// must not trip.
func TestRunScheduleNow_deathStrikesTheScheduleOnly(t *testing.T) {
	f := newFixture(t)
	f.setFailures(f.repo, PauseThreshold-1)
	sched := f.addSchedule("deps", nil)

	run, err := f.svc.RunScheduleNow(t.Context(), sched.ID)
	if err != nil {
		t.Fatalf("RunScheduleNow: %v", err)
	}
	f.runner.Kill(run.SessionName)
	f.clock.Advance(time.Minute)
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
		t.Fatalf("outcome = %q, want death (died before its budget deadline)", got.Outcome)
	}
	if row := f.scheduleRow(sched.ID); row.ConsecutiveFailures != 1 || row.Paused {
		t.Errorf("schedule failures/paused = %d/%v, want 1/false", row.ConsecutiveFailures, row.Paused)
	}
	if n := f.failures(f.repo); n != PauseThreshold-1 {
		t.Errorf("repo failures = %d, want %d (a scheduled death never strikes the repo)", n, PauseThreshold-1)
	}
}

// Run now's labels are minute-granular (ADR-0062), so a second Run now in the
// same minute after the first ended would reuse its session, branch, and
// worktree. It must be refused with ErrScheduleStartedThisMinute BEFORE the
// launch — never a mid-launch git "branch already exists" — and launch
// normally once the minute turns.
func TestRunScheduleNow_sameMinuteIdentityRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(f *fixture, run store.Run)
	}{
		{
			// A neutral Stop keeps the worktree and the branch — the commit
			// makes the branch unmergeable too, so nothing could ever reclaim it.
			name: "stopped run keeps its branch",
			end: func(f *fixture, run store.Run) {
				f.commitInWorktree(run.WorktreePath)
				if err := f.svc.StopAFK(f.t.Context(), run.SessionName); err != nil {
					f.t.Fatalf("StopAFK: %v", err)
				}
				if !f.branchExists(f.repo, run.Branch) {
					f.t.Fatal("fixture: the stopped run's branch did not survive")
				}
			},
		},
		{
			// A clean death reaps its worktree and merged branch: only the
			// ended run's row is left to collide with.
			name: "dead run leaves only its row",
			end: func(f *fixture, run store.Run) {
				f.runner.Kill(run.SessionName)
				f.svc.ReapOnce(f.t.Context(), f.clock.Now()) // same minute
				if got := f.runRow(run.ID); got.Outcome != store.RunOutcomeDeath {
					f.t.Fatalf("fixture: outcome = %q, want death", got.Outcome)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sched := f.addSchedule("deps", nil)
			first, err := f.svc.RunScheduleNow(t.Context(), sched.ID)
			if err != nil {
				t.Fatalf("first Run now: %v", err)
			}
			tc.end(f, first)
			f.clock.Advance(30 * time.Second) // 12:00:30 — still the same minute

			if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, ErrScheduleStartedThisMinute) {
				t.Fatalf("same-minute Run now err = %v, want ErrScheduleStartedThisMinute", err)
			}
			// The locked launch is the authority on its own, too (a minute
			// boundary or a racing launch between pre-check and lock).
			if res := f.svc.launchScheduledRun(t.Context(), sched.ID, true); !errors.Is(res.err, ErrScheduleStartedThisMinute) || res.outcome != spawnSkipped {
				t.Fatalf("locked on-demand launch = %v/%v, want spawnSkipped/ErrScheduleStartedThisMinute", res.outcome, res.err)
			}
			if runs := f.scheduledRuns(); len(runs) != 1 {
				t.Fatalf("scheduled runs = %d, want 1 (the refusal created nothing)", len(runs))
			}

			f.clock.Advance(time.Minute) // 12:01:30 — a fresh label
			second, err := f.svc.RunScheduleNow(t.Context(), sched.ID)
			if err != nil {
				t.Fatalf("next-minute Run now: %v", err)
			}
			if second.SessionName == first.SessionName || second.Branch == first.Branch {
				t.Errorf("next-minute identity %s/%s reuses the first run's", second.SessionName, second.Branch)
			}
		})
	}
}

// The branch in the bare clone is checked on its own: a same-minute branch no
// row of this Schedule accounts for still refuses rather than failing the
// launch in git.
func TestRunScheduleNow_sameMinuteBranchWithoutRowRefused(t *testing.T) {
	f := newFixture(t)
	sched := f.addSchedule("deps", nil)
	f.createClaimBranch(f.repo, f.repo.ManualBranchPrefix+ScheduleLabel(sched.ID, f.clock.Now()))

	if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, ErrScheduleStartedThisMinute) {
		t.Fatalf("err = %v, want ErrScheduleStartedThisMinute", err)
	}
	if runs := f.scheduledRuns(); len(runs) != 0 {
		t.Errorf("scheduled runs = %d, want 0", len(runs))
	}
}

// The specific reason wins over at-cap on the snapshot-veto path too — the
// path where the pass never calls the candidate, so only RunScheduleNow's
// own looks can name a reason.
func TestRunScheduleNow_specificReasonBeatsSnapshotVeto(t *testing.T) {
	// A Schedule whose prompt and flows compose to nothing, at cap: the
	// composed prompt is checked before the pass, in the locked core's order.
	t.Run("empty composed prompt at cap", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("ghostly", func(sc *store.Schedule) {
			sc.Prompt = ""
			sc.Flows = []string{"retired-flow"}
		})
		f.atCap()
		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, ErrScheduleEmptyPrompt) {
			t.Fatalf("err = %v, want ErrScheduleEmptyPrompt (the specific reason wins over at-cap)", err)
		}
		if runs := f.scheduledRuns(); len(runs) != 0 {
			t.Errorf("scheduled runs = %d, want 0", len(runs))
		}
	})

	// The repo's clone goes not-ready AFTER the pre-check passed and before
	// the pass (here: as the pass lists sessions), and the pass's snapshot
	// vetoes at cap. The veto branch re-checks clone readiness.
	t.Run("clone went not-ready before an at-cap pass", func(t *testing.T) {
		var hooked *listHookRunner
		f := newFixtureWrapped(t, "afk/<N>", func(fake *tmuxx.Fake) tmuxx.SessionRunner {
			hooked = &listHookRunner{Fake: fake}
			return hooked
		})
		sched := f.addSchedule("deps", func(sc *store.Schedule) { sc.Cadence = "0 6 * * *" })
		f.atCap()
		hooked.before = func(*tmuxx.Fake) {
			if err := f.st.UpdateRepoCloneStatus(context.Background(), f.repo.ID, store.CloneStatusCloning, ""); err != nil {
				t.Error(err)
			}
		}
		if _, err := f.svc.RunScheduleNow(t.Context(), sched.ID); !errors.Is(err, instance.ErrRepoNotReady) {
			t.Fatalf("err = %v, want instance.ErrRepoNotReady (the specific reason wins over at-cap)", err)
		}
		if runs := f.scheduledRuns(); len(runs) != 0 {
			t.Errorf("scheduled runs = %d, want 0", len(runs))
		}
	})
}

// The LOCKED path's at-cap answer: the pass's snapshot has headroom and calls
// the candidate, and the locked launch's fresh session listing finds the cap
// reached (a session started in between). The answer is "instance cap
// reached" — what the API answers 409 with — nothing launches, and nothing
// is queued: once the cap frees, later passes start nothing for it.
func TestRunScheduleNow_lockedPathAtCap(t *testing.T) {
	var hooked *listHookRunner
	f := newFixtureWrapped(t, "afk/<N>", func(fake *tmuxx.Fake) tmuxx.SessionRunner {
		hooked = &listHookRunner{Fake: fake}
		return hooked
	})
	sched := f.addSchedule("daily", func(sc *store.Schedule) { sc.Cadence = "0 6 * * *" }) // no slot in this test's window
	f.sightSchedules()
	if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
		t.Fatal(err)
	}

	// List #1 is the pass's snapshot (0 live, under the cap of 1); list #2 is
	// the locked launch's fresh count, by which time a session has appeared.
	lists := 0
	hooked.before = func(fake *tmuxx.Fake) {
		lists++
		if lists == 2 {
			fake.AddLive("other~intruder")
		}
	}
	_, err := f.svc.RunScheduleNow(t.Context(), sched.ID)
	if !errors.Is(err, instance.ErrOverCap) || err.Error() != "instance cap reached" {
		t.Fatalf("err = %v, want instance.ErrOverCap (\"instance cap reached\")", err)
	}
	if lists < 2 {
		t.Fatalf("session listings = %d; the candidate never reached the locked launch", lists)
	}
	if runs := f.scheduledRuns(); len(runs) != 0 {
		t.Fatalf("scheduled runs = %d, want 0", len(runs))
	}

	hooked.before = nil
	f.runner.Kill("other~intruder") // the cap frees
	for range 3 {
		f.clock.Advance(time.Minute)
		f.svc.SpawnOnce(t.Context())
	}
	if runs := f.scheduledRuns(); len(runs) != 0 {
		t.Fatalf("a refused Run now fired later: %d scheduled runs, want 0", len(runs))
	}
}
