package seeder

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"git.cloonar.com/Cloonar/coding-lab/assets"
)

// skillsRoot is where the embedded bundle lands inside a worktree (D13),
// slash-separated; joined per-OS at copy time.
const skillsRoot = ".claude/skills"

// seedSkills copies the embedded skills bundle into
// <worktree>/.claude/skills/. Copy-over semantics: lab's files are
// overwritten so a re-seed heals any drift, but files added under skills/ by
// the user (or a previous run) are never deleted — the copy walks the
// bundle, not the destination.
func seedSkills(worktree string) error {
	bundle, err := fs.Sub(assets.Skills, "skills")
	if err != nil {
		return fmt.Errorf("opening embedded skills bundle: %w", err)
	}
	root := filepath.Join(worktree, filepath.FromSlash(skillsRoot))
	return fs.WalkDir(bundle, ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walking skills bundle at %s: %w", p, walkErr)
		}
		dst := filepath.Join(root, filepath.FromSlash(p))
		if d.IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", dst, err)
			}
			return nil
		}
		data, err := fs.ReadFile(bundle, p)
		if err != nil {
			return fmt.Errorf("reading embedded skill %s: %w", p, err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
		return nil
	})
}
