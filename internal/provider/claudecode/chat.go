package claudecode

// Chat surface: locate and read Claude Code's live JSONL transcript, mapped
// into lab's provider-neutral universal schema (issue #7 / ADR-0016), and
// compose the run's conversational state from the transcript plus the hook
// spool's live signals (issue #92 — composition is adapter-owned). Every
// exact string here — the cwd→slug rule, the transcript path shape, and the
// JSONL event field names — is a fragile Claude Code coupling pinned in
// internal/compat (§5) against the installed version (2.1.198 at port time).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// truncateLimit caps a tool chip's expandable input/output and a local-command
// stdout lifecycle — enough to be useful on a phone, small enough to keep the
// messages payload envelope-sized. The chat is chat-first; full fidelity lives
// in the claude.ai deep link. Dialog prompts are exempt (issue #56 decision 4):
// a plan body renders in full — a cut plan is unreviewable, and plans are
// model-output-bounded — and a question prompt doubles as the key
// toolUseResult.answers records under, so truncating it would break the
// answered-dialog outcome lookup (dialogoutcome.go).
const truncateLimit = 2000

// SlugForDir renders claude's transcript project-directory name for an
// absolute cwd: every byte that is not an ASCII letter or digit becomes '-'.
// So /home/x/.local/state → -home-x--local-state (the '/' before '.local' and
// the '.' both map to '-', doubling). Pinned live against 2.1.198 (compat.md
// §5); exported for the compat slug test.
func SlugForDir(dir string) string {
	var b strings.Builder
	b.Grow(len(dir))
	for _, r := range dir {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	return b.String()
}

// LocateTranscript implements provider.AgentProvider: find the transcript
// file for the session running in worktree. It reuses the deep-link registry
// (~/.claude/sessions/<pid>.json) — the newest live claude whose cwd matches
// worktree — to read its sessionId, then builds
// <projects>/<slug(worktree)>/<sessionId>.jsonl and returns it if the file
// exists. A miss (no matching live process, or the file not yet written)
// returns "" with no error, exactly like CaptureDeepLink; the caller retries.
func (p *Provider) LocateTranscript(_ context.Context, _ /*sessionName*/, worktree string) (string, error) {
	sessionID := sessionIDForDir(p.registryDir, worktree)
	if sessionID == "" {
		return "", nil
	}
	path := filepath.Join(p.projectsDir, SlugForDir(worktree), sessionID+".jsonl")
	if _, err := os.Stat(path); err != nil {
		return "", nil
	}
	return path, nil
}

// sessionIDForDir is bridgeURLForDir's sibling: a single registry pass for the
// sessionId of the newest live claude process whose cwd is dir, or "". Unlike
// the deep link, sessionId is present from process start (it is the transcript
// filename), so it needs no bridge-connect wait.
func sessionIDForDir(registryDir, dir string) string {
	files, err := os.ReadDir(registryDir)
	if err != nil {
		return ""
	}
	var best RegistryEntry
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(registryDir, f.Name()))
		if err != nil {
			continue
		}
		var e RegistryEntry
		if json.Unmarshal(b, &e) != nil {
			continue
		}
		if e.SessionID == "" || e.Cwd != dir || !pidAlive(e.PID) {
			continue
		}
		if best.SessionID == "" || e.StartedAt > best.StartedAt {
			best = e
		}
	}
	return best.SessionID
}

// ReadChat implements provider.AgentProvider: read the run's conversation and
// compose its conversational state (issue #92 — "what state is my agent in"
// is adapter-owned now; core keeps only the lifecycle StateEnded override).
//
// Base read: the JSONL transcript at transcriptPath, folded into the
// universal schema. transcriptPath == "" is an active run whose transcript is
// not yet located — an idle empty chat, never an error, with the live
// signals still consulted (a pending dialog can exist before
// LocateTranscript first hits). A vanished file yields
// provider.ErrTranscriptGone (the run ended and claude retired the file)
// WITHOUT consulting the spool — a read error always precedes the overlay,
// exactly as it did when core composed; any other open error is returned
// as-is. Malformed lines are skipped, never fatal — a transcript is appended
// live and its tail can be a half-written line.
//
// Unlike the pure ParseTranscript, the read is INTENT-AWARE (issue #51
// decision 3): resolved dialog tools are verified against the answers lab
// recorded at AnswerDialog time, and a mismatch emits a lifecycle warning
// right after the tool result (dialogintent.go). The intent registry is
// in-memory, so a warning present before a lab restart disappears from parses
// after it — accepted, the backstop is advisory-only (compat §5).
//
// Live-signal overlay (runtimeDir non-empty; "" degrades to the transcript
// alone — ended runs, or no runtime dir configured), in the exact precedence
// core's applyLiveSignals used to apply (ADR-0020): a live spool dialog
// forces StateQuestion and rides Chat.PendingDialog — the side-channel field,
// never a synthetic message, so seq numbers stay reparse-stable when the real
// tool_use retro-flushes (issues #89/#90); else a pending dialog the
// transcript itself shows stands on its own (the dormant flushed-tool_use
// fallback); else a live blocked marker forces StateNeedsInput.
func (p *Provider) ReadChat(runID, runtimeDir, transcriptPath string) (provider.Chat, error) {
	chat := provider.Chat{State: provider.StateIdle}
	if transcriptPath != "" {
		f, err := os.Open(transcriptPath)
		if err != nil {
			if os.IsNotExist(err) {
				return provider.Chat{}, provider.ErrTranscriptGone
			}
			return provider.Chat{}, err
		}
		chat, err = parseTranscript(f, &p.intents)
		_ = f.Close()
		if err != nil {
			return provider.Chat{}, err
		}
	}
	if runtimeDir == "" {
		return chat, nil // signals off — the transcript-only read
	}
	if d, ok := p.pendingDialog(runID, runtimeDir, transcriptPath); ok {
		dc := d
		chat.State = provider.StateQuestion
		chat.PendingDialog = &dc
		return chat, nil
	}
	if chat.State == provider.StateQuestion {
		return chat, nil // a transcript-flushed dialog stands on its own (dormant fallback)
	}
	if st, ok := p.blockedState(runID, runtimeDir, transcriptPath); ok {
		chat.State = st
	}
	return chat, nil
}

// ParseTranscript folds the JSONL lines of r into the universal schema. It is
// a pure function of the byte stream — exported so the compat test drives it
// from a captured fixture (Provider.ReadChat layers the intent-aware
// verification and the live-signal state composition on top). See the
// transcript grammar pinned in
// compat.md §5. A scan error (a line beyond the buffer cap, an I/O failure)
// is returned rather than swallowed: silently stopping mid-file would serve a
// truncated chat — and a cursor that moves backwards — forever.
func ParseTranscript(r io.Reader) (provider.Chat, error) {
	return parseTranscript(r, nil)
}

// parseTranscript is the shared fold; a nil intents skips verification.
func parseTranscript(r io.Reader, intents *intentRegistry) (provider.Chat, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // transcripts run to a few MB
	return foldTranscript(sc, intents)
}

func foldTranscript(sc *bufio.Scanner, intents *intentRegistry) (provider.Chat, error) {
	// First pass: which tool_use ids have a matching tool_result (i.e. are
	// answered), and HOW each resolved — the top-level toolUseResult /
	// toolDenialKind of the event carrying the result block, the same ground
	// truth the verification backstop reads (compat §5). Presence in the map
	// means answered; the recorded resolution feeds the answered dialog's
	// Outcome at emission time (issue #56 decision 3). A dialog tool with no
	// entry is a pending dialog.
	var lines [][]byte
	resolutions := map[string]resolvedTool{}
	for sc.Scan() {
		b := append([]byte(nil), sc.Bytes()...)
		lines = append(lines, b)
		var it tItem
		if json.Unmarshal(b, &it) != nil || it.Message == nil {
			continue
		}
		for _, blk := range it.Message.blocks() {
			if blk.Type == "tool_result" && blk.ToolUseID != "" {
				resolutions[blk.ToolUseID] = resolvedTool{
					toolUseResult: it.ToolUseResult, denialKind: it.ToolDenialKind,
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return provider.Chat{}, fmt.Errorf("scanning transcript: %w", err)
	}

	var (
		msgs    []provider.Message
		seq     int64
		byTool  = map[string]int{} // tool_use id → index into msgs (result back-patch)
		lastKey string             // classification of the last content-bearing event
		// pending tracks async background work this session spawned — agent /
		// workflow ids added structurally from toolUseResult markers, removed on
		// their terminal task-notification or TaskStop (issue #159). While
		// non-empty, an assistant text tail derives `working`, not needs_input:
		// the CLI will re-invoke the model when the work completes, so pushing
		// the operator at turn end would be spurious. Session rotation (/clear,
		// /rewind) needs no reset — folding restarts on the fresh transcript.
		pending = map[string]struct{}{}
	)
	emit := func(m provider.Message) int {
		seq++
		m.Seq = seq
		msgs = append(msgs, m)
		return len(msgs) - 1
	}
	// resolveNotification applies a task-notification payload to the pending
	// set: the <task-id> leaves the set only when a non-empty <status> tag
	// (wild: completed|failed|killed — any value is terminal) accompanies it; a
	// status-less payload is a Monitor interim <event> and must not remove.
	// Idempotent by construction — duplicate notifications for one id are real
	// (SendMessage resume re-notifies, Monitor notifies per event), and a
	// TaskStop's killed enqueue can precede the TaskStop line in file order, so
	// removal is order-tolerant set-delete, never an error.
	resolveNotification := func(payload string) {
		id := tagContent(payload, "task-id")
		if id == "" || strings.TrimSpace(tagContent(payload, "status")) == "" {
			return
		}
		delete(pending, id)
	}

	for _, b := range lines {
		var it tItem
		if json.Unmarshal(b, &it) != nil {
			continue
		}
		switch it.Type {
		case "system":
			if it.Subtype == "bridge_status" && it.Content != "" {
				emit(provider.Message{Kind: provider.MessageLifecycle, Time: it.Timestamp, Text: it.Content})
			}
			continue
		case "queue-operation":
			// Task-notification carrier 1 (issue #159): every notification
			// writes an enqueue first, so this is the earliest removal signal;
			// dequeue/remove repeat the same content and are handled identically
			// (the operation field is deliberately not read). A payload-carrying
			// queue-operation also bumps lastKey: an enqueued notification means
			// the CLI is about to re-invoke the model — the same "(about to be)
			// working" philosophy user:text encodes — closing the needs_input
			// micro-window between enqueue and delivery. Emits no message.
			if payload := strings.TrimSpace(it.Content); strings.HasPrefix(payload, "<task-notification>") {
				resolveNotification(payload)
				lastKey = "task-notification"
			}
			continue
		case "attachment":
			// Task-notification carrier 3 (issue #159): mid-turn delivery rides
			// an attachment{type:"queued_command",commandMode:"task-notification"}
			// with the payload in prompt. Other attachment types and commandModes
			// (task_reminder, edited_text_file, operator-queued messages) stay
			// ignored and state-neutral. Emits no message.
			if it.Attachment != nil && it.Attachment.CommandMode == "task-notification" {
				resolveNotification(strings.TrimSpace(it.Attachment.Prompt))
				lastKey = "task-notification"
			}
			continue
		case "user", "assistant":
			// handled below
		default:
			continue // ai-title / mode / … are not chat
		}
		if it.Message == nil {
			continue
		}
		// Pending-work markers on the event's top-level toolUseResult (issue
		// #159, structural — never message-content prose): an async launch
		// (status "async_launched") adds the agent's id (agentId for Agent,
		// taskId for Workflow — the id its notifications will reference); a
		// SendMessage resume (resumedAgentId) re-adds a stopped agent it just
		// re-activated. See pendingWorkResult for what the status gate excludes
		// (background Bash, Monitor, synchronous Agent calls).
		if it.Type == "user" {
			r := decodePendingWork(it.ToolUseResult)
			if r.Status == "async_launched" {
				if id := r.AgentID; id != "" {
					pending[id] = struct{}{}
				} else if r.TaskID != "" {
					pending[r.TaskID] = struct{}{}
				}
			}
			if r.ResumedAgentID != "" {
				pending[r.ResumedAgentID] = struct{}{}
			}
		}
		// Synthetic API-error assistant lines: always-surface, never hidden.
		if it.IsApiErrorMessage {
			emit(provider.Message{Kind: provider.MessageLifecycle, Role: it.Message.Role,
				Time: it.Timestamp, Text: it.Message.text(), Error: true})
			lastKey = "error"
			continue
		}
		for _, blk := range it.Message.blocks() {
			switch blk.Type {
			case "text":
				text := strings.TrimSpace(blk.textField())
				if text == "" || it.IsMeta {
					continue // skip empty and injected-context (isMeta) blocks
				}
				// A local slash-command echo renders as a plain user text
				// message showing the command line, its captured output as a
				// lifecycle message — but NEITHER touches lastKey: echoes are
				// excluded from state derivation (issues #45/#51, compat §5).
				if cmd, stdout, isEcho := commandEcho(text); isEcho {
					if cmd != "" {
						emit(provider.Message{Kind: provider.MessageText, Role: it.Message.Role, Time: it.Timestamp, Text: cmd})
					}
					if stdout != "" {
						emit(provider.Message{Kind: provider.MessageLifecycle, Time: it.Timestamp, Text: truncate(stdout, truncateLimit)})
					}
					continue
				}
				// Task-notification carrier 2 (issue #159): between-turns
				// delivery is a standalone user message whose plain-string
				// content leads with <task-notification> (no isMeta on these
				// events — compat §5; classified by trimmed prefix, exactly
				// like commandEcho above). Rendering is unchanged — it stays a
				// visible user text message — but it resolves the pending id
				// and takes the task-notification key, which derives working
				// just as user:text did before.
				if strings.HasPrefix(text, "<task-notification>") {
					emit(provider.Message{Kind: provider.MessageText, Role: it.Message.Role, Time: it.Timestamp, Text: text})
					resolveNotification(text)
					lastKey = "task-notification"
					continue
				}
				emit(provider.Message{Kind: provider.MessageText, Role: it.Message.Role, Time: it.Timestamp, Text: text})
				lastKey = it.Message.Role + ":text"
			case "thinking":
				think := strings.TrimSpace(blk.Thinking)
				if think == "" {
					continue
				}
				emit(provider.Message{Kind: provider.MessageText, Role: "assistant", Time: it.Timestamp, Text: think, Thinking: true})
				lastKey = "assistant:thinking"
			case "tool_use":
				// A dialog tool is a DIALOG message whether pending or resolved
				// (issue #56 decision 3): a resolved one carries the recorded
				// answer in Outcome so history renders a compact Q→A summary,
				// never the raw JSON chip it used to demote to. Only genuinely
				// pending dialogs set lastKey="dialog"; a resolved dialog
				// behaves like a resolved tool for state derivation — exactly
				// as its demoted chip did — and is NOT registered in byTool:
				// its Outcome is derived up front from the first pass, there
				// is no chip to back-patch when its tool_result event follows.
				// TaskStop removes its target from the pending set immediately
				// (issue #159) — belt-and-braces with the killed notification
				// that also arrives, sometimes enqueued BEFORE this line in
				// file order (line order ≠ causal order around
				// queue-operations), which set-delete tolerates either way.
				// Everything else about the TaskStop chip — emission, byTool
				// back-patch, lastKey — stays the ordinary tool path below.
				if blk.Name == "TaskStop" {
					if id := str(decodeToolInput(blk.Input)["task_id"]); id != "" {
						delete(pending, id)
					}
				}
				if d, ok := dialogFromToolUse(blk); ok {
					if res, resolved := resolutions[blk.ID]; resolved {
						d.Outcome = outcomeFor(d, res)
						emit(provider.Message{Kind: provider.MessageDialog, Time: it.Timestamp, Dialog: &d})
						lastKey = "tool_use"
						continue
					}
					emit(provider.Message{Kind: provider.MessageDialog, Time: it.Timestamp, Dialog: &d})
					lastKey = "dialog"
					continue
				}
				idx := emit(provider.Message{Kind: provider.MessageTool, Time: it.Timestamp, Tool: toolInfo(blk)})
				byTool[blk.ID] = idx
				lastKey = "tool_use"
			case "tool_result":
				if idx, ok := byTool[blk.ToolUseID]; ok {
					patchToolResult(msgs[idx].Tool, blk)
				}
				lastKey = "tool_result"
				// Verification backstop (issue #51 decision 3): a resolved
				// dialog tool with a recorded answer intent is checked against
				// the event's recorded outcome; a mismatch emits a lifecycle
				// warning immediately after the tool result. Advisory only —
				// it deliberately does not touch lastKey, so the warning never
				// alters state derivation.
				if intents != nil {
					if warn, ok := intents.verify(blk.ToolUseID, resolvedTool{
						toolUseResult: it.ToolUseResult, denialKind: it.ToolDenialKind,
					}); ok && warn != "" {
						emit(provider.Message{Kind: provider.MessageLifecycle, Time: it.Timestamp, Text: warn, Error: true})
					}
				}
			}
		}
	}

	return provider.Chat{Messages: msgs, State: deriveState(msgs, lastKey, len(pending) > 0), Cursor: seq}, nil
}

// commandEcho classifies (and parses) Claude Code's local-command breadcrumbs:
// the slash-command echo it writes when the operator runs a local command
// (`<command-name>…`/`<command-message>…`/`<command-args>…`, e.g. /clear,
// /rewind) and that command's captured output (`<local-command-stdout>…`).
// These carry no isMeta flag (compat.md §5); text is already trimmed, and tag
// order varies with leading whitespace between tags, so classification is a
// trimmed-prefix match and extraction is by tag, never by position.
//
// History: issue #45 DROPPED echoes entirely (they formerly derived `working`
// and stuck the composer on Interrupt after /clear — the state half of that
// fix stands, see foldTranscript: echoes never touch lastKey). Issue #51
// decision 2 reverses the rendering half: the operator's sent command is real
// conversational context, so the echo maps to a visible user text message
// showing the command line — cmd is "<command-name value>[ <command-args
// value>]" (e.g. "/clear", "/foo args") — and a non-empty stdout maps to a
// follow-up lifecycle message. An echo with no extractable command name (or
// an empty stdout) yields empty strings and renders nothing, degrading to the
// old drop.
func commandEcho(text string) (cmd, stdout string, isEcho bool) {
	switch {
	case strings.HasPrefix(text, "<local-command-stdout>"):
		return "", strings.TrimSpace(tagContent(text, "local-command-stdout")), true
	case strings.HasPrefix(text, "<command-name>"),
		strings.HasPrefix(text, "<command-message>"),
		strings.HasPrefix(text, "<command-args>"):
		cmd = strings.TrimSpace(tagContent(text, "command-name")) // carries the slash, live: "/clear"
		if cmd == "" {
			return "", "", true
		}
		if args := strings.TrimSpace(tagContent(text, "command-args")); args != "" {
			cmd += " " + args
		}
		return cmd, "", true
	}
	return "", "", false
}

// tagContent extracts the first <tag>…</tag> body from s, or "" — the minimal
// pull-by-tag parse the echo breadcrumbs need (they are flat, single-level
// pseudo-XML; a real parser would pin more than the coupling warrants).
func tagContent(s, tag string) string {
	open, close := "<"+tag+">", "</"+tag+">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// deriveState reduces the transcript tail to one conversational state
// (issue #7 decision 11). A pending dialog dominates; otherwise the last
// content-bearing event decides: the assistant's own prose ends its turn
// (needs input), while a just-sent user message, a running tool, or a fresh
// tool result mean the agent is (about to be) working.
//
// pendingWork (issue #159) is the async hold: while background agents or
// workflows this session spawned are still running, the assistant's ended
// turn does NOT mean the operator's turn — the CLI re-invokes the model when
// the work completes — so assistant:text derives working instead of
// needs_input and no spurious push fires. Only that one edge is softened: an
// API error stays needs_input unconditionally (errors must surface past
// pending work), and structured live signals (spool dialog, blocked marker —
// ReadChat's overlay) already outrank whatever the transcript derives.
func deriveState(msgs []provider.Message, lastKey string, pendingWork bool) string {
	if len(msgs) == 0 {
		return provider.StateIdle
	}
	// A pending dialog is the last dialog message with Answerable-or-not; its
	// presence anywhere as the final content event means the agent is blocked.
	// Pending ONLY (issue #56 decision 3): an answered dialog (Outcome set)
	// stays a dialog message in history, and a transcript tail of one — the
	// retro-flushed tool_use whose tool_result event carries no message of its
	// own — means the agent just resumed, never that it is waiting.
	if last := msgs[len(msgs)-1]; last.Kind == provider.MessageDialog &&
		last.Dialog != nil && last.Dialog.Outcome == nil {
		return provider.StateQuestion
	}
	switch lastKey {
	case "assistant:text":
		if pendingWork {
			return provider.StateWorking
		}
		return provider.StateNeedsInput
	case "user:text", "tool_use", "tool_result", "assistant:thinking", "task-notification":
		return provider.StateWorking
	case "dialog":
		return provider.StateQuestion
	case "error":
		return provider.StateNeedsInput
	default:
		return provider.StateIdle
	}
}
