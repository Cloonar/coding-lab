package claudecode

// The Claude Code transcript JSONL grammar lab reads (compat.md §5). Only the
// fields lab maps are declared; every other key claude writes is ignored by
// construction. One line is one event; an assistant/user event's message.content
// is either a plain string (a whole user turn) or an array of typed blocks
// (text | thinking | tool_use | tool_result), each block its own line in
// practice but modelled as a slice for tolerance.

import (
	"encoding/json"
	"path"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

type tItem struct {
	Type              string    `json:"type"`      // user|assistant|system|attachment|…
	Subtype           string    `json:"subtype"`   // system: bridge_status|turn_duration|…
	Content           string    `json:"content"`   // system bridge_status text
	Timestamp         string    `json:"timestamp"` // ISO-8601
	IsMeta            bool      `json:"isMeta"`
	IsApiErrorMessage bool      `json:"isApiErrorMessage"`
	Message           *tMessage `json:"message"`
	// ToolUseResult / ToolDenialKind are the top-level resolution ground truth
	// claude stamps onto the user event that carries a tool_result block —
	// the verification-backstop source (issue #51 decision 3, compat §5, live
	// 2026-07-08 2.1.198). ToolUseResult is an OBJECT for a recorded answer
	// ({questions,answers,annotations[,afkTimeoutMs]} for AskUserQuestion;
	// {plan,filePath,…} for an approved ExitPlanMode) and a plain STRING for a
	// denial ("User rejected tool use" / "Error: The user doesn't want to
	// proceed…"); ToolDenialKind is "user-rejected" on a decline.
	ToolUseResult  json.RawMessage `json:"toolUseResult"`
	ToolDenialKind string          `json:"toolDenialKind"`
}

type tMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string | []tBlock
}

// blocks normalizes message.content to a block slice: a bare string becomes a
// single text block, an array is decoded as blocks, anything else is empty.
func (m *tMessage) blocks() []tBlock {
	if m == nil || len(m.Content) == 0 {
		return nil
	}
	if m.Content[0] == '"' {
		var s string
		if json.Unmarshal(m.Content, &s) != nil {
			return nil
		}
		return []tBlock{{Type: "text", Text: s}}
	}
	var blks []tBlock
	if json.Unmarshal(m.Content, &blks) != nil {
		return nil
	}
	return blks
}

// text is the concatenated visible text of a message (used for API-error
// lines, whose content is a single text block).
func (m *tMessage) text() string {
	var b strings.Builder
	for _, blk := range m.blocks() {
		if blk.Type == "text" {
			b.WriteString(blk.textField())
		}
	}
	return strings.TrimSpace(b.String())
}

type tBlock struct {
	Type      string          `json:"type"` // text|thinking|tool_use|tool_result
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`  // tool_use tool name
	ID        string          `json:"id"`    // tool_use id
	Input     json.RawMessage `json:"input"` // tool_use input (object, or a partial string mid-stream)
	ToolUseID string          `json:"tool_use_id"`
	Result    json.RawMessage `json:"content"`  // tool_result content: string | []block
	IsError   bool            `json:"is_error"` // tool_result: block-level failure flag
}

func (b tBlock) textField() string { return b.Text }

// toolInfo maps a tool_use block to a chip: a friendly one-line Title plus the
// truncated raw input, status "running" until the result back-patches it.
func toolInfo(b tBlock) *provider.ToolInfo {
	in := decodeToolInput(b.Input)
	return &provider.ToolInfo{
		Name:   b.Name,
		Title:  toolTitle(b.Name, in),
		Input:  truncate(toolInputText(b.Name, in, b.Input), truncateLimit),
		Status: "running",
	}
}

// patchToolResult back-patches a tool chip when its tool_result lands: the
// truncated output text and a terminal status. Claude marks errors via the
// is_error flag on the tool_result block itself (content is most often a plain
// string); absent that, a result is "ok".
func patchToolResult(t *provider.ToolInfo, b tBlock) {
	if t == nil {
		return
	}
	out, isErr := decodeToolResult(b.Result)
	isErr = isErr || b.IsError
	t.Output = truncate(out, truncateLimit)
	if isErr {
		t.Status = "error"
	} else {
		t.Status = "ok"
	}
}

// toolTitle renders the chat chip label from the tool name and decoded input:
// "Ran <first cmd line>", "Edit <file>", "Read <file>", "Skill <name>", else
// the bare tool name. Best-effort and forgiving — a miss just shows the name.
func toolTitle(name string, in map[string]any) string {
	switch name {
	case "Bash":
		if cmd := firstLine(str(in["command"])); cmd != "" {
			return "Ran " + cmd
		}
	case "Edit", "Write", "NotebookEdit":
		if f := str(in["file_path"]); f != "" {
			return name + " " + path.Base(f)
		}
	case "Read":
		if f := str(in["file_path"]); f != "" {
			return "Read " + path.Base(f)
		}
	case "Skill":
		if s := str(in["skill"]); s != "" {
			return "Skill " + s
		}
	case "Task":
		if d := str(in["description"]); d != "" {
			return "Task: " + d
		}
	}
	if name == "" {
		return "tool"
	}
	return name
}

// toolInputText is the expand-on-tap body: the shell command for Bash, else
// the raw JSON input. Keeps the chip terse while the detail stays inspectable.
func toolInputText(name string, in map[string]any, raw json.RawMessage) string {
	if name == "Bash" {
		if cmd := str(in["command"]); cmd != "" {
			return cmd
		}
	}
	return strings.TrimSpace(string(raw))
}

func decodeToolInput(raw json.RawMessage) map[string]any {
	if len(raw) == 0 || raw[0] != '{' {
		return nil // input can be a partial string on a mid-stream line
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// decodeToolResult flattens a tool_result content (string, or an array of
// {type:text,text} blocks) to plain text. The authoritative failure flag is
// block-level (tBlock.IsError, checked by the caller); an is_error on an inner
// content item is tolerated as a second signal.
func decodeToolResult(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s, false
	}
	var blks []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		IsError bool   `json:"is_error"`
	}
	if json.Unmarshal(raw, &blks) != nil {
		return strings.TrimSpace(string(raw)), false
	}
	var b strings.Builder
	isErr := false
	for _, blk := range blks {
		if blk.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(blk.Text)
		}
		isErr = isErr || blk.IsError
	}
	return b.String(), isErr
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Back up to a rune boundary so the cut never splits a UTF-8 sequence.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}
