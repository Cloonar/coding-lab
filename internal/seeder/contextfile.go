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
// provider's meta, the repo's secrets metadata, and the repo's read-only
// imports. The base template is rendered exactly as always; then, ONLY when
// secrets is non-empty, a Secrets section is appended (issue #104); then,
// ONLY when imports is non-empty, a Read-only imports section is appended
// (issue #261); then, ONLY for a provider that does not discover seeded
// skills natively yet does seed them (!meta.NativeSkillDiscovery &&
// meta.SkillsDir != ""), a generated skills index is appended (issue #79 /
// ADR-0035). A native-discovery provider with a secret-less, import-less
// repo — or one seeding no skills at all — gets byte-for-byte the template
// render, which is exactly what the testdata goldens pin.
func renderContextFile(repo store.Repo, meta provider.SeedMeta, secrets []store.RepoSecret, imports []ImportRef) ([]byte, error) {
	var b bytes.Buffer
	data := struct{ Binding, ForgeKind string }{repo.TrackerBinding, repo.ForgeKind}
	if err := contextFileTemplate.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("rendering context file: %w", err)
	}
	if len(secrets) > 0 {
		appendSecretsSection(&b, secrets)
	}
	if len(imports) > 0 {
		appendImportsSection(&b, imports)
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

// appendSecretsSection writes the "## Secrets" section onto buf, one bullet
// per repo secret (issue #104: per-repo secrets, foundation slice on
// internal/store/reposecrets.go). The seeder is metadata-only end to end — it
// never imports internal/vault, never sees a value, and this section never
// carries one; it only teaches the norm for USING a value the operator has
// already materialized:
//
//   - the norm: this repo has operator-managed secrets, a value is never to
//     be echoed, catted, logged, persisted, or written into a file, commit,
//     issue, PR body, or chat reply — the only sanctioned use is
//     `labctl secret exec <NAME...> -- <cmd>`, which injects it into that one
//     child process's environment and nowhere else;
//   - the quoting pattern, correct and trap forms: `labctl secret exec` must
//     be invoked with the child command single-quoted, because a
//     double-quoted child command is expanded by the AGENT's own shell before
//     labctl ever runs — silently substituting an unset variable with the
//     empty string, not failing loud;
//   - the spawn-time inventory: one bullet per secret the store returned
//     (already name-sorted by RepoSecrets), name plus description when the
//     operator gave one;
//   - a pointer to `labctl secret list` for the live view, since the
//     inventory above is a snapshot from spawn time and rotation is live;
//   - the enforcement pointer (issue #106): pushes are scanned server-side
//     against these values and a leaking push is refused naming secret +
//     file — so an agent whose push is blocked knows what happened and that
//     the fix is rewriting the offending commits, not retrying.
//
// Appended in Go for the same structural reason as appendSkillsIndex: a
// secret-less repo's byte-identity is then guaranteed by the len(secrets) == 0
// check in renderContextFile, never by a template conditional that must stay
// inert. Section order in renderContextFile is template body → secrets →
// read-only imports → skills index: repo-driven content (what THIS repo
// holds) precedes the provider-driven tail (what THIS provider needs
// indexed), the same "what this repo is, then what this agent gets" ordering
// the rest of the file follows.
//
// Shape mirrors appendSkillsIndex: a blank line separates the section from
// whatever precedes it (the template render, which already ends in exactly
// one "\n"), then the heading, then paragraphs each blank-line-separated, then
// the bullets, ending in exactly one trailing "\n".
func appendSecretsSection(buf *bytes.Buffer, secrets []store.RepoSecret) {
	buf.WriteString("\n## Secrets\n\n")
	buf.WriteString("This repository has operator-managed secrets. Never echo, cat, log, or\n")
	buf.WriteString("persist a secret value — never write one into a file, a commit, an issue, a\n")
	buf.WriteString("PR body, or a chat reply. Use a value only through `labctl secret exec\n")
	buf.WriteString("<NAME...> -- <cmd>`, which injects it into that one child process's\n")
	buf.WriteString("environment and nowhere else.\n\n")
	buf.WriteString("Quote the child command with single quotes, never double:\n\n")
	buf.WriteString("    correct: labctl secret exec API_KEY -- sh -c 'curl -H \"Authorization: Bearer $API_KEY\" https://example.com'\n")
	buf.WriteString("    trap:    labctl secret exec API_KEY -- sh -c \"curl -H \\\"Authorization: Bearer $API_KEY\\\" https://example.com\"\n\n")
	buf.WriteString("With double quotes around the whole command, YOUR shell expands `$API_KEY`\n")
	buf.WriteString("before labctl ever runs — to empty, since labctl has not injected it yet.\n")
	buf.WriteString("Single-quote the child command so the CHILD shell — which does have the\n")
	buf.WriteString("injected env — is the one that expands it.\n\n")
	buf.WriteString("This repository's secrets:\n\n")
	for _, sec := range secrets {
		if sec.Description == "" {
			fmt.Fprintf(buf, "- `%s`\n", sec.Name)
		} else {
			fmt.Fprintf(buf, "- `%s` — %s\n", sec.Name, sec.Description)
		}
	}
	buf.WriteString("\n`labctl secret list` shows the live inventory — rotation is live, and\n")
	buf.WriteString("values are fetched at exec time.\n\n")
	buf.WriteString("Every `git push` is scanned server-side against these values (plain or\n")
	buf.WriteString("encoded); a push whose diff carries one is refused, naming the secret and\n")
	buf.WriteString("file. Remove the value and rewrite the offending commits, then push again.\n")
}

// appendImportsSection writes the "## Read-only imports" section onto buf,
// one bullet per import (issue #261: read-only imports — snapshots of other
// lab repos materialized outside the worktree at spawn, so an instance can
// read another repo's code without that repo ever entering its own working
// tree or git history). The seeder only renders the inventory the caller
// hands it (Opts.Imports, already ordered by name); it neither creates nor
// refreshes the snapshots themselves — that is the launch-path's job, this
// section only teaches the norm for USING one already materialized:
//
//   - each snapshot is a read-only copy of the imported repo's default
//     branch, taken at spawn — never part of THIS repository, never to be
//     edited or committed, never in scope for changes;
//   - `/pull-base` refreshes each snapshot in place, for an instance that
//     runs long enough for the import's upstream to move.
//
// Appended in Go for the same structural reason as appendSecretsSection: an
// import-less repo's byte-identity is guaranteed by the len(imports) == 0
// check in renderContextFile, never by a template conditional that must stay
// inert. Section order in renderContextFile is template body → secrets →
// read-only imports → skills index (see appendSecretsSection's ordering
// note): both repo-driven sections precede the provider-driven tail.
//
// Shape mirrors appendSecretsSection: a blank line separates the section from
// whatever precedes it, then the heading, a blank line, the prose paragraph, a
// blank line, then the bullets, ending in exactly one trailing "\n".
func appendImportsSection(buf *bytes.Buffer, imports []ImportRef) {
	buf.WriteString("\n## Read-only imports\n\n")
	buf.WriteString("This repository declares read-only imports: snapshots of other lab repos\n")
	buf.WriteString("whose code this instance may read. Each snapshot below lives outside the\n")
	buf.WriteString("working repo and is a read-only copy of that repo's default branch, taken\n")
	buf.WriteString("at spawn — not part of this repository, never to be edited or committed,\n")
	buf.WriteString("never in scope for changes. `/pull-base` refreshes each snapshot in place.\n\n")
	for _, imp := range imports {
		fmt.Fprintf(buf, "- **%s** — `%s` @ %s\n", imp.Name, imp.Path, imp.Commit)
	}
}

// seedContextFile writes the rendered guide to <worktree>/<meta.ContextFileName>,
// where the name is the provider's declared context-file name (issue #51
// decision 8; claude: "CLAUDE.local.md"), overwriting any previous render
// (idempotent re-seed). A provider that declares no context file (name empty)
// gets none — the write is skipped. meta flows through so the render can append
// the non-native skills index (issue #79); secrets flows through so it can
// append the Secrets section (issue #104); imports flows through so it can
// append the Read-only imports section (issue #261).
func seedContextFile(worktree string, repo store.Repo, meta provider.SeedMeta, secrets []store.RepoSecret, imports []ImportRef) error {
	if meta.ContextFileName == "" {
		return nil
	}
	body, err := renderContextFile(repo, meta, secrets, imports)
	if err != nil {
		return err
	}
	path := filepath.Join(worktree, meta.ContextFileName)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
