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
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
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

// mergeFixture is the production-shaped git topology of a built-in CR merge,
// self-contained in this package rather than routed through the operator
// HTTP surface (httpapi's crs_test.go builds the same shape): a BARE origin
// (the repo's real remote) seeded with one commit via a throwaway work clone,
// the lab bare reference clone at <ReposDir>/<repoID>.git, a builtin repo row,
// the global author identity, and the Service under test.
type mergeFixture struct {
	t      *testing.T
	home   string
	origin string
	bare   string
	env    []string
	eng    *gitx.Engine
	st     *store.Store
	repo   store.Repo
	bus    *events.Bus
	svc    *Service
}

func newMergeFixture(t *testing.T) *mergeFixture {
	t.Helper()
	testutil.RequireTool(t, "git")
	home := t.TempDir()

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

	bus := events.NewBus()
	svc := New(Config{
		Store: st, Git: eng, Bus: bus, ReposDir: reposDir, GitEnv: env,
		Now: time.Now, Logger: logx.New(io.Discard),
	})
	return &mergeFixture{t: t, home: home, origin: origin, bare: bare, env: env,
		eng: eng, st: st, repo: repo, bus: bus, svc: svc}
}

// addCR creates a CR head branch the way a run creates one — worktree forked
// from origin/main in the bare clone, mutate + commit, worktree removed with
// the branch kept — optionally publishes it to origin (an agent's `git push
// origin HEAD`), and files the open CR row for it. Returns the CR and the
// head sha.
func (f *mergeFixture) addCR(branch string, push bool) (store.CR, string) {
	t := f.t
	t.Helper()
	wt := filepath.Join(t.TempDir(), strings.ReplaceAll(branch, "/", "-"))
	if err := f.eng.AddWorktree(context.Background(), f.bare, wt, branch, "main", f.env); err != nil {
		t.Fatalf("AddWorktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, strings.ReplaceAll(branch, "/", "-")+".txt"), []byte(branch+"\n"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	mergeGitCmd(t, f.home, wt, "add", "-A")
	mergeGitCmd(t, f.home, wt, "commit", "-q", "-m", "cr work on "+branch)
	sha := mergeGitCmd(t, f.home, wt, "rev-parse", "HEAD")
	if err := f.eng.RemoveWorktree(context.Background(), f.bare, wt, f.env); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if push {
		mergeGitCmd(t, f.home, f.bare, "push", "-q", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
	}
	cr, err := f.st.CreateCR(context.Background(), f.repo.ID, "feat: "+branch, "", branch, "main", nil, time.Now())
	if err != nil {
		t.Fatalf("CreateCR: %v", err)
	}
	return cr, sha
}

// hasRef reports whether ref resolves in the git dir dir.
func (f *mergeFixture) hasRef(dir, ref string) bool {
	f.t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), testutil.HermeticGitEnv(f.home)...)
	return cmd.Run() == nil
}

// TestMergePublishesRunChanged pins issue #149's new publish: a successful
// Merge fires run.changed alongside the pinned cr.changed, on the same
// repo-scoped {type, repoID} envelope, since every run forked off the base
// branch just became more behind and the SPA's commits_behind badges need
// the same refetch signal a run mutation gives them.
func TestMergePublishesRunChanged(t *testing.T) {
	f := newMergeFixture(t)
	cr, _ := f.addCR("afk/1", false)

	ch, cancel := f.bus.Subscribe(context.Background())
	defer cancel()

	if _, _, err := f.svc.Merge(context.Background(), f.repo.ID, cr.Number, tracker.MergeOptions{}); err != nil {
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

// TestMergeDeletesHeadOnOrigin pins ADR-0081's built-in half (issue #90):
// with MergeOptions.DeleteHead the merged CR's head ref is ABSENT on origin
// afterwards — deleted when it was pushed, and reported deleted too when it
// never was — while the LOCAL head branch is left for teardown/the sweep.
// Without the option origin keeps the ref and the outcome is kept/"setting
// off".
func TestMergeDeletesHeadOnOrigin(t *testing.T) {
	cases := []struct {
		name       string
		push       bool
		opts       tracker.MergeOptions
		want       tracker.HeadResult
		wantOrigin bool // refs/heads/<head> present on origin after the merge
	}{
		{"pushed head deleted", true, tracker.MergeOptions{DeleteHead: true},
			tracker.HeadResult{Outcome: tracker.HeadDeleted}, false},
		{"never pushed reads deleted", false, tracker.MergeOptions{DeleteHead: true},
			tracker.HeadResult{Outcome: tracker.HeadDeleted}, false},
		{"option off keeps it", true, tracker.MergeOptions{},
			tracker.HeadResult{Outcome: tracker.HeadKept, Reason: tracker.HeadKeptSettingOff}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMergeFixture(t)
			cr, sha := f.addCR("afk/1", tc.push)

			merged, head, err := f.svc.Merge(context.Background(), f.repo.ID, cr.Number, tc.opts)
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			if merged.State != store.CRStateMerged {
				t.Fatalf("CR state = %s, want merged", merged.State)
			}
			if head != tc.want {
				t.Errorf("head = %+v, want %+v", head, tc.want)
			}
			if got := f.hasRef(f.origin, "refs/heads/afk/1"); got != tc.wantOrigin {
				t.Errorf("origin refs/heads/afk/1 present = %v, want %v", got, tc.wantOrigin)
			}
			// The bare clone's tracking ref follows origin (the deletion push
			// updates it); it never advertises a deleted head.
			if !tc.wantOrigin && f.hasRef(f.bare, "refs/remotes/origin/afk/1") {
				t.Error("bare clone still carries refs/remotes/origin/afk/1 after the delete")
			}
			if got := mergeGitCmd(t, f.home, f.bare, "rev-parse", "refs/heads/afk/1"); got != sha {
				t.Errorf("local refs/heads/afk/1 = %q, want it untouched at %s", got, sha)
			}
			if got := mergeGitCmd(t, f.home, f.origin, "rev-parse", "refs/heads/main"); got != sha {
				t.Errorf("origin main = %s, want the ff-merged head %s", got, sha)
			}
		})
	}
}

// TestMergeHeadDeleteRefusedStaysMerged: origin refusing the DELETION (a
// pre-receive hook that accepts the merge push but declines ref deletes)
// reads HeadFailed with the hook's own words — and the merge is still a
// success, recorded, with no error (a delete failure never fails a landed
// merge).
func TestMergeHeadDeleteRefusedStaysMerged(t *testing.T) {
	f := newMergeFixture(t)
	cr, _ := f.addCR("afk/1", true)
	hook := "#!/bin/sh\nwhile read old new ref; do\n  case \"$new\" in *[!0]*) ;; *) echo \"branch deletion is protected\" >&2; exit 1 ;; esac\ndone\nexit 0\n"
	if err := os.WriteFile(filepath.Join(f.origin, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatalf("install pre-receive hook: %v", err)
	}

	merged, head, err := f.svc.Merge(context.Background(), f.repo.ID, cr.Number, tracker.MergeOptions{DeleteHead: true})
	if err != nil {
		t.Fatalf("Merge err = %v, want success despite the refused delete", err)
	}
	if merged.State != store.CRStateMerged {
		t.Fatalf("CR state = %s, want merged", merged.State)
	}
	if head.Outcome != tracker.HeadFailed || !strings.Contains(head.Reason, "branch deletion is protected") {
		t.Errorf("head = %+v, want failed carrying the hook's words", head)
	}
	row, err := f.st.CRByRepoNumber(context.Background(), f.repo.ID, cr.Number)
	if err != nil {
		t.Fatalf("CRByRepoNumber: %v", err)
	}
	if row.State != store.CRStateMerged || row.MergeCommit == nil {
		t.Errorf("recorded row = %s (merge_commit %v), want merged", row.State, row.MergeCommit)
	}
	if !f.hasRef(f.origin, "refs/heads/afk/1") {
		t.Error("origin lost refs/heads/afk/1 despite the refusal")
	}
}

// TestDeleteHeadStandalone: the convergent re-merge path's delete — on a head
// already gone from origin it reads deleted (absent is the definition); on
// one still there it deletes it; on an unknown repo it is failed, not a
// panic or an error return.
func TestDeleteHeadStandalone(t *testing.T) {
	f := newMergeFixture(t)
	cr, _ := f.addCR("afk/1", true)
	if _, _, err := f.svc.Merge(context.Background(), f.repo.ID, cr.Number, tracker.MergeOptions{DeleteHead: true}); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got := f.svc.DeleteHead(context.Background(), f.repo.ID, "afk/1"); got != (tracker.HeadResult{Outcome: tracker.HeadDeleted}) {
		t.Errorf("DeleteHead(already gone) = %+v, want deleted", got)
	}

	f.addCR("afk/2", true)
	if got := f.svc.DeleteHead(context.Background(), f.repo.ID, "afk/2"); got.Outcome != tracker.HeadDeleted {
		t.Errorf("DeleteHead(present) = %+v, want deleted", got)
	}
	if f.hasRef(f.origin, "refs/heads/afk/2") {
		t.Error("origin still carries refs/heads/afk/2")
	}
	if !f.hasRef(f.bare, "refs/heads/afk/2") {
		t.Error("DeleteHead removed the LOCAL branch afk/2")
	}

	if got := f.svc.DeleteHead(context.Background(), "repo_nope", "afk/3"); got.Outcome != tracker.HeadFailed || got.Reason == "" {
		t.Errorf("DeleteHead(unknown repo) = %+v, want failed with a reason", got)
	}
}

// TestDeleteHeadNeverTouchesTheBase: the belt-and-braces guard — a head that
// names the base/default branch is kept, never pushed away.
func TestDeleteHeadNeverTouchesTheBase(t *testing.T) {
	f := newMergeFixture(t)
	got := f.svc.DeleteHead(context.Background(), f.repo.ID, "main")
	if got.Outcome != tracker.HeadKept {
		t.Errorf("DeleteHead(main) = %+v, want kept", got)
	}
	if !f.hasRef(f.origin, "refs/heads/main") {
		t.Fatal("origin lost main")
	}
}
