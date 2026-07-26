package afk

import (
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
)

// TestFixLabel pins the fix label grammar (issue #182): fix-<N>, [a-z0-9-]
// only — and, load-bearing, that ParseLabel never misparses one as an AFK
// run (no "afk-" prefix, so the strict inverse rejects it outright).
func TestFixLabel(t *testing.T) {
	if got := FixLabel(7); got != "fix-7" {
		t.Errorf("FixLabel(7) = %q, want fix-7", got)
	}
	if got := FixLabel(182); got != "fix-182" {
		t.Errorf("FixLabel(182) = %q, want fix-182", got)
	}
	for _, n := range []int{1, 7, 182} {
		if _, _, ok := ParseLabel(FixLabel(n)); ok {
			t.Errorf("ParseLabel(%q) accepted a fix label as an AFK run", FixLabel(n))
		}
	}
}

// TestFixSeedPromptTemplate pins that the built-in fix template carries the
// LITERAL tokens (the un-interpolated text a preview would serve) and that
// the incogni variant is the only one carrying the attribution sentence,
// attached to the commit step.
func TestFixSeedPromptTemplate(t *testing.T) {
	for _, incogni := range []bool{false, true} {
		tmpl := FixSeedPromptTemplate(incogni)
		for _, token := range []string{gitx.NToken, PRToken, BranchToken} {
			if !strings.Contains(tmpl, token) {
				t.Errorf("template (incogni=%v) missing literal %s:\n%s", incogni, token, tmpl)
			}
		}
	}
	if strings.Contains(FixSeedPromptTemplate(false), "No AI attribution") {
		t.Error("non-incogni template carries the incogni sentence")
	}
	if !strings.Contains(FixSeedPromptTemplate(true), "5. Commit in Conventional Commits style. No AI attribution anywhere — no co-author trailers, no tool-credit footers, no session links.") {
		t.Error("incogni sentence not attached to the commit step")
	}
}

// TestFixSeedPrompt pins the fix seed prompt (issue #182): tokens
// interpolated at every occurrence, the #182 contract lines verbatim (the
// rejection below the separator is the work order, DETACHED worktree,
// explicit refspec push, `labctl pr rerequest` as the done-signal, never
// `labctl pr create`, stop), the rejection content appended verbatim below
// the separator AFTER interpolation, and no separator when there is nothing
// to append.
func TestFixSeedPrompt(t *testing.T) {
	const rejection = "Lander rejection:\nround-2 findings:\n- test X fails"
	p := FixSeedPrompt(7, 9, "afk/7", false, rejection)
	for _, banned := range []string{gitx.NToken, PRToken, BranchToken} {
		if strings.Contains(p, banned) {
			t.Errorf("prompt carries an un-interpolated %s:\n%s", banned, p)
		}
	}
	for _, want := range []string{
		"You are an autonomous fix run. Pull request #9 (head branch `afk/7`) was rejected in review; resolve every finding, push, re-request review, stop.",
		"1. `labctl issue view 7` and `labctl pr view 9` — read fully.",
		"2. Run `labctl pr checks 9`. If checks are red, run `labctl pr logs 9` first and read the failing jobs' logs before any local repro or re-push.",
		"3. The rejection review below is your work order — address every finding. Work only in this worktree; it is DETACHED at the PR head.",
		"4. Run the project's tests, build, and linters; fix what you break.",
		"5. Commit in Conventional Commits style.",
		"6. `git push origin HEAD:refs/heads/afk/7` (detached worktree — push the refspec explicitly).",
		"7. `labctl pr rerequest 9` — never open a new PR; `labctl pr create` is not yours to run.",
		"8. Then stop working. Do not start unrelated work.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	// The rejection rides verbatim below the separator, after every
	// instruction line.
	if !strings.HasSuffix(p, "\n\n---\n\n"+rejection) {
		t.Errorf("rejection not appended below the separator:\n%s", p)
	}

	// No rejection → no separator, the prompt ends at the stop line.
	bare := FixSeedPrompt(7, 9, "afk/7", false, "")
	if strings.Contains(bare, "---") {
		t.Errorf("empty rejection still appended a separator:\n%s", bare)
	}
	if !strings.HasSuffix(bare, "8. Then stop working. Do not start unrelated work.") {
		t.Errorf("bare prompt does not end at the stop line:\n%s", bare)
	}

	// incogni appends the attribution sentence to the commit step only.
	const sentence = "No AI attribution anywhere — no co-author trailers, no tool-credit footers, no session links."
	inc := FixSeedPrompt(7, 9, "afk/7", true, "")
	if !strings.Contains(inc, "5. Commit in Conventional Commits style. "+sentence) {
		t.Errorf("incogni sentence not attached to the commit step:\n%s", inc)
	}
	if strings.Contains(p, sentence) {
		t.Error("non-incogni prompt carries the attribution sentence")
	}

	// The rejection is appended AFTER interpolation: quoted forge prose is
	// never token-substituted, even when a reviewer typed a token literally.
	quoted := FixSeedPrompt(7, 9, "afk/7", false, "reviewer wrote "+BranchToken+" literally")
	if !strings.HasSuffix(quoted, "reviewer wrote "+BranchToken+" literally") {
		t.Errorf("rejection content was token-substituted:\n%s", quoted)
	}
}
