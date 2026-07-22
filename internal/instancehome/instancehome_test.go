package instancehome

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return New(filepath.Join(t.TempDir(), "instances"))
}

func TestNewDoesNoIO(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "instances")
	m := New(dir)
	if m.Root() != dir {
		t.Errorf("Root() = %q, want %q", m.Root(), dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("New() created %q; want no I/O until Materialize", dir)
	}
}

func TestHomePathShape(t *testing.T) {
	m := newTestManager(t)
	const runID = "run_deadbeefdeadbeefdeadbeefdeadbeef"
	want := filepath.Join(m.Root(), runID, "home")
	if got := m.HomePath(runID); got != want {
		t.Errorf("HomePath(%q) = %q, want %q", runID, got, want)
	}
}

func TestMaterializeCreatesTree0700(t *testing.T) {
	m := newTestManager(t)
	const runID = "run_aaaa"

	home, err := m.Materialize(runID)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if want := m.HomePath(runID); home != want {
		t.Errorf("Materialize returned %q, want %q", home, want)
	}

	for _, dir := range []string{m.Root(), filepath.Join(m.Root(), runID), home} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode %v, want directory 0700", dir, fi.Mode())
		}
	}
}

func TestMaterializeTightensExistingLooseDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "instances")
	const runID = "run_bbbb"
	// Pre-create the tree with looser perms, simulating a prior version or
	// externally created directory — Materialize must still end up 0700.
	if err := os.MkdirAll(filepath.Join(root, runID, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := New(root)
	home, err := m.Materialize(runID)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	for _, dir := range []string{root, filepath.Join(root, runID), home} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s left at %04o, want tightened to 0700", dir, fi.Mode().Perm())
		}
	}
}

func TestMaterializeIdempotent(t *testing.T) {
	m := newTestManager(t)
	const runID = "run_cccc"
	first, err := m.Materialize(runID)
	if err != nil {
		t.Fatalf("first Materialize: %v", err)
	}
	// Drop a marker file to prove the second call doesn't wipe/recreate.
	marker := filepath.Join(first, "marker")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := m.Materialize(runID)
	if err != nil {
		t.Fatalf("second Materialize: %v", err)
	}
	if second != first {
		t.Errorf("second Materialize = %q, want %q", second, first)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("second Materialize disturbed existing content: %v", err)
	}
}

func TestMaterializeRejectsBadRunID(t *testing.T) {
	m := newTestManager(t)
	for _, runID := range []string{"", "has/slash", "has\\backslash", "..", "run..id"} {
		if _, err := m.Materialize(runID); err == nil {
			t.Errorf("Materialize(%q) accepted invalid run id", runID)
		}
		if err := m.Wipe(runID); err == nil {
			t.Errorf("Wipe(%q) accepted invalid run id", runID)
		}
	}
}

func TestWipeRemovesWholeTreeIncludingNestedFiles(t *testing.T) {
	m := newTestManager(t)
	const runID = "run_dddd"
	home, err := m.Materialize(runID)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	nested := filepath.Join(home, "sub", "dir")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "file.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(m.Root(), runID)

	if err := m.Wipe(runID); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Errorf("run dir %s still exists after Wipe", runDir)
	}
	// Root itself survives — Wipe only removes this run's subtree.
	if _, err := os.Stat(m.Root()); err != nil {
		t.Errorf("Wipe removed the instances root: %v", err)
	}
}

func TestWipeIsIdempotent(t *testing.T) {
	m := newTestManager(t)
	const runID = "run_eeee"
	if _, err := m.Materialize(runID); err != nil {
		t.Fatal(err)
	}
	if err := m.Wipe(runID); err != nil {
		t.Fatalf("first Wipe: %v", err)
	}
	if err := m.Wipe(runID); err != nil {
		t.Errorf("second Wipe (already gone): %v", err)
	}
}

func TestWipeOfNeverMaterializedRunIsNil(t *testing.T) {
	m := newTestManager(t)
	if err := m.Wipe("run_ffff"); err != nil {
		t.Errorf("Wipe of never-materialized run: %v", err)
	}
}

func TestSweepAllMissingRootIsNoop(t *testing.T) {
	m := newTestManager(t) // root never created
	if err := m.SweepAll(func(string) bool { return false }); err != nil {
		t.Errorf("SweepAll on missing root: %v", err)
	}
}

// ageDir backdates dir's mtime to well past homeDirMinAge, simulating a
// long-orphaned instance home.
func ageDir(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-2 * homeDirMinAge)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
}

func TestSweepAll(t *testing.T) {
	m := newTestManager(t)

	const (
		keptRun    = "run_11111111111111111111111111111111"
		oldOrphan  = "run_22222222222222222222222222222222"
		youngStray = "run_33333333333333333333333333333333"
	)
	for _, runID := range []string{keptRun, oldOrphan, youngStray} {
		if _, err := m.Materialize(runID); err != nil {
			t.Fatalf("Materialize(%s): %v", runID, err)
		}
	}
	// Age the kept run and the orphan past the min-age window; leave
	// youngStray fresh (just materialized).
	ageDir(t, filepath.Join(m.Root(), keptRun))
	ageDir(t, filepath.Join(m.Root(), oldOrphan))

	// A non-run-shaped directory and a stray plain file in root: never
	// touched regardless of keep/age.
	lostFound := filepath.Join(m.Root(), "lost+found")
	if err := os.MkdirAll(lostFound, 0o700); err != nil {
		t.Fatal(err)
	}
	ageDir(t, lostFound)
	strayFile := filepath.Join(m.Root(), "stray.txt")
	if err := os.WriteFile(strayFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * homeDirMinAge)
	if err := os.Chtimes(strayFile, old, old); err != nil {
		t.Fatal(err)
	}

	keep := map[string]bool{keptRun: true}
	if err := m.SweepAll(func(runID string) bool { return keep[runID] }); err != nil {
		t.Fatalf("SweepAll: %v", err)
	}

	if _, err := os.Stat(filepath.Join(m.Root(), keptRun)); err != nil {
		t.Errorf("kept run was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Root(), oldOrphan)); !os.IsNotExist(err) {
		t.Errorf("old unkept orphan survived SweepAll")
	}
	if _, err := os.Stat(filepath.Join(m.Root(), youngStray)); err != nil {
		t.Errorf("young unkept dir was reaped despite being under min-age: %v", err)
	}
	if _, err := os.Stat(lostFound); err != nil {
		t.Errorf("non-run-shaped directory was removed: %v", err)
	}
	if _, err := os.Stat(strayFile); err != nil {
		t.Errorf("stray file in root was removed: %v", err)
	}
}
