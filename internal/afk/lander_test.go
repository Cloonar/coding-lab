package afk

import (
	"errors"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/assets"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
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

// TestEscalateLabel pins the escalate label grammar (issue #182):
// escalate-<N>, [a-z0-9-] only — and that ParseLabel never misparses one as
// an AFK run.
func TestEscalateLabel(t *testing.T) {
	if got := EscalateLabel(7); got != "escalate-7" {
		t.Errorf("EscalateLabel(7) = %q, want escalate-7", got)
	}
	if got := EscalateLabel(182); got != "escalate-182" {
		t.Errorf("EscalateLabel(182) = %q, want escalate-182", got)
	}
	for _, n := range []int{1, 7, 182} {
		if _, _, ok := ParseLabel(EscalateLabel(n)); ok {
			t.Errorf("ParseLabel(%q) accepted an escalate label as an AFK run", EscalateLabel(n))
		}
	}
}

// TestEscalateSeedPrompt pins the escalate seed prompt (issue #182 /
// ADR-0048): tokens interpolated at every occurrence, the hand-off contract
// verbatim (digest to the issue, idempotent label create before the flip,
// tolerated remove error, `labctl pr escalate` posted LAST as the terminal
// marker, stop), the round history appended verbatim below the separator
// AFTER interpolation, no separator when empty — and no incogni variant, so
// never an attribution sentence (there is no commit step to hang one on).
func TestEscalateSeedPrompt(t *testing.T) {
	const history = "Verdict comments (oldest first):\n\nround history here"
	p := EscalateSeedPrompt(7, 9, "afk/7", history)
	for _, banned := range []string{gitx.NToken, PRToken, BranchToken} {
		if strings.Contains(p, banned) {
			t.Errorf("prompt carries an un-interpolated %s:\n%s", banned, p)
		}
	}
	for _, want := range []string{
		"You are an autonomous escalation run. Pull request #9 (head branch `afk/7`, issue #7) has exhausted its fix budget and is still rejected; hand it to a human and stop.",
		"1. `labctl issue view 7` and `labctl pr view 9` — read fully; the round history is below. Inspect the branch (git log/diff) as needed.",
		"2. Write a history digest: what was rejected each round, what each fix attempt changed, why it still fails.",
		"3. `labctl issue comment 7 <digest>`.",
		"4. `labctl label create --name ready-for-human` (idempotent, safe if it exists), then `labctl issue label remove 7 ready-for-agent` and `labctl issue label add 7 ready-for-human`. Tolerate a remove error if the label is already gone.",
		"5. `labctl pr escalate 9 <digest>` — the terminal marker; post it LAST.",
		"6. Then stop working. Do not start unrelated work.",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	// The escalate marker step comes after the digest comment and the label
	// flip — LAST is load-bearing (posting it first could reap the run
	// mid-hand-off).
	if strings.Index(p, "labctl pr escalate") < strings.Index(p, "labctl issue label add") {
		t.Errorf("pr escalate not the last labctl step:\n%s", p)
	}
	// The history rides verbatim below the separator; `<digest>` is an
	// unknown token and passes through literally.
	if !strings.HasSuffix(p, "\n\n---\n\n"+history) {
		t.Errorf("history not appended below the separator:\n%s", p)
	}

	// No history → no separator, the prompt ends at the stop line.
	bare := EscalateSeedPrompt(7, 9, "afk/7", "")
	if strings.Contains(bare, "---") {
		t.Errorf("empty history still appended a separator:\n%s", bare)
	}
	if !strings.HasSuffix(bare, "6. Then stop working. Do not start unrelated work.") {
		t.Errorf("bare prompt does not end at the stop line:\n%s", bare)
	}
	if strings.Contains(p, "No AI attribution") {
		t.Error("escalate prompt carries an attribution sentence (it has no commit step)")
	}

	// History is appended AFTER interpolation: quoted forge prose is never
	// token-substituted.
	quoted := EscalateSeedPrompt(7, 9, "afk/7", "a review quoting "+gitx.NToken+" literally")
	if !strings.HasSuffix(quoted, "a review quoting "+gitx.NToken+" literally") {
		t.Errorf("history content was token-substituted:\n%s", quoted)
	}
}

// TestEscalateSeedPromptTemplate pins that the escalate template carries the
// LITERAL tokens — including the issue number in three distinct steps (view,
// comment, label flip), which is exactly why token interpolation replaces
// EVERY occurrence.
func TestEscalateSeedPromptTemplate(t *testing.T) {
	tmpl := EscalateSeedPromptTemplate()
	for _, token := range []string{gitx.NToken, PRToken, BranchToken} {
		if !strings.Contains(tmpl, token) {
			t.Errorf("template missing literal %s:\n%s", token, tmpl)
		}
	}
	if strings.Count(tmpl, gitx.NToken) < 3 {
		t.Errorf("template names the issue fewer than 3 times (view, comment, label flip):\n%s", tmpl)
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

	if err := f.svc.LaunchLander(t.Context(), f.repo.ID, 9, "afk/7", 7, false); err != nil {
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

// approveOnly forces the approve-and-stop seed variant regardless of the
// repo's AutoMerge (issue #182): a re-validation lander spawned under an
// outstanding human changes-requested must never merge — only that human's
// newer review clears the rejection (ADR-0048's ownership rule; the producer
// decides the flag, DecideAutoland case 4).
func TestLaunchLander_approveOnlyOverridesAutoMerge(t *testing.T) {
	f := newFixture(t)
	origin := strings.TrimPrefix(f.repo.RemoteURL, "file://")
	gitCmd(t, f.home, origin, "branch", "afk/7", "main")
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		AutoMerge: store.Set(true),
	}); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.LaunchLander(t.Context(), f.repo.ID, 9, "afk/7", 7, true); err != nil {
		t.Fatalf("LaunchLander: %v", err)
	}
	sess, live := f.runner.Session("proj~lander-7")
	if !live {
		t.Fatal("lander session not live")
	}
	if last := sess.Argv[len(sess.Argv)-1]; last != LanderSeedPrompt(9, "afk/7", false, false) {
		t.Errorf("seed = %q, want the approve-only (autoMerge=false) variant despite repo.AutoMerge=true", last)
	}
}

// TestLaunchEscalate drives the engine-level escalate launch end to end
// (issue #182): kind escalate, the escalate identity (label/session/worktree),
// the lander provider chain, the AFK budget/token rule, and the escalate seed
// — history below the separator — as the trailing spawn positional.
func TestLaunchEscalate(t *testing.T) {
	f := newFixture(t)
	origin := strings.TrimPrefix(f.repo.RemoteURL, "file://")
	gitCmd(t, f.home, origin, "branch", "afk/7", "main")

	if err := f.svc.LaunchEscalate(t.Context(), f.repo.ID, 9, "afk/7", 7, "round history"); err != nil {
		t.Fatalf("LaunchEscalate: %v", err)
	}

	active, err := f.st.ActiveRuns(t.Context())
	if err != nil || len(active) != 1 {
		t.Fatalf("active runs = %v (err %v), want exactly one", active, err)
	}
	run := active[0]
	if run.Kind != store.RunKindEscalate || run.Branch != "afk/7" || run.SessionName != "proj~escalate-7" {
		t.Fatalf("run = kind %s branch %s session %s, want an escalate run on afk/7 as proj~escalate-7",
			run.Kind, run.Branch, run.SessionName)
	}
	if run.IssueNumber == nil || *run.IssueNumber != 7 {
		t.Errorf("issue = %v, want 7", run.IssueNumber)
	}
	if want := f.svc.escalateWorktreePath("proj", 7); run.WorktreePath != want || !strings.HasSuffix(want, "proj-escalate-7") {
		t.Errorf("worktree = %q, want %q (<repo>-escalate-<N>)", run.WorktreePath, want)
	}
	if run.BudgetDeadline == nil || !run.BudgetDeadline.Equal(clockTime.Add(120*time.Minute)) {
		t.Errorf("budget deadline = %v, want the AFK rule's default 120m", run.BudgetDeadline)
	}
	sess, live := f.runner.Session(run.SessionName)
	if !live || sess.Dir != run.WorktreePath {
		t.Fatalf("session live=%v dir=%q, want live in %q", live, sess.Dir, run.WorktreePath)
	}
	if last := sess.Argv[len(sess.Argv)-1]; last != EscalateSeedPrompt(7, 9, "afk/7", "round history") {
		t.Errorf("last spawn argv = %q, want the exact EscalateSeedPrompt as one trailing positional", last)
	}
	// The reaper owns the escalate run: it must be in the active set the
	// reaper enumerates.
	reapable, err := f.st.ActiveAFKRuns(t.Context())
	if err != nil || len(reapable) != 1 || reapable[0].ID != run.ID {
		t.Errorf("ActiveAFKRuns = %v (err %v), want the escalate run", reapable, err)
	}
}

// The escalate launch resolves through the LANDER chain (lander_provider as
// the strict request when set) — never the authoring or AFK chains.
func TestLaunchEscalate_unknownLanderProviderFails(t *testing.T) {
	f := newFixture(t)
	bogus := "no-such-provider"
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{
		LanderProvider: store.Set(&bogus),
	}); err != nil {
		t.Fatal(err)
	}

	err := f.svc.LaunchEscalate(t.Context(), f.repo.ID, 9, "afk/7", 7, "history")
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("err = %v, want the strict unknown-provider refusal", err)
	}
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Errorf("failed escalate launch left %d active runs", len(active))
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
	if err := f.svc.LaunchLander(t.Context(), f.repo.ID, 9, "afk/7", 7, false); err != nil {
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

	err := f.svc.LaunchLander(t.Context(), f.repo.ID, 9, "afk/7", 7, false)
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

	err := f.svc.LaunchLander(t.Context(), f.repo.ID, 9, "afk/7", 7, false)
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("err = %v, want the strict unknown-provider refusal", err)
	}
	if active, _ := f.st.ActiveRuns(t.Context()); len(active) != 0 {
		t.Errorf("failed lander launch left %d active runs", len(active))
	}
}
