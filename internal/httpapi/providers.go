package httpapi

// Provider catalog + Claude auth/login endpoints (pinned M3 contract):
//   GET  /api/v1/providers
//   GET  /api/v1/providers/claude/auth/status[?force=1]
//   POST /api/v1/providers/claude/auth/login/start
//   POST /api/v1/providers/claude/auth/login/code {code}

import (
	"errors"
	"net/http"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/claudecode"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

type providerResponse struct {
	ID      string            `json:"id"`
	Models  []provider.Option `json:"models"`
	Efforts []provider.Option `json:"efforts"`
}

// handleProvidersList is GET /api/v1/providers — every provider's id and its
// model/effort catalogs (provider-owned data, D14), in registration order.
func (s *Server) handleProvidersList(w http.ResponseWriter, r *http.Request) {
	provs := s.providers.List()
	items := make([]providerResponse, 0, len(provs))
	for _, p := range provs {
		items = append(items, providerResponse{ID: p.ID(), Models: p.Models(), Efforts: p.Efforts()})
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
