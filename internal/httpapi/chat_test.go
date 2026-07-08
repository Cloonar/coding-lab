package httpapi

// httptest coverage for the embedded-chat surface (issue #7): the messages
// window, reply/answer/interrupt, and their guard rails (ended run, pending
// dialog), driven through the fake provider's scriptable chat.

import (
	"fmt"
	"net/http"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// startRun starts an instance and returns its run id + session name.
func startRun(t *testing.T, x *instTestServer) (string, string) {
	t.Helper()
	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/instances", map[string]any{}, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusCreated)
	run := decodeBody(t, resp)
	return run["id"].(string), run["session_name"].(string)
}

func TestAPI_ChatMessages_window(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)

	x.prov.SetTranscriptPath("/transcript.jsonl")
	x.prov.SetChat(provider.Chat{
		State:  provider.StateNeedsInput,
		Cursor: 3,
		Messages: []provider.Message{
			{Seq: 1, Kind: provider.MessageText, Role: "user", Text: "hi"},
			{Seq: 2, Kind: provider.MessageText, Role: "assistant", Text: "working"},
			{Seq: 3, Kind: provider.MessageText, Role: "assistant", Text: "done"},
		},
	})

	resp := x.do("GET", "/api/v1/runs/"+runID+"/messages", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	if body["state"] != provider.StateNeedsInput {
		t.Errorf("state = %v; want needs_input", body["state"])
	}
	if body["transcript"] != "available" {
		t.Errorf("transcript = %v; want available", body["transcript"])
	}
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) != 3 {
		t.Errorf("messages = %v; want 3", body["messages"])
	}

	// after=2 returns only the tail.
	resp = x.do("GET", "/api/v1/runs/"+runID+"/messages?after=2", nil, nil)
	body = decodeBody(t, resp)
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("after=2 returned %d messages; want 1", len(msgs))
	}
	if m := msgs[0].(map[string]any); m["seq"].(float64) != 3 {
		t.Errorf("after=2 tail seq = %v; want 3", m["seq"])
	}
}

// The live PreToolUse spool (ADR-0020) surfaces as the top-level pending_dialog
// field alongside state:"question" — NOT as a message in the stream.
func TestAPI_ChatMessages_pendingDialog(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)

	x.prov.SetTranscriptPath("/transcript.jsonl")
	// The transcript alone derives 'working' with no dialog message.
	x.prov.SetChat(provider.Chat{State: provider.StateWorking, Cursor: 1,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: "…"}}})
	x.prov.SetPendingDialog(&provider.Dialog{ToolID: "toolu_1", Kind: provider.DialogKindQuestion, Answerable: true,
		Prompt: "Pick?", Options: []provider.DialogOption{{Label: "A"}, {Label: "Other", IsOther: true}}})

	resp := x.do("GET", "/api/v1/runs/"+runID+"/messages", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	if body["state"] != provider.StateQuestion {
		t.Errorf("state = %v; want question (the spool overrides transcript working)", body["state"])
	}
	pd, ok := body["pending_dialog"].(map[string]any)
	if !ok {
		t.Fatalf("pending_dialog = %v; want the spool dialog object", body["pending_dialog"])
	}
	if pd["tool_id"] != "toolu_1" || pd["answerable"] != true {
		t.Errorf("pending_dialog = %v; want an answerable dialog toolu_1", pd)
	}
	// The stream stays transcript-derived: the single text message, no dialog.
	if msgs, _ := body["messages"].([]any); len(msgs) != 1 {
		t.Errorf("messages = %v; want only the transcript's 1 message", body["messages"])
	}
}

// The messages response carries a transcript_id, and it changes when the run's
// transcript rotates (a /clear or /rewind → new sessionId → new file) — the
// token the SPA keys its stream reset on (issue #34).
func TestAPI_ChatMessages_transcriptID(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)

	x.prov.SetTranscriptPath("/a.jsonl")
	x.prov.SetChat(provider.Chat{State: provider.StateWorking, Cursor: 1,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: "old"}}})

	resp := x.do("GET", "/api/v1/runs/"+runID+"/messages", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	idA, _ := body["transcript_id"].(string)
	if idA == "" {
		t.Fatalf("transcript_id = %v; want a non-empty id for a located transcript", body["transcript_id"])
	}

	// A /clear rotates the session's transcript → a new file.
	x.prov.SetTranscriptPath("/b.jsonl")
	x.prov.SetChat(provider.Chat{State: provider.StateNeedsInput, Cursor: 1,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "assistant", Text: "new"}}})

	resp = x.do("GET", "/api/v1/runs/"+runID+"/messages", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body = decodeBody(t, resp)
	idB, _ := body["transcript_id"].(string)
	if idB == "" || idB == idA {
		t.Errorf("transcript_id after rotation = %q; want a new non-empty id (was %q)", idB, idA)
	}
}

func TestAPI_ChatMessages_locatingAndGone(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)

	// No transcript yet → locating.
	x.prov.SetTranscriptPath("")
	resp := x.do("GET", "/api/v1/runs/"+runID+"/messages", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if body := decodeBody(t, resp); body["transcript"] != "locating" {
		t.Errorf("transcript = %v; want locating", body["transcript"])
	}

	// Retired transcript → gone.
	x.prov.SetTranscriptPath("/transcript.jsonl")
	x.prov.SetReadError(provider.ErrTranscriptGone)
	resp = x.do("GET", "/api/v1/runs/"+runID+"/messages", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if body := decodeBody(t, resp); body["transcript"] != "gone" {
		t.Errorf("transcript = %v; want gone", body["transcript"])
	}
}

func TestAPI_ChatReplyAndInterrupt(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	x.prov.SetTranscriptPath("/transcript.jsonl")
	x.prov.SetChat(provider.Chat{State: provider.StateWorking})
	h := csrfHeaders(x.ts.URL)

	resp := x.do("POST", "/api/v1/runs/"+runID+"/reply", map[string]any{"text": "keep going"}, h)
	wantStatus(t, resp, http.StatusNoContent)
	if got := x.prov.Replies(); len(got) != 1 || got[0] != "keep going" {
		t.Errorf("replies = %v; want [keep going]", got)
	}

	resp = x.do("POST", "/api/v1/runs/"+runID+"/interrupt", map[string]any{}, h)
	wantStatus(t, resp, http.StatusNoContent)
	if x.prov.Interrupts() != 1 {
		t.Errorf("interrupts = %d; want 1", x.prov.Interrupts())
	}
}

func TestAPI_ChatReply_lockedWhileDialogPending(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	x.prov.SetTranscriptPath("/transcript.jsonl")
	x.prov.SetChat(provider.Chat{
		State: provider.StateQuestion,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageDialog, Dialog: &provider.Dialog{
			ToolID: "toolu_1", Answerable: true, Options: []provider.DialogOption{{Label: "a"}, {Label: "b"}},
		}}},
	})

	resp := x.do("POST", "/api/v1/runs/"+runID+"/reply", map[string]any{"text": "stray"}, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusConflict)
}

func TestAPI_ChatAnswer(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	x.prov.SetTranscriptPath("/transcript.jsonl")
	x.prov.SetChat(provider.Chat{
		State: provider.StateQuestion,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageDialog, Dialog: &provider.Dialog{
			ToolID: "toolu_1", Answerable: true, Options: []provider.DialogOption{{Label: "a"}, {Label: "b"}},
		}}},
	})
	h := csrfHeaders(x.ts.URL)

	// Matching tool_id answers.
	resp := x.do("POST", "/api/v1/runs/"+runID+"/answer", map[string]any{"tool_id": "toolu_1", "index": 1}, h)
	wantStatus(t, resp, http.StatusNoContent)
	if got := x.prov.Answers(); len(got) != 1 || got[0].Index != 1 {
		t.Errorf("answers = %v; want one with index 1", got)
	}

	// Stale tool_id is a 409.
	resp = x.do("POST", "/api/v1/runs/"+runID+"/answer", map[string]any{"tool_id": "toolu_old", "index": 0}, h)
	wantStatus(t, resp, http.StatusConflict)

	// Missing tool_id is a 400 — an unidentified answer may not drive a
	// picker by blind index.
	resp = x.do("POST", "/api/v1/runs/"+runID+"/answer", map[string]any{"index": 0}, h)
	wantStatus(t, resp, http.StatusBadRequest)
}

// commandsOf extracts the {"commands":[…]} array as decoded maps.
func commandsOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["commands"].([]any)
	if !ok {
		t.Fatalf("commands is not a JSON array (null?): %v", body["commands"])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		out = append(out, e.(map[string]any))
	}
	return out
}

// The composer autocomplete source serves only chat-safe commands (issue #51
// decision 5) and is cached per run: the second read does not re-hit the
// provider's filesystem scan.
func TestAPI_RunCommands(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	x.prov.SetCommands([]provider.CommandSpec{
		{Name: "clear", Description: "Clear the conversation", Source: "builtin", Role: provider.CommandRoleClear, ChatSafe: true},
		{Name: "compact", Source: "builtin", ChatSafe: true},
		{Name: "login", Source: "builtin", ChatSafe: false}, // would strand the TUI in a picker
	}, nil)

	resp := x.do("GET", "/api/v1/runs/"+runID+"/commands", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	cmds := commandsOf(t, decodeBody(t, resp))
	if len(cmds) != 2 {
		t.Fatalf("commands = %v; want 2 chat-safe (login curated out)", cmds)
	}
	if cmds[0]["name"] != "clear" || cmds[0]["role"] != provider.CommandRoleClear || cmds[1]["name"] != "compact" {
		t.Errorf("commands = %v; want clear(role=clear)+compact", cmds)
	}
	for _, c := range cmds {
		if c["name"] == "login" {
			t.Errorf("chat-unsafe command leaked: %v", c)
		}
	}

	// A second read is served from cache — the provider scan ran exactly once.
	resp = x.do("GET", "/api/v1/runs/"+runID+"/commands", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if got := commandsOf(t, decodeBody(t, resp)); len(got) != 2 {
		t.Errorf("cached read commands = %v; want 2", got)
	}
	if n := len(x.prov.CommandsCalls()); n != 1 {
		t.Errorf("Commands called %d times across two reads; want 1 (cached)", n)
	}
}

// No chat-safe commands still serves a non-null empty array (the SPA renders
// "no commands", never crashes on null).
func TestAPI_RunCommands_noneChatSafeIsNonNullArray(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	x.prov.SetCommands([]provider.CommandSpec{
		{Name: "login", Source: "builtin", ChatSafe: false},
	}, nil)

	resp := x.do("GET", "/api/v1/runs/"+runID+"/commands", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if cmds := commandsOf(t, decodeBody(t, resp)); len(cmds) != 0 {
		t.Errorf("commands = %v; want an empty (but non-null) array", cmds)
	}
}

func TestAPI_RunCommands_unknownRun404(t *testing.T) {
	x := newInstanceServer(t)
	resp := x.do("GET", "/api/v1/runs/run_missing/commands", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
}

// A provider scan error is a 500 and is never cached — the next read retries.
func TestAPI_RunCommands_providerError(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	x.prov.SetCommands(nil, fmt.Errorf("scanning command dirs"))

	resp := x.do("GET", "/api/v1/runs/"+runID+"/commands", nil, nil)
	wantStatus(t, resp, http.StatusInternalServerError)
	_ = resp.Body.Close()

	// A retry re-hits the provider (the error was not cached).
	resp = x.do("GET", "/api/v1/runs/"+runID+"/commands", nil, nil)
	wantStatus(t, resp, http.StatusInternalServerError)
	_ = resp.Body.Close()
	if n := len(x.prov.CommandsCalls()); n != 2 {
		t.Errorf("Commands called %d times; want 2 (errors are not cached)", n)
	}
}

// A multi-question dialog is answered in one submit via answers[] (issue #51
// decision 3): each per-question answer threads into the provider's recorded
// DialogAnswer.Answers unchanged.
func TestAPI_ChatAnswer_multiQuestion(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	x.prov.SetTranscriptPath("/transcript.jsonl")
	x.prov.SetChat(provider.Chat{
		State: provider.StateQuestion,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageDialog, Dialog: &provider.Dialog{
			ToolID: "toolu_multi", Kind: provider.DialogKindQuestion, Answerable: true, Prompt: "2 questions",
			Questions: []provider.Question{
				{Text: "Q1", Options: []provider.DialogOption{{Label: "a"}, {Label: "b"}}},
				{Text: "Q2", MultiSelect: true, Options: []provider.DialogOption{{Label: "c"}, {Label: "d"}, {Label: "Other", IsOther: true}}},
			},
		}}},
	})

	body := map[string]any{
		"tool_id": "toolu_multi",
		"answers": []map[string]any{
			{"index": 1},
			{"selected": []int{0, 2}, "other_text": "custom"},
		},
	}
	resp := x.do("POST", "/api/v1/runs/"+runID+"/answer", body, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	got := x.prov.Answers()
	if len(got) != 1 {
		t.Fatalf("answers recorded = %d; want 1", len(got))
	}
	a := got[0]
	if len(a.Answers) != 2 {
		t.Fatalf("per-question answers = %+v; want 2", a.Answers)
	}
	if a.Answers[0].Index != 1 {
		t.Errorf("answers[0].Index = %d; want 1", a.Answers[0].Index)
	}
	if len(a.Answers[1].Selected) != 2 || a.Answers[1].Selected[0] != 0 || a.Answers[1].Selected[1] != 2 {
		t.Errorf("answers[1].Selected = %v; want [0 2]", a.Answers[1].Selected)
	}
	if a.Answers[1].OtherText != "custom" {
		t.Errorf("answers[1].OtherText = %q; want custom", a.Answers[1].OtherText)
	}
}

func TestAPI_ChatSessionGone_conflict(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	x.prov.SetTranscriptPath("/transcript.jsonl")
	x.prov.SetChat(provider.Chat{State: provider.StateWorking})
	// The run row still says active but the tmux session died out from under
	// lab — the send surfaces the runner's typed error, not an opaque 500.
	x.prov.SetReplyError(fmt.Errorf("paste reply: %w", tmuxx.ErrSessionNotFound))

	resp := x.do("POST", "/api/v1/runs/"+runID+"/reply", map[string]any{"text": "hello"}, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusConflict)
}

func TestAPI_ChatEndedRun_readableButNotWritable(t *testing.T) {
	x := newInstanceServer(t)
	runID, session := startRun(t, x)
	x.prov.SetTranscriptPath("/transcript.jsonl")
	x.prov.SetChat(provider.Chat{State: provider.StateNeedsInput,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "user", Text: "hi"}}})

	// Read once while live so the located path persists — ended runs never
	// locate (the worktree's live claude would belong to a successor run).
	resp := x.do("GET", "/api/v1/runs/"+runID+"/messages", nil, nil)
	wantStatus(t, resp, http.StatusOK)

	// Stop the instance (terminal outcome).
	resp = x.do("DELETE", "/api/v1/instances/"+session, nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusOK)

	// Messages still read (read-through) but state is ended.
	resp = x.do("GET", "/api/v1/runs/"+runID+"/messages", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	if body["state"] != provider.StateEnded {
		t.Errorf("ended run state = %v; want ended", body["state"])
	}
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) != 1 {
		t.Errorf("ended run messages = %v; want the persisted transcript's 1 message", body["messages"])
	}

	// The composer is closed.
	resp = x.do("POST", "/api/v1/runs/"+runID+"/reply", map[string]any{"text": "hello"}, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusConflict)
}

func TestAPI_ChatEndedRun_neverLocatedIsGone(t *testing.T) {
	x := newInstanceServer(t)
	runID, session := startRun(t, x)
	// Locatable the whole time — but nothing read the chat while the run was
	// live, so no path persisted. After the end, locating would capture a
	// successor run's transcript; the API must serve "gone" instead.
	x.prov.SetTranscriptPath("/successor.jsonl")
	x.prov.SetChat(provider.Chat{State: provider.StateWorking,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "user", Text: "not yours"}}})

	resp := x.do("DELETE", "/api/v1/instances/"+session, nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusOK)

	resp = x.do("GET", "/api/v1/runs/"+runID+"/messages", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	body := decodeBody(t, resp)
	if body["transcript"] != "gone" {
		t.Errorf("transcript = %v; want gone", body["transcript"])
	}
	if body["state"] != provider.StateEnded {
		t.Errorf("state = %v; want ended", body["state"])
	}
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) != 0 {
		t.Errorf("messages = %v; want none (never this run's transcript)", body["messages"])
	}
}

func TestAPI_ChatMessages_unknownRun404(t *testing.T) {
	x := newInstanceServer(t)
	resp := x.do("GET", "/api/v1/runs/run_missing/messages", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
}
