package seeder

import (
	"fmt"
	"io/fs"
	"strings"

	"go.yaml.in/yaml/v2"

	"git.cloonar.com/Cloonar/coding-lab/assets"
)

// skillEntry is one bundled skill's index datum (issue #79 decision 2): Name
// is the skill's directory name in the bundle — which the strict-mode parse
// below guarantees equals its frontmatter name, so callers can build the
// seeded path as <SkillsDir>/<Name>/SKILL.md without re-deriving anything —
// and Description is the frontmatter description, normalized to a single
// line so the rendered index is exactly one line per skill. A provider
// without native skill discovery (issue #79: e.g. Codex, #2) renders one of
// these per bundled skill into its generated context file, each pointing at
// the seeded path; a provider WITH native discovery (claude-code) never
// consults this type at all.
type skillEntry struct {
	Name        string
	Description string
}

// skillFrontmatter is the subset of a SKILL.md's YAML frontmatter this
// package cares about. Unknown keys (e.g. grill-with-docs's
// disable-model-invocation) are ignored by yaml.Unmarshal's default
// behavior — this index only ever reads name and description, so any
// frontmatter key added later for other purposes needs no change here.
type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// frontmatterDelim is the line that opens and closes a SKILL.md's YAML
// frontmatter block, per the convention every bundled skill follows (see
// assets/skills/*/SKILL.md).
const frontmatterDelim = "---"

// parseSkillsIndex walks the top-level directories of fsys (the embedded
// bundle rooted at the skills/ prefix — pass fs.Sub(assets.Skills, "skills"),
// as seedSkills does) and returns one skillEntry per skill, in fs.ReadDir
// order (lexicographic — so the rendered index has a deterministic,
// reviewable order run to run). Top-level plain files (e.g. skills/README.md)
// are skipped — only directories are skills; nothing below a skill's own
// directory is inspected (a skill's scripts/ subdirectory, e.g.
// diagnose/scripts/, is never mistaken for a skill of its own).
//
// Parsing is STRICT (issue #79 decision 3: a bundled skill with unparseable
// frontmatter must fail loudly, never silently produce a broken or partial
// index): every top-level directory must yield a valid entry or the whole
// walk fails, naming the offending path. See parseSkillEntry for the checks.
func parseSkillsIndex(fsys fs.FS) ([]skillEntry, error) {
	dirEntries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("reading skills bundle root: %w", err)
	}
	var entries []skillEntry
	for _, d := range dirEntries {
		if !d.IsDir() {
			continue
		}
		entry, err := parseSkillEntry(fsys, d.Name())
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// parseSkillEntry reads and validates <dir>/SKILL.md within fsys, returning
// its skillEntry. Every failure mode names dir or the SKILL.md path so a
// broken bundle is diagnosable straight from the panic/error message (there
// is no other reporting path — see skillsIndex below).
func parseSkillEntry(fsys fs.FS, dir string) (skillEntry, error) {
	path := dir + "/SKILL.md"
	data, err := fs.ReadFile(fsys, path)
	if err != nil {
		return skillEntry{}, fmt.Errorf("%s: reading SKILL.md: %w", path, err)
	}
	raw, err := extractFrontmatter(data)
	if err != nil {
		return skillEntry{}, fmt.Errorf("%s: %w", path, err)
	}
	var fm skillFrontmatter
	if err := yaml.Unmarshal(raw, &fm); err != nil {
		return skillEntry{}, fmt.Errorf("%s: parsing frontmatter: %w", path, err)
	}
	if fm.Name == "" {
		return skillEntry{}, fmt.Errorf("%s: frontmatter name is empty", path)
	}
	// The directory name is what seedSkills copies to disk and what the
	// rendered index path (<SkillsDir>/<dir>/SKILL.md) is built from; the
	// frontmatter name is what a skill's own "Use when..." trigger text and
	// any /slash-command vocabulary assume. A mismatch means the bundle is
	// internally inconsistent — better to fail the build than to seed a path
	// that doesn't match what the skill calls itself.
	if fm.Name != dir {
		return skillEntry{}, fmt.Errorf("%s: frontmatter name %q does not match directory %q", path, fm.Name, dir)
	}
	// Folded YAML scalars (`description: >`, e.g. assets/skills/caveman)
	// carry a trailing newline and may carry internal ones from the source
	// wrapping; strings.Fields+Join collapses all whitespace runs (including
	// newlines) to single spaces, which is also idempotent on an
	// already-single-line description.
	desc := strings.Join(strings.Fields(fm.Description), " ")
	if desc == "" {
		return skillEntry{}, fmt.Errorf("%s: frontmatter description is empty", path)
	}
	return skillEntry{Name: fm.Name, Description: desc}, nil
}

// extractFrontmatter returns the bytes between a SKILL.md's opening and
// closing "---" delimiter lines, for the caller to hand to yaml.Unmarshal.
// It does not itself parse YAML — just locates the block — so a bad
// delimiter (missing open, missing close) is reported distinctly from a bad
// YAML body (reported by the caller after Unmarshal fails).
func extractFrontmatter(data []byte) ([]byte, error) {
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != frontmatterDelim {
		return nil, fmt.Errorf("does not start with a %q frontmatter delimiter", frontmatterDelim)
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == frontmatterDelim {
			return []byte(strings.Join(lines[1:i], "\n")), nil
		}
	}
	return nil, fmt.Errorf("frontmatter opened with %q but never closed", frontmatterDelim)
}

// skillsIndex is the bundle's parsed index, computed once at package init —
// deliberately a package-level var rather than a function called per seed
// (issue #79 decision 3): a malformed bundle must panic HERE, at build/test
// time (any `go test ./...` run exercises this package, so CI catches it) and
// at binary boot, never partway through seeding a run. This mirrors the
// template.Must idiom contextfile.go already uses for its embedded template —
// same reasoning, same embedded-asset-is-trusted-input posture.
var skillsIndex = func() []skillEntry {
	bundle, err := fs.Sub(assets.Skills, "skills")
	if err != nil {
		panic(fmt.Sprintf("seeder: opening embedded skills bundle: %v", err))
	}
	entries, err := parseSkillsIndex(bundle)
	if err != nil {
		panic(fmt.Sprintf("seeder: parsing embedded skills index: %v", err))
	}
	return entries
}()
