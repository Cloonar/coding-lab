package codex

// Locate + parse tests over the rollout grammar. The testdata fixtures are
// TRIMMED from real 0.133.0 rollouts (issue #87 live spikes, 2026-07-10):
// structural fields verbatim, base_instructions replaced by a one-line stub,
// long tool outputs shortened.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// writeRollout drops a rollout file whose first line is a session_meta
// record for cwd under sessionsDir/<yyyy>/<mm>/<dd>/<name>.
func writeRollout(t *testing.T, sessionsDir, yyyy, mm, dd, name, cwd string) string {
	t.Helper()
	dir := filepath.Join(sessionsDir, yyyy, mm, dd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	line := `{"timestamp":"2026-07-10T00:00:00.000Z","type":"session_meta","payload":{"id":"x","cwd":"` + cwd + `","originator":"codex-tui","cli_version":"0.133.0"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The newest rollout whose session_meta cwd matches the worktree wins:
// date directories newest-first, files within a day by filename desc. The
// filename timestamp is LOCAL time (payload timestamps are UTC), so recency
// is decided purely by the tree/filename order, never payload times. /new
// rotates to a fresh rollout file, so this newest-first match IS the
// clear-epoch identity.
func TestLocateTranscript_newestFirstCwdMatch(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	ctx := context.Background()
	const wtA, wtB = "/work/a", "/work/b"

	writeRollout(t, p.sessionsDir, "2026", "07", "09", "rollout-2026-07-09T23-59-59-aaa.jsonl", wtA)
	wantB := writeRollout(t, p.sessionsDir, "2026", "07", "10", "rollout-2026-07-10T01-00-00-bbb.jsonl", wtB)
	wantA := writeRollout(t, p.sessionsDir, "2026", "07", "10", "rollout-2026-07-10T02-00-00-ccc.jsonl", wtA)

	if got, err := p.LocateTranscript(ctx, "sess", wtA); err != nil || got != wantA {
		t.Errorf("LocateTranscript(A) = %q, %v; want the newest A rollout %q", got, err, wantA)
	}
	if got, err := p.LocateTranscript(ctx, "sess", wtB); err != nil || got != wantB {
		t.Errorf("LocateTranscript(B) = %q, %v; want %q", got, err, wantB)
	}

	// A fresh rollout in the same day with a later local timestamp — the
	// /new rotation — immediately becomes A's identity.
	rotated := writeRollout(t, p.sessionsDir, "2026", "07", "10", "rollout-2026-07-10T03-00-00-ddd.jsonl", wtA)
	if got, _ := p.LocateTranscript(ctx, "sess", wtA); got != rotated {
		t.Errorf("LocateTranscript(A) after rotation = %q; want %q", got, rotated)
	}
}

func TestLocateTranscript_misses(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	ctx := context.Background()

	// Missing sessions tree: "" with no error (nothing rolled out yet).
	if got, err := p.LocateTranscript(ctx, "sess", "/work/a"); err != nil || got != "" {
		t.Errorf("LocateTranscript on missing tree = %q, %v; want \"\", nil", got, err)
	}

	// A malformed first line and a non-rollout file are skipped quietly.
	dir := filepath.Join(p.sessionsDir, "2026", "07", "10")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-07-10T01-00-00-eee.jsonl"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("irrelevant"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := p.LocateTranscript(ctx, "sess", "/work/a"); err != nil || got != "" {
		t.Errorf("LocateTranscript over junk = %q, %v; want \"\", nil", got, err)
	}
}

func TestReadTranscript_gone(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	if _, err := p.ReadTranscript(filepath.Join(t.TempDir(), "vanished.jsonl")); !errors.Is(err, provider.ErrTranscriptGone) {
		t.Errorf("ReadTranscript(missing) err = %v; want ErrTranscriptGone", err)
	}
}

func parseFixture(t *testing.T, name string) provider.Chat {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	chat, err := ParseTranscript(f)
	if err != nil {
		t.Fatalf("ParseTranscript(%s): %v", name, err)
	}
	return chat
}

// The plain fixture (trimmed real exec run): synthetic response_item
// messages are skipped, prose arrives via user_message/agent_message events,
// the exec chip resolves by call_id, only the non-empty reasoning summary
// renders as thinking, and task_complete derives needs_input.
func TestParseTranscript_plainFixture(t *testing.T) {
	chat := parseFixture(t, "rollout-plain.jsonl")

	type row struct {
		kind, role, text string
		thinking         bool
	}
	want := []row{
		{kind: provider.MessageText, role: "user", text: "Create a file hello.txt containing the word hi, then reply DONE."},
		{kind: provider.MessageText, role: "assistant", text: "I’ll create the file, check the repo state, then reply."},
		{kind: provider.MessageTool},
		{kind: provider.MessageText, role: "assistant", text: "Verifying the file landed before answering.", thinking: true},
		{kind: provider.MessageText, role: "assistant", text: "DONE"},
	}
	if len(chat.Messages) != len(want) {
		t.Fatalf("messages = %d rows %+v; want %d", len(chat.Messages), chat.Messages, len(want))
	}
	for i, w := range want {
		m := chat.Messages[i]
		if m.Kind != w.kind || m.Role != w.role || m.Thinking != w.thinking {
			t.Errorf("msg[%d] = kind %q role %q thinking %v; want %q/%q/%v", i, m.Kind, m.Role, m.Thinking, w.kind, w.role, w.thinking)
		}
		if w.text != "" && m.Text != w.text {
			t.Errorf("msg[%d].Text = %q; want %q", i, m.Text, w.text)
		}
		if m.Seq != int64(i+1) {
			t.Errorf("msg[%d].Seq = %d; want %d (emission order)", i, m.Seq, i+1)
		}
	}

	tool := chat.Messages[2].Tool
	if tool == nil {
		t.Fatal("msg[2] carries no ToolInfo")
	}
	if tool.Name != "exec_command" || tool.Title != "Ran pwd" {
		t.Errorf("tool = %q/%q; want exec_command / Ran pwd", tool.Name, tool.Title)
	}
	if tool.Status != "ok" {
		t.Errorf("tool.Status = %q; want ok (output resolved by call_id)", tool.Status)
	}
	if !strings.Contains(tool.Output, "/var/lib/lab/tmp/spike87/attr") {
		t.Errorf("tool.Output = %q; want the captured pwd output", tool.Output)
	}
	if !strings.Contains(tool.Input, `"cmd":"pwd"`) {
		t.Errorf("tool.Input = %q; want the raw arguments JSON", tool.Input)
	}

	// Time is the record's top-level timestamp verbatim.
	if chat.Messages[0].Time != "2026-07-10T01:18:28.178Z" {
		t.Errorf("msg[0].Time = %q; want the raw record timestamp", chat.Messages[0].Time)
	}
	if chat.State != provider.StateNeedsInput {
		t.Errorf("state = %q; want needs_input after task_complete", chat.State)
	}
	if chat.Cursor != int64(len(want)) {
		t.Errorf("cursor = %d; want the highest seq %d", chat.Cursor, len(want))
	}
}

// The patch fixture: custom_tool_call/custom_tool_call_output resolve like
// function calls (patch_apply_end is skipped), an unresolved call stays
// running, and a lone task_started derives working.
func TestParseTranscript_patchFixture(t *testing.T) {
	chat := parseFixture(t, "rollout-patch.jsonl")

	if len(chat.Messages) != 3 {
		t.Fatalf("messages = %+v; want user text + 2 tool chips", chat.Messages)
	}
	patch := chat.Messages[1].Tool
	if patch == nil || patch.Name != "apply_patch" {
		t.Fatalf("msg[1] = %+v; want the apply_patch chip", chat.Messages[1])
	}
	if patch.Title != "Applied patch hello.txt" {
		t.Errorf("patch title = %q; want Applied patch hello.txt", patch.Title)
	}
	if patch.Status != "ok" || !strings.Contains(patch.Output, "Success. Updated the following files") {
		t.Errorf("patch chip = status %q output %q; want ok + the tool output", patch.Status, patch.Output)
	}
	if !strings.Contains(patch.Input, "*** Begin Patch") {
		t.Errorf("patch.Input = %q; want the raw patch body", patch.Input)
	}

	exec := chat.Messages[2].Tool
	if exec == nil || exec.Title != "Ran git status --porcelain" {
		t.Fatalf("msg[2] = %+v; want the running exec chip", chat.Messages[2])
	}
	if exec.Status != "running" || exec.Output != "" {
		t.Errorf("unresolved exec chip = status %q output %q; want running/empty", exec.Status, exec.Output)
	}

	if chat.State != provider.StateWorking {
		t.Errorf("state = %q; want working (task_started with no terminal event)", chat.State)
	}
}

// The aborted fixture (real TUI Esc interrupt): the <turn_aborted> synthetic
// user response_item is skipped, the turn_aborted event renders a lifecycle
// message with the reason and derives needs_input.
func TestParseTranscript_abortedFixture(t *testing.T) {
	chat := parseFixture(t, "rollout-aborted.jsonl")

	if len(chat.Messages) != 2 {
		t.Fatalf("messages = %+v; want user text + lifecycle", chat.Messages)
	}
	if m := chat.Messages[0]; m.Kind != provider.MessageText || m.Role != "user" ||
		!strings.HasPrefix(m.Text, "Run the shell command 'sleep 60'") {
		t.Errorf("msg[0] = %+v; want the operator prompt", m)
	}
	if m := chat.Messages[1]; m.Kind != provider.MessageLifecycle || m.Text != "interrupted" {
		t.Errorf("msg[1] = %+v; want lifecycle %q", m, "interrupted")
	}
	if chat.State != provider.StateNeedsInput {
		t.Errorf("state = %q; want needs_input after turn_aborted", chat.State)
	}
}

// State derivation edges over inline lines: last turn event wins; a
// meta-only rollout is idle; content with no turn events awaits the
// operator; malformed lines and unknown types never break the fold.
func TestParseTranscript_stateAndTolerance(t *testing.T) {
	const meta = `{"timestamp":"2026-07-10T00:00:00.000Z","type":"session_meta","payload":{"id":"x","cwd":"/w","cli_version":"0.133.0"}}`
	for _, tc := range []struct {
		name  string
		lines []string
		state string
		msgs  int
	}{
		{"meta only is idle", []string{meta}, provider.StateIdle, 0},
		{"empty stream is idle", nil, provider.StateIdle, 0},
		{
			"complete then started means working",
			[]string{
				meta,
				`{"timestamp":"t1","type":"event_msg","payload":{"type":"task_started","turn_id":"a"}}`,
				`{"timestamp":"t2","type":"event_msg","payload":{"type":"task_complete","turn_id":"a"}}`,
				`{"timestamp":"t3","type":"event_msg","payload":{"type":"task_started","turn_id":"b"}}`,
			},
			provider.StateWorking, 0,
		},
		{
			"messages with no turn events await the operator",
			[]string{
				meta,
				`{"timestamp":"t1","type":"event_msg","payload":{"type":"agent_message","message":"hi","phase":"final_answer"}}`,
			},
			provider.StateNeedsInput, 1,
		},
		{
			"turn_aborted with an unknown reason renders the reason text",
			[]string{
				meta,
				`{"timestamp":"t1","type":"event_msg","payload":{"type":"task_started","turn_id":"a"}}`,
				`{"timestamp":"t2","type":"event_msg","payload":{"type":"turn_aborted","turn_id":"a","reason":"replaced"}}`,
			},
			provider.StateNeedsInput, 1,
		},
		{
			"malformed and unknown lines are skipped",
			[]string{
				"{half a line",
				`{"timestamp":"t","type":"mystery_record","payload":{"type":"??"}}`,
				`{"timestamp":"t","type":"event_msg","payload":{"type":"token_count","info":null}}`,
				`{"timestamp":"t","type":"response_item","payload":{"type":"future_shape"}}`,
			},
			provider.StateIdle, 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat, err := ParseTranscript(strings.NewReader(strings.Join(tc.lines, "\n")))
			if err != nil {
				t.Fatalf("ParseTranscript: %v", err)
			}
			if chat.State != tc.state {
				t.Errorf("state = %q; want %q", chat.State, tc.state)
			}
			if len(chat.Messages) != tc.msgs {
				t.Errorf("messages = %+v; want %d", chat.Messages, tc.msgs)
			}
			// No dialogs exist in v1 — the parser must never emit one.
			for i, m := range chat.Messages {
				if m.Kind == provider.MessageDialog {
					t.Errorf("msg[%d] is a dialog; codex v1 never emits MessageDialog", i)
				}
			}
		})
	}
}

// Reasoning content is encrypted: only a non-empty summary renders (as
// assistant thinking), and unexpected summary item shapes degrade to skip.
func TestParseTranscript_reasoningSummaries(t *testing.T) {
	lines := strings.Join([]string{
		`{"timestamp":"t1","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"gAAA"}}`,
		`{"timestamp":"t2","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}],"encrypted_content":"gAAA"}}`,
		`{"timestamp":"t3","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"future_kind","text":"never"}],"encrypted_content":"gAAA"}}`,
	}, "\n")
	chat, err := ParseTranscript(strings.NewReader(lines))
	if err != nil {
		t.Fatalf("ParseTranscript: %v", err)
	}
	if len(chat.Messages) != 1 {
		t.Fatalf("messages = %+v; want exactly the one non-empty summary", chat.Messages)
	}
	m := chat.Messages[0]
	if !m.Thinking || m.Role != "assistant" || m.Text != "first\nsecond" {
		t.Errorf("thinking msg = %+v; want concatenated summary_text items", m)
	}
}
