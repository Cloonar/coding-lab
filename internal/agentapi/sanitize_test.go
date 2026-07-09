package agentapi

// Incogni measure 3 tests (D15 §9): the server-side attribution stripping
// on PR/CR bodies — table tests over stripAttribution/sanitizeBody plus the
// handler-level contract that an incogni repo's PR create reaches the
// tracker sanitized while a non-incogni body passes through byte-identical.

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// claudeScrubPatterns is a pinned fixture twin of claudecode's declared
// SeedMeta().ScrubPatterns (internal/provider/claudecode): agentapi tests
// must NOT import the concrete provider, so the four patterns live here as
// string literals. Drift between this twin and the real declaration is caught
// by claudecode's own seedmeta tests, not here (ADR-0033).
var claudeScrubPatterns = []string{
	`co-authored-by:[[:space:]]*claude`,
	`co-authored-by:.*<[^>]*@anthropic\.com>`,
	`generated with.*claude`,
	`claude-session:`,
}

// codexScrubPatterns is a second, hypothetical provider's declaration (a
// ChatGPT/@openai.com trailer shape, the #2 Codex motivation of ADR-0033). It
// proves the sanitizer carries nothing claude-shaped in core: a codex-only set
// strips codex footers and leaves claude-shaped lines untouched.
var codexScrubPatterns = []string{
	`co-authored-by:.*<[^>]*@openai\.com>`,
	`codex-session:`,
	`generated with.*codex`,
}

// claudeScrub and codexScrub are the fixture pattern sets compiled exactly as
// production compiles a provider's declaration for agentapi.New — through the
// real provider.CompileScrubPatterns (the `(?i)` case-insensitive twin).
var (
	claudeScrub = mustScrub(claudeScrubPatterns)
	codexScrub  = mustScrub(codexScrubPatterns)
)

func mustScrub(patterns []string) []*regexp.Regexp {
	res, err := provider.CompileScrubPatterns(patterns)
	if err != nil {
		panic(err)
	}
	return res
}

func TestStripAttribution(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean body untouched", "Implements the thing.\n\nCloses #7", "Implements the thing.\n\nCloses #7"},
		{"clean body with odd blank runs untouched",
			"a\n\n\n\nb\n\n", "a\n\n\n\nb\n\n"},
		{"canonical claude footer pair",
			"Fix the parser.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\nCo-Authored-By: Claude <noreply@anthropic.com>",
			"Fix the parser."},
		{"multi-footer body",
			"Summary.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nCo-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>\n\nGenerated with Claude Code",
			"Summary."},
		{"trailer in the middle collapses the blank run",
			"para one\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n\npara two",
			"para one\n\npara two"},
		{"any-case co-authored-by",
			"Done.\n\nCO-AUTHORED-BY: CLAUDE <noreply@anthropic.com>", "Done."},
		{"no space after colon",
			"Done.\n\nCo-Authored-By:Claude <noreply@anthropic.com>", "Done."},
		{"human co-author kept",
			"Done.\n\nCo-Authored-By: Alice <alice@example.com>",
			"Done.\n\nCo-Authored-By: Alice <alice@example.com>"},
		{"generated-with variant without brackets",
			"Body.\n\nGenerated with Claude Code", "Body."},
		{"generated-with prose variant",
			"Body.\n\nThis PR was generated with Claude.", "Body."},
		{"generated-with without claude kept",
			"Docs generated with pandoc.", "Docs generated with pandoc."},
		{"claude-session trailer",
			"Body.\n\nClaude-Session: https://claude.ai/code/session_x", "Body."},
		{"indented trailer",
			"Body.\n\n  Co-Authored-By: Claude <noreply@anthropic.com>", "Body."},
		{"attribution-only body becomes empty",
			"🤖 Generated with [Claude Code](https://claude.com/claude-code)\nCo-Authored-By: Claude <noreply@anthropic.com>", ""},
		{"leading footer leaves no leading blank",
			"Co-Authored-By: Claude <noreply@anthropic.com>\n\nActual body.", "Actual body."},
		{"in-fence blank run preserved (seam-local collapse, not global)",
			"Text.\n\n```\nheader\n\n\npayload\n```\n\nGenerated with Claude Code",
			"Text.\n\n```\nheader\n\n\npayload\n```"},
		{"closes directive survives around a stripped footer",
			"Fix it.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\nCloses #7",
			"Fix it.\n\nCloses #7"},
		// ADR-0033 reconciliation delta: the old hardcoded attributionLine
		// stripped ANY bare "anthropic.com" mention after the co-authored-by
		// prefix; the declared pattern requires the BRACKETED email
		// (`<…@anthropic.com>`) — the stable discriminator ADR-0012's review
		// pinned. So an unbracketed mention is now KEPT: deliberate coverage
		// narrowing to match the hook's shape, byte-identical to its golden.
		{"unbracketed anthropic mention kept (declared bracketed-email shape)",
			"Done.\n\nCo-Authored-By: Fable at anthropic.com",
			"Done.\n\nCo-Authored-By: Fable at anthropic.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripAttribution(tt.in, claudeScrub); got != tt.want {
				t.Errorf("stripAttribution(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestStripAttributionProviderAgnostic proves nothing claude-shaped is
// hardcoded: a codex-only pattern set strips a ChatGPT/@openai.com trailer and
// a Codex-Session footer while a claude-shaped Co-Authored-By line — which no
// codex pattern matches — is left in place (ADR-0033).
func TestStripAttributionProviderAgnostic(t *testing.T) {
	in := "Implements the fix.\n\n" +
		"Co-Authored-By: Claude <noreply@anthropic.com>\n\n" +
		"Co-authored-by: ChatGPT <noreply@openai.com>\n" +
		"Codex-Session: https://chatgpt.example/c/session_x"
	want := "Implements the fix.\n\nCo-Authored-By: Claude <noreply@anthropic.com>"
	if got := stripAttribution(in, codexScrub); got != want {
		t.Errorf("stripAttribution(codex) = %q, want %q", got, want)
	}
}

// TestStripAttributionUnion pins the cross-provider union: claude+codex sets
// together strip BOTH providers' footers from one body in one pass — the
// production shape, where scrub is the registry-wide union (ADR-0033).
func TestStripAttributionUnion(t *testing.T) {
	union := slices.Concat(claudeScrub, codexScrub)
	in := "Summary.\n\n" +
		"🤖 Generated with [Claude Code](https://claude.com/claude-code)\n" +
		"Co-authored-by: ChatGPT <noreply@openai.com>\n\n" +
		"Co-Authored-By: Claude <noreply@anthropic.com>\n" +
		"Codex-Session: https://chatgpt.example/c/session_x"
	if got := stripAttribution(in, union); got != "Summary." {
		t.Errorf("stripAttribution(union) = %q, want %q", got, "Summary.")
	}
}

// TestStripAttributionEmptyScrub pins the content-inert degradation: with no
// registered providers (nil/empty scrub) no markers are known, so an otherwise
// poisoned incogni body passes through byte-identical — mirroring the pre-push
// hook's empty-list handling (ADR-0033).
func TestStripAttributionEmptyScrub(t *testing.T) {
	poisoned := "Done.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\n" +
		"Co-Authored-By: Claude <noreply@anthropic.com>"
	for _, scrub := range [][]*regexp.Regexp{nil, {}} {
		if got := stripAttribution(poisoned, scrub); got != poisoned {
			t.Errorf("stripAttribution(empty) = %q, want the body unchanged", got)
		}
	}
	// The same content-inert pass-through through the Server method: an incogni
	// repo with a scrub-less server strips nothing.
	s := &Server{}
	if got := s.sanitizeBody(store.Repo{Incogni: true}, poisoned); got != poisoned {
		t.Errorf("sanitizeBody(nil scrub) = %q, want the body unchanged", got)
	}
}

func TestSanitizeBodyGatesOnIncogni(t *testing.T) {
	// In-package: construct the Server with the claude fixture scrub directly.
	s := &Server{scrub: claudeScrub}
	poisoned := "Done.\n\nCo-Authored-By: Claude <noreply@anthropic.com>"
	if got := s.sanitizeBody(store.Repo{Incogni: false}, poisoned); got != poisoned {
		t.Errorf("non-incogni body altered: %q", got)
	}
	if got := s.sanitizeBody(store.Repo{Incogni: true}, poisoned); got != "Done." {
		t.Errorf("incogni body = %q, want %q", got, "Done.")
	}
}

// TestIssueAndCommentCreateSanitizeIncogniBody pins the ADR-0014 widening:
// EVERY agent-authored body — issue create and comment create, not just
// PR/CR — passes the sanitizer on incogni repos, so the defense-in-depth
// story has no unsanitized write path. Non-incogni bodies pass through
// byte-identical.
func TestIssueAndCommentCreateSanitizeIncogniBody(t *testing.T) {
	poisoned := "Found while testing.\n\nCo-Authored-By: Claude <noreply@anthropic.com>"

	tests := []struct {
		name     string
		incogni  bool
		wantBody string
	}{
		{"incogni stripped", true, "Found while testing."},
		{"non-incogni passthrough", false, poisoned},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedRepoIncogni(t, "repo_f", "forge", "forgejo", tt.incogni)
			f.seedRunKind(t, "run_f", "repo_f", "afk_auto", "active", intp(7), "afk/7")
			token := f.seedToken(t, "run_f", nil)

			fk := &fakeTracker{}
			handler := f.forgeServer(fk).Handler()

			payload, _ := json.Marshal(map[string]string{"title": "bug: the thing", "body": poisoned})
			rr := doJSON(t, handler, "POST", "/agent/v1/issues", token, string(payload))
			if rr.Code != http.StatusCreated {
				t.Fatalf("issue create status = %d, body %s", rr.Code, rr.Body.String())
			}
			if fk.createdIssue == nil || fk.createdIssue.body != tt.wantBody {
				t.Errorf("tracker received issue body %+v, want %q", fk.createdIssue, tt.wantBody)
			}

			payload, _ = json.Marshal(map[string]string{"body": poisoned})
			rr = doJSON(t, handler, "POST", "/agent/v1/issues/7/comments", token, string(payload))
			if rr.Code != http.StatusCreated {
				t.Fatalf("comment create status = %d, body %s", rr.Code, rr.Body.String())
			}
			if len(fk.comments) != 1 || fk.comments[0].body != tt.wantBody {
				t.Errorf("tracker received comment %+v, want body %q", fk.comments, tt.wantBody)
			}
		})
	}
}

// The handler-level contract: on an incogni repo the tracker receives the
// sanitized body — Closes-injection included (ensureCloses runs first, so
// the injected directive survives) — and on a non-incogni repo the exact
// bytes the agent sent.
func TestPRCreateSanitizesIncogniBody(t *testing.T) {
	poisoned := "Implements the fix.\n\n" +
		"🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\n" +
		"Co-Authored-By: Claude <noreply@anthropic.com>"

	tests := []struct {
		name     string
		incogni  bool
		wantBody string
	}{
		{"incogni stripped and closes injected", true, "Implements the fix.\n\nCloses #7"},
		{"non-incogni passthrough", false, poisoned + "\n\nCloses #7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.seedRepoIncogni(t, "repo_f", "forge", "forgejo", tt.incogni)
			f.seedRunKind(t, "run_f", "repo_f", "afk_auto", "active", intp(7), "issue-7")
			token := f.seedToken(t, "run_f", nil)

			fk := &fakeTracker{pullRef: tracker.PullRef{Number: 5, URL: "https://forge.example/pr/5"}}
			handler := f.forgeServer(fk).Handler()

			payload, _ := json.Marshal(map[string]string{"title": "fix: the thing", "body": poisoned})
			rr := doJSON(t, handler, "POST", "/agent/v1/prs", token, string(payload))
			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
			}
			if fk.createdPull == nil {
				t.Fatal("CreatePull not called")
			}
			if fk.createdPull.body != tt.wantBody {
				t.Errorf("tracker received body %q, want %q", fk.createdPull.body, tt.wantBody)
			}
		})
	}
}
