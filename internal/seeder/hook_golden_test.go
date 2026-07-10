package seeder

// Byte-identity proof for lab's pre-push hook. testdata/pre-push-claude.golden
// pins the exact script rendered with claude-code's golden patterns (issue
// #51 decision 8 established the pin; issue #106 refreshed it for the
// unconditional secret leak scan). A refactor that reshapes the script —
// even by a space — fails here.

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

// Empty-pattern degradation (issue #51 decision 8, reshaped by issue #106):
// each incogni scan is emitted only when its pattern list is non-empty, and
// the fail-closed rev-list enumeration renders only when at least one scan
// exists — it guards the incogni loop, nothing else. The secret leak scan,
// the shebang, and the lab marker are UNCONDITIONAL: a provider with neither
// pattern list (a plain repo's nil, nil) yields a secret-scan-only hook with
// no enumeration and no per-commit loop at all, so a token-less human push
// can never be refused by a rev-list failure that guards nothing.
func TestPrePushHookScript_emptyPatternsDegrade(t *testing.T) {
	const msgLine = "git log -1 --format=%B"
	const pathLine = "git diff-tree -m --no-commit-id"
	const revList = "git rev-list $range"

	// The secret leak scan appears in EVERY rendering, patterns or not.
	secretLines := []string{
		`if [ -n "$LAB_TOKEN" ]; then`,
		"labctl secret scan $range",
		"lab secret guard: refusing push of $remote_ref",
		"no LAB_TOKEN in environment; skipping secret leak scan",
	}

	scrubOnly := prePushHookScript(claudeGoldenMeta.ScrubPatterns, nil)
	if !strings.Contains(scrubOnly, msgLine) {
		t.Error("scrub-only hook dropped the message scan")
	}
	if strings.Contains(scrubOnly, pathLine) {
		t.Error("scrub-only hook still emitted the path scan for an empty pattern list")
	}
	if !strings.Contains(scrubOnly, revList) {
		t.Error("scrub-only hook dropped the fail-closed enumeration its scan needs")
	}

	pathOnly := prePushHookScript(nil, claudeGoldenMeta.SeededPathPatterns)
	if strings.Contains(pathOnly, msgLine) {
		t.Error("path-only hook still emitted the message scan for an empty pattern list")
	}
	if !strings.Contains(pathOnly, pathLine) {
		t.Error("path-only hook dropped the path scan")
	}
	if !strings.Contains(pathOnly, revList) {
		t.Error("path-only hook dropped the fail-closed enumeration its scan needs")
	}

	both := prePushHookScript(claudeGoldenMeta.ScrubPatterns, claudeGoldenMeta.SeededPathPatterns)
	if !strings.Contains(both, revList) {
		t.Error("both-patterns hook dropped the fail-closed enumeration")
	}

	none := prePushHookScript(nil, nil)
	if strings.Contains(none, msgLine) || strings.Contains(none, pathLine) {
		t.Error("empty-meta hook emitted a content scan")
	}
	for _, want := range append([]string{"#!/bin/sh", hookMarker}, secretLines...) {
		if !strings.Contains(none, want) {
			t.Errorf("empty-meta hook missing the always-present line %q", want)
		}
	}
	// With no incogni scans there is nothing for the enumeration to feed:
	// no rev-list (its fail-closed refusal is an incogni property), no
	// per-commit loop, and no dangling `grep` with no pattern.
	for _, gone := range []string{"git rev-list", "for commit", "grep"} {
		if strings.Contains(none, gone) {
			t.Errorf("empty-meta hook still contains %q; the incogni section must be omitted entirely", gone)
		}
	}

	for name, script := range map[string]string{"scrubOnly": scrubOnly, "pathOnly": pathOnly, "both": both, "none": none} {
		for _, want := range secretLines {
			if !strings.Contains(script, want) {
				t.Errorf("%s hook missing the unconditional secret-scan line %q", name, want)
			}
		}
	}
}

// TestPrePushHookScript_parsesUnderSh proves every pattern combination renders
// a script /bin/sh actually parses. The empty-meta case is the load-bearing
// one: the incogni scans, the enumeration, and the whole `for` loop vanish
// (an empty `for … do; done` body would be a hard shell syntax error, so the
// section must disappear cleanly, not hollow out), and a parse error would
// make git abort EVERY push raw. `sh -n` parses without executing (no git
// needed), so this is the cheap guard that a string check alone (the sibling
// test) cannot provide.
func TestPrePushHookScript_parsesUnderSh(t *testing.T) {
	sh := shPath(t)
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
