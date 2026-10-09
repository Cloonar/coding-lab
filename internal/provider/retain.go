package provider

// Transcript retention's shared file move (issue #81): the one place every
// adapter's RetainTranscript moves its provider-native transcript file out of
// the run's private HOME into the lab-owned retention dir core created, just
// before the per-run tree is wiped. Adapters own WHAT to move (the layout is
// theirs, core never learns it); this helper owns HOW — containment, the
// same-filesystem rename, and the cross-device copy fallback — so the rules
// that keep a retain from ever touching a file outside the run's HOME or
// clobbering anything beyond destDir live once, not per adapter.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// RenameFunc is the same-filesystem move RetainFile tries first — os.Rename's
// signature. It is a parameter rather than a hard call so an adapter can carry
// it as an unexported field its own tests swap for one returning a
// *os.LinkError wrapping syscall.EXDEV, exercising the copy fallback without a
// second filesystem.
type RenameFunc func(oldpath, newpath string) error

// RetainFile moves the regular file src — which MUST live under home — into
// destDir as destDir/<base(src)> and returns that path: the shared body of
// every adapter's AgentProvider.RetainTranscript (issue #81). rename nil means
// os.Rename.
//
// The move is a rename when src and destDir share a filesystem and otherwise
// (rename failing with EXDEV, the <state>/instances vs <state>/transcripts on
// separate mounts case) a copy: the bytes are streamed into a freshly created
// 0600 file (O_EXCL, so nothing is ever overwritten), fsynced, and only then is
// src removed. A failed copy removes its partial file and returns the error
// with src left in place. A failure to remove src AFTER a complete copy is
// deliberately swallowed — the HOME wipe that follows the retain deletes it
// anyway, and failing the retain would throw away a complete copy. The file is
// moved byte-for-byte and never rewritten (no lab-owned format, ADR-0016); a
// rename keeps the file's own mode, which the 0700 destDir core creates
// contains either way.
//
// Inputs, defensively (the caller is core's pre-wipe step, which must never be
// tricked into moving a file it does not own):
//
//   - src "" is nothing to keep: ("", nil).
//   - destDir "" or not an existing directory, and home "", are programmer
//     errors: core creates destDir before every call and only retains for a
//     run with a per-run HOME.
//   - src must resolve strictly under home — lexically AND after resolving
//     symlinks in its directory (a symlinked parent could otherwise smuggle a
//     file from outside the HOME into the move) — and must itself be a regular
//     file, never a symlink or directory. Anything else is an error, nothing
//     moved.
//   - src missing (fs.ErrNotExist anywhere along its path) is nothing to keep:
//     ("", nil). A run that never wrote its transcript, or whose file the
//     provider already retired, is not a failure.
//   - destDir/<base(src)> already existing is an error — destDir arrives
//     empty, so a collision means a bug, and a retain never overwrites.
//
// Only destDir/<base(src)> is ever created; destDir's siblings are never
// touched.
func RetainFile(home, src, destDir string, rename RenameFunc) (string, error) {
	if src == "" {
		return "", nil
	}
	if home == "" {
		return "", errors.New("provider: retain transcript: empty instance HOME — only a run's own HOME is ever retained from")
	}
	if destDir == "" {
		return "", errors.New("provider: retain transcript: empty destDir — core creates the retention dir before calling")
	}
	if rename == nil {
		rename = os.Rename
	}
	home, src, destDir = filepath.Clean(home), filepath.Clean(src), filepath.Clean(destDir)
	if !within(home, src) {
		return "", fmt.Errorf("provider: retain transcript: %s is not under the run's HOME %s — refusing to move it", src, home)
	}
	if di, err := os.Stat(destDir); err != nil || !di.IsDir() {
		return "", fmt.Errorf("provider: retain transcript: destDir %s is not an existing directory (stat err: %v)", destDir, err)
	}

	// Symlink-resolved containment: the HOME is agent-writable, so a parent
	// directory of src could be a link pointing anywhere.
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil // the HOME is already gone — nothing left to keep
		}
		return "", fmt.Errorf("provider: retain transcript: resolve HOME: %w", err)
	}
	realDir, err := filepath.EvalSymlinks(filepath.Dir(src))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("provider: retain transcript: resolve %s: %w", filepath.Dir(src), err)
	}
	if !within(realHome, filepath.Join(realDir, filepath.Base(src))) {
		return "", fmt.Errorf("provider: retain transcript: %s resolves outside the run's HOME %s — refusing to move it", src, home)
	}
	src = filepath.Join(realDir, filepath.Base(src))

	fi, err := os.Lstat(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("provider: retain transcript: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("provider: retain transcript: %s is not a regular file (mode %s) — refusing to move it", src, fi.Mode())
	}

	dst := filepath.Join(destDir, filepath.Base(src))
	if _, err := os.Lstat(dst); err == nil {
		return "", fmt.Errorf("provider: retain transcript: %s already exists — a retain never overwrites", dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("provider: retain transcript: %w", err)
	}

	err = rename(src, dst)
	if err == nil {
		return dst, nil
	}
	if !errors.Is(err, syscall.EXDEV) {
		return "", fmt.Errorf("provider: retain transcript: %w", err)
	}
	if err := copyFile0600(src, dst); err != nil {
		return "", fmt.Errorf("provider: retain transcript: cross-device copy: %w", err)
	}
	_ = os.Remove(src) // see the doc: the wipe that follows removes it anyway
	return dst, nil
}

// within reports whether path lies strictly under dir (both Clean, absolute
// or not alike) — dir itself and anything reached through ".." are outside.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// copyFile0600 streams src into a NEW 0600 file at dst (O_EXCL — never an
// overwrite; src opened O_NOFOLLOW — never through a link swapped in after the
// Lstat) and fsyncs it before returning. A partial dst is removed on failure.
func copyFile0600(src, dst string) (err error) {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
