package httpapi

// httptest coverage for the embedded-chat surface (issue #7): the messages
// window, reply/answer/interrupt, and their guard rails (ended run, pending
// dialog), driven through the fake provider's scriptable chat.

import (
	"net/http"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
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
}

func TestAPI_ChatEndedRun_readableButNotWritable(t *testing.T) {
	x := newInstanceServer(t)
	runID, session := startRun(t, x)
	x.prov.SetTranscriptPath("/transcript.jsonl")
	x.prov.SetChat(provider.Chat{State: provider.StateNeedsInput,
		Messages: []provider.Message{{Seq: 1, Kind: provider.MessageText, Role: "user", Text: "hi"}}})

	// Stop the instance (terminal outcome).
	resp := x.do("DELETE", "/api/v1/instances/"+session, nil, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusOK)

	// Messages still read (read-through) but state is ended.
	resp = x.do("GET", "/api/v1/runs/"+runID+"/messages", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if body := decodeBody(t, resp); body["state"] != provider.StateEnded {
		t.Errorf("ended run state = %v; want ended", body["state"])
	}

	// The composer is closed.
	resp = x.do("POST", "/api/v1/runs/"+runID+"/reply", map[string]any{"text": "hello"}, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusConflict)
}

func TestAPI_ChatMessages_unknownRun404(t *testing.T) {
	x := newInstanceServer(t)
	resp := x.do("GET", "/api/v1/runs/run_missing/messages", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
}
