package claudecode

// Chat surface: locate and read Claude Code's live JSONL transcript, mapped
// into lab's provider-neutral universal schema (issue #7 / ADR-0016). Every
// exact string here — the cwd→slug rule, the transcript path shape, and the
// JSONL event field names — is a fragile Claude Code coupling pinned in
// internal/compat (§5) against the installed version (2.1.198 at port time).

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// truncateLimit caps a tool chip's expandable input/output and a dialog
// prompt/plan body — enough to be useful on a phone, small enough to keep the
// messages payload envelope-sized. The chat is chat-first; full fidelity lives
// in the claude.ai deep link.
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

// ReadTranscript implements provider.AgentProvider: read the JSONL transcript
// at path and fold it into the universal schema. A vanished file yields
// provider.ErrTranscriptGone (the run ended and claude retired the file); any
// other read error is returned as-is. Malformed lines are skipped, never fatal
// — a transcript is appended live and its tail can be a half-written line.
func (p *Provider) ReadTranscript(path string) (provider.Chat, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return provider.Chat{}, provider.ErrTranscriptGone
		}
		return provider.Chat{}, err
	}
	defer func() { _ = f.Close() }()
	return ParseTranscript(f), nil
}

// ParseTranscript folds the JSONL lines of r into the universal schema. It is
// a pure function of the byte stream — exported so the compat test drives it
// from a captured fixture. See the transcript grammar pinned in compat.md §5.
func ParseTranscript(r io.Reader) provider.Chat {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // transcripts run to a few MB
	return foldTranscript(sc)
}

func foldTranscript(sc *bufio.Scanner) provider.Chat {
	// First pass: which tool_use ids have a matching tool_result (i.e. are
	// answered). A dialog tool with no result is a pending dialog.
	var lines [][]byte
	answered := map[string]bool{}
	for sc.Scan() {
		b := append([]byte(nil), sc.Bytes()...)
		lines = append(lines, b)
		var it tItem
		if json.Unmarshal(b, &it) != nil || it.Message == nil {
			continue
		}
		for _, blk := range it.Message.blocks() {
			if blk.Type == "tool_result" && blk.ToolUseID != "" {
				answered[blk.ToolUseID] = true
			}
		}
	}

	var (
		msgs    []provider.Message
		seq     int64
		byTool  = map[string]int{} // tool_use id → index into msgs (result back-patch)
		lastKey string             // classification of the last content-bearing event
	)
	emit := func(m provider.Message) int {
		seq++
		m.Seq = seq
		msgs = append(msgs, m)
		return len(msgs) - 1
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
		case "user", "assistant":
			// handled below
		default:
			continue // attachment / ai-title / mode / queue-operation / … are not chat
		}
		if it.Message == nil {
			continue
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
					continue // skip empty and injected-context (isMeta) text
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
				if d, ok := dialogFromToolUse(blk); ok && !answered[blk.ID] {
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
			}
		}
	}

	return provider.Chat{Messages: msgs, State: deriveState(msgs, lastKey), Cursor: seq}
}

// deriveState reduces the transcript tail to one conversational state
// (issue #7 decision 11). A pending dialog dominates; otherwise the last
// content-bearing event decides: the assistant's own prose ends its turn
// (needs input), while a just-sent user message, a running tool, or a fresh
// tool result mean the agent is (about to be) working.
func deriveState(msgs []provider.Message, lastKey string) string {
	if len(msgs) == 0 {
		return provider.StateIdle
	}
	// A pending dialog is the last dialog message with Answerable-or-not; its
	// presence anywhere as the final content event means the agent is blocked.
	if last := msgs[len(msgs)-1]; last.Kind == provider.MessageDialog {
		return provider.StateQuestion
	}
	switch lastKey {
	case "assistant:text":
		return provider.StateNeedsInput
	case "user:text", "tool_use", "tool_result", "assistant:thinking":
		return provider.StateWorking
	case "dialog":
		return provider.StateQuestion
	case "error":
		return provider.StateNeedsInput
	default:
		return provider.StateIdle
	}
}
