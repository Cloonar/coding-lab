package afk

// Schedule engine tests (issue #247 / ADR-0062), driven through the same
// fixture as the rest of the suite: real store + git, fake tmux/tracker/
// provider, FakeClock. The producer is exercised through SpawnOnce (the one
// pass), launchScheduled both through the pass and directly for its locked
// guards, and the reaper's scheduled-kind rules (no pull reads, budget
// expiry as completion, per-Schedule strikes with the pause push) through
// ReapOnce — the design §11 bar the sibling suites set.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// logRecorder captures log records so the loud-by-contract lines (missed
// slots, overlap skips, supersession, autoland's terminality suppression) are
// assertable. Attrs are captured alongside the message because issue #188's
// suppression line IS its attrs — a bare "autoland suppressed" would diagnose
// nothing without the repo/pull/branch/source it names.
type logRecorder struct {
	mu   sync.Mutex
	recs []loggedRecord
}

// loggedRecord is one captured record flattened to its message plus a
// key→rendered-value map of its attrs.
type loggedRecord struct {
	msg   string
	attrs map[string]string
}

func (h *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *logRecorder) Handle(_ context.Context, r slog.Record) error {
	rec := loggedRecord{msg: r.Message, attrs: make(map[string]string, r.NumAttrs())}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, rec)
	return nil
}
func (h *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *logRecorder) WithGroup(string) slog.Handler      { return h }

func (h *logRecorder) count(msg string) int { return len(h.matching(msg)) }

// matching snapshots every captured record whose message contains msg.
func (h *logRecorder) matching(msg string) []loggedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []loggedRecord
	for _, r := range h.recs {
		if strings.Contains(r.msg, msg) {
			out = append(out, r)
		}
	}
	return out
}

// recordLogs swaps the service logger for a recorder (same-package test
// wiring, mirroring how notify_test injects the capture notifier).
func recordLogs(f *fixture) *logRecorder {
	rec := &logRecorder{}
	f.svc.log = slog.New(rec)
	return rec
}

// addSchedule creates an enabled every-15-minutes Schedule on the fixture
// repo (matches :00/:15/:30/:45 against the fixture clock's 12:00 start).
// Firing tests follow it with a sighting pass (sightSchedules) before
// advancing the clock: the first pass that sees a schedule ID only arms its
// cadence forward-only, so a schedule must be sighted before a cron match
// can become a candidate.
func (f *fixture) addSchedule(name string, mut func(*store.Schedule)) store.Schedule {
	f.t.Helper()
	sc := store.Schedule{
		ID: ids.NewID("sched"), RepoID: f.repo.ID, Name: name,
		Cadence: "*/15 * * * *",
		Prompt:  "Investigate dependency updates for <SCHEDULE>.",
		Flows:   []string{"autolander"},
		Enabled: true, CreatedAt: clockTime, UpdatedAt: clockTime,
	}
	if mut != nil {
		mut(&sc)
	}
	created, err := f.st.CreateSchedule(f.t.Context(), sc)
	if err != nil {
		f.t.Fatalf("CreateSchedule: %v", err)
	}
	return created
}

// sightSchedules runs one pass at the current clock so every existing
// schedule ID is sighted and its high-water mark armed — the pass a live
// engine's next tick would be.
func (f *fixture) sightSchedules() {
	f.t.Helper()
	f.svc.SpawnOnce(f.t.Context())
}

func (f *fixture) scheduleRow(id string) store.Schedule {
	f.t.Helper()
	sc, err := f.st.ScheduleByID(f.t.Context(), id)
	if err != nil {
		f.t.Fatalf("ScheduleByID: %v", err)
	}
	return sc
}

// scheduledRuns lists the fixture repo's scheduled-kind run rows (any
// outcome), oldest first.
func (f *fixture) scheduledRuns() []store.Run {
	f.t.Helper()
	all, err := f.st.RunsByRepo(f.t.Context(), f.repo.ID, 0)
	if err != nil {
		f.t.Fatalf("RunsByRepo: %v", err)
	}
	var out []store.Run
	for i := len(all) - 1; i >= 0; i-- { // newest-first listing → oldest first
		if all[i].Kind == store.RunKindScheduled {
			out = append(out, all[i])
		}
	}
	return out
}

// --- stage rank --------------------------------------------------------------

// StageScheduled slots between fix and new work: in-flight claim work
// outranks a firing (drain before fill), but a due firing is time-anchored
// and outranks fresh AFK selection (ADR-0062).
func TestSpawnPass_scheduledStageBetweenFixAndNewWork(t *testing.T) {
	f := newFixture(t)
	rec := &spawnRecorder{}
	f.svc.spawnPass(t.Context(), 0, []spawnCandidate{
		rec.candidate("new", StageNewWork, 100, spawnSpawned),
		rec.candidate("sched", StageScheduled, 100, spawnSpawned),
		rec.candidate("lander", StageLander, 100, spawnSpawned),
		rec.candidate("fix", StageFix, 100, spawnSpawned),
	})
	want := []string{"lander", "fix", "sched", "new"}
	if len(rec.order) != len(want) {
		t.Fatalf("launch order = %v, want %v", rec.order, want)
	}
	for i := range want {
		if rec.order[i] != want[i] {
			t.Fatalf("launch order = %v, want %v (lander < fix < scheduled < new work)", rec.order, want)
		}
	}
}

// --- producer + launch identity ---------------------------------------------

// The full happy path through the ONE pass: a due firing spawns a scheduled
// run with the pinned identity — label sched-<id-suffix>-<stamp>, branch
// exactly manual prefix + label (the manual-instance composition reconcile's
// fallback re-derives), ScheduleID linked, no issue, the 30-minute default
// budget, and the composed Schedule prompt VERBATIM (no afk_prompt override,
// no token interpolation beyond <SCHEDULE>).
func TestScheduleFiring_spawnsScheduledRunWithPinnedIdentity(t *testing.T) {
	f := newFixture(t)
	// An afk_prompt override must NOT leak into a schedule prompt — it is an
	// AFK-issue prompt (ADR-0027), not a schedule prompt.
	override := "AFK override that must not appear."
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID,
		store.RepoSettingsUpdate{AFKPrompt: store.Set(&override)}); err != nil {
		t.Fatalf("set afk_prompt: %v", err)
	}
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence

	f.clock.Advance(16 * time.Minute) // 12:16 — one cron match (12:15) in window
	f.svc.SpawnOnce(t.Context())

	active, err := f.st.ActiveRuns(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("active runs = %d, want 1", len(active))
	}
	run := active[0]
	if run.Kind != store.RunKindScheduled {
		t.Fatalf("kind = %q, want scheduled", run.Kind)
	}
	if run.ScheduleID == nil || *run.ScheduleID != sched.ID {
		t.Errorf("ScheduleID = %v, want %q (the firing link is the column, not the label)", run.ScheduleID, sched.ID)
	}
	if run.IssueNumber != nil {
		t.Errorf("IssueNumber = %v, want nil (a scheduled run has no issue and no claim)", *run.IssueNumber)
	}

	// Identity grammar: sched-<last 6 hex of the id>-<yyyymmdd-hhmm>.
	wantLabel := ScheduleLabel(sched.ID, f.clock.Now())
	if wantLabel != "sched-"+sched.ID[len(sched.ID)-6:]+"-20260706-1216" {
		t.Fatalf("ScheduleLabel = %q — the pinned grammar drifted", wantLabel)
	}
	if run.SessionName != "proj~"+wantLabel {
		t.Errorf("session = %q, want proj~%s", run.SessionName, wantLabel)
	}
	// Branch == manual prefix + label: the manual-instance composition
	// (instance.Start does repo.ManualBranchPrefix + label), which is exactly
	// what reconcile's instanceBranch fallback derives for a non-AFK label —
	// the equality that keeps restart re-adoption naming-code-free.
	if want := f.repo.ManualBranchPrefix + wantLabel; run.Branch != want {
		t.Errorf("branch = %q, want %q (manual prefix + label, verbatim)", run.Branch, want)
	}
	if !f.branchExists(f.repo, run.Branch) {
		t.Error("scheduled run branch missing from the bare clone")
	}

	// Budget: default 30 minutes, persisted; token dies 30 min later (§3a).
	wantDeadline := f.clock.Now().Add(30 * time.Minute)
	if run.BudgetDeadline == nil || !run.BudgetDeadline.Equal(wantDeadline) {
		t.Errorf("budget deadline = %v, want %v (the scheduled default is 30m, not the AFK 120m)", run.BudgetDeadline, wantDeadline)
	}

	// SeedPrompt: ComposeSchedulePrompt output VERBATIM as the trailing spawn
	// argv positional — flows appended, <SCHEDULE> interpolated, and the
	// afk_prompt override nowhere.
	sess, live := f.runner.Session(run.SessionName)
	if !live {
		t.Fatalf("session %q not live", run.SessionName)
	}
	wantSeed := ComposeSchedulePrompt(sched.Prompt, sched.Flows, sched.Name)
	if last := sess.Argv[len(sess.Argv)-1]; last != wantSeed {
		t.Errorf("seed argv = %q, want the composed schedule prompt verbatim %q", last, wantSeed)
	}
	if !strings.Contains(wantSeed, "Investigate dependency updates for deps.") {
		t.Errorf("composed prompt did not interpolate the schedule name: %q", wantSeed)
	}
	if strings.Contains(wantSeed, override) {
		t.Error("afk_prompt override leaked into a schedule prompt")
	}

	// The firing is durable: last_fired_at stamped at launch.
	row := f.scheduleRow(sched.ID)
	if row.LastFiredAt == nil || !row.LastFiredAt.Equal(f.clock.Now()) {
		t.Errorf("last_fired_at = %v, want %v", row.LastFiredAt, f.clock.Now())
	}

	// One firing per match: an immediate second pass with no new cron match
	// spawns nothing.
	f.svc.SpawnOnce(t.Context())
	if runs := f.scheduledRuns(); len(runs) != 1 {
		t.Errorf("second pass fired again: %d scheduled runs, want 1", len(runs))
	}
}

// Matches that predate the engine's start are never observed: a schedule
// whose only recent slot passed before construction fires nothing (no
// catch-up, by construction).
func TestScheduleFiring_noCatchUpBeforeEngineStart(t *testing.T) {
	f := newFixture(t)
	f.addSchedule("daily", func(sc *store.Schedule) { sc.Cadence = "0 6 * * *" }) // 06:00 daily; engine started 12:00
	f.sightSchedules()

	f.clock.Advance(30 * time.Minute)
	f.svc.SpawnOnce(t.Context())

	if runs := f.scheduledRuns(); len(runs) != 0 {
		t.Fatalf("fired %d runs for a slot that predates the engine start, want 0", len(runs))
	}
}

// The startup missed-slot Warn: fired once, on the first pass, only for
// schedules with a last_fired_at baseline, counting the slots lost while the
// server was down.
func TestScheduleFiring_missedSlotStartupWarnOnce(t *testing.T) {
	f := newFixture(t)
	rec := recordLogs(f)
	lastFired := clockTime.Add(-72 * time.Hour) // 3 daily 06:00 slots missed before the 12:00 start
	f.addSchedule("daily", func(sc *store.Schedule) {
		sc.Cadence = "0 6 * * *"
		sc.LastFiredAt = &lastFired
	})
	f.addSchedule("virgin", func(sc *store.Schedule) { sc.Cadence = "0 6 * * *" }) // never fired — no baseline, no warn

	f.svc.SpawnOnce(t.Context())
	if n := rec.count("schedule missed firing slot(s)"); n != 1 {
		t.Fatalf("missed-slot warns after the first pass = %d, want exactly 1 (baseline schedule only)", n)
	}
	f.svc.SpawnOnce(t.Context())
	if n := rec.count("schedule missed firing slot(s)"); n != 1 {
		t.Errorf("missed-slot warns after a second pass = %d, want still 1 (the sweep is once per process)", n)
	}
}

// Skip-on-overlap: a firing that comes due while the previous run is still
// live is consumed loudly — never queued behind it — and the NEXT cron match
// fires normally once the run is gone.
func TestScheduleFiring_overlapSkipsAndConsumes(t *testing.T) {
	f := newFixture(t)
	rec := recordLogs(f)
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence

	f.clock.Advance(16 * time.Minute) // 12:16
	f.svc.SpawnOnce(t.Context())
	runs := f.scheduledRuns()
	if len(runs) != 1 {
		t.Fatalf("first firing: %d runs, want 1", len(runs))
	}

	// 12:31 — the 12:30 slot comes due while run 1 is still live: skipped,
	// loud, pending consumed.
	f.clock.Advance(15 * time.Minute)
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 1 {
		t.Fatalf("overlap pass fired anyway: %d runs, want 1", len(got))
	}
	if rec.count("schedule firing skipped: previous run still live") == 0 {
		t.Error("overlap skip was silent, want a loud Warn")
	}
	if _, held := f.svc.schedulePending[sched.ID]; held {
		t.Error("overlap skip left the firing pending (must be consumed, never deferred)")
	}

	// End run 1 (death), then a pass BEFORE the next match: nothing fires —
	// the skipped firing is gone, not deferred.
	f.runner.Kill(runs[0].SessionName)
	f.clock.Advance(time.Minute) // 12:32
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	f.clock.Advance(3 * time.Minute) // 12:35
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 1 {
		t.Fatalf("consumed firing fired late: %d runs, want still 1", len(got))
	}

	// The next cron match (12:45) fires normally.
	f.clock.Advance(11 * time.Minute) // 12:46
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 2 {
		t.Errorf("next match after the skip: %d runs, want 2", len(got))
	}
}

// At-cap firings retry: the pending memo survives the vetoed pass and the
// firing launches on a later pass with NO new cron match needed; launching
// clears the pending and stamps last_fired_at.
func TestScheduleFiring_atCapRetriesUntilLaunched(t *testing.T) {
	f := newFixture(t)
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence
	if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
		t.Fatal(err)
	}
	f.runner.AddLive("other~existing")

	f.clock.Advance(16 * time.Minute) // 12:16 — due, but at cap
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 0 {
		t.Fatalf("at-cap pass launched: %d runs, want 0", len(got))
	}
	due, held := f.svc.schedulePending[sched.ID]
	if !held {
		t.Fatal("at-cap pass dropped the pending firing (must retry)")
	}
	if want := time.Date(2026, 7, 6, 12, 15, 0, 0, time.UTC); !due.Equal(want) {
		t.Errorf("pending due = %v, want %v", due, want)
	}

	// Cap frees; the NEXT pass fires with no new cron match in its window.
	f.runner.Kill("other~existing")
	f.clock.Advance(time.Minute) // 12:17
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 1 {
		t.Fatalf("retry pass after cap freed: %d runs, want 1", len(got))
	}
	if _, held := f.svc.schedulePending[sched.ID]; held {
		t.Error("launch left the firing pending")
	}
	if row := f.scheduleRow(sched.ID); row.LastFiredAt == nil || !row.LastFiredAt.Equal(f.clock.Now()) {
		t.Errorf("last_fired_at = %v, want %v", row.LastFiredAt, f.clock.Now())
	}
}

// Supersession: a pending firing held across passes (at cap) is replaced by
// its own next due time, loudly — the stale slot is dropped, the newer one
// stands in its place.
func TestScheduleFiring_pendingSupersededByNextDueTime(t *testing.T) {
	f := newFixture(t)
	rec := recordLogs(f)
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence
	if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
		t.Fatal(err)
	}
	f.runner.AddLive("other~existing")

	f.clock.Advance(16 * time.Minute) // 12:16 — pending 12:15
	f.svc.SpawnOnce(t.Context())
	f.clock.Advance(15 * time.Minute) // 12:31 — 12:30 supersedes the held 12:15
	f.svc.SpawnOnce(t.Context())

	if n := rec.count("schedule firing superseded by its own next due time"); n != 1 {
		t.Errorf("supersession warns = %d, want exactly 1", n)
	}
	due, held := f.svc.schedulePending[sched.ID]
	if !held {
		t.Fatal("supersession dropped the pending entirely (the newer firing must stand)")
	}
	if want := time.Date(2026, 7, 6, 12, 30, 0, 0, time.UTC); !due.Equal(want) {
		t.Errorf("pending due after supersession = %v, want %v (the newest match)", due, want)
	}
	if got := f.scheduledRuns(); len(got) != 0 {
		t.Errorf("still at cap but %d runs fired", len(got))
	}
}

// Fresh multiple matches within ONE window (an every-minute cadence at a
// slow pass) collapse to the latest silently — that is normal operation, not
// a supersession anomaly.
func TestScheduleFiring_freshMultiMatchWindowCollapsesSilently(t *testing.T) {
	f := newFixture(t)
	rec := recordLogs(f)
	f.addSchedule("minutely", func(sc *store.Schedule) { sc.Cadence = "* * * * *" })
	f.sightSchedules() // 12:00 — arms the cadence

	f.clock.Advance(5 * time.Minute) // five matches in one window, no pending held
	f.svc.SpawnOnce(t.Context())

	if got := f.scheduledRuns(); len(got) != 1 {
		t.Fatalf("fired %d runs for one window, want 1 (the latest match only)", len(got))
	}
	if n := rec.count("superseded"); n != 0 {
		t.Errorf("fresh multi-match window warned %d times, want 0 (collapse is silent)", n)
	}
}

// Disabled, paused, and deleted schedules produce no candidates and their
// memo entries are cleaned up — no leak, no stale firing later.
func TestScheduleFiring_goneSchedulesCleanUpPending(t *testing.T) {
	cases := []struct {
		name string
		gone func(f *fixture, id string)
	}{
		{"disabled", func(f *fixture, id string) {
			if _, err := f.st.UpdateSchedule(f.t.Context(), id, store.ScheduleUpdate{Enabled: store.Set(false)}); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"paused", func(f *fixture, id string) {
			if _, err := f.st.SetSchedulePaused(f.t.Context(), id, true); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"deleted", func(f *fixture, id string) {
			if err := f.st.DeleteSchedule(f.t.Context(), id); err != nil {
				f.t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			sched := f.addSchedule("deps", nil)
			f.sightSchedules() // 12:00 — arms the cadence
			if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
				t.Fatal(err)
			}
			f.runner.AddLive("other~existing")

			f.clock.Advance(16 * time.Minute)
			f.svc.SpawnOnce(t.Context()) // pending held at cap
			if _, held := f.svc.schedulePending[sched.ID]; !held {
				t.Fatal("fixture: expected a held pending firing")
			}

			tc.gone(f, sched.ID)
			f.runner.Kill("other~existing") // cap frees — a stale pending would fire now
			f.clock.Advance(time.Minute)
			f.svc.SpawnOnce(t.Context())

			if got := f.scheduledRuns(); len(got) != 0 {
				t.Errorf("%s schedule fired: %d runs, want 0", tc.name, len(got))
			}
			if _, held := f.svc.schedulePending[sched.ID]; held {
				t.Errorf("%s schedule leaked its pending memo entry", tc.name)
			}
			if _, held := f.svc.scheduleChecked[sched.ID]; held {
				t.Errorf("%s schedule leaked its high-water memo entry", tc.name)
			}
		})
	}
}

// First sighting arms the cadence forward-only: a Schedule created after the
// engine has been running for a while never fires for a slot that predates
// its sighting, and fires normally at its next real match.
func TestScheduleFiring_firstSightingArmsForwardOnly(t *testing.T) {
	f := newFixture(t)
	f.svc.SpawnOnce(t.Context()) // the engine has been passing since 12:00

	f.clock.Advance(16 * time.Minute) // 12:16 — the 12:15 slot has already passed
	f.addSchedule("deps", nil)
	f.svc.SpawnOnce(t.Context()) // first sighting: seeds the mark, walks nothing
	if got := f.scheduledRuns(); len(got) != 0 {
		t.Fatalf("first sighting fired %d runs for a slot predating it, want 0", len(got))
	}

	f.clock.Advance(5 * time.Minute) // 12:21 — still before the next match
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 0 {
		t.Fatalf("pre-sighting slot fired late: %d runs, want 0", len(got))
	}

	f.clock.Advance(10 * time.Minute) // 12:31 — crosses 12:30, the first real match
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 1 {
		t.Fatalf("next real match fired %d runs, want 1", len(got))
	}
}

// Disable→enable across a consumed slot: re-enabling re-sights the schedule
// (the cleanup dropped its memo while disabled), and the already-consumed
// slot must not re-fire — only the next real match does.
func TestScheduleFiring_disableEnableDoesNotRefireConsumedSlot(t *testing.T) {
	f := newFixture(t)
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence

	f.clock.Advance(16 * time.Minute) // 12:16 — consumes the 12:15 slot
	f.svc.SpawnOnce(t.Context())
	runs := f.scheduledRuns()
	if len(runs) != 1 {
		t.Fatalf("first firing: %d runs, want 1", len(runs))
	}
	// End the run so a re-fire could not hide behind the overlap skip.
	f.runner.Kill(runs[0].SessionName)
	f.clock.Advance(time.Minute) // 12:17
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	if _, err := f.st.UpdateSchedule(t.Context(), sched.ID, store.ScheduleUpdate{Enabled: store.Set(false)}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute) // 12:18 — a pass while disabled drops the memo
	f.svc.SpawnOnce(t.Context())

	if _, err := f.st.UpdateSchedule(t.Context(), sched.ID, store.ScheduleUpdate{Enabled: store.Set(true)}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute) // 12:19 — re-sighting pass
	f.svc.SpawnOnce(t.Context())
	f.clock.Advance(time.Minute) // 12:20 — first armed pass after re-enable
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 1 {
		t.Fatalf("re-enable re-fired a consumed slot: %d runs, want still 1", len(got))
	}

	f.clock.Advance(11 * time.Minute) // 12:31 — crosses 12:30, the next real match
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 2 {
		t.Errorf("next match after re-enable: %d runs, want 2", len(got))
	}
}

// Pause → ReenableSchedule → immediate pass (the re-enable endpoint's kick):
// nothing fires until the next real cron match.
func TestScheduleFiring_reenableFiresNothingUntilNextMatch(t *testing.T) {
	f := newFixture(t)
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence

	f.clock.Advance(16 * time.Minute) // 12:16 — consumes the 12:15 slot
	f.svc.SpawnOnce(t.Context())
	runs := f.scheduledRuns()
	if len(runs) != 1 {
		t.Fatalf("first firing: %d runs, want 1", len(runs))
	}
	f.runner.Kill(runs[0].SessionName)
	f.clock.Advance(time.Minute) // 12:17
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	if _, err := f.st.SetSchedulePaused(t.Context(), sched.ID, true); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute) // 12:18 — a pass while paused drops the memo
	f.svc.SpawnOnce(t.Context())

	if _, err := f.st.ReenableSchedule(t.Context(), sched.ID); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute) // 12:19 — the immediate re-enable kick
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 1 {
		t.Fatalf("re-enable kick fired for a stale slot: %d runs, want still 1", len(got))
	}

	f.clock.Advance(12 * time.Minute) // 12:31 — crosses 12:30
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 2 {
		t.Errorf("next match after re-enable: %d runs, want 2", len(got))
	}
}

// A clock step-back (an NTP correction) must not re-open consumed windows:
// the high-water mark stays put while now is at/behind it, and the walk
// resumes forward-only once the clock passes it again.
func TestScheduleFiring_clockStepBackNeverReopensConsumedSlots(t *testing.T) {
	f := newFixture(t)
	f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence

	f.clock.Advance(16 * time.Minute) // 12:16 — consumes the 12:15 slot
	f.svc.SpawnOnce(t.Context())
	runs := f.scheduledRuns()
	if len(runs) != 1 {
		t.Fatalf("first firing: %d runs, want 1", len(runs))
	}
	f.runner.Kill(runs[0].SessionName)
	f.clock.Advance(time.Minute) // 12:17
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	f.clock.Advance(-10 * time.Minute) // the clock steps back to 12:07, behind the consumed 12:15
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 1 {
		t.Fatalf("step-back pass fired: %d runs, want still 1 (a consumed window must stay closed)", len(got))
	}
	f.svc.SpawnOnce(t.Context()) // and a second pass at the same skewed instant
	if got := f.scheduledRuns(); len(got) != 1 {
		t.Fatalf("repeat step-back pass fired: %d runs, want still 1", len(got))
	}

	// Forward past a NEW slot: the walk resumes from the untouched mark and
	// re-crossing the already-consumed 12:15 fires nothing extra.
	f.clock.Advance(24 * time.Minute) // 12:31 — crosses 12:30
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 2 {
		t.Fatalf("recovery pass: %d runs total, want 2 (exactly one new firing)", len(got))
	}
}

// flakyListRunner wraps the fixture's fake runner so a test can fail List
// from the Nth call on — the transient tmux read the locked launch must
// survive without consuming the firing.
type flakyListRunner struct {
	tmuxx.SessionRunner
	mu       sync.Mutex
	failFrom int // 1-based List call number to start failing at; 0 = never
	calls    int
}

func (r *flakyListRunner) setFailFrom(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failFrom, r.calls = n, 0
}

func (r *flakyListRunner) List(ctx context.Context) ([]string, error) {
	r.mu.Lock()
	r.calls++
	fail := r.failFrom > 0 && r.calls >= r.failFrom
	r.mu.Unlock()
	if fail {
		return nil, errors.New("tmux flaked")
	}
	return r.SessionRunner.List(ctx)
}

// A transient session-listing failure inside the locked launch keeps the
// pending firing, and a later pass fires it with no new cron match needed —
// one flaky read must not consume a weekly firing.
func TestScheduleFiring_transientListErrorKeepsPendingForRetry(t *testing.T) {
	var flaky *flakyListRunner
	f := newFixtureWrapped(t, "afk/<N>", func(fake *tmuxx.Fake) tmuxx.SessionRunner {
		flaky = &flakyListRunner{SessionRunner: fake}
		return flaky
	})
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence

	f.clock.Advance(16 * time.Minute) // 12:16 — due
	// The pass's own snapshot read succeeds; the locked launch's fresh
	// liveness read fails.
	flaky.setFailFrom(2)
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 0 {
		t.Fatalf("flaky pass launched %d runs, want 0", len(got))
	}
	if _, held := f.svc.schedulePending[sched.ID]; !held {
		t.Fatal("transient List error consumed the pending firing (must be kept for retry)")
	}

	flaky.setFailFrom(0)
	f.clock.Advance(time.Minute) // 12:17 — no new cron match in the window
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 1 {
		t.Fatalf("retry pass after the transient error: %d runs, want 1", len(got))
	}
	if _, held := f.svc.schedulePending[sched.ID]; held {
		t.Error("launch left the firing pending")
	}
}

// A hard launch error consumes the firing: retrying a deterministic failure
// every tick is auto-retry-shaped runaway — the next cron match tries again.
func TestScheduleFiring_hardLaunchErrorConsumesFiring(t *testing.T) {
	f := newFixture(t)
	rec := recordLogs(f)
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence

	f.clock.Advance(16 * time.Minute) // 12:16 — due; the spawn will fail hard
	f.runner.FailStart("proj~"+ScheduleLabel(sched.ID, f.clock.Now()), errors.New("new-session: server exited"))
	f.svc.SpawnOnce(t.Context())

	if got := f.scheduledRuns(); len(got) != 0 {
		t.Fatalf("failed launch left %d runs, want 0", len(got))
	}
	if _, held := f.svc.schedulePending[sched.ID]; held {
		t.Fatal("hard launch error kept the pending firing (must be consumed)")
	}
	if rec.count("schedule launch failed; firing consumed") != 1 {
		t.Error("hard launch failure was not logged as consumed")
	}

	// Consumed means no retry: the next pass without a new match stays idle.
	f.clock.Advance(time.Minute) // 12:17
	f.svc.SpawnOnce(t.Context())
	if got := f.scheduledRuns(); len(got) != 0 {
		t.Errorf("consumed firing retried anyway: %d runs, want 0", len(got))
	}
}

// --- launchScheduled (locked path) -------------------------------------------

// The locked launch re-checks everything under the engine lock: a schedule
// that vanished, was disabled/paused, or grew an overlap since the gather
// consumes the firing as skipped; at-cap answers at-cap and keeps it (retry).
func TestLaunchScheduled_lockedGuards(t *testing.T) {
	t.Run("vanished", func(t *testing.T) {
		f := newFixture(t)
		outcome, consumed := f.svc.launchScheduled(t.Context(), "sched_missing")
		if outcome != spawnSkipped || !consumed {
			t.Errorf("outcome = %v consumed=%v, want spawnSkipped consumed", outcome, consumed)
		}
	})
	t.Run("paused", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("deps", nil)
		if _, err := f.st.SetSchedulePaused(t.Context(), sched.ID, true); err != nil {
			t.Fatal(err)
		}
		outcome, consumed := f.svc.launchScheduled(t.Context(), sched.ID)
		if outcome != spawnSkipped || !consumed {
			t.Errorf("outcome = %v consumed=%v, want spawnSkipped consumed", outcome, consumed)
		}
		if got := f.scheduledRuns(); len(got) != 0 {
			t.Errorf("paused schedule launched %d runs", len(got))
		}
	})
	t.Run("overlap under the lock", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("deps", nil)
		if outcome, consumed := f.svc.launchScheduled(t.Context(), sched.ID); outcome != spawnSpawned || !consumed {
			t.Fatalf("first launch = %v consumed=%v, want spawnSpawned consumed", outcome, consumed)
		}
		outcome, consumed := f.svc.launchScheduled(t.Context(), sched.ID)
		if outcome != spawnSkipped || !consumed {
			t.Errorf("second launch with a live run = %v consumed=%v, want spawnSkipped consumed (the lock is the overlap authority)", outcome, consumed)
		}
		if got := f.scheduledRuns(); len(got) != 1 {
			t.Errorf("overlap launched anyway: %d runs, want 1", len(got))
		}
	})
	t.Run("at cap", func(t *testing.T) {
		f := newFixture(t)
		sched := f.addSchedule("deps", nil)
		if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
			t.Fatal(err)
		}
		f.runner.AddLive("other~existing")
		outcome, consumed := f.svc.launchScheduled(t.Context(), sched.ID)
		if outcome != spawnAtCap || consumed {
			t.Errorf("outcome = %v consumed=%v, want spawnAtCap kept (retries, never consumed)", outcome, consumed)
		}
	})
}

// A schedule whose brief composes to nothing — here, an empty prompt with
// only a flow key this binary's catalog does not know — must not launch: the
// firing is consumed with a loud error instead of burning a full budget on
// an empty prompt and classifying success forever.
func TestLaunchScheduled_emptyComposedPromptConsumesFiring(t *testing.T) {
	f := newFixture(t)
	rec := recordLogs(f)
	sched := f.addSchedule("ghostly", func(sc *store.Schedule) {
		sc.Prompt = ""
		sc.Flows = []string{"retired-flow"} // survives the store round trip, unknown to the catalog
	})
	f.sightSchedules() // 12:00 — arms the cadence

	f.clock.Advance(16 * time.Minute) // 12:16 — due
	f.svc.SpawnOnce(t.Context())

	if got := f.scheduledRuns(); len(got) != 0 {
		t.Fatalf("empty-brief firing launched %d runs, want 0", len(got))
	}
	if rec.count("schedule composed an empty prompt") != 1 {
		t.Error("empty composed prompt was not logged as an error")
	}
	if _, held := f.svc.schedulePending[sched.ID]; held {
		t.Fatal("empty-brief firing kept its pending (must be consumed)")
	}

	// Consumed, not retried: a pass with no new match stays quiet.
	f.clock.Advance(time.Minute) // 12:17
	f.svc.SpawnOnce(t.Context())
	if n := rec.count("schedule composed an empty prompt"); n != 1 {
		t.Errorf("empty-brief error logged %d times, want 1 (the firing was consumed)", n)
	}
}

// The per-Schedule budget override replaces the 30-minute default.
func TestLaunchScheduled_budgetOverride(t *testing.T) {
	f := newFixture(t)
	mins := 45
	sched := f.addSchedule("deps", func(sc *store.Schedule) { sc.BudgetMinutes = &mins })

	if outcome, _ := f.svc.launchScheduled(t.Context(), sched.ID); outcome != spawnSpawned {
		t.Fatalf("launch = %v, want spawnSpawned", outcome)
	}
	runs := f.scheduledRuns()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	want := f.clock.Now().Add(45 * time.Minute)
	if runs[0].BudgetDeadline == nil || !runs[0].BudgetDeadline.Equal(want) {
		t.Errorf("budget deadline = %v, want %v (per-Schedule override)", runs[0].BudgetDeadline, want)
	}
}

// Per-Schedule knob overrides ride the topmost skip-layer rung end to end:
// a catalog-present model override wins; stale model/effort/provider
// overrides degrade the firing to the inherited chain (the seeded defaults),
// never break it.
func TestLaunchScheduled_knobOverridesSkipLayer(t *testing.T) {
	t.Run("override wins", func(t *testing.T) {
		f := newFixture(t)
		model, effort := "sonnet", "high"
		sched := f.addSchedule("deps", func(sc *store.Schedule) {
			sc.Model, sc.Effort = &model, &effort
		})
		if outcome, _ := f.svc.launchScheduled(t.Context(), sched.ID); outcome != spawnSpawned {
			t.Fatalf("launch = %v, want spawnSpawned", outcome)
		}
		run := f.scheduledRuns()[0]
		if run.Model != "sonnet" || run.Effort != "high" {
			t.Errorf("model/effort = %q/%q, want sonnet/high (the schedule override wins)", run.Model, run.Effort)
		}
	})
	t.Run("stale overrides fall through", func(t *testing.T) {
		f := newFixture(t)
		ghost := "ghost"
		sched := f.addSchedule("deps", func(sc *store.Schedule) {
			sc.Model, sc.Effort, sc.Provider = &ghost, &ghost, &ghost
		})
		if outcome, _ := f.svc.launchScheduled(t.Context(), sched.ID); outcome != spawnSpawned {
			t.Fatalf("launch = %v, want spawnSpawned (stale overrides must degrade, not break the firing)", outcome)
		}
		run := f.scheduledRuns()[0]
		if run.Provider != "claude-code" {
			t.Errorf("provider = %q, want claude-code (stale override skip-layers to the chain)", run.Provider)
		}
		if run.Model != "opus[1m]" || run.Effort != "max" {
			t.Errorf("model/effort = %q/%q, want the seeded chain opus[1m]/max", run.Model, run.Effort)
		}
	})
}

// --- reaper -----------------------------------------------------------------

// Budget expiry IS completion: an alive scheduled run at its deadline reaps
// as success with ZERO pull reads, goes through the normal teardown, resets
// the per-Schedule strike counter, and fires no done-signal push.
func TestReapScheduled_budgetExpiryReapsAsSuccess(t *testing.T) {
	f := newFixture(t)
	cap := &captureNotifier{}
	f.svc.notify = cap.notify
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence
	for range 2 {      // a success must reset an existing streak
		if _, err := f.st.IncrementScheduleFailures(t.Context(), sched.ID); err != nil {
			t.Fatal(err)
		}
	}

	f.clock.Advance(16 * time.Minute)
	f.svc.SpawnOnce(t.Context())
	runs := f.scheduledRuns()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	run := runs[0]

	// At the inclusive deadline (12:16 + 30m), still alive: success.
	f.clock.Advance(30 * time.Minute)
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	got := f.runRow(run.ID)
	if got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success (budget expiry is a scheduled run's completion)", got.Outcome)
	}
	if got.FailureReason != nil {
		t.Errorf("failure_reason = %q, want none (a completion is not a timeout-failure)", *got.FailureReason)
	}
	if n := f.trk.pullsForHeadCallCount(); n != 0 {
		t.Errorf("reaper made %d PullsForHead calls for a scheduled run, want 0 (no done-signal, no forge read)", n)
	}
	if n := f.trk.pullsCallCount(); n != 0 {
		t.Errorf("reaper listed pulls %d times, want 0", n)
	}
	// Teardown ran: session stopped, clean worktree reclaimed.
	if _, live := f.runner.Session(run.SessionName); live {
		t.Error("session still live after the success reap")
	}
	if dirExists(run.WorktreePath) {
		t.Error("clean worktree not removed on the success reap")
	}
	// Per-Schedule counter reset; repo counter untouched.
	if row := f.scheduleRow(sched.ID); row.ConsecutiveFailures != 0 || row.Paused {
		t.Errorf("schedule failures/paused = %d/%v, want 0/false (completion resets the streak)", row.ConsecutiveFailures, row.Paused)
	}
	if n := f.failures(f.repo); n != 0 {
		t.Errorf("repo failures = %d, want 0 (scheduled outcomes never move the repo counter)", n)
	}
	// Silent: completions cost zero notifications.
	if cap.count() != 0 {
		t.Errorf("success reap sent %d notifications, want 0", cap.count())
	}
}

// A session death before budget expiry is a failure that strikes ONLY the
// Schedule; the third consecutive death pauses it, fires the one push, and
// the repo's AFK counter never moves. A paused Schedule stops firing.
func TestReapScheduled_threeDeathsPauseTheSchedule(t *testing.T) {
	f := newFixture(t)
	cap := &captureNotifier{}
	f.svc.notify = cap.notify
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence

	dieOnce := func(wantFailures int) {
		t.Helper()
		f.clock.Advance(16 * time.Minute) // crosses at least one :00/:15/:30/:45 slot
		f.svc.SpawnOnce(t.Context())
		active, err := f.st.ActiveRuns(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(active) != 1 {
			t.Fatalf("active runs = %d, want 1 fresh firing", len(active))
		}
		f.runner.Kill(active[0].SessionName)
		f.clock.Advance(time.Minute)
		f.svc.ReapOnce(t.Context(), f.clock.Now())
		if got := f.runRow(active[0].ID); got.Outcome != store.RunOutcomeDeath {
			t.Fatalf("outcome = %q, want death (died before budget expiry)", got.Outcome)
		}
		if row := f.scheduleRow(sched.ID); row.ConsecutiveFailures != wantFailures {
			t.Fatalf("schedule failures = %d, want %d", row.ConsecutiveFailures, wantFailures)
		}
		if n := f.failures(f.repo); n != 0 {
			t.Fatalf("repo failures = %d, want 0 (a scheduled death must not strike the repo)", n)
		}
	}

	dieOnce(1)
	if cap.count() != 0 {
		t.Fatalf("notifications after 1 death = %d, want 0", cap.count())
	}
	dieOnce(2)
	if cap.count() != 0 {
		t.Fatalf("notifications after 2 deaths = %d, want 0", cap.count())
	}
	dieOnce(3)

	row := f.scheduleRow(sched.ID)
	if !row.Paused {
		t.Fatal("third consecutive death did not pause the schedule")
	}
	if cap.count() != 1 {
		t.Fatalf("notifications after the pause = %d, want exactly 1", cap.count())
	}
	n := cap.last()
	if n.Title != "Schedule deps paused after 3 failures" {
		t.Errorf("push title = %q", n.Title)
	}
	if n.Tag != sched.ID {
		t.Errorf("push tag = %q, want the schedule ID %q", n.Tag, sched.ID)
	}
	if n.Route != "/repos/"+f.repo.ID+"/settings/schedules" {
		t.Errorf("push route = %q, want the repo's schedules settings path", n.Route)
	}

	// Paused schedules never fire: the next slots pass silently.
	f.clock.Advance(31 * time.Minute)
	f.svc.SpawnOnce(t.Context())
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Errorf("paused schedule fired: %d active runs, want 0", len(active))
	}
}

// A budget-expiry completion between deaths resets the streak — three
// non-consecutive deaths never pause.
func TestReapScheduled_successBetweenDeathsResetsStreak(t *testing.T) {
	f := newFixture(t)
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence

	fire := func() store.Run {
		t.Helper()
		f.clock.Advance(16 * time.Minute)
		f.svc.SpawnOnce(t.Context())
		active, err := f.st.ActiveRuns(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(active) != 1 {
			t.Fatalf("active runs = %d, want 1", len(active))
		}
		return active[0]
	}

	// Two deaths.
	for i := 1; i <= 2; i++ {
		run := fire()
		f.runner.Kill(run.SessionName)
		f.clock.Advance(time.Minute)
		f.svc.ReapOnce(t.Context(), f.clock.Now())
		if row := f.scheduleRow(sched.ID); row.ConsecutiveFailures != i {
			t.Fatalf("failures = %d, want %d", row.ConsecutiveFailures, i)
		}
	}
	// A completion: budget expiry with the session alive.
	fire()
	f.clock.Advance(30 * time.Minute)
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if row := f.scheduleRow(sched.ID); row.ConsecutiveFailures != 0 {
		t.Fatalf("failures after the completion = %d, want 0 (reset)", row.ConsecutiveFailures)
	}
	// A third death is a FIRST strike again — no pause.
	run := fire()
	f.runner.Kill(run.SessionName)
	f.clock.Advance(time.Minute)
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	row := f.scheduleRow(sched.ID)
	if row.ConsecutiveFailures != 1 || row.Paused {
		t.Errorf("failures/paused = %d/%v, want 1/false (the streak restarted after the reset)", row.ConsecutiveFailures, row.Paused)
	}
}

// A death observed at/after the budget deadline is a completion, not a
// strike: past the deadline the budget clock has already terminated the run,
// so a session that reached its budget and died before the reaper's next
// look reaps success with zero strike movement.
func TestReapScheduled_deathAtOrPastDeadlineIsCompletion(t *testing.T) {
	f := newFixture(t)
	cap := &captureNotifier{}
	f.svc.notify = cap.notify
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence

	f.clock.Advance(16 * time.Minute) // 12:16 — fires; deadline 12:46
	f.svc.SpawnOnce(t.Context())
	runs := f.scheduledRuns()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	run := runs[0]

	// The session dies unobserved; the reaper's next look lands exactly at
	// the inclusive deadline.
	f.runner.Kill(run.SessionName)
	f.clock.Advance(30 * time.Minute) // 12:46
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	got := f.runRow(run.ID)
	if got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("outcome = %q, want success (death at/after the deadline is a completion)", got.Outcome)
	}
	if got.FailureReason != nil {
		t.Errorf("failure_reason = %q, want none", *got.FailureReason)
	}
	if row := f.scheduleRow(sched.ID); row.ConsecutiveFailures != 0 || row.Paused {
		t.Errorf("schedule failures/paused = %d/%v, want 0/false (zero strike movement)", row.ConsecutiveFailures, row.Paused)
	}
	if n := f.failures(f.repo); n != 0 {
		t.Errorf("repo failures = %d, want 0", n)
	}
	if cap.count() != 0 {
		t.Errorf("completion sent %d notifications, want 0", cap.count())
	}
}

// A broken tracker resolution must not strand a scheduled run: the kind
// makes zero forge reads by design, so its reap proceeds with no tracker at
// all, while the repo's other kinds keep waiting for the tracker to heal.
func TestReapScheduled_reapsWithoutTrackerResolution(t *testing.T) {
	f := newFixture(t)
	rec := recordLogs(f)
	cap := &captureNotifier{}
	f.svc.notify = cap.notify // a stray notification would read the nil tracker and panic
	f.trk.setReady(7)
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence

	afkRun, err := f.svc.StartManualAFK(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("StartManualAFK: %v", err)
	}
	f.clock.Advance(16 * time.Minute) // 12:16 — the schedule fires; deadline 12:46
	f.svc.SpawnOnce(t.Context())
	schedRuns := f.scheduledRuns()
	if len(schedRuns) != 1 {
		t.Fatalf("scheduled runs = %d, want 1", len(schedRuns))
	}

	// The forge credential breaks: tracker resolution fails for the repo.
	// The dead AFK session makes a mistaken classification visible.
	f.trackers.unbind(f.repo.ID)
	f.runner.Kill(afkRun.SessionName)

	f.clock.Advance(30 * time.Minute) // 12:46 — past the scheduled deadline
	f.svc.ReapOnce(t.Context(), f.clock.Now())

	if got := f.runRow(schedRuns[0].ID); got.Outcome != store.RunOutcomeSuccess {
		t.Fatalf("scheduled outcome = %q, want success (a broken tracker must not strand the kind)", got.Outcome)
	}
	if got := f.runRow(afkRun.ID); got.Outcome != store.RunOutcomeActive {
		t.Errorf("afk outcome = %q, want still active (skip until the tracker heals)", got.Outcome)
	}
	if rec.count("afk watcher: tracker") == 0 {
		t.Error("tracker resolution failure was silent, want the warn")
	}
	if row := f.scheduleRow(sched.ID); row.ConsecutiveFailures != 0 || row.Paused {
		t.Errorf("schedule failures/paused = %d/%v, want 0/false", row.ConsecutiveFailures, row.Paused)
	}
	if cap.count() != 0 {
		t.Errorf("tracker-less reap sent %d notifications, want 0", cap.count())
	}

	// Once the tracker heals, the AFK run classifies normally.
	f.trackers.set(f.repo.ID, f.trk)
	f.clock.Advance(time.Minute)
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if got := f.runRow(afkRun.ID); got.Outcome != store.RunOutcomeDeath {
		t.Errorf("afk outcome after the tracker healed = %q, want death", got.Outcome)
	}
}

// repoChangedCount drains a bus subscription (cancel closes the channel) and
// counts the repo.changed events it captured.
func repoChangedCount(evts <-chan events.Event, cancel func()) int {
	cancel()
	n := 0
	for ev := range evts {
		if ev.Type == EventRepoChanged {
			n++
		}
	}
	return n
}

// A success reap publishes repo.changed only when it actually cleared a
// nonzero streak — the settings UI's strike badge refetch rides the real
// transition, and a healthy schedule's completions stay silent on the bus.
func TestReapScheduled_successResetPublishesOnRealTransition(t *testing.T) {
	f := newFixture(t)
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence

	fire := func() store.Run {
		t.Helper()
		f.clock.Advance(16 * time.Minute)
		f.svc.SpawnOnce(t.Context())
		active, err := f.st.ActiveRuns(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(active) != 1 {
			t.Fatalf("active runs = %d, want 1", len(active))
		}
		return active[0]
	}

	// A 2→0 reset is a real transition: exactly one repo.changed.
	for range 2 {
		if _, err := f.st.IncrementScheduleFailures(t.Context(), sched.ID); err != nil {
			t.Fatal(err)
		}
	}
	fire()
	f.clock.Advance(30 * time.Minute)
	evts, cancel := f.bus.Subscribe(t.Context())
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if n := repoChangedCount(evts, cancel); n != 1 {
		t.Fatalf("repo.changed on a 2→0 reset = %d, want 1", n)
	}
	if row := f.scheduleRow(sched.ID); row.ConsecutiveFailures != 0 {
		t.Fatalf("failures after the reset = %d, want 0", row.ConsecutiveFailures)
	}

	// A 0→0 reset is not a transition: no repo.changed.
	fire()
	f.clock.Advance(30 * time.Minute)
	evts, cancel = f.bus.Subscribe(t.Context())
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if n := repoChangedCount(evts, cancel); n != 0 {
		t.Errorf("repo.changed on a 0→0 reset = %d, want 0", n)
	}
}

// A neutral Stop is neither success nor failure for a Schedule too: outcome
// stopped, and the per-Schedule counter moves in NEITHER direction.
func TestStopAFK_scheduledIsNeutral(t *testing.T) {
	f := newFixture(t)
	sched := f.addSchedule("deps", nil)
	f.sightSchedules() // 12:00 — arms the cadence
	for range 2 {
		if _, err := f.st.IncrementScheduleFailures(t.Context(), sched.ID); err != nil {
			t.Fatal(err)
		}
	}

	f.clock.Advance(16 * time.Minute)
	f.svc.SpawnOnce(t.Context())
	runs := f.scheduledRuns()
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	if err := f.svc.StopAFK(t.Context(), runs[0].SessionName); err != nil {
		t.Fatalf("StopAFK: %v", err)
	}
	if got := f.runRow(runs[0].ID); got.Outcome != store.RunOutcomeStopped {
		t.Fatalf("outcome = %q, want stopped", got.Outcome)
	}
	row := f.scheduleRow(sched.ID)
	if row.ConsecutiveFailures != 2 || row.Paused {
		t.Errorf("schedule failures/paused = %d/%v, want 2/false (a neutral Stop touches no counter)", row.ConsecutiveFailures, row.Paused)
	}
	// And the reaper never reclassifies a stopped run.
	f.clock.Advance(45 * time.Minute)
	f.svc.ReapOnce(t.Context(), f.clock.Now())
	if got := f.runRow(runs[0].ID); got.Outcome != store.RunOutcomeStopped {
		t.Errorf("outcome after a later tick = %q, want still stopped", got.Outcome)
	}
	if row := f.scheduleRow(sched.ID); row.ConsecutiveFailures != 2 {
		t.Errorf("failures after a later tick = %d, want still 2", row.ConsecutiveFailures)
	}
}
