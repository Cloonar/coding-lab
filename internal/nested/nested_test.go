package nested

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// wait blocks until every background goroutine started so far has returned
// — the tests' synchronization point.
func (s *Service) wait() { s.bg.Wait() }

// fakeSettings scripts the two store reads: settings rows (absent key →
// store.ErrNotFound), per-key read errors, and the repo half of the gate.
type fakeSettings struct {
	mu       sync.Mutex
	rows     map[string]string
	errs     map[string]error
	anyRepo  bool
	repoErr  error
	repoReqs int
}

func (f *fakeSettings) GetSetting(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs[key]; err != nil {
		return "", err
	}
	v, ok := f.rows[key]
	if !ok {
		return "", store.ErrNotFound
	}
	return v, nil
}

func (f *fakeSettings) AnyRepoNestedContainers(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repoReqs++
	return f.anyRepo, f.repoErr
}

func (f *fakeSettings) set(key, v string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[key] = v
}

// recorder is a podmanx.CmdRunner that records every argv and answers
// through an optional handler (nil: empty output, no error).
type recorder struct {
	mu     sync.Mutex
	calls  [][]string
	handle func(argv []string) ([]byte, error)
}

func (r *recorder) run(_ context.Context, name string, args ...string) ([]byte, error) {
	argv := append([]string{name}, args...)
	r.mu.Lock()
	r.calls = append(r.calls, argv)
	h := r.handle
	r.mu.Unlock()
	if h == nil {
		return nil, nil
	}
	return h(argv)
}

func (r *recorder) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// probeRuns counts the nested probe's `podman … run …` invocations (the
// pre-clean and backstop `rm`s are not runs).
func (r *recorder) probeRuns() int {
	n := 0
	for _, c := range r.recorded() {
		if slices.Contains(c, "run") && slices.Contains(c, "--self-test") {
			n++
		}
	}
	return n
}

// greenDeps are NestedPreflight's probes on a host that passes: the tun
// device usable, podman on PATH, and a self-test that prints its ok lines.
func greenDeps(run *recorder) podmanx.Deps {
	run.handle = func(argv []string) ([]byte, error) {
		if slices.Contains(argv, "--self-test") {
			return []byte("ok: cgroup-child\nok: cgroup-limits\n"), nil
		}
		return nil, nil
	}
	return podmanx.Deps{
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		AccessRW: func(string) error { return nil },
		Run:      run.run,
		UID:      990,
		GID:      991,
	}
}

// logBuf captures the service's log as text.
type logBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type harness struct {
	svc      *Service
	settings *fakeSettings
	probe    *recorder // NestedPreflight's Deps.Run
	pulls    *recorder // Config.Run (the shared-store pulls)
	log      *logBuf
	state    string
	verdicts atomic.Int32
}

func newHarness(t *testing.T, mod func(*Config)) *harness {
	t.Helper()
	h := &harness{
		settings: &fakeSettings{rows: map[string]string{}, errs: map[string]error{}},
		probe:    &recorder{},
		pulls:    &recorder{},
		log:      &logBuf{},
		state:    t.TempDir(),
	}
	cfg := Config{
		PodmanBin: "podman",
		StateDir:  h.state,
		// Sorted provider order picks codex's ref, not claude-code's
		// blank one.
		ToolsImages: map[string]string{"zz": "tools@sha256:zz", "codex": "tools@sha256:codex", "claude-code": ""},
		Settings:    h.settings,
		Run:         h.pulls.run,
		Deps:        greenDeps(h.probe),
		Log:         slog.New(slog.NewTextHandler(h.log, nil)),
		OnVerdict:   func() { h.verdicts.Add(1) },
	}
	if mod != nil {
		mod(&cfg)
	}
	h.svc = New(t.Context(), cfg)
	h.svc.pollEvery = time.Millisecond
	t.Cleanup(h.svc.wait)
	return h
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// Kick is idempotent: however many triggers fire — concurrently, before and
// after the verdict — the probe container runs exactly once, the verdict is
// published once, OnVerdict fires once, and the pass is logged. The first
// call also creates the shared store's directory, so a nested run's mount
// is stable before any image is cached.
func TestKick_runsTheChecksOnce(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.svc.WriteSeccomp(); err != nil {
		t.Fatalf("WriteSeccomp: %v", err)
	}
	if _, done := h.svc.Verdict(); done {
		t.Fatal("Verdict done before any Kick")
	}
	if h.svc.SharedStore() != "" {
		t.Fatal("SharedStore non-empty before the store dir exists")
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(h.svc.Kick)
	}
	wg.Wait()
	if !dirExists(podmanx.SharedStorePath(h.state)) {
		t.Error("Kick returned without creating the shared store dir")
	}
	h.svc.wait()
	h.svc.Kick() // after the verdict: a no-op too
	h.svc.wait()

	if n := h.probe.probeRuns(); n != 1 {
		t.Errorf("probe runs = %d, want exactly 1", n)
	}
	failures, done := h.svc.Verdict()
	if !done || failures != nil {
		t.Errorf("Verdict = %v, %v; want nil, true", failures, done)
	}
	if n := h.verdicts.Load(); n != 1 {
		t.Errorf("OnVerdict calls = %d, want 1", n)
	}
	if got, want := h.svc.SharedStore(), podmanx.SharedStorePath(h.state); got != want {
		t.Errorf("SharedStore = %q, want %q", got, want)
	}
	if !strings.Contains(h.log.String(), "nested containers preflight passed") {
		t.Errorf("log lacks the pass:\n%s", h.log)
	}
	// The probe ran the first non-empty tools ref in sorted provider order,
	// with the profile WriteSeccomp wrote.
	var probe []string
	for _, c := range h.probe.recorded() {
		if slices.Contains(c, "--self-test") {
			probe = c
		}
	}
	if !slices.Contains(probe, "tools@sha256:codex") {
		t.Errorf("probe argv %q does not run the codex tools image", probe)
	}
	if !slices.Contains(probe, "seccomp="+podmanx.NestedSeccompPath(h.state)) {
		t.Errorf("probe argv %q does not pass the written seccomp profile", probe)
	}
}

// A failing host's verdict carries every failure, each one logged with its
// check id, detail and hint under component "nested".
func TestKick_publishesFailures(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Deps.AccessRW = func(path string) error {
			return &fs.PathError{Op: "access", Path: path, Err: fs.ErrNotExist}
		}
	})
	// No WriteSeccomp: the missing profile is a probe failure too.
	h.svc.Kick()
	h.svc.wait()

	failures, done := h.svc.Verdict()
	if !done {
		t.Fatal("Verdict not published after the checks ran")
	}
	var checks []string
	for _, f := range failures {
		checks = append(checks, f.Check)
	}
	if want := []string{podmanx.CheckNestedTun, podmanx.CheckNestedProbe}; !slices.Equal(checks, want) {
		t.Errorf("failure checks = %q, want %q", checks, want)
	}
	if h.probe.probeRuns() != 0 {
		t.Error("probe container ran despite a missing seccomp profile")
	}
	logged := h.log.String()
	for _, want := range []string{"nested containers preflight failed", "component=nested", "check=" + podmanx.CheckNestedTun, "check=" + podmanx.CheckNestedProbe, "modprobe tun"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}
	if strings.Contains(logged, "preflight passed") {
		t.Error("a failing verdict logged a pass")
	}
	// The verdict is the caller's copy: mutating it changes nothing.
	failures[0].Check = "mutated"
	if again, _ := h.svc.Verdict(); again[0].Check != podmanx.CheckNestedTun {
		t.Error("Verdict returned the service's own slice")
	}
}

// The checks wait for an OK main preflight: while it is unpublished or
// failed, a kicked check stays pending and no probe runs; the moment it
// passes, the nested verdict follows.
func TestKick_waitsForTheMainPreflight(t *testing.T) {
	var main atomic.Pointer[podmanx.Result]
	h := newHarness(t, func(c *Config) {
		c.Preflight = func() (podmanx.Result, bool) {
			if r := main.Load(); r != nil {
				return *r, true
			}
			return podmanx.Result{}, false
		}
	})
	if err := h.svc.WriteSeccomp(); err != nil {
		t.Fatalf("WriteSeccomp: %v", err)
	}
	h.svc.Kick()
	time.Sleep(20 * time.Millisecond)
	main.Store(&podmanx.Result{Failures: []podmanx.Failure{{Check: podmanx.CheckToolsPull}}})
	time.Sleep(20 * time.Millisecond)
	if _, done := h.svc.Verdict(); done || h.probe.probeRuns() != 0 {
		t.Fatalf("nested checks ran before the main preflight passed (done=%v, runs=%d)", done, h.probe.probeRuns())
	}
	main.Store(&podmanx.Result{Version: "5.4.0"})
	h.svc.wait()
	if failures, done := h.svc.Verdict(); !done || failures != nil {
		t.Errorf("Verdict = %v, %v after the main preflight passed; want nil, true", failures, done)
	}
}

// A server shutting down while the checks wait publishes nothing.
func TestKick_shutdownPublishesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	h := newHarness(t, func(c *Config) {
		c.Preflight = func() (podmanx.Result, bool) { return podmanx.Result{}, false }
	})
	h.svc.ctx = ctx
	h.svc.Kick()
	cancel()
	h.svc.wait()
	if _, done := h.svc.Verdict(); done {
		t.Error("a verdict was published after shutdown")
	}
	if h.verdicts.Load() != 0 {
		t.Error("OnVerdict fired after shutdown")
	}
}

// WriteSeccomp publishes the profile at its state-dir path and remembers it;
// a failed write leaves SeccompPath "" and returns the error.
func TestWriteSeccomp(t *testing.T) {
	h := newHarness(t, nil)
	if h.svc.SeccompPath() != "" {
		t.Error("SeccompPath non-empty before WriteSeccomp")
	}
	if err := h.svc.WriteSeccomp(); err != nil {
		t.Fatalf("WriteSeccomp: %v", err)
	}
	want := podmanx.NestedSeccompPath(h.state)
	if got := h.svc.SeccompPath(); got != want {
		t.Errorf("SeccompPath = %q, want %q", got, want)
	}
	if b, err := os.ReadFile(want); err != nil || !bytes.Contains(b, []byte("sethostname")) {
		t.Errorf("profile at %s: err %v, has sethostname=%v", want, err, bytes.Contains(b, []byte("sethostname")))
	}

	// <state> is a regular file: <state>/containers cannot be created.
	blocked := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bad := newHarness(t, func(c *Config) { c.StateDir = blocked })
	if err := bad.svc.WriteSeccomp(); err == nil {
		t.Error("WriteSeccomp under an unwritable state dir succeeded")
	}
	if got := bad.svc.SeccompPath(); got != "" {
		t.Errorf("SeccompPath after a failed write = %q, want \"\"", got)
	}
}

// pullArgv is the exact shared-store pull of ref.
func pullArgv(state, ref string) []string {
	store := filepath.Join(state, "containers", "shared-store")
	return []string{"podman", "--root", store, "--runroot", store + "-run", "pull", ref}
}

// RefreshCache pulls each listed ref into the shared store, in list order,
// with the store as --root and its sibling runroot; it creates the store
// dir first.
func TestRefreshCache_pullsEachRef(t *testing.T) {
	h := newHarness(t, nil)
	h.settings.set(store.SettingNestedImageCache, "ddev/ddev-webserver:v1.25.5\n  docker.io/library/mariadb:11.8 \n\n")
	h.svc.RefreshCache()
	h.svc.wait()

	want := [][]string{
		pullArgv(h.state, "ddev/ddev-webserver:v1.25.5"),
		pullArgv(h.state, "docker.io/library/mariadb:11.8"),
	}
	if got := h.pulls.recorded(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("pulls =\n  %q\nwant\n  %q", got, want)
	}
	if !dirExists(podmanx.SharedStorePath(h.state)) {
		t.Error("RefreshCache did not create the shared store dir")
	}
	if got := SharedStoreRunRoot(h.state); got != filepath.Join(h.state, "containers", "shared-store-run") {
		t.Errorf("SharedStoreRunRoot = %q", got)
	}
}

// An empty (or absent) list pulls nothing and creates nothing.
func TestRefreshCache_emptyListPullsNothing(t *testing.T) {
	for name, rows := range map[string]map[string]string{
		"empty":  {store.SettingNestedImageCache: ""},
		"blank":  {store.SettingNestedImageCache: " \n\n"},
		"absent": {},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.settings.rows = rows
			h.svc.RefreshCache()
			h.svc.wait()
			if got := h.pulls.recorded(); len(got) != 0 {
				t.Errorf("pulls = %q, want none", got)
			}
			if dirExists(podmanx.SharedStorePath(h.state)) {
				t.Error("an empty list created the shared store dir")
			}
			if strings.Contains(h.log.String(), "level=WARN") {
				t.Errorf("an empty list logged a warning:\n%s", h.log)
			}
		})
	}
}

// A failed pull is logged naming its ref — never returned, never stopping
// the refs after it — and an unreadable list is logged too.
func TestRefreshCache_failuresAreLogged(t *testing.T) {
	h := newHarness(t, nil)
	h.settings.set(store.SettingNestedImageCache, "bad/ref:1\n-flaglike\ngood/ref:2")
	h.pulls.handle = func(argv []string) ([]byte, error) {
		if argv[len(argv)-1] == "bad/ref:1" {
			return nil, errors.New("exit status 125: manifest unknown")
		}
		return nil, nil
	}
	h.svc.RefreshCache()
	h.svc.wait()

	want := [][]string{pullArgv(h.state, "bad/ref:1"), pullArgv(h.state, "good/ref:2")}
	if got := h.pulls.recorded(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("pulls =\n  %q\nwant\n  %q (the failure must not stop the rest; a flag-like entry never reaches the argv)", got, want)
	}
	logged := h.log.String()
	for _, want := range []string{"component=nested", "ref=bad/ref:1", "manifest unknown", "ref=-flaglike", "ref=good/ref:2"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}

	broken := newHarness(t, nil)
	broken.settings.errs[store.SettingNestedImageCache] = errors.New("database is locked")
	broken.svc.RefreshCache()
	broken.svc.wait()
	if len(broken.pulls.recorded()) != 0 {
		t.Error("an unreadable list pulled something")
	}
	if !strings.Contains(broken.log.String(), "database is locked") {
		t.Errorf("unreadable list not logged:\n%s", broken.log)
	}
}

// Calls while a refresh runs coalesce into exactly ONE re-run after it,
// which reads the list as saved by then.
func TestRefreshCache_coalesces(t *testing.T) {
	h := newHarness(t, nil)
	h.settings.set(store.SettingNestedImageCache, "first:1")
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.pulls.handle = func([]string) ([]byte, error) {
		once.Do(func() {
			close(started)
			<-release
		})
		return nil, nil
	}

	h.svc.RefreshCache()
	<-started // the first refresh is inside its pull
	h.settings.set(store.SettingNestedImageCache, "second:2")
	for range 5 {
		h.svc.RefreshCache()
	}
	close(release)
	h.svc.wait()

	want := [][]string{pullArgv(h.state, "first:1"), pullArgv(h.state, "second:2")}
	if got := h.pulls.recorded(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("pulls =\n  %q\nwant exactly one coalesced re-run\n  %q", got, want)
	}

	// Idle again: the next call refreshes once more.
	h.svc.RefreshCache()
	h.svc.wait()
	if n := len(h.pulls.recorded()); n != 3 {
		t.Errorf("pulls after an idle call = %d, want 3", n)
	}
}

// EnabledAnywhere: the default "true" answers alone; otherwise the repos
// decide; and a broken default is an error only when no repo answers.
func TestEnabledAnywhere(t *testing.T) {
	locked := errors.New("database is locked")
	cases := []struct {
		name       string
		def        *string // nil = absent row
		defErr     error
		anyRepo    bool
		repoErr    error
		want       bool
		wantErr    []string
		wantRepoQs int
	}{
		{name: "default on", def: new("true"), want: true},
		{name: "default off, no repo", def: new("false"), wantRepoQs: 1},
		{name: "default off, a repo pinned on", def: new("false"), anyRepo: true, want: true, wantRepoQs: 1},
		{name: "unreadable default, a repo pinned on", defErr: locked, anyRepo: true, want: true, wantRepoQs: 1},
		{name: "unreadable default, no repo", defErr: locked, wantRepoQs: 1,
			wantErr: []string{"nested_containers_default", "database is locked"}},
		{name: "absent default, no repo", wantRepoQs: 1, wantErr: []string{"nested_containers_default", "not found"}},
		{name: "garbage default, no repo", def: new("yes"), wantRepoQs: 1,
			wantErr: []string{"nested_containers_default", `holds "yes"`}},
		{name: "repo query fails", def: new("false"), repoErr: errors.New("no such column"), wantRepoQs: 1,
			wantErr: []string{"no such column"}},
		{name: "both fail", defErr: locked, repoErr: errors.New("no such column"), wantRepoQs: 1,
			wantErr: []string{"database is locked", "no such column"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			if tc.def != nil {
				h.settings.rows[store.SettingNestedContainersDefault] = *tc.def
			}
			if tc.defErr != nil {
				h.settings.errs[store.SettingNestedContainersDefault] = tc.defErr
			}
			h.settings.anyRepo, h.settings.repoErr = tc.anyRepo, tc.repoErr

			got, err := h.svc.EnabledAnywhere(t.Context())
			if got != tc.want {
				t.Errorf("EnabledAnywhere = %v, want %v", got, tc.want)
			}
			if h.settings.repoReqs != tc.wantRepoQs {
				t.Errorf("repo queries = %d, want %d", h.settings.repoReqs, tc.wantRepoQs)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("err = nil, want an error")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}
