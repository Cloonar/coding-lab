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
	pulls    *recorder // Config.Run (the image-cache refresh's podman commands)
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

// Kick is idempotent: however many triggers fire — concurrently, before and
// after the verdict — the probe container runs exactly once, the verdict is
// published once, OnVerdict fires once, and the pass is logged. The first
// call also creates the pull cache and image archives directories, so a
// nested run's mount is stable before any image is archived.
func TestKick_runsTheChecksOnce(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.svc.WriteSeccomp(); err != nil {
		t.Fatalf("WriteSeccomp: %v", err)
	}
	if _, done := h.svc.Verdict(); done {
		t.Fatal("Verdict done before any Kick")
	}
	if h.svc.ImageArchives() != "" {
		t.Fatal("ImageArchives non-empty before the archives dir exists")
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(h.svc.Kick)
	}
	wg.Wait()
	if !dirExists(podmanx.ImageArchivesPath(h.state)) {
		t.Error("Kick returned without creating the image archives dir")
	}
	if !dirExists(podmanx.PullCachePath(h.state)) {
		t.Error("Kick returned without creating the pull cache dir")
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
	if got, want := h.svc.ImageArchives(), podmanx.ImageArchivesPath(h.state); got != want {
		t.Errorf("ImageArchives = %q, want %q", got, want)
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

// fakePodman scripts the refresh's podman commands as a recorder handler:
// pull succeeds unless pullErr names the ref, inspect prints ids[ref]
// (unless inspectErr names it), and save — unless saveErr names it —
// writes "<ref> <id>" to its -o file, so the rename onto the archive is a
// real one and the archive's content says which id it holds.
type fakePodman struct {
	mu         sync.Mutex
	ids        map[string]string
	pullErr    map[string]error
	inspectErr map[string]error
	saveErr    map[string]error
}

func newFakePodman() *fakePodman {
	return &fakePodman{ids: map[string]string{}, pullErr: map[string]error{}, inspectErr: map[string]error{}, saveErr: map[string]error{}}
}

func (p *fakePodman) setID(ref, id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ids[ref] = id
}

func (p *fakePodman) handle(argv []string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ref := argv[len(argv)-1]
	switch verb := argv[5]; verb { // after podman --root R --runroot RR
	case "pull":
		return nil, p.pullErr[ref]
	case "image":
		if err := p.inspectErr[ref]; err != nil {
			return nil, err
		}
		return []byte(p.ids[ref] + "\n"), nil
	case "save":
		if err := p.saveErr[ref]; err != nil {
			// A failing save may leave a partial file behind.
			_ = os.WriteFile(argv[slices.Index(argv, "-o")+1], []byte("partial"), 0o600)
			return nil, err
		}
		return nil, os.WriteFile(argv[slices.Index(argv, "-o")+1], []byte(ref+" "+p.ids[ref]), 0o600)
	default:
		return nil, errors.New("unscripted podman verb " + verb)
	}
}

// withPodman wires a fakePodman into the harness's refresh runner.
func (h *harness) withPodman() *fakePodman {
	p := newFakePodman()
	h.pulls.handle = p.handle
	return p
}

// refresh runs one RefreshCache with list as the saved list and waits for
// it, returning the argvs it ran.
func (h *harness) refresh(t *testing.T, list string) [][]string {
	t.Helper()
	before := len(h.pulls.recorded())
	h.settings.set(store.SettingNestedImageCache, list)
	h.svc.RefreshCache()
	h.svc.wait()
	return h.pulls.recorded()[before:]
}

// podmanArgv is the refresh's podman command with the pull cache as
// graphroot and its sibling runroot.
func podmanArgv(state string, args ...string) []string {
	cache := filepath.Join(state, "containers", "shared-store")
	return append([]string{"podman", "--root", cache, "--runroot", cache + "-run"}, args...)
}

func pullArgv(state, ref string) []string { return podmanArgv(state, "pull", ref) }

func inspectArgv(state, ref string) []string {
	return podmanArgv(state, "image", "inspect", "--format", "{{.Id}}", ref)
}

func saveArgv(state, ref string) []string {
	tmp := filepath.Join(podmanx.ImageArchivesPath(state), archiveName(ref)+".tar.tmp")
	return podmanArgv(state, "save", "--format", "docker-archive", "-o", tmp, ref)
}

func archivePath(state, ref string) string {
	return filepath.Join(podmanx.ImageArchivesPath(state), archiveName(ref)+".tar")
}

func sidecarPath(state, ref string) string {
	return filepath.Join(podmanx.ImageArchivesPath(state), archiveName(ref)+".id")
}

func assertArgvs(t *testing.T, got, want [][]string) {
	t.Helper()
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("podman commands =\n  %q\nwant\n  %q", got, want)
	}
}

// assertArchived checks ref's archive holds id (the fake save's content)
// and its sidecar records id.
func assertArchived(t *testing.T, state, ref, id string) {
	t.Helper()
	if b, err := os.ReadFile(archivePath(state, ref)); err != nil || string(b) != ref+" "+id {
		t.Errorf("archive of %s = %q, %v; want %q", ref, b, err, ref+" "+id)
	}
	if b, err := os.ReadFile(sidecarPath(state, ref)); err != nil || strings.TrimSpace(string(b)) != id {
		t.Errorf("sidecar of %s = %q, %v; want %q", ref, b, err, id)
	}
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s still present (err %v)", path, err)
	}
}

// archiveFiles lists the archives directory's file names, sorted.
func archiveFiles(t *testing.T, state string) []string {
	t.Helper()
	entries, err := os.ReadDir(podmanx.ImageArchivesPath(state))
	if err != nil {
		t.Fatalf("reading the archives dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// The archive name: every character outside [A-Za-z0-9._-] becomes "_".
func TestArchiveName(t *testing.T) {
	for ref, want := range map[string]string{
		"docker.io/ddev/ddev-webserver:v1.25.5":   "docker.io_ddev_ddev-webserver_v1.25.5",
		"ddev/ddev-dbserver-mariadb-11.8:v1.25.5": "ddev_ddev-dbserver-mariadb-11.8_v1.25.5",
		"quay.io/x/y@sha256:abc":                  "quay.io_x_y_sha256_abc",
		"localhost:5000/ä b":                      "localhost_5000___b",
	} {
		if got := archiveName(ref); got != want {
			t.Errorf("archiveName(%q) = %q, want %q", ref, got, want)
		}
	}
}

// RefreshCache pulls each listed ref into the pull cache, reads its id and
// saves it as a docker-archive through a temp file renamed into place, with
// the id in a sidecar — in list order, with the pull cache as --root and
// its sibling runroot. It creates both directories first.
func TestRefreshCache_archivesEachRef(t *testing.T) {
	h := newHarness(t, nil)
	p := h.withPodman()
	const web, db = "ddev/ddev-webserver:v1.25.5", "docker.io/library/mariadb:11.8"
	p.setID(web, "sha256:web1")
	p.setID(db, "sha256:db1")

	got := h.refresh(t, web+"\n  "+db+" \n\n")
	assertArgvs(t, got, [][]string{
		pullArgv(h.state, web), inspectArgv(h.state, web), saveArgv(h.state, web),
		pullArgv(h.state, db), inspectArgv(h.state, db), saveArgv(h.state, db),
	})
	assertArchived(t, h.state, web, "sha256:web1")
	assertArchived(t, h.state, db, "sha256:db1")
	if got, want := archiveFiles(t, h.state), []string{
		"ddev_ddev-webserver_v1.25.5.id", "ddev_ddev-webserver_v1.25.5.tar",
		"docker.io_library_mariadb_11.8.id", "docker.io_library_mariadb_11.8.tar",
	}; !slices.Equal(got, want) {
		t.Errorf("archives dir = %q, want %q (no temp file left)", got, want)
	}
	if !dirExists(podmanx.PullCachePath(h.state)) {
		t.Error("RefreshCache did not create the pull cache dir")
	}
	if got := PullCacheRunRoot(h.state); got != filepath.Join(h.state, "containers", "shared-store-run") {
		t.Errorf("PullCacheRunRoot = %q", got)
	}
	if got := h.svc.ImageArchives(); got != podmanx.ImageArchivesPath(h.state) {
		t.Errorf("ImageArchives = %q after a refresh", got)
	}
	if !strings.Contains(h.log.String(), "saved the nested image archive") {
		t.Errorf("a save was not logged:\n%s", h.log)
	}
}

// A second refresh whose pulled ids are unchanged saves nothing: no save
// argv, archive untouched (mtime included). A changed id is re-saved and
// its sidecar updated; a missing archive with an intact sidecar is re-saved
// too.
func TestRefreshCache_savesOnlyChangedImages(t *testing.T) {
	h := newHarness(t, nil)
	p := h.withPodman()
	const a, b = "example.com/a:1", "example.com/b:1"
	p.setID(a, "sha256:a1")
	p.setID(b, "sha256:b1")
	list := a + "\n" + b
	h.refresh(t, list)

	// Back-date the archives so an unwanted rewrite cannot hide inside the
	// filesystem's timestamp granularity.
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, ref := range []string{a, b} {
		if err := os.Chtimes(archivePath(h.state, ref), old, old); err != nil {
			t.Fatal(err)
		}
	}

	got := h.refresh(t, list)
	assertArgvs(t, got, [][]string{
		pullArgv(h.state, a), inspectArgv(h.state, a),
		pullArgv(h.state, b), inspectArgv(h.state, b),
	})
	for _, ref := range []string{a, b} {
		if fi, err := os.Stat(archivePath(h.state, ref)); err != nil || !fi.ModTime().Equal(old) {
			t.Errorf("archive of %s rewritten by an unchanged refresh (mtime %v, err %v)", ref, fi.ModTime(), err)
		}
	}

	// b's tag moved: only b is re-saved.
	p.setID(b, "sha256:b2")
	got = h.refresh(t, list)
	assertArgvs(t, got, [][]string{
		pullArgv(h.state, a), inspectArgv(h.state, a),
		pullArgv(h.state, b), inspectArgv(h.state, b), saveArgv(h.state, b),
	})
	assertArchived(t, h.state, a, "sha256:a1")
	assertArchived(t, h.state, b, "sha256:b2")

	// a's archive vanished, its sidecar did not: re-saved.
	if err := os.Remove(archivePath(h.state, a)); err != nil {
		t.Fatal(err)
	}
	got = h.refresh(t, list)
	assertArgvs(t, got, [][]string{
		pullArgv(h.state, a), inspectArgv(h.state, a), saveArgv(h.state, a),
		pullArgv(h.state, b), inspectArgv(h.state, b),
	})
	assertArchived(t, h.state, a, "sha256:a1")
}

// A ref dropped from the list loses its archive and sidecar on the next
// refresh; listed refs' archives and files that are no archive stay, and
// leftover temp files go. An emptied list — and a never-saved one — prunes
// every archive but pulls nothing.
func TestRefreshCache_prunesUnlistedArchives(t *testing.T) {
	for name, empty := range map[string]func(h *harness){
		"emptied": func(h *harness) { h.settings.set(store.SettingNestedImageCache, "") },
		"absent":  func(h *harness) { delete(h.settings.rows, store.SettingNestedImageCache) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			p := h.withPodman()
			const a, b = "example.com/a:1", "example.com/b:1"
			p.setID(a, "sha256:a1")
			p.setID(b, "sha256:b1")
			h.refresh(t, a+"\n"+b)

			dir := podmanx.ImageArchivesPath(h.state)
			unrelated := filepath.Join(dir, "README")
			stale := filepath.Join(dir, archiveName(a)+".tar.tmp")
			for _, f := range []string{unrelated, stale} {
				if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			h.refresh(t, a)
			assertArchived(t, h.state, a, "sha256:a1")
			assertGone(t, archivePath(h.state, b))
			assertGone(t, sidecarPath(h.state, b))
			assertGone(t, stale)
			if _, err := os.Stat(unrelated); err != nil {
				t.Errorf("prune touched a file that is no archive: %v", err)
			}
			if !strings.Contains(h.log.String(), "component=nested") || !strings.Contains(h.log.String(), archiveName(b)+".tar") {
				t.Errorf("the removal was not logged:\n%s", h.log)
			}

			before := len(h.pulls.recorded())
			empty(h)
			h.svc.RefreshCache()
			h.svc.wait()
			if got := h.pulls.recorded()[before:]; len(got) != 0 {
				t.Errorf("an empty list ran podman: %q", got)
			}
			if got := archiveFiles(t, h.state); !slices.Equal(got, []string{"README"}) {
				t.Errorf("archives dir after an emptied list = %q, want only README", got)
			}
		})
	}
}

// An empty (or absent) list with no archives dir pulls nothing and creates
// nothing.
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
				t.Errorf("podman commands = %q, want none", got)
			}
			if dirExists(podmanx.ImageArchivesPath(h.state)) || dirExists(podmanx.PullCachePath(h.state)) {
				t.Error("an empty list created a cache dir")
			}
			if strings.Contains(h.log.String(), "level=WARN") {
				t.Errorf("an empty list logged a warning:\n%s", h.log)
			}
		})
	}
}

// A failed pull, inspect or save is logged naming its ref — never returned,
// never stopping the refs after it. A listed ref whose pull fails keeps
// its earlier archive; a failed save leaves no temp file and no sidecar
// claiming the new id. A flag-like entry never reaches the argv.
func TestRefreshCache_failuresAreLogged(t *testing.T) {
	h := newHarness(t, nil)
	p := h.withPodman()
	const pullBad, inspectBad, saveBad, good = "pull/bad:1", "inspect/bad:1", "save/bad:1", "good/ref:2"
	for _, ref := range []string{pullBad, inspectBad, saveBad, good} {
		p.setID(ref, "sha256:"+archiveName(ref))
	}
	list := pullBad + "\n-flaglike\n" + inspectBad + "\n" + saveBad + "\n" + good
	h.refresh(t, list) // everything archived once

	p.setID(pullBad, "sha256:new")
	p.setID(saveBad, "sha256:new")
	p.pullErr[pullBad] = errors.New("exit status 125: manifest unknown")
	p.inspectErr[inspectBad] = errors.New("exit status 125: no such image")
	p.saveErr[saveBad] = errors.New("exit status 125: no space left on device")
	p.setID(good, "sha256:good2")
	got := h.refresh(t, list)
	assertArgvs(t, got, [][]string{
		pullArgv(h.state, pullBad),
		pullArgv(h.state, inspectBad), inspectArgv(h.state, inspectBad),
		pullArgv(h.state, saveBad), inspectArgv(h.state, saveBad), saveArgv(h.state, saveBad),
		pullArgv(h.state, good), inspectArgv(h.state, good), saveArgv(h.state, good),
	})
	// Stale but still listed: kept as they were.
	assertArchived(t, h.state, pullBad, "sha256:"+archiveName(pullBad))
	assertArchived(t, h.state, inspectBad, "sha256:"+archiveName(inspectBad))
	assertArchived(t, h.state, saveBad, "sha256:"+archiveName(saveBad))
	assertGone(t, archivePath(h.state, saveBad)+".tmp")
	assertArchived(t, h.state, good, "sha256:good2")

	logged := h.log.String()
	for _, want := range []string{
		"component=nested",
		"ref=" + pullBad, "manifest unknown",
		"ref=" + inspectBad, "no such image",
		"ref=" + saveBad, "no space left on device",
		"ref=-flaglike",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "level=WARN") && !strings.Contains(line, "component=nested") {
			t.Errorf("warning without component=nested: %s", line)
		}
	}

	// An inspect that prints no id is a failure too.
	p.inspectErr = map[string]error{}
	p.setID(inspectBad, "")
	got = h.refresh(t, inspectBad)
	assertArgvs(t, got, [][]string{pullArgv(h.state, inspectBad), inspectArgv(h.state, inspectBad)})
	if !strings.Contains(h.log.String(), "printed no image id") {
		t.Errorf("an empty inspect id was not logged:\n%s", h.log)
	}

	// An unreadable list is logged and prunes nothing.
	broken := newHarness(t, nil)
	bp := broken.withPodman()
	bp.setID(good, "sha256:g")
	broken.refresh(t, good)
	before := len(broken.pulls.recorded())
	broken.settings.errs[store.SettingNestedImageCache] = errors.New("database is locked")
	broken.svc.RefreshCache()
	broken.svc.wait()
	if len(broken.pulls.recorded()) != before {
		t.Error("an unreadable list ran podman")
	}
	if !strings.Contains(broken.log.String(), "database is locked") {
		t.Errorf("unreadable list not logged:\n%s", broken.log)
	}
	assertArchived(t, broken.state, good, "sha256:g")
}

// Two listed refs that map to one archive name: the first is archived, the
// later one is skipped with a warning naming it.
func TestRefreshCache_nameCollision(t *testing.T) {
	h := newHarness(t, nil)
	p := h.withPodman()
	const first, second = "a/b:c", "a_b:c"
	p.setID(first, "sha256:1")
	p.setID(second, "sha256:2")
	got := h.refresh(t, first+"\n"+second)
	assertArgvs(t, got, [][]string{pullArgv(h.state, first), inspectArgv(h.state, first), saveArgv(h.state, first)})
	assertArchived(t, h.state, first, "sha256:1")
	if logged := h.log.String(); !strings.Contains(logged, "ref="+second) || !strings.Contains(logged, "archive name") {
		t.Errorf("the collision was not logged:\n%s", logged)
	}
}

// A pass cut short by shutdown prunes nothing: the refs it never reached
// may still be listed.
func TestRefreshCache_shutdownDoesNotPrune(t *testing.T) {
	h := newHarness(t, nil)
	p := h.withPodman()
	const a, b = "example.com/a:1", "example.com/b:1"
	p.setID(a, "sha256:a1")
	p.setID(b, "sha256:b1")
	h.refresh(t, a+"\n"+b)

	ctx, cancel := context.WithCancel(t.Context())
	h.svc.ctx = ctx
	h.pulls.handle = func(argv []string) ([]byte, error) {
		cancel() // shutdown during the first command of the pass
		return nil, context.Canceled
	}
	h.refresh(t, "example.com/other:1")
	assertArchived(t, h.state, a, "sha256:a1")
	assertArchived(t, h.state, b, "sha256:b1")
}

// Calls while a refresh runs coalesce into exactly ONE re-run after it,
// which reads the list as saved by then.
func TestRefreshCache_coalesces(t *testing.T) {
	h := newHarness(t, nil)
	h.settings.set(store.SettingNestedImageCache, "first:1")
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	p := newFakePodman()
	p.setID("first:1", "sha256:1")
	p.setID("second:2", "sha256:2")
	h.pulls.handle = func(argv []string) ([]byte, error) {
		once.Do(func() {
			close(started)
			<-release
		})
		return p.handle(argv)
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
	if got := pullsOf(h.pulls.recorded()); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("pulls =\n  %q\nwant exactly one coalesced re-run\n  %q", got, want)
	}

	// Idle again: the next call refreshes once more.
	h.svc.RefreshCache()
	h.svc.wait()
	if n := len(pullsOf(h.pulls.recorded())); n != 3 {
		t.Errorf("pulls after an idle call = %d, want 3", n)
	}
}

// pullsOf filters the pull commands out of a recording.
func pullsOf(calls [][]string) [][]string {
	var pulls [][]string
	for _, c := range calls {
		if len(c) > 5 && c[5] == "pull" {
			pulls = append(pulls, c)
		}
	}
	return pulls
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
