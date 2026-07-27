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
//	event_msg:     {"type":"token_count","info":{"last_token_usage":{"total_tokens":…,
//	                "reasoning_output_tokens":…},"model_context_window":…}} — READ for
//	                the context-pressure meter (issue #243 / ADR-0061); the degenerate
//	                {"info":null} shape codex also writes is tolerated (no meter)
//	event_msg:     {"type":"patch_apply_end",…} — skipped

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

	// event_msg / token_count carries the context-pressure meter (issue #243 /
	// ADR-0061): only this event type populates Info; every other payload leaves
	// it nil (including the degenerate {"info":null} token_count records).
	Info *tTokenInfo `json:"info"`
}

// tTokenInfo is a token_count event's info object (issue #243 / ADR-0061). The
// context-pressure meter reads ONLY last_token_usage — the latest request's
// accounting, whose input_tokens already carry the whole conversation — and the
// self-reported model_context_window. total_token_usage (cumulative session
// spend) and rate_limits are deliberately NOT decoded: the meter is occupancy,
// not cumulative cost. Info is nil for the {"info":null} shape codex writes for
// some token_count records (LastTokenUsage then nil too).
type tTokenInfo struct {
	LastTokenUsage     *tTokenUsage `json:"last_token_usage"`
	ModelContextWindow int64        `json:"model_context_window"`
}

// tTokenUsage is the subset of a token_count usage object the meter needs:
// codex's own tokens_in_context_window = total_tokens − reasoning_output_tokens
// (reasoning output is dropped from the window between turns). The input/cached/
// output token counts are not decoded — nothing downstream reads them.
type tTokenUsage struct {
	TotalTokens           int64 `json:"total_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
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
//
// codex's two exec shapes additionally carry a command ToolView (issue #146),
// the panel-sized companion to the one-line chip: exec_command's {"cmd":…} and
// shell's {"command":…} (a string or an argv array, see shellCommand) each
// yield a ToolViewCommand when the command parses non-empty. Every other
// function_call has no rich view (View nil), so the client falls back to the
// raw Input chip.
func functionCallInfo(pl tPayload) *provider.ToolInfo {
	name := pl.Name
	if name == "" {
		name = "tool"
	}
	title := name
	var view *provider.ToolView
	switch pl.Name {
	case "exec_command":
		var args struct {
			Cmd string `json:"cmd"`
		}
		if json.Unmarshal([]byte(pl.Arguments), &args) == nil {
			if cmd := firstLine(args.Cmd); cmd != "" {
				title = "Ran " + truncate(cmd, 120)
			}
			// The chip trims to the first line; the panel view carries the
			// whole (possibly multi-line) command verbatim.
			if args.Cmd != "" {
				view = &provider.ToolView{Kind: provider.ToolViewCommand, Command: provider.TruncateDetail(args.Cmd)}
			}
		}
	case "shell":
		if cmd := shellCommand(pl.Arguments); cmd != "" {
			view = &provider.ToolView{Kind: provider.ToolViewCommand, Command: provider.TruncateDetail(cmd)}
		}
	}
	return &provider.ToolInfo{
		Name:   name,
		Title:  title,
		Input:  truncate(pl.Arguments, truncateLimit),
		Status: "running",
		View:   view,
	}
}

// shellCommand extracts the runnable command line from a codex "shell" tool's
// arguments JSON, "" when it does not parse or is empty. The shape is
// {"command": …} where command is either a bare string or an argv array; the
// wrapper forms ["bash","-lc",X] and ["sh","-c",X] collapse to just X (the
// operator wants the script, not the interpreter incantation), and any other
// array joins with a single space. This shape is DEFENSIVE — there is no live
// shell rollout in testdata, so it mirrors codex's documented grammar the way
// tPayload pins the rest of the record union, held down by unit tests rather
// than a fixture spike.
func shellCommand(arguments string) string {
	var args struct {
		Command json.RawMessage `json:"command"`
	}
	if json.Unmarshal([]byte(arguments), &args) != nil || len(args.Command) == 0 {
		return ""
	}
	// String form: the command line as typed.
	var s string
	if json.Unmarshal(args.Command, &s) == nil {
		return s
	}
	// Array form: unwrap the two known interpreter wrappers to their script,
	// else join the argv with single spaces.
	var parts []string
	if json.Unmarshal(args.Command, &parts) != nil {
		return ""
	}
	if len(parts) == 3 && ((parts[0] == "bash" && parts[1] == "-lc") || (parts[0] == "sh" && parts[1] == "-c")) {
		return parts[2]
	}
	return strings.Join(parts, " ")
}

// customToolCallInfo maps a custom_tool_call payload to a chip: apply_patch
// reads "Applied patch <first file>" when the patch's first file line is
// cheaply parseable, else "Applied patch"; other custom tools show the bare
// name. Status stays "running" until the custom_tool_call_output resolves it
// by call_id (the payload's own status field is ignored — one resolution
// path for both tool kinds).
//
// apply_patch also carries a diff ToolView (issue #146): Path is the first
// touched file (the panel's one-line header) and Text is the V4A envelope
// normalized to unified-diff text (normalizePatch). Any other custom tool has
// no rich view.
func customToolCallInfo(pl tPayload) *provider.ToolInfo {
	name := pl.Name
	if name == "" {
		name = "tool"
	}
	title := name
	var view *provider.ToolView
	if pl.Name == "apply_patch" {
		first := patchFirstFile(pl.Input)
		title = "Applied patch"
		if first != "" {
			title += " " + first
		}
		if pl.Input != "" {
			view = &provider.ToolView{
				Kind: provider.ToolViewDiff,
				Path: first,
				Text: provider.TruncateDetail(normalizePatch(pl.Input)),
			}
		}
	}
	return &provider.ToolInfo{
		Name:   name,
		Title:  title,
		Input:  truncate(pl.Input, truncateLimit),
		Status: "running",
		View:   view,
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

// normalizePatch rewrites codex's apply_patch envelope (the "V4A" patch
// grammar — *** Begin Patch / *** Update File: X / *** Move to: Y / …) into
// the client-renderable unified-diff text a ToolViewDiff carries. The SPA
// colors each line purely by its prefix (+++/---/@@ → header, + → add, - →
// del), so the transform's only job is to translate codex's *** envelope
// verbs into the ---/+++ file headers a unified diff leads with; the body's
// @@, context, +, and - lines are already prefix-shaped and pass through
// verbatim.
//
// The file headers belong IN this text, not in ToolView.Path, precisely
// because one apply_patch can span several files: the diff must carry every
// file's own ---/+++ pair inline so all of them render, while Path carries
// only the FIRST (patchFirstFile) as the panel's one-line header. A trailing
// newline is trimmed up front so the rebuilt diff never ends on a blank line.
func normalizePatch(patch string) string {
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		rest, isEnvelope := strings.CutPrefix(line, "*** ")
		switch {
		case !isEnvelope:
			// Body line (@@, context, +, -): already prefix-shaped — verbatim.
			out = append(out, line)
		case rest == "Begin Patch", rest == "End Patch", rest == "End of File":
			// Envelope framing carries no diff content — dropped.
		case strings.HasPrefix(rest, "Add File: "):
			f := strings.TrimSpace(strings.TrimPrefix(rest, "Add File: "))
			out = append(out, "--- /dev/null", "+++ b/"+f)
		case strings.HasPrefix(rest, "Update File: "):
			f := strings.TrimSpace(strings.TrimPrefix(rest, "Update File: "))
			to := f
			// A rename rides in the immediately following *** Move to: line —
			// consume it so the +++ side names the destination path.
			if i+1 < len(lines) {
				if mv, ok := strings.CutPrefix(lines[i+1], "*** Move to: "); ok {
					to = strings.TrimSpace(mv)
					i++
				}
			}
			out = append(out, "--- a/"+f, "+++ b/"+to)
		case strings.HasPrefix(rest, "Delete File: "):
			f := strings.TrimSpace(strings.TrimPrefix(rest, "Delete File: "))
			out = append(out, "--- a/"+f, "+++ /dev/null")
		default:
			// An unknown *** verb (a forward-compatible envelope addition) is
			// dropped — it must never leak into the rendered diff.
		}
	}
	return strings.Join(out, "\n")
}

// patchToolOutput back-patches a tool chip when its output record lands.
// Status is always "ok": codex does not mark failure distinctly on
// function_call_output/custom_tool_call_output — a failed command's exit
// code rides inside the output text ("Process exited with code 1"), which
// the model reads, not lab.
//
// Output is sized for the detail panel (issue #146): DetailTruncateLimit
// (20KB) with an explicit truncation marker, an order of magnitude above the
// 2000-byte chip cap the Title/Input still keep — the panel shows the whole
// command result, the chip stays a glance.
func patchToolOutput(t *provider.ToolInfo, output string) {
	if t == nil {
		return
	}
	t.Output = provider.TruncateDetail(output)
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
