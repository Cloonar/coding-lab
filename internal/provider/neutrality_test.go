// Package provider_test hosts internal/provider/neutrality_test.go, the Go
// twin of web/src/providerNeutral.test.ts (ADR-0026's SPA grep-guard),
// added by issue #75 / ADR-0033's "Core neutrality made testable" decision.
//
// The SPA guard forbids the broad brand token /claude/i anywhere in src/,
// because the SPA never has a legitimate reason to name a provider by
// literal string — provider identity flows from API data. Core is
// different: it legitimately spells "claude" in CLI/config UX that has
// nothing to do with attribution — flag names (`claude-config`), paths
// (`~/.claude.json`), usage text (internal/config/config.go, cmd/lab/main.go,
// provider.go's "claude.ai" error). Forbidding the broad token in core would
// flag all of that noise and teach nobody to trust the guard. So this guard
// knows a narrower vocabulary: exactly the four attribution-marker tokens
// the pre-push hook and the agent-API body sanitizer enforce (ADR-0033) —
// anthropic, claude-session, co-authored-by, generated with — matched
// case-insensitively. A hit on one of these tokens outside the provider
// declaration itself is not broad-brand noise; it is exactly the shape of
// bug ADR-0033 fixed (a hardcoded claude-specific attribution literal baked
// into a supposedly provider-neutral seam).
//
// Only the provider adapter packages — internal/provider/claudecode and
// internal/provider/codex (the declarations these tokens describe —
// AgentProvider.SeedMeta().ScrubPatterns — are not leaks, they are the
// contract) — and internal/provider/providertest (its fakes deliberately
// mirror claudecode's declared shape for tests) are exempted.
// Every other package's non-test Go source is fair game, and *_test.go files
// are exempt everywhere (fixture literals, per issue #75) — a test asserting
// against a synthetic "Co-Authored-By" trailer is not a leak either.
//
// Like the SPA guard, there is deliberately NO allowlist beyond those
// exemptions: a hit anywhere else must be refactored into provider-declared
// metadata (a registered AgentProvider's SeedMeta), never exempted in this
// file. If this guard ever finds a real hit, the fix belongs to whichever
// file leaked the literal, not to this test.
package provider_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// attributionMarkerPattern is the forbidden token set (issue #75 / ADR-0033):
// exactly the four tokens the pre-push hook / body-sanitizer enforce, not
// the SPA guard's broad /claude/i — see the package doc comment above for
// why core's vocabulary must stay this narrow.
var attributionMarkerPattern = regexp.MustCompile(`(?i)(anthropic|claude-session|co-authored-by|generated with)`)

// violation is one flagged string literal: its source position (via the
// token.FileSet the literal was parsed with) and its decoded value.
type violation struct {
	pos   token.Position
	value string
}

func (v violation) String() string {
	return fmt.Sprintf("%s: %q", v.pos, v.value)
}

// scanSource parses one Go source (filename plus its bytes) and reports every
// string literal whose decoded value matches attributionMarkerPattern, each
// tagged with its file:line via the parser's own token.FileSet.
//
// Import paths are skipped: an *ast.ImportSpec's Path is a string literal
// too (e.g. `import x "example.com/anthropic/sdk"`), but it names a package,
// not a body of prose or a trailer shape — flagging it would be noise, not a
// leak. ast.Inspect returning false at an *ast.ImportSpec prunes that whole
// subtree (Name and Path) without needing a special case for Path alone.
//
// Comments are never visited as string literals in the first place: a
// go/ast walk only produces *ast.BasicLit nodes for literal expressions, and
// a *ast.CommentGroup's text lives on *ast.Comment.Text (a plain string
// field), never wrapped in a BasicLit — so doc comments that legitimately
// describe the contract (e.g. provider.go's SeedMeta docs, or this very file's
// header) are structurally exempt without any filtering here.
//
// Each literal's value is decoded with strconv.Unquote (handling both
// interpreted "..." strings and raw `...` strings uniformly); on the rare
// unquote failure (should not happen for a literal that already parsed as
// valid Go) the raw token text is matched instead, so a decode hiccup fails
// open to still scanning something rather than silently skipping the literal.
func scanSource(filename string, src []byte) ([]violation, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	var out []violation
	ast.Inspect(f, func(n ast.Node) bool {
		if _, ok := n.(*ast.ImportSpec); ok {
			return false
		}
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			value = lit.Value
		}
		if attributionMarkerPattern.MatchString(value) {
			out = append(out, violation{pos: fset.Position(lit.Pos()), value: value})
		}
		return true
	})
	return out, nil
}

// TestScanSourceHelper pins scanSource's own decisions against synthetic
// sources — the fixtures a reviewer would reach for first when asking "does
// this guard actually distinguish an attribution leak from legitimate core
// noise?" — before TestCoreAttributionNeutrality trusts it over the whole
// tree.
func TestScanSourceHelper(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int // number of violations scanSource must report
	}{
		{
			name: "co-authored-by trailer is flagged regardless of which provider it names",
			src: `package x

func f() string {
	return "Co-Authored-By: ChatGPT <a@openai.com>"
}
`,
			want: 1,
		},
		{
			name: "raw string literal mentioning anthropic.com is flagged",
			src:  "package x\n\nconst blurb = `see anthropic.com for details`\n",
			want: 1,
		},
		{
			name: "an import path is not flagged",
			src: `package x

import x "example.com/anthropic/sdk"

var _ = x.Anything
`,
			want: 0,
		},
		{
			name: "a comment describing the co-authored-by shape is not flagged",
			src: `package x

// matches co-authored-by trailers
func f() {}
`,
			want: 0,
		},
		{
			name: "broad claude literals for CLI/config UX are not flagged",
			src: `package x

const (
	flagName = "claude-config"
	credPath = "~/.claude.json"
)
`,
			want: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scanSource("synthetic.go", []byte(tc.src))
			if err != nil {
				t.Fatalf("scanSource: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("scanSource flagged %d literal(s), want %d: %v", len(got), tc.want, got)
			}
		})
	}
}

// moduleRoot resolves the module root from the package's own directory
// (internal/provider — the test binary's CWD). "../.." is the root exactly
// because this test file lives two directories below it; a go.mod there is
// verified rather than assumed, so a future package move that breaks this
// relative assumption fails loudly instead of silently under-scanning.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolving module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod (internal/provider must sit exactly two directories below the module root): %v", root, err)
	}
	return root
}

// exemptPackageDirs is the set of module-root-relative directories whose
// non-test Go source is skipped entirely: the provider declarations
// themselves and their shared test fakes (issue #75 / ADR-0033 — see the
// package doc comment for why these, and only these, are exempt).
var exemptPackageDirs = []string{
	filepath.Join("internal", "provider", "claudecode"),
	// Provider packages are where attribution-marker declarations legitimately
	// live: the codex adapter (issue #87) declares its own ScrubPatterns as
	// literals, exactly like claudecode.
	filepath.Join("internal", "provider", "codex"),
	filepath.Join("internal", "provider", "providertest"),
}

// scanTree walks root for every non-test *.go file outside the exempt
// packages and directories, running scanSource over each. It returns the
// module-root-relative paths scanned (for the sanity floor) and every
// violation found (for the neutrality assertion) — both computed once and
// shared between the two subtests of TestCoreAttributionNeutrality.
func scanTree(t *testing.T, root string) (scanned []string, violations []violation) {
	t.Helper()

	exempt := make(map[string]bool, len(exemptPackageDirs))
	for _, d := range exemptPackageDirs {
		exempt[filepath.Join(root, d)] = true
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "web", "node_modules", "testdata":
				return filepath.SkipDir
			}
			if exempt[path] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			t.Fatalf("computing %s relative to module root %s: %v", path, root, relErr)
		}
		rel = filepath.ToSlash(rel)
		scanned = append(scanned, rel)

		src, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("reading %s: %v", path, readErr)
		}
		vs, parseErr := scanSource(path, src)
		if parseErr != nil {
			t.Fatalf("parsing %s: %v", path, parseErr)
		}
		violations = append(violations, vs...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking module root %s: %v", root, err)
	}
	return scanned, violations
}

// TestCoreAttributionNeutrality is the arch test itself (issue #75 /
// ADR-0033's "Core neutrality made testable" decision): every non-test Go
// file in the module, outside the exemptPackageDirs adapter/fake packages,
// must be free of the four attribution-marker tokens. Its two subtests mirror web/src/providerNeutral.test.ts's
// describe/it split — a sanity floor first (so a guard silently scanning
// zero files can never read as a pass), then the actual neutrality
// assertion — both run unconditionally so a sanity-floor regression and a
// real leak are never masked by each other.
func TestCoreAttributionNeutrality(t *testing.T) {
	root := moduleRoot(t)
	scanned, violations := scanTree(t, root)
	t.Logf("scanned %d non-test .go file(s) under %s for attribution-marker tokens", len(scanned), root)

	t.Run("scans the real module tree (sanity: the guard must never go blind)", func(t *testing.T) {
		if len(scanned) <= 50 {
			t.Fatalf("only %d file(s) scanned, want more than 50 — the walk likely stopped early", len(scanned))
		}
		want := []string{
			filepath.ToSlash(filepath.Join("internal", "agentapi", "handlers.go")),
			filepath.ToSlash(filepath.Join("cmd", "lab", "main.go")),
		}
		for _, w := range want {
			if !slices.Contains(scanned, w) {
				t.Fatalf("scanned set does not contain %s (scanned %d files total)", w, len(scanned))
			}
		}
	})

	t.Run("contains no attribution-marker token outside the declaration packages", func(t *testing.T) {
		if len(violations) == 0 {
			return
		}
		sort.Slice(violations, func(i, j int) bool {
			return violations[i].pos.String() < violations[j].pos.String()
		})
		var b strings.Builder
		fmt.Fprintf(&b, "found %d attribution-marker literal(s) outside the exempt adapter/fake packages %v (issue #75 / ADR-0033 — refactor into provider-declared metadata, do not exempt here):\n", len(violations), exemptPackageDirs)
		for _, v := range violations {
			fmt.Fprintf(&b, "  %s\n", v)
		}
		t.Fatal(b.String())
	})
}
