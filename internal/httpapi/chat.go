package httpapi

// Embedded-chat endpoints (issue #7 / ADR-0016): read a run's transcript
// mapped into the universal message schema, and reply / answer a dialog /
// interrupt back into the live session. Business logic lives in internal/chat
// (which owns the provider seam); this file translates JSON ⇄ service calls and
// maps typed errors onto status codes. Intervention is budget/claim/strike
// neutral — none of these touch a run's outcome.

import (
	"errors"
	"net/http"
	"strconv"

	"git.cloonar.com/Cloonar/coding-lab/internal/chat"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/claudecode"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// defaultMessagesLimit / maxMessagesLimit bound one messages window.
const (
	defaultMessagesLimit = 60
	maxMessagesLimit     = 500
)

// handleRunGet is GET /api/v1/runs/{id}: one run (any outcome) — the chat
// header's source. 404 for an unknown id.
func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.RunByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRunError(w, "getting run", err)
		return
	}
	writeJSON(w, http.StatusOK, runJSON(run))
}

type messagesResponse struct {
	Messages []provider.Message `json:"messages"`
	State    string             `json:"state"`
	Cursor   int64              `json:"cursor"`   // highest seq present (append cursor)
	HasMore  bool               `json:"has_more"` // older messages exist before this window
	// PendingDialog is the run's live interactive dialog from the PreToolUse
	// spool (ADR-0020), nullable and top-level — NOT a message in the stream,
	// so seq numbers stay reparse-stable. Present alongside state:"question".
	PendingDialog *provider.Dialog `json:"pending_dialog"`
	Transcript    string           `json:"transcript"` // available|locating|gone
}

// handleRunMessages is GET /api/v1/runs/{id}/messages?after=&before=&limit=.
// Default (no cursor): the latest window. after=<seq>: the append tail (seq >
// after). before=<seq>: the older window (the limit messages with seq <
// before) for scroll-up. The append-only cursor is the message Seq.
func (s *Server) handleRunMessages(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.RunByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRunError(w, "reading messages", err)
		return
	}
	limit, ok := parseMessagesLimit(w, r)
	if !ok {
		return
	}
	after, ok := parseSeqParam(w, r, "after")
	if !ok {
		return
	}
	before, ok := parseSeqParam(w, r, "before")
	if !ok {
		return
	}

	resp := messagesResponse{Messages: []provider.Message{}, Transcript: "available"}
	chatData, err := s.chat.Read(r.Context(), run)
	switch {
	case errors.Is(err, provider.ErrTranscriptGone):
		resp.Transcript = "gone"
		resp.State = provider.StateEnded
		writeJSON(w, http.StatusOK, resp)
		return
	case err != nil:
		s.internalError(w, "reading messages", err)
		return
	}
	resp.State = chatData.State
	resp.Cursor = chatData.Cursor
	resp.PendingDialog = chatData.PendingDialog
	if len(chatData.Messages) == 0 && chatData.PendingDialog == nil && run.Outcome == store.RunOutcomeActive {
		resp.Transcript = "locating"
	}

	window, hasMore := windowMessages(chatData.Messages, after, before, limit)
	resp.Messages = window
	resp.HasMore = hasMore
	writeJSON(w, http.StatusOK, resp)
}

// windowMessages selects the requested slice and reports whether older
// messages exist before it. all is ascending by Seq.
func windowMessages(all []provider.Message, after, before int64, limit int) ([]provider.Message, bool) {
	switch {
	case after > 0:
		// Append tail: everything newer than the client's cursor.
		start := 0
		for start < len(all) && all[start].Seq <= after {
			start++
		}
		tail := all[start:]
		hasMore := start > 0 // older messages exist behind the client's cursor
		if len(tail) > limit {
			tail = tail[:limit]
		}
		return append([]provider.Message{}, tail...), hasMore
	case before > 0:
		// Older window: the last `limit` messages strictly before `before`.
		end := 0
		for end < len(all) && all[end].Seq < before {
			end++
		}
		older := all[:end]
		hasMore := false
		if len(older) > limit {
			older = older[len(older)-limit:]
			hasMore = true
		}
		return append([]provider.Message{}, older...), hasMore
	default:
		// Latest window: the last `limit` messages.
		start := 0
		hasMore := false
		if len(all) > limit {
			start = len(all) - limit
			hasMore = true
		}
		return append([]provider.Message{}, all[start:]...), hasMore
	}
}

type replyRequest struct {
	Text string `json:"text"`
}

// handleRunReply is POST /api/v1/runs/{id}/reply — a free-text reply to a live
// instance. 409 for an ended run or a pending dialog; 400 for empty/oversize.
func (s *Server) handleRunReply(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.RunByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRunError(w, "replying", err)
		return
	}
	var req replyRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	if err := s.chat.Reply(r.Context(), run, req.Text); err != nil {
		s.writeChatActionError(w, "replying", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type answerRequest struct {
	ToolID    string `json:"tool_id"`
	Index     int    `json:"index"`
	Selected  []int  `json:"selected"`
	OtherText string `json:"other_text"`
}

// handleRunAnswer is POST /api/v1/runs/{id}/answer — answer the pending dialog.
// tool_id is required: the service re-reads the live dialog under the session
// lock and refuses a mismatch, so a stale client never answers a dialog that
// already moved on.
func (s *Server) handleRunAnswer(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.RunByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRunError(w, "answering", err)
		return
	}
	var req answerRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	if req.ToolID == "" {
		writeError(w, http.StatusBadRequest, "tool_id is required")
		return
	}
	answer := provider.DialogAnswer{Index: req.Index, Selected: req.Selected, OtherText: req.OtherText}
	if err := s.chat.AnswerDialog(r.Context(), run, req.ToolID, answer); err != nil {
		s.writeChatActionError(w, "answering", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRunInterrupt is POST /api/v1/runs/{id}/interrupt — send Escape to a
// live instance. 409 for an ended run.
func (s *Server) handleRunInterrupt(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.RunByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRunError(w, "interrupting", err)
		return
	}
	if err := s.chat.Interrupt(r.Context(), run); err != nil {
		s.writeChatActionError(w, "interrupting", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeRunError maps store lookups for a run onto status codes.
func (s *Server) writeRunError(w http.ResponseWriter, doing string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.internalError(w, doing, err)
}

// writeChatActionError maps reply/answer/interrupt errors onto status codes.
func (s *Server) writeChatActionError(w http.ResponseWriter, doing string, err error) {
	switch {
	case errors.Is(err, chat.ErrRunEnded), errors.Is(err, chat.ErrDialogPending),
		errors.Is(err, chat.ErrNoDialog), errors.Is(err, chat.ErrDialogChanged),
		errors.Is(err, claudecode.ErrDialogNotAnswerable):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, tmuxx.ErrSessionNotFound):
		// The run row says active but the tmux session is gone (killed
		// externally; the reaper hasn't flipped the outcome yet).
		writeError(w, http.StatusConflict, "the instance's session is gone; it will be marked ended shortly")
	case errors.Is(err, claudecode.ErrInvalidReply):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		s.internalError(w, doing, err)
	}
}

func parseMessagesLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	limit := defaultMessagesLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return 0, false
		}
		limit = min(n, maxMessagesLimit)
	}
	return limit, true
}

func parseSeqParam(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		writeError(w, http.StatusBadRequest, name+" must be a non-negative integer")
		return 0, false
	}
	return n, true
}
