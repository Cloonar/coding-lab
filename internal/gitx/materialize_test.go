package gitx

// Integration tests for the read-only-import snapshot materializer against
// real git (design §11 / D17: gitx paths run on real repos in t.TempDir()).
// The fixture is the production topology minus the forge: a non-bare origin
// standing in for the imported repo's remote, the lab-owned bare reference
// clone the Engine fetches and exports from, and a snapshot directory playing
// the container's bind-mount target.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

type msFixture struct {
	t                        *testing.T
	home, origin, bare, dest string
	env                      []string
	eng                      *Engine
}

func newMSFixture(t *testing.T) *msFixture {
	t.Helper()
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	origin := makeOrigin(t, home, "main", 1) // f0.txt
	env := testutil.HermeticGitEnv(home)
	eng := New("git")
	bare := filepath.Join(t.TempDir(), "repo.git")
	if err := eng.CloneBare(t.Context(), "file://"+origin, bare, env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	f := &msFixture{
		t:      t,
		home:   home,
		origin: origin,
		bare:   bare,
		dest:   filepath.Join(t.TempDir(), "snapshot"),
		env:    env,
		eng:    eng,
	}
	// A write-protected snapshot left behind by a failing assertion would
	// make t.TempDir's cleanup fail too (a read-only directory blocks the
	// unlink of its children); restore writability so the test reports its
	// own failure rather than a RemoveAll error.
	t.Cleanup(func() { _ = restoreWritable(f.dest) })
	return f
}

// commitOrigin applies mutate to origin's working tree and commits the whole
// result — `add -A`, so deletions count — returning the new tip.
func (f *msFixture) commitOrigin(msg string, mutate func(dir string)) string {
	f.t.Helper()
	mutate(f.origin)
	gitCmd(f.t, f.home, f.origin, "add", "-A")
	gitCmd(f.t, f.home, f.origin, "commit", "-q", "-m", msg)
	return f.originTip()
}

func (f *msFixture) originTip() string {
	f.t.Helper()
	return gitCmd(f.t, f.home, f.origin, "rev-parse", "HEAD")
}

func (f *msFixture) materialize() string {
	f.t.Helper()
	commit, err := f.eng.MaterializeSnapshot(f.t.Context(), f.bare, f.dest, "main", f.env)
	if err != nil {
		f.t.Fatalf("MaterializeSnapshot: %v", err)
	}
	return commit
}

// snapshotFile reads one file out of the snapshot, failing the test when it
// is missing.
func (f *msFixture) snapshotFile(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dest, rel))
	if err != nil {
		f.t.Fatalf("read snapshot file %s: %v", rel, err)
	}
	return string(b)
}

// requireAbsent fails the test when rel exists in the snapshot.
func (f *msFixture) requireAbsent(rel string) {
	f.t.Helper()
	if _, err := os.Lstat(filepath.Join(f.dest, rel)); !errors.Is(err, fs.ErrNotExist) {
		f.t.Errorf("snapshot still carries %s (lstat err = %v)", rel, err)
	}
}

// worktreeCount counts the bare repo's worktree entries (the bare repo itself
// is always one) — the temp-worktree-leak detector.
func (f *msFixture) worktreeCount() int {
	f.t.Helper()
	wts, err := f.eng.Worktrees(f.t.Context(), f.bare, f.env)
	if err != nil {
		f.t.Fatalf("Worktrees: %v", err)
	}
	return len(wts)
}

// inodeOf returns the inode number of path — the identity the container's
// bind mount is pinned to.
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: no syscall.Stat_t", path)
	}
	return st.Ino
}

// protectSnapshot strips every write bit from root and everything under it —
// files AND directories — the way a published read-only import is hardened.
func protectSnapshot(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		return os.Chmod(path, info.Mode().Perm()&^0o222)
	})
	if err != nil {
		t.Fatalf("write-protect %s: %v", root, err)
	}
}

// A fresh materialization is the origin's tip as a plain directory: the exact
// file contents, git's executable bit, symlinks as symlinks — and no .git,
// because the snapshot is a tree, not a repository.
func TestMaterializeSnapshot_freshExport(t *testing.T) {
	f := newMSFixture(t)
	want := f.commitOrigin("seed snapshot tree", func(dir string) {
		writeFileT(t, filepath.Join(dir, "top.txt"), "top\n")
		if err := os.MkdirAll(filepath.Join(dir, "sub", "deep"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFileT(t, filepath.Join(dir, "sub", "deep", "nested.txt"), "nested\n")
		if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
			t.Fatalf("write run.sh: %v", err)
		}
		if err := os.Symlink("top.txt", filepath.Join(dir, "link.txt")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	})

	got := f.materialize()
	if got != want {
		t.Errorf("MaterializeSnapshot = %s, want origin tip %s", got, want)
	}

	for rel, content := range map[string]string{
		"f0.txt":              "content 0\n",
		"top.txt":             "top\n",
		"sub/deep/nested.txt": "nested\n",
		"run.sh":              "#!/bin/sh\necho hi\n",
	} {
		if snap := f.snapshotFile(rel); snap != content {
			t.Errorf("snapshot %s = %q, want %q", rel, snap, content)
		}
	}

	f.requireAbsent(".git")

	// Git tracks exactly two file modes; both must survive the export.
	if st, err := os.Stat(filepath.Join(f.dest, "run.sh")); err != nil {
		t.Fatalf("stat run.sh: %v", err)
	} else if st.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode = %v, want 0755 (executable bit lost)", st.Mode().Perm())
	}
	if st, err := os.Stat(filepath.Join(f.dest, "top.txt")); err != nil {
		t.Fatalf("stat top.txt: %v", err)
	} else if st.Mode().Perm() != 0o644 {
		t.Errorf("top.txt mode = %v, want 0644", st.Mode().Perm())
	}

	// A symlink round-trips as a symlink pointing at the same target — not
	// as a copy of the file it names.
	link := filepath.Join(f.dest, "link.txt")
	li, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat link.txt: %v", err)
	}
	if li.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("link.txt mode = %v, want a symlink", li.Mode())
	}
	if target, err := os.Readlink(link); err != nil {
		t.Fatalf("readlink link.txt: %v", err)
	} else if target != "top.txt" {
		t.Errorf("link.txt -> %q, want top.txt", target)
	}
	if got := f.snapshotFile("link.txt"); got != "top\n" {
		t.Errorf("link.txt resolves to %q, want top's content", got)
	}
}

// Re-materializing into the SAME directory after the upstream moved picks up
// modified and added files and returns the advanced commit.
func TestMaterializeSnapshot_refreshPicksUpUpstreamChange(t *testing.T) {
	f := newMSFixture(t)
	first := f.materialize()
	if want := f.originTip(); first != want {
		t.Fatalf("first materialize = %s, want %s", first, want)
	}
	if got := f.snapshotFile("f0.txt"); got != "content 0\n" {
		t.Fatalf("f0.txt = %q, want the seeded content", got)
	}

	want := f.commitOrigin("advance", func(dir string) {
		writeFileT(t, filepath.Join(dir, "f0.txt"), "changed\n")
		writeFileT(t, filepath.Join(dir, "added.txt"), "added\n")
	})

	got := f.materialize()
	if got != want {
		t.Errorf("second materialize = %s, want %s", got, want)
	}
	if got == first {
		t.Errorf("commit did not advance across the refresh (still %s)", first)
	}
	if snap := f.snapshotFile("f0.txt"); snap != "changed\n" {
		t.Errorf("refreshed f0.txt = %q, want %q", snap, "changed\n")
	}
	if snap := f.snapshotFile("added.txt"); snap != "added\n" {
		t.Errorf("refreshed added.txt = %q, want %q", snap, "added\n")
	}
}

// A file deleted upstream must disappear from the snapshot — the refresh
// clears the destination rather than overlaying the new tree on the old.
func TestMaterializeSnapshot_upstreamDeletionDisappears(t *testing.T) {
	f := newMSFixture(t)
	f.commitOrigin("add doomed files", func(dir string) {
		writeFileT(t, filepath.Join(dir, "doomed.txt"), "doomed\n")
		if err := os.MkdirAll(filepath.Join(dir, "doomed"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFileT(t, filepath.Join(dir, "doomed", "inner.txt"), "inner\n")
	})
	f.materialize()
	if got := f.snapshotFile("doomed/inner.txt"); got != "inner\n" {
		t.Fatalf("doomed/inner.txt = %q before the deletion, want %q", got, "inner\n")
	}

	f.commitOrigin("delete them", func(dir string) {
		if err := os.Remove(filepath.Join(dir, "doomed.txt")); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := os.RemoveAll(filepath.Join(dir, "doomed")); err != nil {
			t.Fatalf("remove dir: %v", err)
		}
	})
	f.materialize()

	f.requireAbsent("doomed.txt")
	f.requireAbsent("doomed")
	if got := f.snapshotFile("f0.txt"); got != "content 0\n" {
		t.Errorf("surviving f0.txt = %q, want the seeded content", got)
	}
}

// Re-materializing with nothing changed upstream is a no-op that succeeds:
// same commit, same tree.
func TestMaterializeSnapshot_idempotentSameCommit(t *testing.T) {
	f := newMSFixture(t)
	first := f.materialize()
	second := f.materialize()
	if second != first {
		t.Errorf("second materialize = %s, want the unchanged %s", second, first)
	}
	if got := f.snapshotFile("f0.txt"); got != "content 0\n" {
		t.Errorf("f0.txt after the second materialize = %q, want the seeded content", got)
	}
	entries, err := os.ReadDir(f.dest)
	if err != nil {
		t.Fatalf("read snapshot dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("snapshot has %d entries, want exactly f0.txt", len(entries))
	}
}

// The destination directory keeps its identity across a refresh. This is
// load-bearing: the snapshot directory is bind-mounted into a RUNNING
// container, and a mount follows the inode — clearing the directory in place
// keeps the mount live, while a rename or rm-then-recreate would leave the
// container looking at an unlinked inode.
func TestMaterializeSnapshot_destDirInodeStable(t *testing.T) {
	f := newMSFixture(t)
	f.materialize()
	before := inodeOf(t, f.dest)

	f.commitOrigin("advance", func(dir string) {
		writeFileT(t, filepath.Join(dir, "f0.txt"), "changed\n")
	})
	f.materialize()
	after := inodeOf(t, f.dest)

	if before != after {
		t.Errorf("snapshot dir inode changed %d -> %d: re-materialization must clear the directory in place, never replace it (a bind mount would break)", before, after)
	}
}

// A previous snapshot published read-only (chmod a-w over files AND
// directories) must not block the next refresh: the clear restores write
// permission and the stale tree goes.
func TestMaterializeSnapshot_writeProtectedPreviousSnapshot(t *testing.T) {
	f := newMSFixture(t)
	f.commitOrigin("add stale tree", func(dir string) {
		if err := os.MkdirAll(filepath.Join(dir, "stale"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFileT(t, filepath.Join(dir, "stale", "stale.txt"), "stale\n")
	})
	f.materialize()

	want := f.commitOrigin("drop the stale tree", func(dir string) {
		if err := os.RemoveAll(filepath.Join(dir, "stale")); err != nil {
			t.Fatalf("remove dir: %v", err)
		}
	})
	protectSnapshot(t, f.dest)

	got := f.materialize()
	if got != want {
		t.Errorf("materialize over a write-protected snapshot = %s, want %s", got, want)
	}
	f.requireAbsent("stale")
	if snap := f.snapshotFile("f0.txt"); snap != "content 0\n" {
		t.Errorf("f0.txt after the protected refresh = %q, want the seeded content", snap)
	}
}

// A refresh whose fetch fails must leave the previous snapshot standing: the
// fetch and the ref resolve both happen before anything is deleted, so a dead
// remote can never destroy a working import.
func TestMaterializeSnapshot_fetchFailureLeavesSnapshotIntact(t *testing.T) {
	f := newMSFixture(t)
	f.materialize()

	gitCmd(t, f.home, f.bare, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))

	_, err := f.eng.MaterializeSnapshot(t.Context(), f.bare, f.dest, "main", f.env)
	if err == nil {
		t.Fatal("MaterializeSnapshot against a missing origin succeeded, want error")
	}
	if !strings.Contains(err.Error(), "fetch") {
		t.Errorf("error does not name the failing fetch: %v", err)
	}
	if got := f.snapshotFile("f0.txt"); got != "content 0\n" {
		t.Errorf("f0.txt after the failed refresh = %q, want the previous snapshot untouched", got)
	}
}

// An unresolvable branch (an empty repo, a branch that never existed) names
// the ref it could not resolve — and, like the fetch failure, leaves the
// existing snapshot alone.
func TestMaterializeSnapshot_unresolvableBranchNamesTheRef(t *testing.T) {
	f := newMSFixture(t)
	f.materialize()

	_, err := f.eng.MaterializeSnapshot(t.Context(), f.bare, f.dest, "no-such-branch", f.env)
	if err == nil {
		t.Fatal("MaterializeSnapshot of a missing branch succeeded, want error")
	}
	if !strings.Contains(err.Error(), "refs/remotes/origin/no-such-branch") {
		t.Errorf("error does not name the unresolvable ref: %v", err)
	}
	if got := f.snapshotFile("f0.txt"); got != "content 0\n" {
		t.Errorf("f0.txt after the failed resolve = %q, want the previous snapshot untouched", got)
	}
}

// The export is read-only on the bare repo: `git archive` streams from the
// object store, so no temporary worktree is ever added (and none can leak).
func TestMaterializeSnapshot_noWorktreeLeaked(t *testing.T) {
	f := newMSFixture(t)
	before := f.worktreeCount()

	f.materialize()
	f.commitOrigin("advance", func(dir string) {
		writeFileT(t, filepath.Join(dir, "f0.txt"), "changed\n")
	})
	f.materialize()

	if after := f.worktreeCount(); after != before {
		t.Errorf("worktree count %d -> %d: materialization leaked a worktree", before, after)
	}
}
