// Package fsx provides crash-safe file publication: write-then-publish
// helpers that a power loss cannot leave in a torn state. It exists so the
// key-file writers — the vault master key and the web push VAPID key —
// share one audited implementation of the same-directory temp file + fsync
// + rename/link + directory fsync sequence rather than each re-deriving the
// same durability contract.
package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// WriteFileAtomic writes content to path via a same-directory temp file,
// an explicit chmod (independent of umask), fsync, rename, and an fsync
// of the parent directory (without it a power loss can drop the rename
// even though the file data was synced). Replaces an existing path —
// the semantics re-materialization relies on. The temp file is removed
// on any failure; os errors carry paths, never content.
func WriteFileAtomic(path string, content []byte, perm os.FileMode) error {
	tmpName, err := writeTemp(path, content, perm)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed into place
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// WriteFileExclusive is WriteFileAtomic with first-writer-wins publish
// semantics: the temp file is linked into place, and link(2) fails with
// fs.ErrExist if path already exists — no stat-then-rename TOCTOU
// window. Used by key generation; materialization keeps rename semantics.
func WriteFileExclusive(path string, content []byte, perm os.FileMode) error {
	tmpName, err := writeTemp(path, content, perm)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmpName) }() // the temp name persists after link(2)
	if err := os.Link(tmpName, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// writeTemp writes content to a fresh same-directory temp file (chmod,
// write, fsync, close) and returns its name. The caller publishes it via
// rename or link and removes it afterwards; on error the temp is already
// removed.
func writeTemp(path string, content []byte, perm os.FileMode) (string, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	fail := func(err error) (string, error) {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Chmod(perm); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(content); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	return tmpName, nil
}

// syncDir fsyncs a directory so a just-published directory entry (rename
// or link) survives power loss. Filesystems that cannot fsync a
// directory return EINVAL/ENOTSUP; those are ignored by convention —
// there is nothing more the caller could do.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
