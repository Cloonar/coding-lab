package compat

// End-to-end LIVE re-verification of the dialog keystroke recipes (compat.md
// §7 / §Live re-verification; issue #51). These drive the REAL installed
// claude binary in a REAL tmux session through the production send path
// (claudecode.Provider.Reply / AnswerDialog with production keyDelay pacing),
// then read the transcript back and assert the RECORDED answers
// (toolUseResult, compat §5) match the intent — the same comparison the
// verification backstop makes. They exist so the next Claude Code upgrade
// re-verifies the recipes with one command instead of a by-hand TUI session
// (the 2026-07-08 session that pinned §7 found two live recipe bugs).
//
// Opt-in via LAB_COMPAT_LIVE=1 (skipped otherwise — CI stays hermetic); they
// additionally need tmux on PATH and a logged-in claude, cost real model
// calls, and take a minute or two each.
//
// NOTE on capture-pane: the tests poll the tmux pane to know WHEN the picker
// is up before driving it. That is the verification HARNESS observing its own
// probe — production code never scrapes the pane (the never-scrape rule); the
// recipes themselves stay blind sequences.
//
// Side effects on the host: a folder-trust entry for the scratch dir is
// seeded into the real ~/.claude.json (the additive, atomic production
// seeding path — required, or the first spawn blocks on the trust dialog) and
// claude leaves the scratch session's transcript under ~/.claude/projects.
// Both are inert leftovers; the tmux sessions run on a private socket that is
// killed on cleanup.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/claudecode"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// liveRecipeRig is the shared harness: a real provider over a real tmux on a
// private socket, a trusted scratch dir, and the spawned claude session.
type liveRecipeRig struct {
	prov    *claudecode.Provider
	tm      *tmuxx.Tmux
	session string
	dir     string // the scratch cwd (transcripts key off it via the §5 slug)
	home    string
}

func newLiveRecipeRig(t *testing.T, name string, extraArgs ...string) *liveRecipeRig {
	t.Helper()
	if os.Getenv("LAB_COMPAT_LIVE") != "1" {
		t.Skip("set LAB_COMPAT_LIVE=1 to drive the installed claude binary end to end")
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not on PATH: %v", err)
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not on PATH: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}

	socket := fmt.Sprintf("lab-compat-live-%d", os.Getpid())
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	tm := tmuxx.New("tmux", tmuxx.WithSocket(socket))

	prov, err := claudecode.New(claudecode.Options{
		ClaudeBin:   claudeBin,
		ConfigPath:  filepath.Join(home, ".claude.json"),
		RegistryDir: filepath.Join(home, ".claude", "sessions"),
		LoginDir:    home,
		Runner:      tm,
		Bus:         events.NewBus(),
	})
	if err != nil {
		t.Fatalf("claudecode.New: %v", err)
	}
	// Logged-in is a hard prerequisite: an unauthenticated claude never
	// reaches the composer.
	st, _ := prov.AuthStatus(context.Background(), true)
	if !st.LoggedIn {
		t.Skip("claude is not logged in on this machine; the live recipe probe needs real model calls")
	}

	dir := t.TempDir()
	// Production trust seeding (compat §4) so the spawn goes straight to the
	// composer instead of the trust dialog.
	if err := claudecode.SeedTrust(filepath.Join(home, ".claude.json"), dir); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}

	rig := &liveRecipeRig{prov: prov, tm: tm, session: name, dir: dir, home: home}
	argv := append([]string{claudeBin, "--model", "haiku"}, extraArgs...)
	if err := tm.Start(context.Background(), name, dir, argv, nil); err != nil {
		t.Fatalf("tmux start: %v", err)
	}
	t.Cleanup(func() { _ = tm.Stop(context.Background(), name) })

	// Wait for the composer ("? for shortcuts" is the 2.1.198 composer hint).
	// The trust dialog can still appear despite SeedTrust: a PRIOR test's
	// claude exiting rewrites ~/.claude.json from its own startup snapshot and
	// clobbers the just-seeded entry (the documented SeedTrust concurrency
	// caveat — harmless in production, where spawns don't race exits on the
	// same config write). Accept it like an operator would and continue.
	deadline := time.Now().Add(90 * time.Second)
	for {
		pane, err := rig.tm.CapturePane(context.Background(), name)
		if err == nil && strings.Contains(pane, "? for shortcuts") {
			break
		}
		if err == nil && strings.Contains(pane, "Yes, I trust this folder") {
			if err := rig.tm.SendNamedKeys(context.Background(), name, "Enter"); err != nil {
				t.Fatalf("accepting trust dialog: %v", err)
			}
			rig.waitPane(t, 60*time.Second, "? for shortcuts")
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("composer never appeared within 90s; last pane:\n%s", pane)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return rig
}

// waitPane polls capture-pane until it contains needle (see the NOTE in the
// file comment: harness-only observation) and returns the pane content.
func (r *liveRecipeRig) waitPane(t *testing.T, timeout time.Duration, needle string) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		pane, err := r.tm.CapturePane(context.Background(), r.session)
		if err == nil {
			last = pane
			if strings.Contains(pane, needle) {
				return pane
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("pane never showed %q within %s; last pane:\n%s", needle, timeout, last)
	return ""
}

// transcriptPath globs the newest transcript claude created for the scratch
// dir (the §5 slug rule). The registry-based LocateTranscript would work too,
// but the glob keeps the harness independent of registry timing.
func (r *liveRecipeRig) transcriptPath(t *testing.T) string {
	t.Helper()
	pattern := filepath.Join(r.home, ".claude", "projects", claudecode.SlugForDir(r.dir), "*.jsonl")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(pattern)
		var newest string
		var newestMod time.Time
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && fi.ModTime().After(newestMod) {
				newest, newestMod = m, fi.ModTime()
			}
		}
		if newest != "" {
			return newest
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no transcript appeared under %s", pattern)
	return ""
}

// waitRecordedResult polls the transcript for a top-level toolUseResult that
// match(raw) accepts, returning the raw JSON — the recorded ground truth the
// backstop reads (compat §5).
func (r *liveRecipeRig) waitRecordedResult(t *testing.T, timeout time.Duration, match func(raw json.RawMessage) bool) json.RawMessage {
	t.Helper()
	path := r.transcriptPath(t)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				var item struct {
					ToolUseResult json.RawMessage `json:"toolUseResult"`
				}
				if json.Unmarshal([]byte(line), &item) != nil || len(item.ToolUseResult) == 0 {
					continue
				}
				if match(item.ToolUseResult) {
					return item.ToolUseResult
				}
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("no matching toolUseResult landed in %s within %s", path, timeout)
	return nil
}

// sortedLabels normalizes a comma+space-joined multi-select answer to a stable
// order for comparison (the TUI records them in a non-option-index order).
func sortedLabels(s string) string {
	parts := strings.Split(s, ", ")
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}

// TestCompat_Live_askUserQuestionRecipe drives the pinned 2-question form
// (one single-select, one multiSelect — the exact shapes of the 2026-07-08
// fixtures) through the production recipe: per-question pickers, the
// Submit-row commit, the review step. The assertion is the backstop's:
// toolUseResult.answers must record exactly the intended labels.
func TestCompat_Live_askUserQuestionRecipe(t *testing.T) {
	rig := newLiveRecipeRig(t, "lab-compat-live-askq")

	prompt := `Call the AskUserQuestion tool right now, before any other reply or action, with exactly two questions. ` +
		`Question 1: header "Color", question "Which color do you prefer?", multiSelect false, options: {label "Red", description "warm"}, {label "Blue", description "cool"}. ` +
		`Question 2: header "Fruits", question "Which fruits do you like?", multiSelect true, options: {label "Apple", description "crisp"}, {label "Banana", description "soft"}, {label "Cherry", description "tart"}. ` +
		`Use exactly these strings.`
	if err := rig.prov.Reply(context.Background(), rig.session, prompt); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	// Wait for the FIRST question's picker, then answer promptly — the picker
	// self-resolves after 60s (compat §5 afkTimeout).
	rig.waitPane(t, 120*time.Second, "Which color do you prefer?")
	dialog := provider.Dialog{
		Kind: provider.DialogKindQuestion, Prompt: "2 questions", Answerable: true,
		Questions: []provider.Question{
			{Header: "Color", Text: "Which color do you prefer?", Options: []provider.DialogOption{
				{Label: "Red"}, {Label: "Blue"}, {Label: "Other", IsOther: true}}},
			{Header: "Fruits", Text: "Which fruits do you like?", MultiSelect: true, Options: []provider.DialogOption{
				{Label: "Apple"}, {Label: "Banana"}, {Label: "Cherry"}, {Label: "Other", IsOther: true}}},
		},
	}
	answer := provider.DialogAnswer{Answers: []provider.QuestionAnswer{
		{Index: 1},              // Color → Blue
		{Selected: []int{0, 2}}, // Fruits → Apple, Cherry (committed via the Submit row)
	}}
	if err := rig.prov.AnswerDialog(context.Background(), rig.session, dialog, answer); err != nil {
		t.Fatalf("AnswerDialog: %v", err)
	}

	// The recorded ground truth must match the intent exactly.
	raw := rig.waitRecordedResult(t, 90*time.Second, func(raw json.RawMessage) bool {
		var rec struct {
			Answers map[string]string `json:"answers"`
		}
		return json.Unmarshal(raw, &rec) == nil && len(rec.Answers) > 0
	})
	var rec struct {
		Answers map[string]string `json:"answers"`
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("toolUseResult did not decode: %v\n%s", err, raw)
	}
	// The single-select answer is exact; the multi-select answer is compared as
	// a SET — LIVE 2026-07-08, the TUI records multi-select labels in a
	// non-option-index order (toggling Apple then Cherry recorded
	// "Cherry, Apple"), which is why the §5 backstop compares them order-
	// insensitively too.
	if got := rec.Answers["Which color do you prefer?"]; got != "Blue" {
		t.Errorf("color answer = %q; want %q (full: %v)", got, "Blue", rec.Answers)
	}
	if got := sortedLabels(rec.Answers["Which fruits do you like?"]); got != "Apple, Cherry" {
		t.Errorf("fruits answer (sorted) = %q; want %q (full: %v)", got, "Apple, Cherry", rec.Answers)
	}

	// And the production read path maps the resolution onto the tool chip — a
	// transcript-only ReadChat: the live rig has no lab run or runtime dir.
	chat, err := rig.prov.ReadChat("", "", rig.transcriptPath(t))
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	found := false
	for _, m := range chat.Messages {
		if m.Tool != nil && m.Tool.Name == "AskUserQuestion" && strings.Contains(m.Tool.Output, `"Which color do you prefer?"="Blue"`) {
			found = true
		}
	}
	if !found {
		t.Error("ReadChat shows no AskUserQuestion chip with the recorded answers")
	}
}

// TestCompat_Live_exitPlanModeApproval drives plan approval under
// --permission-mode auto (lab's spawn shape, which pins the §7 picker rows):
// prompt a plan, wait for the approval picker, approve via the pinned recipe
// (row 0), and assert the recorded "User has approved your plan" resolution.
func TestCompat_Live_exitPlanModeApproval(t *testing.T) {
	rig := newLiveRecipeRig(t, "lab-compat-live-plan", "--permission-mode", "auto")

	prompt := `Enter plan mode now using the EnterPlanMode tool. Then produce a two-line plan for adding a README.md ` +
		`note to this folder and call ExitPlanMode to present the plan for approval. Do not implement anything before approval.`
	if err := rig.prov.Reply(context.Background(), rig.session, prompt); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	// The plan picker's row 1 is a stable, single-line needle (row 0's label
	// varies with session state, and the prompt "Would you like to proceed?"
	// line-wraps in the pane — both unreliable needles; live 2026-07-08). The
	// dialog Options are lab's own semantic labels (planPickerOptions) — the
	// recipe couples to the index, not the label.
	rig.waitPane(t, 180*time.Second, "Yes, manually approve edits")
	dialog := provider.Dialog{
		Kind: provider.DialogKindPlan, Prompt: "plan", Answerable: true,
		Options: []provider.DialogOption{
			{Label: "Approve — auto-accept edits"},
			{Label: "Approve — review each edit"},
			{Label: "Reject — refine the plan"},
			{Label: "Reject with feedback", IsOther: true},
		},
	}
	if err := rig.prov.AnswerDialog(context.Background(), rig.session, dialog, provider.DialogAnswer{Index: 0}); err != nil {
		t.Fatalf("AnswerDialog: %v", err)
	}

	// Approval records toolUseResult as an OBJECT carrying the plan (compat
	// §5); a rejection would record the denial STRING instead.
	raw := rig.waitRecordedResult(t, 90*time.Second, func(raw json.RawMessage) bool {
		var rec struct {
			Plan string `json:"plan"`
		}
		return raw[0] == '{' && json.Unmarshal(raw, &rec) == nil && rec.Plan != ""
	})
	if len(raw) == 0 {
		t.Fatal("no plan approval recorded")
	}
	chat, err := rig.prov.ReadChat("", "", rig.transcriptPath(t))
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	approved := false
	for _, m := range chat.Messages {
		if m.Tool != nil && m.Tool.Name == "ExitPlanMode" && strings.HasPrefix(m.Tool.Output, "User has approved your plan") {
			approved = true
		}
	}
	if !approved {
		t.Error("ReadChat shows no approved ExitPlanMode chip")
	}
}
