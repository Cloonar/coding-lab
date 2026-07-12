package httpapi

// Embedded-chat endpoints (issue #7 / ADR-0016, generalized in issue #51):
// read a run's transcript mapped into the universal message schema, list the
// provider's slash-command catalog for the composer's autocomplete, and reply /
// answer a dialog / interrupt back into the live session:
//   GET  /api/v1/runs/{id}/messages
//   GET  /api/v1/runs/{id}/commands
//   POST /api/v1/runs/{id}/reply
//   POST /api/v1/runs/{id}/answer
//   POST /api/v1/runs/{id}/interrupt
// Business logic lives in internal/chat (which owns the provider seam); this
// file translates JSON ⇄ service calls and maps typed errors onto status
// codes. Intervention is budget/claim/strike neutral — none of these touch a
// run's outcome.
//
// The reply path additionally intercepts LAB COMMANDS (issue #149): composer
// slash commands the server executes itself instead of forwarding to the
// provider. /pull-base is the first — see handleRunReply.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/chat"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/pull"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// defaultMessagesLimit / maxMessagesLimit bound one messages window.
const (
	defaultMessagesLimit = 60
	maxMessagesLimit     = 500
)

// handleRunGet is GET /api/v1/runs/{id}: one run (any outcome) — the chat
// header's source. 404 for an unknown id. Unlike the runs-list and title-PATCH
// handlers, this one also enriches the response with exposed_secrets (issue
// #108): the chat header is the only surface that needs to warn "this run's
// transcript leaked a secret", so the lookup happens exactly here rather than
// inside the shared runJSON, keeping list pages N+1-free. It also populates
// commits_behind (issue #149) for the same reason — one extra RepoByID here
// (for the repo's default branch) is fine on a single-run endpoint; a RepoByID
// failure just leaves commits_behind uncomputed rather than failing the whole
// run detail.
func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.RunByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRunError(w, "getting run", err)
		return
	}
	exposed, err := s.store.ExposedSecretNamesForRun(r.Context(), run.ID)
	if err != nil {
		s.writeRunError(w, "getting run", err)
		return
	}
	resp := runJSON(run)
	resp.ExposedSecrets = exposed
	if repo, err := s.store.RepoByID(r.Context(), run.RepoID); err == nil {
		resp.CommitsBehind = s.commitsBehind(r.Context(), run, repo.DefaultBranch)
	}
	writeJSON(w, http.StatusOK, resp)
}

type messagesResponse struct {
	Messages []provider.Message `json:"messages"`
	State    string             `json:"state"`
	Cursor   int64              `json:"cursor"`   // highest seq present (append cursor)
	HasMore  bool               `json:"has_more"` // older messages exist before this window
	// PendingDialog is the run's live interactive dialog as composed by its
	// adapter's ReadChat (issue #92; claude-code reads it from the PreToolUse
	// spool, ADR-0020), nullable and top-level — NOT a message in the stream,
	// so seq numbers stay reparse-stable (issues #89/#90). Present alongside
	// state:"question".
	PendingDialog *provider.Dialog `json:"pending_dialog"`
	Transcript    string           `json:"transcript"` // available|locating|gone
	// TranscriptID is the opaque identity of the located transcript (issue #34):
	// provider-neutral in the envelope, provider-derived in value. It changes
	// when the run's transcript rotates (a /clear or /rewind → new sessionId →
	// new file, compat §5); the SPA keys its stream reset on it. "" while
	// locating or gone.
	TranscriptID string `json:"transcript_id"`
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
	resp.TranscriptID = chatData.TranscriptID
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

// commandsCacheTTL bounds how long a run's slash-command catalog is memoized
// (issue #51 decision 5). The provider's Commands scans the worktree's
// project/user command directories — too costly to re-run on every
// autocomplete keystroke — so a successful result is cached per run for this
// window; short enough that a freshly-added project command surfaces promptly.
const commandsCacheTTL = 30 * time.Second

// commandsCacheEntry is one memoized Commands result with the wall-clock time
// it was computed (the TTL anchor). Only the served (chat-safe) slice is
// cached, and only on success — a scan error is never memoized, so a transient
// failure stays retryable.
type commandsCacheEntry struct {
	commands []provider.CommandSpec
	at       time.Time
}

type commandsResponse struct {
	Commands []provider.CommandSpec `json:"commands"`
}

// pullBaseCommand is the lab-owned /pull-base catalog entry (issue #149): the
// composer autocompletes it like any provider command, but the reply path
// intercepts and executes it server-side — it never reaches the provider.
// Source "lab" tells the client who owns it. Appended to the commands
// response at build time only, never stored into the provider-commands cache.
var pullBaseCommand = provider.CommandSpec{
	Name:        "pull-base",
	Description: "Pull the base branch into the worktree and brief the agent (lab command)",
	Source:      "lab",
	ChatSafe:    true,
}

// handleRunCommands is GET /api/v1/runs/{id}/commands — the composer's
// slash-command autocomplete source (issue #51 decision 5). Worktree-dependent
// (project/user commands are discovered relative to the run's worktree), hence
// served per run; any run outcome may ask, since an ended run still
// autocompletes its history's commands. Only ChatSafe entries reach the client:
// a command that would strand the TUI in a picker lab cannot see ships
// ChatSafe=false and is curated out by the adapter. The chat-safe slice is
// cached per run for commandsCacheTTL (Commands is a filesystem scan); a scan
// error is a 500 and is never cached. Lab-owned commands (issue #149) join
// the response after the provider entries.
func (s *Server) handleRunCommands(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.RunByID(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRunError(w, "listing commands", err)
		return
	}
	cmds, err := s.chatSafeCommands(r.Context(), run)
	if err != nil {
		s.internalError(w, "listing commands", err)
		return
	}
	// Lab-owned entries are appended onto a fresh slice at response-build
	// time: the cache stays purely provider-derived, and a shared cached
	// backing array must never grow under a concurrent reader.
	out := make([]provider.CommandSpec, 0, len(cmds)+1)
	out = append(out, cmds...)
	if s.pull != nil {
		out = append(out, pullBaseCommand)
	}
	writeJSON(w, http.StatusOK, commandsResponse{Commands: out})
}

// chatSafeCommands resolves run's chat-safe provider command catalog: the
// memoized slice when fresh, otherwise a provider scan whose result is cached
// for commandsCacheTTL (errors never are — a transient failure stays
// retryable). Shared by the commands endpoint and the reply path's clear-hook
// detection, so both see the same catalog under the same cache clock.
func (s *Server) chatSafeCommands(ctx context.Context, run store.Run) ([]provider.CommandSpec, error) {
	if cmds, ok := s.cachedCommands(run.ID); ok {
		return cmds, nil
	}
	// An existing run always names a registered provider, so a miss (or an
	// unwired registry) is a server-side inconsistency — the caller's 500.
	if s.providers == nil {
		return nil, fmt.Errorf("provider registry not configured")
	}
	prov, ok := s.providers.Get(run.Provider)
	if !ok {
		return nil, fmt.Errorf("run %s: unknown provider %q", run.ID, run.Provider)
	}
	all, err := prov.Commands(ctx, run.WorktreePath)
	if err != nil {
		return nil, err
	}
	// Keep only chat-safe entries; always a non-null slice, even when none
	// survive curation (the SPA renders "no commands", never a null crash).
	cmds := make([]provider.CommandSpec, 0, len(all))
	for _, c := range all {
		if c.ChatSafe {
			cmds = append(cmds, c)
		}
	}
	s.storeCommands(run.ID, cmds)
	return cmds, nil
}

// cachedCommands returns a run's memoized chat-safe command catalog when the
// entry is still within commandsCacheTTL, keyed by run id.
func (s *Server) cachedCommands(runID string) ([]provider.CommandSpec, bool) {
	s.commandsMu.Lock()
	defer s.commandsMu.Unlock()
	e, ok := s.commandsCache[runID]
	if !ok || s.now().Sub(e.at) >= commandsCacheTTL {
		return nil, false
	}
	return e.commands, true
}

// storeCommands memoizes a run's chat-safe command catalog under the TTL clock.
func (s *Server) storeCommands(runID string, cmds []provider.CommandSpec) {
	s.commandsMu.Lock()
	defer s.commandsMu.Unlock()
	s.commandsCache[runID] = commandsCacheEntry{commands: cmds, at: s.now()}
}

type replyRequest struct {
	Text string `json:"text"`
}

// noticeResponse is a 200 reply-path body for an outcome the operator must be
// told about that is NOT an error (issue #149): nothing to pull, a merge that
// landed without its digest, a clear that went through unpulled. Plain
// success stays 204.
type noticeResponse struct {
	Notice string `json:"notice"`
}

// handleRunReply is POST /api/v1/runs/{id}/reply — a free-text reply to a live
// instance. 409 for an ended run or a pending dialog; 400 for empty/oversize.
// Two shapes of text divert from the verbatim forward (issue #149):
//
//   - "/pull-base" is the first lab command: the server executes it itself
//     (merge origin/<base> into the run's worktree, then inject the digest as
//     an agent message) and it NEVER reaches the provider — unwired it is
//     refused (503), never forwarded, and an argument form is a 400.
//   - the provider's role=clear command (e.g. claude's /clear) pulls the base
//     BEFORE the clear is forwarded, so the fresh conversation starts on the
//     fresh base. The hook applies only to active runs with a pull service
//     wired; a catalog hiccup reads as "not a clear" (the forward must never
//     block on a scan).
//
// Everything else forwards byte-for-byte as before.
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
	cmd := strings.TrimSpace(req.Text)
	switch {
	case cmd == "/pull-base":
		s.handlePullBase(w, r, run)
		return
	case strings.HasPrefix(cmd, "/pull-base "):
		writeError(w, http.StatusBadRequest, "pull-base takes no arguments")
		return
	}
	if s.pull != nil && run.Outcome == store.RunOutcomeActive && s.isClearCommand(r.Context(), run, cmd) {
		s.handleClearWithPull(w, r, run, req.Text)
		return
	}
	if err := s.chat.Reply(r.Context(), run, req.Text); err != nil {
		s.writeChatActionError(w, "replying", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// isClearCommand reports whether cmd (already whitespace-trimmed) is exactly
// the provider's role=clear command — "/"+spec.Name, no arguments — per the
// run's chat-safe catalog (the same cached view the commands endpoint serves,
// under the same 30s clock). Plain text skips the catalog scan entirely, and
// a resolve error reads as "not a clear": a catalog hiccup must never block a
// clear from going through as an ordinary reply.
func (s *Server) isClearCommand(ctx context.Context, run store.Run, cmd string) bool {
	if !strings.HasPrefix(cmd, "/") {
		return false
	}
	cmds, err := s.chatSafeCommands(ctx, run)
	if err != nil {
		return false
	}
	for _, c := range cmds {
		if c.Role == provider.CommandRoleClear && cmd == "/"+c.Name {
			return true
		}
	}
	return false
}

// handlePullBase executes the /pull-base lab command (issue #149): merge the
// current origin/<base> into the run's live worktree and paste the rendered
// digest to the agent as a chat message. Guard rails, in order:
//
//   - no pull service wired → 503 (refused, never forwarded to the provider);
//   - an ended run → the same 409 an ordinary reply gets;
//   - a POSITIVELY working agent → 409 busy. The gate is a fresh synchronous
//     chat read and only StateWorking refuses — idle/needs-input/question and
//     even a failed read all proceed, because the merge itself is safe in any
//     state; the gate only avoids yanking files out from under a mid-turn
//     agent.
//
// Success is 204 when the digest landed; 200 {"notice"} when there was
// nothing to pull (no agent message — decision: an up-to-date pull says
// nothing); and 200 {"notice"} when the merge landed but the digest could not
// be delivered (a dialog raced in, or the session died mid-pull) — the merge
// is real and must not read as an error, only the context repair failed, and
// the notice says so.
func (s *Server) handlePullBase(w http.ResponseWriter, r *http.Request, run store.Run) {
	if s.pull == nil {
		writeError(w, http.StatusServiceUnavailable, "pull-base is not available")
		return
	}
	if run.Outcome != store.RunOutcomeActive {
		s.writeChatActionError(w, "pulling base", chat.ErrRunEnded)
		return
	}
	if view, err := s.chat.Read(r.Context(), run); err == nil && view.State == provider.StateWorking {
		writeError(w, http.StatusConflict, "agent is busy — retry when idle")
		return
	}
	res, err := s.pull.PullBase(r.Context(), run)
	if err != nil {
		s.writePullError(w, err)
		return
	}
	if res.UpToDate {
		writeJSON(w, http.StatusOK, noticeResponse{Notice: fmt.Sprintf("Already up to date with origin/%s.", res.Base)})
		return
	}
	if err := s.chat.Reply(r.Context(), run, res.Digest); err != nil {
		writeJSON(w, http.StatusOK, noticeResponse{Notice: fmt.Sprintf(
			"Pulled origin/%s (%s..%s) but the digest was not delivered: %s. The agent may still assume the old base.",
			res.Base, short12(res.OldHead), short12(res.NewHead), err)})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleClearWithPull is the clear-hook (issue #149): a reply that is the
// provider's role=clear command pulls the base BEFORE the clear is forwarded,
// so the fresh conversation starts on the fresh base. The clear is sacrosanct
// — it ALWAYS goes through: a working agent skips the pull (a clear must
// never block on the busy gate), a failed pull is reported loudly but not
// fatally, and a successful pull (including already-up-to-date) forwards
// silently with no digest — post-clear there is no stale context to repair.
// Only the forward itself failing is an error, and it wins over any pending
// notice.
func (s *Server) handleClearWithPull(w http.ResponseWriter, r *http.Request, run store.Run, text string) {
	notice := ""
	if view, err := s.chat.Read(r.Context(), run); err == nil && view.State == provider.StateWorking {
		notice = "Base not pulled (agent is busy); the new conversation starts on the old base."
	} else if _, err := s.pull.PullBase(r.Context(), run); err != nil {
		notice = fmt.Sprintf("Base not pulled (%s); the new conversation starts on the old base.", shortPullReason(err))
	}
	if err := s.chat.Reply(r.Context(), run, text); err != nil {
		s.writeChatActionError(w, "replying", err)
		return
	}
	if notice != "" {
		writeJSON(w, http.StatusOK, noticeResponse{Notice: notice})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writePullError maps a PullBase failure onto a status code — deliberately
// local to the pull flows; writeChatActionError keeps its existing mappings
// untouched. A conflict and the author-identity refusal are 409s (operator-
// fixable conditions, mirroring how the CR-merge surface maps gitx refusals
// and crmerge.ErrNoAuthorIdentity); everything else — fetch failures, git's
// dirty-clobber refusal — is a 502: an upstream/worktree condition rather
// than a server bug, with git's verbatim text preserved for the operator.
func (s *Server) writePullError(w http.ResponseWriter, err error) {
	var conflict *gitx.ConflictError
	switch {
	case errors.As(err, &conflict):
		msg := err.Error()
		if len(conflict.Files) > 0 {
			msg = "merge conflict — worktree unchanged; conflicting files: " + strings.Join(conflict.Files, ", ")
		}
		writeError(w, http.StatusConflict, msg)
	case errors.Is(err, pull.ErrNoAuthorIdentity):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusBadGateway, "pull failed: "+err.Error())
	}
}

// shortPullReason compresses a PullBase failure for the clear-hook notice: a
// conflict names its files, anything else keeps the error text.
func shortPullReason(err error) string {
	var conflict *gitx.ConflictError
	if errors.As(err, &conflict) && len(conflict.Files) > 0 {
		return "merge conflict in " + strings.Join(conflict.Files, ", ")
	}
	return err.Error()
}

// short12 is the notices' sha abbreviation: the first 12 characters, the same
// width the pull digest itself uses.
func short12(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

type answerRequest struct {
	ToolID    string `json:"tool_id"`
	Index     int    `json:"index"`
	Selected  []int  `json:"selected"`
	OtherText string `json:"other_text"`
	// Answers carries one entry per question of a MULTI-question dialog (issue
	// #51 decision 3), all collected in one submit. Both the flat fields and
	// Answers are threaded through to the provider unchanged — when Answers is
	// non-empty it wins, but the seam (not this layer) decides that, so the
	// flat shape keeps working for single-question dialogs with zero churn.
	Answers []answerQuestion `json:"answers"`
}

// answerQuestion is one question's answer within a multi-question submit: the
// single-select Index into that question's options, the multi-select Selected
// toggles, and OtherText when the chosen row is the free-text "Other".
type answerQuestion struct {
	Index     int    `json:"index"`
	Selected  []int  `json:"selected"`
	OtherText string `json:"other_text"`
}

// handleRunAnswer is POST /api/v1/runs/{id}/answer — answer the pending dialog.
// tool_id is required: the service re-reads the live dialog under the session
// lock and refuses a mismatch, so a stale client never answers a dialog that
// already moved on. The flat fields answer a single-question dialog; answers[]
// answers a multi-question form (issue #51 decision 3) — both are passed to the
// provider, which resolves precedence.
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
	if len(req.Answers) > 0 {
		answer.Answers = make([]provider.QuestionAnswer, len(req.Answers))
		for i, a := range req.Answers {
			answer.Answers[i] = provider.QuestionAnswer{Index: a.Index, Selected: a.Selected, OtherText: a.OtherText}
		}
	}
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
// The interaction sentinels are provider-generic (issue #51 decision 7) —
// this file never imports a concrete provider.
func (s *Server) writeChatActionError(w http.ResponseWriter, doing string, err error) {
	switch {
	case errors.Is(err, chat.ErrRunEnded), errors.Is(err, chat.ErrDialogPending),
		errors.Is(err, chat.ErrNoDialog), errors.Is(err, chat.ErrDialogChanged),
		errors.Is(err, provider.ErrDialogNotAnswerable):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, tmuxx.ErrSessionNotFound):
		// The run row says active but the tmux session is gone (killed
		// externally; the reaper hasn't flipped the outcome yet).
		writeError(w, http.StatusConflict, "the instance's session is gone; it will be marked ended shortly")
	case errors.Is(err, provider.ErrInvalidReply):
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
