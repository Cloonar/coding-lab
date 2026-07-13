package claudecode

// toolView's per-tool mapping (issue #146) from a decoded tool_use input to
// the provider-neutral ToolView union, plus patchToolResult's detail-sized
// (20KB) Output — distinct from the 2000-byte chip-facing Title/Input cap the
// rest of this package's tests exercise via truncateLimit.

import (
	"encoding/json"
	"fmt"
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

// TestToolView_read pins Path from file_path with Text left empty — the
// excerpt only exists once the tool_result lands (patchToolResult fills it
// in), not at tool_use time.
func TestToolView_read(t *testing.T) {
	v := toolView("Read", map[string]any{"file_path": "a.txt"})
	if v == nil {
		t.Fatal("View = nil; want a read view")
	}
	if v.Kind != provider.ToolViewRead {
		t.Errorf("Kind = %q; want %q", v.Kind, provider.ToolViewRead)
	}
	if v.Path != "a.txt" {
		t.Errorf("Path = %q; want %q", v.Path, "a.txt")
	}
	if v.Text != "" {
		t.Errorf("Text = %q; want empty at tool_use time", v.Text)
	}
}

// TestToolView_readNoPathNoView: a missing/empty file_path (a partial or
// degenerate input) yields no view.
func TestToolView_readNoPathNoView(t *testing.T) {
	if v := toolView("Read", map[string]any{}); v != nil {
		t.Errorf("View = %+v; want nil", v)
	}
	if v := toolView("Read", map[string]any{"file_path": ""}); v != nil {
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

// TestToolView_unmappedTool: a tool this mapper does not know about — Glob
// included, since it is the paradigm case the client's raw Input/Output
// fallback exists for — gets no view.
func TestToolView_unmappedTool(t *testing.T) {
	for _, name := range []string{"Grep", "Glob", "Task", "Skill", "AskUserQuestion", "SomeFutureTool"} {
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

// TestPatchToolResult_readView pins the read view's Text contract: a cat -n
// formatted result has its line-number prefixes stripped and a trailing
// <system-reminder> block dropped.
func TestPatchToolResult_readView(t *testing.T) {
	raw, _ := json.Marshal("     1\tpackage main\n     2\t\n     3\tfunc main() {}\n\n<system-reminder>\nsome note\n</system-reminder>")
	tool := &provider.ToolInfo{View: &provider.ToolView{Kind: provider.ToolViewRead, Path: "a.go"}}
	patchToolResult(tool, tBlock{Result: json.RawMessage(raw)})
	want := "package main\n\nfunc main() {}"
	if tool.View == nil {
		t.Fatal("View = nil; want the read view to survive an ok result")
	}
	if tool.View.Text != want {
		t.Errorf("View.Text = %q; want %q", tool.View.Text, want)
	}
	if tool.View.Kind != provider.ToolViewRead || tool.View.Path != "a.go" {
		t.Errorf("View = %+v; want Kind/Path untouched", tool.View)
	}
}

// TestPatchToolResult_readViewUnrecognizedShape: when at least one non-empty
// line does not match the cat -n prefix, readExcerpt is conservative and
// passes the text through unmodified rather than mangle an unexpected shape.
func TestPatchToolResult_readViewUnrecognizedShape(t *testing.T) {
	out := "     1\tpackage main\nnot a numbered line"
	raw, _ := json.Marshal(out)
	tool := &provider.ToolInfo{View: &provider.ToolView{Kind: provider.ToolViewRead, Path: "a.go"}}
	patchToolResult(tool, tBlock{Result: json.RawMessage(raw)})
	if tool.View == nil || tool.View.Text != out {
		t.Errorf("View = %+v; want Text unchanged from %q", tool.View, out)
	}
}

// TestPatchToolResult_readViewError: an error result is not file content, so
// the read view is cleared entirely — the client falls back to the raw
// Output chip.
func TestPatchToolResult_readViewError(t *testing.T) {
	raw, _ := json.Marshal("ENOENT: no such file")
	tool := &provider.ToolInfo{View: &provider.ToolView{Kind: provider.ToolViewRead, Path: "missing.go"}}
	patchToolResult(tool, tBlock{Result: json.RawMessage(raw), IsError: true})
	if tool.View != nil {
		t.Errorf("View = %+v; want nil on an error result", tool.View)
	}
	if tool.Status != "error" {
		t.Errorf("Status = %q; want error", tool.Status)
	}
}

// TestPatchToolResult_readViewDetailSizing confirms the read view's Text
// goes through the same 20000-byte TruncateDetail cap as the write/diff
// views (TestPatchToolResult_detailSizing's sizing approach), keyed off the
// cat -n line count so the excerpt still exceeds the limit after stripping.
func TestPatchToolResult_readViewDetailSizing(t *testing.T) {
	line := strings.Repeat("x", 100)
	var b strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i, line)
	}
	out := b.String()
	raw, _ := json.Marshal(out)
	tool := &provider.ToolInfo{View: &provider.ToolView{Kind: provider.ToolViewRead, Path: "big.go"}}
	patchToolResult(tool, tBlock{Result: json.RawMessage(raw)})
	want := provider.TruncateDetail(readExcerpt(out))
	if tool.View.Text != want {
		t.Errorf("View.Text = %q; want provider.TruncateDetail(readExcerpt(out))", tool.View.Text)
	}
	if !strings.Contains(tool.View.Text, "truncated (") {
		t.Errorf("View.Text does not carry the truncation marker: %q", tool.View.Text[len(tool.View.Text)-60:])
	}
}
