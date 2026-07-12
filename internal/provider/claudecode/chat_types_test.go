package claudecode

// toolView's per-tool mapping (issue #146) from a decoded tool_use input to
// the provider-neutral ToolView union, plus patchToolResult's detail-sized
// (20KB) Output — distinct from the 2000-byte chip-facing Title/Input cap the
// rest of this package's tests exercise via truncateLimit.

import (
	"encoding/json"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// TestToolView_edit pins the exact unified-diff body an Edit's old_string/
// new_string pair produces — computed by hand against internal/unidiff's
// documented @@ header shape, not just asserted non-empty.
func TestToolView_edit(t *testing.T) {
	v := toolView("Edit", map[string]any{
		"file_path": "a.txt", "old_string": "foo\n", "new_string": "bar\n",
	})
	if v == nil {
		t.Fatal("View = nil; want a diff view")
	}
	if v.Kind != provider.ToolViewDiff {
		t.Errorf("Kind = %q; want %q", v.Kind, provider.ToolViewDiff)
	}
	if v.Path != "a.txt" {
		t.Errorf("Path = %q; want %q", v.Path, "a.txt")
	}
	want := "@@ -1,1 +1,1 @@\n-foo\n+bar\n"
	if v.Text != want {
		t.Errorf("Text = %q; want %q", v.Text, want)
	}
}

// TestToolView_editNoView covers the guard clause: no path, or an old/new
// pair that is entirely empty (nothing to diff — e.g. a partial/degenerate
// input), yields no view.
func TestToolView_editNoView(t *testing.T) {
	cases := map[string]map[string]any{
		"no path":         {"old_string": "a", "new_string": "b"},
		"empty path":      {"file_path": "", "old_string": "a", "new_string": "b"},
		"both empty":      {"file_path": "a.txt", "old_string": "", "new_string": ""},
		"missing old/new": {"file_path": "a.txt"},
	}
	for name, in := range cases {
		if v := toolView("Edit", in); v != nil {
			t.Errorf("%s: View = %+v; want nil", name, v)
		}
	}
}

// TestToolView_editOneSided confirms a pure deletion (new_string empty) and a
// pure insertion (old_string empty) each still qualify for a view — only a
// TOTALLY empty pair is excluded (TestToolView_editNoView).
func TestToolView_editOneSided(t *testing.T) {
	del := toolView("Edit", map[string]any{"file_path": "a.txt", "old_string": "gone\n", "new_string": ""})
	if del == nil || del.Kind != provider.ToolViewDiff {
		t.Fatalf("deletion: View = %+v; want a diff view", del)
	}
	ins := toolView("Edit", map[string]any{"file_path": "a.txt", "old_string": "", "new_string": "new\n"})
	if ins == nil || ins.Kind != provider.ToolViewDiff {
		t.Fatalf("insertion: View = %+v; want a diff view", ins)
	}
}

// TestToolView_write pins Path/Text = file_path/content verbatim (no diffing
// — a Write replaces the whole file).
func TestToolView_write(t *testing.T) {
	v := toolView("Write", map[string]any{"file_path": "new.go", "content": "package main\n"})
	if v == nil {
		t.Fatal("View = nil; want a write view")
	}
	if v.Kind != provider.ToolViewWrite {
		t.Errorf("Kind = %q; want %q", v.Kind, provider.ToolViewWrite)
	}
	if v.Path != "new.go" {
		t.Errorf("Path = %q; want %q", v.Path, "new.go")
	}
	if v.Text != "package main\n" {
		t.Errorf("Text = %q; want %q", v.Text, "package main\n")
	}
}

// TestToolView_writeNoPathNoView: an empty file_path (a partial/degenerate
// input) yields no view; an empty content with a real path still does (an
// empty file is a legitimate Write).
func TestToolView_writeNoPathNoView(t *testing.T) {
	if v := toolView("Write", map[string]any{"content": "x"}); v != nil {
		t.Errorf("View = %+v; want nil", v)
	}
	v := toolView("Write", map[string]any{"file_path": "empty.txt", "content": ""})
	if v == nil || v.Text != "" {
		t.Errorf("View = %+v; want an empty-content write view", v)
	}
}

// TestToolView_bash pins Command to the FULL multi-line command — unlike the
// chip Title's firstLine() summary, the detail view must show every line.
func TestToolView_bash(t *testing.T) {
	cmd := "echo one\necho two"
	v := toolView("Bash", map[string]any{"command": cmd})
	if v == nil {
		t.Fatal("View = nil; want a command view")
	}
	if v.Kind != provider.ToolViewCommand {
		t.Errorf("Kind = %q; want %q", v.Kind, provider.ToolViewCommand)
	}
	if v.Command != cmd {
		t.Errorf("Command = %q; want %q", v.Command, cmd)
	}
	if v.Path != "" || v.Text != "" {
		t.Errorf("Path/Text should stay empty on a command view: %+v", v)
	}
}

func TestToolView_bashNoCommandNoView(t *testing.T) {
	if v := toolView("Bash", map[string]any{"command": ""}); v != nil {
		t.Errorf("View = %+v; want nil", v)
	}
}

// TestToolView_notebookEditAllAdded: NotebookEdit's input carries only the
// new cell source, never the old one, so the view is an all-added diff
// (oldText "") — pinned against unidiff's documented pure-insertion header
// shape ("@@ -0,0 +N,N @@").
func TestToolView_notebookEditAllAdded(t *testing.T) {
	v := toolView("NotebookEdit", map[string]any{"notebook_path": "nb.ipynb", "new_source": "print(1)\n"})
	if v == nil {
		t.Fatal("View = nil; want a diff view")
	}
	if v.Kind != provider.ToolViewDiff {
		t.Errorf("Kind = %q; want %q", v.Kind, provider.ToolViewDiff)
	}
	if v.Path != "nb.ipynb" {
		t.Errorf("Path = %q; want %q", v.Path, "nb.ipynb")
	}
	want := "@@ -0,0 +1,1 @@\n+print(1)\n"
	if v.Text != want {
		t.Errorf("Text = %q; want %q", v.Text, want)
	}
}

// TestToolView_notebookEditDeleteNoView: edit_mode=delete carries no
// new_source — nothing to show, so no view (rather than a misleading empty
// diff).
func TestToolView_notebookEditDeleteNoView(t *testing.T) {
	v := toolView("NotebookEdit", map[string]any{"notebook_path": "nb.ipynb", "edit_mode": "delete"})
	if v != nil {
		t.Errorf("View = %+v; want nil", v)
	}
}

// TestToolView_unmappedTool: a tool this mapper does not know about — Read
// included, since it is the paradigm case the client's raw Input/Output
// fallback exists for — gets no view.
func TestToolView_unmappedTool(t *testing.T) {
	for _, name := range []string{"Read", "Grep", "Glob", "Task", "Skill", "AskUserQuestion", "SomeFutureTool"} {
		if v := toolView(name, map[string]any{"file_path": "x", "command": "y"}); v != nil {
			t.Errorf("%s: View = %+v; want nil", name, v)
		}
	}
}

// TestToolView_nilInput: a partial mid-stream tool_use whose input JSON
// hasn't finished streaming decodes to a nil map (decodeToolInput). Every
// mapped tool must degrade to no view rather than panic on the nil lookup.
func TestToolView_nilInput(t *testing.T) {
	for _, name := range []string{"Edit", "NotebookEdit", "Write", "Bash", "Read"} {
		if v := toolView(name, nil); v != nil {
			t.Errorf("%s: View = %+v; want nil on nil input", name, v)
		}
	}
}

// TestToolInfo_attachesView is the end-to-end wiring check: toolInfo decodes
// the tool_use block's raw JSON input and attaches the View toolView derives
// from it — confirming the two are actually connected, not just individually
// correct.
func TestToolInfo_attachesView(t *testing.T) {
	b := tBlock{Name: "Edit", Input: json.RawMessage(`{"file_path":"a.txt","old_string":"foo\n","new_string":"bar\n"}`)}
	info := toolInfo(b)
	if info.View == nil {
		t.Fatal("View = nil; want a diff view")
	}
	if info.View.Kind != provider.ToolViewDiff || info.View.Path != "a.txt" {
		t.Errorf("View = %+v; want the Edit diff view", info.View)
	}
}

// TestToolInfo_partialInputNoView: a tool_use line whose input JSON is still
// mid-stream (a bare partial string, not yet a closed object) must not panic
// and must carry no View — decodeToolInput's documented nil-on-partial
// contract, exercised through the public toolInfo entry point.
func TestToolInfo_partialInputNoView(t *testing.T) {
	b := tBlock{Name: "Edit", Input: json.RawMessage(`"file_pa`)}
	info := toolInfo(b)
	if info.View != nil {
		t.Errorf("View = %+v; want nil on partial input", info.View)
	}
}

// TestPatchToolResult_detailSizing pins issue #146's Output contract: the
// panel-sized DetailTruncateLimit (20000 bytes, TruncateDetail's explicit
// marker), NOT the 2000-byte chip cap truncateLimit still applies to
// Title/Input. An output comfortably above the OLD 2000-byte limit but below
// the new 20000-byte one must now come through untouched.
func TestPatchToolResult_detailSizing(t *testing.T) {
	mkResult := func(out string) tBlock {
		raw, err := json.Marshal(out)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		return tBlock{Result: json.RawMessage(raw)}
	}

	t.Run("short output unchanged", func(t *testing.T) {
		out := "all good"
		tool := &provider.ToolInfo{}
		patchToolResult(tool, mkResult(out))
		if tool.Output != out {
			t.Errorf("Output = %q; want %q", tool.Output, out)
		}
		if tool.Status != "ok" {
			t.Errorf("Status = %q; want ok", tool.Status)
		}
	})

	t.Run("above old 2000-byte chip cap stays whole", func(t *testing.T) {
		out := strings.Repeat("a", 5000)
		tool := &provider.ToolInfo{}
		patchToolResult(tool, mkResult(out))
		if tool.Output != out {
			t.Errorf("Output len = %d; want the full %d bytes untouched (old 2000-byte chip cap must not apply here)", len(tool.Output), len(out))
		}
	})

	t.Run("exactly at the 20000-byte detail limit is untouched", func(t *testing.T) {
		out := strings.Repeat("b", provider.DetailTruncateLimit)
		tool := &provider.ToolInfo{}
		patchToolResult(tool, mkResult(out))
		if tool.Output != out {
			t.Errorf("Output was mutated at exactly the limit; want it unchanged")
		}
	})

	t.Run("above the 20000-byte detail limit is cut with the explicit marker", func(t *testing.T) {
		out := strings.Repeat("c", provider.DetailTruncateLimit+1)
		tool := &provider.ToolInfo{}
		patchToolResult(tool, mkResult(out))
		want := provider.TruncateDetail(out)
		if tool.Output != want {
			t.Errorf("Output = %q; want provider.TruncateDetail's output", tool.Output)
		}
		if !strings.Contains(tool.Output, "truncated (") || !strings.HasSuffix(tool.Output, "KB total)") {
			t.Errorf("Output does not end with the truncation marker: %q", tool.Output[len(tool.Output)-60:])
		}
	})
}

// TestPatchToolResult_errorStatus confirms the is_error flag still drives
// Status independent of the Output sizing change.
func TestPatchToolResult_errorStatus(t *testing.T) {
	raw, _ := json.Marshal("boom")
	tool := &provider.ToolInfo{}
	patchToolResult(tool, tBlock{Result: json.RawMessage(raw), IsError: true})
	if tool.Status != "error" {
		t.Errorf("Status = %q; want error", tool.Status)
	}
	if tool.Output != "boom" {
		t.Errorf("Output = %q; want %q", tool.Output, "boom")
	}
}
