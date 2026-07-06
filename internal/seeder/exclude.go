package seeder

// The .git/info/exclude management lived in provider/claudecode (M3
// seedGitExclude); M7 lifts it here — the seeder owns exclude-entry
// management (design §1) — and the provider's trust step now calls
// EnsureExcludes for its ".claude/" line. Behavior is the M3 contract
// verbatim: create-if-missing, dedup against existing lines, no rewrite when
// everything is already present, and a hand-edited last line without a
// trailing newline stays intact.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// EnsureExcludes appends the missing entries to worktree's git exclude file.
// info/exclude is shared through the common git dir, so one append covers
// every worktree of the repo — and it is never .gitignore (D15 §9 measure 6:
// lab's seeding must not dirty or change committed files).
func EnsureExcludes(worktree string, entries []string) error {
	path, err := GitExcludePath(worktree)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		for line := range strings.SplitSeq(string(data), "\n") {
			existing[strings.TrimSpace(line)] = true
		}
	case errors.Is(err, fs.ErrNotExist):
		// no exclude file yet — start from empty
	default:
		return fmt.Errorf("read %s: %w", path, err)
	}

	var missing []string
	for _, e := range entries {
		if !existing[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	var b strings.Builder
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		b.WriteString("\n") // keep a hand-edited last line intact
	}
	for _, e := range missing {
		b.WriteString(e)
		b.WriteString("\n")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// GitExcludePath resolves the exclude file for worktree without shelling out
// to git. A linked worktree's .git is a file pointing at
// <bare>/worktrees/<name>/, whose commondir file points back at the shared
// git dir — git reads info/exclude from there (gitrepository-layout: info/
// is common). A plain .git directory (tests, main checkouts) resolves to
// itself. Exported for the trust and seeder tests that read the file back.
func GitExcludePath(worktree string) (string, error) {
	gitPath := filepath.Join(worktree, ".git")
	fi, err := os.Stat(gitPath)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", gitPath, err)
	}
	gitDir := gitPath
	if !fi.IsDir() {
		b, err := os.ReadFile(gitPath)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", gitPath, err)
		}
		rest, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
		if !ok {
			return "", fmt.Errorf("parse %s: no gitdir pointer", gitPath)
		}
		gitDir = strings.TrimSpace(rest)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(worktree, gitDir)
		}
	}
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		common := strings.TrimSpace(string(b))
		if !filepath.IsAbs(common) {
			common = filepath.Join(gitDir, common)
		}
		gitDir = common
	}
	return filepath.Join(gitDir, "info", "exclude"), nil
}
