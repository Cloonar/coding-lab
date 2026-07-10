package seeder

// Lab's pre-push hook, installed in the BARE reference repo of a lab-managed
// repo. Hooks live in the common git dir, so every worktree of the repo
// shares the one hook — an agent pushing from its run worktree runs it. For
// each pushed ref the hook computes the OUTGOING range (remote sha known →
// <remote>..<local>; zero/new branch → <local> --not --remotes=origin, which
// works because lab's bare clones carry the standard remote-tracking
// refspec, gitx.CloneBare) and applies two guards:
//
//   - Secret leak scan (issue #106), rendered UNCONDITIONALLY: the hook
//     shells out to `labctl secret scan <range>`, which submits the outgoing
//     diff to the lab server for matching against the repo's secret values —
//     the values never reach the hook. With LAB_TOKEN in the pusher's
//     environment the scan is mandatory and any labctl failure (leak found,
//     server down, labctl missing) refuses the push: fail closed. Without a
//     token (a human pushing by hand) the scan is skipped with a loud
//     warning and the push proceeds — the human needs neither a token nor
//     labctl on PATH.
//
//   - Incogni measure 7 (brief D15 §9), rendered only when the repo has
//     incogni patterns: scans each outgoing commit for AI attribution in the
//     commit message and lab-seeded paths in changed files, rejecting the
//     push naming the offending commit. These checks fail CLOSED — an
//     un-enumerable range refuses the push.
//
// The hook is LAB's guard, not the user's policy: reposvc installs it on
// every ready repo and re-renders it when the incogni flag changes — the
// hook itself never goes away while lab manages the repo. RemovePrePushHook
// has no production caller today; it stays as the marker-guarded inverse of
// Install, pinning the never-delete-a-foreign-hook rule under test.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// hookMarker identifies lab's own pre-push hook. Install refuses to
// overwrite a hook without it; Remove only deletes a hook carrying it.
// The phrase is a recognition token pinned BYTE-IDENTICAL forever: deployed
// hooks are recognized by strings.Contains with this exact string, so it
// stays "incogni" even though the hook now guards secrets on every repo —
// changing it would strand every installed hook as "foreign".
const hookMarker = "# lab incogni guard"

// PrePushHookPath is the hook location inside a bare reference repo.
func PrePushHookPath(bareDir string) string {
	return filepath.Join(bareDir, "hooks", "pre-push")
}

// InstallPrePushHook writes lab's guard as bareDir's pre-push hook (0755,
// atomic tmp+rename). Every rendered hook carries the secret leak scan;
// scrubPatterns are the case-insensitive greps run against each outgoing
// commit MESSAGE (attribution markers) and seededPathPatterns the BREs run
// against each commit's CHANGED PATHS — both provider-declared (issue #51
// decision 8: SeedMeta.ScrubPatterns / .SeededPathPatterns), so the guard
// scrubs whatever the repo's agent CLI stamps. Either list empty omits its
// scan (a provider with no attribution markers still guards seeded paths,
// and vice versa); a non-incogni repo passes nil, nil and gets the
// secret-scan-only hook. Idempotent over lab's own hook; a pre-existing
// FOREIGN pre-push hook is never overwritten — that is an error the caller
// surfaces (the operator resolves the conflict by hand).
func InstallPrePushHook(bareDir string, scrubPatterns, seededPathPatterns []string) error {
	path := PrePushHookPath(bareDir)
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		if !strings.Contains(string(existing), hookMarker) {
			return fmt.Errorf("refusing to overwrite existing pre-push hook %s: not lab's incogni guard", path)
		}
	case errors.Is(err, fs.ErrNotExist):
		// no hook yet
	default:
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "pre-push.tmp.*")
	if err != nil {
		return fmt.Errorf("tmpfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.WriteString(prePushHookScript(scrubPatterns, seededPathPatterns)); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// RemovePrePushHook deletes lab's guard from bareDir. A missing hook is a
// no-op, and a foreign hook (no marker) is left untouched — lab only ever
// removes what it installed.
func RemovePrePushHook(bareDir string) error {
	path := PrePushHookPath(bareDir)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !strings.Contains(string(data), hookMarker) {
		return nil // not lab's hook — never delete a user's
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// PrePushHookInstalled reports whether bareDir carries lab's pre-push guard.
func PrePushHookInstalled(bareDir string) bool {
	data, err := os.ReadFile(PrePushHookPath(bareDir))
	return err == nil && strings.Contains(string(data), hookMarker)
}

// prePushHookScript renders the POSIX-sh hook. The secret leak scan is
// UNCONDITIONAL — every rendering shells out to `labctl secret scan <range>`,
// mandatory (fail closed) under a LAB_TOKEN and skipped with a warning
// without one. The incogni scans come from the provider's patterns (issue
// #51 decision 8): scrubPatterns match each outgoing commit MESSAGE
// (case-insensitive), seededPathPatterns the CHANGED PATHS — the paths lab
// actually seeds, NOT the broader ExcludeEntries, which would reject a
// repo's own tracked provider config. Each incogni scan is emitted only when
// its pattern list is non-empty, and with BOTH lists empty the commit
// enumeration and loop are omitted entirely — rev-list's fail-closed
// refusal is an incogni-guard property, so a token-less human on a plain
// repo is never blocked by an un-enumerable range (labctl does its own
// fail-closed enumeration when a token is present). When rendered, the
// incogni guard FAILS CLOSED: any error enumerating a ref's outgoing commits
// rejects the push, because measure 7 is leak-proof-by-construction (D15) —
// an unscannable push is a refused push.
//
// With claude-code's patterns the exact rendering is pinned byte-for-byte by
// TestPrePushHookScript_claudeGolden.
func prePushHookScript(scrubPatterns, seededPathPatterns []string) string {
	msgBlock := ""
	if len(scrubPatterns) > 0 {
		var msg strings.Builder
		for _, p := range scrubPatterns {
			msg.WriteString(" \\\n            -e '" + p + "'")
		}
		msgBlock = `        if git log -1 --format=%B "$commit" | grep -i -q` + msg.String() + `; then
            echo "lab incogni guard: commit $commit ($remote_ref) carries AI attribution in its message; rewrite it before pushing" >&2
            fail=1
        fi
`
	}
	pathBlock := ""
	if len(seededPathPatterns) > 0 {
		var path strings.Builder
		for _, p := range seededPathPatterns {
			path.WriteString(" \\\n            -e '" + p + "'")
		}
		pathBlock = `        # -m diffs a merge against each parent so files the merge itself
        # introduces (evil merges, conflict resolutions) are not invisible.
        if git diff-tree -m --no-commit-id --name-only -r "$commit" | grep -q` + path.String() + `; then
            echo "lab incogni guard: commit $commit ($remote_ref) touches lab-seeded files; drop them before pushing" >&2
            fail=1
        fi
`
	}
	// The incogni enumeration and per-commit loop render only when at least
	// one scan block exists. When a provider declares NEITHER pattern set
	// (e.g. the zero SeedMeta a degraded no-registry boot passes, or a plain
	// non-incogni repo's nil, nil) the whole section is omitted — no empty
	// `for` body (a hard /bin/sh syntax error), no `:` no-op, and no
	// rev-list, so a token-less human push on a plain repo can never be
	// refused by a failed enumeration that guards nothing.
	incogniSection := ""
	if loopBody := msgBlock + pathBlock; loopBody != "" {
		incogniSection = `    # Enumerate outgoing commits; a rev-list failure (unknown remote sha after
    # a force-push, wrong-length sentinel, corrupt range) fails the push.
    commits=$(git rev-list $range) || {
        echo "lab incogni guard: cannot enumerate outgoing commits for $remote_ref ($range); refusing push" >&2
        exit 1
    }
    for commit in $commits; do
` + loopBody + `    done
`
	}
	return `#!/bin/sh
` + hookMarker + ` — lab's pre-push guard; do not edit. ("incogni" is the
# marker's historical name — this hook runs on every lab-managed repo.)
# Scans outgoing pushes for the repo's secret values via labctl: mandatory
# under a run token (LAB_TOKEN), skipped with a loud warning without one.
# On incogni repos it also rejects AI attribution in commit messages and
# lab-seeded paths in changed files; those checks fail CLOSED.

fail=0
while read -r local_ref local_sha remote_ref remote_sha; do
    # Deleting a remote ref (all-zero local sha, any hash length) pushes no
    # commits. Match zeros by pattern so SHA-1 (40) and SHA-256 (64) both work.
    case "$local_sha" in *[!0]*) : ;; *) continue ;; esac
    case "$remote_sha" in
        *[!0]*) range="$remote_sha..$local_sha" ;;      # known remote tip
        *)      range="$local_sha --not --remotes=origin" ;; # new branch
    esac
    # Secret leak scan — labctl submits the outgoing diff to the lab server,
    # which matches it against the repo's secret values; values never reach
    # this hook. With a run token the scan is mandatory: any failure refuses
    # the push. Without one (a human push) it is skipped with a warning.
    if [ -n "$LAB_TOKEN" ]; then
        if ! labctl secret scan $range; then
            echo "lab secret guard: refusing push of $remote_ref" >&2
            fail=1
        fi
    else
        echo "lab secret guard: no LAB_TOKEN in environment; skipping secret leak scan for $remote_ref" >&2
    fi
` + incogniSection + `done
exit $fail
`
}
