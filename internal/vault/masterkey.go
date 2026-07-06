package vault

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Load reads the master key file (design §6): exactly 64 hex characters
// (32 bytes), optionally followed by a single trailing newline. It
// refuses files whose permissions grant anything to group or other,
// naming the path and the actual permissions. Malformed content is
// reported without echoing it.
func Load(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("master key file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("master key file %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("master key file %s has permissions %04o, want 0600 or stricter; refusing to start", path, perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("master key file: %w", err)
	}
	content := strings.TrimSuffix(string(raw), "\n")
	if len(content) != hex.EncodedLen(KeySize) {
		return nil, malformedKey(path)
	}
	key, err := hex.DecodeString(content)
	if err != nil {
		return nil, malformedKey(path)
	}
	return key, nil
}

// malformedKey describes the expected format without echoing what the
// file actually contained (it may be a secret in the wrong format).
func malformedKey(path string) error {
	return fmt.Errorf("master key file %s is malformed: want exactly %d hex characters with an optional trailing newline", path, hex.EncodedLen(KeySize))
}

// Generate creates a fresh random master key at path — 64 lowercase hex
// characters plus a trailing newline, mode 0600, written atomically
// (same-dir temp file published via link(2)) so a crash cannot leave a
// partial key. Used on first start when the key file is absent (design
// §6). It refuses to overwrite an existing file — link(2) fails with
// EEXIST if path appears between the existence check and the publish, so
// even two processes racing Generate get exactly one winner: silently
// replacing a master key would orphan every encrypted credential.
func Generate(path string) ([]byte, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, errKeyExists(path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("master key file: %w", err)
	}
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("master key file: random key: %w", err)
	}
	content := hex.EncodeToString(key) + "\n"
	if err := writeFileExclusive(path, []byte(content), 0o600); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, errKeyExists(path) // lost the race: another process published first
		}
		return nil, fmt.Errorf("master key file: %w", err)
	}
	return key, nil
}

func errKeyExists(path string) error {
	return fmt.Errorf("master key file %s already exists; refusing to overwrite", path)
}

// writeFileAtomic writes content to path via a same-directory temp file,
// an explicit chmod (independent of umask), fsync, rename, and an fsync
// of the parent directory (without it a power loss can drop the rename
// even though the file data was synced). Replaces an existing path —
// the semantics re-materialization relies on. The temp file is removed
// on any failure; os errors carry paths, never content.
func writeFileAtomic(path string, content []byte, perm os.FileMode) error {
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

// writeFileExclusive is writeFileAtomic with first-writer-wins publish
// semantics: the temp file is linked into place, and link(2) fails with
// fs.ErrExist if path already exists — no stat-then-rename TOCTOU
// window. Used by Generate; materialization keeps rename semantics.
func writeFileExclusive(path string, content []byte, perm os.FileMode) error {
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
