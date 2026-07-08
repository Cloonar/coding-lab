package seeder

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"git.cloonar.com/Cloonar/coding-lab/assets"
)

// seedSkills copies the embedded skills bundle into <worktree>/<skillsDir>/,
// where skillsDir is the provider's declared skills layout (issue #51 decision
// 8; claude: ".claude/skills"), slash-separated and joined per-OS at copy
// time. A provider that declares no skills dir (skillsDir empty) gets no
// bundle — the copy is skipped. Copy-over semantics: lab's files are
// overwritten so a re-seed heals any drift, but files added under the dir by
// the user (or a previous run) are never deleted — the copy walks the
// bundle, not the destination.
func seedSkills(worktree, skillsDir string) error {
	if skillsDir == "" {
		return nil
	}
	bundle, err := fs.Sub(assets.Skills, "skills")
	if err != nil {
		return fmt.Errorf("opening embedded skills bundle: %w", err)
	}
	root := filepath.Join(worktree, filepath.FromSlash(skillsDir))
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
