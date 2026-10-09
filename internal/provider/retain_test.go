package provider

// RetainFile — the shared move behind every adapter's RetainTranscript (issue
// #81): the same-filesystem rename, the EXDEV copy fallback, the
// nothing-to-keep cases, and the containment refusals that keep a retain from
// ever moving a file it does not own.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// exdevRename fails every rename the way os.Rename does across mounts.
func exdevRename(oldpath, newpath string) error {
	return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
}

// retainFixture lays out a HOME holding one transcript at rel and an empty
// destDir (core's 0700 per-run retention dir) beside a sibling run's dir.
func retainFixture(t *testing.T, rel, body string) (home, src, destDir string) {
	t.Helper()
	home = t.TempDir()
	src = filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0o644); err != nil { // independent of the test umask
		t.Fatal(err)
	}
	root := t.TempDir()
	destDir = filepath.Join(root, "run-1")
	if err := os.Mkdir(destDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return home, src, destDir
}

func TestRetainFile_moves(t *testing.T) {
	const body = "{\"line\":1}\n{\"line\":2}\n"
	for _, tc := range []struct {
		name     string
		rename   RenameFunc
		wantMode os.FileMode // the retained file's permission bits
	}{
		// Same filesystem: a plain rename keeps the file's own mode.
		{"same-fs rename", nil, 0o644},
		// Cross-device: a fresh 0600 copy, fsynced, then the source removed.
		{"cross-device copy fallback", exdevRename, 0o600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, src, destDir := retainFixture(t, ".agent/projects/p/session.jsonl", body)
			got, err := RetainFile(home, src, destDir, tc.rename)
			if err != nil {
				t.Fatalf("RetainFile: %v", err)
			}
			if want := filepath.Join(destDir, "session.jsonl"); got != want {
				t.Errorf("retained path = %q; want %q", got, want)
			}
			b, err := os.ReadFile(got)
			if err != nil || string(b) != body {
				t.Errorf("retained content = %q, %v; want the source bytes unchanged", b, err)
			}
			if fi, err := os.Stat(got); err != nil || fi.Mode().Perm() != tc.wantMode {
				t.Errorf("retained mode = %v, %v; want %v", fi.Mode().Perm(), err, tc.wantMode)
			}
			if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("source still present after the move (lstat err %v)", err)
			}
			if entries, _ := os.ReadDir(filepath.Dir(destDir)); len(entries) != 1 {
				t.Errorf("destDir's parent holds %d entries; want only destDir", len(entries))
			}
		})
	}
}

func TestRetainFile_nothingToKeep(t *testing.T) {
	home, _, destDir := retainFixture(t, "a.jsonl", "x\n")
	for _, tc := range []struct{ name, src string }{
		{"empty transcript path", ""},
		{"missing file", filepath.Join(home, "gone.jsonl")},
		{"missing parent dir", filepath.Join(home, "no", "such", "dir", "gone.jsonl")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RetainFile(home, tc.src, destDir, nil)
			if err != nil || got != "" {
				t.Errorf("RetainFile = %q, %v; want \"\", nil", got, err)
			}
			if entries, _ := os.ReadDir(destDir); len(entries) != 0 {
				t.Errorf("destDir holds %d entries; want none", len(entries))
			}
		})
	}
	// A HOME already wiped is nothing to keep too.
	gone := filepath.Join(t.TempDir(), "wiped-home")
	if got, err := RetainFile(gone, filepath.Join(gone, "a.jsonl"), destDir, nil); err != nil || got != "" {
		t.Errorf("RetainFile on a vanished HOME = %q, %v; want \"\", nil", got, err)
	}
}

func TestRetainFile_refusals(t *testing.T) {
	home, src, destDir := retainFixture(t, "t/a.jsonl", "x\n")

	// A file outside the HOME, reached through a symlinked parent INSIDE it.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.jsonl"), []byte("s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "escape")); err != nil {
		t.Fatal(err)
	}
	// A symlink as the transcript itself.
	if err := os.Symlink(filepath.Join(outside, "secret.jsonl"), filepath.Join(home, "t", "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	// A directory where the file should be.
	if err := os.Mkdir(filepath.Join(home, "t", "dir.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A destination collision.
	collide := t.TempDir()
	if err := os.WriteFile(filepath.Join(collide, "a.jsonl"), []byte("prior\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name               string
		home, src, dest    string
		rename             RenameFunc
		wantErr            string
		outsideMustSurvive bool
	}{
		{name: "empty home", home: "", src: src, dest: destDir, wantErr: "empty instance HOME"},
		{name: "empty destDir", home: home, src: src, dest: "", wantErr: "empty destDir"},
		{name: "missing destDir", home: home, src: src, dest: filepath.Join(destDir, "nope"), wantErr: "not an existing directory"},
		{name: "path outside home", home: home, src: filepath.Join(outside, "secret.jsonl"), dest: destDir, wantErr: "not under the run's HOME", outsideMustSurvive: true},
		{name: "dot-dot escape", home: home, src: filepath.Join(home, "..", filepath.Base(outside), "secret.jsonl"), dest: destDir, wantErr: "not under the run's HOME", outsideMustSurvive: true},
		{name: "home itself", home: home, src: home, dest: destDir, wantErr: "not under the run's HOME"},
		{name: "symlinked parent", home: home, src: filepath.Join(home, "escape", "secret.jsonl"), dest: destDir, wantErr: "resolves outside", outsideMustSurvive: true},
		{name: "symlink transcript", home: home, src: filepath.Join(home, "t", "link.jsonl"), dest: destDir, wantErr: "not a regular file", outsideMustSurvive: true},
		{name: "directory transcript", home: home, src: filepath.Join(home, "t", "dir.jsonl"), dest: destDir, wantErr: "not a regular file"},
		{name: "destination exists", home: home, src: src, dest: collide, wantErr: "never overwrites"},
		{name: "non-EXDEV rename failure", home: home, src: src, dest: destDir, wantErr: "permission denied",
			rename: func(o, n string) error { return &os.LinkError{Op: "rename", Old: o, New: n, Err: syscall.EACCES} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RetainFile(tc.home, tc.src, tc.dest, tc.rename)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != "" {
				t.Errorf("RetainFile = %q, %v; want an error containing %q", got, err, tc.wantErr)
			}
			if tc.outsideMustSurvive {
				if _, err := os.Stat(filepath.Join(outside, "secret.jsonl")); err != nil {
					t.Errorf("the out-of-HOME file was moved: %v", err)
				}
			}
		})
	}
	// Nothing was moved by any refusal: the in-HOME source and the colliding
	// destination are both intact.
	if b, err := os.ReadFile(src); err != nil || string(b) != "x\n" {
		t.Errorf("source after refusals = %q, %v; want intact", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(collide, "a.jsonl")); err != nil || string(b) != "prior\n" {
		t.Errorf("collision target after refusals = %q, %v; want intact", b, err)
	}
}

// A failed cross-device copy leaves no partial file and keeps the source.
func TestRetainFile_copyFailureKeepsSource(t *testing.T) {
	home, src, destDir := retainFixture(t, "a.jsonl", "x\n")
	// Make the copy fail at open: the source is unreadable (root bypasses
	// permission bits, so skip there).
	if os.Geteuid() == 0 {
		t.Skip("root reads 0000 files")
	}
	if err := os.Chmod(src, 0); err != nil {
		t.Fatal(err)
	}
	if got, err := RetainFile(home, src, destDir, exdevRename); err == nil || got != "" {
		t.Fatalf("RetainFile = %q, %v; want the copy error", got, err)
	}
	if entries, _ := os.ReadDir(destDir); len(entries) != 0 {
		t.Errorf("destDir holds %d entries after a failed copy; want none", len(entries))
	}
	if _, err := os.Lstat(src); err != nil {
		t.Errorf("source gone after a failed copy: %v", err)
	}
}
