package instance

// The Warpgate SSH bastion's INTEGRATION coverage (issue #39 / ADR-0068):
// bastion.go's pure core is pinned next door in bastion_test.go, and this file
// drives the wiring through real Launch/Start/Stop calls on the package's
// existing fixture, so every assertion is about what a spawn actually
// produced — the session env, the podman argv, the files in the run's tree,
// the seeded context file — and about what a refusal or a rollback left
// behind, in Warpgate included.
//
// Warpgate itself is a hand-written BastionAPI stub (bastionAPIStub) plus a
// BastionHostKeys stub: the wire-level fidelity of the real client is
// internal/warpgate's own tests' job (against a stubbed admin API), and here
// the point is to force one step to fail at an exact moment and to count
// calls precisely — "no Warpgate call" is an acceptance criterion, so it is
// asserted as a zero, not inferred.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/instancehome"
	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/warpgate"
)

// The wiring values the fixture's bastion is configured with — what cmd/lab
// would pass through instance.Options, plus the two per-host lookups tests
// pin so they never depend on this machine's PATH or passwd entry.
const (
	testWarpgateUserID = "wg-user-1"
	testWarpgateRoleID = "wg-role-1"
	testHostPATH       = "/run/current-system/sw/bin:/usr/bin:/bin"
	testUserSSHConfig  = "/var/lib/lab-test/.ssh/config"
	testKnownHosts     = "[10.88.0.1]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPinnedHostKeyForTests\n"
)

// --- the BastionAPI stub ------------------------------------------------------

// bastionAPIStub is a hand-written BastionAPI: scripted answers, scripted
// errors, and a record of every call. onAdd runs inside AddPublicKey (after
// the key is "registered"), which is how a test plants an obstruction in the
// run's tree at exactly the moment between registration and the file writes.
type bastionAPIStub struct {
	mu sync.Mutex

	identity   warpgate.Identity
	ensureErr  error
	targets    []warpgate.Target
	targetsErr error
	addErr     error
	onAdd      func(userID, label string)
	removeErr  error

	// The sweep's inputs: identities by repo id (absent = no user), and keys
	// by user id; errors by repo id / user id.
	identities map[string]warpgate.Identity
	findErr    map[string]error
	keys       map[string][]warpgate.PublicKey
	listErr    map[string]error

	ensured     []string // repo ids
	targetReads []string // role ids
	added       []addedKey
	finds       []string // repo ids
	listed      []string // user ids
	removed     []removedKey
}

type addedKey struct{ userID, label, authorizedKey string }

type removedKey struct{ userID, keyID string }

// newBastionStub answers for one repo: its identity (user + role, username
// the repo slug), and targets as the role's fresh SSH target read.
func newBastionStub(repoID string, targets ...warpgate.Target) *bastionAPIStub {
	return &bastionAPIStub{
		identity: warpgate.Identity{
			User: warpgate.User{ID: testWarpgateUserID, Username: warpgate.RepoSlug(repoID)},
			Role: warpgate.Role{ID: testWarpgateRoleID, Name: warpgate.RepoSlug(repoID)},
		},
		targets:    targets,
		identities: map[string]warpgate.Identity{},
		findErr:    map[string]error{},
		keys:       map[string][]warpgate.PublicKey{},
		listErr:    map[string]error{},
	}
}

func (s *bastionAPIStub) EnsureRepoIdentity(_ context.Context, repoID, _ string) (warpgate.Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensured = append(s.ensured, repoID)
	if s.ensureErr != nil {
		return warpgate.Identity{}, s.ensureErr
	}
	return s.identity, nil
}

func (s *bastionAPIStub) FindRepoIdentity(_ context.Context, repoID string) (warpgate.Identity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finds = append(s.finds, repoID)
	if err := s.findErr[repoID]; err != nil {
		return warpgate.Identity{}, false, err
	}
	id, ok := s.identities[repoID]
	return id, ok && id.User.ID != "" && id.Role.ID != "", nil
}

func (s *bastionAPIStub) RoleSSHTargets(_ context.Context, roleID string) ([]warpgate.Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targetReads = append(s.targetReads, roleID)
	if s.targetsErr != nil {
		return nil, s.targetsErr
	}
	return append([]warpgate.Target(nil), s.targets...), nil
}

func (s *bastionAPIStub) AddPublicKey(_ context.Context, userID, label, authorizedKey string) (warpgate.PublicKey, error) {
	s.mu.Lock()
	s.added = append(s.added, addedKey{userID: userID, label: label, authorizedKey: authorizedKey})
	n := len(s.added)
	err, onAdd := s.addErr, s.onAdd
	s.mu.Unlock()
	if err != nil {
		return warpgate.PublicKey{}, err
	}
	if onAdd != nil {
		onAdd(userID, label)
	}
	return warpgate.PublicKey{ID: fmt.Sprintf("wg-key-%d", n), Label: label}, nil
}

func (s *bastionAPIStub) ListPublicKeys(_ context.Context, userID string) ([]warpgate.PublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed = append(s.listed, userID)
	if err := s.listErr[userID]; err != nil {
		return nil, err
	}
	return append([]warpgate.PublicKey(nil), s.keys[userID]...), nil
}

func (s *bastionAPIStub) RemovePublicKey(ctx context.Context, userID, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		// A revocation must survive its caller's cancelled context
		// (RevokeBastionKey detaches it); a cancelled one reaching here is
		// the bug.
		return fmt.Errorf("RemovePublicKey called with a done context: %w", ctx.Err())
	}
	s.removed = append(s.removed, removedKey{userID: userID, keyID: keyID})
	return s.removeErr
}

// calls is every call the stub has seen, of any kind.
func (s *bastionAPIStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ensured) + len(s.targetReads) + len(s.added) + len(s.finds) + len(s.listed) + len(s.removed)
}

func (s *bastionAPIStub) addedKeys() []addedKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]addedKey(nil), s.added...)
}

func (s *bastionAPIStub) removedKeys() []removedKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]removedKey(nil), s.removed...)
}

// hostKeysStub is the BastionHostKeys seam: the known_hosts text a matching
// pin renders, or the error a mismatch/unreachable listener produces.
type hostKeysStub struct {
	mu    sync.Mutex
	text  string
	err   error
	calls int
}

func (h *hostKeysStub) KnownHosts(context.Context) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return h.text, h.err
}

func (h *hostKeysStub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// --- fixture wiring -----------------------------------------------------------

// enableBastion wires the fixture service's Warpgate seam fully (all three
// halves bastionActive gates on), with the per-host PATH and per-user config
// lookups pinned to constants. Internal-package field pokes, the same values
// cmd/lab would pass through instance.Options — enableGateway's shape.
func (f *fixture) enableBastion(t *testing.T, api *bastionAPIStub, keys *hostKeysStub) {
	t.Helper()
	f.svc.warpgate = api
	f.svc.warpgateSSHAddr = testBastionAddr
	f.svc.warpgateHostKeys = keys
	f.svc.hostPATH = func() string { return testHostPATH }
	f.svc.userSSHConfig = func() string { return testUserSSHConfig }
}

// cacheSSHTargets seeds the repo's lab-side SSH target cache — the one thing
// that decides whether a spawn asks Warpgate at all.
func (f *fixture) cacheSSHTargets(t *testing.T, targets ...store.SSHTarget) {
	t.Helper()
	if err := f.st.ReplaceRepoSSHTargets(t.Context(), f.repo.ID, targets); err != nil {
		t.Fatalf("ReplaceRepoSSHTargets: %v", err)
	}
}

// syncBuffer is a goroutine-safe log sink (Launch arms background capture
// goroutines that may log while a test reads).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs points the service's logger at a buffer, debug level included.
func (f *fixture) captureLogs() *syncBuffer {
	buf := &syncBuffer{}
	f.svc.log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return buf
}

// spawnSnapshot is what a spawn produced, normalized so two fixtures (two
// temp state dirs, two repo ids, two run ids, two tokens) compare equal
// exactly when their spawns are byte-identical up to those identities.
type spawnSnapshot struct {
	env     []string
	argv    []string
	context string
}

func (f *fixture) snapshot(t *testing.T, run store.Run) spawnSnapshot {
	t.Helper()
	sess, live := f.runner.Session(run.SessionName)
	if !live {
		t.Fatalf("session %s not live", run.SessionName)
	}
	token := envValue(sess.ExtraEnv, "LAB_TOKEN")
	if token == "" {
		t.Fatal("spawn env carries no LAB_TOKEN")
	}
	r := strings.NewReplacer(
		f.instancesDir, "<instances>", f.worktreeRoot, "<worktrees>", f.reposDir, "<repos>",
		f.repo.ID, "<repo>", run.ID, "<run>", token, "<token>")
	norm := func(in []string) []string {
		out := make([]string, len(in))
		for i, s := range in {
			out[i] = r.Replace(s)
		}
		return out
	}
	return spawnSnapshot{env: norm(sess.ExtraEnv), argv: norm(sess.Argv), context: contextFile(t, sess.Dir)}
}

// assertSameSpawn fails unless got is byte-identical to want (after
// normalization) — env, argv, and context file.
func assertSameSpawn(t *testing.T, got, want spawnSnapshot) {
	t.Helper()
	if !slices.Equal(got.env, want.env) {
		t.Errorf("spawn env differs from the unwired baseline:\n got  %q\n want %q", got.env, want.env)
	}
	if !slices.Equal(got.argv, want.argv) {
		t.Errorf("spawn argv differs from the unwired baseline:\n got  %q\n want %q", got.argv, want.argv)
	}
	if got.context != want.context {
		t.Errorf("context file differs from the unwired baseline:\ngot\n%s\nwant\n%s", got.context, want.context)
	}
}

// assertNoBastionFiles fails if the run's tree carries anything of the
// bastion wiring.
func assertNoBastionFiles(t *testing.T, f *fixture, run store.Run) {
	t.Helper()
	p := newBastionPaths(f.homes.RuntimePath(run.ID), f.homes.HomePath(run.ID))
	for _, path := range []string{p.key, p.knownHosts, p.config, p.binDir, p.marker, p.homeSSHDir} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("unwired run has %s (lstat err %v)", path, err)
		}
	}
}

// baselineSpawn is the unwired reference: a fresh fixture that never heard of
// Warpgate, one manual Start, container runner when asked.
func baselineSpawn(t *testing.T, container bool) spawnSnapshot {
	t.Helper()
	f := newFixture(t)
	if container {
		f.enableContainer(t)
	}
	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("baseline Start: %v", err)
	}
	return f.snapshot(t, run)
}

// --- 1. parity: an unwired spawn is byte-identical ---------------------------

// Issue #39's first acceptance criterion, asserted the way the OneCLI parity
// test asserts #24's: with the wiring OFF a spawn is indistinguishable from a
// lab built before the bastion existed — env, argv (the podman argv for a
// container run) and context file, byte for byte — and Warpgate saw ZERO
// calls. Three OFF shapes, for both runners:
//
//   - Warpgate not configured at all;
//   - the REST pair only (no --warpgate-ssh-addr), even for a repo whose cache
//     says it HAS targets — "unconfigured for this purpose, not a refusal";
//   - fully configured, but the repo has no SSH target (empty cache): a
//     grant-free spawn makes no Warpgate call and writes nothing.
func TestLaunch_BastionUnwiredParity(t *testing.T) {
	for _, runner := range []string{"host", "container"} {
		container := runner == "container"
		t.Run(runner, func(t *testing.T) {
			want := baselineSpawn(t, container)
			cases := []struct {
				name string
				wire func(t *testing.T, f *fixture, api *bastionAPIStub, keys *hostKeysStub)
			}{
				{"not configured", func(*testing.T, *fixture, *bastionAPIStub, *hostKeysStub) {}},
				{"REST pair only, repo has cached targets", func(t *testing.T, f *fixture, api *bastionAPIStub, _ *hostKeysStub) {
					f.svc.warpgate = api // no SSH address, no host-key pin
					f.cacheSSHTargets(t, store.SSHTarget{ID: "t1", Name: "staging"})
				}},
				{"configured, repo has no SSH target", func(t *testing.T, f *fixture, api *bastionAPIStub, keys *hostKeysStub) {
					f.enableBastion(t, api, keys)
				}},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					f := newFixture(t)
					if container {
						f.enableContainer(t)
					}
					api := newBastionStub(f.repo.ID, warpgate.Target{ID: "t1", Name: "staging"})
					keys := &hostKeysStub{text: testKnownHosts}
					tc.wire(t, f, api, keys)
					f.svc.userSSHConfig = func() string {
						t.Error("the per-user ssh config was looked up for an unwired run")
						return ""
					}

					run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
					if err != nil {
						t.Fatalf("Start: %v", err)
					}
					assertSameSpawn(t, f.snapshot(t, run), want)
					assertNoBastionFiles(t, f, run)
					if n := api.calls(); n != 0 {
						t.Errorf("Warpgate saw %d calls for an unwired spawn; want none", n)
					}
					if n := keys.count(); n != 0 {
						t.Errorf("the host key was scanned %d times for an unwired spawn; want none", n)
					}
					sess, _ := f.runner.Session(run.SessionName)
					for _, kv := range sess.ExtraEnv {
						if strings.HasPrefix(kv, "PATH=") {
							t.Errorf("unwired spawn env carries %q", kv)
						}
					}
				})
			}
		})
	}
}

// --- 2. fail closed, before the claim ----------------------------------------

// A target-bearing spawn with a bastion that cannot be used refuses with an
// actionable 400 BEFORE the claim — no worktree, no branch, no run row, no
// session, no per-run tree, no start-guard mark, and no key registered. Each
// row breaks a different pre-claim step: Warpgate unreachable at the identity
// heal, the fresh target read failing, and the host key no longer matching
// lab's pin.
func TestLaunch_BastionRefusesBeforeTheClaim(t *testing.T) {
	cases := []struct {
		name    string
		wire    func(api *bastionAPIStub, keys *hostKeysStub)
		wantMsg []string
		scanned int
	}{
		{
			name: "Warpgate unreachable",
			wire: func(api *bastionAPIStub, _ *hostKeysStub) {
				api.ensureErr = errors.New(`warpgate GET /users: Get "https://localhost:8888/@warpgate/admin/api/users": dial tcp 127.0.0.1:8888: connect: connection refused`)
			},
			wantMsg: []string{
				"refusing to spawn for repo proj: its SSH targets need the Warpgate bastion, and resolving its Warpgate user and role failed",
				"connection refused",
			},
		},
		{
			name: "the role's targets cannot be read",
			wire: func(api *bastionAPIStub, _ *hostKeysStub) {
				api.targetsErr = errors.New("warpgate GET /role/wg-role-1/targets: 401 Unauthorized: check --warpgate-admin-token-file")
			},
			wantMsg: []string{
				"refusing to spawn for repo proj: its SSH targets need the Warpgate bastion, and reading its SSH targets from Warpgate failed",
				"check --warpgate-admin-token-file",
			},
		},
		{
			name: "the host key no longer matches the pin",
			wire: func(_ *bastionAPIStub, keys *hostKeysStub) {
				keys.err = errors.New("warpgate: the SSH host key(s) presented at 10.88.0.1:2222 [SHA256:new] do not match lab's pin [SHA256:old]; refusing to wire SSH targets")
			},
			wantMsg: []string{
				"refusing to spawn for repo proj: its SSH targets need the Warpgate bastion, and Warpgate's SSH host key could not be verified against lab's pin",
				"do not match lab's pin",
			},
			scanned: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			api := newBastionStub(f.repo.ID, warpgate.Target{ID: "t1", Name: "staging"})
			keys := &hostKeysStub{text: testKnownHosts}
			f.enableBastion(t, api, keys)
			f.cacheSSHTargets(t, store.SSHTarget{ID: "t1", Name: "staging"})
			tc.wire(api, keys)

			_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
			var bad *BadRequestError
			if !errors.As(err, &bad) {
				t.Fatalf("Start err = %T (%v), want *BadRequestError (the 400 mapping)", err, err)
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to contain %q", err, want)
				}
			}
			assertNothingClaimed(t, f)
			if snap := f.guard.Snapshot(); len(snap) != 0 {
				t.Errorf("startguard marked despite a pre-guard refusal: %v", snap)
			}
			if added := api.addedKeys(); len(added) != 0 {
				t.Errorf("a key was registered for a refused spawn: %+v", added)
			}
			if n := keys.count(); n != tc.scanned {
				t.Errorf("host key scanned %d times, want %d", n, tc.scanned)
			}
		})
	}
}

// Registering the run key is the one refusal that lands AFTER the per-run
// tree exists: still a 400, still before the claim, the tree wiped — and
// nothing to revoke, since nothing was registered.
func TestLaunch_BastionKeyRegistrationFailureRefuses(t *testing.T) {
	f := newFixture(t)
	api := newBastionStub(f.repo.ID, warpgate.Target{ID: "t1", Name: "staging"})
	api.addErr = errors.New("warpgate POST /users/wg-user-1/credentials/public-keys: 500 Internal Server Error")
	f.enableBastion(t, api, &hostKeysStub{text: testKnownHosts})
	f.cacheSSHTargets(t, store.SSHTarget{ID: "t1", Name: "staging"})

	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	var bad *BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("Start err = %T (%v), want *BadRequestError", err, err)
	}
	if !strings.Contains(err.Error(), "registering this run's key on its Warpgate user failed") {
		t.Errorf("refusal = %q, want it to name the key registration", err)
	}
	assertNothingClaimed(t, f)
	if removed := api.removedKeys(); len(removed) != 0 {
		t.Errorf("RemovePublicKey called %+v for a key that was never registered", removed)
	}
}

// --- 3. reaching Warpgate and finding nothing to wire ------------------------

// The cache said "targets", but the fresh read says none (they were
// unassigned in Warpgate's UI): the cache is CLEARED and the spawn proceeds
// exactly as an unwired one — no key, no files, no PATH change, no host-key
// scan. And the next spawn makes no Warpgate call at all.
func TestLaunch_BastionFreshReadEmptyClearsCacheAndSpawnsUnwired(t *testing.T) {
	want := baselineSpawn(t, false)
	f := newFixture(t)
	api := newBastionStub(f.repo.ID) // the role carries no target any more
	keys := &hostKeysStub{text: testKnownHosts}
	f.enableBastion(t, api, keys)
	f.cacheSSHTargets(t, store.SSHTarget{ID: "t1", Name: "staging"})

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertSameSpawn(t, f.snapshot(t, run), want)
	assertNoBastionFiles(t, f, run)
	if cached, err := f.st.RepoSSHTargets(t.Context(), f.repo.ID); err != nil || len(cached) != 0 {
		t.Errorf("cache after an empty fresh read = %+v, %v; want cleared", cached, err)
	}
	if added := api.addedKeys(); len(added) != 0 {
		t.Errorf("a key was registered with nothing to wire: %+v", added)
	}
	if n := keys.count(); n != 0 {
		t.Errorf("host key scanned %d times with nothing to wire; want none", n)
	}

	before := api.calls()
	if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "again"}); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if n := api.calls() - before; n != 0 {
		t.Errorf("the spawn after the cache was cleared made %d Warpgate calls; want none", n)
	}
}

// Every target the fresh read returns is skipped by the alias rule — here one
// named like the forge host (case differing) and one with a space: no key is
// registered, the spawn is unwired, and ONE warning names the skipped
// targets. The cache still records them (it answers "does this repo have
// targets", not "which are aliases").
func TestLaunch_BastionAllTargetsSkippedSpawnsUnwired(t *testing.T) {
	f := newFixture(t)
	logs := f.captureLogs()
	api := newBastionStub(f.repo.ID,
		warpgate.Target{ID: "t1", Name: "Git.Example.com"},
		warpgate.Target{ID: "t2", Name: "bad name"})
	keys := &hostKeysStub{text: testKnownHosts}
	f.enableBastion(t, api, keys)
	f.cacheSSHTargets(t, store.SSHTarget{ID: "t1", Name: "Git.Example.com"})

	repo := f.repo
	repo.RemoteURL = "git@git.example.com:Cloonar/coding-lab.git"
	wt := filepath.Join(f.worktreeRoot, "proj-skipped")
	run, err := f.svc.Launch(t.Context(), LaunchSpec{
		Repo: repo, Provider: f.prov, Kind: store.RunKindManual,
		SessionName: "proj~skipped", Branch: "lab/skipped", WorktreePath: wt,
		Model: "opus[1m]", Effort: "max",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	assertNoBastionFiles(t, f, run)
	if added := api.addedKeys(); len(added) != 0 {
		t.Errorf("a key was registered with no alias to use it: %+v", added)
	}
	if n := keys.count(); n != 0 {
		t.Errorf("host key scanned %d times with nothing to wire; want none", n)
	}
	sess, _ := f.runner.Session("proj~skipped")
	if got := envValue(sess.ExtraEnv, "PATH"); got != "" {
		t.Errorf("unwired spawn env PATH = %q, want none", got)
	}
	if local := contextFile(t, wt); strings.Contains(local, "## SSH targets") {
		t.Errorf("context file of an unwired run carries an SSH targets section:\n%s", local)
	}
	out := logs.String()
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Errorf("%d warnings logged, want exactly one:\n%s", n, out)
	}
	for _, want := range []string{"every SSH target of the repo was skipped", "Git.Example.com", "bad name"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning does not contain %q:\n%s", want, out)
		}
	}
	if cached, _ := f.st.RepoSSHTargets(t.Context(), f.repo.ID); len(cached) != 2 {
		t.Errorf("cache = %+v, want both fresh targets recorded", cached)
	}
}

// --- 4. the wired happy path, host runner ------------------------------------

// A target-bearing host spawn, end to end: the identity is healed, the role's
// targets are read fresh (the stale cache is replaced by them), the host key
// is checked once, ONE key is registered on the repo's user under
// lab-run:<runID> — a parseable ed25519 key whose private half is on disk —
// and the run's files exist with ADR-0068's exact modes, ~/.ssh/config
// resolving to the runtime config. The spawn env gains exactly one entry,
// PATH = wrapper dir + lab's PATH, and the seeder got the aliases (and only
// the aliases).
func TestLaunch_BastionWiredHostRunner(t *testing.T) {
	f := newFixture(t)
	logs := f.captureLogs()
	api := newBastionStub(f.repo.ID,
		warpgate.Target{ID: "t2", Name: "staging"},
		warpgate.Target{ID: "t1", Name: "build-box"},
		warpgate.Target{ID: "t3", Name: "bad name"})
	keys := &hostKeysStub{text: testKnownHosts}
	f.enableBastion(t, api, keys)
	f.cacheSSHTargets(t, store.SSHTarget{ID: "t2", Name: "staging"}) // stale: one of three

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	runtimeDir := f.homes.RuntimePath(run.ID)
	p := newBastionPaths(runtimeDir, f.homes.HomePath(run.ID))

	// Warpgate: heal, fresh read, one registration under the run label.
	if !slices.Equal(api.ensured, []string{f.repo.ID}) || !slices.Equal(api.targetReads, []string{testWarpgateRoleID}) {
		t.Errorf("identity heals %q / target reads %q, want one each for the repo / its role", api.ensured, api.targetReads)
	}
	added := api.addedKeys()
	if len(added) != 1 {
		t.Fatalf("keys registered = %+v, want exactly one", added)
	}
	if added[0].userID != testWarpgateUserID || added[0].label != warpgate.RunKeyLabel(run.ID) {
		t.Errorf("key registered as (%q, %q), want (%q, %q)", added[0].userID, added[0].label, testWarpgateUserID, warpgate.RunKeyLabel(run.ID))
	}
	pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(added[0].authorizedKey))
	if err != nil || pub.Type() != ssh.KeyAlgoED25519 || comment != warpgate.RunKeyLabel(run.ID) {
		t.Errorf("registered key %q: parse err %v, want an ed25519 key commented %q", added[0].authorizedKey, err, warpgate.RunKeyLabel(run.ID))
	}
	if n := keys.count(); n != 1 {
		t.Errorf("host key scanned %d times, want once", n)
	}
	if removed := api.removedKeys(); len(removed) != 0 {
		t.Errorf("a key was removed on the happy path: %+v", removed)
	}
	// The cache now mirrors the fresh read — all three, the skipped one too.
	cached, err := f.st.RepoSSHTargets(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []store.SSHTarget{{ID: "t3", Name: "bad name"}, {ID: "t1", Name: "build-box"}, {ID: "t2", Name: "staging"}}; !slices.Equal(cached, want) {
		t.Errorf("cache = %+v, want the fresh set %+v", cached, want)
	}

	// The files, with their exact modes.
	for path, mode := range map[string]os.FileMode{
		p.key: 0o600, p.knownHosts: 0o600, p.config: 0o600, p.marker: 0o600,
		p.binDir: 0o700, filepath.Join(p.binDir, "ssh"): 0o700, filepath.Join(p.binDir, "scp"): 0o700,
		filepath.Join(p.binDir, "sftp"): 0o700, p.homeSSHDir: 0o700,
	} {
		assertMode(t, path, mode)
	}
	priv, err := os.ReadFile(p.key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		t.Fatalf("the run key on disk does not parse: %v", err)
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), pub.Marshal()) {
		t.Error("the key on disk is not the key registered with Warpgate")
	}
	if got, _ := os.ReadFile(p.knownHosts); string(got) != testKnownHosts {
		t.Errorf("known_hosts = %q, want the pin rendered verbatim %q", got, testKnownHosts)
	}
	m, err := readBastionMarker(runtimeDir)
	if err != nil || m != (bastionMarker{UserID: testWarpgateUserID, KeyID: "wg-key-1"}) {
		t.Errorf("marker = %+v, %v; want the registered key's ids", m, err)
	}
	config, _ := os.ReadFile(p.config)
	wantConfig, err := renderBastionSSHConfig(bastionConfigSpec{
		runID: run.ID, username: warpgate.RepoSlug(f.repo.ID), addr: testBastionAddr,
		aliases: []string{"build-box", "staging"}, paths: p, userConfig: testUserSSHConfig,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(config) != wantConfig {
		t.Errorf("config =\n%s\nwant\n%s", config, wantConfig)
	}
	if resolved, err := filepath.EvalSymlinks(p.homeConfig); err != nil || resolved != mustEvalSymlinks(t, p.config) {
		t.Errorf("~/.ssh/config resolves to %q (%v), want the runtime config %q", resolved, err, p.config)
	}

	// The spawn env: exactly one PATH, the wrapper dir first.
	sess, _ := f.runner.Session(run.SessionName)
	var paths []string
	for _, kv := range sess.ExtraEnv {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			paths = append(paths, v)
		}
	}
	if want := []string{p.binDir + ":" + testHostPATH}; !slices.Equal(paths, want) {
		t.Errorf("spawn env PATH entries = %q, want %q", paths, want)
	}

	// The seeder got the aliases, and nothing else of the wiring.
	local := contextFile(t, sess.Dir)
	for _, want := range []string{"## SSH targets", "- `ssh build-box`\n- `ssh staging`\n"} {
		if !strings.Contains(local, want) {
			t.Errorf("context file missing %q:\n%s", want, local)
		}
	}
	for _, leak := range []string{"bad name", testBastionAddr, "10.88.0.1", warpgate.RepoSlug(f.repo.ID), runtimeDir} {
		if strings.Contains(local, leak) {
			t.Errorf("context file carries %q, which the seeder must never be handed:\n%s", leak, local)
		}
	}

	// The private key reached no env, no argv, no log line.
	keyBody := strings.TrimSpace(string(priv))
	for _, s := range append(append([]string{logs.String()}, sess.ExtraEnv...), sess.Argv...) {
		if strings.Contains(s, keyBody) || strings.Contains(s, "PRIVATE KEY") {
			t.Errorf("the run's private key leaked into %q", s)
		}
	}
	out := logs.String()
	if !strings.Contains(out, "SSH bastion wired for run") || !strings.Contains(out, ssh.FingerprintSHA256(pub)) {
		t.Errorf("no wiring log line naming the key's fingerprint:\n%s", out)
	}
	if !strings.Contains(out, "bad name") || strings.Count(out, "level=WARN") != 1 {
		t.Errorf("want exactly one warning naming the skipped target:\n%s", out)
	}
}

// mustEvalSymlinks resolves path or fails the test.
func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	return resolved
}

// --- 5. the wired happy path, container runner -------------------------------

// A target-bearing CONTAINER spawn, against the exact-argv comparison the
// other container tests use: the only difference from an unwired container
// argv is PATH, which is still EXACTLY ONE entry — the wrapper dir (a runtime
// path, valid inside through the host-identical runtime bind) in front of
// podmanx.PATH — and the host's PATH is not forwarded. The tmux payload stays
// the secret forward alone. The config re-includes only the system config
// (inside a keep-id container ssh never read a per-user one), and the
// per-user lookup is never consulted.
func TestStart_ContainerRunnerBastionPATH(t *testing.T) {
	f := newFixture(t)
	f.enableContainer(t)
	api := newBastionStub(f.repo.ID, warpgate.Target{ID: "t1", Name: "staging"})
	f.enableBastion(t, api, &hostKeysStub{text: testKnownHosts})
	f.svc.userSSHConfig = func() string {
		t.Error("the per-user ssh config was looked up for a container run")
		return testUserSSHConfig
	}
	f.cacheSSHTargets(t, store.SSHTarget{ID: "t1", Name: "staging"})

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	name := "proj~20260608-1530"
	sess, live := f.runner.Session(name)
	if !live {
		t.Fatal("session not live after a wired container Start")
	}
	runtimeDir := f.homes.RuntimePath(run.ID)
	p := newBastionPaths(runtimeDir, f.homes.HomePath(run.ID))

	wantArgv := podmanx.RunArgv(podmanx.RunSpec{
		Bin:         testPodmanBin,
		Name:        podmanx.ContainerName(name),
		Image:       testDevImage,
		ToolsImage:  testToolsImage,
		WorktreeDir: filepath.Join(f.worktreeRoot, "proj-20260608-1530"),
		BareDir:     f.bare(),
		AgentDir:    "/var/lib/lab-test/agent",
		HomeDir:     f.homes.HomePath(run.ID),
		RuntimeDir:  runtimeDir,
		Memory:      "8g",
		Pids:        4096,
		Nofile:      16384,
		Env: []string{
			"LAB_URL=unix:///var/lib/lab-test/agent/agent.sock",
			"HOME=" + podmanx.Home,
			"PATH=" + p.binDir + ":" + podmanx.PATH,
		},
		ForwardEnv: []string{"LAB_TOKEN", "TERM"},
		Argv:       f.wantSpawnArgv(name, run.Model, run.Effort, "", run.ID),
	})
	if !slices.Equal(sess.Argv, wantArgv) {
		t.Errorf("container pane argv =\n  %q\nwant\n  %q", sess.Argv, wantArgv)
	}
	if n := strings.Count(strings.Join(sess.Argv, "\n"), "\nPATH="); n != 1 {
		t.Errorf("container argv carries %d PATH entries, want exactly one", n)
	}
	if strings.Contains(strings.Join(sess.Argv, " "), testHostPATH) {
		t.Error("the host's PATH was forwarded into the container")
	}
	if len(sess.ExtraEnv) != 1 || !strings.HasPrefix(sess.ExtraEnv[0], "LAB_TOKEN=") {
		t.Errorf("container tmux env = %q, want exactly [LAB_TOKEN=…]", sess.ExtraEnv)
	}
	config, err := os.ReadFile(p.config)
	if err != nil {
		t.Fatalf("container run has no ssh config: %v", err)
	}
	if !strings.HasSuffix(string(config), "Match all\nInclude /etc/ssh/ssh_config\n") {
		t.Errorf("container config tail should include only the system config:\n%s", config)
	}
	// The ~/.ssh/config link names the runtime path, which the container sees
	// at the same absolute path.
	if target, err := os.Readlink(p.homeConfig); err != nil || target != p.config {
		t.Errorf("~/.ssh/config -> %q (%v), want %q", target, err, p.config)
	}
	if bind := runtimeDir + ":" + runtimeDir; !slices.Contains(sess.Argv, bind) {
		t.Errorf("pane argv carries no host-identical runtime bind %q", bind)
	}
}

// --- 6. every failure after registration revokes the key ---------------------

// The key is registered, the marker written, then writing the run's files
// fails (an obstruction planted at the private key's path the moment
// Warpgate answered): the spawn fails as a StartFailedError before the
// claim, the tree is wiped, and the registered key is REMOVED from Warpgate —
// by Launch's own rollback, with no pre-wipe hook installed.
func TestLaunch_BastionWriteFailureRevokesTheKey(t *testing.T) {
	f := newFixture(t)
	api := newBastionStub(f.repo.ID, warpgate.Target{ID: "t1", Name: "staging"})
	api.onAdd = func(_, label string) {
		runID, _ := warpgate.RunIDFromLabel(label)
		if err := os.Mkdir(filepath.Join(f.homes.RuntimePath(runID), bastionKeyName), 0o700); err != nil {
			t.Errorf("planting the obstruction: %v", err)
		}
	}
	f.enableBastion(t, api, &hostKeysStub{text: testKnownHosts})
	f.cacheSSHTargets(t, store.SSHTarget{ID: "t1", Name: "staging"})

	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	var failed *StartFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("Start err = %T (%v), want *StartFailedError", err, err)
	}
	assertNothingClaimed(t, f)
	if want := []removedKey{{userID: testWarpgateUserID, keyID: "wg-key-1"}}; !slices.Equal(api.removedKeys(), want) {
		t.Errorf("keys removed = %+v, want the registered key revoked once %+v", api.removedKeys(), want)
	}
}

// The marker itself cannot be written: no file on disk names the key, so the
// launch revokes it from memory — once — and the rollback's marker-driven
// revoke finds nothing to do.
func TestLaunch_BastionMarkerWriteFailureRevokesFromMemory(t *testing.T) {
	f := newFixture(t)
	api := newBastionStub(f.repo.ID, warpgate.Target{ID: "t1", Name: "staging"})
	api.onAdd = func(_, label string) {
		runID, _ := warpgate.RunIDFromLabel(label)
		if err := os.Mkdir(filepath.Join(f.homes.RuntimePath(runID), bastionMarkerName), 0o700); err != nil {
			t.Errorf("planting the obstruction: %v", err)
		}
	}
	f.enableBastion(t, api, &hostKeysStub{text: testKnownHosts})
	f.cacheSSHTargets(t, store.SSHTarget{ID: "t1", Name: "staging"})

	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	var failed *StartFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("Start err = %T (%v), want *StartFailedError", err, err)
	}
	assertNothingClaimed(t, f)
	if want := []removedKey{{userID: testWarpgateUserID, keyID: "wg-key-1"}}; !slices.Equal(api.removedKeys(), want) {
		t.Errorf("keys removed = %+v, want exactly one revocation %+v", api.removedKeys(), want)
	}
}

// A failure AFTER the claim (the spawn itself) rolls the whole launch back,
// and the rollback's wipe revokes the key too.
func TestLaunch_BastionPostClaimRollbackRevokesTheKey(t *testing.T) {
	f := newFixture(t)
	api := newBastionStub(f.repo.ID, warpgate.Target{ID: "t1", Name: "staging"})
	f.enableBastion(t, api, &hostKeysStub{text: testKnownHosts})
	f.cacheSSHTargets(t, store.SSHTarget{ID: "t1", Name: "staging"})
	f.runner.FailStart("proj~20260608-1530", errors.New("boom: session exited"))

	_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	var failed *StartFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("Start err = %T (%v), want *StartFailedError", err, err)
	}
	assertNothingClaimed(t, f)
	if want := []removedKey{{userID: testWarpgateUserID, keyID: "wg-key-1"}}; !slices.Equal(api.removedKeys(), want) {
		t.Errorf("keys removed = %+v, want %+v", api.removedKeys(), want)
	}
}

// Stop revokes through the pre-wipe hook, composed the way cmd/lab composes
// it: the hook calls RevokeBastionKey, which finds the marker in the tree
// Wipe is about to remove. One removal, for the run's own key.
func TestStop_BastionKeyRevokedThroughThePreWipeHook(t *testing.T) {
	f := newFixture(t)
	api := newBastionStub(f.repo.ID, warpgate.Target{ID: "t1", Name: "staging"})
	f.enableBastion(t, api, &hostKeysStub{text: testKnownHosts})
	f.cacheSSHTargets(t, store.SSHTarget{ID: "t1", Name: "staging"})
	f.homes.SetPreWipeHook(func(runID string) { f.svc.RevokeBastionKey(context.Background(), runID) })

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if removed := api.removedKeys(); len(removed) != 0 {
		t.Fatalf("key removed before Stop: %+v", removed)
	}
	if _, err := f.svc.Stop(t.Context(), run.SessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if want := []removedKey{{userID: testWarpgateUserID, keyID: "wg-key-1"}}; !slices.Equal(api.removedKeys(), want) {
		t.Errorf("keys removed = %+v, want the run's key revoked once %+v", api.removedKeys(), want)
	}
	if dirExists(filepath.Join(f.instancesDir, run.ID)) {
		t.Error("the run's tree survived Stop")
	}
}

// --- 7. RevokeBastionKey ------------------------------------------------------

// revokeService is a Service with just what RevokeBastionKey and
// SweepBastionKeys touch — no git fixture needed for the per-run cases.
func revokeService(t *testing.T, api BastionAPI) (*Service, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	return &Service{
		homes:    newTestHomes(t),
		warpgate: api,
		log:      slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, buf
}

// newTestHomes is an instance-home manager over a fresh temp root.
func newTestHomes(t *testing.T) *instancehome.Manager {
	t.Helper()
	return instancehome.New(filepath.Join(t.TempDir(), "instances"))
}

// plantMarker materializes runID's tree and writes its marker.
func plantMarker(t *testing.T, s *Service, runID string, m bastionMarker) string {
	t.Helper()
	if _, err := s.homes.Materialize(runID); err != nil {
		t.Fatal(err)
	}
	if err := writeBastionMarker(s.homes.RuntimePath(runID), m); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(s.homes.RuntimePath(runID), bastionMarkerName)
}

// RevokeBastionKey's whole contract: marker present → that one key removed and
// the marker deleted, so a second call does nothing; no marker → no call; a
// failed removal → one warning and the marker KEPT for a later wipe path to
// retry; a cancelled caller context still revokes; no REST client, a garbage
// marker, or a path-climbing run id → nothing, and never a panic.
func TestRevokeBastionKey(t *testing.T) {
	runID := ids.NewID("run")
	want := bastionMarker{UserID: "wg-user-9", KeyID: "wg-key-9"}

	t.Run("marker present: removed once, marker deleted", func(t *testing.T) {
		api := newBastionStub("repo_x")
		s, _ := revokeService(t, api)
		marker := plantMarker(t, s, runID, want)
		s.RevokeBastionKey(t.Context(), runID)
		s.RevokeBastionKey(t.Context(), runID) // the hook's second look
		if got := api.removedKeys(); !slices.Equal(got, []removedKey{{userID: "wg-user-9", keyID: "wg-key-9"}}) {
			t.Errorf("removed = %+v, want exactly the marked key once", got)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("marker survived a successful revoke (stat err %v)", err)
		}
	})

	t.Run("a cancelled caller context still revokes", func(t *testing.T) {
		api := newBastionStub("repo_x")
		s, _ := revokeService(t, api)
		plantMarker(t, s, runID, want)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		s.RevokeBastionKey(ctx, runID)
		if got := api.removedKeys(); len(got) != 1 {
			t.Errorf("removed = %+v, want the key revoked despite the cancelled context", got)
		}
	})

	t.Run("no marker: no call", func(t *testing.T) {
		api := newBastionStub("repo_x")
		s, logs := revokeService(t, api)
		if _, err := s.homes.Materialize(runID); err != nil {
			t.Fatal(err)
		}
		s.RevokeBastionKey(t.Context(), runID)
		s.RevokeBastionKey(t.Context(), ids.NewID("run")) // no tree at all
		if n := api.calls(); n != 0 {
			t.Errorf("Warpgate saw %d calls for a run without a marker", n)
		}
		if out := logs.String(); out != "" {
			t.Errorf("an unwired run's revoke logged:\n%s", out)
		}
	})

	t.Run("removal fails: warned, marker kept", func(t *testing.T) {
		api := newBastionStub("repo_x")
		api.removeErr = errors.New("warpgate DELETE /users/wg-user-9/credentials/public-keys/wg-key-9: 503 Service Unavailable")
		s, logs := revokeService(t, api)
		marker := plantMarker(t, s, runID, want)
		s.RevokeBastionKey(t.Context(), runID)
		out := logs.String()
		if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, runID) || !strings.Contains(out, "503") {
			t.Errorf("want one warning naming the run and the cause:\n%s", out)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Errorf("marker gone after a FAILED revoke: %v", err)
		}
	})

	t.Run("no REST client: no-op", func(t *testing.T) {
		s, _ := revokeService(t, nil)
		plantMarker(t, s, runID, want)
		s.RevokeBastionKey(t.Context(), runID) // must not panic
	})

	t.Run("garbage marker or a climbing run id: no call", func(t *testing.T) {
		api := newBastionStub("repo_x")
		s, _ := revokeService(t, api)
		if _, err := s.homes.Materialize(runID); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(s.homes.RuntimePath(runID), bastionMarkerName), "{not json")
		s.RevokeBastionKey(t.Context(), runID)
		s.RevokeBastionKey(t.Context(), "../"+runID)
		s.RevokeBastionKey(t.Context(), "")
		if n := api.calls(); n != 0 {
			t.Errorf("Warpgate saw %d calls", n)
		}
	})
}

// --- 8. SweepBastionKeys ------------------------------------------------------

// The startup sweep removes exactly the orphans: keys whose label is a lab run
// label naming a run that is not active. It keeps an active run's key, an
// operator's own key, a label that only looks like lab's, and a key whose
// run's marker is still on disk (a launch in flight, or a tree whose own wipe
// will revoke it); it sweeps a user whose role is missing, skips a repo with
// no Warpgate user, and reports what it removed.
func TestSweepBastionKeys_removesOnlyOrphanedRunKeys(t *testing.T) {
	f := newFixture(t)
	logs := f.captureLogs()
	api := newBastionStub(f.repo.ID)
	f.svc.warpgate = api // the REST client alone is enough to sweep

	active := ids.NewID("run")
	if _, err := f.st.CreateRun(t.Context(), store.Run{
		ID: active, RepoID: f.repo.ID, Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/x", WorktreePath: "/tmp/unused", SessionName: "proj~x",
		Model: "opus", Effort: "max", StartedAt: f.clock.Now(),
	}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	orphan, marked, staleMarked := ids.NewID("run"), ids.NewID("run"), ids.NewID("run")
	plantMarker(t, f.svc, marked, bastionMarker{UserID: "u-proj", KeyID: "k-marked"})
	plantMarker(t, f.svc, staleMarked, bastionMarker{UserID: "u-proj", KeyID: "k-some-other-key"})

	alpha := f.addRepo(t, "alpha")
	zeta := f.addRepo(t, "zeta")
	zetaOrphan := ids.NewID("run")
	api.identities[f.repo.ID] = warpgate.Identity{User: warpgate.User{ID: "u-proj"}, Role: warpgate.Role{ID: "r-proj"}}
	api.identities[zeta.ID] = warpgate.Identity{User: warpgate.User{ID: "u-zeta"}} // role missing: still swept
	api.keys["u-proj"] = []warpgate.PublicKey{
		{ID: "k-active", Label: warpgate.RunKeyLabel(active)},
		{ID: "k-orphan", Label: warpgate.RunKeyLabel(orphan)},
		{ID: "k-operator", Label: "dominik's laptop"},
		{ID: "k-lookalike", Label: "lab-run:not-a-run-id"},
		{ID: "k-marked", Label: warpgate.RunKeyLabel(marked)},
		{ID: "k-stale", Label: warpgate.RunKeyLabel(staleMarked)},
	}
	api.keys["u-zeta"] = []warpgate.PublicKey{{ID: "k-zeta", Label: warpgate.RunKeyLabel(zetaOrphan)}}

	f.svc.SweepBastionKeys(t.Context())

	want := []removedKey{{"u-proj", "k-orphan"}, {"u-proj", "k-stale"}, {"u-zeta", "k-zeta"}}
	if got := api.removedKeys(); !slices.Equal(got, want) {
		t.Errorf("removed = %+v, want exactly the orphans %+v", got, want)
	}
	if !slices.Equal(api.finds, []string{alpha.ID, f.repo.ID, zeta.ID}) {
		t.Errorf("identity lookups = %q, want every repo once, in name order", api.finds)
	}
	if !slices.Equal(api.listed, []string{"u-proj", "u-zeta"}) {
		t.Errorf("key listings = %q, want only the users that exist", api.listed)
	}
	if len(api.ensured) != 0 {
		t.Errorf("the sweep WROTE identities %q; it must be read-only", api.ensured)
	}
	out := logs.String()
	if !strings.Contains(out, "removed orphaned Warpgate run keys") || !strings.Contains(out, "removed=3") {
		t.Errorf("want one info line reporting 3 removals:\n%s", out)
	}
	if strings.Contains(out, "level=WARN") {
		t.Errorf("a clean sweep warned:\n%s", out)
	}
}

// The sweep stops at the FIRST Warpgate error with ONE warning naming the repo
// and how many repos it never reached — the same outage waiting for every
// remaining repo is not worth a wall of identical warnings. What it removed
// before the error is still reported.
func TestSweepBastionKeys_stopsAtTheFirstError(t *testing.T) {
	f := newFixture(t)
	logs := f.captureLogs()
	api := newBastionStub(f.repo.ID)
	f.svc.warpgate = api
	alpha := f.addRepo(t, "alpha")
	zeta := f.addRepo(t, "zeta")
	api.identities[alpha.ID] = warpgate.Identity{User: warpgate.User{ID: "u-alpha"}, Role: warpgate.Role{ID: "r"}}
	api.keys["u-alpha"] = []warpgate.PublicKey{{ID: "k-alpha", Label: warpgate.RunKeyLabel(ids.NewID("run"))}}
	api.findErr[f.repo.ID] = errors.New("warpgate GET /users: 401 Unauthorized: check --warpgate-admin-token-file")

	f.svc.SweepBastionKeys(t.Context())

	if got := api.removedKeys(); !slices.Equal(got, []removedKey{{"u-alpha", "k-alpha"}}) {
		t.Errorf("removed = %+v, want alpha's orphan (swept before the error)", got)
	}
	if slices.Contains(api.finds, zeta.ID) {
		t.Error("the sweep pressed on past the first error")
	}
	out := logs.String()
	if n := strings.Count(out, "level=WARN"); n != 1 {
		t.Errorf("%d warnings, want exactly one:\n%s", n, out)
	}
	for _, want := range []string{"repo=" + f.repo.ID, "skipped=1", "401 Unauthorized"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning does not contain %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "removed=1") {
		t.Errorf("the removal before the error was not reported:\n%s", out)
	}
}

// No REST client: the sweep is a no-op (no store read worth logging, no call).
func TestSweepBastionKeys_noClientIsNoOp(t *testing.T) {
	f := newFixture(t)
	logs := f.captureLogs()
	f.svc.SweepBastionKeys(t.Context())
	if out := logs.String(); out != "" {
		t.Errorf("an unconfigured sweep logged:\n%s", out)
	}
}

// addRepo inserts a second repo row named name (the sweep only reads rows).
func (f *fixture) addRepo(t *testing.T, name string) store.Repo {
	t.Helper()
	repo, err := f.st.CreateRepo(t.Context(), store.Repo{
		ID: ids.NewID("repo"), Name: name, RemoteURL: "file:///srv/" + name,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: clockTime,
	})
	if err != nil {
		t.Fatalf("CreateRepo(%s): %v", name, err)
	}
	return repo
}
