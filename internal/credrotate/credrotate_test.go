package credrotate

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/instancehome"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

var (
	baseClock = time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	t1        = baseClock.Add(1 * time.Minute)
	t2        = baseClock.Add(2 * time.Minute)
	t3        = baseClock.Add(3 * time.Minute)
	t4        = baseClock.Add(4 * time.Minute)
)

type fixture struct {
	t     *testing.T
	svc   *Service
	st    *store.Store
	fake  *providertest.Fake
	homes *instancehome.Manager
	clk   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st := testutil.TempStore(t)
	fake := providertest.New()
	reg, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	homes := instancehome.New(t.TempDir())
	f := &fixture{t: t, st: st, fake: fake, homes: homes, clk: baseClock}
	svc, err := New(Options{
		Providers: reg, Store: st, Homes: homes, Logger: logx.New(io.Discard),
		Now: func() time.Time { return f.clk },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.svc = svc
	if _, err := st.CreateRepo(context.Background(), store.Repo{
		ID: "repo1", Name: "proj", RemoteURL: "file:///x", TrackerBinding: store.TrackerBindingBuiltin,
		ForgeKind: "none", DefaultBranch: "main", AFKBranchPattern: "afk/<N>",
		ManualBranchPrefix: "lab/", CloneStatus: store.CloneStatusReady, CreatedAt: baseClock,
	}); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return f
}

// seedRun creates an active claude-code run with the given last-injected
// baseline, materializing its instance home on disk when withHome is set (the
// liveRuns filter requires the home to exist).
func (f *fixture) seedRun(id, credSig string, withHome bool) store.Run {
	f.t.Helper()
	run, err := f.st.CreateRun(context.Background(), store.Run{
		ID: id, RepoID: "repo1", Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/" + id, WorktreePath: "/wt/" + id, SessionName: "proj~" + id,
		StartedAt: baseClock, Outcome: store.RunOutcomeActive, CredSig: credSig,
	})
	if err != nil {
		f.t.Fatalf("CreateRun %s: %v", id, err)
	}
	if withHome {
		if err := os.MkdirAll(f.homes.HomePath(id), 0o700); err != nil {
			f.t.Fatalf("mkdir home %s: %v", id, err)
		}
	}
	return run
}

func (f *fixture) setMaster(sig string, mtime time.Time) {
	f.fake.SetCredentialsSig("", sig, mtime)
}

func (f *fixture) setHome(id, sig string, mtime time.Time) {
	f.fake.SetCredentialsSig(f.homes.HomePath(id), sig, mtime)
}

// credSig reads a run's persisted baseline back from the store.
func (f *fixture) credSig(id string) string {
	f.t.Helper()
	run, err := f.st.RunByID(context.Background(), id)
	if err != nil {
		f.t.Fatalf("RunByID %s: %v", id, err)
	}
	return run.CredSig
}

func (f *fixture) tick() { f.svc.tick(context.Background()) }

func (f *fixture) home(id string) string { return f.homes.HomePath(id) }

// countHome returns how many times home was passed to InjectCredentials.
func countHome(homes []string, want string) int {
	n := 0
	for _, h := range homes {
		if h == want {
			n++
		}
	}
	return n
}

//  1. A master rotation re-injects every live home and re-stamps every baseline;
//     no adopt happens.
func TestTick_MasterRotationFansOut(t *testing.T) {
	f := newFixture(t)
	f.setMaster("sigA", t1)
	f.seedRun("runA", "", true)
	f.seedRun("runB", "", true)
	f.tick() // bootstrap: fan sigA out, stamp both baselines
	if f.credSig("runA") != "sigA" || f.credSig("runB") != "sigA" {
		t.Fatalf("after settle: baselines = %q/%q; want sigA/sigA", f.credSig("runA"), f.credSig("runB"))
	}
	injected0 := len(f.fake.InjectHomes())

	f.setMaster("sigB", t2) // rotation
	f.tick()

	if got := f.credSig("runA"); got != "sigB" {
		t.Errorf("runA baseline = %q; want sigB", got)
	}
	if got := f.credSig("runB"); got != "sigB" {
		t.Errorf("runB baseline = %q; want sigB", got)
	}
	if n := len(f.fake.InjectHomes()) - injected0; n != 2 {
		t.Errorf("re-injections = %d; want 2 (both homes)", n)
	}
	if got := f.fake.AdoptedHomes(); len(got) != 0 {
		t.Errorf("adopts = %v; want none", got)
	}
}

// 2. A poke that rotates nothing advances RefreshCalls but triggers no fan-out.
func TestTick_NoopPokeDoesNotFanOut(t *testing.T) {
	f := newFixture(t)
	f.setMaster("sigA", t1)
	f.seedRun("runA", "", true)
	f.tick() // settle
	injected0 := len(f.fake.InjectHomes())
	refresh0 := f.fake.RefreshCalls()

	f.clk = f.clk.Add(pokeEvery + time.Second) // let the poke gate open
	f.tick()

	if got := f.fake.RefreshCalls(); got != refresh0+1 {
		t.Errorf("RefreshCalls = %d; want %d (one poke)", got, refresh0+1)
	}
	if n := len(f.fake.InjectHomes()) - injected0; n != 0 {
		t.Errorf("re-injections = %d; want 0 (no rotation → no fan-out)", n)
	}
	if got := f.credSig("runA"); got != "sigA" {
		t.Errorf("baseline = %q; want unchanged sigA", got)
	}
}

//  3. A poke that DOES rotate the master (the CLI decided the token was near
//     expiry) fans out within the same tick.
func TestTick_PokeRotationFansOutSameTick(t *testing.T) {
	f := newFixture(t)
	f.setMaster("sigA", t1)
	f.seedRun("runA", "", true)
	f.tick() // settle
	injected0 := len(f.fake.InjectHomes())
	refresh0 := f.fake.RefreshCalls()

	f.fake.SetRefreshHook(func() { f.fake.SetCredentialsSig("", "sigB", t2) })
	f.clk = f.clk.Add(pokeEvery + time.Second)
	f.tick()

	if got := f.fake.RefreshCalls(); got != refresh0+1 {
		t.Errorf("RefreshCalls = %d; want %d", got, refresh0+1)
	}
	if got := f.credSig("runA"); got != "sigB" {
		t.Errorf("baseline = %q; want sigB (poke-rotated family fanned out)", got)
	}
	if n := len(f.fake.InjectHomes()) - injected0; n != 1 {
		t.Errorf("re-injections = %d; want 1", n)
	}
	if got := f.fake.AdoptedHomes(); len(got) != 0 {
		t.Errorf("adopts = %v; want none", got)
	}
}

//  4. A single self-refresh is adopted, then fanned out to the other live homes
//     and all baselines re-stamped.
func TestTick_SelfRefreshAdoptedAndFannedOut(t *testing.T) {
	f := newFixture(t)
	f.setMaster("sigA", t1)
	f.seedRun("runA", "", true)
	f.seedRun("runB", "", true)
	f.tick() // settle: both baselines sigA
	injected0 := len(f.fake.InjectHomes())

	f.setHome("runA", "sigB", t2) // runA's CLI self-refreshed
	f.tick()

	if got := f.fake.AdoptedHomes(); len(got) != 1 || got[0] != f.home("runA") {
		t.Errorf("adopts = %v; want exactly [%s]", got, f.home("runA"))
	}
	if got := f.credSig("runA"); got != "sigB" {
		t.Errorf("runA baseline = %q; want sigB", got)
	}
	if got := f.credSig("runB"); got != "sigB" {
		t.Errorf("runB baseline = %q; want sigB (adopted family fanned out)", got)
	}
	// The other home was re-injected in the post-adopt fan-out.
	if countHome(f.fake.InjectHomes()[injected0:], f.home("runB")) == 0 {
		t.Errorf("runB was not re-injected after the adopt: %v", f.fake.InjectHomes())
	}
}

//  5. Several homes self-refresh in one tick → ONLY the newest-mtime one is
//     adopted (each rotation invalidates the others).
func TestTick_NewestSelfRefreshWins(t *testing.T) {
	f := newFixture(t)
	f.setMaster("sigA", t1)
	f.seedRun("runA", "", true)
	f.seedRun("runB", "", true)
	f.seedRun("runC", "", true)
	f.tick() // settle

	f.setHome("runA", "sigX", t2)
	f.setHome("runB", "sigY", t4) // newest
	f.setHome("runC", "sigZ", t3)
	f.tick()

	if got := f.fake.AdoptedHomes(); len(got) != 1 || got[0] != f.home("runB") {
		t.Fatalf("adopts = %v; want exactly [%s] (newest mtime)", got, f.home("runB"))
	}
	for _, id := range []string{"runA", "runB", "runC"} {
		if got := f.credSig(id); got != "sigY" {
			t.Errorf("%s baseline = %q; want sigY (newest family fanned out)", id, got)
		}
	}
}

//  6. An unstamped row (baseline "") whose home holds credentials gets its
//     baseline stamped — no adopt, no self-refresh treatment.
func TestTick_UnstampedRowStampsBaseline(t *testing.T) {
	f := newFixture(t)
	f.setMaster("", time.Time{}) // logged out → no bootstrap fan-out to clobber the stamp
	f.seedRun("runA", "", true)
	f.setHome("runA", "sigX", t1)
	f.tick()

	if got := f.credSig("runA"); got != "sigX" {
		t.Errorf("baseline = %q; want sigX (stamped from the live home)", got)
	}
	if got := f.fake.AdoptedHomes(); len(got) != 0 {
		t.Errorf("adopts = %v; want none (no baseline to diverge from)", got)
	}
	if got := f.fake.RefreshCalls(); got != 0 {
		t.Errorf("RefreshCalls = %d; want 0 (never poke a logged-out host)", got)
	}
	if got := f.fake.InjectHomes(); len(got) != 0 {
		t.Errorf("injects = %v; want none", got)
	}
}

//  7. A logged-out master never pokes and never fans out; a later login is
//     detected on the next tick and heals the fleet.
func TestTick_LoggedOutThenLogin(t *testing.T) {
	f := newFixture(t)
	f.setMaster("", time.Time{})
	f.seedRun("runA", "", true)
	f.seedRun("runB", "", true)

	f.tick() // logged out
	if got := f.fake.RefreshCalls(); got != 0 {
		t.Errorf("RefreshCalls = %d; want 0 while logged out", got)
	}
	if got := f.fake.InjectHomes(); len(got) != 0 {
		t.Errorf("injects = %v; want none while logged out", got)
	}
	if got := f.fake.AdoptedHomes(); len(got) != 0 {
		t.Errorf("adopts = %v; want none while logged out", got)
	}

	f.setMaster("sigA", t1) // operator logs in
	f.tick()

	if countHome(f.fake.InjectHomes(), f.home("runA")) == 0 || countHome(f.fake.InjectHomes(), f.home("runB")) == 0 {
		t.Errorf("login did not fan out to both homes: %v", f.fake.InjectHomes())
	}
	if f.credSig("runA") != "sigA" || f.credSig("runB") != "sigA" {
		t.Errorf("post-login baselines = %q/%q; want sigA/sigA", f.credSig("runA"), f.credSig("runB"))
	}
}

// 8. AdoptCheck: the pre-wipe adopt-check.
func TestAdoptCheck(t *testing.T) {
	ctx := context.Background()

	t.Run("diverged adopts and fans out to the other home", func(t *testing.T) {
		f := newFixture(t)
		f.setMaster("sigA", t1)
		f.seedRun("runA", "", true)
		f.seedRun("runB", "", true)
		f.tick() // settle: both baselines sigA
		injected0 := len(f.fake.InjectHomes())

		f.setHome("runA", "sigB", t2) // runA self-refreshed
		f.svc.AdoptCheck(ctx, "runA")

		if got := f.fake.AdoptedHomes(); len(got) != 1 || got[0] != f.home("runA") {
			t.Fatalf("adopts = %v; want exactly [%s]", got, f.home("runA"))
		}
		if sig, _ := f.fake.CredentialsSig(""); sig != "sigB" {
			t.Errorf("master sig = %q; want sigB (adopted)", sig)
		}
		if got := f.credSig("runB"); got != "sigB" {
			t.Errorf("runB baseline = %q; want sigB (fanned out)", got)
		}
		post := f.fake.InjectHomes()[injected0:]
		if countHome(post, f.home("runB")) == 0 {
			t.Errorf("runB not re-injected: %v", post)
		}
		if countHome(post, f.home("runA")) != 0 {
			t.Errorf("runA re-injected but it is the checked (about-to-wipe) run: %v", post)
		}
	})

	t.Run("matching sig does not adopt", func(t *testing.T) {
		f := newFixture(t)
		f.setMaster("sigA", t1)
		f.seedRun("runA", "", true)
		f.tick() // baseline sigA, home sigA

		f.svc.AdoptCheck(ctx, "runA")

		if got := f.fake.AdoptedHomes(); len(got) != 0 {
			t.Errorf("adopts = %v; want none (home sig unchanged)", got)
		}
	})

	t.Run("missing row does not adopt", func(t *testing.T) {
		f := newFixture(t)
		f.svc.AdoptCheck(ctx, "nope")
		if got := f.fake.AdoptedHomes(); len(got) != 0 {
			t.Errorf("adopts = %v; want none (no such run)", got)
		}
	})

	t.Run("empty baseline does not adopt", func(t *testing.T) {
		f := newFixture(t)
		f.setMaster("sigA", t1)
		f.seedRun("runA", "", true) // CredSig ""
		f.setHome("runA", "sigX", t1)

		f.svc.AdoptCheck(ctx, "runA")

		if got := f.fake.AdoptedHomes(); len(got) != 0 {
			t.Errorf("adopts = %v; want none (no baseline to diverge from)", got)
		}
	})
}
