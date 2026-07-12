package crmerge

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// keyedMutex is the per-CR merge/close serializer (ADR-0011: a close landing
// inside a merge's git window strands origin merged while the row reads
// closed-unmerged). Pin its two properties: same key excludes, different keys
// do not.
func TestKeyedMutex(t *testing.T) {
	var km keyedMutex
	unlockA := km.lock("a")
	// Different key: never blocks.
	done := make(chan struct{})
	go func() {
		unlock := km.lock("b")
		unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("lock(b) blocked behind lock(a)")
	}
	// Same key: blocks until release.
	acquired := make(chan struct{})
	go func() {
		unlock := km.lock("a")
		unlock()
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("second lock(a) acquired while held")
	case <-time.After(50 * time.Millisecond):
	}
	unlockA()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second lock(a) never acquired after release")
	}
}

// mergeGitCmd runs one git command with a hermetic env — the package-local
// equivalent of httpapi's repoGitCmd/reconcile's recGitCmd test helpers (test
// fixtures are not shared across packages).
func mergeGitCmd(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), testutil.HermeticGitEnv(home)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestMergePublishesRunChanged pins issue #149's new publish: a successful
// Merge fires run.changed alongside the pinned cr.changed, on the same
// repo-scoped {type, repoID} envelope, since every run forked off the base
// branch just became more behind and the SPA's commits_behind badges need
// the same refetch signal a run mutation gives them. Builds the same
// production-shaped git topology as httpapi's crs_test.go (bare origin +
// worktree-forked head branch, lab bare reference clone) but self-contained
// in this package rather than routed through the operator HTTP surface.
func TestMergePublishesRunChanged(t *testing.T) {
	testutil.RequireTool(t, "git")
	home := t.TempDir()

	// The bare origin (the repo's real remote) seeded with one commit via a
	// throwaway work clone — a bare repo has no working tree to commit into
	// directly.
	origin := filepath.Join(t.TempDir(), "origin.git")
	mergeGitCmd(t, home, "", "init", "-q", "--bare", "-b", "main", origin)
	work := filepath.Join(t.TempDir(), "work")
	mergeGitCmd(t, home, "", "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write base.txt: %v", err)
	}
	mergeGitCmd(t, home, work, "add", ".")
	mergeGitCmd(t, home, work, "commit", "-q", "-m", "base")
	mergeGitCmd(t, home, work, "remote", "add", "origin", origin)
	mergeGitCmd(t, home, work, "push", "-q", "origin", "main")

	eng := gitx.New("git")
	env := testutil.HermeticGitEnv(home)
	reposDir := t.TempDir()
	st := testutil.TempStore(t)

	repo, err := st.CreateRepo(context.Background(), store.Repo{
		ID: ids.NewID("repo"), Name: "proj", RemoteURL: origin,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	bare := filepath.Join(reposDir, repo.ID+".git")
	if err := eng.CloneBare(context.Background(), origin, bare, env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	if err := st.SetSetting(context.Background(), store.SettingGitAuthorName, "Test Author"); err != nil {
		t.Fatalf("SetSetting name: %v", err)
	}
	if err := st.SetSetting(context.Background(), store.SettingGitAuthorEmail, "test@example.invalid"); err != nil {
		t.Fatalf("SetSetting email: %v", err)
	}

	// The CR head branch, created the way a run creates one: worktree forked
	// from origin/main in the bare clone, mutate + commit, worktree removed
	// with the branch kept.
	wt := filepath.Join(t.TempDir(), "afk-1")
	if err := eng.AddWorktree(context.Background(), bare, wt, "afk/1", "main", env); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatalf("write feature.txt: %v", err)
	}
	mergeGitCmd(t, home, wt, "add", "-A")
	mergeGitCmd(t, home, wt, "commit", "-q", "-m", "cr work")
	if err := eng.RemoveWorktree(context.Background(), bare, wt, env); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}

	cr, err := st.CreateCR(context.Background(), repo.ID, "feat: thing", "", "afk/1", "main", nil, time.Now())
	if err != nil {
		t.Fatalf("CreateCR: %v", err)
	}

	bus := events.NewBus()
	ch, cancel := bus.Subscribe(context.Background())
	defer cancel()

	svc := New(Config{
		Store: st, Git: eng, Bus: bus, ReposDir: reposDir, GitEnv: env,
		Now: time.Now, Logger: logx.New(io.Discard),
	})

	if _, err := svc.Merge(context.Background(), repo.ID, cr.Number); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	seen := map[string]bool{}
	deadline := time.Now().Add(2 * time.Second)
	for (!seen[EventCRChanged] || !seen[EventRunChanged]) && time.Now().Before(deadline) {
		select {
		case e := <-ch:
			seen[e.Type] = true
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !seen[EventCRChanged] {
		t.Error("no cr.changed event on successful merge")
	}
	if !seen[EventRunChanged] {
		t.Error("no run.changed event on successful merge")
	}
}
