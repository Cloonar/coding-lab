package seeder

// Incogni measure 7 (brief D15 §9): a pre-push hook installed in the BARE
// reference repo of an incogni repo. Hooks live in the common git dir, so
// every worktree of the repo shares the one hook — an agent pushing from
// its run worktree runs it. The hook scans the OUTGOING commits of each
// pushed ref (remote sha known → <remote>..<local>; zero/new branch →
// <local> --not --remotes=origin, which works because lab's bare clones
// carry the standard remote-tracking refspec, gitx.CloneBare) for AI
// attribution in commit messages and for lab-seeded paths in changed
// files, and rejects the push naming the offending commit.
//
// The hook is LAB's guard, not the user's policy: reposvc installs it when
// a repo's incogni flag turns on (repo add via clone completion, PATCH
// toggle-on) and removes it on toggle-off — after which a previously
// rejected push goes through.

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
const hookMarker = "# lab incogni guard"

// PrePushHookPath is the hook location inside a bare reference repo.
func PrePushHookPath(bareDir string) string {
	return filepath.Join(bareDir, "hooks", "pre-push")
}

// InstallPrePushHook writes the incogni guard as bareDir's pre-push hook
// (0755, atomic tmp+rename). scrubPatterns are the case-insensitive greps run
// against each outgoing commit MESSAGE (attribution markers) and
// seededPathPatterns the BREs run against each commit's CHANGED PATHS — both
// provider-declared (issue #51 decision 8: SeedMeta.ScrubPatterns /
// .SeededPathPatterns), so the guard scrubs whatever the repo's agent CLI
// stamps. Either list empty omits its scan (a provider with no attribution
// markers still guards seeded paths, and vice versa). Idempotent over lab's
// own hook; a pre-existing FOREIGN pre-push hook is never overwritten — that
// is an error the caller surfaces (the operator resolves the conflict by hand).
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

// RemovePrePushHook deletes lab's incogni guard from bareDir. A missing
// hook is a no-op, and a foreign hook (no marker) is left untouched — lab
// only ever removes what it installed.
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

// PrePushHookInstalled reports whether bareDir carries lab's incogni guard.
func PrePushHookInstalled(bareDir string) bool {
	data, err := os.ReadFile(PrePushHookPath(bareDir))
	return err == nil && strings.Contains(string(data), hookMarker)
}

// prePushHookScript renders the POSIX-sh hook from the provider's patterns
// (issue #51 decision 8). scrubPatterns match each outgoing commit MESSAGE
// (case-insensitive), seededPathPatterns the CHANGED PATHS — the paths lab
// actually seeds, NOT the broader ExcludeEntries, which would reject a repo's
// own tracked provider config. Each scan is emitted only when its pattern list
// is non-empty: a provider that declares no scrub markers keeps the path guard
// (and vice versa); a provider that declares neither yields a hook that
// enumerates but rejects nothing on content. The guard FAILS CLOSED
// regardless: any error enumerating a ref's outgoing commits rejects the push
// rather than letting it through, because measure 7 is
// leak-proof-by-construction (D15) — an unscannable push is a refused push.
//
// BYTE-IDENTITY (issue #51): with claude-code's patterns the two scans are
// both present and this renders the exact script it did before the seam split
// — pinned by TestPrePushHookScript_claudeGolden.
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
	// The per-commit loop body is the two optional scan blocks. When a provider
	// declares NEITHER pattern set (both blocks empty — e.g. the zero SeedMeta a
	// degraded no-registry boot passes, or a future provider with no attribution
	// markers and no seeded paths), the body must still be a valid `for` body: an
	// empty `do … done` is a hard /bin/sh syntax error, so git would abort EVERY
	// push with a raw parse error instead of the guard enumerating-but-rejecting-
	// nothing. A `:` no-op keeps the script parseable and the loop a clean pass.
	loopBody := msgBlock + pathBlock
	if loopBody == "" {
		loopBody = "        :\n" // no scan patterns declared — enumerate, reject nothing
	}
	return `#!/bin/sh
` + hookMarker + ` — installed by lab on incogni repos; do not edit.
# Rejects pushes whose outgoing commits carry AI attribution in the commit
# message or touch lab-seeded files. Removed when incogni is toggled off.
# Fails CLOSED: an unscannable ref is refused, never waved through.

fail=0
while read -r local_ref local_sha remote_ref remote_sha; do
    # Deleting a remote ref (all-zero local sha, any hash length) pushes no
    # commits. Match zeros by pattern so SHA-1 (40) and SHA-256 (64) both work.
    case "$local_sha" in *[!0]*) : ;; *) continue ;; esac
    case "$remote_sha" in
        *[!0]*) range="$remote_sha..$local_sha" ;;      # known remote tip
        *)      range="$local_sha --not --remotes=origin" ;; # new branch
    esac
    # Enumerate outgoing commits; a rev-list failure (unknown remote sha after
    # a force-push, wrong-length sentinel, corrupt range) fails the push.
    commits=$(git rev-list $range) || {
        echo "lab incogni guard: cannot enumerate outgoing commits for $remote_ref ($range); refusing push" >&2
        exit 1
    }
    for commit in $commits; do
` + loopBody + `    done
done
exit $fail
`
}
