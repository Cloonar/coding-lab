package codex

// Chat surface: locate and read codex's rollout JSONL transcript, mapped
// into lab's provider-neutral universal schema (issue #7 / ADR-0016). Every
// exact string here — the sessions-tree layout, the rollout filename shape,
// and the record grammar — is a fragile Codex coupling pinned against
// codex-cli 0.133.0 (issue #87 live rollouts, 2026-07-10).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// truncateLimit caps a tool chip's expandable input/output — enough to be
// useful on a phone, small enough to keep the messages payload
// envelope-sized (claudecode's pinned value).
const truncateLimit = 2000

// LocateTranscript implements provider.AgentProvider: find the rollout file
// for the session running in worktree. Rollouts live at
// <SessionsDir>/YYYY/MM/DD/rollout-<local-ts>-<uuid>.jsonl and the first
// line is a session_meta record whose payload.cwd is the session's working
// directory. Date directories are scanned newest-first (years desc, months
// desc, days desc; the components are zero-padded, so a string sort is a
// date sort) and files within a day sorted by filename desc — NOTE the
// filename timestamp is LOCAL time while the payload timestamps are UTC, so
// only the filename order is used for recency, never compared against
// payload times. Only the first line of each candidate is read. Returns ""
// (no error) when no rollout matches.
//
// Clear/epoch obligation (issue #51 decision 2): /new in the codex TUI
// natively rotates to a fresh rollout file with a new session id, so the
// newest-first cwd match surfaces the post-clear identity for free — no
// epoch synthesis needed.
func (p *Provider) LocateTranscript(_ context.Context, _ /*sessionName*/, worktree string) (string, error) {
	for _, day := range dateDirsNewestFirst(p.sessionsDir) {
		entries, err := os.ReadDir(day)
		if err != nil {
			continue
		}
		var names []string
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			names = append(names, name)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(names)))
		for _, name := range names {
			path := filepath.Join(day, name)
			if rolloutCwd(path) == worktree {
				return path, nil
			}
		}
	}
	return "", nil
}

// dateDirsNewestFirst walks the sessions tree's YYYY/MM/DD directories in
// reverse chronological order. A missing or unreadable level is silently
// empty — the tree grows lazily with the first rollout.
func dateDirsNewestFirst(root string) []string {
	var days []string
	for _, year := range subdirsDesc(root) {
		for _, month := range subdirsDesc(filepath.Join(root, year)) {
			for _, day := range subdirsDesc(filepath.Join(root, year, month)) {
				days = append(days, filepath.Join(root, year, month, day))
			}
		}
	}
	return days
}

// subdirsDesc lists dir's subdirectory names sorted descending (zero-padded
// date components, so string order is date order).
func subdirsDesc(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names
}

// rolloutCwd reads ONLY the first line of the rollout at path and returns
// its session_meta payload.cwd, or "" on any miss (unreadable file, a
// malformed line, a non-session_meta first record).
func rolloutCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // session_meta carries multi-KB base_instructions
	if !sc.Scan() {
		return ""
	}
	var rec struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.Type != "session_meta" {
		return ""
	}
	return rec.Payload.Cwd
}

// ReadTranscript implements provider.AgentProvider: read the rollout JSONL
// at path and fold it into the universal schema. A vanished file yields
// provider.ErrTranscriptGone; any other read error is returned as-is.
// Malformed lines are skipped, never fatal — a rollout is appended live and
// its tail can be a half-written line.
func (p *Provider) ReadTranscript(path string) (provider.Chat, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return provider.Chat{}, provider.ErrTranscriptGone
		}
		return provider.Chat{}, err
	}
	defer func() { _ = f.Close() }()
	return ParseTranscript(f)
}

// ParseTranscript folds the rollout JSONL lines of r into the universal
// schema. It is a pure function of the byte stream — exported so the compat
// test drives it from a captured fixture. See chat_types.go for the record
// grammar pins. A scan error (a line beyond the buffer cap, an I/O failure)
// is returned rather than swallowed: silently stopping mid-file would serve
// a truncated chat — and a cursor that moves backwards — forever.
func ParseTranscript(r io.Reader) (provider.Chat, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // rollouts run to a few MB
	return foldTranscript(sc)
}

func foldTranscript(sc *bufio.Scanner) (provider.Chat, error) {
	var (
		msgs     []provider.Message
		seq      int64
		byCall   = map[string]int{} // call_id → index into msgs (output back-patch)
		lastTurn string             // last task_started/task_complete/turn_aborted seen
	)
	emit := func(m provider.Message) int {
		seq++
		m.Seq = seq
		msgs = append(msgs, m)
		return len(msgs) - 1
	}

	for sc.Scan() {
		var rec tRecord
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue // half-written tail or foreign line — skip, never fatal
		}
		var pl tPayload
		if len(rec.Payload) > 0 && json.Unmarshal(rec.Payload, &pl) != nil {
			continue
		}
		switch rec.Type {
		case "response_item":
			switch pl.Type {
			case "message":
				// SKIP entirely: developer/user response_items are synthetic
				// context wrappers (<environment_context>, <permissions
				// instructions>, <turn_aborted>…), and the operator's real
				// prompt / the assistant's prose arrive via the user_message /
				// agent_message event_msg records below — consuming both would
				// double-render every turn.
			case "reasoning":
				// content is encrypted (encrypted_content); only a non-empty
				// summary is renderable. Summary items are checked
				// defensively for the {type:"summary_text",text} shape.
				if think := pl.summaryText(); think != "" {
					emit(provider.Message{Kind: provider.MessageText, Role: "assistant",
						Time: rec.Timestamp, Text: think, Thinking: true})
				}
			case "function_call":
				idx := emit(provider.Message{Kind: provider.MessageTool, Time: rec.Timestamp,
					Tool: functionCallInfo(pl)})
				if pl.CallID != "" {
					byCall[pl.CallID] = idx
				}
			case "custom_tool_call":
				idx := emit(provider.Message{Kind: provider.MessageTool, Time: rec.Timestamp,
					Tool: customToolCallInfo(pl)})
				if pl.CallID != "" {
					byCall[pl.CallID] = idx
				}
			case "function_call_output", "custom_tool_call_output":
				if idx, ok := byCall[pl.CallID]; ok {
					patchToolOutput(msgs[idx].Tool, pl.Output)
				}
			default:
				// unknown response_item payloads are skipped (forward-compatible)
			}
		case "event_msg":
			switch pl.Type {
			case "user_message":
				// The operator's actual prompt (typed or seeded).
				if text := strings.TrimSpace(pl.Message); text != "" {
					emit(provider.Message{Kind: provider.MessageText, Role: "user",
						Time: rec.Timestamp, Text: text})
				}
			case "agent_message":
				// Assistant prose; phase (commentary|final_answer|…) is not
				// distinguished — both are visible assistant turns.
				if text := strings.TrimSpace(pl.Message); text != "" {
					emit(provider.Message{Kind: provider.MessageText, Role: "assistant",
						Time: rec.Timestamp, Text: text})
				}
			case "task_started":
				lastTurn = "started"
			case "task_complete":
				lastTurn = "complete"
			case "turn_aborted":
				reason := strings.TrimSpace(pl.Reason)
				if reason == "" {
					reason = "aborted"
				}
				emit(provider.Message{Kind: provider.MessageLifecycle, Time: rec.Timestamp, Text: reason})
				lastTurn = "aborted"
			default:
				// token_count, patch_apply_end, and unknown event types are
				// skipped silently (forward-compatible)
			}
		default:
			// session_meta, turn_context, and unknown record types carry no
			// chat content
		}
	}
	if err := sc.Err(); err != nil {
		return provider.Chat{}, fmt.Errorf("scanning transcript: %w", err)
	}
	return provider.Chat{Messages: msgs, State: deriveState(msgs, lastTurn), Cursor: seq}, nil
}

// deriveState reduces the transcript tail to one conversational state
// (issue #7 decision 11): the LAST of task_started/task_complete/turn_aborted
// wins — started means mid-turn, complete and aborted both hand the
// composer back to the operator. With none seen, an empty rollout is a
// fresh spawn (idle) and anything else awaits the operator. No dialogs
// exist in v1 (never-ask posture, no structured question tool), so
// StateQuestion is never derived.
func deriveState(msgs []provider.Message, lastTurn string) string {
	switch lastTurn {
	case "started":
		return provider.StateWorking
	case "complete", "aborted":
		return provider.StateNeedsInput
	}
	if len(msgs) == 0 {
		return provider.StateIdle
	}
	return provider.StateNeedsInput
}
