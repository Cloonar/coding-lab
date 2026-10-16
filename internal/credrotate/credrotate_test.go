package credrotate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
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
	logs  bytes.Buffer // the service's JSON log lines (issue #101 assertions)
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
		Providers: reg, Store: st, Homes: homes, Logger: logx.New(&f.logs),
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

// logged returns the decoded log records whose msg starts with prefix, in order.
func (f *fixture) logged(prefix string) []map[string]any {
	f.t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			f.t.Fatalf("decode log line %q: %v", line, err)
		}
		if msg, _ := rec["msg"].(string); strings.HasPrefix(msg, prefix) {
			out = append(out, rec)
		}
	}
	return out
}

// errLoggedOut is the adapter's refusal of a cleared credential file, as a
// real AdoptCredentials wraps it (issue #101).
var errLoggedOut = fmt.Errorf("adopt: %w", provider.ErrCredentialsLoggedOut)

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

// 5a. A home whose CLI lost the refresh race and cleared its tokens in place
// (issue #101) is NEVER a candidate, even with the newest mtime: the
// adapter reports it as "" (the vanished branch), so the older VALID
// self-refresh is adopted and fanned back into the cleared home. Both
// modellings: the fake's logged-out mark (zero mtime, as the adapter
// reports it) and a scripted "" sig carrying the newer mtime.
func TestTick_LoggedOutHomeNeverAdopted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clear func(f *fixture, id string)
	}{
		{"logged-out mark", func(f *fixture, id string) { f.fake.SetCredentialsLoggedOut(f.home(id)) }},
		{"empty sig newer mtime", func(f *fixture, id string) { f.setHome(id, "", t4) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.setMaster("sigA", t1)
			f.seedRun("runA", "", true)
			f.seedRun("runB", "", true)
			f.tick() // settle: both baselines sigA
			injected0 := len(f.fake.InjectHomes())

			f.setHome("runA", "sigX", t2) // runA won the refresh race (older mtime)
			tc.clear(f, "runB")           // runB lost it: invalid_grant, tokens cleared (newer)
			f.tick()

			if got := f.fake.AdoptedHomes(); len(got) != 1 || got[0] != f.home("runA") {
				t.Fatalf("adopts = %v; want exactly [%s] (the valid family)", got, f.home("runA"))
			}
			master, _ := f.fake.CredentialsSig("")
			if master != "sigX" {
				t.Errorf("master sig = %q; want sigX (runA's family)", master)
			}
			if sig, _ := f.fake.CredentialsSig(f.home("runB")); sig != master {
				t.Errorf("runB sig = %q; want the master's %q (re-injected)", sig, master)
			}
			for _, id := range []string{"runA", "runB"} {
				if got := f.credSig(id); got != "sigX" {
					t.Errorf("%s baseline = %q; want sigX", id, got)
				}
			}
			if countHome(f.fake.InjectHomes()[injected0:], f.home("runB")) == 0 {
				t.Errorf("runB was not re-injected: %v", f.fake.InjectHomes())
			}
			if got := f.logged("credrotate: rejected logged-out"); len(got) != 0 {
				t.Errorf("rejection logs = %v; want none (a cleared home is never a candidate)", got)
			}
			if got := f.logged("credrotate: credentials vanished"); len(got) != 1 || got[0]["run"] != "runB" {
				t.Errorf("vanished logs = %v; want one naming runB", got)
			}
		})
	}
}

// 5b. The backstop (issue #101): runB's file still had a valid, newer sig at the
// scan but the CLI cleared it before the adopt, so AdoptCredentials refuses
// with ErrCredentialsLoggedOut. The scan falls through to runA, fans its
// family out to both homes, and WARNs the rejection naming runB — not the
// generic adopt-failed line.
func TestTick_LoggedOutAdoptRejectedFallsThrough(t *testing.T) {
	f := newFixture(t)
	f.setMaster("sigA", t1)
	f.seedRun("runA", "", true)
	f.seedRun("runB", "", true)
	f.tick() // settle
	injected0 := len(f.fake.InjectHomes())

	f.setHome("runA", "sigX", t2)
	f.setHome("runB", "sigY", t4)                       // newest at the scan...
	f.fake.SetAdoptErrFor(f.home("runB"), errLoggedOut) // ...cleared before the adopt
	f.tick()

	if got := f.fake.AdoptedHomes(); len(got) != 1 || got[0] != f.home("runA") {
		t.Fatalf("adopts = %v; want exactly [%s] (fell through past the rejected runB)", got, f.home("runA"))
	}
	if sig, _ := f.fake.CredentialsSig(""); sig != "sigX" {
		t.Errorf("master sig = %q; want sigX", sig)
	}
	post := f.fake.InjectHomes()[injected0:]
	for _, id := range []string{"runA", "runB"} {
		if got := f.credSig(id); got != "sigX" {
			t.Errorf("%s baseline = %q; want sigX", id, got)
		}
		if countHome(post, f.home(id)) == 0 {
			t.Errorf("%s not re-injected: %v", id, post)
		}
	}
	rej := f.logged("credrotate: rejected logged-out adopt candidate")
	if len(rej) != 1 || rej[0]["run"] != "runB" || rej[0]["level"] != "WARN" {
		t.Fatalf("rejection logs = %v; want one WARN naming runB", rej)
	}
	if rej[0]["sig"] != "sigY" || rej[0]["baseline"] != "sigA" {
		t.Errorf("rejection sig/baseline = %v/%v; want sigY/sigA", rej[0]["sig"], rej[0]["baseline"])
	}
	if got := f.logged("credrotate: adopt failed"); len(got) != 0 {
		t.Errorf("generic adopt-failed logs = %v; want none for a logged-out refusal", got)
	}
	ok := f.logged("credrotate: adopted self-refreshed credentials")
	if len(ok) != 1 || ok[0]["run"] != "runA" || ok[0]["sig"] != "sigX" || ok[0]["baseline"] != "sigA" {
		t.Errorf("adopt logs = %v; want one naming runA with sig sigX, baseline sigA", ok)
	}
}

// 5c. Any OTHER adopt error keeps the generic warn (issue #101 only singles out
// the logged-out refusal).
func TestTick_OtherAdoptErrorGenericWarn(t *testing.T) {
	f := newFixture(t)
	f.setMaster("sigA", t1)
	f.seedRun("runA", "", true)
	f.tick() // settle

	f.setHome("runA", "sigX", t2)
	f.fake.SetAdoptErrFor(f.home("runA"), errors.New("boom"))
	f.tick()

	if got := f.logged("credrotate: adopt failed"); len(got) != 1 || got[0]["run"] != "runA" {
		t.Errorf("generic adopt-failed logs = %v; want one naming runA", got)
	}
	if got := f.logged("credrotate: rejected logged-out"); len(got) != 0 {
		t.Errorf("rejection logs = %v; want none for a non-logged-out error", got)
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

// 7a. A master that goes logged out after being valid — e.g. a cleared store
// the adapter reports as "" (issue #101) — gates off both the poke and the
// change-driven fan-out: a dead family is never pushed into live homes.
func TestTick_MasterLoggedOutGatesPokeAndFanOut(t *testing.T) {
	f := newFixture(t)
	f.setMaster("sigA", t1)
	f.seedRun("runA", "", true)
	f.tick() // settle (bootstrap fan-out + first poke)
	injected0 := len(f.fake.InjectHomes())
	refresh0 := f.fake.RefreshCalls()

	f.fake.SetCredentialsLoggedOut("")
	f.clk = f.clk.Add(pokeEvery + time.Second) // the poke gate is open
	f.tick()

	if got := f.fake.RefreshCalls(); got != refresh0 {
		t.Errorf("RefreshCalls = %d; want %d (never poke a logged-out master)", got, refresh0)
	}
	if n := len(f.fake.InjectHomes()) - injected0; n != 0 {
		t.Errorf("re-injections = %d; want 0 (never fan out a logged-out master)", n)
	}
	if got := f.credSig("runA"); got != "sigA" {
		t.Errorf("baseline = %q; want unchanged sigA", got)
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

	// A logged-out home (issue #101) is never adopted before wipe, however it is
	// modelled, and the master is left untouched.
	for _, tc := range []struct {
		name    string
		clear   func(f *fixture)
		rejects bool // the adopt was attempted and refused (cleared after the sig read)
	}{
		{"logged-out mark", func(f *fixture) { f.fake.SetCredentialsLoggedOut(f.home("runA")) }, false},
		{"empty sig", func(f *fixture) { f.setHome("runA", "", t2) }, false},
		{"cleared after sig read", func(f *fixture) {
			f.setHome("runA", "sigB", t2)
			f.fake.SetAdoptErrFor(f.home("runA"), errLoggedOut)
		}, true},
	} {
		t.Run("logged-out home not adopted: "+tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.setMaster("sigA", t1)
			f.seedRun("runA", "", true)
			f.seedRun("runB", "", true)
			f.tick() // settle: both baselines sigA
			injected0 := len(f.fake.InjectHomes())

			tc.clear(f)
			f.svc.AdoptCheck(ctx, "runA")

			if got := f.fake.AdoptedHomes(); len(got) != 0 {
				t.Errorf("adopts = %v; want none (logged-out home)", got)
			}
			if sig, _ := f.fake.CredentialsSig(""); sig != "sigA" {
				t.Errorf("master sig = %q; want untouched sigA", sig)
			}
			if n := len(f.fake.InjectHomes()) - injected0; n != 0 {
				t.Errorf("re-injections = %d; want 0 (nothing adopted, nothing to fan out)", n)
			}
			rej := f.logged("credrotate: adopt-check rejected logged-out credentials")
			if tc.rejects {
				if len(rej) != 1 || rej[0]["run"] != "runA" || rej[0]["level"] != "WARN" {
					t.Errorf("rejection logs = %v; want one WARN naming runA", rej)
				}
				if got := f.logged("credrotate: adopt-check adopt failed"); len(got) != 0 {
					t.Errorf("generic adopt-check logs = %v; want none for a logged-out refusal", got)
				}
			} else if len(rej) != 0 {
				t.Errorf("rejection logs = %v; want none (\"\" sig: no adopt attempted)", rej)
			}
		})
	}

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
