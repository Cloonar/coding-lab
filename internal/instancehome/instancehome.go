// Package instancehome owns <state>/instances — one private per-run tree per
// run: <root>/<runID>/home, the run's private HOME (issue #202), and
// <root>/<runID>/runtime, the run's private runtime dir (issue #205) holding
// its materialized git credential files, known_hosts, dialog-hook settings
// file, and dialog spools. Both are the isolation seam the container runner
// will later bind-mount into a run's container at host-identical paths: an
// agent process sees a HOME and a credential/spool surface scoped to that run
// alone — never the host's, and never another run's (a shared runtime dir
// would expose other runs' materialized git credentials inside the
// container). Since issue #261 a third sibling joins them:
// <root>/<runID>/imports, the run's read-only import snapshots (ADR-0063) —
// mounted read-only rather than rw, and the reason Wipe/SweepAll must cope
// with write-protected trees.
//
// Its lifecycle deliberately mirrors internal/vault's credential
// Materializer: Materialize writes the tree at launch, Wipe removes it
// immediately at stop/rollback (the normal synchronous path), and SweepAll
// is the belt-and-suspenders GC a throttled sweep runs against a live-run
// keep-set — reaping anything a crash left behind between launch and a
// clean Wipe, while sparing a directory too young to have made it into the
// keep-set yet. See credFileMinAge in vault/materialize.go for the same
// reasoning applied to homeDirMinAge below.
package instancehome

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// homeDirMinAge is how old a run's instance-home directory must be before
// SweepAll's keep-set is allowed to reap it. It guards the same mid-launch
// window credFileMinAge guards in vault/materialize.go: Materialize runs at
// the start of instance Launch, but the run only enters the live keep-set
// once its tmux session is actually up — after the worktree fetch, seed,
// and spawn. A throttled sweep firing inside that window would otherwise
// unlink the home out from under an in-flight launch. A genuine orphan (its
// launch crashed or rolled back without calling Wipe) is still reaped by a
// later sweep once it ages out; the normal stop/rollback path removes it
// immediately via Wipe regardless of age.
const homeDirMinAge = 5 * time.Minute

// runIDPattern is the exact shape ids.NewID("run") produces:
// run_<32 lowercase hex>. SweepAll only ever matches <root> entries whose
// name fits this pattern — a stray file, a human-dropped directory,
// "lost+found", or anything else is never touched, whether or not it would
// otherwise look orphaned.
var runIDPattern = regexp.MustCompile(`^run_[0-9a-f]{32}$`)

// Manager owns <state>/instances — one private per-run tree (home + runtime)
// per run (issues #202/#205): the isolation seam the container runner will
// later mount. See the package doc comment for the full lifecycle.
type Manager struct {
	root string

	// preWipe is the issue #222 adopt-check seam, installed by SetPreWipeHook.
	// nil (the zero value) is a no-op.
	preWipe func(runID string)
}

// New returns a Manager rooted at root. Unlike vault.NewMaterializer, which
// eagerly creates and chmods its runtime dir (and so can fail and returns
// an error), New performs no I/O at all: constructing a Manager can never
// fail, and a Manager can be wired up at process startup before any run has
// ever launched and thus before <state>/instances need exist. All directory
// creation — including the root itself — happens lazily in Materialize.
func New(root string) *Manager {
	return &Manager{root: root}
}

// Root returns the instances root directory (<state>/instances).
func (m *Manager) Root() string { return m.root }

// SetPreWipeHook installs hook as the issue #222 adopt-check seam. Wipe (the
// stop/rollback path) and SweepAll (the orphan GC) — the ONLY two places any
// per-run home tree is ever destroyed — call it synchronously with the run's
// id immediately BEFORE that run's tree is removed, so a wipe can never
// destroy the only valid credential family: the hook gets one last look at
// the live tree to adopt a self-refreshed family into the master store
// before it is gone for good.
//
// Wipe fires it only when the run's directory actually exists — an
// idempotent re-wipe of an already-gone tree must not re-fire an adopt-check.
// SweepAll fires it only for entries that already passed the run-id-pattern,
// keep-set, and min-age gates — a genuine reap, never a kept, too-young, or
// non-run-shaped entry.
//
// Manager has no lock of its own, so — like the rest of its invariants (e.g.
// checkRunID's assumption that every runID it sees already satisfies the
// id-shape contract) — this one is enforced by the caller, not the type: the
// hook must be installed once at composition time, before any run can reach
// Wipe or SweepAll, never concurrently with wipes already in flight. A nil
// hook (the zero value — SetPreWipeHook never called) is a no-op:
// Wipe/SweepAll behave exactly as they did before issue #222.
func (m *Manager) SetPreWipeHook(hook func(runID string)) {
	m.preWipe = hook
}

// HomePath returns <root>/<runID>/home — pure path derivation, no I/O; the
// directory need not exist yet.
func (m *Manager) HomePath(runID string) string {
	return filepath.Join(m.root, runID, "home")
}

// RuntimePath returns <root>/<runID>/runtime — the run's private runtime dir
// (issue #205), where the launch materializes the run's git credential files
// and known_hosts and writes its dialog-hook settings file, and where the
// provider's hooks spool live signals. Pure path derivation like HomePath;
// the directory need not exist yet. Living inside the per-run tree means
// Wipe/SweepAll cover it for free, and the container runner can bind-mount
// exactly one run's credential/spool surface.
func (m *Manager) RuntimePath(runID string) string {
	return filepath.Join(m.root, runID, "runtime")
}

// ImportsPath returns <root>/<runID>/imports — the run's read-only import
// snapshots (issue #261 / ADR-0063), one subdirectory per imported repo
// holding a .git-less copy of that repo's origin/<default> taken at spawn,
// each with a sibling <name>.commit file recording the snapshotted commit for
// /pull-base. The third sibling of home and runtime, for the same reason they
// live here: the snapshots are per-run, never shared and never cached across
// runs, so Wipe/SweepAll cover them for free and the container runner can
// bind-mount exactly this run's snapshots — read-only, at their host-identical
// paths. Pure path derivation like HomePath/RuntimePath; the directory need
// not exist yet.
func (m *Manager) ImportsPath(runID string) string {
	return filepath.Join(m.root, runID, "imports")
}

// checkRunID validates runID with the same defensiveness vault's
// checkNameParts applies to credential/op ids: non-empty and free of the
// path separators, NUL, and dot that could let a hand-fed value escape
// <root> (a bare ".." contains no separator but does contain a dot, so this
// also blocks it). IDs from internal/ids always satisfy this; the check
// only guards against malformed values reaching Materialize/Wipe directly.
func checkRunID(runID string) error {
	if runID == "" {
		return errors.New("instancehome: run id must not be empty")
	}
	if strings.ContainsAny(runID, "./\\\x00") {
		return errors.New("instancehome: invalid run id for instance home path")
	}
	return nil
}

// Materialize creates <root>, <root>/<runID>, <root>/<runID>/home,
// <root>/<runID>/runtime, and <root>/<runID>/imports — all mode 0700
// (owner-only: HOME may end up holding provider credentials or config the
// agent process itself writes, and runtime holds materialized git credential
// files, issue #205) — tightening the mode on any that already existed
// looser, exactly as vault.NewMaterializer tightens its runtime dir. It
// returns HomePath(runID). Calling it again for the same runID is a no-op
// beyond re-tightening perms.
//
// The imports dir (issue #261) is created unconditionally, even for a run
// whose repo declares no read-only imports: an empty directory costs nothing
// and keeps the per-run tree's shape uniform, so nothing downstream has to
// ask whether this run happens to have imports before deriving a path.
func (m *Manager) Materialize(runID string) (string, error) {
	if err := checkRunID(runID); err != nil {
		return "", err
	}
	if err := mkdirTight(m.root); err != nil {
		return "", fmt.Errorf("instances dir: %w", err)
	}
	runDir := filepath.Join(m.root, runID)
	if err := mkdirTight(runDir); err != nil {
		return "", fmt.Errorf("instance dir for run %s: %w", runID, err)
	}
	home := filepath.Join(runDir, "home")
	if err := mkdirTight(home); err != nil {
		return "", fmt.Errorf("home dir for run %s: %w", runID, err)
	}
	if err := mkdirTight(filepath.Join(runDir, "runtime")); err != nil {
		return "", fmt.Errorf("runtime dir for run %s: %w", runID, err)
	}
	if err := mkdirTight(filepath.Join(runDir, "imports")); err != nil {
		return "", fmt.Errorf("imports dir for run %s: %w", runID, err)
	}
	return home, nil
}

// mkdirTight creates dir (and any missing parents) and ensures it ends up
// exactly mode 0700, even if it already existed with a looser mode —
// MkdirAll alone leaves an existing directory's mode untouched.
func mkdirTight(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// Wipe removes <root>/<runID> recursively — the whole per-run tree, not
// just home — at the stop/rollback lifecycle point (mirrors
// vault.Materializer.Cleanup). A missing directory is success: idempotent
// across repeated calls, and a run whose home was never materialized (e.g.
// a rollback before Materialize ran) wipes cleanly too.
//
// If a pre-wipe hook is installed (SetPreWipeHook, issue #222) and the run's
// directory actually exists, it runs synchronously here, immediately before
// the RemoveAll — the run's last chance to adopt a self-refreshed credential
// family into the master store before its home is gone for good. It does
// NOT fire for an already-missing directory: an idempotent re-wipe must not
// re-fire the adopt-check.
func (m *Manager) Wipe(runID string) error {
	if err := checkRunID(runID); err != nil {
		return err
	}
	runDir := filepath.Join(m.root, runID)
	if m.preWipe != nil {
		if _, err := os.Stat(runDir); err == nil {
			m.preWipe(runID)
		}
	}
	if err := removeTree(runDir); err != nil {
		return fmt.Errorf("wipe instance home for run %s: %w", runID, err)
	}
	return nil
}

// removeTree removes dir recursively, hardened against WRITE-PROTECTED
// content (issue #261): a run's imports/ holds read-only import snapshots,
// which the host runner strips write bits from (ADR-0063's best-effort
// `chmod a-w`) and which can also carry read-only modes from the imported
// repo's own files — and an unwritable DIRECTORY refuses the unlink of its
// children, so a plain RemoveAll fails at the first one and would strand the
// whole per-run tree. The fast path is still one RemoveAll; only on failure
// does the tree get walked to restore owner write+traverse, then removed
// again. Exactly one retry: a second failure is a real I/O problem (a
// different owner, a read-only mount), not a permission bit this process can
// fix, so it surfaces rather than looping. The makeWritable/restoreWritable
// pair below mirrors internal/gitx/materialize.go's identically-named helpers
// (package-private there, deliberately re-implemented here rather than
// exported — one small copy per package beats a dependency edge).
func removeTree(dir string) error {
	err := os.RemoveAll(dir)
	if err == nil {
		return nil
	}
	if rerr := restoreWritable(dir); rerr != nil {
		return err // the original removal failure is the interesting one
	}
	return os.RemoveAll(dir)
}

// makeWritable gives the owner write (and, for a directory, the traverse and
// list bits needed to unlink its children) on one path. Symlinks are left
// alone — chmod would follow them out of the tree.
func makeWritable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil
	}
	want := info.Mode().Perm() | 0o200
	if info.IsDir() {
		want |= 0o700
	}
	if want == info.Mode().Perm() {
		return nil
	}
	return os.Chmod(path, want)
}

// restoreWritable applies makeWritable to root and everything beneath it.
// Directories are fixed on the way down — WalkDir calls the callback for a
// directory before reading it — so a subtree that was write- and read-
// protected becomes walkable as the walk proceeds.
func restoreWritable(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return makeWritable(path)
	})
}

// SweepAll GCs orphaned instance-home trees: for every entry of <root> that
// is a directory whose name matches the run-id shape (runIDPattern), it
// removes <root>/<runID> recursively iff keep(runID) is false AND the
// directory is older than homeDirMinAge. Entries that are not run-id-shaped
// directories — stray files, "lost+found", anything a human dropped in —
// are never touched, kept or not. A missing root directory is a no-op:
// nothing has been materialized yet, which is normal (unlike vault's
// runtime dir, <root> is not created until the first Materialize).
//
// Error handling mirrors vault.CleanupAll exactly: per-entry removal
// failures never abort the pass (one bad entry must not block reaping the
// rest), are collected with errors.Join, and are returned to the caller to
// log — the same shape reconcile's sweep/startup callers already handle for
// CleanupAll.
//
// If a pre-wipe hook is installed (SetPreWipeHook, issue #222), it runs
// synchronously right before each entry's RemoveAll — i.e. only for entries
// that already passed the run-id-pattern, keep-set, and min-age gates, never
// for a kept or too-young or non-run-shaped entry — so an orphan reaped here
// (the restart-after-downtime race a startup sweep exists to close) gets the
// same adopt-check chance as the synchronous Wipe path.
func (m *Manager) SweepAll(keep func(runID string) bool) error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("instances dir: %w", err)
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !runIDPattern.MatchString(name) {
			continue
		}
		if keep != nil && keep(name) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || time.Since(info.ModTime()) < homeDirMinAge {
			continue // in-flight launch: materialized, but its run isn't in the keep-set yet
		}
		if m.preWipe != nil {
			m.preWipe(name)
		}
		// removeTree, not a bare RemoveAll: an orphan reaped here carries the
		// same write-protected import snapshots (issue #261) a synchronous
		// Wipe would have had to clear.
		if err := removeTree(filepath.Join(m.root, name)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
