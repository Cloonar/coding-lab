package seeder

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// claudeLocalName is the generated per-worktree agent guide (D13), written
// at the worktree root and kept out of git via .git/info/exclude.
const claudeLocalName = "CLAUDE.local.md"

// claudeLocalTmpl is the CLAUDE.local.md body: the repo's tracker binding
// and what it means, the labctl vocabulary (brief §8.3 — transcribed from
// internal/labctl usage), the five-triage-label table (meanings from
// docs/agents/triage-labels.md; names/colors seeded per repo by the store),
// and the explicit note that labctl supersedes any committed tea/gh
// instructions.
//
//go:embed claude_local.md.tmpl
var claudeLocalTmpl string

var claudeLocal = template.Must(template.New(claudeLocalName).Parse(claudeLocalTmpl))

// renderClaudeLocal renders the CLAUDE.local.md body for repo.
func renderClaudeLocal(repo store.Repo) ([]byte, error) {
	var b bytes.Buffer
	data := struct{ Binding, ForgeKind string }{repo.TrackerBinding, repo.ForgeKind}
	if err := claudeLocal.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("rendering %s: %w", claudeLocalName, err)
	}
	return b.Bytes(), nil
}

// seedClaudeLocal writes the rendered guide to <worktree>/CLAUDE.local.md,
// overwriting any previous render (idempotent re-seed).
func seedClaudeLocal(worktree string, repo store.Repo) error {
	body, err := renderClaudeLocal(repo)
	if err != nil {
		return err
	}
	path := filepath.Join(worktree, claudeLocalName)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
