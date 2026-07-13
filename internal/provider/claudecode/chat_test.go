package claudecode

// Transcript location (registry cwd-match → slug/sessionId path), the
// state-derivation edges the compat fixture doesn't cover, and ReadChat's
// adapter-owned conversational-state composition (issue #92) — the transcript
// fold overlaid with the hook spool's live signals, the precedence core's
// chat service used to apply. The full JSONL → schema mapping is pinned in
// internal/compat against a captured fixture.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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

func chatProvider(t *testing.T, registryDir, projectsDir string) *Provider {
	t.Helper()
	p, err := New(Options{
		ClaudeBin: "claude", ConfigPath: filepath.Join(t.TempDir(), ".claude.json"),
		RegistryDir: registryDir, ProjectsDir: projectsDir, LoginDir: t.TempDir(),
		Runner: tmuxx.NewFake(), Bus: events.NewBus(),
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestLocateTranscript_registryMatch(t *testing.T) {
	registryDir := t.TempDir()
	projectsDir := t.TempDir()
	worktree := "/home/op/state/worktrees/proj-manual-1"
	sessionID := "abcd-1234"

	// A live registry entry (this test process) whose cwd is the worktree.
	writeJSONFile(t, filepath.Join(registryDir, "1.json"), map[string]any{
		"pid": os.Getpid(), "cwd": worktree, "startedAt": 1, "sessionId": sessionID,
		"bridgeSessionId": "session_x",
	})
	// The transcript file at the slug path.
	dir := filepath.Join(projectsDir, SlugForDir(worktree))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := chatProvider(t, registryDir, projectsDir)
	got, err := p.LocateTranscript(context.Background(), "proj~manual-1", worktree)
	if err != nil {
		t.Fatalf("LocateTranscript: %v", err)
	}
	if got != path {
		t.Errorf("LocateTranscript = %q; want %q", got, path)
	}

	// A worktree with no registry entry misses cleanly (no error).
	got, err = p.LocateTranscript(context.Background(), "proj~other", "/no/such/worktree")
	if err != nil || got != "" {
		t.Errorf("miss = (%q, %v); want (\"\", nil)", got, err)
	}
}

// A non-empty transcriptPath that no longer exists is ErrTranscriptGone —
// even when a live spool dialog exists (issue #92): the transcript read gates
// the overlay, exactly as core returned the read error before applying
// signals, so a gone transcript never resurrects as a dialog-only chat.
func TestReadChat_goneFile(t *testing.T) {
	p := chatProvider(t, t.TempDir(), t.TempDir())
	if _, err := p.ReadChat("", "", filepath.Join(t.TempDir(), "absent.jsonl")); err != provider.ErrTranscriptGone {
		t.Errorf("ReadChat(absent) err = %v; want ErrTranscriptGone", err)
	}
	dir := t.TempDir()
	writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)
	if _, err := p.ReadChat("run_1", dir, filepath.Join(dir, "absent.jsonl")); err != provider.ErrTranscriptGone {
		t.Errorf("ReadChat(absent, live spool dialog) err = %v; want ErrTranscriptGone", err)
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
	chat, err := p.ReadChat("run_1", dir, transcript)
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
	chat, err := p.ReadChat("run_1", dir, "")
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
	empty, err := p.ReadChat("run_none", dir, "")
	if err != nil {
		t.Fatalf("ReadChat(no transcript, no spool): %v", err)
	}
	if empty.State != provider.StateIdle || empty.PendingDialog != nil || len(empty.Messages) != 0 {
		t.Errorf("chat = %+v; want an idle empty chat", empty)
	}
}

// runtimeDir == "" is the transcript-only degradation (ended runs, or no
// runtime dir configured — the same gate core's liveSignals() applied): spool
// and marker files on disk are never consulted, state comes from the
// transcript alone (issue #92).
func TestReadChat_emptyRuntimeDirReadsTranscriptOnly(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)
	writeSpool(t, dir, stateSubdir, "run_1", `{"notification_type":"permission_prompt"}`)
	// A user-text tail derives working — distinct from both the dialog's
	// question and the marker's needs_input, so any overlay would be visible.
	transcript := filepath.Join(dir, "sess-live.jsonl")
	if err := os.WriteFile(transcript,
		[]byte(line(`{"type":"user","message":{"role":"user","content":"go"}}`)), 0o600); err != nil {
		t.Fatal(err)
	}
	chat, err := p.ReadChat("run_1", "", transcript)
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
	chat, err := p.ReadChat("run_1", dir, transcript)
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

// Blocked-marker precedence (issue #92, decision 7 semantics unchanged): a
// live marker forces needs_input over a non-question transcript state, but a
// pending question — transcript-derived — outranks it (the marker is ignored,
// never demoting a question the operator must answer).
func TestReadChat_blockedMarkerPrecedence(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	t0 := time.Now().Add(-time.Hour)
	// Marker + no dialog: the working tail is overridden to needs_input. The
	// transcript is older than the marker so the marker is current.
	working := filepath.Join(dir, "sess-live.jsonl")
	writeFileWithModTime(t, working, line(`{"type":"user","message":{"role":"user","content":"go"}}`), t0)
	writeSpool(t, dir, stateSubdir, "run_1", `{"notification_type":"permission_prompt"}`)
	chat, err := p.ReadChat("run_1", dir, working)
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	if chat.State != provider.StateNeedsInput {
		t.Errorf("marker over working tail: state = %q; want %q", chat.State, provider.StateNeedsInput)
	}

	// Marker while the transcript tail is a pending dialog: the question wins.
	pending := filepath.Join(dir, "sess-pending.jsonl")
	writeFileWithModTime(t, pending, line(colorUseLine), t0)
	chat, err = p.ReadChat("run_1", dir, pending)
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	if chat.State != provider.StateQuestion || chat.PendingDialog != nil {
		t.Errorf("marker vs transcript question: chat = %+v; want StateQuestion with nil PendingDialog", chat)
	}
}

// Spool dialog + blocked marker together: the dialog wins — first in the
// composition order, exactly as core applied it (issue #92).
func TestReadChat_spoolDialogWinsOverMarker(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)
	writeSpool(t, dir, stateSubdir, "run_1", `{"notification_type":"permission_prompt"}`)
	chat, err := p.ReadChat("run_1", dir, "")
	if err != nil {
		t.Fatalf("ReadChat: %v", err)
	}
	if chat.State != provider.StateQuestion || chat.PendingDialog == nil {
		t.Errorf("chat = %+v; want the dialog (StateQuestion + PendingDialog) over the marker", chat)
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
// marker still forces StateNeedsInput, in ReadChat's unchanged composition
// order. A waiting permission prompt or question must never hide behind
// "working on background tasks".
func TestReadChat_liveSignalsOutrankPendingHold(t *testing.T) {
	p := spoolTestProvider(t)
	dir := t.TempDir()
	t0 := time.Now().Add(-time.Hour)
	// A transcript tail that holds working: async launch + turn-ending text.
	// Named sess-live to match prePayload's session_id (dialog staleness key),
	// and older than the spool files so the blocked marker reads as current.
	transcript := filepath.Join(dir, "sess-live.jsonl")
	writeFileWithModTime(t, transcript,
		line(agentUseLine)+line(agentAsyncResultLine)+line(turnEndTextLine), t0)

	chat, err := p.ReadChat("run_1", dir, transcript)
	if err != nil {
		t.Fatalf("ReadChat(no signals): %v", err)
	}
	if chat.State != provider.StateWorking {
		t.Fatalf("no signals: state = %q; want %q (the pending hold)", chat.State, provider.StateWorking)
	}

	// A live spool dialog wins over the hold.
	writeSpool(t, dir, dialogsSubdir, "run_1", prePayload)
	chat, err = p.ReadChat("run_1", dir, transcript)
	if err != nil {
		t.Fatalf("ReadChat(spool dialog): %v", err)
	}
	if chat.State != provider.StateQuestion || chat.PendingDialog == nil {
		t.Errorf("spool dialog over hold: chat = %+v; want StateQuestion with PendingDialog", chat)
	}

	// Dialog gone, blocked marker present (newer than the transcript): the
	// marker's needs_input wins over the hold too.
	if err := os.Remove(filepath.Join(dir, dialogsSubdir, "run_1.json")); err != nil {
		t.Fatal(err)
	}
	writeSpool(t, dir, stateSubdir, "run_1", `{"notification_type":"permission_prompt"}`)
	chat, err = p.ReadChat("run_1", dir, transcript)
	if err != nil {
		t.Fatalf("ReadChat(blocked marker): %v", err)
	}
	if chat.State != provider.StateNeedsInput {
		t.Errorf("blocked marker over hold: state = %q; want %q", chat.State, provider.StateNeedsInput)
	}
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
