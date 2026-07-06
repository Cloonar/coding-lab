package seeder

// Incogni measure 7 tests against REAL git: the pre-push guard installed in
// a bare reference repo (the shape lab clones — remote-tracking refspec
// included) rejects poisoned pushes from a linked worktree naming the
// offending commit, lets clean pushes through, and disappears on toggle-off
// so the previously rejected push succeeds — the hook is lab's guard, not
// the user's policy.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// hookFixture is origin (bare remote) ← bare (lab's reference clone, hook
// host) ← wt (a linked run worktree pushing through the shared hook).
type hookFixture struct {
	t      *testing.T
	env    []string
	origin string
	bare   string
	wt     string
}

func newHookFixture(t *testing.T) *hookFixture {
	t.Helper()
	testutil.RequireTool(t, "git")
	root := t.TempDir()
	env := append(os.Environ(), testutil.HermeticGitEnv(root)...)
	f := &hookFixture{t: t, env: env}

	// The remote: a bare repo with one commit on main.
	work := filepath.Join(root, "work")
	git(t, env, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, env, "-C", work, "add", "f.txt")
	git(t, env, "-C", work, "commit", "-q", "-m", "init")
	f.origin = filepath.Join(root, "origin.git")
	git(t, env, "clone", "-q", "--bare", work, f.origin)

	// Lab's bare reference clone — the gitx.CloneBare shape: bare +
	// remote-tracking refspec, so refs/remotes/origin/* exists (the hook's
	// new-branch scan base).
	f.bare = filepath.Join(root, "bare.git")
	git(t, env, "clone", "-q", "--bare",
		"--config", "remote.origin.fetch=+refs/heads/*:refs/remotes/origin/*",
		f.origin, f.bare)
	git(t, env, "-C", f.bare, "fetch", "-q", "origin")

	// A run worktree, forked like gitx.AddWorktree.
	f.wt = filepath.Join(root, "wt")
	git(t, env, "-C", f.bare, "worktree", "add", "-q", "-b", "issue-7", f.wt, "origin/main")
	return f
}

// commit writes files and commits them in the worktree with message msg.
func (f *hookFixture) commit(msg string, files map[string]string) string {
	f.t.Helper()
	for name, content := range files {
		path := filepath.Join(f.wt, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	git(f.t, f.env, "-C", f.wt, "add", "-A")
	git(f.t, f.env, "-C", f.wt, "commit", "-q", "-m", msg)
	return git(f.t, f.env, "-C", f.wt, "rev-parse", "HEAD")
}

// push runs `git push origin issue-7` from the worktree and returns the
// combined output and whether it succeeded.
func (f *hookFixture) push() (string, bool) {
	f.t.Helper()
	cmd := exec.Command("git", "-C", f.wt, "push", "origin", "issue-7")
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

func (f *hookFixture) mustReject(sha, why string) {
	f.t.Helper()
	out, ok := f.push()
	if ok {
		f.t.Fatalf("push succeeded, want rejection (%s)\n%s", why, out)
	}
	if !strings.Contains(out, "lab incogni guard") {
		f.t.Errorf("rejection output missing the guard's message (%s):\n%s", why, out)
	}
	if !strings.Contains(out, sha) {
		f.t.Errorf("rejection does not name the offending commit %s (%s):\n%s", sha, why, out)
	}
}

// resetTo discards the worktree branch's unpushed commits back to sha.
func (f *hookFixture) resetTo(sha string) {
	f.t.Helper()
	git(f.t, f.env, "-C", f.wt, "reset", "-q", "--hard", sha)
}

func TestPrePushHook_endToEnd(t *testing.T) {
	f := newHookFixture(t)
	if err := InstallPrePushHook(f.bare); err != nil {
		t.Fatalf("InstallPrePushHook: %v", err)
	}
	if !PrePushHookInstalled(f.bare) {
		t.Fatal("hook not reported installed")
	}
	if fi, err := os.Stat(PrePushHookPath(f.bare)); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("hook not executable: %v %v", fi, err)
	}

	// Clean commit → NEW branch push (the <local> --not --remotes=origin
	// scan: the pre-existing origin/main history must not trip the guard).
	clean := f.commit("feat: implement the thing", map[string]string{"a.txt": "a\n"})
	if out, ok := f.push(); !ok {
		t.Fatalf("clean push rejected:\n%s", out)
	}

	// Co-Authored-By: Claude trailer → rejected naming the commit (now the
	// known-remote-sha <remote>..<local> scan).
	sha := f.commit("fix: tweak\n\nCo-Authored-By: Claude <noreply@anthropic.com>",
		map[string]string{"b.txt": "b\n"})
	f.mustReject(sha, "Co-Authored-By trailer")
	f.resetTo(clean)

	// Lower-case variant → still rejected (grep -i).
	sha = f.commit("fix: tweak\n\nco-authored-by: claude opus <noreply@anthropic.com>",
		map[string]string{"b.txt": "b2\n"})
	f.mustReject(sha, "lower-case co-authored-by")
	f.resetTo(clean)

	// Generated-with footer → rejected.
	sha = f.commit("fix: other\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)",
		map[string]string{"c.txt": "c\n"})
	f.mustReject(sha, "generated-with footer")
	f.resetTo(clean)

	// Email-only trailer (a future model rename drops the "Claude" display
	// name but keeps the anthropic.com email) → rejected. Regression: the
	// matcher must key on the stable email, not the display name.
	sha = f.commit("fix: renamed\n\nCo-Authored-By: Fable 5 <noreply@anthropic.com>",
		map[string]string{"c.txt": "c2\n"})
	f.mustReject(sha, "anthropic.com email without a Claude display name")
	f.resetTo(clean)

	// A commit touching .claude/ → rejected.
	sha = f.commit("chore: oops", map[string]string{".claude/settings.local.json": "{}\n"})
	f.mustReject(sha, "seeded .claude/ path")
	f.resetTo(clean)

	// A commit touching CLAUDE.local.md → rejected.
	sha = f.commit("chore: oops again", map[string]string{"CLAUDE.local.md": "seeded\n"})
	f.mustReject(sha, "seeded CLAUDE.local.md")
	f.resetTo(clean)

	// A clean commit on top of a poisoned one: the WHOLE outgoing range is
	// scanned, so the push is still rejected.
	sha = f.commit("fix: poisoned\n\nCo-Authored-By: Claude <noreply@anthropic.com>",
		map[string]string{"d.txt": "d\n"})
	f.commit("fix: clean on top", map[string]string{"e.txt": "e\n"})
	f.mustReject(sha, "poisoned commit below a clean tip")

	// Toggle-off: the hook is lab's guard, not the user's policy — after
	// removal the previously rejected push goes through.
	if err := RemovePrePushHook(f.bare); err != nil {
		t.Fatalf("RemovePrePushHook: %v", err)
	}
	if PrePushHookInstalled(f.bare) {
		t.Fatal("hook still reported installed after removal")
	}
	if out, ok := f.push(); !ok {
		t.Fatalf("push still rejected after hook removal:\n%s", out)
	}
}

func TestPrePushHook_installIdempotentAndRemoveMissing(t *testing.T) {
	bare := t.TempDir()
	for range 2 { // second install overwrites lab's own hook silently
		if err := InstallPrePushHook(bare); err != nil {
			t.Fatalf("InstallPrePushHook: %v", err)
		}
	}
	if err := RemovePrePushHook(bare); err != nil {
		t.Fatalf("RemovePrePushHook: %v", err)
	}
	if err := RemovePrePushHook(bare); err != nil { // already gone → no-op
		t.Fatalf("RemovePrePushHook on missing hook: %v", err)
	}
}

// A foreign pre-push hook is never overwritten (install errors) and never
// deleted (remove leaves it and reports nothing to do).
func TestPrePushHook_neverTouchesForeignHook(t *testing.T) {
	bare := t.TempDir()
	path := PrePushHookPath(bare)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "#!/bin/sh\nexit 0 # the user's own hook\n"
	if err := os.WriteFile(path, []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := InstallPrePushHook(bare); err == nil {
		t.Error("InstallPrePushHook overwrote a foreign hook; want an error")
	}
	if err := RemovePrePushHook(bare); err != nil {
		t.Errorf("RemovePrePushHook on a foreign hook: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != foreign {
		t.Errorf("foreign hook altered: %q, %v", got, err)
	}
}

// Regression (M7 review): a commit editing the repo's OWN tracked .claude/
// content (commands/agents/committed settings) is NOT lab-seeded, so the
// guard must let it push. Only lab's actual seeded paths are blocked.
func TestPrePushHook_allowsTrackedNonSeededClaudeFiles(t *testing.T) {
	f := newHookFixture(t)
	if err := InstallPrePushHook(f.bare); err != nil {
		t.Fatalf("InstallPrePushHook: %v", err)
	}
	// A repo tracking its own .claude/commands/deploy.md — editing it pushes.
	if out, ok := func() (string, bool) {
		f.commit("chore: update my own claude command", map[string]string{
			".claude/commands/deploy.md": "run the deploy\n",
		})
		return f.push()
	}(); !ok {
		t.Fatalf("push of a tracked non-seeded .claude/ file was rejected:\n%s", out)
	}
	// But a lab-seeded path IS still blocked.
	sha := f.commit("chore: leak", map[string]string{".claude/settings.local.json": "{}\n"})
	f.mustReject(sha, "lab-seeded settings.local.json")
}

// Regression (M7 review, CRITICAL): the guard fails CLOSED. A merge commit
// that itself introduces a seeded path is caught (diff-tree -m), and a range
// git cannot enumerate refuses the push rather than waving it through.
func TestPrePushHook_mergeCommitIntroducingSeededPathRejected(t *testing.T) {
	f := newHookFixture(t)
	if err := InstallPrePushHook(f.bare); err != nil {
		t.Fatalf("InstallPrePushHook: %v", err)
	}
	// Build a side branch, then a merge commit that adds the seeded file
	// during the merge itself (empty default diff-tree without -m).
	base := git(f.t, f.env, "-C", f.wt, "rev-parse", "HEAD")
	f.commit("feat: side work", map[string]string{"side.txt": "s\n"})
	side := git(f.t, f.env, "-C", f.wt, "rev-parse", "HEAD")
	git(f.t, f.env, "-C", f.wt, "reset", "-q", "--hard", base)
	f.commit("feat: main work", map[string]string{"main.txt": "m\n"})
	// --no-ff --no-commit leaves the merge staged; the seeded file is added
	// during the merge itself, so it appears only in the merge commit (its
	// default diff-tree is empty without -m).
	git(f.t, f.env, "-C", f.wt, "merge", "-q", "--no-ff", "--no-commit", side)
	claudeDir := filepath.Join(f.wt, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.local.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(f.t, f.env, "-C", f.wt, "add", "-A")
	git(f.t, f.env, "-C", f.wt, "commit", "-q", "-m", "merge with a seeded file snuck in")
	sha := git(f.t, f.env, "-C", f.wt, "rev-parse", "HEAD")
	f.mustReject(sha, "merge commit introducing a seeded path")
}
