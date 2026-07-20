package afk

import (
	"errors"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/assets"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// TestLanderLabel pins the lander label grammar (issue #181): lander-<N>,
// [a-z0-9-] only — and, load-bearing, that ParseLabel never misparses one as
// an AFK run (no "afk-" prefix, so the strict inverse rejects it outright).
func TestLanderLabel(t *testing.T) {
	if got := LanderLabel(7); got != "lander-7" {
		t.Errorf("LanderLabel(7) = %q, want lander-7", got)
	}
	if got := LanderLabel(181); got != "lander-181" {
		t.Errorf("LanderLabel(181) = %q, want lander-181", got)
	}
	for _, n := range []int{1, 7, 181} {
		if _, _, ok := ParseLabel(LanderLabel(n)); ok {
			t.Errorf("ParseLabel(%q) accepted a lander label as an AFK run", LanderLabel(n))
		}
	}
}

// TestLanderSeedPrompt pins the lander seed prompt (issue #181 / ADR-0048):
// tokens interpolated at every occurrence, the autoMerge variants differing
// exactly at the verdict step, the incogni sentence on the inline-fix step,
// and the full validation core embedded verbatim below the separator —
// instructions above, doc below.
func TestLanderSeedPrompt(t *testing.T) {
	core, err := assets.Skills.ReadFile("skills/land-pr/validation-core.md")
	if err != nil {
		t.Fatalf("embedded validation core: %v", err)
	}

	p := LanderSeedPrompt(9, "afk/7", true, false)
	for _, banned := range []string{PRToken, BranchToken} {
		if strings.Contains(p, banned) {
			t.Errorf("prompt carries an un-interpolated %s:\n%s", banned, p)
		}
	}
	for _, want := range []string{
		"You are an autonomous lander run. Validate pull request #9 (head branch `afk/7`)",
		"1. Run `labctl pr view 9` and `labctl pr checks 9 --wait`.",
		"2. Validate the pull request against the validation core below.",
		"3. Fix trivial findings only — formatting, lint, merge conflicts — inline: commit, then `git push origin HEAD:refs/heads/afk/7` (your worktree is DETACHED at the PR head, so push the refspec explicitly — a bare `git push` has no upstream to resolve). Substantive problems are never yours to fix — they belong to the verdict.",
		"clean PASS → `labctl pr approve 9`, then `labctl pr merge 9`.",
		"PASS with CONCERNS → `labctl pr approve 9 <concerns>`, then stop — never merge.",
		"FAIL or blocking CONCERNS → `labctl pr reject 9 <findings>`.",
		"5. Then stop working. Do not start unrelated work.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	// The prompt WRAPS the doc: the full validation core sits verbatim below
	// the separator, after every instruction line.
	if !strings.Contains(p, "\n---\n\n"+string(core)) {
		t.Error("validation core not embedded verbatim below the separator")
	}
	if !strings.HasSuffix(p, string(core)) {
		t.Error("prompt does not end with the validation core (instructions above, doc below)")
	}

	// autoMerge=false swaps the merge for a stop at the approve — and that is
	// the ONLY line that changes.
	noMerge := LanderSeedPrompt(9, "afk/7", false, false)
	if !strings.Contains(noMerge, "clean PASS → `labctl pr approve 9`, then stop — a human merges.") {
		t.Errorf("autoMerge=false prompt missing the approve-and-stop verdict:\n%s", noMerge)
	}
	if strings.Contains(noMerge, "labctl pr merge") {
		t.Errorf("autoMerge=false prompt still instructs a merge:\n%s", noMerge)
	}
	withLines, withoutLines := strings.Split(p, "\n"), strings.Split(noMerge, "\n")
	if len(withLines) != len(withoutLines) {
		t.Fatalf("autoMerge variants differ in shape: %d vs %d lines", len(withLines), len(withoutLines))
	}
	for i := range withLines {
		differ := withLines[i] != withoutLines[i]
		isVerdict := strings.HasPrefix(withLines[i], "4. ")
		if differ != isVerdict {
			t.Errorf("line %d: autoMerge variants must differ exactly at the verdict step;\n with: %q\n without: %q", i, withLines[i], withoutLines[i])
		}
	}

	// incogni appends the attribution sentence to the committing step only.
	inc := LanderSeedPrompt(9, "afk/7", true, true)
	const sentence = "No AI attribution anywhere — no co-author trailers, no tool-credit footers, no session links."
	if !strings.Contains(inc, "Substantive problems are never yours to fix — they belong to the verdict. "+sentence) {
		t.Errorf("incogni sentence not appended to the inline-fix step:\n%s", inc)
	}
	if strings.Contains(p, sentence) {
		t.Error("non-incogni prompt carries the attribution sentence")
	}
}

// TestLaunchLander drives the engine-level lander launch end to end (issue
// #181): no issue selection — the (PR, head branch, issue) triple is the
// poller's, already decided — an adopt-branch spawn on the existing claim
// branch, the lander-specific worktree path disjoint from the AFK run's, the
// AFK budget/token-expiry rule, and the seed prompt rendered from the repo's
// AutoMerge/Incogni settings as the trailing spawn positional.
func TestLaunchLander(t *testing.T) {
	f := newFixture(t)
	origin := strings.TrimPrefix(f.repo.RemoteURL, "file://")
	gitCmd(t, f.home, origin, "branch", "afk/7", "main")
	gitCmd(t, f.home, origin, "checkout", "-q", "afk/7")
	gitCmd(t, f.home, origin, "commit", "-q", "--allow-empty", "-m", "claim work")
	gitCmd(t, f.home, origin, "checkout", "-q", "main")
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		AutoMerge: store.Set(true),
	}); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.LaunchLander(t.Context(), f.repo.ID, 9, "afk/7", 7); err != nil {
		t.Fatalf("LaunchLander: %v", err)
	}

	active, err := f.st.ActiveRuns(t.Context())
	if err != nil || len(active) != 1 {
		t.Fatalf("active runs = %v (err %v), want exactly one", active, err)
	}
	run := active[0]
	if run.Kind != store.RunKindLander || run.Branch != "afk/7" {
		t.Fatalf("run = %+v, want a lander on afk/7", run)
	}
	if run.SessionName != "proj~lander-7" {
		t.Errorf("session = %q, want proj~lander-7", run.SessionName)
	}
	if run.IssueNumber == nil || *run.IssueNumber != 7 {
		t.Errorf("issue = %v, want 7", run.IssueNumber)
	}
	if want := f.svc.landerWorktreePath("proj", 7); run.WorktreePath != want || !strings.HasSuffix(want, "proj-lander-7") {
		t.Errorf("worktree = %q, want %q (<repo>-lander-<N>, disjoint from the AFK run's <repo>-<N>)", run.WorktreePath, want)
	}
	if !dirExists(run.WorktreePath) {
		t.Error("lander worktree not created")
	}

	// Budget clock + token expiry follow the AFK rule exactly (default 120
	// minutes; token dies 30 minutes past the deadline).
	wantDeadline := clockTime.Add(120 * time.Minute)
	if run.BudgetDeadline == nil || !run.BudgetDeadline.Equal(wantDeadline) {
		t.Errorf("budget deadline = %v, want %v", run.BudgetDeadline, wantDeadline)
	}
	sess, live := f.runner.Session(run.SessionName)
	if !live || sess.Dir != run.WorktreePath {
		t.Fatalf("session live=%v dir=%q, want live in %q", live, sess.Dir, run.WorktreePath)
	}
	token := envValue(sess.ExtraEnv, "LAB_TOKEN")
	info, err := f.st.RunTokenByHash(t.Context(), ids.HashToken(token))
	if err != nil {
		t.Fatalf("RunTokenByHash: %v", err)
	}
	wantExpiry := wantDeadline.Add(30 * time.Minute)
	if info.ExpiresAt == nil || !info.ExpiresAt.Equal(wantExpiry) {
		t.Errorf("token expiry = %v, want %v", info.ExpiresAt, wantExpiry)
	}

	// The lander seed prompt — the repo's AutoMerge=true and Incogni=false —
	// rides as the trailing spawn positional, exactly like the AFK seed.
	if last := sess.Argv[len(sess.Argv)-1]; last != LanderSeedPrompt(9, "afk/7", true, false) {
		t.Errorf("last spawn argv = %q, want the exact LanderSeedPrompt as one trailing positional", last)
	}

	// The reaper owns the lander: its done-signal contract comes later, but
	// the run must already be in the reaper's active set (ActiveAFKRuns).
	reapable, err := f.st.ActiveAFKRuns(t.Context())
	if err != nil || len(reapable) != 1 || reapable[0].ID != run.ID {
		t.Errorf("ActiveAFKRuns = %v (err %v), want the lander run", reapable, err)
	}
}

// A lander run's Stop is the neutral Stop (§4c), delegated through the
// instance service exactly like the AFK kinds: outcome 'stopped', session
// killed, and the adopted claim KEPT — a Stop must never destroy it.
//
// The claim is the branch ON THE FORGE (it is the PR's head); the adopt is
// detached and never creates a local ref, so origin's branch is what this
// asserts. A local refs/heads/afk/7 used to exist here only as a by-product
// of the old on-branch adopt, and checking it tested the by-product.
func TestLaunchLander_neutralStopParks(t *testing.T) {
	f := newFixture(t)
	origin := strings.TrimPrefix(f.repo.RemoteURL, "file://")
	gitCmd(t, f.home, origin, "branch", "afk/7", "main")
	if err := f.svc.LaunchLander(t.Context(), f.repo.ID, 9, "afk/7", 7); err != nil {
		t.Fatalf("LaunchLander: %v", err)
	}

	outcome, err := f.inst.Stop(t.Context(), "proj~lander-7")
	if err != nil {
		t.Fatalf("instance.Stop: %v", err)
	}
	if outcome != instance.OutcomeParked {
		t.Errorf("stop outcome = %q, want parked", outcome)
	}
	active, _ := f.st.ActiveRuns(t.Context())
	if len(active) != 0 {
		t.Errorf("lander still active after Stop: %v", active)
	}
	if got := gitCmd(t, f.home, origin, "rev-parse", "--verify", "refs/heads/afk/7"); got == "" {
		t.Error("neutral Stop destroyed the claim branch on the forge")
	}
	if n := f.failures(f.repo); n != 0 {
		t.Errorf("failures = %d, want 0 (a neutral Stop never counts)", n)
	}
}

// A lander launch at the live-instance cap is refused with ErrOverCap before
// anything is created — nothing to roll back, the poller retries next tick.
func TestLaunchLander_overCap(t *testing.T) {
	f := newFixture(t)
	if err := f.st.SetSetting(t.Context(), store.SettingMaxInstances, "1"); err != nil {
		t.Fatal(err)
	}
	f.runner.AddLive("other~existing")

	err := f.svc.LaunchLander(t.Context(), f.repo.ID, 9, "afk/7", 7)
	if !errors.Is(err, instance.ErrOverCap) {
		t.Fatalf("err = %v, want ErrOverCap", err)
	}
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Errorf("at-cap lander launch left %d active runs", len(active))
	}
}

// The lander resolves its provider through lander_provider as a STRICT
// per-spawn request: an unknown id fails the launch loudly (the operator
// asked for it by name), never a silent fallback.
func TestLaunchLander_unknownLanderProviderFails(t *testing.T) {
	f := newFixture(t)
	bogus := "no-such-provider"
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		LanderProvider: store.Set(&bogus),
	}); err != nil {
		t.Fatal(err)
	}

	err := f.svc.LaunchLander(t.Context(), f.repo.ID, 9, "afk/7", 7)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("err = %v, want the strict unknown-provider refusal", err)
	}
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Errorf("failed lander launch left %d active runs", len(active))
	}
}
