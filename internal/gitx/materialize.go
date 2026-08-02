package gitx

// Snapshot materialization for read-only imports (issue #261): a registered
// repo's reference clone is exported as a plain directory tree — no .git, no
// history — that a run's container bind-mounts read-only. The export is a
// `git archive` stream extracted in pure Go, so the snapshot never carries a
// second repository into the run and never needs an external tar binary.

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MaterializeSnapshot fetches the reference repo at bareDir and exports the
// tree of origin/<branch> (no .git, no history) into destDir, returning the
// snapshotted commit. Idempotent and in-place: destDir is created if missing,
// existing contents are cleared before extraction (restoring write permission
// where a previous snapshot was write-protected), and destDir's own inode is
// preserved — the directory may be bind-mounted into a running container, and
// replacing it via rename or rm-then-recreate would leave that mount pointing
// at an unlinked inode.
//
// The order is load-bearing: the fetch AND the ref resolve both happen BEFORE
// anything is deleted, so a refresh against a dead remote (network down,
// credential expired, repo deleted upstream) fails with the previous snapshot
// still intact rather than destroying a working import. Only once the new
// commit is known does the destination get cleared.
//
// Everything but the fetch is read-only on the bare repo: no ref moves, no
// HEAD changes, no worktree — `git archive` reads the object store and
// streams a tar to stdout. Regular files land with git's two modes (0644 /
// 0755, chmod'ed explicitly so umask cannot alter them), directories with
// 0755, and symlinks as symlinks; an entry whose path would escape destDir is
// refused (git archive never emits one, but this tree gets mounted into
// containers).
func (e *Engine) MaterializeSnapshot(ctx context.Context, bareDir, destDir, branch string, extraEnv []string) (string, error) {
	// Fail-loud fetch: its shaped error travels up unwrapped — the caller
	// names the import target, gitx only knows a directory.
	if err := e.Fetch(ctx, bareDir, extraEnv); err != nil {
		return "", err
	}
	ref := "refs/remotes/origin/" + branch
	commit, err := e.refCommit(ctx, bareDir, ref, extraEnv)
	if err != nil {
		return "", fmt.Errorf("snapshot ref %s does not resolve after fetch (empty repository or unknown branch): %w", ref, err)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("create snapshot dir %s: %w", destDir, err)
	}
	if err := clearDir(destDir); err != nil {
		return "", err
	}
	if err := e.extractArchive(ctx, bareDir, destDir, commit, extraEnv); err != nil {
		return "", err
	}
	return commit, nil
}

// clearDir removes every entry inside dir while leaving dir itself — the same
// inode, the same mount target — untouched. A missing dir is not an error
// (the fresh-materialization case). Entries that refuse to go are retried
// after write permission is restored: a previous snapshot may have been
// write-protected with `chmod a-w` over files AND directories, and a
// read-only directory blocks the unlink of its children, so both the entry's
// subtree and dir itself have to regain owner write first.
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read snapshot dir %s: %w", dir, err)
	}
	for _, ent := range entries {
		target := filepath.Join(dir, ent.Name())
		if err := os.RemoveAll(target); err == nil {
			continue
		}
		if err := makeWritable(dir); err != nil {
			return fmt.Errorf("clear snapshot dir %s: %w", dir, err)
		}
		if err := restoreWritable(target); err != nil {
			return fmt.Errorf("clear snapshot entry %s: %w", target, err)
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("clear snapshot entry %s: %w", target, err)
		}
	}
	return nil
}

// makeWritable gives the owner write (and, for a directory, the traverse and
// list bits needed to unlink its children) on one path. Symlinks are left
// alone — chmod would follow them out of the snapshot.
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

// extractArchive streams `git archive --format=tar <commit>` from the bare
// repo and unpacks it into destDir with archive/tar (no external tar binary).
// Bounded by e.timeout like run(), with git's stderr surfaced verbatim in the
// shaped error; an extraction failure kills git rather than draining a stream
// nobody will read.
func (e *Engine) extractArchive(ctx context.Context, bareDir, destDir, commit string, extraEnv []string) error {
	args := []string{"archive", "--format=tar", commit}
	joined := strings.Join(args, " ")
	runCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, e.bin, args...)
	cmd.Dir = bareDir
	cmd.Env = append(e.baseEnv(), extraEnv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git %s: %w", joined, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("git %s: %w", joined, err)
	}

	extractErr := extractTar(stdout, destDir)
	if extractErr != nil {
		cancel()
	} else {
		// Drain the trailing tar padding so git never blocks on a full pipe.
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()

	// An extraction failure is the interesting one — the kill above is what
	// made git fail, so its exit status says nothing.
	if extractErr != nil {
		return extractErr
	}
	if waitErr != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("git %s: timed out after %s", joined, e.timeout)
		}
		return fmt.Errorf("git %s: %v: %s", joined, waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// extractTar unpacks one git-archive tar stream into destDir. It handles the
// entry kinds git emits — regular files (mapped to git's 0644/0755), trees,
// and symlinks (recreated as symlinks; git stores the target in the tar) —
// skips the pax global header git prefixes the archive with (it carries the
// commit id), and refuses anything else rather than silently dropping it.
func extractTar(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read snapshot archive: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		target, err := snapshotPath(destDir, hdr.Name)
		if err != nil {
			return err
		}
		if target == destDir {
			continue // the archive's own root entry
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create snapshot dir %s: %w", filepath.Dir(target), err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create snapshot dir %s: %w", target, err)
			}
			// Explicit chmod: MkdirAll's mode is masked by the umask.
			if err := os.Chmod(target, 0o755); err != nil {
				return fmt.Errorf("chmod snapshot dir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := writeSnapshotFile(target, tr, hdr.Mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("create snapshot symlink %s: %w", target, err)
			}
		default:
			return fmt.Errorf("snapshot archive entry %s: unsupported type %q", hdr.Name, hdr.Typeflag)
		}
	}
}

// writeSnapshotFile writes one regular entry, giving it git's executable mode
// (0755) when the archive's mode has any execute bit and 0644 otherwise —
// the only two modes git tracks. The chmod is explicit and after the write
// because OpenFile's permission argument is masked by the process umask.
func writeSnapshotFile(target string, r io.Reader, tarMode int64) error {
	perm := fs.FileMode(0o644)
	if tarMode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("create snapshot file %s: %w", target, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("write snapshot file %s: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write snapshot file %s: %w", target, err)
	}
	if err := os.Chmod(target, perm); err != nil {
		return fmt.Errorf("chmod snapshot file %s: %w", target, err)
	}
	return nil
}

// snapshotPath resolves one archive entry name against destDir and refuses
// any name that would land outside it — an absolute path or one climbing out
// with "..". git archive never emits either, but the snapshot is mounted into
// containers, so the guard is worth its three lines. destDir itself is
// returned for the archive root ("." / ""), which the caller skips.
func snapshotPath(destDir, name string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("snapshot archive entry %s escapes the snapshot directory", name)
	}
	if rel == "." {
		return destDir, nil
	}
	return filepath.Join(destDir, rel), nil
}
