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
	"regexp"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/unidiff"
)

// lineNumPrefix matches a Read result's cat -n line-number prefix ("     1\t").
var lineNumPrefix = regexp.MustCompile(`^\s*\d+\t`)

type tItem struct {
	Type    string `json:"type"`    // user|assistant|system|attachment|queue-operation|…
	Subtype string `json:"subtype"` // system: bridge_status|turn_duration|…
	// Content is system bridge_status text AND a queue-operation's queued
	// payload: a task-notification enqueue/dequeue/remove carries the full
	// "<task-notification>…" text here (issue #159, compat §5 — mined live
	// from 2.1.198–2.1.206; a payload-less dequeue leaves it empty).
	Content           string       `json:"content"`
	Timestamp         string       `json:"timestamp"` // ISO-8601
	IsMeta            bool         `json:"isMeta"`
	IsApiErrorMessage bool         `json:"isApiErrorMessage"`
	Message           *tMessage    `json:"message"`
	Attachment        *tAttachment `json:"attachment"` // type:"attachment" events only
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

// tAttachment is the mid-turn injection envelope on type:"attachment" events.
// The only shape lab reads is the delivered task notification —
// type:"queued_command" with commandMode:"task-notification", payload in
// prompt (issue #159, compat §5; mined live from 2.1.198–2.1.206). Other
// attachment types ("task_reminder", "edited_text_file", …) and other
// commandModes (operator-queued messages) are ignored and state-neutral by
// construction: foldTranscript matches on CommandMode alone.
type tAttachment struct {
	Type        string `json:"type"`        // queued_command|task_reminder|edited_text_file|…
	CommandMode string `json:"commandMode"` // "task-notification" for delivered task payloads
	Prompt      string `json:"prompt"`      // the "<task-notification>…" payload text
}

// pendingWorkResult is the slice of a top-level toolUseResult that marks
// background work (issue #159, compat §5; shapes mined live from
// 2.1.198–2.1.206). The add rule is STRUCTURAL, never prose:
// status=="async_launched" is stamped only on async Agent launches (agentId)
// and Workflow launches (taskId — the id notifications reference, never the
// wf_… runId). That gate excludes background Bash by construction — its
// result carries backgroundTaskId and no status, and a dev server never
// exits, so admitting it would pin `working` forever and suppress every
// push — as well as Monitor ({taskId,timeoutMs} — no status) and SYNCHRONOUS
// Agent calls (status "completed" + totalDurationMs). ResumedAgentID is
// SendMessage's re-activation stamp: messaging a stopped agent resumes it in
// the background, so the id re-enters the pending set.
type pendingWorkResult struct {
	Status         string `json:"status"`
	AgentID        string `json:"agentId"`
	TaskID         string `json:"taskId"`
	ResumedAgentID string `json:"resumedAgentId"`
}

// decodePendingWork tolerantly decodes an event's top-level toolUseResult
// into the pending-work slice. toolUseResult is an OBJECT on tool results but
// a plain STRING on denials ("User rejected tool use" — see the
// tItem.ToolUseResult contract), so a non-object yields the zero value — no
// marker, never a panic.
func decodePendingWork(raw json.RawMessage) pendingWorkResult {
	var r pendingWorkResult
	if len(raw) == 0 || raw[0] != '{' {
		return r
	}
	_ = json.Unmarshal(raw, &r) // best-effort: unknown shapes carry no marker
	return r
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
// truncated raw input, status "running" until the result back-patches it. The
// rich View (issue #146) is attached from the same decoded input, so a tool
// this mapper does not recognize simply carries no View and the client falls
// back to the chip's raw Input/Output.
func toolInfo(b tBlock) *provider.ToolInfo {
	in := decodeToolInput(b.Input)
	return &provider.ToolInfo{
		Name:   b.Name,
		Title:  toolTitle(b.Name, in),
		Input:  truncate(toolInputText(b.Name, in, b.Input), truncateLimit),
		Status: "running",
		View:   toolView(b.Name, in),
	}
}

// toolView is the ONLY place Claude Code's tool-input shapes are known — the
// mapping from a tool_use's name + decoded input to the provider-neutral
// ToolView union (issue #146). Every other package (the httpapi layer, the
// SPA) renders by ToolView.Kind alone, never by tool name, so a new tool this
// function does not recognize degrades cleanly to no view rather than a
// half-guessed one.
//
// in is nil for a partial mid-stream input (the tool_use line arrived before
// its JSON input finished streaming) — every branch below is a plain map
// lookup that reads "" from a nil map, so a partial input yields no view
// exactly like an unmapped tool, never a panic.
func toolView(name string, in map[string]any) *provider.ToolView {
	switch name {
	case "Edit":
		// A find/replace edit: old_string/new_string is the whole edit, so a
		// line-level diff against them is exact — no need to read the file.
		path, old, newS := str(in["file_path"]), str(in["old_string"]), str(in["new_string"])
		if path == "" || (old == "" && newS == "") {
			return nil
		}
		return &provider.ToolView{Kind: provider.ToolViewDiff, Path: path, Text: provider.TruncateDetail(unidiff.Unified(old, newS))}
	case "NotebookEdit":
		// The input carries only the new cell source, never the old one, so
		// this renders as an all-added diff (oldText ""). edit_mode=delete has
		// no new_source and correctly gets no view — there is nothing to show.
		path, src := str(in["notebook_path"]), str(in["new_source"])
		if path == "" || src == "" {
			return nil
		}
		return &provider.ToolView{Kind: provider.ToolViewDiff, Path: path, Text: provider.TruncateDetail(unidiff.Unified("", src))}
	case "Write":
		path, content := str(in["file_path"]), str(in["content"])
		if path == "" {
			return nil
		}
		return &provider.ToolView{Kind: provider.ToolViewWrite, Path: path, Text: provider.TruncateDetail(content)}
	case "Bash":
		cmd := str(in["command"])
		if cmd == "" {
			return nil
		}
		return &provider.ToolView{Kind: provider.ToolViewCommand, Command: provider.TruncateDetail(cmd)}
	case "Read":
		// Text stays empty here — the excerpt only exists once the tool_result
		// lands (patchToolResult fills it in).
		path := str(in["file_path"])
		if path == "" {
			return nil
		}
		return &provider.ToolView{Kind: provider.ToolViewRead, Path: path}
	default:
		return nil
	}
}

// patchToolResult back-patches a tool chip when its tool_result lands: the
// detail-sized output text (issue #146 — the panel body, capped at
// DetailTruncateLimit with an explicit in-text marker, NOT the 2000-byte
// chip cap the Title/Input fields keep) and a terminal status. Claude marks
// errors via the is_error flag on the tool_result block itself (content is
// most often a plain string); absent that, a result is "ok".
func patchToolResult(t *provider.ToolInfo, b tBlock) {
	if t == nil {
		return
	}
	out, isErr := decodeToolResult(b.Result)
	isErr = isErr || b.IsError
	t.Output = provider.TruncateDetail(out)
	if isErr {
		t.Status = "error"
	} else {
		t.Status = "ok"
	}
	if t.View != nil && t.View.Kind == provider.ToolViewRead {
		if isErr {
			// An error message is not file content — fall back to the raw
			// Output chip rather than render it as an excerpt.
			t.View = nil
		} else {
			t.View.Text = provider.TruncateDetail(readExcerpt(out))
		}
	}
}

// readExcerpt cleans a Read tool_result into a plain file excerpt. Claude
// Code renders Read results cat -n style (right-aligned line number + tab +
// content) and sometimes appends a trailing <system-reminder>…</system-reminder>
// block that is not part of the file.
func readExcerpt(s string) string {
	if i := strings.Index(s, "\n<system-reminder>"); i >= 0 {
		s = strings.TrimRight(s[:i], " \t\n")
	} else if strings.HasPrefix(s, "<system-reminder>") {
		s = ""
	}
	lines := strings.Split(s, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		if !lineNumPrefix.MatchString(line) {
			return s // not every line matches — conservative, return as-is
		}
	}
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = lineNumPrefix.ReplaceAllString(line, "")
	}
	return strings.Join(lines, "\n")
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
