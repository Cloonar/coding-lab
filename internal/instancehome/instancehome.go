// Package instancehome owns <state>/instances — one private HOME directory
// per run (issue #202): the isolation seam the container runner will later
// bind-mount into a run's container, so an agent process sees a HOME scoped
// to that run alone and never the host's or another run's.
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

// Manager owns <state>/instances — one private HOME per run (issue #202):
// the isolation seam the container runner will later mount. See the package
// doc comment for the full lifecycle.
type Manager struct {
	root string
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

// HomePath returns <root>/<runID>/home — pure path derivation, no I/O; the
// directory need not exist yet.
func (m *Manager) HomePath(runID string) string {
	return filepath.Join(m.root, runID, "home")
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

// Materialize creates <root>, <root>/<runID>, and <root>/<runID>/home — all
// mode 0700 (owner-only: HOME may end up holding provider credentials or
// config the agent process itself writes) — tightening the mode on any of
// the three that already existed looser, exactly as vault.NewMaterializer
// tightens its runtime dir. It returns HomePath(runID). Calling it again
// for the same runID is a no-op beyond re-tightening perms.
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
func (m *Manager) Wipe(runID string) error {
	if err := checkRunID(runID); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(m.root, runID)); err != nil {
		return fmt.Errorf("wipe instance home for run %s: %w", runID, err)
	}
	return nil
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
		if err := os.RemoveAll(filepath.Join(m.root, name)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
