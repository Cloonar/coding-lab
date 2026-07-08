package seeder

// Byte-identity proof for the incogni pre-push hook (issue #51 decision 8
// acceptance: "the installed hook for claude-code must be byte-identical to
// today"). testdata/pre-push-claude.golden is the exact script the hook
// emitted BEFORE the seam split, when the patterns were package vars; this
// test renders the generic prePushHookScript with claude-code's golden
// patterns and asserts the bytes are unchanged. A refactor that reshapes the
// script — even by a space — fails here.

import (
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

//go:embed testdata/pre-push-claude.golden
var claudePrePushGolden string

func TestPrePushHookScript_claudeGolden(t *testing.T) {
	got := prePushHookScript(claudeGoldenMeta.ScrubPatterns, claudeGoldenMeta.SeededPathPatterns)
	if got != claudePrePushGolden {
		t.Errorf("pre-push hook script drifted from the pinned claude golden.\n--- got ---\n%s\n--- want ---\n%s", got, claudePrePushGolden)
	}
}

// Empty-pattern degradation (issue #51 decision 8): each scan is emitted only
// when its pattern list is non-empty. A provider with no scrub markers keeps
// the seeded-path guard; one with no seeded paths keeps the message scrub; one
// with neither yields a hook that enumerates but rejects nothing on content.
// The shebang, the lab marker, and the fail-closed enumeration are always
// present so the hook stays installable and recognizable in every case.
func TestPrePushHookScript_emptyPatternsDegrade(t *testing.T) {
	const msgLine = "git log -1 --format=%B"
	const pathLine = "git diff-tree -m --no-commit-id"

	scrubOnly := prePushHookScript(claudeGoldenMeta.ScrubPatterns, nil)
	if !strings.Contains(scrubOnly, msgLine) {
		t.Error("scrub-only hook dropped the message scan")
	}
	if strings.Contains(scrubOnly, pathLine) {
		t.Error("scrub-only hook still emitted the path scan for an empty pattern list")
	}

	pathOnly := prePushHookScript(nil, claudeGoldenMeta.SeededPathPatterns)
	if strings.Contains(pathOnly, msgLine) {
		t.Error("path-only hook still emitted the message scan for an empty pattern list")
	}
	if !strings.Contains(pathOnly, pathLine) {
		t.Error("path-only hook dropped the path scan")
	}

	none := prePushHookScript(nil, nil)
	if strings.Contains(none, msgLine) || strings.Contains(none, pathLine) {
		t.Error("empty-meta hook emitted a content scan")
	}
	for _, want := range []string{"#!/bin/sh", hookMarker, "git rev-list $range"} {
		if !strings.Contains(none, want) {
			t.Errorf("empty-meta hook missing the always-present line %q", want)
		}
	}
	// An empty-meta hook must still be a syntactically valid, installable
	// shell script — no dangling `grep` with no pattern.
	if strings.Contains(none, "grep") {
		t.Error("empty-meta hook left a grep with no pattern (broken shell)")
	}
}

// TestPrePushHookScript_parsesUnderSh proves every pattern combination renders
// a script /bin/sh actually parses. The empty-meta case is the load-bearing
// one: both scan blocks vanish, and an empty `for … do; done` body is a hard
// shell syntax error that would make git abort EVERY push with a raw parse
// error instead of the guard enumerating-but-rejecting-nothing. `sh -n` parses
// without executing (no git needed), so this is the cheap guard that a string
// check alone (the sibling test) cannot provide.
func TestPrePushHookScript_parsesUnderSh(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not on PATH: %v", err)
	}
	cases := map[string]string{
		"both":      prePushHookScript(claudeGoldenMeta.ScrubPatterns, claudeGoldenMeta.SeededPathPatterns),
		"scrubOnly": prePushHookScript(claudeGoldenMeta.ScrubPatterns, nil),
		"pathOnly":  prePushHookScript(nil, claudeGoldenMeta.SeededPathPatterns),
		"empty":     prePushHookScript(nil, nil),
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pre-push")
			if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(sh, "-n", path).CombinedOutput(); err != nil {
				t.Errorf("%s hook is not valid /bin/sh: %v\n%s\n--- script ---\n%s", name, err, out, script)
			}
		})
	}
}
