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

// contextFileTmpl is the context-file body — lab-owned content (issue #51
// decision 8: only the file NAME is provider-declared, the body is generic):
// the repo's tracker binding and what it means, the labctl vocabulary (brief
// §8.3 — transcribed from internal/labctl usage), the five-triage-label table
// (meanings from docs/agents/triage-labels.md; names/colors seeded per repo by
// the store), and the explicit note that labctl supersedes any committed tea/gh
// instructions. None of it names a specific agent CLI, so the same body serves
// claude's CLAUDE.local.md, codex's AGENTS.local.md, and any future provider.
//
//go:embed contextfile.md.tmpl
var contextFileTmpl string

// contextFileTemplate parses the embedded body once. The template name is
// cosmetic (error messages only) — the ON-DISK name comes from the provider's
// SeedMeta.ContextFileName at seed time.
var contextFileTemplate = template.Must(template.New("context-file").Parse(contextFileTmpl))

// renderContextFile renders the context-file body for repo.
func renderContextFile(repo store.Repo) ([]byte, error) {
	var b bytes.Buffer
	data := struct{ Binding, ForgeKind string }{repo.TrackerBinding, repo.ForgeKind}
	if err := contextFileTemplate.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("rendering context file: %w", err)
	}
	return b.Bytes(), nil
}

// seedContextFile writes the rendered guide to <worktree>/<name>, where name
// is the provider's declared context-file name (issue #51 decision 8; claude:
// "CLAUDE.local.md"), overwriting any previous render (idempotent re-seed). A
// provider that declares no context file (name empty) gets none — the write is
// skipped.
func seedContextFile(worktree, name string, repo store.Repo) error {
	if name == "" {
		return nil
	}
	body, err := renderContextFile(repo)
	if err != nil {
		return err
	}
	path := filepath.Join(worktree, name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
