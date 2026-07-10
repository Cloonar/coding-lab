package seeder

// Pre-push guard tests against REAL git: the hook installed in a bare
// reference repo (the shape lab clones — remote-tracking refspec included)
// runs on pushes from a linked worktree. Incogni measure 7: poisoned pushes
// are rejected naming the offending commit, clean pushes go through, and the
// hook disappears on toggle-off so the previously rejected push succeeds —
// the hook is lab's guard, not the user's policy. Secret leak scan (issue
// #106): under a LAB_TOKEN the hook's `labctl secret scan <range>` call is
// mandatory and fails closed; without one it is skipped with a warning.

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// labFilteredEnviron is os.Environ() minus LAB_TOKEN and LAB_URL. The test
// process itself runs inside a lab session whose environment carries a REAL
// token; a push inheriting it would take the hook's mandatory-scan path and
// run the real labctl against the real lab server — nondeterministic and
// wrong. Fixture pushes are therefore token-less by default; tests opt back
// in per push via pushEnv.
func labFilteredEnviron() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "LAB_TOKEN=") || strings.HasPrefix(kv, "LAB_URL=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
}

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
	env := append(labFilteredEnviron(), testutil.HermeticGitEnv(root)...)
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

// push runs `git push origin issue-7` from the worktree with the fixture's
// token-less environment and returns the combined output and whether it
// succeeded.
func (f *hookFixture) push() (string, bool) {
	return f.pushEnv()
}

// pushEnv is push with extra environment entries appended for this one push
// (os/exec dedupes on key, last wins — so "PATH=…" or "LAB_TOKEN=…" entries
// override the fixture's). This is the opt-in for the hook's secret-scan
// token path and for putting a stub labctl first on the child's PATH.
func (f *hookFixture) pushEnv(extra ...string) (string, bool) {
	f.t.Helper()
	cmd := exec.Command("git", "-C", f.wt, "push", "origin", "issue-7")
	cmd.Env = append(slices.Clone(f.env), extra...)
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
	if err := InstallPrePushHook(f.bare, claudeGoldenMeta.ScrubPatterns, claudeGoldenMeta.SeededPathPatterns); err != nil {
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
		if err := InstallPrePushHook(bare, claudeGoldenMeta.ScrubPatterns, claudeGoldenMeta.SeededPathPatterns); err != nil {
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

	if err := InstallPrePushHook(bare, claudeGoldenMeta.ScrubPatterns, claudeGoldenMeta.SeededPathPatterns); err == nil {
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
	if err := InstallPrePushHook(f.bare, claudeGoldenMeta.ScrubPatterns, claudeGoldenMeta.SeededPathPatterns); err != nil {
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
	if err := InstallPrePushHook(f.bare, claudeGoldenMeta.ScrubPatterns, claudeGoldenMeta.SeededPathPatterns); err != nil {
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

// stubLabctl writes an executable fake labctl into its own fresh dir and
// returns that dir plus the file the stub appends its argv to (one argument
// per line, invocations concatenated). exit is the stub's exit code: 0
// simulates a clean scan, nonzero a leak / server failure.
func stubLabctl(t *testing.T, exit int) (dir, record string) {
	t.Helper()
	dir = t.TempDir()
	record = filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> '" + record + "'\nexit " + strconv.Itoa(exit) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "labctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, record
}

// pathWith returns a PATH env entry resolving dir first, with the ambient
// PATH after it so sh and git stay resolvable for the push child.
func pathWith(dir string) string {
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// readArgv returns the stub's recorded argv, one argument per line.
func readArgv(t *testing.T, record string) []string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("stub labctl was never invoked: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// tokenEnv is the opt-in for the hook's mandatory-scan path: a fake run
// token and lab URL (never the session's real ones — labFilteredEnviron
// stripped those).
var tokenEnv = []string{"LAB_TOKEN=test-token", "LAB_URL=http://lab.invalid"}

// Under a run token a clean scan (labctl exit 0) lets the push through, and
// the hook hands labctl the outgoing range word-split, on both shapes: the
// first push of a new branch passes the multi-word
// `<local> --not --remotes=origin` range, a follow-up push the
// `<remote>..<local>` form.
func TestPrePushHook_secretScanCleanPushSucceeds(t *testing.T) {
	f := newHookFixture(t)
	if err := InstallPrePushHook(f.bare, nil, nil); err != nil {
		t.Fatalf("InstallPrePushHook: %v", err)
	}
	dir, record := stubLabctl(t, 0)
	env := append([]string{pathWith(dir)}, tokenEnv...)

	// New-branch push: remote sha is all zeros, range is multi-word.
	sha := f.commit("feat: one", map[string]string{"a.txt": "a\n"})
	if out, ok := f.pushEnv(env...); !ok {
		t.Fatalf("clean new-branch push refused:\n%s", out)
	}
	argv := readArgv(t, record)
	want := []string{"secret", "scan", sha, "--not", "--remotes=origin"}
	if !slices.Equal(argv, want) {
		t.Errorf("new-branch stub argv = %q, want %q", argv, want)
	}

	// Known-remote-tip push: the two-dot range, a single word.
	sha2 := f.commit("feat: two", map[string]string{"b.txt": "b\n"})
	if out, ok := f.pushEnv(env...); !ok {
		t.Fatalf("clean follow-up push refused:\n%s", out)
	}
	argv = readArgv(t, record)
	want = append(want, "secret", "scan", sha+".."+sha2)
	if !slices.Equal(argv, want) {
		t.Errorf("follow-up stub argv = %q, want %q", argv, want)
	}
}

// Under a run token the scan is mandatory: labctl exiting nonzero (a leak
// finding — labctl prints the details itself) refuses the push with the
// hook's one-line refusal, after invoking `labctl secret scan <range>`.
func TestPrePushHook_secretScanLeakRefusesPush(t *testing.T) {
	f := newHookFixture(t)
	if err := InstallPrePushHook(f.bare, nil, nil); err != nil {
		t.Fatalf("InstallPrePushHook: %v", err)
	}
	f.commit("feat: carries a secret", map[string]string{"a.txt": "a\n"})
	dir, record := stubLabctl(t, 1)
	out, ok := f.pushEnv(append([]string{pathWith(dir)}, tokenEnv...)...)
	if ok {
		t.Fatalf("push succeeded, want refusal on labctl exit 1:\n%s", out)
	}
	if !strings.Contains(out, "lab secret guard: refusing push") {
		t.Errorf("refusal message missing:\n%s", out)
	}
	argv := readArgv(t, record)
	if len(argv) < 3 || argv[0] != "secret" || argv[1] != "scan" {
		t.Errorf("stub argv = %q, want secret scan <range...>", argv)
	}
}

// Without a LAB_TOKEN the scan is skipped with a loud warning and the push
// proceeds. The stub (which would refuse) IS on PATH, proving the token
// check — not a missing binary — does the skipping, and it is never invoked.
func TestPrePushHook_secretScanSkippedWithoutToken(t *testing.T) {
	f := newHookFixture(t)
	if err := InstallPrePushHook(f.bare, nil, nil); err != nil {
		t.Fatalf("InstallPrePushHook: %v", err)
	}
	f.commit("feat: human work", map[string]string{"a.txt": "a\n"})
	dir, record := stubLabctl(t, 1) // would refuse the push if it ran
	out, ok := f.pushEnv(pathWith(dir))
	if !ok {
		t.Fatalf("token-less push refused:\n%s", out)
	}
	if !strings.Contains(out, "lab secret guard: no LAB_TOKEN in environment; skipping secret leak scan") {
		t.Errorf("skip warning missing:\n%s", out)
	}
	if data, err := os.ReadFile(record); err == nil && len(data) > 0 {
		t.Errorf("stub labctl was invoked on a token-less push: %q", data)
	}
}

// shPath resolves a POSIX sh for tests: PATH first, then the absolute
// /bin/sh the hook's shebang uses anyway — the lab sandbox's PATH carries
// no sh, yet git runs the hooks fine through the shebang.
func shPath(t *testing.T) string {
	t.Helper()
	if sh, err := exec.LookPath("sh"); err == nil {
		return sh
	}
	const fallback = "/bin/sh"
	if fi, err := os.Stat(fallback); err == nil && fi.Mode()&0o111 != 0 {
		return fallback
	}
	t.Skipf("no sh on PATH and no %s", fallback)
	return ""
}

// pathWithoutLabctl builds a PATH where sh and git resolve but labctl does
// not: a symlink dir carrying just those two tools, then the ambient PATH
// entries that do NOT contain a labctl binary (the lab session's own PATH
// carries the real one).
func pathWithoutLabctl(t *testing.T) string {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	bin := t.TempDir()
	for tool, src := range map[string]string{"git": gitBin, "sh": shPath(t)} {
		if err := os.Symlink(src, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	keep := []string{bin}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, "labctl")); err == nil {
			continue
		}
		keep = append(keep, dir)
	}
	return strings.Join(keep, string(os.PathListSeparator))
}

// Under a run token with NO labctl on the push child's PATH the scan cannot
// run: command-not-found is nonzero, so the hook fails closed and refuses —
// a run whose environment lost labctl must not push unscanned.
func TestPrePushHook_secretScanFailsClosedWithoutLabctl(t *testing.T) {
	f := newHookFixture(t)
	if err := InstallPrePushHook(f.bare, nil, nil); err != nil {
		t.Fatalf("InstallPrePushHook: %v", err)
	}
	f.commit("feat: unscannable", map[string]string{"a.txt": "a\n"})
	out, ok := f.pushEnv(append([]string{"PATH=" + pathWithoutLabctl(t)}, tokenEnv...)...)
	if ok {
		t.Fatalf("push succeeded with labctl missing from PATH, want fail-closed refusal:\n%s", out)
	}
	if !strings.Contains(out, "lab secret guard: refusing push") {
		t.Errorf("refusal message missing:\n%s", out)
	}
}

// The two guards coexist: on an incogni repo a token-less push of an
// attributed commit emits BOTH the secret-scan skip warning and the incogni
// rejection naming the commit, is refused by the incogni scan alone, and
// never touches labctl.
func TestPrePushHook_incogniStillBlocksAlongsideSecretSkip(t *testing.T) {
	f := newHookFixture(t)
	if err := InstallPrePushHook(f.bare, claudeGoldenMeta.ScrubPatterns, claudeGoldenMeta.SeededPathPatterns); err != nil {
		t.Fatalf("InstallPrePushHook: %v", err)
	}
	sha := f.commit("fix: tweak\n\nCo-Authored-By: Claude <noreply@anthropic.com>",
		map[string]string{"a.txt": "a\n"})
	dir, record := stubLabctl(t, 0)
	out, ok := f.pushEnv(pathWith(dir)) // no token
	if ok {
		t.Fatalf("attributed push succeeded, want incogni rejection:\n%s", out)
	}
	if !strings.Contains(out, "lab secret guard: no LAB_TOKEN in environment; skipping secret leak scan") {
		t.Errorf("skip warning missing:\n%s", out)
	}
	if !strings.Contains(out, "lab incogni guard") || !strings.Contains(out, sha) {
		t.Errorf("incogni rejection naming %s missing:\n%s", sha, out)
	}
	if data, err := os.ReadFile(record); err == nil && len(data) > 0 {
		t.Errorf("stub labctl was invoked on a token-less push: %q", data)
	}
}
