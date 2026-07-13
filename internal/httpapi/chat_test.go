package httpapi

// httptest coverage for the embedded-chat surface (issue #7): the messages
// window, reply/answer/interrupt, and their guard rails (ended run, pending
// dialog), driven through the fake provider's scriptable chat.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
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
	if len(cmds) != 3 {
		t.Fatalf("commands = %v; want 2 chat-safe (login curated out) + lab pull-base", cmds)
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
	if got := commandsOf(t, decodeBody(t, resp)); len(got) != 3 {
		t.Errorf("cached read commands = %v; want 3", got)
	}
	if n := len(x.prov.CommandsCalls()); n != 1 {
		t.Errorf("Commands called %d times across two reads; want 1 (cached)", n)
	}
}

// No chat-safe commands still serves a non-null empty array (the SPA renders
// "no commands", never crashes on null). Pinned on a server without a pull
// service — with one wired the lab-owned pull-base entry always shows.
func TestAPI_RunCommands_noneChatSafeIsNonNullArray(t *testing.T) {
	x := newInstanceServerMod(t, func(o *Options) { o.Pull = nil })
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

// --- /pull-base lab command (issue #149) -----------------------------------

// pullIdentity configures the global git author identity every real pull
// requires (pull.Service refuses to author a merge commit as nobody).
func pullIdentity(t *testing.T, x *instTestServer) {
	t.Helper()
	ctx := context.Background()
	if err := x.st.SetSetting(ctx, store.SettingGitAuthorName, "Lab Operator"); err != nil {
		t.Fatal(err)
	}
	if err := x.st.SetSetting(ctx, store.SettingGitAuthorEmail, "op@example.invalid"); err != nil {
		t.Fatal(err)
	}
}

// advanceOrigin lands one commit (name/content/subject) on the fixture
// origin's main. gitx.PullBase does its own fail-loud fetch, so no bare-clone
// refresh is needed here (unlike the commits_behind fixture dance).
func advanceOrigin(t *testing.T, x *instTestServer, name, content, subject string) {
	t.Helper()
	writeFile(t, filepath.Join(x.origin, name), content)
	repoGitCmd(t, x.home, x.origin, "add", ".")
	repoGitCmd(t, x.home, x.origin, "commit", "-q", "-m", subject)
}

// runWorktree resolves the run's live worktree path from the store.
func runWorktree(t *testing.T, x *instTestServer, runID string) string {
	t.Helper()
	run, err := x.st.RunByID(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return run.WorktreePath
}

// worktreeHead is the worktree's current HEAD sha (the merged/unchanged probe).
func worktreeHead(t *testing.T, x *instTestServer, wt string) string {
	t.Helper()
	return repoGitCmd(t, x.home, wt, "rev-parse", "HEAD")
}

// postReply POSTs text to the run's reply endpoint.
func postReply(t *testing.T, x *instTestServer, runID, text string) *http.Response {
	t.Helper()
	return x.do("POST", "/api/v1/runs/"+runID+"/reply", map[string]any{"text": text}, csrfHeaders(x.ts.URL))
}

// clearCatalog scripts the provider catalog with a role=clear entry (mirrors
// TestAPI_RunCommands) plus a plain chat-safe /compact.
func clearCatalog(x *instTestServer) {
	x.prov.SetCommands([]provider.CommandSpec{
		{Name: "clear", Description: "Clear the conversation", Source: "builtin", Role: provider.CommandRoleClear, ChatSafe: true},
		{Name: "compact", Source: "builtin", ChatSafe: true},
	}, nil)
}

// The commands catalog carries the lab-owned pull-base entry (issue #149):
// appended after the provider's chat-safe entries with source "lab", still
// present on the cached second read (without re-hitting the provider), and
// absent entirely when no pull service is wired.
func TestAPI_RunCommands_labPullBaseEntry(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	x.prov.SetCommands([]provider.CommandSpec{
		{Name: "clear", Source: "builtin", Role: provider.CommandRoleClear, ChatSafe: true},
	}, nil)

	for _, read := range []string{"fresh", "cached"} {
		resp := x.do("GET", "/api/v1/runs/"+runID+"/commands", nil, nil)
		wantStatus(t, resp, http.StatusOK)
		cmds := commandsOf(t, decodeBody(t, resp))
		if len(cmds) != 2 {
			t.Fatalf("%s read: commands = %v; want provider clear + lab pull-base", read, cmds)
		}
		last := cmds[len(cmds)-1]
		if last["name"] != "pull-base" || last["source"] != "lab" || last["chat_safe"] != true {
			t.Errorf("%s read: lab entry = %v; want pull-base/lab/chat-safe", read, last)
		}
	}
	if n := len(x.prov.CommandsCalls()); n != 1 {
		t.Errorf("Commands called %d times across two reads; want 1 (the lab entry never invalidates the cache)", n)
	}

	// Without the pull service the entry vanishes from the catalog.
	y := newInstanceServerMod(t, func(o *Options) { o.Pull = nil })
	yRunID, _ := startRun(t, y)
	y.prov.SetCommands([]provider.CommandSpec{{Name: "compact", Source: "builtin", ChatSafe: true}}, nil)
	resp := y.do("GET", "/api/v1/runs/"+yRunID+"/commands", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	for _, c := range commandsOf(t, decodeBody(t, resp)) {
		if c["name"] == "pull-base" {
			t.Errorf("pull-base present without a pull service: %v", c)
		}
	}
}

// The /pull-base happy path: the lab command merges the advanced origin into
// the run's worktree and pastes exactly one agent message — the digest,
// carrying the sha range, an incoming subject and a filename — while the raw
// command never reaches the provider.
func TestAPI_PullBase_happyPath(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	pullIdentity(t, x)
	advanceOrigin(t, x, "feature.txt", "feature", "add feature x")
	wt := runWorktree(t, x, runID)
	oldHead := worktreeHead(t, x, wt)

	resp := postReply(t, x, runID, "/pull-base")
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	newHead := worktreeHead(t, x, wt)
	if newHead == oldHead {
		t.Fatal("worktree HEAD unchanged; the pull did not land")
	}
	if _, err := os.Stat(filepath.Join(wt, "feature.txt")); err != nil {
		t.Errorf("merged file missing from the worktree: %v", err)
	}
	replies := x.prov.Replies()
	if len(replies) != 1 {
		t.Fatalf("replies = %v; want exactly one (the digest)", replies)
	}
	digest := replies[0]
	if digest == "/pull-base" {
		t.Fatal("the raw /pull-base command reached the provider")
	}
	wantRange := oldHead[:12] + ".." + newHead[:12]
	for _, part := range []string{wantRange, "add feature x", "feature.txt"} {
		if !strings.Contains(digest, part) {
			t.Errorf("digest missing %q:\n%s", part, digest)
		}
	}
}

// /pull-base takes no arguments: any argument form is a 400 that forwards
// nothing to the provider.
func TestAPI_PullBase_argsRejected(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)

	resp := postReply(t, x, runID, "/pull-base now")
	wantStatus(t, resp, http.StatusBadRequest)
	if body := decodeBody(t, resp); body["error"] != "pull-base takes no arguments" {
		t.Errorf("error = %v; want the no-arguments refusal", body["error"])
	}
	if got := x.prov.Replies(); len(got) != 0 {
		t.Errorf("replies = %v; want none", got)
	}
}

// Without a pull service the lab command is refused (503) — and still never
// forwarded: the never-reaches-the-provider guarantee holds even unwired.
func TestAPI_PullBase_unavailableWithoutService(t *testing.T) {
	x := newInstanceServerMod(t, func(o *Options) { o.Pull = nil })
	runID, _ := startRun(t, x)

	resp := postReply(t, x, runID, "/pull-base")
	wantStatus(t, resp, http.StatusServiceUnavailable)
	if body := decodeBody(t, resp); body["error"] != "pull-base is not available" {
		t.Errorf("error = %v; want the unavailable refusal", body["error"])
	}
	if got := x.prov.Replies(); len(got) != 0 {
		t.Errorf("replies = %v; want none", got)
	}
}

// A POSITIVELY working agent refuses the pull (409 busy): nothing merges and
// nothing reaches the provider.
func TestAPI_PullBase_busyAgent(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	pullIdentity(t, x)
	advanceOrigin(t, x, "feature.txt", "feature", "add feature x")
	x.prov.SetTranscriptPath("/transcript.jsonl")
	x.prov.SetChat(provider.Chat{State: provider.StateWorking})
	wt := runWorktree(t, x, runID)
	oldHead := worktreeHead(t, x, wt)

	resp := postReply(t, x, runID, "/pull-base")
	wantStatus(t, resp, http.StatusConflict)
	if body := decodeBody(t, resp); !strings.Contains(fmt.Sprint(body["error"]), "busy") {
		t.Errorf("error = %v; want a busy message", body["error"])
	}
	if got := x.prov.Replies(); len(got) != 0 {
		t.Errorf("replies = %v; want none", got)
	}
	if worktreeHead(t, x, wt) != oldHead {
		t.Error("worktree HEAD moved despite the busy refusal")
	}
}

// A conflicting local commit → 409 naming the conflicted file, worktree HEAD
// untouched (gitx aborts the merge), nothing forwarded.
func TestAPI_PullBase_conflict(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	pullIdentity(t, x)
	wt := runWorktree(t, x, runID)
	// The run commits clash.txt locally; origin lands different content at
	// the same path — an add/add conflict.
	writeFile(t, filepath.Join(wt, "clash.txt"), "local side")
	repoGitCmd(t, x.home, wt, "add", ".")
	repoGitCmd(t, x.home, wt, "commit", "-q", "-m", "local clash")
	advanceOrigin(t, x, "clash.txt", "origin side", "origin clash")
	oldHead := worktreeHead(t, x, wt)

	resp := postReply(t, x, runID, "/pull-base")
	wantStatus(t, resp, http.StatusConflict)
	if body := decodeBody(t, resp); !strings.Contains(fmt.Sprint(body["error"]), "clash.txt") {
		t.Errorf("conflict error does not name the file: %v", body["error"])
	}
	if worktreeHead(t, x, wt) != oldHead {
		t.Error("worktree HEAD moved despite the conflict abort")
	}
	if got := x.prov.Replies(); len(got) != 0 {
		t.Errorf("replies = %v; want none", got)
	}
}

// Nothing to pull → 200 with the up-to-date notice and NO agent message.
func TestAPI_PullBase_upToDate(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	pullIdentity(t, x)

	resp := postReply(t, x, runID, "/pull-base")
	wantStatus(t, resp, http.StatusOK)
	if body := decodeBody(t, resp); body["notice"] != "Already up to date with origin/main." {
		t.Errorf("notice = %v; want the exact up-to-date wording", body["notice"])
	}
	if got := x.prov.Replies(); len(got) != 0 {
		t.Errorf("replies = %v; want none (no digest when nothing came in)", got)
	}
}

// A dialog racing in between the merge and the digest paste degrades to a 200
// notice: the merge is real (worktree updated), only the context repair
// failed — never an error.
func TestAPI_PullBase_digestUndelivered(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	pullIdentity(t, x)
	advanceOrigin(t, x, "feature.txt", "feature", "add feature x")
	x.prov.SetPendingDialog(&provider.Dialog{ToolID: "toolu_1", Kind: provider.DialogKindQuestion,
		Answerable: true, Prompt: "Pick?", Options: []provider.DialogOption{{Label: "A"}}})
	wt := runWorktree(t, x, runID)

	resp := postReply(t, x, runID, "/pull-base")
	wantStatus(t, resp, http.StatusOK)
	notice, _ := decodeBody(t, resp)["notice"].(string)
	if !strings.Contains(notice, "not delivered") {
		t.Errorf("notice = %q; want the undelivered-digest wording", notice)
	}
	if _, err := os.Stat(filepath.Join(wt, "feature.txt")); err != nil {
		t.Errorf("merge did not land despite the notice: %v", err)
	}
	if got := x.prov.Replies(); len(got) != 0 {
		t.Errorf("replies = %v; want none (the digest was refused by the dialog gate)", got)
	}
}

// A role=clear reply pulls the base BEFORE forwarding the clear: worktree
// updated, the clear forwarded verbatim, and NO digest (post-clear nothing is
// stale) — plain 204.
func TestAPI_ClearHook_pullsBeforeClear(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	pullIdentity(t, x)
	clearCatalog(x)
	advanceOrigin(t, x, "feature.txt", "feature", "add feature x")
	wt := runWorktree(t, x, runID)

	resp := postReply(t, x, runID, "/clear")
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	if _, err := os.Stat(filepath.Join(wt, "feature.txt")); err != nil {
		t.Errorf("pull did not land before the clear: %v", err)
	}
	if got := x.prov.Replies(); len(got) != 1 || got[0] != "/clear" {
		t.Errorf("replies = %v; want exactly [/clear] (forwarded, no digest)", got)
	}
}

// A working agent never blocks its clear: the pull is skipped, the clear
// forwards, and the operator learns via the 200 notice.
func TestAPI_ClearHook_busySkipsPull(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	pullIdentity(t, x)
	clearCatalog(x)
	advanceOrigin(t, x, "feature.txt", "feature", "add feature x")
	x.prov.SetTranscriptPath("/transcript.jsonl")
	x.prov.SetChat(provider.Chat{State: provider.StateWorking})
	wt := runWorktree(t, x, runID)
	oldHead := worktreeHead(t, x, wt)

	resp := postReply(t, x, runID, "/clear")
	wantStatus(t, resp, http.StatusOK)
	notice, _ := decodeBody(t, resp)["notice"].(string)
	if !strings.Contains(notice, "agent is busy") {
		t.Errorf("notice = %q; want the busy wording", notice)
	}
	if worktreeHead(t, x, wt) != oldHead {
		t.Error("worktree updated despite the busy skip")
	}
	if got := x.prov.Replies(); len(got) != 1 || got[0] != "/clear" {
		t.Errorf("replies = %v; want exactly [/clear]", got)
	}
}

// A failing pull is loud (200 notice) but never blocks the clear.
func TestAPI_ClearHook_pullFailureStillClears(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	pullIdentity(t, x)
	clearCatalog(x)
	// Break the fetch: the bare reference clone's origin points nowhere.
	bareDir := filepath.Join(x.reposDir, x.repo.ID+".git")
	repoGitCmd(t, x.home, bareDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))

	resp := postReply(t, x, runID, "/clear")
	wantStatus(t, resp, http.StatusOK)
	notice, _ := decodeBody(t, resp)["notice"].(string)
	if !strings.Contains(notice, "Base not pulled") {
		t.Errorf("notice = %q; want 'Base not pulled …'", notice)
	}
	if got := x.prov.Replies(); len(got) != 1 || got[0] != "/clear" {
		t.Errorf("replies = %v; want exactly [/clear]", got)
	}
}

// A non-clear command forwards verbatim with a 204, byte-for-byte like before
// lab commands existed (plain text is covered by TestAPI_ChatReplyAndInterrupt).
func TestAPI_ChatReply_nonClearCommandForwards(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	clearCatalog(x)

	resp := postReply(t, x, runID, "/compact")
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	if got := x.prov.Replies(); len(got) != 1 || got[0] != "/compact" {
		t.Errorf("replies = %v; want exactly [/compact]", got)
	}
}

// An arg-bearing clear (claude's /clear carries ArgHint "[name]", and the
// autocomplete gesture model invites typing one) is still THE clear command:
// isClearCommand must match on the command word, not the whole string, or the
// pull silently never happens (issue #151 — the bug #149 was meant to close).
// The argument survives into the forward untouched.
func TestAPI_ClearHook_argBearingClearPulls(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	pullIdentity(t, x)
	clearCatalog(x)
	advanceOrigin(t, x, "feature.txt", "feature", "add feature x")
	wt := runWorktree(t, x, runID)

	resp := postReply(t, x, runID, "/clear my-session")
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	if _, err := os.Stat(filepath.Join(wt, "feature.txt")); err != nil {
		t.Errorf("pull did not land before the clear: %v", err)
	}
	if got := x.prov.Replies(); len(got) != 1 || got[0] != "/clear my-session" {
		t.Errorf("replies = %v; want exactly [/clear my-session] (argument preserved, no digest)", got)
	}
}

// A prefix-confusion guard: "/clearx" is not "/clear" plus an argument — its
// command word is "/clearx" whole — so it must NOT trip the clear hook. Pins
// that commandWord's whitespace cut, not a HasPrefix, is what isClearCommand
// matches on.
func TestAPI_ClearHook_prefixIsNotClear(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)
	pullIdentity(t, x)
	clearCatalog(x)
	advanceOrigin(t, x, "feature.txt", "feature", "add feature x")
	wt := runWorktree(t, x, runID)
	oldHead := worktreeHead(t, x, wt)

	resp := postReply(t, x, runID, "/clearx")
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()

	if worktreeHead(t, x, wt) != oldHead {
		t.Error("worktree HEAD moved; /clearx wrongly matched the clear hook")
	}
	if got := x.prov.Replies(); len(got) != 1 || got[0] != "/clearx" {
		t.Errorf("replies = %v; want exactly [/clearx]", got)
	}
}

// /pull-base takes no arguments regardless of which whitespace separates the
// command word from the argument — a tab must 400 exactly like a space does,
// since commandWord (not a literal " " split) is what dispatch matches on.
func TestAPI_PullBase_tabArgRejected(t *testing.T) {
	x := newInstanceServer(t)
	runID, _ := startRun(t, x)

	resp := postReply(t, x, runID, "/pull-base\targ")
	wantStatus(t, resp, http.StatusBadRequest)
	if body := decodeBody(t, resp); body["error"] != "pull-base takes no arguments" {
		t.Errorf("error = %v; want the no-arguments refusal", body["error"])
	}
	if got := x.prov.Replies(); len(got) != 0 {
		t.Errorf("replies = %v; want none", got)
	}
}
