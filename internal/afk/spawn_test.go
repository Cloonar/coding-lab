package afk

// Spawn-pass core tests (issue #185): spawnPass driven over SYNTHETIC
// candidates — closures that record their invocation and return a scripted
// outcome — so stage ordering, in-stage stability, the snapshot veto, the
// atCap floor-raise, and slot accounting are pinned without any tracker or
// tmux choreography. The gather side (producers emitting real candidates)
// is exercised end-to-end by engine_test.go, autoland_test.go, and the
// cycle suites, which drive SpawnOnce.

import (
	"context"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// cappedRepo builds a repo row whose effective cap is n via the per-repo
// override — EffectiveCap reads the override straight off the row, no store
// round-trip, which keeps these candidates fully synthetic.
func cappedRepo(name string, n int) store.Repo {
	return store.Repo{ID: name, Name: name, MaxInstancesOverride: &n}
}

// spawnRecorder scripts candidates and records the order their launch
// closures actually ran in.
type spawnRecorder struct {
	order []string
}

func (r *spawnRecorder) candidate(label string, stage SpawnStage, repoCap int, result spawnOutcome) spawnCandidate {
	return spawnCandidate{
		stage: stage,
		repo:  cappedRepo(label, repoCap),
		label: label,
		launch: func(context.Context) spawnOutcome {
			r.order = append(r.order, label)
			return result
		},
	}
}

func (r *spawnRecorder) launched(label string) bool {
	for _, l := range r.order {
		if l == label {
			return true
		}
	}
	return false
}

// The priority rule as behavior: a scrambled production order launches by
// stage — lander > fix > new work — and stably WITHIN a stage (production
// order preserved: repos in listing order, pulls in listing order). The
// StageFix stub is #182's extension point exercised before any producer
// emits it: a candidate at the reserved rank slots into the order with zero
// pass changes.
func TestSpawnPass_stageOrderIsTheOnlySortKey(t *testing.T) {
	f := newFixture(t)
	rec := &spawnRecorder{}
	f.svc.spawnPass(t.Context(), 0, []spawnCandidate{
		rec.candidate("new-a", StageNewWork, 100, spawnSpawned),
		rec.candidate("lander-a", StageLander, 100, spawnSpawned),
		rec.candidate("fix-a", StageFix, 100, spawnSpawned),
		rec.candidate("new-b", StageNewWork, 100, spawnSpawned),
		rec.candidate("lander-b", StageLander, 100, spawnSpawned),
	})
	want := []string{"lander-a", "lander-b", "fix-a", "new-a", "new-b"}
	if len(rec.order) != len(want) {
		t.Fatalf("launch order = %v, want %v", rec.order, want)
	}
	for i := range want {
		if rec.order[i] != want[i] {
			t.Fatalf("launch order = %v, want %v (stage first, stable within a stage)", rec.order, want)
		}
	}
}

// The cheap snapshot veto: a candidate whose repo is at/over its effective
// cap on the pass snapshot never gets its launch closure called; one with
// headroom does.
func TestSpawnPass_snapshotVetoSkipsAtCapRepos(t *testing.T) {
	f := newFixture(t)
	rec := &spawnRecorder{}
	f.svc.spawnPass(t.Context(), 1, []spawnCandidate{
		rec.candidate("capped", StageLander, 1, spawnSpawned),
		rec.candidate("headroom", StageLander, 2, spawnSpawned),
	})
	if rec.launched("capped") {
		t.Error("at-cap candidate's launch closure ran (the snapshot veto must skip it)")
	}
	if !rec.launched("headroom") {
		t.Error("candidate with headroom never launched")
	}
}

// The atCap floor-raise (the scheduler's old launchAtCap semantics, kept):
// a locked launch proving THIS repo's cap reached raises the snapshot to
// that cap — a later same-cap candidate is vetoed without a launch call —
// but a later repo with a higher per-repo cap must still launch. This is
// also the pinned replacement for the old autoland whole-sweep stop on
// ErrOverCap (#185).
func TestSpawnPass_atCapRaisesTheSnapshotFloor(t *testing.T) {
	f := newFixture(t)
	rec := &spawnRecorder{}
	f.svc.spawnPass(t.Context(), 0, []spawnCandidate{
		rec.candidate("hits-cap", StageLander, 1, spawnAtCap),
		rec.candidate("same-cap", StageLander, 1, spawnSpawned),
		rec.candidate("higher-cap", StageLander, 3, spawnSpawned),
	})
	if !rec.launched("hits-cap") {
		t.Fatal("first candidate never launched (snapshot 0 < cap 1 had headroom)")
	}
	if rec.launched("same-cap") {
		t.Error("same-cap candidate launched after atCap proved the count reached 1 (floor-raise must veto it)")
	}
	if !rec.launched("higher-cap") {
		t.Error("higher-cap candidate vetoed — the atCap floor-raise must not stop the whole pass")
	}
}

// Slot accounting: every spawnSpawned consumes a snapshot slot for later
// vetoes; spawnSkipped consumes nothing.
func TestSpawnPass_spawnedCountsTheSlotSkippedDoesNot(t *testing.T) {
	f := newFixture(t)
	rec := &spawnRecorder{}
	f.svc.spawnPass(t.Context(), 0, []spawnCandidate{
		rec.candidate("skips", StageLander, 2, spawnSkipped),
		rec.candidate("first", StageLander, 2, spawnSpawned),
		rec.candidate("second", StageLander, 2, spawnSpawned),
		rec.candidate("starved", StageLander, 2, spawnSpawned),
	})
	for _, label := range []string{"skips", "first", "second"} {
		if !rec.launched(label) {
			t.Errorf("candidate %q never launched", label)
		}
	}
	if rec.launched("starved") {
		t.Error("candidate launched past an exhausted snapshot (two spawns against cap 2 must veto the third)")
	}
}
