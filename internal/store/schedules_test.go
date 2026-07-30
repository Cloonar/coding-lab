package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pressly/goose/v3"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/migrations"
)

// schedClock is the fixed instant the Schedule fixtures are stamped with;
// tests needing a second or third instant derive from it.
var schedClock = time.Date(2026, 7, 30, 6, 0, 0, 0, time.UTC)

// fixtureSchedule creates an enabled Schedule with every nullable left NULL —
// the inherit-everything shape the UI's default form produces.
func fixtureSchedule(t *testing.T, st *Store, repoID, name, cadence string) Schedule {
	t.Helper()
	sc, err := st.CreateSchedule(context.Background(), Schedule{
		ID: ids.NewID("sched"), RepoID: repoID, Name: name, Cadence: cadence,
		Prompt: "investigate dependency updates", Enabled: true,
		CreatedAt: schedClock, UpdatedAt: schedClock,
	})
	if err != nil {
		t.Fatalf("CreateSchedule %q: %v", name, err)
	}
	return sc
}

// TestScheduleCRUD round-trips every column in both directions: one Schedule
// with every optional override set, one with all of them NULL (the inherit
// shape), then reads them back through both the by-id and the per-repo
// listing, and finally deletes.
func TestScheduleCRUD(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		repo := afkFixtureRepo(t, st, "proj")

		fired := schedClock.Add(-24 * time.Hour)
		full := Schedule{
			ID: ids.NewID("sched"), RepoID: repo.ID, Name: "weekly-audit",
			Cadence: "0 6 * * 1", Prompt: "security audit",
			Flows:   []string{"autolander", "human-triage"},
			Enabled: true, BudgetMinutes: intPtr(90),
			Model: strPtr("opus[1m]"), Effort: strPtr("max"),
			Provider: strPtr("claude-code"), ConsecutiveFailures: 2,
			Paused: true, LastFiredAt: &fired,
			// Sub-millisecond input must normalize exactly like every other
			// create method (design §2 stored precision).
			CreatedAt: schedClock.Add(123_456 * time.Nanosecond),
			UpdatedAt: schedClock.Add(123_456 * time.Nanosecond),
		}
		created, err := st.CreateSchedule(ctx, full)
		if err != nil {
			t.Fatalf("CreateSchedule full: %v", err)
		}
		if want := storedTime(full.CreatedAt); !created.CreatedAt.Equal(want) || !created.UpdatedAt.Equal(want) {
			t.Errorf("timestamps = %v/%v, want both %v", created.CreatedAt, created.UpdatedAt, want)
		}

		bare := fixtureSchedule(t, st, repo.ID, "daily-sweep", "30 5 * * *")

		got, err := st.ScheduleByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("ScheduleByID: %v", err)
		}
		if got.RepoID != repo.ID || got.Name != "weekly-audit" || got.Cadence != "0 6 * * 1" {
			t.Errorf("identity = %+v", got)
		}
		if got.Prompt != "security audit" {
			t.Errorf("prompt = %q", got.Prompt)
		}
		if len(got.Flows) != 2 || got.Flows[0] != "autolander" || got.Flows[1] != "human-triage" {
			t.Errorf("flows = %v, want [autolander human-triage]", got.Flows)
		}
		if !got.Enabled || !got.Paused || got.ConsecutiveFailures != 2 {
			t.Errorf("enabled/paused/failures = %v/%v/%d, want true/true/2", got.Enabled, got.Paused, got.ConsecutiveFailures)
		}
		if got.BudgetMinutes == nil || *got.BudgetMinutes != 90 {
			t.Errorf("budget = %v, want 90", got.BudgetMinutes)
		}
		if got.Model == nil || *got.Model != "opus[1m]" ||
			got.Effort == nil || *got.Effort != "max" ||
			got.Provider == nil || *got.Provider != "claude-code" {
			t.Errorf("overrides = %v/%v/%v, want opus[1m]/max/claude-code", got.Model, got.Effort, got.Provider)
		}
		if got.LastFiredAt == nil || !got.LastFiredAt.Equal(storedTime(fired)) {
			t.Errorf("last fired = %v, want %v", got.LastFiredAt, storedTime(fired))
		}

		// The inherit shape: every optional column reads back nil, never a
		// zero value standing in for "unset".
		got, err = st.ScheduleByID(ctx, bare.ID)
		if err != nil {
			t.Fatalf("ScheduleByID bare: %v", err)
		}
		if got.BudgetMinutes != nil || got.Model != nil || got.Effort != nil || got.Provider != nil {
			t.Errorf("bare overrides = %v/%v/%v/%v, want all nil (inherit)",
				got.BudgetMinutes, got.Model, got.Effort, got.Provider)
		}
		if got.LastFiredAt != nil {
			t.Errorf("bare last fired = %v, want nil (never fired)", got.LastFiredAt)
		}
		if got.Paused || got.ConsecutiveFailures != 0 {
			t.Errorf("bare paused/failures = %v/%d, want false/0", got.Paused, got.ConsecutiveFailures)
		}

		// Listing is name-ordered and repo-scoped.
		list, err := st.SchedulesByRepo(ctx, repo.ID)
		if err != nil {
			t.Fatalf("SchedulesByRepo: %v", err)
		}
		if len(list) != 2 || list[0].Name != "daily-sweep" || list[1].Name != "weekly-audit" {
			t.Fatalf("listing = %v, want [daily-sweep weekly-audit]", names(list))
		}
		other := afkFixtureRepo(t, st, "other")
		if list, err = st.SchedulesByRepo(ctx, other.ID); err != nil || len(list) != 0 {
			t.Errorf("other repo listing = %v (err %v), want empty", names(list), err)
		}

		if _, err := st.ScheduleByID(ctx, "sched_00000000000000000000000000000000"); !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown id err = %v, want ErrNotFound", err)
		}

		// Delete: gone afterwards, and deleting twice is ErrNotFound.
		if err := st.DeleteSchedule(ctx, bare.ID); err != nil {
			t.Fatalf("DeleteSchedule: %v", err)
		}
		if _, err := st.ScheduleByID(ctx, bare.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("ScheduleByID after delete = %v, want ErrNotFound", err)
		}
		if err := st.DeleteSchedule(ctx, bare.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("second DeleteSchedule = %v, want ErrNotFound", err)
		}
	})
}

// TestScheduleFlowsRoundTrip pins the JSON-in-TEXT flow selection: zero flows
// is a legal pure-prompt Schedule that reads back as an empty (never nil)
// slice, a multi-flow selection keeps its stored order, and an update
// replaces the whole array.
func TestScheduleFlowsRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		repo := afkFixtureRepo(t, st, "proj")

		// Nil in, empty out — both at create time and on the read back, so a
		// created Schedule compares equal to the stored row.
		bare := fixtureSchedule(t, st, repo.ID, "pure-prompt", "0 6 * * *")
		if bare.Flows == nil || len(bare.Flows) != 0 {
			t.Errorf("CreateSchedule returned flows = %v, want empty non-nil", bare.Flows)
		}
		got, err := st.ScheduleByID(ctx, bare.ID)
		if err != nil {
			t.Fatalf("ScheduleByID: %v", err)
		}
		if got.Flows == nil || len(got.Flows) != 0 {
			t.Errorf("stored flows = %v, want empty non-nil", got.Flows)
		}

		// Multi-flow selection survives verbatim, order included.
		multi, err := st.CreateSchedule(ctx, Schedule{
			ID: ids.NewID("sched"), RepoID: repo.ID, Name: "routed", Cadence: "0 6 * * 1",
			Flows: []string{"human-triage", "autolander"}, Enabled: true,
			CreatedAt: schedClock, UpdatedAt: schedClock,
		})
		if err != nil {
			t.Fatalf("CreateSchedule multi: %v", err)
		}
		if got, err = st.ScheduleByID(ctx, multi.ID); err != nil {
			t.Fatalf("ScheduleByID multi: %v", err)
		}
		if len(got.Flows) != 2 || got.Flows[0] != "human-triage" || got.Flows[1] != "autolander" {
			t.Errorf("flows = %v, want [human-triage autolander]", got.Flows)
		}

		// An update replaces the array wholesale, including back to none.
		if got, err = st.UpdateSchedule(ctx, multi.ID, ScheduleUpdate{Flows: Set([]string{"autolander"})}); err != nil {
			t.Fatalf("UpdateSchedule flows: %v", err)
		}
		if len(got.Flows) != 1 || got.Flows[0] != "autolander" {
			t.Errorf("flows after update = %v, want [autolander]", got.Flows)
		}
		if got, err = st.UpdateSchedule(ctx, multi.ID, ScheduleUpdate{Flows: Set([]string(nil))}); err != nil {
			t.Fatalf("UpdateSchedule flows cleared: %v", err)
		}
		if got.Flows == nil || len(got.Flows) != 0 {
			t.Errorf("cleared flows = %v, want empty non-nil", got.Flows)
		}
	})
}

// TestCreateSchedule_nameScopedToRepo pins UNIQUE(repo_id, name): a duplicate
// name inside one repo is ErrNameTaken, the SAME name in another repo is
// fine, and an unknown repo is ErrNotFound rather than a raw FK error.
func TestCreateSchedule_nameScopedToRepo(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		repo := afkFixtureRepo(t, st, "proj")
		other := afkFixtureRepo(t, st, "other")

		fixtureSchedule(t, st, repo.ID, "weekly", "0 6 * * 1")

		_, err := st.CreateSchedule(ctx, Schedule{
			ID: ids.NewID("sched"), RepoID: repo.ID, Name: "weekly", Cadence: "0 7 * * 2",
			CreatedAt: schedClock, UpdatedAt: schedClock,
		})
		if !errors.Is(err, ErrNameTaken) {
			t.Errorf("duplicate name err = %v, want ErrNameTaken", err)
		}

		// Same name, different repo: Schedules are per-repo objects.
		if _, err := st.CreateSchedule(ctx, Schedule{
			ID: ids.NewID("sched"), RepoID: other.ID, Name: "weekly", Cadence: "0 6 * * 1",
			CreatedAt: schedClock, UpdatedAt: schedClock,
		}); err != nil {
			t.Errorf("same name in another repo: %v", err)
		}

		if _, err := st.CreateSchedule(ctx, Schedule{
			ID: ids.NewID("sched"), RepoID: "repo_00000000000000000000000000000000",
			Name: "orphan", Cadence: "0 6 * * 1", CreatedAt: schedClock, UpdatedAt: schedClock,
		}); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing repo err = %v, want ErrNotFound", err)
		}
	})
}

// TestSchedules_cascadeOnRepoDelete pins the FK cascade: a deleted repo takes
// its Schedules with it.
func TestSchedules_cascadeOnRepoDelete(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		repo := afkFixtureRepo(t, st, "proj")
		keep := afkFixtureRepo(t, st, "keeper")

		fixtureSchedule(t, st, repo.ID, "weekly", "0 6 * * 1")
		fixtureSchedule(t, st, repo.ID, "daily", "0 6 * * *")
		survivor := fixtureSchedule(t, st, keep.ID, "weekly", "0 6 * * 1")
		if n := count(t, st, "schedules"); n != 3 {
			t.Fatalf("schedules rows = %d, want 3", n)
		}

		if err := st.DeleteRepo(ctx, repo.ID); err != nil {
			t.Fatalf("DeleteRepo: %v", err)
		}
		if n := count(t, st, "schedules"); n != 1 {
			t.Errorf("schedules rows after cascade = %d, want 1", n)
		}
		if _, err := st.ScheduleByID(ctx, survivor.ID); err != nil {
			t.Errorf("another repo's Schedule did not survive: %v", err)
		}
	})
}

// TestUpdateSchedule_partial pins the Opt semantics: only Set fields move,
// updated_at is stamped from st.Now on a real write, an empty update verifies
// existence without touching the row, and a rename collision inside the repo
// is ErrNameTaken.
func TestUpdateSchedule_partial(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		repo := afkFixtureRepo(t, st, "proj")

		sc, err := st.CreateSchedule(ctx, Schedule{
			ID: ids.NewID("sched"), RepoID: repo.ID, Name: "weekly", Cadence: "0 6 * * 1",
			Prompt: "investigate dependency updates", Flows: []string{"autolander"},
			Enabled: true, BudgetMinutes: intPtr(45), Model: strPtr("opus[1m]"),
			Effort: strPtr("max"), Provider: strPtr("claude-code"),
			CreatedAt: schedClock, UpdatedAt: schedClock,
		})
		if err != nil {
			t.Fatalf("CreateSchedule: %v", err)
		}

		edited := schedClock.Add(3 * time.Hour)
		st.Now = func() time.Time { return edited }

		got, err := st.UpdateSchedule(ctx, sc.ID, ScheduleUpdate{
			Cadence:       Set("15 7 * * 2"),
			Enabled:       Set(false),
			BudgetMinutes: Set((*int)(nil)), // clear → back to the 30-minute default
		})
		if err != nil {
			t.Fatalf("UpdateSchedule: %v", err)
		}
		if got.Cadence != "15 7 * * 2" || got.Enabled {
			t.Errorf("cadence/enabled = %q/%v, want 15 7 * * 2/false", got.Cadence, got.Enabled)
		}
		if got.BudgetMinutes != nil {
			t.Errorf("budget after clear = %v, want nil (inherit the default)", got.BudgetMinutes)
		}
		// Untouched fields keep their values — including the other overrides,
		// which a naive full-row write would have flattened.
		if got.Name != "weekly" || got.Prompt != "investigate dependency updates" {
			t.Errorf("untouched name/prompt = %q/%q", got.Name, got.Prompt)
		}
		if len(got.Flows) != 1 || got.Flows[0] != "autolander" {
			t.Errorf("untouched flows = %v, want [autolander]", got.Flows)
		}
		if got.Model == nil || *got.Model != "opus[1m]" || got.Effort == nil || *got.Effort != "max" ||
			got.Provider == nil || *got.Provider != "claude-code" {
			t.Errorf("untouched overrides = %v/%v/%v", got.Model, got.Effort, got.Provider)
		}
		if !got.CreatedAt.Equal(storedTime(schedClock)) {
			t.Errorf("created_at = %v, want unchanged %v", got.CreatedAt, storedTime(schedClock))
		}
		if !got.UpdatedAt.Equal(storedTime(edited)) {
			t.Errorf("updated_at = %v, want the injected clock %v", got.UpdatedAt, storedTime(edited))
		}

		// An empty update verifies existence and leaves the row alone —
		// updated_at must not move even though the clock has.
		st.Now = func() time.Time { return edited.Add(time.Hour) }
		got, err = st.UpdateSchedule(ctx, sc.ID, ScheduleUpdate{})
		if err != nil {
			t.Fatalf("empty UpdateSchedule: %v", err)
		}
		if !got.UpdatedAt.Equal(storedTime(edited)) {
			t.Errorf("updated_at after empty update = %v, want unchanged %v", got.UpdatedAt, storedTime(edited))
		}

		// Rename onto a sibling's name inside the same repo is ErrNameTaken.
		fixtureSchedule(t, st, repo.ID, "daily", "0 6 * * *")
		if _, err := st.UpdateSchedule(ctx, sc.ID, ScheduleUpdate{Name: Set("daily")}); !errors.Is(err, ErrNameTaken) {
			t.Errorf("rename collision err = %v, want ErrNameTaken", err)
		}

		if _, err := st.UpdateSchedule(ctx, "sched_00000000000000000000000000000000",
			ScheduleUpdate{Prompt: Set("x")}); !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown id err = %v, want ErrNotFound", err)
		}
		if _, err := st.UpdateSchedule(ctx, "sched_00000000000000000000000000000000",
			ScheduleUpdate{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("empty update on unknown id err = %v, want ErrNotFound", err)
		}
	})
}

// TestEnabledSchedules pins the producer's one listing per pass: disabled and
// paused Schedules are filtered out in SQL, and the survivors come back
// ordered by (repo_id, name) so a pass is deterministic.
func TestEnabledSchedules(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		repo := afkFixtureRepo(t, st, "proj")
		other := afkFixtureRepo(t, st, "other")

		if list, err := st.EnabledSchedules(ctx); err != nil || len(list) != 0 {
			t.Fatalf("empty store = %v (err %v), want no candidates", names(list), err)
		}

		fixtureSchedule(t, st, repo.ID, "alpha", "0 6 * * *")
		fixtureSchedule(t, st, repo.ID, "beta", "0 7 * * *")
		fixtureSchedule(t, st, other.ID, "gamma", "0 8 * * *")

		disabled, err := st.CreateSchedule(ctx, Schedule{
			ID: ids.NewID("sched"), RepoID: repo.ID, Name: "switched-off",
			Cadence: "0 9 * * *", Enabled: false, CreatedAt: schedClock, UpdatedAt: schedClock,
		})
		if err != nil {
			t.Fatalf("CreateSchedule disabled: %v", err)
		}
		paused, err := st.CreateSchedule(ctx, Schedule{
			ID: ids.NewID("sched"), RepoID: repo.ID, Name: "struck-out",
			Cadence: "0 10 * * *", Enabled: true, Paused: true, ConsecutiveFailures: 3,
			CreatedAt: schedClock, UpdatedAt: schedClock,
		})
		if err != nil {
			t.Fatalf("CreateSchedule paused: %v", err)
		}

		list, err := st.EnabledSchedules(ctx)
		if err != nil {
			t.Fatalf("EnabledSchedules: %v", err)
		}
		if len(list) != 3 {
			t.Fatalf("EnabledSchedules = %v, want the 3 live ones", names(list))
		}
		for _, sc := range list {
			if sc.ID == disabled.ID || sc.ID == paused.ID {
				t.Errorf("%q leaked into the candidate listing", sc.Name)
			}
		}
		// Ordered by (repo_id, name): repo ids are random, so the assertion is
		// the ordering rule itself rather than a hardcoded sequence.
		for i := 1; i < len(list); i++ {
			prev, cur := list[i-1], list[i]
			if prev.RepoID > cur.RepoID || (prev.RepoID == cur.RepoID && prev.Name > cur.Name) {
				t.Errorf("order broken at %d: (%s,%s) before (%s,%s)", i,
					prev.RepoID, prev.Name, cur.RepoID, cur.Name)
			}
		}

		// Re-enabling brings a paused Schedule back into the listing.
		if _, err := st.ReenableSchedule(ctx, paused.ID); err != nil {
			t.Fatalf("ReenableSchedule: %v", err)
		}
		if list, err = st.EnabledSchedules(ctx); err != nil || len(list) != 4 {
			t.Errorf("after re-enable = %v (err %v), want 4", names(list), err)
		}
	})
}

// TestScheduleFailureCounters pins the per-Schedule three-strikes state: the
// counter increments and reports its new value, resets report whether they
// changed anything, the pause flag reports its own edge (the push
// notification's trigger), and re-enable clears both in one write.
func TestScheduleFailureCounters(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		repo := afkFixtureRepo(t, st, "proj")
		sc := fixtureSchedule(t, st, repo.ID, "weekly", "0 6 * * 1")

		for want := 1; want <= 3; want++ {
			n, err := st.IncrementScheduleFailures(ctx, sc.ID)
			if err != nil {
				t.Fatalf("IncrementScheduleFailures: %v", err)
			}
			if n != want {
				t.Errorf("counter = %d, want %d", n, want)
			}
		}
		got, err := st.ScheduleByID(ctx, sc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ConsecutiveFailures != 3 {
			t.Errorf("persisted counter = %d, want 3", got.ConsecutiveFailures)
		}

		// The pause edge: first write flips it, a re-assert reports no change,
		// so the "paused after 3 failures" push sends exactly once.
		changed, err := st.SetSchedulePaused(ctx, sc.ID, true)
		if err != nil || !changed {
			t.Fatalf("SetSchedulePaused: changed=%v err=%v, want a real transition", changed, err)
		}
		changed, err = st.SetSchedulePaused(ctx, sc.ID, true)
		if err != nil || changed {
			t.Errorf("re-asserting paused: changed=%v err=%v, want no-op", changed, err)
		}
		if got, _ = st.ScheduleByID(ctx, sc.ID); !got.Paused || got.ConsecutiveFailures != 3 {
			t.Errorf("after pause = paused %v, failures %d, want true/3 (pause never erases the strikes)",
				got.Paused, got.ConsecutiveFailures)
		}

		// Re-enable clears the pause AND the counter, returning the fresh row.
		fresh, err := st.ReenableSchedule(ctx, sc.ID)
		if err != nil {
			t.Fatalf("ReenableSchedule: %v", err)
		}
		if fresh.Paused || fresh.ConsecutiveFailures != 0 {
			t.Errorf("re-enabled = paused %v, failures %d, want false/0", fresh.Paused, fresh.ConsecutiveFailures)
		}

		// A budget-expiry completion resets the counter; resetting an
		// already-zero counter is not a change.
		if _, err := st.IncrementScheduleFailures(ctx, sc.ID); err != nil {
			t.Fatalf("IncrementScheduleFailures: %v", err)
		}
		if changed, err = st.ResetScheduleFailures(ctx, sc.ID); err != nil || !changed {
			t.Fatalf("ResetScheduleFailures: changed=%v err=%v, want a real transition", changed, err)
		}
		if changed, err = st.ResetScheduleFailures(ctx, sc.ID); err != nil || changed {
			t.Errorf("second reset: changed=%v err=%v, want no-op", changed, err)
		}

		// Unpausing through the plain setter reports its edge too.
		if changed, err = st.SetSchedulePaused(ctx, sc.ID, false); err != nil || changed {
			t.Errorf("unpause of a live Schedule: changed=%v err=%v, want no-op", changed, err)
		}

		const missing = "sched_00000000000000000000000000000000"
		if _, err := st.IncrementScheduleFailures(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Errorf("increment on missing Schedule = %v, want ErrNotFound", err)
		}
		if _, err := st.ReenableSchedule(ctx, missing); !errors.Is(err, ErrNotFound) {
			t.Errorf("re-enable of missing Schedule = %v, want ErrNotFound", err)
		}
	})
}

// TestMarkScheduleFired pins the firing stamp: last_fired_at moves, and
// updated_at does NOT — a Schedule that fires every morning must not read as
// edited every morning.
func TestMarkScheduleFired(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		repo := afkFixtureRepo(t, st, "proj")
		sc := fixtureSchedule(t, st, repo.ID, "weekly", "0 6 * * 1")
		if sc.LastFiredAt != nil {
			t.Fatalf("fresh Schedule last fired = %v, want nil", sc.LastFiredAt)
		}

		fired := schedClock.Add(48 * time.Hour)
		if err := st.MarkScheduleFired(ctx, sc.ID, fired); err != nil {
			t.Fatalf("MarkScheduleFired: %v", err)
		}
		got, err := st.ScheduleByID(ctx, sc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.LastFiredAt == nil || !got.LastFiredAt.Equal(storedTime(fired)) {
			t.Errorf("last fired = %v, want %v", got.LastFiredAt, storedTime(fired))
		}
		if !got.UpdatedAt.Equal(storedTime(schedClock)) {
			t.Errorf("updated_at = %v, want unchanged %v (a firing is not an edit)",
				got.UpdatedAt, storedTime(schedClock))
		}

		// A later firing overwrites — no history, the row carries only the
		// most recent slot the pass observed.
		again := fired.Add(7 * 24 * time.Hour)
		if err := st.MarkScheduleFired(ctx, sc.ID, again); err != nil {
			t.Fatalf("second MarkScheduleFired: %v", err)
		}
		if got, _ = st.ScheduleByID(ctx, sc.ID); got.LastFiredAt == nil || !got.LastFiredAt.Equal(storedTime(again)) {
			t.Errorf("last fired after second firing = %v, want %v", got.LastFiredAt, storedTime(again))
		}

		if err := st.MarkScheduleFired(ctx, "sched_00000000000000000000000000000000", fired); !errors.Is(err, ErrNotFound) {
			t.Errorf("firing a missing Schedule = %v, want ErrNotFound", err)
		}
	})
}

// TestScheduledRun_linkAndOrphaning is the migration proof for the widened
// runs.kind CHECK and the new schedule_id column: a scheduled run round-trips
// through the ordinary run API, the reaper's listing sees it, and deleting the
// Schedule orphans the link (ON DELETE SET NULL) without touching the run.
func TestScheduledRun_linkAndOrphaning(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		repo := afkFixtureRepo(t, st, "proj")
		sc := fixtureSchedule(t, st, repo.ID, "weekly", "0 6 * * 1")

		run, err := st.CreateRun(ctx, Run{
			ID: ids.NewID("run"), RepoID: repo.ID, Kind: RunKindScheduled,
			Provider: "claude-code", ScheduleID: &sc.ID, Branch: "lab/sched-1",
			WorktreePath: "/wt/sched-1", SessionName: "proj~sched-1",
			Model: "opus[1m]", Effort: "max", StartedAt: schedClock,
			Outcome: RunOutcomeActive,
		})
		if err != nil {
			t.Fatalf("CreateRun scheduled: %v", err)
		}
		if run.ScheduleID == nil || *run.ScheduleID != sc.ID {
			t.Errorf("CreateRun returned schedule id = %v, want %q", run.ScheduleID, sc.ID)
		}

		got, err := st.RunByID(ctx, run.ID)
		if err != nil {
			t.Fatalf("RunByID: %v", err)
		}
		if got.Kind != RunKindScheduled {
			t.Errorf("kind = %q, want %q", got.Kind, RunKindScheduled)
		}
		if got.ScheduleID == nil || *got.ScheduleID != sc.ID {
			t.Errorf("stored schedule id = %v, want %q", got.ScheduleID, sc.ID)
		}

		// The reaper's listing must include the kind, or the budget clock that
		// is a scheduled run's ONLY terminator never fires.
		unattended, err := st.ActiveAFKRuns(ctx)
		if err != nil {
			t.Fatalf("ActiveAFKRuns: %v", err)
		}
		if len(unattended) != 1 || unattended[0].ID != run.ID {
			t.Errorf("ActiveAFKRuns = %d runs, want the scheduled one", len(unattended))
		}

		// A run of another kind carries no link at all.
		plain := afkFixtureRun(t, st, repo.ID, RunKindManual, "proj~20260730-0600", schedClock)
		if got, err = st.RunByID(ctx, plain.ID); err != nil {
			t.Fatalf("RunByID manual: %v", err)
		}
		if got.ScheduleID != nil {
			t.Errorf("manual run schedule id = %v, want nil", got.ScheduleID)
		}

		// Deleting the Schedule orphans the link and leaves the live run alone:
		// its budget clock still owns its lifetime.
		if err := st.DeleteSchedule(ctx, sc.ID); err != nil {
			t.Fatalf("DeleteSchedule: %v", err)
		}
		got, err = st.RunByID(ctx, run.ID)
		if err != nil {
			t.Fatalf("RunByID after schedule delete: %v", err)
		}
		if got.ScheduleID != nil {
			t.Errorf("schedule id after delete = %v, want nil (ON DELETE SET NULL)", got.ScheduleID)
		}
		if got.Outcome != RunOutcomeActive || got.Kind != RunKindScheduled {
			t.Errorf("run after schedule delete = kind %q outcome %q, want the run untouched", got.Kind, got.Outcome)
		}
		if n := count(t, st, "runs"); n != 2 {
			t.Errorf("runs rows after schedule delete = %d, want 2 (nothing cascaded)", n)
		}
	})
}

// TestActiveRunForSchedule pins the skip-on-overlap gate: it finds only the
// live run of the named Schedule, and answers ErrNotFound for a Schedule with
// nothing in flight, another Schedule's run, or a run that has already reaped.
func TestActiveRunForSchedule(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		repo := afkFixtureRepo(t, st, "proj")
		weekly := fixtureSchedule(t, st, repo.ID, "weekly", "0 6 * * 1")
		daily := fixtureSchedule(t, st, repo.ID, "daily", "0 6 * * *")

		if _, err := st.ActiveRunForSchedule(ctx, weekly.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("never-fired Schedule = %v, want ErrNotFound", err)
		}

		run, err := st.CreateRun(ctx, Run{
			ID: ids.NewID("run"), RepoID: repo.ID, Kind: RunKindScheduled,
			Provider: "claude-code", ScheduleID: &weekly.ID, Branch: "lab/sched-1",
			WorktreePath: "/wt/sched-1", SessionName: "proj~sched-1",
			Model: "opus[1m]", Effort: "max", StartedAt: schedClock,
			Outcome: RunOutcomeActive,
		})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		got, err := st.ActiveRunForSchedule(ctx, weekly.ID)
		if err != nil {
			t.Fatalf("ActiveRunForSchedule: %v", err)
		}
		if got.ID != run.ID {
			t.Errorf("got run %q, want %q", got.ID, run.ID)
		}
		// Per Schedule, never per repo: the sibling cadence is free to fire.
		if _, err := st.ActiveRunForSchedule(ctx, daily.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("sibling Schedule = %v, want ErrNotFound", err)
		}

		// Budget expiry ends the run success — the overlap is over and the next
		// due firing launches.
		if err := st.EndRun(ctx, run.ID, RunOutcomeSuccess, schedClock.Add(30*time.Minute), ""); err != nil {
			t.Fatalf("EndRun: %v", err)
		}
		if _, err := st.ActiveRunForSchedule(ctx, weekly.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("reaped run still reported in flight: %v", err)
		}

		// The next firing's run is found again — the gate is per-run, not a
		// once-per-Schedule latch.
		next, err := st.CreateRun(ctx, Run{
			ID: ids.NewID("run"), RepoID: repo.ID, Kind: RunKindScheduled,
			Provider: "claude-code", ScheduleID: &weekly.ID, Branch: "lab/sched-2",
			WorktreePath: "/wt/sched-2", SessionName: "proj~sched-2",
			Model: "opus[1m]", Effort: "max", StartedAt: schedClock.Add(7 * 24 * time.Hour),
			Outcome: RunOutcomeActive,
		})
		if err != nil {
			t.Fatalf("CreateRun next: %v", err)
		}
		if got, err = st.ActiveRunForSchedule(ctx, weekly.ID); err != nil || got.ID != next.ID {
			t.Errorf("second firing: got %q err=%v, want %q", got.ID, err, next.ID)
		}
	})
}

// TestPauseScheduleIfStruck pins the one-statement guarded pause: the write
// lands only while the strikes still clear the threshold, so a human
// re-enable racing the reaper's increment→pause window wins instead of being
// silently reverted into paused-with-zero-strikes.
func TestPauseScheduleIfStruck(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		repo := afkFixtureRepo(t, st, "proj")
		sc := fixtureSchedule(t, st, repo.ID, "weekly", "0 6 * * 1")

		// Below the threshold nothing pauses.
		if _, err := st.IncrementScheduleFailures(ctx, sc.ID); err != nil {
			t.Fatal(err)
		}
		if changed, err := st.PauseScheduleIfStruck(ctx, sc.ID, 3); err != nil || changed {
			t.Fatalf("pause below threshold: changed=%v err=%v, want a no-op", changed, err)
		}

		// The race the verb exists for: three strikes land, then a re-enable
		// resets the counter BEFORE the pause write — the guarded pause must
		// no-op, never revert the human's reset into a pause.
		for range 2 {
			if _, err := st.IncrementScheduleFailures(ctx, sc.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.ReenableSchedule(ctx, sc.ID); err != nil {
			t.Fatalf("ReenableSchedule: %v", err)
		}
		if changed, err := st.PauseScheduleIfStruck(ctx, sc.ID, 3); err != nil || changed {
			t.Fatalf("pause after a reset: changed=%v err=%v, want a no-op (the re-enable wins)", changed, err)
		}
		if got, _ := st.ScheduleByID(ctx, sc.ID); got.Paused || got.ConsecutiveFailures != 0 {
			t.Fatalf("after the raced pause = paused %v, failures %d, want false/0", got.Paused, got.ConsecutiveFailures)
		}

		// At the threshold it pauses exactly once; the second call reports no
		// edge (the push's send-once trigger), and the strikes survive.
		for range 3 {
			if _, err := st.IncrementScheduleFailures(ctx, sc.ID); err != nil {
				t.Fatal(err)
			}
		}
		if changed, err := st.PauseScheduleIfStruck(ctx, sc.ID, 3); err != nil || !changed {
			t.Fatalf("pause at threshold: changed=%v err=%v, want a real transition", changed, err)
		}
		if changed, err := st.PauseScheduleIfStruck(ctx, sc.ID, 3); err != nil || changed {
			t.Errorf("second pause: changed=%v err=%v, want a no-op", changed, err)
		}
		if got, _ := st.ScheduleByID(ctx, sc.ID); !got.Paused || got.ConsecutiveFailures != 3 {
			t.Errorf("after the pause = paused %v, failures %d, want true/3 (pause never erases the strikes)",
				got.Paused, got.ConsecutiveFailures)
		}

		// A missing Schedule is a plain no-op, like an unstruck one.
		if changed, err := st.PauseScheduleIfStruck(ctx, "sched_00000000000000000000000000000000", 3); err != nil || changed {
			t.Errorf("pause of a missing Schedule: changed=%v err=%v, want a no-op", changed, err)
		}
	})
}

// TestScheduleMigrationPreservesRunChildren is the 0020 rebuild proof, in
// TestRunRemoteBackfill's shape: the sqlite runs-table rebuild (kind CHECK
// widened, schedule_id appended) runs with foreign_keys OFF, so the rows of
// every table REFERENCING runs — run_tokens (CASCADE), issues.run_id and
// issue_comments.run_id (both SET NULL) — must survive the copy AND keep
// behaving as foreign keys against the rebuilt table afterwards.
func TestScheduleMigrationPreservesRunChildren(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lab.db")
	db, err := sql.Open("sqlite", "file:"+path+
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	p, err := goose.NewProvider(goose.DialectSQLite3, db, migrations.SQLite)
	if err != nil {
		t.Fatalf("goose provider: %v", err)
	}

	// The schema as it stood before 0020: no schedule_id column at all.
	if _, err := p.UpTo(ctx, 19); err != nil {
		t.Fatalf("migrate to 0019: %v", err)
	}
	var hasScheduleID int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name = 'schedule_id'`).Scan(&hasScheduleID); err != nil {
		t.Fatalf("inspect pre-migration runs table: %v", err)
	}
	if hasScheduleID != 0 {
		t.Fatalf("runs.schedule_id already exists at version 0019 — the fixture is not pre-migration")
	}

	repoID, runID := ids.NewID("repo"), ids.NewID("run")
	tokenID, issueID, commentID := ids.NewID("tok"), ids.NewID("issue"), ids.NewID("cmt")
	now := fmtTime(time.Now())
	if _, err := db.ExecContext(ctx,
		`INSERT INTO repos (id, name, remote_url, tracker_binding, forge_kind,
		     afk_branch_pattern, manual_branch_prefix, clone_status, created_at)
		 VALUES (?, ?, ?, 'builtin', 'none', 'afk/<N>', 'lab/', 'ready', ?)`,
		repoID, "legacy", "/tmp/legacy", now); err != nil {
		t.Fatalf("insert pre-migration repo: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO runs (id, repo_id, kind, provider, branch, worktree_path,
		     session_name, model, effort, started_at, outcome)
		 VALUES (?, ?, 'afk_manual', 'claude-code', 'afk/7', '/wt/legacy-7',
		     'legacy~afk-7', 'opus[1m]', 'max', ?, 'active')`,
		runID, repoID, now); err != nil {
		t.Fatalf("insert pre-migration run: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO run_tokens (id, run_id, token_hash, created_at) VALUES (?, ?, ?, ?)`,
		tokenID, runID, ids.HashToken("lab_run_legacy"), now); err != nil {
		t.Fatalf("insert pre-migration run token: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO issues (id, repo_id, number, title, state, created_at, updated_at, author_kind, run_id)
		 VALUES (?, ?, 7, 'legacy issue', 'open', ?, ?, 'run', ?)`,
		issueID, repoID, now, now, runID); err != nil {
		t.Fatalf("insert pre-migration issue: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO issue_comments (id, issue_id, author_kind, run_id, body, created_at)
		 VALUES (?, ?, 'run', ?, 'done', ?)`,
		commentID, issueID, runID, now); err != nil {
		t.Fatalf("insert pre-migration issue comment: %v", err)
	}

	// Apply 0020: the rebuild copies runs and re-points the children's
	// REFERENCES at the renamed table.
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	// All three child rows survive with their run link intact.
	for _, probe := range []struct{ table, id string }{
		{"run_tokens", tokenID}, {"issues", issueID}, {"issue_comments", commentID},
	} {
		var linked sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT run_id FROM `+probe.table+` WHERE id = ?`, probe.id).Scan(&linked); err != nil {
			t.Fatalf("%s row after migration: %v", probe.table, err)
		}
		if !linked.Valid || linked.String != runID {
			t.Errorf("%s.run_id after migration = %v, want %q", probe.table, linked, runID)
		}
	}

	// runs.schedule_id exists and is NULL for the pre-existing row.
	var scheduleID sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT schedule_id FROM runs WHERE id = ?`, runID).Scan(&scheduleID); err != nil {
		t.Fatalf("read schedule_id: %v", err)
	}
	if scheduleID.Valid {
		t.Errorf("pre-existing run schedule_id = %q, want NULL (no run predating 0020 was a firing's)", scheduleID.String)
	}

	// The widened kind CHECK admits 'scheduled'.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO runs (id, repo_id, kind, provider, branch, worktree_path,
		     session_name, model, effort, started_at, outcome)
		 VALUES (?, ?, 'scheduled', 'claude-code', 'lab/sched-1', '/wt/sched-1',
		     'legacy~sched-1', 'opus[1m]', 'max', ?, 'active')`,
		ids.NewID("run"), repoID, now); err != nil {
		t.Fatalf("insert scheduled-kind run after migration: %v", err)
	}

	// The children's FKs still point at the rebuilt runs table: deleting the
	// run cascades the token and SET-NULLs both run_id links.
	if _, err := db.ExecContext(ctx, `DELETE FROM runs WHERE id = ?`, runID); err != nil {
		t.Fatalf("delete migrated run: %v", err)
	}
	var tokens int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_tokens WHERE id = ?`, tokenID).Scan(&tokens); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if tokens != 0 {
		t.Errorf("run token survived the run delete: the CASCADE no longer points at runs")
	}
	for _, probe := range []struct{ table, id string }{
		{"issues", issueID}, {"issue_comments", commentID},
	} {
		var linked sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT run_id FROM `+probe.table+` WHERE id = ?`, probe.id).Scan(&linked); err != nil {
			t.Fatalf("%s row after run delete: %v", probe.table, err)
		}
		if linked.Valid {
			t.Errorf("%s.run_id after run delete = %q, want NULL (SET NULL no longer points at runs)", probe.table, linked.String)
		}
	}
}

// names renders a Schedule listing for failure messages.
func names(list []Schedule) []string {
	out := make([]string, 0, len(list))
	for _, sc := range list {
		out = append(out, sc.Name)
	}
	return out
}
