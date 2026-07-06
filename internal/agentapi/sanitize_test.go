package agentapi

// Incogni measure 3 tests (D15 §9): the server-side attribution stripping
// on PR/CR bodies — table tests over stripAttribution/sanitizeBody plus the
// handler-level contract that an incogni repo's PR create reaches the
// tracker sanitized while a non-incogni body passes through byte-identical.

import (
	"encoding/json"
	"net/http"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripAttribution(tt.in); got != tt.want {
				t.Errorf("stripAttribution(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeBodyGatesOnIncogni(t *testing.T) {
	poisoned := "Done.\n\nCo-Authored-By: Claude <noreply@anthropic.com>"
	if got := sanitizeBody(store.Repo{Incogni: false}, poisoned); got != poisoned {
		t.Errorf("non-incogni body altered: %q", got)
	}
	if got := sanitizeBody(store.Repo{Incogni: true}, poisoned); got != "Done." {
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
