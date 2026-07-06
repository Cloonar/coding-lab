package instance

import (
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// addActiveRun inserts an active run row of the given kind, optionally with a
// live fake tmux session — the raw material of the lab_instances_active
// scrape source.
func (f *fixture) addActiveRun(kind, session string, live bool) {
	f.t.Helper()
	if _, err := f.st.CreateRun(f.t.Context(), store.Run{
		ID: ids.NewID("run"), RepoID: f.repo.ID, Kind: kind, Provider: "claude-code",
		Branch: "afk/1", WorktreePath: "/tmp/unused", SessionName: session,
		Model: "opus", Effort: "max", StartedAt: f.clock.Now(),
	}); err != nil {
		f.t.Fatalf("CreateRun(%s): %v", session, err)
	}
	if live {
		f.runner.AddLive(session)
	}
}

func TestLiveCounts_reflectsActiveRunsAndTmuxLiveness(t *testing.T) {
	f := newFixture(t)

	// A real manual instance (live fake session via the normal Start path).
	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// One live AFK run of each kind, plus an active row whose session died
	// (not yet reaped) — that one must NOT count: liveness comes from tmux,
	// never the DB.
	f.addActiveRun(store.RunKindAFKManual, "proj~afk-7", true)
	f.addActiveRun(store.RunKindAFKAuto, "proj~afk-auto-9", true)
	f.addActiveRun(store.RunKindAFKAuto, "proj~afk-auto-11", false)

	counts, err := f.svc.LiveCounts(t.Context())
	if err != nil {
		t.Fatalf("LiveCounts: %v", err)
	}
	if counts.Manual != 1 || counts.AFKManual != 1 || counts.AFKAuto != 1 {
		t.Fatalf("LiveCounts = %+v, want {Manual:1 AFKManual:1 AFKAuto:1} (dead session must not count)", counts)
	}

	// A session killed out from under lab drops out on the next read — the
	// scrape-time source can never drift.
	f.runner.Kill(run.SessionName)
	counts, err = f.svc.LiveCounts(t.Context())
	if err != nil {
		t.Fatalf("LiveCounts after kill: %v", err)
	}
	if counts.Manual != 0 || counts.AFKManual != 1 || counts.AFKAuto != 1 {
		t.Fatalf("LiveCounts after kill = %+v, want {Manual:0 AFKManual:1 AFKAuto:1}", counts)
	}
}
