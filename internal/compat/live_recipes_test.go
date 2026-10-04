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
		ClaudeBin:  claudeBin,
		ConfigPath: filepath.Join(home, ".claude.json"),
		LoginDir:   home,
		Runner:     tm,
		Bus:        events.NewBus(),
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
	//
	// The dialog opens with the cursor on "No, exit" (observed on 2.1.265,
	// 2.1.280 and 2.1.284 — compat §4 / the 2.1.280 note), so a bare Enter
	// QUITS claude and the composer never comes: move to "Yes, I trust this
	// folder" first. The two keys go in separate send-keys calls with a pause
	// between them — a Down+Enter burst can act on the pre-move row (an
	// upstream burst-input fix landed only in 2.1.281).
	deadline := time.Now().Add(90 * time.Second)
	for {
		pane, err := rig.tm.CapturePane(context.Background(), name)
		if err == nil && strings.Contains(pane, "? for shortcuts") {
			break
		}
		if err == nil && strings.Contains(pane, "Yes, I trust this folder") {
			if err := rig.tm.SendNamedKeys(context.Background(), name, "Down"); err != nil {
				t.Fatalf("moving to the trust row: %v", err)
			}
			time.Sleep(300 * time.Millisecond)
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
	// Harness-only pane observation (see the file comment): what the TUI
	// shows when the ground truth never lands is the first diagnostic.
	pane, _ := r.tm.CapturePane(context.Background(), r.session)
	t.Fatalf("no matching toolUseResult landed in %s within %s; pane:\n%s", path, timeout, pane)
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

	// And the production read path keeps the resolution ON the dialog message:
	// since issue #56 an answered dialog stays a DIALOG message whose Outcome
	// carries the recorded answers, never a demoted tool chip. Transcript-only
	// ReadChat: the live rig has no lab run or runtime dir.
	chat, err := rig.prov.ReadChat(provider.ReadSpec{TranscriptPath: rig.transcriptPath(t)})
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	found := false
	var seen []provider.DialogOutcome // what the dialogs did carry, for the failure message
	for _, m := range chat.Messages {
		if m.Kind != provider.MessageDialog || m.Dialog == nil ||
			m.Dialog.Kind != provider.DialogKindQuestion || m.Dialog.Outcome == nil {
			continue
		}
		seen = append(seen, *m.Dialog.Outcome)
		// One QuestionResult per question, in DIALOG order. The single-select
		// Chosen is exact; the multi-select one is compared as a SET, for the
		// same recording-order reason as the recorded answer above.
		res := m.Dialog.Outcome.Results
		if len(res) != 2 {
			continue
		}
		fruits := slices.Clone(res[1].Chosen)
		slices.Sort(fruits)
		if res[0].Question == "Which color do you prefer?" && slices.Equal(res[0].Chosen, []string{"Blue"}) &&
			res[1].Question == "Which fruits do you like?" && slices.Equal(fruits, []string{"Apple", "Cherry"}) {
			found = true
		}
	}
	if !found {
		t.Errorf("ReadChat shows no answered AskUserQuestion dialog whose Outcome records Blue and {Apple, Cherry}; outcomes: %+v", seen)
	}
}

// TestCompat_Live_exitPlanModeApproval drives plan approval under
// --permission-mode auto (lab's spawn shape, which pins the §7 picker rows):
// prompt a plan, wait for the approval picker, approve via the pinned recipe
// (row 0), and assert the recorded "User has approved your plan" resolution.
func TestCompat_Live_exitPlanModeApproval(t *testing.T) {
	rig := newLiveRecipeRig(t, "lab-compat-live-plan", "--permission-mode", "auto")

	if err := rig.prov.Reply(context.Background(), rig.session, livePlanPrompt); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	// The plan picker's row 1 is a stable, single-line needle (row 0's label
	// varies with session state, and the prompt "Would you like to proceed?"
	// line-wraps in the pane — both unreliable needles; live 2026-07-08). The
	// dialog Options are lab's own semantic labels (planPickerOptions) — the
	// recipe couples to the index, not the label. Three rows since 2.1.221 (the
	// "refine on the web" row is gone under lab's no-remote spawn) — compat §7.
	rig.waitPane(t, 180*time.Second, "Yes, manually approve edits")
	if err := rig.prov.AnswerDialog(context.Background(), rig.session, livePlanDialog(), provider.DialogAnswer{Index: 0}); err != nil {
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
	chat, err := rig.prov.ReadChat(provider.ReadSpec{TranscriptPath: rig.transcriptPath(t)})
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	// The approval rides the DIALOG message's Outcome (issue #56 — never a
	// demoted tool chip): Approved alone, since a rejection would record the
	// typed Feedback or a bare Dismissed instead.
	approved := false
	var seen []provider.DialogOutcome // what the dialogs did carry, for the failure message
	for _, m := range chat.Messages {
		if m.Kind != provider.MessageDialog || m.Dialog == nil ||
			m.Dialog.Kind != provider.DialogKindPlan || m.Dialog.Outcome == nil {
			continue
		}
		seen = append(seen, *m.Dialog.Outcome)
		if m.Dialog.Outcome.Approved && !m.Dialog.Outcome.Dismissed && m.Dialog.Outcome.Feedback == "" {
			approved = true
		}
	}
	if !approved {
		t.Errorf("ReadChat shows no ExitPlanMode dialog whose Outcome records the approval; outcomes: %+v", seen)
	}
}

// livePlanPrompt asks haiku to raise the ExitPlanMode picker (compat §7) —
// shared by every plan-picker live test so they drive the same shape.
const livePlanPrompt = `Enter plan mode now using the EnterPlanMode tool. Then produce a two-line plan for adding a README.md ` +
	`note to this folder and call ExitPlanMode to present the plan for approval. Do not implement anything before approval.`

// livePlanDialog is the plan dialog as lab renders it: the pinned three-row
// planPickerOptions (index 0/1 approve, index 2 the IsOther feedback row). The
// labels are lab's own; the recipe couples to the INDEX — compat §7.
func livePlanDialog() provider.Dialog {
	return provider.Dialog{
		Kind: provider.DialogKindPlan, Prompt: "plan", Answerable: true,
		Options: []provider.DialogOption{
			{Label: "Approve — auto-accept edits"},
			{Label: "Approve — review each edit"},
			{Label: "Reject with feedback", IsOther: true},
		},
	}
}

// TestCompat_Live_exitPlanModeRows drives the plan picker's OTHER two rows —
// the ones TestCompat_Live_exitPlanModeApproval (index 0, Enter — valid on ANY
// row count) leaves alone. The 2.1.265 bump found the picker had silently lost
// a row on the OUTGOING pin too, and the four-row model's Down×3 "reject with
// feedback" recipe wrapped back onto row 0 and APPROVED (compat §7); that went
// unseen precisely because only index 0 was ever driven by the suite. Now the
// semantic of every pinned index is asserted from the recorded ground truth:
// index 1 (Down,Enter) must record the plan OBJECT (approve); index 2 (Down,
// Down, type-first feedback, Enter) must record the §5 denial STRING carrying
// the typed feedback verbatim after "the user said:\n". Each row gets its own
// fresh spawn — a fresh picker opens on row 0 (the §7 no-climb rule), so a
// recipe is only meaningful from a fresh picker.
func TestCompat_Live_exitPlanModeRows(t *testing.T) {
	const feedback = "tighten the tests"
	cases := []struct {
		name    string
		answer  provider.DialogAnswer
		approve bool
	}{
		{name: "index1-approve-review-each-edit", answer: provider.DialogAnswer{Index: 1}, approve: true},
		{name: "index2-reject-with-feedback", answer: provider.DialogAnswer{Index: 2, OtherText: feedback}, approve: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newLiveRecipeRig(t, "lab-compat-live-"+tc.name, "--permission-mode", "auto")
			if err := rig.prov.Reply(context.Background(), rig.session, livePlanPrompt); err != nil {
				t.Fatalf("Reply: %v", err)
			}
			rig.waitPane(t, 180*time.Second, "Yes, manually approve edits")
			if err := rig.prov.AnswerDialog(context.Background(), rig.session, livePlanDialog(), tc.answer); err != nil {
				t.Fatalf("AnswerDialog: %v", err)
			}

			if tc.approve {
				// Approval: toolUseResult is the plan OBJECT (compat §5).
				rig.waitRecordedResult(t, 90*time.Second, func(raw json.RawMessage) bool {
					var rec struct {
						Plan string `json:"plan"`
					}
					return raw[0] == '{' && json.Unmarshal(raw, &rec) == nil && rec.Plan != ""
				})
			} else {
				// Rejection: toolUseResult is the denial STRING with the typed
				// feedback riding inside it (compat §5) — a plan OBJECT here
				// would be the reject→approve inversion §7 guards against.
				raw := rig.waitRecordedResult(t, 90*time.Second, func(raw json.RawMessage) bool {
					var s string
					return raw[0] == '"' && json.Unmarshal(raw, &s) == nil && strings.Contains(s, feedback)
				})
				var denial string
				_ = json.Unmarshal(raw, &denial)
				if !strings.Contains(denial, "the user said:\n"+feedback) {
					t.Errorf("denial string does not carry the feedback marker + typed text:\n%s", denial)
				}
			}

			// The production read path must agree with the ground truth: the
			// DIALOG message's Outcome carries the approval, or the feedback.
			chat, err := rig.prov.ReadChat(provider.ReadSpec{TranscriptPath: rig.transcriptPath(t)})
			if err != nil {
				t.Fatalf("ReadChat: %v", err)
			}
			found := false
			var seen []provider.DialogOutcome
			for _, m := range chat.Messages {
				if m.Kind != provider.MessageDialog || m.Dialog == nil ||
					m.Dialog.Kind != provider.DialogKindPlan || m.Dialog.Outcome == nil {
					continue
				}
				o := *m.Dialog.Outcome
				seen = append(seen, o)
				if tc.approve && o.Approved && !o.Dismissed && o.Feedback == "" {
					found = true
				}
				if !tc.approve && !o.Approved && o.Feedback == feedback {
					found = true
				}
			}
			if !found {
				t.Errorf("ReadChat shows no ExitPlanMode dialog whose Outcome matches (approve=%v, feedback=%q); outcomes: %+v", tc.approve, feedback, seen)
			}
		})
	}
}

// TestCompat_Live_chatAboutThisRow drives issue #58's "Chat about this"
// recipe (compat §7 "Chat about this") through the production AnswerDialog
// path with production pacing: a downward walk onto the picker's own trailing
// "Chat about this" row and Enter — after the operator's earlier answers on a
// multi-question form — and nothing else. The recipe was written from the §7
// row models and has NOT been driven live yet, so this test's first green run
// IS the live verification the compat entry is waiting on. Per shape it
// asserts:
//
//	(1) the dialog RESOLVES — a picker still pending means the walk landed on
//	    an option row of a form (the Enter answered that question and the
//	    form moved on) or on nothing at all;
//	(2) it resolves through the dismissed summary — a Results outcome means
//	    the walk landed on an option and answered the question;
//	(3) no backstop warning is emitted.
//
// It logs the tool_result line the row leaves behind — the transcript shape
// §7 still has to pin, including what it says about the EARLIER answers of a
// form — and the pane afterwards, which shows whether claude went on to ask
// what the operator wants to know.
func TestCompat_Live_chatAboutThisRow(t *testing.T) {
	other := provider.DialogOption{Label: "Other", IsOther: true}
	const formPrompt = `Call the AskUserQuestion tool right now, before any other reply or action, with exactly two questions. ` +
		`Question 1: header "Color", question "Which color do you prefer?", multiSelect false, options: {label "Red", description "warm"}, {label "Blue", description "cool"}. ` +
		`Question 2: header "Fruits", question "Which fruits do you like?", multiSelect true, options: {label "Apple", description "crisp"}, {label "Banana", description "soft"}, {label "Cherry", description "tart"}. ` +
		`Use exactly these strings.`
	form := provider.Dialog{Kind: provider.DialogKindQuestion, Prompt: "2 questions", Answerable: true,
		Questions: []provider.Question{
			{Header: "Color", Text: "Which color do you prefer?", Options: []provider.DialogOption{{Label: "Red"}, {Label: "Blue"}, other}},
			{Header: "Fruits", Text: "Which fruits do you like?", MultiSelect: true, Options: []provider.DialogOption{
				{Label: "Apple"}, {Label: "Banana"}, {Label: "Cherry"}, other}},
		}}
	shapes := []struct {
		name   string
		prompt string
		needle string // the first picker's question text, to know WHEN it is up
		dialog provider.Dialog
		answer provider.DialogAnswer
	}{
		{
			name: "single-select",
			prompt: `Call the AskUserQuestion tool right now, before any other reply or action, with exactly one question: ` +
				`header "Pet", question "Favorite pet?", multiSelect false, options: {label "Dog", description "loyal"}, {label "Cat", description "aloof"}. ` +
				`Use exactly these strings.`,
			needle: "Favorite pet?",
			dialog: provider.Dialog{Kind: provider.DialogKindQuestion, Prompt: "Favorite pet?", Answerable: true,
				Options: []provider.DialogOption{{Label: "Dog"}, {Label: "Cat"}, other}},
			answer: provider.DialogAnswer{Chat: true},
		},
		{
			name: "multi-select",
			prompt: `Call the AskUserQuestion tool right now, before any other reply or action, with exactly one question: ` +
				`header "Toppings", question "Which toppings?", multiSelect true, options: {label "Olives", description "salty"}, {label "Onions", description "sharp"}. ` +
				`Use exactly these strings.`,
			needle: "Which toppings?",
			dialog: provider.Dialog{Kind: provider.DialogKindQuestion, Prompt: "Which toppings?", Answerable: true, Multi: true,
				Options: []provider.DialogOption{{Label: "Olives"}, {Label: "Onions"}, other}},
			answer: provider.DialogAnswer{Chat: true},
		},
		{
			name:   "form-chat-on-first-question",
			prompt: formPrompt, needle: "Which color do you prefer?", dialog: form,
			answer: provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Chat: true}}},
		},
		{
			// The maintainer's example: second option on the first question,
			// chat about the second.
			name:   "form-answer-then-chat-on-second-question",
			prompt: formPrompt, needle: "Which color do you prefer?", dialog: form,
			answer: provider.DialogAnswer{Answers: []provider.QuestionAnswer{{Index: 1}, {Chat: true}}},
		},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			rig := newLiveRecipeRig(t, "lab-compat-live-chat-"+shape.name)
			if err := rig.prov.Reply(context.Background(), rig.session, shape.prompt); err != nil {
				t.Fatalf("Reply: %v", err)
			}
			rig.waitPane(t, 120*time.Second, shape.needle)

			if err := rig.prov.AnswerDialog(context.Background(), rig.session, shape.dialog, shape.answer); err != nil {
				t.Fatalf("AnswerDialog(chat): %v", err)
			}

			// Poll the production read until the dialog shows up RESOLVED,
			// then check how it resolved and renders.
			deadline := time.Now().Add(90 * time.Second)
			for {
				path := rig.transcriptPath(t)
				chat, err := rig.prov.ReadChat(provider.ReadSpec{TranscriptPath: path})
				if err != nil {
					t.Fatalf("ReadChat: %v", err)
				}
				var outcome *provider.DialogOutcome
				for _, m := range chat.Messages {
					if m.Kind == provider.MessageDialog && m.Dialog != nil && m.Dialog.Kind == provider.DialogKindQuestion && m.Dialog.Outcome != nil {
						outcome = m.Dialog.Outcome
					}
					if m.Kind == provider.MessageLifecycle && m.Error && strings.HasPrefix(m.Text, "Dialog answer may not have landed") {
						t.Errorf("the row selection emitted a backstop warning: %q", m.Text)
					}
				}
				if outcome != nil {
					if !outcome.Dismissed {
						t.Errorf("dialog outcome = %+v; want the dismissed summary — a Results outcome means the walk picked an option", outcome)
					}
					// The shape the row records is the pin this run captures
					// (compat §7): log every tool_result line for the notes.
					if b, err := os.ReadFile(path); err == nil {
						for _, line := range strings.Split(string(b), "\n") {
							if strings.Contains(line, `"tool_result"`) {
								t.Logf("recorded tool_result line: %s", line)
							}
						}
					}
					// Give claude a moment, then record what it did next —
					// the expectation is that it asks what to clarify.
					time.Sleep(10 * time.Second)
					pane, _ := rig.tm.CapturePane(context.Background(), rig.session)
					t.Logf("pane after the row was chosen:\n%s", pane)
					return
				}
				if time.Now().After(deadline) {
					pane, _ := rig.tm.CapturePane(context.Background(), rig.session)
					t.Fatalf("the dialog never resolved within 90s — the walk did not land on \"Chat about this\"; pane:\n%s", pane)
				}
				time.Sleep(time.Second)
			}
		})
	}
}
