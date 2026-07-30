package reconcile

import (
	"reflect"
	"sort"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// Transcribed from the sessions-spawn / reconcile-parked port specs — the
// behavioral contract for the AFK-label inverse parse and branch ownership.

func TestParseAFKLabel_roundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 7, 62, 1000} {
		for _, auto := range []bool{false, true} {
			label := "afk-"
			if auto {
				label += "auto-"
			}
			label += itoa(n)
			gotN, gotAuto, ok := parseAFKLabel(label)
			if !ok || gotN != n || gotAuto != auto {
				t.Errorf("parseAFKLabel(%q) = (%d,%v,%v), want (%d,%v,true)", label, gotN, gotAuto, ok, n, auto)
			}
		}
	}
}

func TestParseAFKLabel_rejectsNonAFK(t *testing.T) {
	reject := []string{
		"", "debug", "afk", "afk-", "afk-x", "afk-0", "afk--1", "afk-1.5", "afk-1x",
		"notafk-1", "AFK-1", "afk-auto", "afk-auto-", "afk-auto-x", "afk-auto-0",
		"afk-auto--1", "afk-autox", "afk-auto-auto-1", "20260608-1530",
		"debug-20260608-1530", "afk-feature-20260608-1530",
	}
	for _, label := range reject {
		if _, _, ok := parseAFKLabel(label); ok {
			t.Errorf("parseAFKLabel(%q) ok=true, want false", label)
		}
	}
}

func TestInstanceBranch(t *testing.T) {
	tests := []struct {
		afkPattern, manualPrefix, label, want string
	}{
		{"afk/<N>", "lab/", "afk-7", "afk/7"},
		{"afk/<N>", "lab/", "afk-auto-7", "afk/7"}, // auto marker never reaches the branch
		{"afk/<N>", "lab/", "lander-7", "afk/7"},   // the lander adopts issue 7's claim branch (issue #181)
		{"afk/<N>", "lab/", "fix-5", "afk/5"},      // a fix run adopts issue 5's claim branch (issue #182)
		{"afk/<N>", "lab/", "escalate-5", "afk/5"}, // so does the escalate-mode lander
		{"afk/<N>", "lab/", "20260608-1530", "lab/20260608-1530"},
		{"afk/<N>", "lab/", "debug-20260608-1530", "lab/debug-20260608-1530"},
		// A scheduled run's label (issue #247 / ADR-0062) is deliberately
		// outside every parsed namespace, so the manual fallback derives its
		// branch — the exact string the spawn side composes (manual prefix +
		// label), which is what keeps re-adoption naming-code-free.
		{"afk/<N>", "lab/", "sched-3f9a1c-20260730-0600", "lab/sched-3f9a1c-20260730-0600"},
		{"afk/<N>", "lab/", "lander-x", "lab/lander-x"}, // strict inverse: not a lander label
		{"afk/<N>", "lab/", "lander-0", "lab/lander-0"},
		{"afk/<N>", "lab/", "fix-x", "lab/fix-x"}, // strict inverse holds for the #182 prefixes too
		{"afk/<N>", "lab/", "fix-0", "lab/fix-0"},
		{"afk/<N>", "lab/", "escalate--1", "lab/escalate--1"},
		// Per-repo patterns (incogni): nothing assumes the literal afk//lab/.
		{"issue-<N>", "wip/", "afk-9", "issue-9"},
		{"issue-<N>", "wip/", "lander-9", "issue-9"},
		{"issue-<N>", "wip/", "fix-9", "issue-9"},
		{"issue-<N>", "wip/", "escalate-9", "issue-9"},
		{"issue-<N>", "wip/", "feature", "wip/feature"},
	}
	for _, tt := range tests {
		if got := instanceBranch(tt.afkPattern, tt.manualPrefix, tt.label); got != tt.want {
			t.Errorf("instanceBranch(%q,%q,%q) = %q, want %q", tt.afkPattern, tt.manualPrefix, tt.label, got, tt.want)
		}
	}
}

func TestOwnedBranches(t *testing.T) {
	// Two providers' login sessions coexist, plus the legacy bare name — none
	// of them own a branch (issue #77).
	sessions := []string{
		"proj~afk-7", "proj~afk-auto-8", "proj~feature-20260608-1530", "proj~20260608-1600",
		"proj~lander-4",                 // a live lander owns the claim branch it adopted (issue #181)
		"proj~fix-5", "proj~escalate-6", // so do live fix/escalate runs (issue #182) — a restart must re-adopt, never park them
		"projfoo~afk-9", "other~afk-3", tmuxx.LoginSessionName("claude-code"),
		tmuxx.LoginSessionName("codex"), "lab-login",
	}
	got := ownedBranches("afk/<N>", "lab/", sessions, "proj")
	want := map[string]bool{
		"afk/7": true, "afk/8": true, "afk/4": true, "afk/5": true, "afk/6": true,
		"lab/feature-20260608-1530": true, "lab/20260608-1600": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ownedBranches = %v, want %v", keys(got), keys(want))
	}
	// No live session owns anything.
	if empty := ownedBranches("afk/<N>", "lab/", nil, "proj"); len(empty) != 0 {
		t.Errorf("ownedBranches(nil) = %v, want empty", empty)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
