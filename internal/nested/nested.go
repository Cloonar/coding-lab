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
//   - the image archives (ImageArchives, RefreshCache, issue #124): the
//     best-effort pulls of the nested_image_cache list into lab's host-side
//     pull cache, a docker-archive per ref saved from it into the archives
//     directory nested runs mount read-only, and the prune of archives no
//     longer listed. The image's start-nested-podman helper imports every
//     archive into the run's own storage, so the run's podman applies its
//     own id mapping (a graphroot shared across mappings does not).
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
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
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
	// image-cache refresh's (pull, inspect, save).
	PodmanBin string
	// StateDir is lab's state dir: the seccomp profile, the pull cache and
	// the image archives live under <state>/containers
	// (podmanx.NestedSeccompPath, podmanx.PullCachePath,
	// podmanx.ImageArchivesPath).
	StateDir string
	// ToolsImages is --container-tools-image (provider id → ref). The probe
	// runs the first non-empty ref in sorted provider order: every
	// agent-tools image carries the same static labctl, so any one proves
	// the host, and sorting makes "any one" deterministic.
	ToolsImages map[string]string
	// Settings is the store (see the interface).
	Settings Settings
	// Run is the exec seam the image-cache refresh's podman commands run
	// through (podmanx.ExecRunner in production, a recorder in tests).
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

	// The image-cache refresh's coalescing state (RefreshCache).
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

// ImageArchives is the image archives directory's host path when that
// directory exists, else "". A nested run mounts it only when it exists
// (podman refuses a bind whose source is missing — RunSpec.Nested's
// ImageArchives doc), and it exists from the first Kick on, so in practice
// every nested run gets the mount: an empty directory is a stable mount
// that simply imports nothing. Checked per call, never cached: it is a stat
// on the spawn path.
func (s *Service) ImageArchives() string {
	dir := podmanx.ImageArchivesPath(s.cfg.StateDir)
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir
	}
	return ""
}

// PullCacheRunRoot is the podman --runroot of the pull cache:
// <state>/containers/shared-store-run, beside the pull cache's graphroot
// (ADR-0086: "a --runroot beside it"). A runroot holds podman's locks and
// transient container state for one graphroot; the refresh needs its own,
// because pairing the pull cache's --root with the service user's default
// runroot would mix two stores' state in one directory. On disk rather than
// under /run/user/<uid>: it serves only lab's pulls and saves, which mount
// nothing, so nothing in it outlives a command that a reboot would need to
// clear.
func PullCacheRunRoot(stateDir string) string {
	return podmanx.PullCachePath(stateDir) + "-run"
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
// The first call creates the pull cache and image archives directories
// first — nested containers are now on somewhere, and the archives
// directory must exist for the run's read-only mount to be stable even while
// the nested_image_cache list is empty (ADR-0086) — logging a failure, which
// only costs nested runs the mount. Then, in the background, it waits for an
// OK main preflight (Config.Preflight), runs podmanx.NestedPreflight once,
// publishes the verdict, logs each failure (or the pass) and calls
// OnVerdict.
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
		s.log.Warn("creating the nested image cache directories", "component", "nested", "err", err)
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

// ensureStoreDir creates the pull cache's graphroot and the image archives
// directory, 0700 like the <state>/containers dir around them
// (podmanx.WriteNestedSeccomp's reasons: only the service user reads them,
// as lab and as the rootless podman of every nested run, which runs under
// the same uid with keep-id).
func (s *Service) ensureStoreDir() error {
	if err := os.MkdirAll(podmanx.PullCachePath(s.cfg.StateDir), 0o700); err != nil {
		return err
	}
	return os.MkdirAll(podmanx.ImageArchivesPath(s.cfg.StateDir), 0o700)
}

// RefreshCache brings the image archives in line with the
// nested_image_cache list, in the background, and returns at once. Called
// at startup and after every settings save that included the list.
//
// One pass (refreshOnce) handles every listed ref in list order, as the
// service user, with the pull cache as graphroot (podmanx.PullCachePath,
// runroot PullCacheRunRoot):
//
//  1. `<podman> --root <cache> --runroot <cache>-run pull <ref>` — the pull
//     cache keeps unchanged layers, so only changed ones are downloaded;
//  2. `… image inspect --format {{.Id}} <ref>` — the pulled image's id;
//  3. when that id differs from the ref's sidecar (archiveNames), `… save
//     --format docker-archive -o <archives>/<name>.tar.tmp <ref>`, then a
//     rename onto <name>.tar and an atomic write of the id into the
//     sidecar — so a run's helper never sees a partial archive, one tag
//     never lives in two archives, and an unchanged image is never
//     rewritten (a restart does not re-save gigabytes).
//
// A pass that ran to completion then prunes the archives directory: every
// archive, sidecar or temp file that belongs to no listed ref is removed
// (pruneArchives). An emptied list therefore removes every archive.
//
// Best effort, all the way: an unreadable list, a directory that cannot be
// created, and every failed pull, inspect, save or removal are LOGGED
// (component "nested", naming the ref or file) and never returned or turned
// into a refusal — a ref missing from the archives only means a nested run
// pulls it into its own storage (ADR-0086). An empty list pulls nothing and
// creates nothing: the directories are Kick's to create when nested
// containers are on, and an operator who lists refs has asked for them too.
//
// Serialized and coalesced: a call while a refresh is running schedules
// exactly one more refresh after it, however many calls arrive meanwhile,
// and that refresh re-reads the list — so the last saved list is always
// archived, and two refreshes never write the pull cache or the archives at
// once.
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

// Archive file suffixes in the image archives directory: <name>.tar is the
// docker-archive the run's helper imports (it loads *.tar only), <name>.id
// the sidecar holding the saved image id, and tmpSuffix marks a file being
// written (<name>.tar.tmp, <name>.id.tmp), renamed into place when complete.
const (
	archiveSuffix = ".tar"
	sidecarSuffix = ".id"
	tmpSuffix     = ".tmp"
)

// refreshOnce is one pass of RefreshCache: read the list, create the
// directories, archive each ref, prune what is no longer listed.
func (s *Service) refreshOnce() {
	key := store.SettingNestedImageCache
	raw, err := s.cfg.Settings.GetSetting(s.ctx, key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		raw = "" // never saved: the empty list
	case err != nil:
		// An unknown list must not prune: the archives may all still be
		// listed.
		s.log.Warn("reading the nested image cache list", "component", "nested", "setting", key, "err", err)
		return
	}
	refs := cacheRefs(raw)
	archives := podmanx.ImageArchivesPath(s.cfg.StateDir)
	if len(refs) == 0 {
		// Nothing to pull and nothing to create, but archives of refs the
		// operator just removed must still go.
		if dirExists(archives) {
			s.pruneArchives(archives, nil)
		}
		return
	}
	if err := s.ensureStoreDir(); err != nil {
		s.log.Warn("creating the nested image cache directories", "component", "nested", "err", err)
		return
	}
	keep := make(map[string]bool, len(refs)) // archive base names of listed refs
	for _, ref := range refs {
		if s.ctx.Err() != nil {
			return // cut short: an incomplete pass never prunes
		}
		if strings.HasPrefix(ref, "-") {
			// The settings PATCH refuses such a line; a row written around
			// it must still never reach the argv as a flag.
			s.log.Warn("skipping a nested image cache entry that is not an image ref", "component", "nested", "ref", ref)
			continue
		}
		name := archiveName(ref)
		if keep[name] {
			s.log.Warn("skipping a nested image cache entry whose archive name an earlier entry already uses", "component", "nested",
				"ref", ref, "archive", name+archiveSuffix)
			continue
		}
		// Kept even when the pull or save below fails: a stale archive of
		// a still-listed ref still saves a run most of its pull.
		keep[name] = true
		s.archiveRef(archives, name, ref)
	}
	if s.ctx.Err() != nil {
		return
	}
	s.pruneArchives(archives, keep)
}

// archiveRef pulls ref into the pull cache and, when the pulled image's id
// differs from the one recorded in <name>.id (or the archive is missing),
// saves it to <archives>/<name>.tar via a temp file and records the new id.
// Every failure is logged naming the ref; nothing is returned.
func (s *Service) archiveRef(archives, name, ref string) {
	root := podmanx.PullCachePath(s.cfg.StateDir)
	podman := func(args ...string) ([]byte, error) {
		return s.cfg.Run(s.ctx, s.cfg.PodmanBin, append([]string{"--root", root, "--runroot", PullCacheRunRoot(s.cfg.StateDir)}, args...)...)
	}
	fail := func(msg string, err error) {
		s.log.Warn(msg+"; the archive stays as it was (a nested run lacking it pulls the image itself)", "component", "nested", "ref", ref, "err", err)
	}

	if _, err := podman("pull", ref); err != nil {
		fail("pulling into the nested image pull cache failed", err)
		return
	}
	out, err := podman("image", "inspect", "--format", "{{.Id}}", ref)
	id := strings.TrimSpace(string(out))
	if err == nil && id == "" {
		err = errors.New("podman image inspect printed no image id")
	}
	if err != nil {
		fail("reading the pulled nested image's id failed", err)
		return
	}

	tarPath := filepath.Join(archives, name+archiveSuffix)
	idPath := filepath.Join(archives, name+sidecarSuffix)
	if old, err := os.ReadFile(idPath); err == nil && strings.TrimSpace(string(old)) == id && fileExists(tarPath) {
		return // archived already: no write at all
	}

	tmp := tarPath + tmpSuffix
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		fail("removing a stale nested image archive temp file failed", err)
		return
	}
	if _, err := podman("save", "--format", "docker-archive", "-o", tmp, ref); err != nil {
		_ = os.Remove(tmp)
		fail("saving the nested image archive failed", err)
		return
	}
	if err := os.Rename(tmp, tarPath); err != nil {
		_ = os.Remove(tmp)
		fail("renaming the nested image archive into place failed", err)
		return
	}
	// The archive is in place; a sidecar that fails to write only costs
	// the next pass a re-save.
	if err := writeFileAtomic(idPath, []byte(id+"\n")); err != nil {
		fail("recording the nested image archive's id failed", err)
		return
	}
	s.log.Info("saved the nested image archive", "component", "nested", "ref", ref, "id", id, "archive", tarPath)
}

// pruneArchives removes every regular file in the archives directory that
// is an archive or sidecar (by suffix) of no name in keep — refs dropped
// from the list — and every leftover temp file, listed ref or not: the pass
// that calls this has finished, and every write it made renamed or removed
// its own temp file, so one still present is a crash's or a failed
// removal's remnant. Only that directory's direct entries with those
// suffixes are touched; anything else in it is left alone. Each removal and
// each failure is logged.
func (s *Service) pruneArchives(archives string, keep map[string]bool) {
	entries, err := os.ReadDir(archives)
	if err != nil {
		s.log.Warn("listing the nested image archives for pruning failed", "component", "nested", "dir", archives, "err", err)
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		file := e.Name()
		base, ok := archiveBase(file)
		if !ok || (keep[base] && !strings.HasSuffix(file, tmpSuffix)) {
			continue
		}
		path := filepath.Join(archives, file)
		if err := os.Remove(path); err != nil {
			s.log.Warn("removing a nested image archive file no listed ref uses failed", "component", "nested", "file", path, "err", err)
			continue
		}
		s.log.Info("removed a nested image archive file no listed ref uses", "component", "nested", "file", path)
	}
}

// archiveBase is the archive base name a file in the archives directory
// belongs to — <base>.tar, <base>.id, <base>.tar.tmp, <base>.id.tmp — and
// whether the file is one of those at all.
func archiveBase(file string) (string, bool) {
	file = strings.TrimSuffix(file, tmpSuffix)
	for _, suf := range []string{archiveSuffix, sidecarSuffix} {
		if base, ok := strings.CutSuffix(file, suf); ok && base != "" {
			return base, true
		}
	}
	return "", false
}

// archiveName is the fixed archive base name of ref: every character
// outside [A-Za-z0-9._-] replaced by "_", e.g.
// docker.io/ddev/ddev-webserver:v1.25.5 → docker.io_ddev_ddev-webserver_v1.25.5
// (archive <name>.tar, sidecar <name>.id). Fixed per ref so a tag never
// lives in two archives. Distinct refs can collide (a/b:c and a_b:c);
// refreshOnce archives the first and skips the later ones with a warning.
func archiveName(ref string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '_'
	}, ref)
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

// writeFileAtomic writes data to path via <path>.tmp and a rename, 0600.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + tmpSuffix
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
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
