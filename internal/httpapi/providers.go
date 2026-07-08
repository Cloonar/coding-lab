package httpapi

// Provider catalog + Claude auth/login endpoints (pinned M3 contract):
//   GET  /api/v1/providers
//   GET  /api/v1/providers/claude/auth/status[?force=1]
//   POST /api/v1/providers/claude/auth/login/start
//   POST /api/v1/providers/claude/auth/login/code {code}

import (
	"errors"
	"net/http"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/claudecode"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

type providerResponse struct {
	ID      string            `json:"id"`
	Models  []provider.Option `json:"models"`
	Efforts []provider.Option `json:"efforts"`
	// Options is the provider's declared spawn-options schema (issue #19 /
	// ADR-0021) — always present (may be []), driving the schema-rendered "AFK
	// defaults" section in the settings UIs.
	Options []provider.OptionSpec `json:"options"`
	// FallbackOpen is the provider's generic web open affordance (URL + human
	// title, ADR-0017), present only when the provider implements DeepLinker
	// (has a web surface); absent for link-less providers, whose instance rows
	// render a copyable tmux-attach affordance instead. Additive to the pinned
	// M3 providers shape.
	FallbackOpen *provider.OpenAffordance `json:"fallback_open,omitempty"`
}

// handleProvidersList is GET /api/v1/providers — every provider's id, its
// model/effort catalogs (provider-owned data, D14), and its fallback-open
// metadata when it has a web surface (ADR-0017), in registration order.
func (s *Server) handleProvidersList(w http.ResponseWriter, r *http.Request) {
	provs := s.providers.List()
	items := make([]providerResponse, 0, len(provs))
	for _, p := range provs {
		resp := providerResponse{ID: p.ID(), Models: p.Models(), Efforts: p.Efforts(), Options: p.SpawnOptions()}
		if resp.Options == nil {
			resp.Options = []provider.OptionSpec{}
		}
		if dl, ok := p.(provider.DeepLinker); ok {
			fo := dl.FallbackOpen()
			resp.FallbackOpen = &fo
		}
		items = append(items, resp)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": items})
}

// claudeProvider resolves the claude-code provider that the /providers/claude
// auth routes act on (the URL segment is "claude"; the registered id is
// claude-code). Missing → the routes 404.
func (s *Server) claudeProvider(w http.ResponseWriter) (provider.AgentProvider, bool) {
	p, ok := s.providers.Get(claudecode.ID)
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return nil, false
	}
	return p, true
}

type authStatusResponse struct {
	LoggedIn  bool   `json:"logged_in"`
	Email     string `json:"email"`
	Method    string `json:"method"`
	CheckedAt string `json:"checked_at"`
}

// handleClaudeAuthStatus is GET /api/v1/providers/claude/auth/status[?force=1].
// An error is treated as logged-out — the status is returned alongside it and
// is already cached (v0 semantics).
func (s *Server) handleClaudeAuthStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := s.claudeProvider(w)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	st, _ := p.AuthStatus(r.Context(), force)
	writeJSON(w, http.StatusOK, authStatusResponse{
		LoggedIn:  st.LoggedIn,
		Email:     st.Email,
		Method:    st.Method,
		CheckedAt: store.FormatTime(st.CheckedAt),
	})
}

// handleClaudeLoginStart is POST /api/v1/providers/claude/auth/login/start:
// 200 {oauth_url} (the URL may be "" on a scrape miss — retry to re-scrape), or
// 409 when already logged in.
func (s *Server) handleClaudeLoginStart(w http.ResponseWriter, r *http.Request) {
	p, ok := s.claudeProvider(w)
	if !ok {
		return
	}
	if st, _ := p.AuthStatus(r.Context(), true); st.LoggedIn {
		writeError(w, http.StatusConflict, "already logged in")
		return
	}
	url, err := p.LoginStart(r.Context())
	if err != nil {
		s.internalError(w, "starting claude login", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"oauth_url": url})
}

type loginCodeRequest struct {
	Code string `json:"code"`
}

// handleClaudeLoginCode is POST /api/v1/providers/claude/auth/login/code
// {code}: 202 on success, 400 for a rejected code, 504 on login timeout.
func (s *Server) handleClaudeLoginCode(w http.ResponseWriter, r *http.Request) {
	p, ok := s.claudeProvider(w)
	if !ok {
		return
	}
	var req loginCodeRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	err := p.LoginSubmitCode(r.Context(), req.Code)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, claudecode.ErrInvalidCode):
		writeError(w, http.StatusBadRequest, "paste the code from the authorize page")
	case errors.Is(err, claudecode.ErrLoginTimeout):
		writeError(w, http.StatusGatewayTimeout, claudecode.ErrLoginTimeout.Error())
	default:
		s.internalError(w, "submitting claude login code", err)
	}
}

// handleClaudeLogout is POST /api/v1/providers/claude/auth/logout: drop the
// machine's current Claude account so a fresh one can be logged in (issue #46).
// A machine-wide auth cut behind the SPA's confirm gate — bare by decision, it
// does not touch running instances. The provider's Logout force-refreshes
// status, so a 200 reflects the now-logged-out cache; on success this endpoint
// (mirroring /login/code's post-login publish) emits claude.auth.changed so the
// SPA flips live. A logout that could not drop the account is the canonical
// opaque 500 (the tool's stderr is logged server-side, not returned).
func (s *Server) handleClaudeLogout(w http.ResponseWriter, r *http.Request) {
	p, ok := s.claudeProvider(w)
	if !ok {
		return
	}
	if err := p.Logout(r.Context()); err != nil {
		s.internalError(w, "logging out of claude", err)
		return
	}
	s.bus.Publish(events.Event{
		Type:    claudecode.EventAuthChanged,
		Payload: map[string]string{"type": claudecode.EventAuthChanged},
	})
	// The provider's Logout left the status cache fresh (logged-out); echo it so
	// the client can update without waiting for the SSE round-trip.
	st, _ := p.AuthStatus(r.Context(), false)
	writeJSON(w, http.StatusOK, authStatusResponse{
		LoggedIn:  st.LoggedIn,
		Email:     st.Email,
		Method:    st.Method,
		CheckedAt: store.FormatTime(st.CheckedAt),
	})
}
