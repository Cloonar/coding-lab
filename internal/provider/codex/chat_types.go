package codex

// The codex rollout JSONL grammar lab reads, pinned from REAL 0.133.0
// rollouts (issue #87, 2026-07-10). Only the fields lab maps are declared;
// every other key codex writes is ignored by construction. One line is one
// record: {"timestamp":"…","type":"…","payload":{…}} where type is
// session_meta | turn_context | response_item | event_msg (unknown types
// skipped). Payload shapes lab consumes:
//
//	response_item: {"type":"message","role":"developer|user|assistant",
//	                "content":[{"type":"input_text|output_text","text":…}]}
//	                                              — SKIPPED (see chat.go)
//	response_item: {"type":"reasoning","summary":[{"type":"summary_text",
//	                "text":…}],"encrypted_content":"gAAAA…"}
//	response_item: {"type":"function_call","name":"exec_command",
//	                "arguments":"{\"cmd\":…}","call_id":"call_…"}
//	response_item: {"type":"function_call_output","call_id":…,"output":…}
//	response_item: {"type":"custom_tool_call","status":"completed",
//	                "call_id":…,"name":"apply_patch","input":"*** Begin Patch…"}
//	response_item: {"type":"custom_tool_call_output","call_id":…,"output":…}
//	event_msg:     {"type":"user_message","message":…,"images":[],…}
//	event_msg:     {"type":"agent_message","message":…,"phase":"commentary|…"}
//	event_msg:     {"type":"task_started","turn_id":…} / {"type":"task_complete",…}
//	event_msg:     {"type":"turn_aborted","turn_id":…,"reason":"interrupted",…}
//	event_msg:     {"type":"token_count",…} / {"type":"patch_apply_end",…} — skipped

import (
	"encoding/json"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// tRecord is one rollout line's envelope.
type tRecord struct {
	Timestamp string          `json:"timestamp"` // ISO-8601 UTC, passed through verbatim
	Type      string          `json:"type"`      // session_meta|turn_context|response_item|event_msg|…
	Payload   json.RawMessage `json:"payload"`
}

// tPayload is the union of the response_item and event_msg payload fields
// lab maps — one decode serves both record types.
type tPayload struct {
	Type string `json:"type"`

	// reasoning
	Summary []tSummary `json:"summary"`

	// function_call / custom_tool_call (+ their outputs)
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // function_call: JSON-encoded string
	Input     string `json:"input"`     // custom_tool_call: raw tool input (apply_patch body)
	CallID    string `json:"call_id"`
	Output    string `json:"output"`

	// event_msg
	Message string `json:"message"` // user_message / agent_message text
	Reason  string `json:"reason"`  // turn_aborted: "interrupted" | …
}

// tSummary is one reasoning summary item; only the summary_text shape is
// consumed, defensively (the live 0.133.0 rollouts all carried empty
// summaries, so this shape is the API's documented one, not a live pin).
type tSummary struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// summaryText concatenates a reasoning payload's renderable summary items,
// "" when the summary is empty (the encrypted body is never rendered).
func (pl tPayload) summaryText() string {
	var b strings.Builder
	for _, s := range pl.Summary {
		text := strings.TrimSpace(s.Text)
		if text == "" || (s.Type != "" && s.Type != "summary_text") {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	return b.String()
}

// functionCallInfo maps a function_call payload to a chip: for exec_command
// the arguments JSON is parsed and the chip reads "Ran <cmd>"; any other (or
// unparseable) call shows the bare tool name. Status stays "running" until a
// matching function_call_output back-patches it.
func functionCallInfo(pl tPayload) *provider.ToolInfo {
	name := pl.Name
	if name == "" {
		name = "tool"
	}
	title := name
	if pl.Name == "exec_command" {
		var args struct {
			Cmd string `json:"cmd"`
		}
		if json.Unmarshal([]byte(pl.Arguments), &args) == nil {
			if cmd := firstLine(args.Cmd); cmd != "" {
				title = "Ran " + truncate(cmd, 120)
			}
		}
	}
	return &provider.ToolInfo{
		Name:   name,
		Title:  title,
		Input:  truncate(pl.Arguments, truncateLimit),
		Status: "running",
	}
}

// customToolCallInfo maps a custom_tool_call payload to a chip: apply_patch
// reads "Applied patch <first file>" when the patch's first file line is
// cheaply parseable, else "Applied patch"; other custom tools show the bare
// name. Status stays "running" until the custom_tool_call_output resolves it
// by call_id (the payload's own status field is ignored — one resolution
// path for both tool kinds).
func customToolCallInfo(pl tPayload) *provider.ToolInfo {
	name := pl.Name
	if name == "" {
		name = "tool"
	}
	title := name
	if pl.Name == "apply_patch" {
		title = "Applied patch"
		if f := patchFirstFile(pl.Input); f != "" {
			title += " " + f
		}
	}
	return &provider.ToolInfo{
		Name:   name,
		Title:  title,
		Input:  truncate(pl.Input, truncateLimit),
		Status: "running",
	}
}

// patchFirstFile extracts the first file path from an apply_patch body
// ("*** Add File: hello.txt" / "*** Update File: …" / "*** Delete File: …"),
// "" when none parses cheaply.
func patchFirstFile(patch string) string {
	for _, line := range strings.Split(patch, "\n") {
		rest, ok := strings.CutPrefix(line, "*** ")
		if !ok {
			continue
		}
		for _, verb := range []string{"Add File: ", "Update File: ", "Delete File: ", "Move to: "} {
			if f, ok := strings.CutPrefix(rest, verb); ok {
				return strings.TrimSpace(f)
			}
		}
	}
	return ""
}

// patchToolOutput back-patches a tool chip when its output record lands.
// Status is always "ok": codex does not mark failure distinctly on
// function_call_output/custom_tool_call_output — a failed command's exit
// code rides inside the output text ("Process exited with code 1"), which
// the model reads, not lab.
func patchToolOutput(t *provider.ToolInfo, output string) {
	if t == nil {
		return
	}
	t.Output = truncate(output, truncateLimit)
	t.Status = "ok"
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
