package gitx

import (
	"strings"
	"testing"
	"time"
)

// The tables below are transcribed from v0 instance_test.go via the
// git-worktrees port-spec §2.2–§2.6 — they are the naming contract.

func TestComposeParseRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		repo, label string
	}{
		{"manual unlabelled (timestamp)", "foo", "20260608-1530"},
		{"manual labelled", "foo", "debug-20260608-1530"},
		{"manual dashed user label", "foo", "my-feature-20260608-1530"},
		{"afk manual", "foo", "afk-7"},
		{"afk auto", "foo", "afk-auto-7"},
		{"repo with dashes", "foo-bar", "20260608-1530"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name := ComposeSessionName(tt.repo, tt.label)
			if want := tt.repo + "~" + tt.label; name != want {
				t.Fatalf("ComposeSessionName(%q, %q) = %q; want %q", tt.repo, tt.label, name, want)
			}
			if repo, label := ParseSessionName(name); repo != tt.repo || label != tt.label {
				t.Errorf("ParseSessionName(%q) = (%q, %q); want (%q, %q)", name, repo, label, tt.repo, tt.label)
			}
		})
	}
}

func TestParseSessionName_splitsOnFirstTilde(t *testing.T) {
	tests := []struct {
		in                  string
		wantRepo, wantLabel string
	}{
		{"foo~20260608-1530", "foo", "20260608-1530"},
		{"foo~debug-20260608-1530", "foo", "debug-20260608-1530"},
		{"foo~afk-7", "foo", "afk-7"},
		{"foo-bar~20260608-1530", "foo-bar", "20260608-1530"},
		// Split is on the FIRST "~": everything after it is the label
		// verbatim. A sanitized label can't actually contain "~", so this
		// only documents robustness against a hand-made session.
		{"foo~a~b", "foo", "a~b"},
		// No separator → a bare/legacy name is the whole repo, empty label.
		{"foo", "foo", ""},
		{"foo~", "foo", ""},
	}
	for _, tt := range tests {
		if repo, label := ParseSessionName(tt.in); repo != tt.wantRepo || label != tt.wantLabel {
			t.Errorf("ParseSessionName(%q) = (%q, %q); want (%q, %q)", tt.in, repo, label, tt.wantRepo, tt.wantLabel)
		}
	}
}

func TestBelongsTo_prefixSafety(t *testing.T) {
	tests := []struct {
		name          string
		session, repo string
		want          bool
	}{
		{"timestamp instance belongs", "foo~20260608-1530", "foo", true},
		{"labelled instance belongs", "foo~dbg-20260608-1530", "foo", true},
		{"afk instance belongs", "foo~afk-7", "foo", true},
		{"longer repo name does not belong", "foobar~20260608-1530", "foo", false},
		{"foo instance does not belong to foobar", "foo~x", "foobar", false},
		{"unrelated", "bar~x", "foo", false},
		{"dashed repo instance", "foo-bar~20260608-1530", "foo-bar", true},
		{"foo does not own foo-bar", "foo-bar~x", "foo", false},
		{"legacy bare name belongs", "foo", "foo", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BelongsTo(tt.session, tt.repo); got != tt.want {
				t.Errorf("BelongsTo(%q, %q) = %v; want %v", tt.session, tt.repo, got, tt.want)
			}
		})
	}
}

func TestWorktreeDir(t *testing.T) {
	tests := []struct {
		repo, label string
		want        string
	}{
		// Manual: <repoName>-<label>.
		{"foo", "20260608-1530", "foo-20260608-1530"},
		{"foo", "debug-20260608-1530", "foo-debug-20260608-1530"},
		// AFK (M5): the caller passes the bare issue number, both kinds —
		// <repoName>-<N> (v0 TestWorktreeDir rows for afk-7 / afk-auto-7).
		{"foo", "7", "foo-7"},
	}
	for _, tt := range tests {
		got := WorktreeDir(tt.repo, tt.label)
		if got != tt.want {
			t.Errorf("WorktreeDir(%q, %q) = %q; want %q", tt.repo, tt.label, got, tt.want)
		}
		// The property that gates unattended edits: never a "~" in a path
		// (Windows 8.3 short-name heuristic).
		if strings.ContainsRune(got, '~') {
			t.Errorf("WorktreeDir(%q, %q) = %q; must not contain '~'", tt.repo, tt.label, got)
		}
	}
}

func TestManualLabel(t *testing.T) {
	at := time.Date(2026, 6, 8, 15, 30, 0, 0, time.UTC)
	if got := ManualLabel("", at); got != "20260608-1530" {
		t.Errorf("ManualLabel(unlabelled) = %q; want 20260608-1530", got)
	}
	if got := ManualLabel("debug", at); got != "debug-20260608-1530" {
		t.Errorf("ManualLabel(debug) = %q; want debug-20260608-1530", got)
	}
}

func TestParseManualLabel(t *testing.T) {
	tests := []struct {
		label, user, hhmm string
	}{
		{"20260608-1530", "", "15:30"},
		{"debug-20260608-1530", "debug", "15:30"},
		{"my-feature-20260608-0905", "my-feature", "09:05"},
		{"20260608-0000", "", "00:00"},
		// No well-formed trailing timestamp → whole thing is the user
		// portion, no time (a hand-made or legacy session).
		{"debug", "debug", ""},
		{"", "", ""},
		{"123", "123", ""},
	}
	for _, tt := range tests {
		user, hhmm := ParseManualLabel(tt.label)
		if user != tt.user || hhmm != tt.hhmm {
			t.Errorf("ParseManualLabel(%q) = (%q,%q); want (%q,%q)", tt.label, user, hhmm, tt.user, tt.hhmm)
		}
	}
}

func TestUniqueManualLabel_bumpsMinuteOnCollision(t *testing.T) {
	at := time.Date(2026, 6, 8, 15, 30, 0, 0, time.UTC)
	// No collision: the base timestamp is used as-is.
	if got := UniqueManualLabel("foo", "", at, nil); got != "20260608-1530" {
		t.Errorf("no-collision = %q; want 20260608-1530", got)
	}
	// foo~20260608-1530 taken → bump one minute.
	taken := map[string]bool{"foo~20260608-1530": true}
	if got := UniqueManualLabel("foo", "", at, taken); got != "20260608-1531" {
		t.Errorf("one-collision = %q; want bump to 20260608-1531", got)
	}
	// Two consecutive minutes taken → bump twice.
	taken["foo~20260608-1531"] = true
	if got := UniqueManualLabel("foo", "", at, taken); got != "20260608-1532" {
		t.Errorf("two-collision = %q; want 20260608-1532", got)
	}
	// A labelled collision bumps the same way, keeping the user prefix.
	takenL := map[string]bool{"foo~debug-20260608-1530": true}
	if got := UniqueManualLabel("foo", "debug", at, takenL); got != "debug-20260608-1531" {
		t.Errorf("labelled-collision = %q; want debug-20260608-1531", got)
	}
	// Collision is keyed by the FULL session name: a same-timestamp
	// collision on a DIFFERENT repo doesn't bump this one.
	if got := UniqueManualLabel("bar", "", at, taken); got != "20260608-1530" {
		t.Errorf("other-repo collision leaked: %q; want 20260608-1530", got)
	}
}

func TestSanitizeLabel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"debug", "debug"},
		{"  trimmed  ", "trimmed"},
		{"my feature", "my_feature"},
		{"tilde~here", "tilde_here"}, // "~" can never survive into a label
		{"slash/here", "slash-here"}, // same rule as repo names
		{"weird:*chars", "weird__chars"},
		{"", ""},
		{"   ", ""},
		// "." would land in the "repo~label" session name and break tmux
		// targeting exactly as in a repo name, so it must convert; "_"/"-"
		// survive.
		{"dot.keep_and-dash", "dot_keep_and-dash"},
	}
	for _, tt := range tests {
		if got := SanitizeLabel(tt.in); got != tt.want {
			t.Errorf("SanitizeLabel(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

// TestManagedBranch: the v0 managedBranch table (port-spec §2.9) under the
// default patterns, plus the per-repo rows design §4a/§11 add — the incogni
// defaults ("issue-<N>" + "wip/") and the strict-inverse-parse rule for
// non-renderings under the afk prefix.
func TestManagedBranch(t *testing.T) {
	tests := []struct {
		afkPattern, manualPrefix, branch string
		want                             bool
	}{
		// v0 table, default patterns.
		{"afk/<N>", "lab/", "lab/x-20260608-1530", true},
		{"afk/<N>", "lab/", "afk/7", true},
		{"afk/<N>", "lab/", "main", false},
		{"afk/<N>", "lab/", "feature/x", false},
		{"afk/<N>", "lab/", "laboratory/x", false}, // prefix is "lab/", not "lab"
		{"afk/<N>", "lab/", "", false},
		// Strict inverse parse: a stray branch under the afk prefix that is
		// not an exact rendering never registers as managed.
		{"afk/<N>", "lab/", "afk/notanumber", false},
		{"afk/<N>", "lab/", "afk/0", false},
		{"afk/<N>", "lab/", "afk/007", false},

		// Incogni defaults: issue-<N> + wip/.
		{"issue-<N>", "wip/", "issue-7", true},
		{"issue-<N>", "wip/", "issue-x", false},
		{"issue-<N>", "wip/", "issue-0", false},
		{"issue-<N>", "wip/", "issues-7", false},
		{"issue-<N>", "wip/", "wip/x-20260608-1530", true},
		{"issue-<N>", "wip/", "lab/x-20260608-1530", false},
		{"issue-<N>", "wip/", "afk/7", false},
		{"issue-<N>", "wip/", "main", false},
	}
	for _, tt := range tests {
		if got := ManagedBranch(tt.afkPattern, tt.manualPrefix, tt.branch); got != tt.want {
			t.Errorf("ManagedBranch(%q, %q, %q) = %v; want %v", tt.afkPattern, tt.manualPrefix, tt.branch, got, tt.want)
		}
	}
}
