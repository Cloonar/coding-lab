// Package nested is the runtime side of Nested containers (ADR-0086, issue
// #115): the host state a nested run needs beyond a plain container run, and
// the background work that produces it. podmanx renders the nested argv and
// holds the checks; instance resolves the per-repo knob and gates the spawn;
// this package owns what lives between them for the server's lifetime:
//
//   - the seccomp profile file (WriteSeccomp, at startup), whose path every
//     nested run's argv names;
//   - the nested preflight verdict (Kick, Verdict): podmanx.NestedPreflight
//     run once in the background and published atomically, the way
//     podmanx.Gate publishes the main preflight;
//   - the shared read-only image store (SharedStore, RefreshCache): the
//     directory nested runs mount and the best-effort pulls of the
//     nested_image_cache list into it.
//
// The gate on all of it is the caller's. cmd/lab kicks the checks at startup
// only when nested containers are on anywhere (EnabledAnywhere), so a host on
// which nobody uses them pays nothing; the settings and repo PATCH handlers
// kick them when an operator turns the knob on later, and a spawn that finds
// no verdict kicks them too (instance.refuseContainerSpawn) — so a knob
// turned on after startup gets its checks on the first spawn attempt, which
// is refused with "retry in a moment" while they run.
package nested

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// Settings is the slice of the store the service reads: the raw settings
// row (never a defaulting getter — EnabledAnywhere must tell a broken
// nested_containers_default from "off", and RefreshCache reads the
// nested_image_cache list) and the repo half of the "on anywhere" gate.
// *store.Store satisfies it; tests script read errors through a fake.
type Settings interface {
	GetSetting(ctx context.Context, key string) (string, error)
	AnyRepoNestedContainers(ctx context.Context) (bool, error)
}

// Config is the service's wiring. Every field but Preflight, Log and
// OnVerdict is required in production; tests set what they exercise.
type Config struct {
	// PodmanBin is the podman binary (--podman): the nested probe's and the
	// shared-store pulls'.
	PodmanBin string
	// StateDir is lab's state dir: the seccomp profile and the shared
	// store live under <state>/containers (podmanx.NestedSeccompPath,
	// podmanx.SharedStorePath).
	StateDir string
	// ToolsImages is --container-tools-image (provider id → ref). The probe
	// runs the first non-empty ref in sorted provider order: every
	// agent-tools image carries the same static labctl, so any one proves
	// the host, and sorting makes "any one" deterministic.
	ToolsImages map[string]string
	// Settings is the store (see the interface).
	Settings Settings
	// Run is the exec seam the shared-store pulls run through
	// (podmanx.ExecRunner in production, a recorder in tests).
	Run podmanx.CmdRunner
	// Deps are NestedPreflight's probes (podmanx.RealDeps in production).
	Deps podmanx.Deps
	// Preflight, when non-nil, is the main container preflight's verdict
	// (podmanx.Gate.Result). The nested checks wait for it to be published
	// and OK before they run: the nested probe runs the tools image the main
	// preflight pulls and the podman it checks, so a nested verdict taken
	// first would fail on the main preflight's root causes and — being
	// final until restart — keep failing after they healed (a tools-image
	// pull the main preflight's retry loop eventually wins). Waiting costs
	// nothing a spawn can see: the container gate refuses on the main
	// verdict before it ever consults the nested one. nil runs the checks
	// at once.
	Preflight func() (podmanx.Result, bool)
	// Log receives every failure and pass (component "nested"); nil is
	// slog.Default().
	Log *slog.Logger
	// OnVerdict, when non-nil, is called after the nested verdict is
	// published — cmd/lab announces the container repos, whose readiness
	// check reads the verdict through the container gate and would
	// otherwise keep showing "pending" on an open page.
	OnVerdict func()
}

// preflightPollEvery is how often a kicked check re-reads Config.Preflight
// while the main preflight has no OK verdict yet: a spawn waiting on the
// nested verdict is told to retry "in a moment", and a second is a moment.
const preflightPollEvery = time.Second

// Service is the nested-containers runtime of one server. Safe for
// concurrent use; every method but WriteSeccomp and EnabledAnywhere returns
// at once and leaves its work to a background goroutine bound to the server
// lifetime context given to New.
type Service struct {
	ctx context.Context
	cfg Config
	log *slog.Logger

	seccomp atomic.Pointer[string] // WriteSeccomp's path; nil or "" = not written

	// The nested verdict: kicked flips once, verdict publishes once. A
	// verdict is final until restart — see Kick.
	kicked  atomic.Bool
	verdict atomic.Pointer[[]podmanx.Failure]

	// The shared-store refresh's coalescing state (RefreshCache).
	refreshMu      sync.Mutex
	refreshing     bool
	refreshPending bool

	// bg tracks every background goroutine, so tests can wait for them;
	// pollEvery is preflightPollEvery, shortened by tests.
	bg        sync.WaitGroup
	pollEvery time.Duration
}

// New returns the service. ctx is the server lifetime context: every
// background check and pull runs under it and stops with it.
func New(ctx context.Context, cfg Config) *Service {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Service{ctx: ctx, cfg: cfg, log: log, pollEvery: preflightPollEvery}
}

// WriteSeccomp generates the nested seccomp profile and writes it to
// podmanx.NestedSeccompPath(StateDir) (podmanx.WriteNestedSeccomp: the
// embedded upstream profile plus the ADR-0086 delta, written atomically),
// remembering the path for SeccompPath. Called once at startup, whether or
// not nested containers are on anywhere: the file is a few kilobytes, and a
// knob turned on later must find it. On failure the remembered path is ""
// and the error is returned for the caller to log; the nested probe then
// reports the missing profile under its own check id, so every nested spawn
// is refused naming it rather than failing inside podman.
func (s *Service) WriteSeccomp() error {
	path, err := podmanx.WriteNestedSeccomp(s.cfg.StateDir)
	s.seccomp.Store(&path)
	return err
}

// SeccompPath is the profile WriteSeccomp wrote, "" when it has not run or
// failed.
func (s *Service) SeccompPath() string {
	if p := s.seccomp.Load(); p != nil {
		return *p
	}
	return ""
}

// SharedStore is the shared image store's host path when that directory
// exists, else "". A nested run mounts it only when it exists (podman
// refuses a bind whose source is missing — RunSpec.Nested's SharedStore
// doc), and it exists from the first Kick on, so in practice every nested
// run gets the mount: an empty store is a stable mount that simply caches
// nothing. Checked per call, never cached: it is a stat on the spawn path.
func (s *Service) SharedStore() string {
	dir := podmanx.SharedStorePath(s.cfg.StateDir)
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir
	}
	return ""
}

// SharedStoreRunRoot is the podman --runroot of the shared-store pulls:
// <state>/containers/shared-store-run, beside the store itself (ADR-0086:
// "a --runroot beside it"). A runroot holds podman's locks and transient
// container state for one graphroot; the pulls need their own, because
// pairing the shared --root with the service user's default runroot would
// mix two stores' state in one directory. On disk rather than under
// /run/user/<uid>: it serves only lab's pulls, which mount nothing, so
// nothing in it outlives a pull that a reboot would need to clear.
func SharedStoreRunRoot(stateDir string) string {
	return podmanx.SharedStorePath(stateDir) + "-run"
}

// EnabledAnywhere reports whether nested containers are on for any repo or
// as the global default — the startup gate of the nested checks. It is the
// two halves ADR-0086 names: nested_containers_default is exactly "true"
// (answered without querying the repos), or some repo pins the knob on
// (store.AnyRepoNestedContainers, which ignores the Runner on purpose).
//
// It errors only when the answer is unknown. A default that cannot be read,
// is absent, or holds anything but "true"/"false" is not an answer by
// itself — but a repo pinned on still is, so the repos are asked anyway and
// (true, nil) wins; with none pinned the default's error is returned. The
// caller (cmd/lab) logs the error and kicks the checks anyway: an
// inheriting repo's spawn is refused under such a default regardless (the
// resolver's ContainerGateNestedUnresolved), so the one extra probe buys a
// verdict that is ready the moment the operator repairs the setting, and
// costs nothing but itself.
func (s *Service) EnabledAnywhere(ctx context.Context) (bool, error) {
	key := store.SettingNestedContainersDefault
	var defErr error
	switch v, err := s.cfg.Settings.GetSetting(ctx, key); {
	case err != nil:
		defErr = fmt.Errorf("reading %s: %w", key, err)
	case v == "true":
		return true, nil
	case v != "false":
		defErr = fmt.Errorf("%s holds %q, which is not true or false", key, v)
	}
	anyRepo, err := s.cfg.Settings.AnyRepoNestedContainers(ctx)
	if err != nil {
		return false, errors.Join(defErr, err)
	}
	if anyRepo {
		return true, nil
	}
	return false, defErr
}

// Kick starts the nested checks if they have never started, and returns at
// once; every later call is a no-op, whether the checks are still running
// or their verdict is published. Idempotent and non-blocking so that every
// trigger may simply call it: startup, the settings and repo PATCH hooks,
// and a spawn the gate refused as pending.
//
// The first call creates the shared store's directory first — nested
// containers are now on somewhere, and the store must exist for the run's
// read-only mount to be stable even while the nested_image_cache list is
// empty (ADR-0086) — logging a failure, which only costs nested runs the
// mount. Then, in the background, it waits for an OK main preflight (Config.
// Preflight), runs podmanx.NestedPreflight once, publishes the verdict,
// logs each failure (or the pass) and calls OnVerdict.
//
// A verdict is final until restart: both checks report host state — the tun
// device, systemd mode's cgroup handoff, crun, the tools image's labctl —
// that changes with a redeploy (and so a fresh boot), the posture of every
// non-retryable main preflight check. An operator who fixes the host
// restarts lab, exactly as for a failed main preflight.
func (s *Service) Kick() {
	if !s.kicked.CompareAndSwap(false, true) {
		return
	}
	if err := s.ensureStoreDir(); err != nil {
		s.log.Warn("creating the nested shared image store", "component", "nested", "err", err)
	}
	s.bg.Go(func() {
		if !s.awaitMainPreflight() {
			return // shutting down: no verdict, nothing to publish
		}
		failures := podmanx.NestedPreflight(s.ctx, podmanx.NestedPreflightConfig{
			PodmanBin:      s.cfg.PodmanBin,
			ToolsImage:     s.toolsImage(),
			SeccompProfile: s.SeccompPath(),
		}, s.cfg.Deps)
		if s.ctx.Err() != nil {
			return // a probe cut short by shutdown proves nothing
		}
		s.verdict.Store(&failures)
		if len(failures) == 0 {
			s.log.Info("nested containers preflight passed", "component", "nested")
		}
		for _, f := range failures {
			s.log.Warn("nested containers preflight failed", "component", "nested",
				"check", f.Check, "detail", f.Detail, "hint", f.Hint)
		}
		if s.cfg.OnVerdict != nil {
			s.cfg.OnVerdict()
		}
	})
}

// awaitMainPreflight blocks until Config.Preflight reports a published OK
// verdict (at once when it is nil), false when the server context ends
// first. A main preflight that failed for good leaves this waiting until
// shutdown — one idle goroutine — and nothing is lost by it: the container
// gate refuses every spawn on the main verdict before it reads this one.
func (s *Service) awaitMainPreflight() bool {
	for s.cfg.Preflight != nil {
		if r, done := s.cfg.Preflight(); done && r.OK() {
			return true
		}
		select {
		case <-s.ctx.Done():
			return false
		case <-time.After(s.pollEvery):
		}
	}
	return true
}

// Verdict is the published nested preflight verdict: (failures, true) once
// the checks finished — nil failures meaning nested runs may start — and
// (nil, false) while they have not been kicked or are still running. The
// slice is the caller's.
func (s *Service) Verdict() (failures []podmanx.Failure, done bool) {
	p := s.verdict.Load()
	if p == nil {
		return nil, false
	}
	return slices.Clone(*p), true
}

// toolsImage is the probe's tools image: the first non-empty ref in sorted
// provider order ("" — no tools image at all — makes NestedPreflight skip
// the probe, the main preflight's CheckToolsImage naming that root cause).
func (s *Service) toolsImage() string {
	ids := make([]string, 0, len(s.cfg.ToolsImages))
	for id := range s.cfg.ToolsImages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if ref := s.cfg.ToolsImages[id]; ref != "" {
			return ref
		}
	}
	return ""
}

// ensureStoreDir creates the shared store's directory, 0700 like the
// <state>/containers dir around it (podmanx.WriteNestedSeccomp's reasons:
// only the service user reads it, as lab and as the rootless podman of every
// nested run, which runs under the same uid with keep-id).
func (s *Service) ensureStoreDir() error {
	return os.MkdirAll(podmanx.SharedStorePath(s.cfg.StateDir), 0o700)
}

// RefreshCache pulls every ref of the nested_image_cache list into the
// shared image store, in the background, and returns at once. Called at
// startup and after every settings save that included the list.
//
// Best effort, all the way: an unreadable list, a store directory that
// cannot be created, and every failed pull are LOGGED (component "nested",
// a pull failure naming its ref) and never returned or turned into a
// refusal — a ref missing from the cache only means a nested run pulls it
// into its own storage (ADR-0086). An empty list pulls nothing and creates
// nothing: the store directory is Kick's to create when nested containers
// are on, and an operator who lists refs has asked for the directory too.
//
// Each pull is `<podman> --root <store> --runroot <store>-run pull <ref>`
// as the service user: --root makes the shared store the pull's graphroot
// (podmanx.SharedStorePath), and the runroot beside it is
// SharedStoreRunRoot's. Pulls run one ref at a time, in list order.
//
// Serialized and coalesced: a call while a refresh is running schedules
// exactly one more refresh after it, however many calls arrive meanwhile,
// and that refresh re-reads the list — so the last saved list is always
// pulled, and two refreshes never write the store at once.
func (s *Service) RefreshCache() {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if s.refreshing {
		s.refreshPending = true
		return
	}
	s.refreshing = true
	s.bg.Go(func() {
		for {
			s.refreshOnce()
			s.refreshMu.Lock()
			if !s.refreshPending || s.ctx.Err() != nil {
				s.refreshing, s.refreshPending = false, false
				s.refreshMu.Unlock()
				return
			}
			s.refreshPending = false
			s.refreshMu.Unlock()
		}
	})
}

// refreshOnce is one pass of RefreshCache: read the list, create the store
// directory, pull each ref.
func (s *Service) refreshOnce() {
	key := store.SettingNestedImageCache
	raw, err := s.cfg.Settings.GetSetting(s.ctx, key)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("reading the nested image cache list", "component", "nested", "setting", key, "err", err)
		}
		return
	}
	refs := cacheRefs(raw)
	if len(refs) == 0 {
		return
	}
	if err := s.ensureStoreDir(); err != nil {
		s.log.Warn("creating the nested shared image store", "component", "nested", "err", err)
		return
	}
	root := podmanx.SharedStorePath(s.cfg.StateDir)
	runRoot := SharedStoreRunRoot(s.cfg.StateDir)
	for _, ref := range refs {
		if s.ctx.Err() != nil {
			return
		}
		if strings.HasPrefix(ref, "-") {
			// The settings PATCH refuses such a line; a row written around
			// it must still never reach the argv as a flag.
			s.log.Warn("skipping a nested image cache entry that is not an image ref", "component", "nested", "ref", ref)
			continue
		}
		if _, err := s.cfg.Run(s.ctx, s.cfg.PodmanBin, "--root", root, "--runroot", runRoot, "pull", ref); err != nil {
			s.log.Warn("pulling into the nested shared image store failed; nested runs pull this image themselves", "component", "nested",
				"ref", ref, "err", err)
			continue
		}
		s.log.Info("pulled into the nested shared image store", "component", "nested", "ref", ref)
	}
}

// cacheRefs splits a stored nested_image_cache value into its refs. The
// settings PATCH stores it normalized (one trimmed ref per line), but the
// split trims and drops blanks again, so a row written by other means
// still yields clean refs.
func cacheRefs(raw string) []string {
	var refs []string
	for line := range strings.SplitSeq(raw, "\n") {
		if ref := strings.TrimSpace(line); ref != "" {
			refs = append(refs, ref)
		}
	}
	return refs
}
