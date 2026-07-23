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
	"reflect"
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
	// The rollout tree lives under the run's private HOME (issue #202):
	// <home>/.codex/sessions.
	home := t.TempDir()
	sessions := filepath.Join(instanceCodexHome(home), "sessions")
	const wtA, wtB = "/work/a", "/work/b"

	writeRollout(t, sessions, "2026", "07", "09", "rollout-2026-07-09T23-59-59-aaa.jsonl", wtA)
	wantB := writeRollout(t, sessions, "2026", "07", "10", "rollout-2026-07-10T01-00-00-bbb.jsonl", wtB)
	wantA := writeRollout(t, sessions, "2026", "07", "10", "rollout-2026-07-10T02-00-00-ccc.jsonl", wtA)

	if got, err := p.LocateTranscript(ctx, "sess", wtA, home); err != nil || got != wantA {
		t.Errorf("LocateTranscript(A) = %q, %v; want the newest A rollout %q", got, err, wantA)
	}
	if got, err := p.LocateTranscript(ctx, "sess", wtB, home); err != nil || got != wantB {
		t.Errorf("LocateTranscript(B) = %q, %v; want %q", got, err, wantB)
	}

	// A fresh rollout in the same day with a later local timestamp — the
	// /new rotation — immediately becomes A's identity.
	rotated := writeRollout(t, sessions, "2026", "07", "10", "rollout-2026-07-10T03-00-00-ddd.jsonl", wtA)
	if got, _ := p.LocateTranscript(ctx, "sess", wtA, home); got != rotated {
		t.Errorf("LocateTranscript(A) after rotation = %q; want %q", got, rotated)
	}
}

func TestLocateTranscript_misses(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	ctx := context.Background()
	home := t.TempDir()
	sessions := filepath.Join(instanceCodexHome(home), "sessions")

	// Missing sessions tree: "" with no error (nothing rolled out yet).
	if got, err := p.LocateTranscript(ctx, "sess", "/work/a", home); err != nil || got != "" {
		t.Errorf("LocateTranscript on missing tree = %q, %v; want \"\", nil", got, err)
	}

	// A malformed first line and a non-rollout file are skipped quietly.
	dir := filepath.Join(sessions, "2026", "07", "10")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-07-10T01-00-00-eee.jsonl"), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("irrelevant"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := p.LocateTranscript(ctx, "sess", "/work/a", home); err != nil || got != "" {
		t.Errorf("LocateTranscript over junk = %q, %v; want \"\", nil", got, err)
	}

	// An empty home is a miss too — no per-run home ⇒ no instance rollout tree,
	// never a fall back to the master store (issue #202).
	if got, err := p.LocateTranscript(ctx, "sess", "/work/a", ""); err != nil || got != "" {
		t.Errorf("homeless locate = %q, %v; want \"\", nil", got, err)
	}
}

// TestLocateTranscript_emptyHomeIgnoresMasterStoreMatch proves the no-fallback
// rule (issue #202) with teeth: a $CODEX_HOME master-store tree carrying a
// rollout that WOULD match the worktree exists, yet LocateTranscript with
// home="" still misses cleanly — it never consults $CODEX_HOME or any other
// master-store location, only the home argument threaded on the seam.
func TestLocateTranscript_emptyHomeIgnoresMasterStoreMatch(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	ctx := context.Background()
	const worktree = "/work/a"

	// A master store (CODEX_HOME, un-nested — codex's own convention) holding
	// a rollout whose session_meta.cwd matches worktree exactly.
	master := t.TempDir()
	t.Setenv("CODEX_HOME", master)
	writeRollout(t, filepath.Join(master, "sessions"), "2026", "07", "10", "rollout-2026-07-10T01-00-00-fff.jsonl", worktree)

	if got, err := p.LocateTranscript(ctx, "sess", worktree, ""); err != nil || got != "" {
		t.Errorf("LocateTranscript(home=\"\") = %q, %v; want \"\", nil even though the master store %q holds a matching rollout", got, err, master)
	}
}

// ReadChat's argument contract (issue #92): an empty transcriptPath is the
// pre-transcript read of an active run — an idle empty chat, never an error.
// runID/runtimeDir are ignored (codex has no live-signal channel, ADR-0037),
// so passing them populated must change nothing.
func TestReadChat_emptyPathIsIdle(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	chat, err := p.ReadChat("run-1", t.TempDir(), "")
	if err != nil {
		t.Fatalf("ReadChat(\"\") err = %v; want nil — an unlocated transcript is a fresh spawn, not a failure", err)
	}
	if chat.State != provider.StateIdle || len(chat.Messages) != 0 || chat.Cursor != 0 || chat.PendingDialog != nil {
		t.Errorf("ReadChat(\"\") = %+v; want the idle empty chat", chat)
	}
}

// A vanished non-empty path yields the ErrTranscriptGone sentinel — the
// caller renders "transcript no longer available" from it (issue #92).
func TestReadChat_gone(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	if _, err := p.ReadChat("run-1", "", filepath.Join(t.TempDir(), "vanished.jsonl")); !errors.Is(err, provider.ErrTranscriptGone) {
		t.Errorf("ReadChat(missing) err = %v; want ErrTranscriptGone", err)
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
	// The rich diff view (issue #146): the V4A envelope normalized to
	// unified-diff text, first file in Path.
	if patch.View == nil || patch.View.Kind != provider.ToolViewDiff ||
		patch.View.Path != "hello.txt" || patch.View.Text != "--- /dev/null\n+++ b/hello.txt\n+hi" {
		t.Errorf("patch.View = %+v; want a diff view of hello.txt", patch.View)
	}

	exec := chat.Messages[2].Tool
	if exec == nil || exec.Title != "Ran git status --porcelain" {
		t.Fatalf("msg[2] = %+v; want the running exec chip", chat.Messages[2])
	}
	if exec.Status != "running" || exec.Output != "" {
		t.Errorf("unresolved exec chip = status %q output %q; want running/empty", exec.Status, exec.Output)
	}
	// exec_command carries a command view of the parsed cmd (issue #146).
	if exec.View == nil || exec.View.Kind != provider.ToolViewCommand ||
		exec.View.Command != "git status --porcelain" {
		t.Errorf("exec.View = %+v; want a command view of the git status", exec.View)
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

// normalizePatch turns codex's V4A apply_patch envelope into unified-diff
// text (issue #146): envelope verbs become ---/+++ file headers, the body's
// prefix-shaped lines pass through verbatim, an unknown *** verb is dropped,
// and the output never ends on a blank line.
func TestNormalizePatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		patch string
		want  string
	}{
		{
			"single-file update: headers plus verbatim body",
			"*** Begin Patch\n*** Update File: main.go\n@@ func main()\n-\told := 1\n+\tnew := 2\n \tkeep := 3\n*** End Patch\n",
			"--- a/main.go\n+++ b/main.go\n@@ func main()\n-\told := 1\n+\tnew := 2\n \tkeep := 3",
		},
		{
			"add file: the real fixture body",
			"*** Begin Patch\n*** Add File: hello.txt\n+hi\n*** End Patch\n",
			"--- /dev/null\n+++ b/hello.txt\n+hi",
		},
		{
			"delete file",
			"*** Begin Patch\n*** Delete File: gone.txt\n-bye\n*** End Patch\n",
			"--- a/gone.txt\n+++ /dev/null\n-bye",
		},
		{
			"move to: a rename consumes the following line",
			"*** Begin Patch\n*** Update File: old.go\n*** Move to: new.go\n@@\n-a\n+b\n*** End Patch\n",
			"--- a/old.go\n+++ b/new.go\n@@\n-a\n+b",
		},
		{
			"multi-file: two update sections, both header pairs, bodies in order",
			"*** Begin Patch\n*** Update File: a.txt\n@@\n-old a\n+new a\n*** Update File: b.txt\n@@\n-old b\n+new b\n*** End Patch\n",
			"--- a/a.txt\n+++ b/a.txt\n@@\n-old a\n+new a\n--- a/b.txt\n+++ b/b.txt\n@@\n-old b\n+new b",
		},
		{
			"an unknown envelope verb is dropped, not leaked",
			"*** Begin Patch\n*** Frobnicate: whatever\n*** Update File: c.txt\n+x\n*** End Patch\n",
			"--- a/c.txt\n+++ b/c.txt\n+x",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizePatch(tc.patch); got != tc.want {
				t.Errorf("normalizePatch =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// functionCallInfo attaches a command ToolView for codex's two exec shapes
// (issue #146): exec_command's {"cmd":…} and shell's {"command":…} — string,
// the two interpreter wrappers, or a bare argv array. The shell shape is
// defensive (no live fixture), so it is pinned here rather than by a rollout.
func TestFunctionCallInfo_commandView(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload tPayload
		title   string
		view    *provider.ToolView
	}{
		{
			"exec_command parses to a command view",
			tPayload{Name: "exec_command", Arguments: `{"cmd":"git status --porcelain","workdir":"/w"}`},
			"Ran git status --porcelain",
			&provider.ToolView{Kind: provider.ToolViewCommand, Command: "git status --porcelain"},
		},
		{
			"exec_command with unparseable arguments has no view",
			tPayload{Name: "exec_command", Arguments: "not json"},
			"exec_command",
			nil,
		},
		{
			"shell string form",
			tPayload{Name: "shell", Arguments: `{"command":"echo hi"}`},
			"shell",
			&provider.ToolView{Kind: provider.ToolViewCommand, Command: "echo hi"},
		},
		{
			"shell bash -lc wrapper unwraps to the script",
			tPayload{Name: "shell", Arguments: `{"command":["bash","-lc","make test"]}`},
			"shell",
			&provider.ToolView{Kind: provider.ToolViewCommand, Command: "make test"},
		},
		{
			"shell sh -c wrapper unwraps to the script",
			tPayload{Name: "shell", Arguments: `{"command":["sh","-c","echo done"]}`},
			"shell",
			&provider.ToolView{Kind: provider.ToolViewCommand, Command: "echo done"},
		},
		{
			"shell non-wrapper array joins with single spaces",
			tPayload{Name: "shell", Arguments: `{"command":["ls","-la","/tmp"]}`},
			"shell",
			&provider.ToolView{Kind: provider.ToolViewCommand, Command: "ls -la /tmp"},
		},
		{
			"shell with unparseable arguments has no view",
			tPayload{Name: "shell", Arguments: "not json"},
			"shell",
			nil,
		},
		{
			"a non-exec function_call has no view",
			tPayload{Name: "update_plan", Arguments: `{"steps":[]}`},
			"update_plan",
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := functionCallInfo(tc.payload)
			if got.Title != tc.title {
				t.Errorf("title = %q; want %q", got.Title, tc.title)
			}
			if !reflect.DeepEqual(got.View, tc.view) {
				t.Errorf("view = %+v; want %+v", got.View, tc.view)
			}
		})
	}
}

// customToolCallInfo attaches a diff ToolView for apply_patch (Kind/Path/Text)
// and leaves every other custom tool viewless (issue #146).
func TestCustomToolCallInfo_views(t *testing.T) {
	patch := customToolCallInfo(tPayload{Name: "apply_patch", Input: "*** Begin Patch\n*** Add File: hi.txt\n+yo\n*** End Patch\n"})
	if patch.View == nil || patch.View.Kind != provider.ToolViewDiff ||
		patch.View.Path != "hi.txt" || patch.View.Text != "--- /dev/null\n+++ b/hi.txt\n+yo" {
		t.Errorf("apply_patch view = %+v; want a diff view of hi.txt", patch.View)
	}

	// A custom tool lab does not recognize gets no rich view.
	if other := customToolCallInfo(tPayload{Name: "mystery_tool", Input: "whatever"}); other.View != nil {
		t.Errorf("mystery_tool view = %+v; want nil", other.View)
	}
}

// patchToolOutput sizes tool output for the detail panel (issue #146): the
// 20KB cap with an explicit marker, an order of magnitude above the 2000-byte
// chip cap; short output rides through untouched.
func TestPatchToolOutput_detailSized(t *testing.T) {
	big := &provider.ToolInfo{Status: "running"}
	patchToolOutput(big, strings.Repeat("x", provider.DetailTruncateLimit+500))
	if big.Status != "ok" {
		t.Errorf("status = %q; want ok", big.Status)
	}
	if !strings.Contains(big.Output, "truncated (") {
		t.Errorf("output missing the truncation marker (len=%d)", len(big.Output))
	}
	if len(big.Output) <= truncateLimit {
		t.Errorf("output len = %d; want detail-sized (well above the %d chip cap)", len(big.Output), truncateLimit)
	}

	short := &provider.ToolInfo{Status: "running"}
	patchToolOutput(short, "Exit code: 0")
	if short.Output != "Exit code: 0" {
		t.Errorf("short output = %q; want it unchanged", short.Output)
	}
}
