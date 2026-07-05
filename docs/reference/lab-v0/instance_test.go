package main

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestComposeParseRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   instanceID
	}{
		{"manual unlabelled (timestamp)", instanceID{Project: "foo", Label: "20260608-1530"}},
		{"manual labelled", instanceID{Project: "foo", Label: "debug-20260608-1530"}},
		{"manual dashed user label", instanceID{Project: "foo", Label: "my-feature-20260608-1530"}},
		{"afk manual", instanceID{Project: "foo", Label: "afk-7"}},
		{"afk auto", instanceID{Project: "foo", Label: "afk-auto-7"}},
		{"project with dashes", instanceID{Project: "foo-bar", Label: "20260608-1530"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := composeSessionName(tc.id)
			if want := tc.id.Project + "~" + tc.id.Label; name != want {
				t.Fatalf("composeSessionName(%+v) = %q; want %q", tc.id, name, want)
			}
			if back := parseSessionName(name); back != tc.id {
				t.Errorf("parseSessionName(%q) = %+v; want %+v", name, back, tc.id)
			}
		})
	}
}

func TestParseSessionName_splitsOnFirstTilde(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want instanceID
	}{
		{"foo~20260608-1530", instanceID{Project: "foo", Label: "20260608-1530"}},
		{"foo~debug-20260608-1530", instanceID{Project: "foo", Label: "debug-20260608-1530"}},
		{"foo~afk-7", instanceID{Project: "foo", Label: "afk-7"}},
		{"foo-bar~20260608-1530", instanceID{Project: "foo-bar", Label: "20260608-1530"}},
		// Split is on the FIRST "~": everything after it is the label verbatim. A
		// sanitised label can't actually contain "~", so this only documents
		// robustness against a hand-made session.
		{"foo~a~b", instanceID{Project: "foo", Label: "a~b"}},
		// No separator → a bare/legacy name is the whole project, empty label.
		{"foo", instanceID{Project: "foo"}},
		{"foo~", instanceID{Project: "foo"}},
	} {
		if got := parseSessionName(tc.in); got != tc.want {
			t.Errorf("parseSessionName(%q) = %+v; want %+v", tc.in, got, tc.want)
		}
	}
}

func TestBelongsTo_prefixSafety(t *testing.T) {
	for _, tc := range []struct {
		name            string
		session, projct string
		want            bool
	}{
		{"timestamp instance belongs", "foo~20260608-1530", "foo", true},
		{"labelled instance belongs", "foo~dbg-20260608-1530", "foo", true},
		{"afk instance belongs", "foo~afk-7", "foo", true},
		{"longer project name does not belong", "foobar~20260608-1530", "foo", false},
		{"foo instance does not belong to foobar", "foo~x", "foobar", false},
		{"unrelated", "bar~x", "foo", false},
		{"dashed project instance", "foo-bar~20260608-1530", "foo-bar", true},
		{"foo does not own foo-bar", "foo-bar~x", "foo", false},
		{"legacy bare name belongs", "foo", "foo", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := belongsTo(tc.session, tc.projct); got != tc.want {
				t.Errorf("belongsTo(%q, %q) = %v; want %v", tc.session, tc.projct, got, tc.want)
			}
		})
	}
}

func TestInstanceBranch(t *testing.T) {
	for _, tc := range []struct {
		id   instanceID
		want string
	}{
		{instanceID{Project: "foo", Label: "afk-7"}, "afk/7"},
		{instanceID{Project: "foo", Label: "afk-auto-7"}, "afk/7"},
		{instanceID{Project: "foo", Label: "20260608-1530"}, "lab/20260608-1530"},
		{instanceID{Project: "foo", Label: "debug-20260608-1530"}, "lab/debug-20260608-1530"},
	} {
		if got := instanceBranch(tc.id); got != tc.want {
			t.Errorf("instanceBranch(%+v) = %q; want %q", tc.id, got, tc.want)
		}
	}
}

func TestWorktreeDir(t *testing.T) {
	for _, tc := range []struct {
		id   instanceID
		want string
	}{
		// AFK: <project>-<N> from the issue number, regardless of manual/auto kind.
		{instanceID{Project: "foo", Label: "afk-7"}, "foo-7"},
		{instanceID{Project: "foo", Label: "afk-auto-7"}, "foo-7"},
		// Manual: <project>-<label>.
		{instanceID{Project: "foo", Label: "20260608-1530"}, "foo-20260608-1530"},
		{instanceID{Project: "foo", Label: "debug-20260608-1530"}, "foo-debug-20260608-1530"},
	} {
		got := worktreeDir(tc.id)
		if got != tc.want {
			t.Errorf("worktreeDir(%+v) = %q; want %q", tc.id, got, tc.want)
		}
		// The property that gates AFK edits: never a "~" (8.3 short-name heuristic).
		if strings.ContainsRune(got, '~') {
			t.Errorf("worktreeDir(%+v) = %q; must not contain '~'", tc.id, got)
		}
	}
}

func TestManualLabel(t *testing.T) {
	at := time.Date(2026, 6, 8, 15, 30, 0, 0, time.UTC)
	if got := manualLabel("", at); got != "20260608-1530" {
		t.Errorf("manualLabel(unlabelled) = %q; want 20260608-1530", got)
	}
	if got := manualLabel("debug", at); got != "debug-20260608-1530" {
		t.Errorf("manualLabel(debug) = %q; want debug-20260608-1530", got)
	}
}

func TestParseManualLabel(t *testing.T) {
	for _, tc := range []struct {
		label, user, hhmm string
	}{
		{"20260608-1530", "", "15:30"},
		{"debug-20260608-1530", "debug", "15:30"},
		{"my-feature-20260608-0905", "my-feature", "09:05"},
		{"20260608-0000", "", "00:00"},
		// No well-formed trailing timestamp → whole thing is the user portion, no
		// time (a hand-made or legacy session).
		{"debug", "debug", ""},
		{"", "", ""},
		{"123", "123", ""},
	} {
		user, hhmm := parseManualLabel(tc.label)
		if user != tc.user || hhmm != tc.hhmm {
			t.Errorf("parseManualLabel(%q) = (%q,%q); want (%q,%q)", tc.label, user, hhmm, tc.user, tc.hhmm)
		}
	}
}

func TestUniqueManualLabel_bumpsMinuteOnCollision(t *testing.T) {
	at := time.Date(2026, 6, 8, 15, 30, 0, 0, time.UTC)
	// No collision: the base timestamp is used as-is.
	if got := uniqueManualLabel("foo", "", at, nil); got != "20260608-1530" {
		t.Errorf("no-collision = %q; want 20260608-1530", got)
	}
	// foo~20260608-1530 taken → bump one minute.
	taken := map[string]bool{"foo~20260608-1530": true}
	if got := uniqueManualLabel("foo", "", at, taken); got != "20260608-1531" {
		t.Errorf("one-collision = %q; want bump to 20260608-1531", got)
	}
	// Two consecutive minutes taken → bump twice.
	taken["foo~20260608-1531"] = true
	if got := uniqueManualLabel("foo", "", at, taken); got != "20260608-1532" {
		t.Errorf("two-collision = %q; want 20260608-1532", got)
	}
	// A labelled collision bumps the same way, keeping the user prefix.
	takenL := map[string]bool{"foo~debug-20260608-1530": true}
	if got := uniqueManualLabel("foo", "debug", at, takenL); got != "debug-20260608-1531" {
		t.Errorf("labelled-collision = %q; want debug-20260608-1531", got)
	}
	// A same-timestamp collision on a DIFFERENT project doesn't bump this one.
	if got := uniqueManualLabel("bar", "", at, taken); got != "20260608-1530" {
		t.Errorf("other-project collision leaked: %q; want 20260608-1530", got)
	}
}

func TestSanitizeLabel(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"debug", "debug"},
		{"  trimmed  ", "trimmed"},
		{"my feature", "my_feature"},
		{"tilde~here", "tilde_here"}, // "~" can never survive into a label
		{"slash/here", "slash-here"}, // same rule as project names
		{"weird:*chars", "weird__chars"},
		{"", ""},
		{"   ", ""},
		// "." would land in the "project~label" session name and break tmux
		// targeting exactly as in a project name, so it must convert; "_"/"-" survive.
		{"dot.keep_and-dash", "dot_keep_and-dash"},
	} {
		if got := sanitizeLabel(tc.in); got != tc.want {
			t.Errorf("sanitizeLabel(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestAFKLabel_roundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 7, 62, 1000} {
		for _, auto := range []bool{false, true} {
			label := afkLabel(n, auto)
			want := "afk-" + strconv.Itoa(n)
			if auto {
				want = "afk-auto-" + strconv.Itoa(n)
			}
			if label != want {
				t.Errorf("afkLabel(%d, %v) = %q; want %q", n, auto, label, want)
			}
			if gotN, gotAuto, ok := parseAFKLabel(label); !ok || gotN != n || gotAuto != auto {
				t.Errorf("parseAFKLabel(%q) = (%d,%v,%v); want (%d,%v,true)", label, gotN, gotAuto, ok, n, auto)
			}
			// Through the full session-name scheme <project>~afk[-auto]-<N>: both the
			// issue number AND the kind must survive compose → parse → parseAFKLabel —
			// the round-trip a restart leans on to re-adopt runs with the right kind.
			name := composeSessionName(instanceID{Project: "proj", Label: label})
			if want := "proj~" + label; name != want {
				t.Errorf("composeSessionName(afk %d auto=%v) = %q; want %q", n, auto, name, want)
			}
			gotN, gotAuto, ok := parseAFKLabel(parseSessionName(name).Label)
			if !ok || gotN != n || gotAuto != auto {
				t.Errorf("round-trip parseAFKLabel(%q) = (%d,%v,%v); want (%d,%v,true)", name, gotN, gotAuto, ok, n, auto)
			}
		}
	}
}

func TestParseAFKLabel_rejectsNonAFK(t *testing.T) {
	// Ordinary labels, the bare prefix(es), malformed/non-positive suffixes, and —
	// critically — manual labels (which always end in a dashed timestamp) must
	// never be mistaken for an AFK run.
	for _, in := range []string{
		"", "debug", "afk", "afk-", "afk-x", "afk-0", "afk--1", "afk-1.5", "afk-1x", "notafk-1", "AFK-1",
		"afk-auto", "afk-auto-", "afk-auto-x", "afk-auto-0", "afk-auto--1", "afk-autox", "afk-auto-auto-1",
		"20260608-1530", "debug-20260608-1530", "afk-feature-20260608-1530",
	} {
		if n, auto, ok := parseAFKLabel(in); ok {
			t.Errorf("parseAFKLabel(%q) = (%d,%v,true); want ok=false", in, n, auto)
		}
	}
}
