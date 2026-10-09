package claudecode

// Transcript location (registry cwd-match → slug/sessionId path), the
// state-derivation edges the compat fixture doesn't cover, and ReadChat's
// adapter-owned conversational-state composition (issue #92) — the transcript
// fold overlaid with the hook spool's pending dialog and the session
// registry's status (issue #79's layered precedence). The full JSONL → schema
// mapping is pinned in internal/compat against a captured fixture.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// Inline dialog tool_use events (live 2.1.198 input shapes, compat §5/§7) and
// their resolution lines, shared by the outcome and state-edge tests. All key
// tool ids are "d1" so one result line pairs with any use line.
const (
	colorUseLine  = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"d1","name":"AskUserQuestion","input":{"questions":[{"question":"Which color do you prefer?","header":"Color","multiSelect":false,"options":[{"label":"Red","description":"warm"},{"label":"Blue","description":"cool"}]}]}}]}}`
	fruitsUseLine = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"d1","name":"AskUserQuestion","input":{"questions":[{"question":"Which fruits do you like?","header":"Fruits","multiSelect":true,"options":[{"label":"Apple","description":"crisp"},{"label":"Banana","description":"soft"},{"label":"Cherry","description":"tart"}]}]}}]}}`
	twoQUseLine   = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"d1","name":"AskUserQuestion","input":{"questions":[{"question":"Which color do you prefer?","header":"Color","multiSelect":false,"options":[{"label":"Red","description":"warm"},{"label":"Blue","description":"cool"}]},{"question":"Which fruits do you like?","header":"Fruits","multiSelect":true,"options":[{"label":"Apple","description":"crisp"},{"label":"Banana","description":"soft"},{"label":"Cherry","description":"tart"}]}]}}]}}`
	planUseLine   = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"d1","name":"ExitPlanMode","input":{"plan":"# The plan\n\n- do the thing"}}]}}`
)

// resolutionLine renders the user event that flushes d1's tool_result, with
// toolUseResult as the top-level ground truth (compat §5); denial adds the
// user-rejected stamp that accompanies every decline on 2.1.198.
func resolutionLine(toolUseResult string, denial bool) string {
	l := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"resolved","tool_use_id":"d1"}]},"toolUseResult":` + toolUseResult
	if denial {
		l += `,"toolDenialKind":"user-rejected"`
	}
	return l + `}`
}

// Pending-work marker lines (issue #159), mined live from 2.1.198–2.1.206
// transcripts (long values shortened, structure and key sets intact): the
// async Agent and Workflow launch pairs that ADD to the pending set, their
// structural foils that must NOT (background Bash — backgroundTaskId, no
// status; a synchronous Agent call — status "completed" + totalDurationMs),
// and the TaskStop / SendMessage pairs that remove and re-add. Ids are
// consistent across lines so launches pair with their notifications.
const (
	pendingAgentID = "a66020d7eda3c3c35"

	agentUseLine         = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_A1","name":"Agent","input":{"description":"bg probe","prompt":"Implement the export backend feature.","subagent_type":"general-purpose","model":"opus"},"caller":{"type":"direct"}}]}}`
	agentAsyncResultLine = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_A1","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully. agentId: a66020d7eda3c3c35 (internal ID). The agent is working in the background."}]}]},"toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"a66020d7eda3c3c35","description":"bg probe","resolvedModel":"claude-opus-4-8[1m]","prompt":"Implement the export backend feature."}}`

	workflowUseLine         = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_W1","name":"Workflow","input":{"script":"export const meta = { name: 'fixture-probe' }"},"caller":{"type":"direct"}}]}}`
	workflowAsyncResultLine = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_W1","type":"tool_result","content":"Workflow launched in background. Task ID: we3dnh19f","is_error":false}]},"toolUseResult":{"status":"async_launched","taskId":"we3dnh19f","taskType":"local_workflow","workflowName":"fixture-probe","runId":"wf_490e165a-e59","summary":"probe"}}`

	bashBgUseLine    = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_B1","name":"Bash","input":{"command":"go test ./...","description":"Run test baseline","run_in_background":true},"caller":{"type":"direct"}}]}}`
	bashBgResultLine = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_B1","type":"tool_result","content":"Command running in background with ID: b07izrfyc. You will be notified when it completes.","is_error":false}]},"toolUseResult":{"stdout":"","stderr":"","interrupted":false,"isImage":false,"noOutputExpected":false,"backgroundTaskId":"b07izrfyc"}}`

	syncAgentUseLine    = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_S1","name":"Agent","input":{"description":"Explore the fold","prompt":"Find the fold.","subagent_type":"Explore"},"caller":{"type":"direct"}}]}}`
	syncAgentResultLine = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_S1","type":"tool_result","content":[{"type":"text","text":"final report"}]}]},"toolUseResult":{"status":"completed","agentId":"ad53a61667af00037","agentType":"Explore","resolvedModel":"claude-opus-4-8[1m]","totalDurationMs":244268,"totalTokens":100132,"totalToolUseCount":28,"prompt":"Find the fold."}}`

	taskStopUseLine    = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_T1","name":"TaskStop","input":{"task_id":"a66020d7eda3c3c35"},"caller":{"type":"direct"}}]}}`
	taskStopResultLine = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_T1","type":"tool_result","content":"{\"message\":\"Successfully stopped task: a66020d7eda3c3c35 (bg probe)\"}"}]},"toolUseResult":{"message":"Successfully stopped task: a66020d7eda3c3c35 (bg probe)","task_id":"a66020d7eda3c3c35","task_type":"local_agent","command":"bg probe"}}`

	sendMessageUseLine    = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_M1","name":"SendMessage","input":{"to":"a66020d7eda3c3c35","summary":"probe resume","message":"Reply with exactly: ok"},"caller":{"type":"direct"}}]}}`
	sendMessageResultLine = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_M1","type":"tool_result","content":[{"type":"text","text":"resumed"}]}]},"toolUseResult":{"success":true,"message":"Agent \"a66020d7eda3c3c35\" was stopped (completed); resumed it in the background with your message.","resumedAgentId":"a66020d7eda3c3c35","pin":{"id":"a66020d7eda3c3c35","name":"a66020d7eda3c3c35","ref":"197887"}}}`

	turnEndTextLine = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Launched; reporting status."}]}}`
	apiErrorLine    = `{"type":"assistant","isApiErrorMessage":true,"message":{"role":"assistant","content":[{"type":"text","text":"API Error: 529 overloaded"}]}}`
)

// taskNotification renders the payload every notification carrier shares
// (live shapes: statuses completed|failed|killed; the escaped \n and \" are
// JSON-string escapes, exactly as claude writes them). status "" renders the
// Monitor interim <event> form, which carries NO <status> tag — the shape
// that must never remove a pending id.
func taskNotification(id, status string) string {
	if status == "" {
		return `<task-notification>\n<task-id>` + id + `</task-id>\n<summary>Monitor event: \"probe\"</summary>\n<event>PROBE_UP</event>\n</task-notification>`
	}
	return `<task-notification>\n<task-id>` + id + `</task-id>\n<tool-use-id>toolu_A1</tool-use-id>\n<status>` + status + `</status>\n<summary>Agent \"bg probe\" finished</summary>\n</task-notification>`
}

// The three notification carriers (issue #159, compat §5): the queue-operation
// bookkeeping that always precedes delivery, the between-turns standalone user
// message (plain-string content, origin task-notification, no isMeta), and
// the mid-turn attachment injection (queued_command, commandMode
// task-notification).
func enqueueLine(payload string) string {
	return `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-07-11T09:49:25.949Z","sessionId":"sess","content":"` + payload + `"}`
}

func userNotifyLine(payload string) string {
	return `{"type":"user","message":{"role":"user","content":"` + payload + `"},"origin":{"kind":"task-notification"},"promptSource":"system"}`
}

func attachNotifyLine(payload string) string {
	return `{"type":"attachment","attachment":{"type":"queued_command","commandMode":"task-notification","prompt":"` + payload + `","timestamp":"2026-07-12T02:59:20.258Z"}}`
}

func chatProvider(t *testing.T) *Provider {
	t.Helper()
	p, err := New(Options{
		ClaudeBin: "claude", ConfigPath: filepath.Join(t.TempDir(), ".claude.json"),
		LoginDir: t.TempDir(),
		Runner:   tmuxx.NewFake(), Bus: events.NewBus(),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// The instance registry + transcript tree live under the run's private HOME
// (issue #202): <home>/.claude/sessions and <home>/.claude/projects.
func TestLocateTranscript_registryMatch(t *testing.T) {
	home := t.TempDir()
	registryDir := registryDirUnder(home)
	if err := os.MkdirAll(registryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	worktree := "/home/op/state/worktrees/proj-manual-1"
	sessionID := "abcd-1234"

	// A live registry entry (this test process) whose cwd is the worktree.
	writeJSONFile(t, filepath.Join(registryDir, "1.json"), map[string]any{
		"pid": os.Getpid(), "cwd": worktree, "startedAt": 1, "sessionId": sessionID,
		"bridgeSessionId": "session_x",
	})
	// The transcript file at the slug path under the instance projects tree.
	dir := filepath.Join(projectsDirUnder(home), SlugForDir(worktree))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := chatProvider(t)
	got, err := p.LocateTranscript(context.Background(), "proj~manual-1", worktree, home)
	if err != nil {
		t.Fatalf("LocateTranscript: %v", err)
	}
	if got != path {
		t.Errorf("LocateTranscript = %q; want %q", got, path)
	}

	// A worktree with no registry entry misses cleanly (no error).
	got, err = p.LocateTranscript(context.Background(), "proj~other", "/no/such/worktree", home)
	if err != nil || got != "" {
		t.Errorf("miss = (%q, %v); want (\"\", nil)", got, err)
	}

	// An empty home is a miss too — no per-run home ⇒ no instance registry to
	// read, and never a fall back to the master store (issue #202).
	if got, err := p.LocateTranscript(context.Background(), "proj~manual-1", worktree, ""); err != nil || got != "" {
		t.Errorf("homeless locate = (%q, %v); want (\"\", nil)", got, err)
	}
}

// /clear rotation via the registry only (issue #79 decision 5): on 2.1.284 the
// registry's sessionId rotates at clear time but the fresh transcript is
// written only with the next message, so LocateTranscript returns the
// computed path even though the file does not exist yet — core adopts the
// rotation at once instead of staying on the old file's needs_input tail.
func TestLocateTranscript_returnsPathBeforeFileExists(t *testing.T) {
	home := t.TempDir()
	worktree := "/home/op/state/worktrees/proj-manual-1"
	writeHomeEntry(t, home, "1.json", liveEntry(worktree, "fresh-after-clear", "idle", "", 1))

	got, err := chatProvider(t).LocateTranscript(context.Background(), "proj~manual-1", worktree, home)
	if err != nil {
		t.Fatalf("LocateTranscript: %v", err)
	}
	want := filepath.Join(projectsDirUnder(home), SlugForDir(worktree), "fresh-after-clear.jsonl")
	if got != want {
		t.Errorf("LocateTranscript = %q; want the computed path %q", got, want)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("fixture invariant: the transcript must not exist yet (stat err %v)", err)
	}
}

// writeHomeEntry writes one session registry file under the instance HOME
// (<home>/.claude/sessions/<name>, compat §2).
func writeHomeEntry(t *testing.T, home, name string, e RegistryEntry) {
	t.Helper()
	dir := registryDirUnder(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeRegistryEntry(t, dir, name, e)
}

// liveEntry renders a registry entry owned by THIS test process (os.Getpid,
// so pidAlive passes) with the 2.1.284 status fields (issue #79).
func liveEntry(cwd, sessionID, status, waitingFor string, startedAt int64) RegistryEntry {
	return RegistryEntry{
		PID: os.Getpid(), Cwd: cwd, StartedAt: startedAt, SessionID: sessionID,
		Status: status, WaitingFor: waitingFor, StatusUpdatedAt: startedAt + 1,
	}
}

// Entry selection (issue #79 decision 3): transcript identity and status come
// from ONE entry — cwd match, live pid, newest startedAt, non-empty sessionId.
// A dead pid's leftover, a sibling cwd, and a sessionId-less entry are all
// skipped even when they are newer or carry a louder status.
func TestRegistryEntryForDir_selection(t *testing.T) {
	home := t.TempDir()
	const wt = "/wt/run"
	writeHomeEntry(t, home, "a.json", liveEntry(wt, "older", "busy", "", 100))
	writeHomeEntry(t, home, "b.json", liveEntry(wt, "newest-live", "idle", "", 200))
	dead := liveEntry(wt, "dead-leftover", "waiting", "input needed", 900)
	dead.PID = deadPID(t)
	writeHomeEntry(t, home, "c.json", dead)
	writeHomeEntry(t, home, "d.json", liveEntry("/wt/other", "wrong-cwd", "waiting", "dialog open", 950))
	writeHomeEntry(t, home, "e.json", liveEntry(wt, "", "busy", "", 999)) // no sessionId yet
	if err := os.WriteFile(filepath.Join(registryDirUnder(home), "f.json"), []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	e, ok := registryEntryForDir(registryDirUnder(home), wt)
	if !ok || e.SessionID != "newest-live" || e.Status != "idle" {
		t.Fatalf("registryEntryForDir = %+v,%v; want the newest LIVE same-cwd entry newest-live/idle", e, ok)
	}

	// LocateTranscript and ReadChat agree on that one entry: the transcript is
	// newest-live's, and its idle status leaves the fold in charge — the older
	// entry's busy and the dead/foreign entries' waiting never leak in.
	p := chatProvider(t)
	path, err := p.LocateTranscript(context.Background(), "s", wt, home)
	if err != nil || filepath.Base(path) != "newest-live.jsonl" {
		t.Fatalf("LocateTranscript = (%q, %v); want newest-live.jsonl", path, err)
	}
	writeTranscript(t, path, foldNeedsInputTail)
	chat, err := p.ReadChat(provider.ReadSpec{RunID: "run_1", RuntimeDir: t.TempDir(), Home: home, Worktree: wt, TranscriptPath: path})
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	if chat.State != provider.StateNeedsInput || chat.StateDetail != "" {
		t.Errorf("chat = {state:%q detail:%q}; want the fold's needs_input with no detail (idle registry)", chat.State, chat.StateDetail)
	}

	// No live entry at all → unusable.
	if _, ok := registryEntryForDir(registryDirUnder(home), "/wt/none"); ok {
		t.Error("an unmatched cwd must select nothing")
	}
}

// writeTranscript writes body at path, creating its parent dirs.
func writeTranscript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A non-empty transcriptPath that does not exist splits on the read kind
// (issue #79 decision 5). ENDED reads (no RuntimeDir, no Home) keep
// ErrTranscriptGone — the transcript read gates the overlay, so a retired file
// never resurrects as a signal-only chat. ACTIVE reads treat it as the fresh
// post-/clear identity LocateTranscript names before claude writes it: an idle
// empty chat, never an error, with the overlays still composed on top.
func TestReadChat_missingTranscript(t *testing.T) {
	p := chatProvider(t)
	const wt = "/wt/run"
	absent := filepath.Join(t.TempDir(), "sess-live.jsonl")

	// Ended: ErrTranscriptGone, even with a worktree on the spec (a worktree
	// alone never makes a read active).
	if _, err := p.ReadChat(provider.ReadSpec{TranscriptPath: absent}); err != provider.ErrTranscriptGone {
		t.Errorf("ended ReadChat(absent) err = %v; want ErrTranscriptGone", err)
	}
	if _, err := p.ReadChat(provider.ReadSpec{TranscriptPath: absent, Worktree: wt}); err != provider.ErrTranscriptGone {
		t.Errorf("ended ReadChat(absent, worktree) err = %v; want ErrTranscriptGone", err)
	}

	// Active, no signals: the idle empty conversation.
	for name, spec := range map[string]provider.ReadSpec{
		"runtime dir": {RunID: "run_1", RuntimeDir: t.TempDir(), TranscriptPath: absent, Model: "fable"},
		"home only":   {RunID: "run_1", Home: t.TempDir(), Worktree: wt, TranscriptPath: absent, Model: "fable"},
	} {
		chat, err := p.ReadChat(spec)
		if err != nil {
			t.Fatalf("%s: active ReadChat(absent) err = %v; want nil", name, err)
		}
		if chat.State != provider.StateIdle || len(chat.Messages) != 0 || chat.Cursor != 0 ||
			chat.PendingDialog != nil || chat.ContextUsage != nil || chat.StateDetail != "" {
			t.Errorf("%s: chat = %+v; want an idle empty chat", name, chat)
		}
	}

	// Active, overlays still apply: a live spool dialog surfaces …
	dir := t.TempDir()
	writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)
	chat, err := p.ReadChat(provider.ReadSpec{RunID: "run_1", RuntimeDir: dir, TranscriptPath: absent})
	if err != nil || chat.State != provider.StateQuestion || chat.PendingDialog == nil {
		t.Errorf("active absent + spool = %+v, %v; want StateQuestion with the PendingDialog", chat, err)
	}
	// … and so does the registry status.
	home := t.TempDir()
	writeHomeEntry(t, home, "1.json", liveEntry(wt, "sess-live", "busy", "", 1))
	chat, err = p.ReadChat(provider.ReadSpec{RunID: "run_1", RuntimeDir: t.TempDir(), Home: home, Worktree: wt, TranscriptPath: absent})
	if err != nil || chat.State != provider.StateWorking {
		t.Errorf("active absent + registry busy = %+v, %v; want StateWorking", chat, err)
	}
}

// The composition core (issue #92): a live spool dialog wins over whatever the
// transcript derives, surfaces on the side-channel Chat.PendingDialog with
// StateQuestion, and NEVER enters Messages — the stream stays purely
// transcript-derived so seq numbers are reparse-stable when the real tool_use
// retro-flushes (issues #89/#90).
func TestReadChat_spoolDialogOverridesTranscript(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)
	// A same-session transcript (filename stem = the spool's session_id, so the
	// rotation-staleness check passes) whose tail alone would derive
	// needs_input; the spool's tool_use_id is absent, so the dialog is pending.
	transcript := filepath.Join(dir, "sess-live.jsonl")
	if err := os.WriteFile(transcript,
		[]byte(line(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)), 0o600); err != nil {
		t.Fatal(err)
	}
	chat, err := p.ReadChat(provider.ReadSpec{RunID: "run_1", RuntimeDir: dir, TranscriptPath: transcript})
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	if chat.State != provider.StateQuestion {
		t.Errorf("state = %q; want %q (the live dialog wins)", chat.State, provider.StateQuestion)
	}
	if chat.PendingDialog == nil || chat.PendingDialog.ToolID != "toolu_ABC" {
		t.Fatalf("PendingDialog = %+v; want the spool dialog toolu_ABC", chat.PendingDialog)
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Kind != provider.MessageText {
		t.Fatalf("messages = %+v; want only the transcript text — the live dialog must not leak into the stream", chat.Messages)
	}
}

// A pending dialog can exist before LocateTranscript first hits (the hooks
// spool the instant the picker is about to show), so a transcript-less read of
// an active run still consults the spool; without one it is an idle empty
// chat, never an error (issue #92).
func TestReadChat_spoolDialogBeforeTranscriptLocated(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)
	chat, err := p.ReadChat(provider.ReadSpec{RunID: "run_1", RuntimeDir: dir})
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	if chat.State != provider.StateQuestion || chat.PendingDialog == nil {
		t.Fatalf("chat = %+v; want StateQuestion with the pending dialog before the transcript is located", chat)
	}
	if len(chat.Messages) != 0 || chat.Cursor != 0 {
		t.Errorf("base chat = %+v; want empty messages and cursor 0", chat)
	}

	// No spool either: the idle empty base, no error.
	empty, err := p.ReadChat(provider.ReadSpec{RunID: "run_none", RuntimeDir: dir})
	if err != nil {
		t.Fatalf("ReadChat(no transcript, no spool): %v", err)
	}
	if empty.State != provider.StateIdle || empty.PendingDialog != nil || len(empty.Messages) != 0 {
		t.Errorf("chat = %+v; want an idle empty chat", empty)
	}
}

// runtimeDir == "" and home == "" is the transcript-only degradation (ended
// runs, or nothing configured — the same gate core's liveSignals() applied):
// the spool on disk is never consulted, state comes from the transcript alone
// (issues #92/#79).
func TestReadChat_emptyRuntimeDirReadsTranscriptOnly(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)
	// A user-text tail derives working — distinct from the dialog's question,
	// so any overlay would be visible.
	transcript := filepath.Join(dir, "sess-live.jsonl")
	if err := os.WriteFile(transcript,
		[]byte(line(`{"type":"user","message":{"role":"user","content":"go"}}`)), 0o600); err != nil {
		t.Fatal(err)
	}
	chat, err := p.ReadChat(provider.ReadSpec{RunID: "run_1", TranscriptPath: transcript})
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	if chat.State != provider.StateWorking || chat.PendingDialog != nil {
		t.Errorf("chat = %+v; want the transcript's working state with no dialog (signals off)", chat)
	}
}

// The dormant fallback (issue #92, unchanged from core): a transcript whose
// tail is a pending flushed tool_use derives StateQuestion on its own — the
// dialog stays a MESSAGE and PendingDialog stays nil, so a provider (or
// window) without a spool still surfaces the question in the stream.
func TestReadChat_transcriptDialogDormantFallback(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir() // runtime dir armed, but no spool for the run
	transcript := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(transcript, []byte(line(colorUseLine)), 0o600); err != nil {
		t.Fatal(err)
	}
	chat, err := p.ReadChat(provider.ReadSpec{RunID: "run_1", RuntimeDir: dir, TranscriptPath: transcript})
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	if chat.State != provider.StateQuestion {
		t.Errorf("state = %q; want %q (transcript-derived question stands)", chat.State, provider.StateQuestion)
	}
	if chat.PendingDialog != nil {
		t.Errorf("PendingDialog = %+v; want nil — a transcript-flushed dialog stays in Messages", chat.PendingDialog)
	}
	if len(chat.Messages) != 1 || chat.Messages[0].Kind != provider.MessageDialog {
		t.Errorf("messages = %+v; want the dialog message in the stream", chat.Messages)
	}
}

// Transcript fold tails for the precedence table (issue #79): each derives a
// distinct fold state on its own.
var (
	foldWorkingTail    = line(`{"type":"user","message":{"role":"user","content":"go"}}`)
	foldNeedsInputTail = line(`{"type":"user","message":{"role":"user","content":"go"}}`) +
		line(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
	// The issue #159 pending-work hold: an async agent launch then a
	// turn-ending text — the fold says working.
	foldHoldTail     = line(agentUseLine) + line(agentAsyncResultLine) + line(turnEndTextLine)
	foldQuestionTail = line(colorUseLine) // a transcript-flushed pending dialog
)

// The layered precedence (issue #79 decision 1), composed inside ReadChat:
// (1) spool dialog / transcript-flushed dialog → question; (2) registry
// waiting → needs_input; (3) registry busy → working, overriding the fold;
// (4) registry idle → the fold; (5) unusable registry → the fold alone.
// StateDetail = waitingFor whenever the registry says waiting and the state
// is question/needs_input.
func TestReadChat_registryPrecedence(t *testing.T) {
	const (
		wt     = "/wt/run"
		reason = "Claude needs your permission to use Bash"
	)
	cases := []struct {
		name       string
		fold       string // transcript body
		spool      bool   // a live PreToolUse dialog spool
		status     string // registry status; "-" = no registry entry at all
		waitingFor string
		noWorktree bool // the spec carries no Worktree
		wantState  string
		wantDetail string
		wantDialog bool
	}{
		// Layer 3: busy overrides the fold — the background-subagent fix.
		{name: "fold working + busy", fold: foldWorkingTail, status: "busy", wantState: provider.StateWorking},
		{name: "fold needs_input + busy", fold: foldNeedsInputTail, status: "busy", wantState: provider.StateWorking},
		// Layer 4: idle hands the decision to the fold, hold included.
		{name: "pending-work hold + idle", fold: foldHoldTail, status: "idle", wantState: provider.StateWorking},
		{name: "fold needs_input + idle", fold: foldNeedsInputTail, status: "idle", wantState: provider.StateNeedsInput},
		{name: "empty fold + idle never needs_input", fold: "", status: "idle", wantState: provider.StateIdle},
		// Layer 2: waiting with nothing composed → needs_input + the reason.
		{name: "waiting, no spool", fold: foldNeedsInputTail, status: "waiting", waitingFor: reason,
			wantState: provider.StateNeedsInput, wantDetail: reason},
		{name: "waiting over a working fold", fold: foldWorkingTail, status: "waiting", waitingFor: "  " + reason + " ",
			wantState: provider.StateNeedsInput, wantDetail: reason},
		// Layer 1 wins, the waiting reason rides along.
		{name: "waiting + spool dialog", fold: foldWorkingTail, spool: true, status: "waiting", waitingFor: "dialog open",
			wantState: provider.StateQuestion, wantDetail: "dialog open", wantDialog: true},
		{name: "waiting + transcript dialog", fold: foldQuestionTail, status: "waiting", waitingFor: "dialog open",
			wantState: provider.StateQuestion, wantDetail: "dialog open"},
		{name: "spool dialog + busy", fold: foldWorkingTail, spool: true, status: "busy",
			wantState: provider.StateQuestion, wantDialog: true},
		{name: "transcript dialog + busy", fold: foldQuestionTail, status: "busy", wantState: provider.StateQuestion},
		// Layer 5: unusable status → the fold alone, no detail.
		{name: "unknown status", fold: foldNeedsInputTail, status: "thinking", waitingFor: reason, wantState: provider.StateNeedsInput},
		{name: "unknown status, working fold", fold: foldWorkingTail, status: "sleeping", wantState: provider.StateWorking},
		{name: "empty status", fold: foldNeedsInputTail, status: "", wantState: provider.StateNeedsInput},
		{name: "no registry entry", fold: foldNeedsInputTail, status: "-", wantState: provider.StateNeedsInput},
		{name: "no worktree on the spec", fold: foldNeedsInputTail, status: "busy", noWorktree: true, wantState: provider.StateNeedsInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := spoolTestProvider(t)
			dir, home := t.TempDir(), t.TempDir()
			// The transcript lives at the registry entry's own computed path, its
			// stem matching prePayload's session_id so the spool is current.
			transcript := transcriptPathFor(home, wt, "sess-live")
			writeTranscript(t, transcript, tc.fold)
			if tc.status != "-" {
				writeHomeEntry(t, home, "1.json", liveEntry(wt, "sess-live", tc.status, tc.waitingFor, 1))
			}
			if tc.spool {
				writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)
			}
			worktree := wt
			if tc.noWorktree {
				worktree = ""
			}
			chat, err := p.ReadChat(provider.ReadSpec{RunID: "run_1", RuntimeDir: dir, Home: home,
				Worktree: worktree, TranscriptPath: transcript})
			if err != nil {
				t.Fatalf("ReadChat: %v", err)
			}
			if chat.State != tc.wantState || chat.StateDetail != tc.wantDetail {
				t.Errorf("chat = {state:%q detail:%q}; want {state:%q detail:%q}",
					chat.State, chat.StateDetail, tc.wantState, tc.wantDetail)
			}
			if got := chat.PendingDialog != nil; got != tc.wantDialog {
				t.Errorf("PendingDialog = %+v; want present=%v", chat.PendingDialog, tc.wantDialog)
			}
		})
	}
}

// An ENDED read (Home "") never consults the registry, whatever it says: a
// terminal view is the transcript alone (issue #79 decision 6).
func TestReadChat_endedReadIgnoresRegistry(t *testing.T) {
	p := chatProvider(t)
	home := t.TempDir()
	const wt = "/wt/run"
	transcript := transcriptPathFor(home, wt, "sess-live")
	writeTranscript(t, transcript, foldNeedsInputTail)
	writeHomeEntry(t, home, "1.json", liveEntry(wt, "sess-live", "waiting", "input needed", 1))
	chat, err := p.ReadChat(provider.ReadSpec{RunID: "run_1", Worktree: wt, TranscriptPath: transcript})
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	if chat.State != provider.StateNeedsInput || chat.StateDetail != "" {
		t.Errorf("chat = {state:%q detail:%q}; want the fold's needs_input with no detail", chat.State, chat.StateDetail)
	}
}

func TestParseTranscript_stateEdges(t *testing.T) {
	cases := map[string]struct {
		lines string
		want  string
	}{
		"empty":      {"", provider.StateIdle},
		"user waits": {line(`{"type":"user","message":{"role":"user","content":"go"}}`), provider.StateWorking},
		"assistant ended": {
			line(`{"type":"user","message":{"role":"user","content":"go"}}`) +
				line(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`),
			provider.StateNeedsInput,
		},
		"tool running": {
			line(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","id":"t1","input":{"command":"ls"}}]}}`),
			provider.StateWorking,
		},
		// A /clear rotates to a fresh transcript whose only tail is the local
		// slash-command echo; it renders (issue #51 decision 2) but stays
		// state-neutral, so the run reads idle — not stuck in `working`
		// (issue #45).
		"clear echo idles": {
			line(`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>\n  <command-message>clear</command-message>\n  <command-args></command-args>"}}`),
			provider.StateIdle,
		},
		// Tag order varies (compat.md §5), so an echo that leads with
		// <command-message> is also recognised — pins that OR arm.
		"command-message-led echo idles": {
			line(`{"type":"user","message":{"role":"user","content":"<command-message>rewind</command-message>\n  <command-name>/rewind</command-name>"}}`),
			provider.StateIdle,
		},
		// Local-command output is likewise state-neutral.
		"command output idles": {
			line(`{"type":"user","message":{"role":"user","content":"<local-command-stdout>ok</local-command-stdout>"}}`),
			provider.StateIdle,
		},
		// A pending dialog (no tool_result yet) blocks the run on the operator.
		"pending dialog asks": {
			line(colorUseLine),
			provider.StateQuestion,
		},
		// A RESOLVED dialog at the tail is history, not a question (issue #56
		// decision 3): the retro-flushed tool_use + tool_result pair means the
		// agent just resumed — the answered dialog message must derive working
		// exactly as its demoted tool chip did before.
		"resolved dialog works": {
			line(colorUseLine) +
				line(resolutionLine(`{"questions":[],"answers":{"Which color do you prefer?":"Red"},"annotations":{}}`, false)),
			provider.StateWorking,
		},
	}
	for name, c := range cases {
		got, err := ParseTranscript(strings.NewReader(c.lines))
		if err != nil {
			t.Fatalf("%s: ParseTranscript: %v", name, err)
		}
		if got.State != c.want {
			t.Errorf("%s: state = %q; want %q", name, got.State, c.want)
		}
	}
}

// The pending-work hold (issue #159): async agents/workflows this session
// spawned keep the run `working` past the assistant's turn-ending text — the
// CLI re-invokes the model when they complete, so needs_input (and its push)
// would be spurious — while the structurally excluded shapes (background
// Bash, sync Agent calls) must not hold, terminal notifications and TaskStop
// release, and an API error outranks the hold.
func TestParseTranscript_pendingWorkStateEdges(t *testing.T) {
	launch := line(agentUseLine) + line(agentAsyncResultLine)
	bashBg := line(bashBgUseLine) + line(bashBgResultLine) + line(turnEndTextLine)
	cases := map[string]struct {
		lines string
		want  string
	}{
		// THE hold: an async Agent launch, then the model ends its turn.
		"async agent launch holds working": {
			launch + line(turnEndTextLine), provider.StateWorking,
		},
		// Workflows are always async; the pending id is taskId (w…), not runId.
		"workflow launch holds working": {
			line(workflowUseLine) + line(workflowAsyncResultLine) + line(turnEndTextLine),
			provider.StateWorking,
		},
		// Background Bash must NOT hold — its result has backgroundTaskId and
		// no status, so it never enters the set (a dev server never exits and
		// would pin `working` forever, suppressing every push).
		"background bash never holds": {bashBg, provider.StateNeedsInput},
		// Its completion notification removes nothing (the id was never added)
		// but the carrier itself still bumps working…
		"bash completion notification bumps working": {
			bashBg + line(enqueueLine(taskNotification("b07izrfyc", "completed"))),
			provider.StateWorking,
		},
		// …and the model's next ended turn is a real needs_input again.
		"bash notification then text needs input": {
			bashBg + line(enqueueLine(taskNotification("b07izrfyc", "completed"))) + line(turnEndTextLine),
			provider.StateNeedsInput,
		},
		// A SYNCHRONOUS Agent call resolves in-turn: status "completed" +
		// totalDurationMs, never async_launched — no add, no hold.
		"sync agent call never holds": {
			line(syncAgentUseLine) + line(syncAgentResultLine) + line(turnEndTextLine),
			provider.StateNeedsInput,
		},
		// Any non-empty terminal status releases exactly like completed.
		"failed notification releases": {
			launch + line(turnEndTextLine) +
				line(enqueueLine(taskNotification(pendingAgentID, "failed"))) + line(turnEndTextLine),
			provider.StateNeedsInput,
		},
		"killed notification releases": {
			launch + line(turnEndTextLine) +
				line(enqueueLine(taskNotification(pendingAgentID, "killed"))) + line(turnEndTextLine),
			provider.StateNeedsInput,
		},
		// The status-tag gate: a status-less payload (Monitor-style <event>)
		// naming a pending id must NOT remove it — the hold stands.
		"status-less payload keeps the hold": {
			launch + line(enqueueLine(taskNotification(pendingAgentID, ""))) + line(turnEndTextLine),
			provider.StateWorking,
		},
		// Duplicate notifications for one id are real (SendMessage resume,
		// Monitor per-event); a second delivery after removal is a no-op.
		"duplicate notification is idempotent": {
			launch + line(turnEndTextLine) +
				line(userNotifyLine(taskNotification(pendingAgentID, "completed"))) +
				line(userNotifyLine(taskNotification(pendingAgentID, "completed"))) +
				line(turnEndTextLine),
			provider.StateNeedsInput,
		},
		// Errors must surface past pending work: needs_input unconditionally.
		"api error outranks the hold": {
			launch + line(apiErrorLine), provider.StateNeedsInput,
		},
	}
	for name, c := range cases {
		got, err := ParseTranscript(strings.NewReader(c.lines))
		if err != nil {
			t.Fatalf("%s: ParseTranscript: %v", name, err)
		}
		if got.State != c.want {
			t.Errorf("%s: state = %q; want %q", name, got.State, c.want)
		}
	}
}

// The full async-agent roundtrip (issue #159, from the mined m125 lifecycle),
// asserted at EVERY cumulative prefix: no prefix anywhere inside the pending
// window may read needs_input — the push-gate acceptance in unit form. The
// enqueue both removes the id AND bumps working itself, closing the
// micro-window between enqueue and delivery.
func TestParseTranscript_pendingWorkTrajectory(t *testing.T) {
	steps := []struct{ name, add, want string }{
		{"launch tool_use", line(agentUseLine), provider.StateWorking},
		{"async_launched result", line(agentAsyncResultLine), provider.StateWorking},
		{"turn-ending text is held", line(turnEndTextLine), provider.StateWorking},
		{"completion enqueue", line(enqueueLine(taskNotification(pendingAgentID, "completed"))), provider.StateWorking},
		{"user-form delivery", line(userNotifyLine(taskNotification(pendingAgentID, "completed"))), provider.StateWorking},
		{"final text needs input", line(turnEndTextLine), provider.StateNeedsInput},
	}
	var lines strings.Builder
	for _, s := range steps {
		lines.WriteString(s.add)
		got, err := ParseTranscript(strings.NewReader(lines.String()))
		if err != nil {
			t.Fatalf("%s: ParseTranscript: %v", s.name, err)
		}
		if got.State != s.want {
			t.Errorf("%s: state = %q; want %q", s.name, got.State, s.want)
		}
	}
}

// The attachment delivery variant (issue #159): a mid-turn injected
// queued_command/task-notification releases the pending id and itself derives
// working; only the model's NEXT ended turn reads needs_input. Other
// attachment types stay state-neutral (the stateEdges cases above never see
// them because foldTranscript skips them wholesale).
func TestParseTranscript_pendingWorkAttachmentDelivery(t *testing.T) {
	delivered := line(agentUseLine) + line(agentAsyncResultLine) + line(turnEndTextLine) +
		line(enqueueLine(taskNotification(pendingAgentID, "completed"))) +
		line(attachNotifyLine(taskNotification(pendingAgentID, "completed")))

	got, err := ParseTranscript(strings.NewReader(delivered))
	if err != nil {
		t.Fatalf("ParseTranscript(attachment tail): %v", err)
	}
	if got.State != provider.StateWorking {
		t.Errorf("attachment as last event: state = %q; want %q (delivery means the model is about to resume)", got.State, provider.StateWorking)
	}

	got, err = ParseTranscript(strings.NewReader(delivered + line(turnEndTextLine)))
	if err != nil {
		t.Fatalf("ParseTranscript(attachment+text): %v", err)
	}
	if got.State != provider.StateNeedsInput {
		t.Errorf("text after attachment delivery: state = %q; want %q (the id was released)", got.State, provider.StateNeedsInput)
	}
}

// TaskStop releases the pending id immediately (issue #159) — no notification
// line needed at all, and the removal is order-tolerant (live transcripts
// show the killed enqueue sometimes BEFORE the TaskStop line in file order).
func TestParseTranscript_pendingWorkTaskStop(t *testing.T) {
	steps := []struct{ name, add, want string }{
		{"launch", line(agentUseLine) + line(agentAsyncResultLine), provider.StateWorking},
		{"held text", line(turnEndTextLine), provider.StateWorking},
		{"TaskStop pair", line(taskStopUseLine) + line(taskStopResultLine), provider.StateWorking},
		{"text after stop needs input", line(turnEndTextLine), provider.StateNeedsInput},
	}
	var lines strings.Builder
	for _, s := range steps {
		lines.WriteString(s.add)
		got, err := ParseTranscript(strings.NewReader(lines.String()))
		if err != nil {
			t.Fatalf("%s: ParseTranscript: %v", s.name, err)
		}
		if got.State != s.want {
			t.Errorf("%s: state = %q; want %q", s.name, got.State, s.want)
		}
	}
}

// SendMessage to a stopped agent resumes it in the background (live-verified:
// the result carries resumedAgentId), so the id RE-ENTERS the pending set and
// the hold re-arms until its second notification (issue #159).
func TestParseTranscript_pendingWorkSendMessageResume(t *testing.T) {
	steps := []struct{ name, add, want string }{
		{"launch", line(agentUseLine) + line(agentAsyncResultLine), provider.StateWorking},
		{"first completion delivered", line(userNotifyLine(taskNotification(pendingAgentID, "completed"))), provider.StateWorking},
		{"text after completion needs input", line(turnEndTextLine), provider.StateNeedsInput},
		{"SendMessage resume pair", line(sendMessageUseLine) + line(sendMessageResultLine), provider.StateWorking},
		{"text after resume is held again", line(turnEndTextLine), provider.StateWorking},
		{"second completion delivered", line(userNotifyLine(taskNotification(pendingAgentID, "completed"))), provider.StateWorking},
		{"final text needs input", line(turnEndTextLine), provider.StateNeedsInput},
	}
	var lines strings.Builder
	for _, s := range steps {
		lines.WriteString(s.add)
		got, err := ParseTranscript(strings.NewReader(lines.String()))
		if err != nil {
			t.Fatalf("%s: ParseTranscript: %v", s.name, err)
		}
		if got.State != s.want {
			t.Errorf("%s: state = %q; want %q", s.name, got.State, s.want)
		}
	}
}

// Structured live signals break through the pending-work hold (issue #159):
// the hold only softens the transcript's assistant-text edge — a live spool
// dialog still forces StateQuestion + PendingDialog, and a live blocked
// registry "waiting" (issue #79 — the Notification marker's successor) still
// forces StateNeedsInput, in ReadChat's layered order. A waiting permission
// prompt or question must never hide behind "working on background tasks".
func TestReadChat_liveSignalsOutrankPendingHold(t *testing.T) {
	p := spoolTestProvider(t)
	dir, home := t.TempDir(), t.TempDir()
	const wt = "/wt/run"
	// A transcript tail that holds working: async launch + turn-ending text.
	// Named sess-live to match prePayload's session_id (dialog staleness key).
	transcript := transcriptPathFor(home, wt, "sess-live")
	writeTranscript(t, transcript, foldHoldTail)
	spec := provider.ReadSpec{RunID: "run_1", RuntimeDir: dir, Home: home, Worktree: wt, TranscriptPath: transcript}

	chat, err := p.ReadChat(spec)
	if err != nil {
		t.Fatalf("ReadChat(no signals): %v", err)
	}
	if chat.State != provider.StateWorking {
		t.Fatalf("no signals: state = %q; want %q (the pending hold)", chat.State, provider.StateWorking)
	}

	// A live spool dialog wins over the hold.
	writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)
	chat, err = p.ReadChat(spec)
	if err != nil {
		t.Fatalf("ReadChat(spool dialog): %v", err)
	}
	if chat.State != provider.StateQuestion || chat.PendingDialog == nil {
		t.Errorf("spool dialog over hold: chat = %+v; want StateQuestion with PendingDialog", chat)
	}

	// Dialog gone, the registry says waiting (a permission prompt lab cannot
	// render): needs_input wins over the hold too, with the reason attached.
	if err := os.Remove(filepath.Join(dir, dialogsSubdir, "run_1.json")); err != nil {
		t.Fatal(err)
	}
	writeHomeEntry(t, home, "1.json", liveEntry(wt, "sess-live", "waiting", "permission request", 1))
	chat, err = p.ReadChat(spec)
	if err != nil {
		t.Fatalf("ReadChat(registry waiting): %v", err)
	}
	if chat.State != provider.StateNeedsInput || chat.StateDetail != "permission request" {
		t.Errorf("registry waiting over hold: chat = {state:%q detail:%q}; want needs_input with the reason",
			chat.State, chat.StateDetail)
	}
}

// usageLine renders an assistant text line carrying a usage object with the
// three prompt-side token fields the meter reads (input + cache_creation +
// cache_read). output_tokens and server_tool_use ride along exactly as claude
// writes them (compat §5) but must NOT be decoded into Used (ADR-0061).
func usageLine(input, cacheCreation, cacheRead int64) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":999,"server_tool_use":{"web_search_requests":0}}}}`,
		input, cacheCreation, cacheRead)
}

// The context-occupancy meter (issue #243 / ADR-0061): ReadChat composes
// Chat.ContextUsage from the LATEST assistant line's prompt-side usage sum over
// the spawn-time model's window. Every read here degrades to the transcript
// alone (RuntimeDir "") so the meter is isolated from the live-signal overlay.
func TestReadChat_contextUsage(t *testing.T) {
	p := chatProvider(t)
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "sess.jsonl")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// Happy path: two assistant turns with different usage — the LATER turn's
	// three fields decide Used, and fable's window is the 1M limit.
	t.Run("latest assistant usage wins", func(t *testing.T) {
		path := write(t, line(usageLine(9, 7374, 19409))+line(usageLine(11, 500, 40000)))
		chat, err := p.ReadChat(provider.ReadSpec{TranscriptPath: path, Model: "fable"})
		if err != nil {
			t.Fatalf("ReadChat: %v", err)
		}
		if chat.ContextUsage == nil {
			t.Fatalf("ContextUsage = nil; want the composed meter")
		}
		if got, want := chat.ContextUsage.Used, int64(11+500+40000); got != want {
			t.Errorf("Used = %d; want %d (the LATER turn's sum)", got, want)
		}
		if got, want := chat.ContextUsage.Limit, int64(1_000_000); got != want {
			t.Errorf("Limit = %d; want %d (fable window)", got, want)
		}
	})

	// A later assistant line lacking usage (the <synthetic> API-error shape) or
	// carrying an all-zero usage must NOT clobber the real earlier value.
	t.Run("usage-less and zero tails keep the real value", func(t *testing.T) {
		const realSum = 9 + 7374 + 19409
		real := line(usageLine(9, 7374, 19409))
		tails := map[string]string{
			"synthetic error (no usage)": line(`{"type":"assistant","isApiErrorMessage":true,"message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"API Error: 529"}]}}`),
			"all-zero usage":             line(usageLine(0, 0, 0)),
		}
		for name, tail := range tails {
			chat, err := p.ReadChat(provider.ReadSpec{TranscriptPath: write(t, real+tail), Model: "fable"})
			if err != nil {
				t.Fatalf("%s: ReadChat: %v", name, err)
			}
			if chat.ContextUsage == nil || chat.ContextUsage.Used != realSum {
				t.Errorf("%s: ContextUsage = %+v; want the earlier real sum %d", name, chat.ContextUsage, realSum)
			}
		}
	})

	// An unknown or empty model hides the meter even with real usage present.
	t.Run("unknown and empty model hide the meter", func(t *testing.T) {
		path := write(t, line(usageLine(9, 7374, 19409)))
		for _, model := range []string{"gpt-9", ""} {
			chat, err := p.ReadChat(provider.ReadSpec{TranscriptPath: path, Model: model})
			if err != nil {
				t.Fatalf("model %q: ReadChat: %v", model, err)
			}
			if chat.ContextUsage != nil {
				t.Errorf("model %q: ContextUsage = %+v; want nil (window unknown)", model, chat.ContextUsage)
			}
		}
	})

	// No assistant usage anywhere → nil, even for a known model.
	t.Run("no assistant usage hides the meter", func(t *testing.T) {
		path := write(t, line(`{"type":"user","message":{"role":"user","content":"go"}}`)+
			line(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`))
		chat, err := p.ReadChat(provider.ReadSpec{TranscriptPath: path, Model: "fable"})
		if err != nil {
			t.Fatalf("ReadChat: %v", err)
		}
		if chat.ContextUsage != nil {
			t.Errorf("ContextUsage = %+v; want nil (no assistant usage present)", chat.ContextUsage)
		}
	})

	// The window table keyed on the spawn-time catalog value: bare `opus`
	// survives on older rows at 200K, `opus[1m]` opts into the 1M window.
	t.Run("window per spawn-time model value", func(t *testing.T) {
		path := write(t, line(usageLine(100, 0, 0)))
		for _, tc := range []struct {
			model string
			limit int64
		}{
			{"opus", 200_000},
			{"opus[1m]", 1_000_000},
			{"sonnet", 200_000},
			{"haiku", 200_000},
			{"fable", 1_000_000},
		} {
			chat, err := p.ReadChat(provider.ReadSpec{TranscriptPath: path, Model: tc.model})
			if err != nil {
				t.Fatalf("model %q: ReadChat: %v", tc.model, err)
			}
			if chat.ContextUsage == nil || chat.ContextUsage.Limit != tc.limit {
				t.Errorf("model %q: ContextUsage = %+v; want Limit %d", tc.model, chat.ContextUsage, tc.limit)
			}
		}
	})
}

// An answered dialog stays a DIALOG message carrying the resolved answer in
// Outcome (issue #56 decision 3) — never the demoted raw-JSON tool chip it
// used to become. Every resolution below is the live 2.1.198 shape (compat
// §5): the answers map keyed by question text, the ", "-joined multi-select
// labels in TOGGLE order, the denial string + user-rejected stamp, the 60s
// unattended timeout's answers:{} + afkTimeoutMs, and the plan approve
// object / reject-with-feedback denial string.
func TestParseTranscript_answeredDialogOutcomes(t *testing.T) {
	cases := map[string]struct {
		lines string
		want  provider.DialogOutcome
	}{
		"single-select recorded label": {
			lines: line(colorUseLine) +
				line(resolutionLine(`{"questions":[],"answers":{"Which color do you prefer?":"Red"},"annotations":{}}`, false)),
			want: provider.DialogOutcome{Results: []provider.QuestionResult{
				{Question: "Which color do you prefer?", Chosen: []string{"Red"}},
			}},
		},
		// A free-text (Other-row) answer records verbatim and is not a listed
		// label — it must land in OtherText untouched, even with a ", " in it
		// (single-select never splits).
		"free-text Other answer": {
			lines: line(colorUseLine) +
				line(resolutionLine(`{"questions":[],"answers":{"Which color do you prefer?":"Chartreuse, please"},"annotations":{}}`, false)),
			want: provider.DialogOutcome{Results: []provider.QuestionResult{
				{Question: "Which color do you prefer?", OtherText: "Chartreuse, please"},
			}},
		},
		// Results come one per question in DIALOG order, regardless of the
		// answers map's JSON order (fruits first here).
		"multi-question dialog order": {
			lines: line(twoQUseLine) +
				line(resolutionLine(`{"questions":[],"answers":{"Which fruits do you like?":"Apple, Cherry","Which color do you prefer?":"Red"},"annotations":{}}`, false)),
			want: provider.DialogOutcome{Results: []provider.QuestionResult{
				{Question: "Which color do you prefer?", Chosen: []string{"Red"}},
				{Question: "Which fruits do you like?", Chosen: []string{"Apple", "Cherry"}},
			}},
		},
		// Multi-select records TOGGLE order, not option order (live: Cherry
		// toggled before Apple records "Cherry, Apple") — Chosen preserves it.
		"multi-select toggle order": {
			lines: line(fruitsUseLine) +
				line(resolutionLine(`{"questions":[],"answers":{"Which fruits do you like?":"Cherry, Apple"},"annotations":{}}`, false)),
			want: provider.DialogOutcome{Results: []provider.QuestionResult{
				{Question: "Which fruits do you like?", Chosen: []string{"Cherry", "Apple"}},
			}},
		},
		// Non-label segments of a multi-select answer are the Other free text,
		// re-joined with ", " when the text itself contained the separator.
		"multi-select with free-text leftover": {
			lines: line(fruitsUseLine) +
				line(resolutionLine(`{"questions":[],"answers":{"Which fruits do you like?":"Cherry, hold the mayo, extra sauce"},"annotations":{}}`, false)),
			want: provider.DialogOutcome{Results: []provider.QuestionResult{
				{Question: "Which fruits do you like?", Chosen: []string{"Cherry"}, OtherText: "hold the mayo, extra sauce"},
			}},
		},
		"denial dismissed": {
			lines: line(colorUseLine) + line(resolutionLine(`"User rejected tool use"`, true)),
			want:  provider.DialogOutcome{Dismissed: true},
		},
		// The 60s unattended timeout resolves with answers:{} — no answer to
		// show, so it dismisses rather than inventing empty results.
		"unattended timeout dismissed": {
			lines: line(fruitsUseLine) +
				line(resolutionLine(`{"questions":[],"answers":{},"annotations":{},"afkTimeoutMs":60000}`, false)),
			want: provider.DialogOutcome{Dismissed: true},
		},
		"plan approved": {
			lines: line(planUseLine) +
				line(resolutionLine(`{"plan":"# The plan\n\n- do the thing","isAgent":false,"filePath":"/tmp/p.md","planWasEdited":false}`, false)),
			want: provider.DialogOutcome{Approved: true},
		},
		// The typed rejection feedback rides after the last "the user said:\n"
		// of the denial string; a plain rejection (no marker) leaves it empty.
		"plan rejected with feedback": {
			lines: line(planUseLine) +
				line(resolutionLine(`"Error: The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). To tell you how to proceed, the user said:\nAdd tests first"`, true)),
			want: provider.DialogOutcome{Feedback: "Add tests first"},
		},
		// Live 2026-09-23 on 2.1.265 AND 2.1.280: claude may append a memory
		// note AFTER the typed feedback inside the same denial string; it is
		// claude's text, not the operator's, and must not reach Feedback.
		"plan rejected with feedback and appended note": {
			lines: line(planUseLine) +
				line(resolutionLine(`"Error: The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). To tell you how to proceed, the user said:\nAdd tests first\n\nNote: The user's next message may contain a correction or preference. Pay close attention — if they explain what went wrong or how they'd prefer you to work, consider saving that to memory for future sessions."`, true)),
			want: provider.DialogOutcome{Feedback: "Add tests first"},
		},
	}
	for name, c := range cases {
		got, err := ParseTranscript(strings.NewReader(c.lines))
		if err != nil {
			t.Fatalf("%s: ParseTranscript: %v", name, err)
		}
		if len(got.Messages) != 1 {
			t.Fatalf("%s: %d messages; want exactly the dialog (the tool_result event emits none)", name, len(got.Messages))
		}
		m := got.Messages[0]
		if m.Kind != provider.MessageDialog || m.Dialog == nil || m.Tool != nil {
			t.Fatalf("%s: message = %+v; want a dialog message, never a tool chip", name, m)
		}
		if m.Dialog.Outcome == nil {
			t.Fatalf("%s: Outcome = nil; want %+v (a resolved dialog must not look pending)", name, c.want)
		}
		if !reflect.DeepEqual(*m.Dialog.Outcome, c.want) {
			t.Errorf("%s: outcome = %+v; want %+v", name, *m.Dialog.Outcome, c.want)
		}
	}
}

// Issue #56 decision 4: the plan markdown is NEVER truncated — the UI renders
// plans in full, so a body past the old 2000-byte cap passes through whole
// (pending and answered alike; the live spool path shares planDialog).
func TestParseTranscript_planPromptUntruncated(t *testing.T) {
	plan := strings.Repeat("x", 3*truncateLimit)
	use := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"p1","name":"ExitPlanMode","input":{"plan":"` + plan + `"}}]}}`
	got, err := ParseTranscript(strings.NewReader(line(use)))
	if err != nil {
		t.Fatalf("ParseTranscript: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Dialog == nil {
		t.Fatalf("messages = %+v; want the one plan dialog", got.Messages)
	}
	if p := got.Messages[0].Dialog.Prompt; p != plan {
		t.Errorf("plan prompt = %d bytes; want the full %d-byte plan untruncated", len(p), len(plan))
	}
}

// A local slash-command echo (/clear, /rewind, …) renders as a plain user
// text message showing the command line, and its captured output as a
// lifecycle message (issue #51 decision 2 reversed issue #45's rendering
// half) — the raw <command-…> / <local-command-stdout> tags never leak into
// the chat, and a genuine plain-text reply after the echo is unaffected.
func TestParseTranscript_localCommandEchoRenders(t *testing.T) {
	echo := line(`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>\n  <command-message>clear</command-message>\n  <command-args></command-args>"}}`) +
		line(`{"type":"user","message":{"role":"user","content":"<local-command-stdout>Conversation cleared</local-command-stdout>"}}`)

	got, err := ParseTranscript(strings.NewReader(echo))
	if err != nil {
		t.Fatalf("ParseTranscript(echo): %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("echo rendered %d messages; want the command line + its output", len(got.Messages))
	}
	if m := got.Messages[0]; m.Kind != provider.MessageText || m.Role != "user" || m.Text != "/clear" {
		t.Errorf("echo msg0 = %+v; want user text \"/clear\"", m)
	}
	if m := got.Messages[1]; m.Kind != provider.MessageLifecycle || m.Text != "Conversation cleared" || m.Error {
		t.Errorf("echo msg1 = %+v; want a non-error lifecycle with the stdout", m)
	}

	// A command with args echoes as one "/name args" line; an empty stdout is
	// dropped entirely.
	got, err = ParseTranscript(strings.NewReader(
		line(`{"type":"user","message":{"role":"user","content":"<command-name>/foo</command-name>\n  <command-message>foo</command-message>\n  <command-args>bar baz</command-args>"}}`) +
			line(`{"type":"user","message":{"role":"user","content":"<local-command-stdout></local-command-stdout>"}}`)))
	if err != nil {
		t.Fatalf("ParseTranscript(args echo): %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Text != "/foo bar baz" {
		t.Fatalf("args echo messages = %+v; want just \"/foo bar baz\"", got.Messages)
	}

	// Echo then a genuine reply: the reply still renders and derives working
	// (the Send→Interrupt morph, ADR-0022).
	got, err = ParseTranscript(strings.NewReader(echo +
		line(`{"type":"user","message":{"role":"user","content":"now do the thing"}}`)))
	if err != nil {
		t.Fatalf("ParseTranscript(echo+reply): %v", err)
	}
	if n := len(got.Messages); n != 3 || got.Messages[2].Text != "now do the thing" {
		t.Fatalf("echo+reply messages = %+v; want echo, stdout, reply", got.Messages)
	}
	if got.State != provider.StateWorking {
		t.Errorf("echo+reply state = %q; want %q (Send→Interrupt morph preserved)", got.State, provider.StateWorking)
	}
}

// THE issue-#45 / issue-#51 regression pin: command echoes are excluded from
// state derivation even though they now render. The echo formerly drove
// deriveState to `working`, which stranded the composer on a pulsing
// Interrupt with no Send after /clear (the stuck-composer root cause,
// ADR-0022 / issue #45); issue #51 decision 2 made echoes visible but kept
// them state-neutral. Two edges: a transcript whose tail is ONLY an echo
// (+stdout) after real turns keeps the pre-echo state, and a fresh
// post-/clear transcript containing ONLY the echo derives idle.
func TestParseTranscript_commandEchoNeverDrivesState(t *testing.T) {
	echoTail := line(`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>\n  <command-message>clear</command-message>\n  <command-args></command-args>"}}`) +
		line(`{"type":"user","message":{"role":"user","content":"<local-command-stdout>cleared</local-command-stdout>"}}`)

	// Real turns, then only the echo: the assistant's ended turn still decides
	// the state — needs_input, exactly as if the echo were absent.
	turns := line(`{"type":"user","message":{"role":"user","content":"go"}}`) +
		line(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
	got, err := ParseTranscript(strings.NewReader(turns + echoTail))
	if err != nil {
		t.Fatalf("ParseTranscript(turns+echo): %v", err)
	}
	if got.State != provider.StateNeedsInput {
		t.Errorf("turns+echo state = %q; want %q (echo must not override the pre-echo state)", got.State, provider.StateNeedsInput)
	}

	// A fresh post-/clear transcript contains ONLY the echo: idle, never
	// `working` — the composer shows Send, not the stuck Interrupt.
	got, err = ParseTranscript(strings.NewReader(echoTail))
	if err != nil {
		t.Fatalf("ParseTranscript(echo only): %v", err)
	}
	if got.State != provider.StateIdle {
		t.Errorf("fresh post-/clear state = %q; want %q (the stuck-composer case)", got.State, provider.StateIdle)
	}
}

// The 2.1.284 /clear write sequence (issue #79, compat §5 — the five lines the
// fresh transcript holds right after /clear, live 2026-10-09): mode,
// file-history-snapshot, an isMeta caveat, the user <command-name> echo, and a
// system/local_command event whose parentUuid is that echo's uuid. Exactly
// ONE "/clear" user text renders (the system event does not repeat the
// command its parent already showed; its stdout is empty), and the state is
// idle.
const (
	clearEchoUUID    = "aaaaaaaa-0000-4000-8000-000000000001"
	clearSequence284 = `{"type":"mode","mode":"normal","sessionId":"s"}` + "\n" +
		`{"type":"file-history-snapshot","messageId":"` + clearEchoUUID + `","snapshot":{},"isSnapshotUpdate":false}` + "\n" +
		`{"parentUuid":null,"type":"user","message":{"role":"user","content":"<local-command-caveat>Caveat: …</local-command-caveat>"},"isMeta":true,"uuid":"aaaaaaaa-0000-4000-8000-000000000002"}` + "\n" +
		`{"parentUuid":"aaaaaaaa-0000-4000-8000-000000000002","type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>\n            <command-message>clear</command-message>\n            <command-args></command-args>"},"uuid":"` + clearEchoUUID + `"}` + "\n" +
		`{"parentUuid":"` + clearEchoUUID + `","type":"system","subtype":"local_command","content":"<local-command-stdout></local-command-stdout>","level":"info","uuid":"aaaaaaaa-0000-4000-8000-000000000003","isMeta":false,"commandRun":{"command":"clear","args":""}}` + "\n"
)

func TestParseTranscript_localCommandSystemEcho_clearSequence(t *testing.T) {
	got, err := ParseTranscript(strings.NewReader(clearSequence284))
	if err != nil {
		t.Fatalf("ParseTranscript: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("messages = %+v; want exactly one \"/clear\" user text", got.Messages)
	}
	if m := got.Messages[0]; m.Kind != provider.MessageText || m.Role != "user" || m.Text != "/clear" {
		t.Errorf("msg0 = %+v; want user text \"/clear\"", m)
	}
	if got.State != provider.StateIdle {
		t.Errorf("state = %q; want %q (the echo pair is state-neutral)", got.State, provider.StateIdle)
	}
}

// A system/local_command with NO preceding user echo (its parentUuid names
// nothing that rendered a command) still shows the command line — rebuilt
// from commandRun as "/" + command [+ " " + args] — followed by its non-empty
// stdout as a lifecycle (truncated like the user-echo stdout). Neither line
// touches state: appended after an assistant text tail the state stays
// needs_input; alone it is idle.
func TestParseTranscript_localCommandSystemEcho_lone(t *testing.T) {
	lone := line(`{"parentUuid":"some-assistant-uuid","type":"system","subtype":"local_command","content":"<local-command-stdout>Total cost: $0.01</local-command-stdout>","uuid":"u-sys","commandRun":{"command":"cost","args":" --verbose "}}`)

	got, err := ParseTranscript(strings.NewReader(lone))
	if err != nil {
		t.Fatalf("ParseTranscript(lone): %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("lone messages = %+v; want the command line + its stdout", got.Messages)
	}
	if m := got.Messages[0]; m.Kind != provider.MessageText || m.Role != "user" || m.Text != "/cost --verbose" {
		t.Errorf("lone msg0 = %+v; want user text \"/cost --verbose\"", m)
	}
	if m := got.Messages[1]; m.Kind != provider.MessageLifecycle || m.Error || m.Text != "Total cost: $0.01" {
		t.Errorf("lone msg1 = %+v; want a non-error lifecycle with the stdout", m)
	}
	if got.State != provider.StateIdle {
		t.Errorf("lone state = %q; want %q", got.State, provider.StateIdle)
	}

	// A long stdout is truncated like the user-echo path.
	long := strings.Repeat("x", truncateLimit+50)
	got, err = ParseTranscript(strings.NewReader(line(`{"type":"system","subtype":"local_command","content":"<local-command-stdout>` + long + `</local-command-stdout>","commandRun":{"command":"context","args":""}}`)))
	if err != nil {
		t.Fatalf("ParseTranscript(long): %v", err)
	}
	if len(got.Messages) != 2 || got.Messages[1].Text != truncate(long, truncateLimit) {
		t.Errorf("long stdout not truncated to truncateLimit: %+v", got.Messages)
	}

	// State-neutral after real turns: the assistant's ended turn still decides.
	turns := line(`{"type":"user","message":{"role":"user","content":"go"}}`) +
		line(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
	for name, tail := range map[string]string{"lone system echo": lone, "2.1.284 /clear sequence": clearSequence284} {
		got, err = ParseTranscript(strings.NewReader(turns + tail))
		if err != nil {
			t.Fatalf("%s: ParseTranscript: %v", name, err)
		}
		if got.State != provider.StateNeedsInput {
			t.Errorf("%s after an assistant text tail: state = %q; want %q (echoes never touch lastKey)", name, got.State, provider.StateNeedsInput)
		}
	}

	// A malformed commandRun never drops the line: the stdout still renders,
	// the command line is simply absent.
	got, err = ParseTranscript(strings.NewReader(line(`{"type":"system","subtype":"local_command","content":"<local-command-stdout>ok</local-command-stdout>","commandRun":"clear"}`)))
	if err != nil {
		t.Fatalf("ParseTranscript(malformed commandRun): %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0].Kind != provider.MessageLifecycle || got.Messages[0].Text != "ok" {
		t.Errorf("malformed commandRun messages = %+v; want just the stdout lifecycle", got.Messages)
	}
}

// A line beyond the scanner cap must surface as an error — silently stopping
// mid-file would serve a truncated chat (and a backwards-moving cursor) on
// every subsequent reparse.
func TestParseTranscript_oversizeLineErrors(t *testing.T) {
	huge := line(`{"type":"user","message":{"role":"user","content":"` + strings.Repeat("x", 9*1024*1024) + `"}}`)
	if _, err := ParseTranscript(strings.NewReader(huge)); err == nil {
		t.Fatal("ParseTranscript(9MB line) = nil error; want a scan error")
	}
}

func line(s string) string { return s + "\n" }

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
