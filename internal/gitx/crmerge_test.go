package gitx

// Integration tests for the M6 change-request diff + merge surface, against
// real git (design §11 / D17: gitx merge paths run on real repos in
// t.TempDir()). The fixture mirrors production topology: a BARE origin (the
// repo's real remote — a bare push target, like any forge), a work clone
// that drives origin's main forward (the "someone else pushed" actor), and
// the lab bare reference clone the Engine operates on. CR head branches are
// created exactly the way runs create them — AddWorktree fork from
// origin/main, commit, RemoveWorktree with the branch kept — the state a
// reaped run leaves behind.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

const (
	crAuthorName  = "Real Name"
	crAuthorEmail = "real@example.invalid"
	crAuthorID    = crAuthorName + "|" + crAuthorEmail + "|" + crAuthorName + "|" + crAuthorEmail
)

type crFixture struct {
	t      *testing.T
	home   string
	origin string // bare push target — the repo's REAL remote
	work   string // working clone that advances origin's main
	bare   string // lab bare reference clone (the Engine's bareDir)
	env    []string
	eng    *Engine
}

func newCRFixture(t *testing.T) *crFixture {
	t.Helper()
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	env := testutil.HermeticGitEnv(home)

	work := makeOrigin(t, home, "main", 2) // f0.txt, f1.txt
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitCmd(t, home, "", "init", "-q", "--bare", "-b", "main", origin)
	gitCmd(t, home, work, "remote", "add", "origin", origin)
	gitCmd(t, home, work, "push", "-q", "origin", "main")

	bare := filepath.Join(t.TempDir(), "repo.git")
	eng := New("git")
	if err := eng.CloneBare(t.Context(), origin, bare, env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}
	return &crFixture{t: t, home: home, origin: origin, work: work, bare: bare, env: env, eng: eng}
}

// addHead creates a CR head branch the way a run does: worktree forked from
// origin/main, mutate + commit, worktree removed with the branch kept (the
// guarded teardown keeps an unmerged branch). Returns the head sha.
func (f *crFixture) addHead(branch string, mutate func(dir string)) string {
	t := f.t
	t.Helper()
	wt := filepath.Join(t.TempDir(), strings.ReplaceAll(branch, "/", "-"))
	if err := f.eng.AddWorktree(t.Context(), f.bare, wt, branch, "main", f.env); err != nil {
		t.Fatalf("AddWorktree(%s): %v", branch, err)
	}
	mutate(wt)
	gitCmd(t, f.home, wt, "add", "-A")
	gitCmd(t, f.home, wt, "commit", "-q", "-m", "cr work on "+branch)
	sha := gitCmd(t, f.home, wt, "rev-parse", "HEAD")
	if err := f.eng.RemoveWorktree(t.Context(), f.bare, wt, f.env); err != nil {
		t.Fatalf("RemoveWorktree(%s): %v", branch, err)
	}
	return sha
}

// advanceOrigin commits file=content in the work clone and pushes it to
// origin's main — base movement the bare reference clone has NOT fetched.
func (f *crFixture) advanceOrigin(file, content string) string {
	t := f.t
	t.Helper()
	writeFileT(t, filepath.Join(f.work, file), content)
	gitCmd(t, f.home, f.work, "add", "-A")
	gitCmd(t, f.home, f.work, "commit", "-q", "-m", "advance "+file)
	gitCmd(t, f.home, f.work, "push", "-q", "origin", "main")
	return gitCmd(t, f.home, f.work, "rev-parse", "HEAD")
}

// installRejectHook makes origin refuse every push via a pre-receive hook —
// the protected-branch stand-in. msg is what the hook prints to stderr.
func (f *crFixture) installRejectHook(msg string) {
	f.t.Helper()
	hook := "#!/bin/sh\necho \"" + msg + "\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(f.origin, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		f.t.Fatalf("install pre-receive hook: %v", err)
	}
}

// worktreeCount counts the bare repo's worktree entries (the bare repo
// itself is always one) — the temp-worktree-leak detector.
func (f *crFixture) worktreeCount() int {
	f.t.Helper()
	wts, err := f.eng.Worktrees(f.t.Context(), f.bare, f.env)
	if err != nil {
		f.t.Fatalf("Worktrees: %v", err)
	}
	return len(wts)
}

func (f *crFixture) originMain() string {
	f.t.Helper()
	return gitCmd(f.t, f.home, f.origin, "rev-parse", "main")
}

func (f *crFixture) localOriginMain() string {
	f.t.Helper()
	return gitCmd(f.t, f.home, f.bare, "rev-parse", "refs/remotes/origin/main")
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestCRMerge_fastForward(t *testing.T) {
	f := newCRFixture(t)
	head := f.addHead("afk/1", func(dir string) {
		writeFileT(t, filepath.Join(dir, "feature.txt"), "feature\n")
	})

	got, err := f.eng.CRMerge(t.Context(), f.bare, "main", "afk/1", "Merge change request #1", crAuthorName, crAuthorEmail, f.env)
	if err != nil {
		t.Fatalf("CRMerge: %v", err)
	}
	if got != head {
		t.Errorf("mergeCommit = %s, want head sha %s (a fast-forward must not create a commit)", got, head)
	}
	if o := f.originMain(); o != head {
		t.Errorf("origin main = %s, want %s (origin base must advance to head)", o, head)
	}
	if l := f.localOriginMain(); l != head {
		t.Errorf("local refs/remotes/origin/main = %s, want %s (fetch-after-push must refresh it)", l, head)
	}

	// Second merge of the already-merged CR: the convergence half of the
	// CRMerge concurrency invariant — ancestry makes it a no-op ff (push of
	// an already-present sha), same returned sha, origin untouched. The
	// service-level open-state guard is the real double-merge protection.
	again, err := f.eng.CRMerge(t.Context(), f.bare, "main", "afk/1", "Merge change request #1", crAuthorName, crAuthorEmail, f.env)
	if err != nil {
		t.Fatalf("second CRMerge: %v", err)
	}
	if again != head {
		t.Errorf("second merge returned %s, want %s", again, head)
	}
	if o := f.originMain(); o != head {
		t.Errorf("origin main moved to %s on the second merge, want %s", o, head)
	}
}

func TestCRMerge_mergeCommit(t *testing.T) {
	f := newCRFixture(t)
	head := f.addHead("afk/2", func(dir string) {
		writeFileT(t, filepath.Join(dir, "cr.txt"), "cr\n")
	})
	baseSHA := f.advanceOrigin("base.txt", "base\n") // diverge; the bare clone has NOT fetched this
	before := f.worktreeCount()

	msg := "Merge change request #2: add cr.txt"
	got, err := f.eng.CRMerge(t.Context(), f.bare, "main", "afk/2", msg, crAuthorName, crAuthorEmail, f.env)
	if err != nil {
		t.Fatalf("CRMerge: %v", err)
	}
	if got == head || got == baseSHA {
		t.Fatalf("mergeCommit = %s, want a fresh merge commit (head %s, base %s)", got, head, baseSHA)
	}
	// Parent order: ^1 = the base being merged into (proving the fetch-first
	// picked up origin's CURRENT tip, not the stale clone-time ref), ^2 = head.
	if p1 := gitCmd(t, f.home, f.bare, "rev-parse", got+"^1"); p1 != baseSHA {
		t.Errorf("merge commit ^1 = %s, want fresh origin base %s", p1, baseSHA)
	}
	if p2 := gitCmd(t, f.home, f.bare, "rev-parse", got+"^2"); p2 != head {
		t.Errorf("merge commit ^2 = %s, want head %s", p2, head)
	}
	if s := gitCmd(t, f.home, f.bare, "log", "-1", "--format=%s", got); s != msg {
		t.Errorf("merge commit subject = %q, want %q", s, msg)
	}
	// D15 measure 5: the passed real identity must win over the ambient
	// GIT_AUTHOR_*/GIT_COMMITTER_* (HermeticGitEnv sets lab-test).
	if id := gitCmd(t, f.home, f.bare, "log", "-1", "--format=%an|%ae|%cn|%ce", got); id != crAuthorID {
		t.Errorf("merge commit identity = %q, want %q", id, crAuthorID)
	}
	if o := f.originMain(); o != got {
		t.Errorf("origin main = %s, want merge commit %s", o, got)
	}
	if l := f.localOriginMain(); l != got {
		t.Errorf("local refs/remotes/origin/main = %s, want %s (fetch-after-push)", l, got)
	}
	if after := f.worktreeCount(); after != before {
		t.Errorf("worktree count %d → %d: temporary merge worktree leaked", before, after)
	}

	// Second merge: head is already contained in origin/main, so the merge
	// path answers "Already up to date" — no second merge commit, no ref
	// movement, the existing tip comes back (documented CRMerge invariant).
	count := gitCmd(t, f.home, f.bare, "rev-list", "--count", "refs/remotes/origin/main")
	again, err := f.eng.CRMerge(t.Context(), f.bare, "main", "afk/2", msg, crAuthorName, crAuthorEmail, f.env)
	if err != nil {
		t.Fatalf("second CRMerge: %v", err)
	}
	if again != got {
		t.Errorf("second merge returned %s, want existing merge commit %s", again, got)
	}
	if c := gitCmd(t, f.home, f.bare, "rev-list", "--count", "refs/remotes/origin/main"); c != count {
		t.Errorf("origin/main commit count %s → %s: second merge created a commit", count, c)
	}
	if after := f.worktreeCount(); after != before {
		t.Errorf("worktree count %d → %d after second merge: temp worktree leaked", before, after)
	}
}

func TestCRMerge_pushRejected(t *testing.T) {
	f := newCRFixture(t)
	f.addHead("afk/3", func(dir string) {
		writeFileT(t, filepath.Join(dir, "cr3.txt"), "cr3\n")
	})
	baseSHA := f.advanceOrigin("base2.txt", "base2\n") // force the merge-commit path
	f.installRejectHook("push declined: protected branch main")
	before := f.worktreeCount()

	t.Run("merge-commit path", func(t *testing.T) {
		_, err := f.eng.CRMerge(t.Context(), f.bare, "main", "afk/3", "Merge change request #3", crAuthorName, crAuthorEmail, f.env)
		if err == nil {
			t.Fatal("CRMerge against a rejecting origin succeeded, want ErrPushRejected")
		}
		if !errors.Is(err, ErrPushRejected) {
			t.Fatalf("err = %v, want ErrPushRejected", err)
		}
		if !strings.Contains(err.Error(), "push declined: protected branch main") {
			t.Errorf("rejection error does not carry the hook's stderr verbatim: %v", err)
		}
		if o := f.originMain(); o != baseSHA {
			t.Errorf("origin main = %s after rejected push, want unchanged %s", o, baseSHA)
		}
		if after := f.worktreeCount(); after != before {
			t.Errorf("worktree count %d → %d: temp worktree leaked on the rejection path", before, after)
		}
	})

	t.Run("fast-forward path", func(t *testing.T) {
		// A head forked from origin's current tip is ff-able; the hook still
		// rejects, and the ff push must type the same way.
		f.addHead("afk/4", func(dir string) {
			writeFileT(t, filepath.Join(dir, "cr4.txt"), "cr4\n")
		})
		_, err := f.eng.CRMerge(t.Context(), f.bare, "main", "afk/4", "Merge change request #4", crAuthorName, crAuthorEmail, f.env)
		if !errors.Is(err, ErrPushRejected) {
			t.Fatalf("ff-path err = %v, want ErrPushRejected", err)
		}
		if o := f.originMain(); o != baseSHA {
			t.Errorf("origin main = %s after rejected ff push, want unchanged %s", o, baseSHA)
		}
	})
}

func TestCRMerge_missingHeadBranch(t *testing.T) {
	f := newCRFixture(t)
	_, err := f.eng.CRMerge(t.Context(), f.bare, "main", "afk/nope", "Merge", crAuthorName, crAuthorEmail, f.env)
	if !errors.Is(err, ErrHeadMissing) {
		t.Fatalf("err = %v, want ErrHeadMissing", err)
	}
}

func TestCRMerge_conflictCleansTempWorktree(t *testing.T) {
	f := newCRFixture(t)
	f.addHead("afk/5", func(dir string) {
		writeFileT(t, filepath.Join(dir, "f0.txt"), "head version\n")
	})
	baseSHA := f.advanceOrigin("f0.txt", "base version\n") // same file, conflicting change
	before := f.worktreeCount()

	_, err := f.eng.CRMerge(t.Context(), f.bare, "main", "afk/5", "Merge change request #5", crAuthorName, crAuthorEmail, f.env)
	if err == nil {
		t.Fatal("conflicting CRMerge succeeded, want error")
	}
	if errors.Is(err, ErrPushRejected) {
		t.Errorf("merge conflict misclassified as push rejection: %v", err)
	}
	// Regression (review): merge-ort writes its whole conflict report to
	// STDOUT; the stderr-only error shape surfaced a conflict as a blank
	// "exit status 1". The typed sentinel + the report are the 409 body.
	if !errors.Is(err, ErrMergeConflict) {
		t.Errorf("conflict error is not ErrMergeConflict: %v", err)
	}
	if !strings.Contains(err.Error(), "CONFLICT") {
		t.Errorf("conflict error does not carry git's conflict report: %v", err)
	}
	if o := f.originMain(); o != baseSHA {
		t.Errorf("origin main = %s after failed merge, want unchanged %s", o, baseSHA)
	}
	if after := f.worktreeCount(); after != before {
		t.Errorf("worktree count %d → %d: conflicted temp worktree leaked", before, after)
	}
}

func TestCRDiff_contentRenameAndThreeDot(t *testing.T) {
	f := newCRFixture(t)
	f.addHead("afk/6", func(dir string) {
		writeFileT(t, filepath.Join(dir, "f0.txt"), "content zero\nadded line\n")
		if err := os.Rename(filepath.Join(dir, "f1.txt"), filepath.Join(dir, "renamed.txt")); err != nil {
			t.Fatalf("rename fixture file: %v", err)
		}
	})
	// Base-side movement AFTER the fork, fetched into the local origin ref:
	// the three-dot (merge-base) diff must not show it.
	f.advanceOrigin("base-only.txt", "base only\n")
	if err := f.eng.Fetch(t.Context(), f.bare, f.env); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	diff, truncated, err := f.eng.CRDiff(t.Context(), f.bare, "main", "afk/6", f.env)
	if err != nil {
		t.Fatalf("CRDiff: %v", err)
	}
	if truncated {
		t.Error("small diff reported truncated")
	}
	for _, want := range []string{
		"-content 0",                   // removed line
		"+content zero",                // added line
		"+added line",                  // added line
		"rename from f1.txt",           // -M rename detection
		"rename to renamed.txt",        //
		"diff --git a/f0.txt b/f0.txt", // file header
	} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q:\n%s", want, diff)
		}
	}
	if strings.Contains(diff, "base-only.txt") {
		t.Errorf("three-dot diff leaked a base-side change:\n%s", diff)
	}
}

func TestCRDiff_truncationBound(t *testing.T) {
	f := newCRFixture(t)
	var b strings.Builder
	for i := 0; b.Len() <= crDiffMaxBytes+(1<<18); i++ {
		fmt.Fprintf(&b, "line %08d: %s\n", i, strings.Repeat("x", 48))
	}
	f.addHead("afk/7", func(dir string) {
		writeFileT(t, filepath.Join(dir, "big.txt"), b.String())
	})

	diff, truncated, err := f.eng.CRDiff(t.Context(), f.bare, "main", "afk/7", f.env)
	if err != nil {
		t.Fatalf("CRDiff: %v", err)
	}
	if !truncated {
		t.Fatalf("diff of a %d-byte file not reported truncated", b.Len())
	}
	if len(diff) > crDiffMaxBytes {
		t.Errorf("truncated diff is %d bytes, want <= %d", len(diff), crDiffMaxBytes)
	}
	if !strings.HasSuffix(diff, "\n") {
		t.Error("truncated diff does not end at a line boundary")
	}
	if !strings.Contains(diff[:200], "diff --git") {
		t.Errorf("truncated diff lost its header, starts with %q", diff[:80])
	}
}

func TestCRDiff_missingHead(t *testing.T) {
	f := newCRFixture(t)
	_, _, err := f.eng.CRDiff(t.Context(), f.bare, "main", "afk/none", f.env)
	if !errors.Is(err, ErrHeadMissing) {
		t.Fatalf("err = %v, want ErrHeadMissing", err)
	}
}

// Regression (review): headCommit must distinguish "branch missing" (rev-parse
// exit 1) from a broken/missing repo (exit 128) — a corrupt bare dir
// masquerading as ErrHeadMissing would 409 "head branch missing" on the merge
// route when the truth is an internal failure.
func TestHeadCommit_discriminatesMissingFromBroken(t *testing.T) {
	f := newCRFixture(t)
	if _, err := f.eng.headCommit(t.Context(), f.bare, "no-such-branch", f.env); !errors.Is(err, ErrHeadMissing) {
		t.Errorf("missing branch: err = %v, want ErrHeadMissing", err)
	}
	if _, err := f.eng.headCommit(t.Context(), t.TempDir(), "main", f.env); errors.Is(err, ErrHeadMissing) {
		t.Errorf("non-repo dir classified as ErrHeadMissing: %v", err)
	} else if err == nil {
		t.Error("non-repo dir resolved a head commit")
	}
}

// --- DeleteRemoteBranch (ADR-0081, issue #90) --------------------------------

// pushHead publishes a local head branch to origin from the bare clone — the
// state an agent's `git push origin HEAD` leaves (origin carries the head,
// and the push refreshes refs/remotes/origin/<branch>).
func (f *crFixture) pushHead(branch string) {
	f.t.Helper()
	gitCmd(f.t, f.home, f.bare, "push", "-q", "origin", "refs/heads/"+branch+":refs/heads/"+branch)
}

// refIn reports whether ref resolves in the git dir dir.
func (f *crFixture) refIn(dir, ref string) bool {
	f.t.Helper()
	return f.eng.refExists(f.t.Context(), dir, ref, f.env)
}

// installRejectDeletionHook makes origin refuse every ref DELETION (all-zero
// new sha) via pre-receive while accepting ordinary pushes — the "branch
// protection forbids deleting this ref" stand-in.
func (f *crFixture) installRejectDeletionHook(msg string) {
	f.t.Helper()
	hook := "#!/bin/sh\nwhile read old new ref; do\n  case \"$new\" in *[!0]*) ;; *) echo \"" + msg + "\" >&2; exit 1 ;; esac\ndone\nexit 0\n"
	if err := os.WriteFile(filepath.Join(f.origin, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		f.t.Fatalf("install pre-receive hook: %v", err)
	}
}

// TestDeleteRemoteBranch_pushedHead: a head published to origin is deleted
// there; the push-side tracking-ref update drops refs/remotes/origin/<head>
// too; the LOCAL branch is untouched (teardown/the sweep own it).
func TestDeleteRemoteBranch_pushedHead(t *testing.T) {
	f := newCRFixture(t)
	head := f.addHead("afk/1", func(dir string) {
		writeFileT(t, filepath.Join(dir, "feature.txt"), "feature\n")
	})
	f.pushHead("afk/1")
	if !f.refIn(f.origin, "refs/heads/afk/1") || !f.refIn(f.bare, "refs/remotes/origin/afk/1") {
		t.Fatal("fixture: head not published to origin")
	}

	if err := f.eng.DeleteRemoteBranch(t.Context(), f.bare, "afk/1", f.env); err != nil {
		t.Fatalf("DeleteRemoteBranch: %v", err)
	}
	if f.refIn(f.origin, "refs/heads/afk/1") {
		t.Error("origin still carries refs/heads/afk/1 after the delete")
	}
	if f.refIn(f.bare, "refs/remotes/origin/afk/1") {
		t.Error("bare clone still carries refs/remotes/origin/afk/1 after the delete")
	}
	if got := gitCmd(t, f.home, f.bare, "rev-parse", "refs/heads/afk/1"); got != head {
		t.Errorf("local refs/heads/afk/1 = %q, want it untouched at %s", got, head)
	}
}

// TestDeleteRemoteBranch_absentIsSuccess: "absent on origin afterwards" is
// the contract, so a never-pushed head and an already-deleted one (the
// convergent re-merge) both answer nil — and a stale remote-tracking ref left
// behind by an out-of-band delete is dropped.
func TestDeleteRemoteBranch_absentIsSuccess(t *testing.T) {
	f := newCRFixture(t)

	t.Run("never pushed", func(t *testing.T) {
		f.addHead("afk/2", func(dir string) {
			writeFileT(t, filepath.Join(dir, "two.txt"), "two\n")
		})
		if err := f.eng.DeleteRemoteBranch(t.Context(), f.bare, "afk/2", f.env); err != nil {
			t.Fatalf("DeleteRemoteBranch(never pushed) = %v, want nil", err)
		}
		if !f.refIn(f.bare, "refs/heads/afk/2") {
			t.Error("local branch afk/2 removed, want it untouched")
		}
	})

	t.Run("already gone with a stale tracking ref", func(t *testing.T) {
		f.addHead("afk/3", func(dir string) {
			writeFileT(t, filepath.Join(dir, "three.txt"), "three\n")
		})
		f.pushHead("afk/3")
		// Someone deletes it on origin directly; the bare clone's tracking
		// ref goes stale (a plain fetch does not prune).
		gitCmd(t, f.home, f.origin, "update-ref", "-d", "refs/heads/afk/3")
		if !f.refIn(f.bare, "refs/remotes/origin/afk/3") {
			t.Fatal("fixture: tracking ref missing before the delete")
		}
		if err := f.eng.DeleteRemoteBranch(t.Context(), f.bare, "afk/3", f.env); err != nil {
			t.Fatalf("DeleteRemoteBranch(already gone) = %v, want nil", err)
		}
		if f.refIn(f.bare, "refs/remotes/origin/afk/3") {
			t.Error("stale refs/remotes/origin/afk/3 survived the already-absent path")
		}
	})

}

// TestDeleteRemoteBranch_refusedAndUnreachable: an origin that refuses the
// deletion surfaces ErrPushRejected with the hook's own words (the ref is
// still there), and an unreachable origin is an error too — never a silent
// "absent".
func TestDeleteRemoteBranch_refusedAndUnreachable(t *testing.T) {
	f := newCRFixture(t)
	f.addHead("afk/5", func(dir string) {
		writeFileT(t, filepath.Join(dir, "five.txt"), "five\n")
	})
	f.pushHead("afk/5")
	f.installRejectDeletionHook("deleting branches is forbidden here")

	err := f.eng.DeleteRemoteBranch(t.Context(), f.bare, "afk/5", f.env)
	if !errors.Is(err, ErrPushRejected) {
		t.Fatalf("err = %v, want ErrPushRejected", err)
	}
	if !strings.Contains(err.Error(), "deleting branches is forbidden here") {
		t.Errorf("refusal does not carry the hook's words: %v", err)
	}
	if !f.refIn(f.origin, "refs/heads/afk/5") {
		t.Error("origin lost refs/heads/afk/5 despite the refusal")
	}

	gitCmd(t, f.home, f.bare, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	if err := f.eng.DeleteRemoteBranch(t.Context(), f.bare, "afk/5", f.env); err == nil {
		t.Fatal("DeleteRemoteBranch against an unreachable origin = nil, want an error")
	}
}
