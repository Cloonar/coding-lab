package seeder

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// contextFileTmpl is the context-file body — lab-owned content (issue #51
// decision 8: only the file NAME is provider-declared, the body is generic):
// the repo's tracker binding and what it means, the labctl vocabulary (brief
// §8.3 — transcribed from internal/labctl usage), the five-triage-label table
// (meanings from docs/agents/triage-labels.md; names/colors seeded per repo by
// the store), and the explicit note that labctl supersedes any committed tea/gh
// instructions. None of it names a specific agent CLI, so the same body serves
// claude's CLAUDE.local.md, codex's AGENTS.local.md, and any future provider.
//
// The template is the WHOLE body for a provider with native skill discovery
// (claude-code): its skills bundle is discovered off the filesystem, so its
// context file is exactly this render, byte-for-byte. A provider WITHOUT native
// discovery gets a generated skills index appended AFTER this render (issue #79
// / ADR-0035), by renderContextFile in Go rather than as a second template
// section — see appendSkillsIndex for why the append is structural, not woven.
//
//go:embed contextfile.md.tmpl
var contextFileTmpl string

// contextFileTemplate parses the embedded body once. The template name is
// cosmetic (error messages only) — the ON-DISK name comes from the provider's
// SeedMeta.ContextFileName at seed time.
var contextFileTemplate = template.Must(template.New("context-file").Parse(contextFileTmpl))

// renderContextFile renders the context-file body for repo, driven by the
// provider's meta. The base template is rendered exactly as always; then, ONLY
// for a provider that does not discover seeded skills natively yet does seed
// them (!meta.NativeSkillDiscovery && meta.SkillsDir != ""), a generated skills
// index is appended (issue #79 / ADR-0035). A native-discovery provider — or
// one seeding no skills at all — gets byte-for-byte the template render, which
// is exactly what the testdata goldens pin.
func renderContextFile(repo store.Repo, meta provider.SeedMeta) ([]byte, error) {
	var b bytes.Buffer
	data := struct{ Binding, ForgeKind string }{repo.TrackerBinding, repo.ForgeKind}
	if err := contextFileTemplate.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("rendering context file: %w", err)
	}
	if !meta.NativeSkillDiscovery && meta.SkillsDir != "" {
		appendSkillsIndex(&b, meta.SkillsDir, skillsIndex)
	}
	return b.Bytes(), nil
}

// appendSkillsIndex writes the "## Seeded skills" section onto buf, one bullet
// per skillsIndex entry (in its existing lexicographic order), each pointing at
// the skill's seeded path under skillsDir (issue #79 / ADR-0035): an agent that
// only reads context files — no native .claude/skills-style discovery — still
// finds every seeded playbook and its trigger on demand.
//
// The section is APPENDED in Go rather than woven into contextfile.md.tmpl on
// purpose: byte-identity for a native-discovery provider is then guaranteed
// STRUCTURALLY, not by a template conditional that must be kept inert. That
// provider's code path renders the untouched template and returns — there is no
// index-shaped branch in the template that could ever leak a byte into its
// output, so its render can never drift from the goldens. A second embedded
// template would reintroduce exactly the coupling this avoids.
//
// Shape: a single blank line separates the section from the template output
// (which already ends in exactly one "\n"), then the heading, a blank line, the
// three-line intro paragraph, a blank line, then the bullets — each ending in
// "\n", nothing after the last.
func appendSkillsIndex(buf *bytes.Buffer, skillsDir string, index []skillEntry) {
	buf.WriteString("\n## Seeded skills\n\n")
	fmt.Fprintf(buf, "Lab seeded workflow playbooks (skills) into `%s/` — regenerated on\n", skillsDir)
	buf.WriteString("every spawn and, like this file, never committable. When the work at hand\n")
	buf.WriteString("matches a trigger below, read that skill's SKILL.md fully before starting:\n\n")
	for _, e := range index {
		fmt.Fprintf(buf, "- **%s** — %s → read `%s/%s/SKILL.md`\n", e.Name, e.Description, skillsDir, e.Name)
	}
}

// seedContextFile writes the rendered guide to <worktree>/<meta.ContextFileName>,
// where the name is the provider's declared context-file name (issue #51
// decision 8; claude: "CLAUDE.local.md"), overwriting any previous render
// (idempotent re-seed). A provider that declares no context file (name empty)
// gets none — the write is skipped. meta flows through so the render can append
// the non-native skills index (issue #79).
func seedContextFile(worktree string, repo store.Repo, meta provider.SeedMeta) error {
	if meta.ContextFileName == "" {
		return nil
	}
	body, err := renderContextFile(repo, meta)
	if err != nil {
		return err
	}
	path := filepath.Join(worktree, meta.ContextFileName)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
