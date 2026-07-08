package httpapi

// Provider catalog + per-provider auth/login endpoints (issue #51 decision 7 —
// the clean per-{id} generalization of the pinned M3 claude-only routes; the
// SPA ships with this backend, so the old /providers/claude/* names are gone):
//   GET  /api/v1/providers
//   GET  /api/v1/providers/{id}/auth/status[?force=1]
//   POST /api/v1/providers/{id}/auth/login/start
//   POST /api/v1/providers/{id}/auth/login/code {code}
//   POST /api/v1/providers/{id}/auth/logout
// {id} is the registered provider id (e.g. "claude-code"); an unknown id 404s.
// httpapi imports no concrete provider — the typed login errors and the auth-
// changed event are provider-generic (the provider package), so nothing here
// couples to claudecode.

import (
	"errors"
	"net/http"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

type providerResponse struct {
	ID string `json:"id"`
	// DisplayName is the provider's human name (issue #51 decision 9): the SPA
	// renders it instead of hardcoding any provider's name, so all user-facing
	// copy ("<name> is waiting for your reply", the auth card title) is data-
	// driven. Always present.
	DisplayName string            `json:"display_name"`
	Models      []provider.Option `json:"models"`
	Efforts     []provider.Option `json:"efforts"`
	// Options is the provider's declared spawn-options schema (issue #19 /
	// ADR-0021) — always present (may be []), driving the schema-rendered "AFK
	// defaults" section in the settings UIs.
	Options []provider.OptionSpec `json:"options"`
	// Auth is the provider's declared auth-flow descriptor (issue #51 decision
	// 7) — always present, driving the SPA's descriptor-rendered auth card so
	// no frontend hardcodes a provider's login mechanics (oauth-code today).
	Auth provider.AuthFlow `json:"auth"`
	// FallbackOpen is the provider's generic web open affordance (URL + human
	// title, ADR-0017), present only when the provider implements DeepLinker
	// (has a web surface); absent for link-less providers, whose instance rows
	// render a copyable tmux-attach affordance instead. Additive to the pinned
	// M3 providers shape.
	FallbackOpen *provider.OpenAffordance `json:"fallback_open,omitempty"`
}

// handleProvidersList is GET /api/v1/providers — every provider's id, human
// display name (issue #51 decision 9), model/effort catalogs (provider-owned
// data, D14), spawn-options schema (issue #19), auth-flow descriptor (issue #51
// decision 7), and fallback-open metadata when it has a web surface (ADR-0017),
// in registration order.
func (s *Server) handleProvidersList(w http.ResponseWriter, r *http.Request) {
	provs := s.providers.List()
	items := make([]providerResponse, 0, len(provs))
	for _, p := range provs {
		resp := providerResponse{
			ID:          p.ID(),
			DisplayName: p.DisplayName(),
			Models:      p.Models(),
			Efforts:     p.Efforts(),
			Options:     p.SpawnOptions(),
			Auth:        p.AuthFlow(),
		}
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

// providerFromPath resolves the provider the /providers/{id}/auth/* routes act
// on from the request path (issue #51 decision 7): {id} is the registered
// provider id ("claude-code"). An unknown id 404s. This is the clean per-{id}
// replacement for the old claude-only helper — httpapi still imports no
// concrete provider, only the registry keyed by the URL segment.
func (s *Server) providerFromPath(w http.ResponseWriter, r *http.Request) (provider.AgentProvider, bool) {
	p, ok := s.providers.Get(r.PathValue("id"))
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

// handleProviderAuthStatus is GET /api/v1/providers/{id}/auth/status[?force=1].
// An error is treated as logged-out — the status is returned alongside it and
// is already cached (v0 semantics).
func (s *Server) handleProviderAuthStatus(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providerFromPath(w, r)
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

// handleProviderLoginStart is POST /api/v1/providers/{id}/auth/login/start:
// 200 {oauth_url} (the URL may be "" on a scrape miss — retry to re-scrape), or
// 409 when already logged in.
func (s *Server) handleProviderLoginStart(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providerFromPath(w, r)
	if !ok {
		return
	}
	if st, _ := p.AuthStatus(r.Context(), true); st.LoggedIn {
		writeError(w, http.StatusConflict, "already logged in")
		return
	}
	url, err := p.LoginStart(r.Context())
	if err != nil {
		s.internalError(w, "starting login for "+p.ID(), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"oauth_url": url})
}

type loginCodeRequest struct {
	Code string `json:"code"`
}

// handleProviderLoginCode is POST /api/v1/providers/{id}/auth/login/code
// {code}: 202 on success, 400 for a rejected code, 504 on login timeout. The
// error mapping is on the provider-generic sentinels (issue #51 decision 7), so
// this file couples to no concrete provider's error types.
func (s *Server) handleProviderLoginCode(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providerFromPath(w, r)
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
	case errors.Is(err, provider.ErrInvalidCode):
		writeError(w, http.StatusBadRequest, "paste the code from the authorize page")
	case errors.Is(err, provider.ErrLoginTimeout):
		writeError(w, http.StatusGatewayTimeout, provider.ErrLoginTimeout.Error())
	default:
		s.internalError(w, "submitting login code for "+p.ID(), err)
	}
}

// handleProviderLogout is POST /api/v1/providers/{id}/auth/logout: drop the
// machine's current provider account so a fresh one can be logged in (issue
// #46). A machine-wide auth cut behind the SPA's confirm gate — bare by
// decision, it does not touch running instances. The provider's Logout
// force-refreshes status, so a 200 reflects the now-logged-out cache; on
// success this endpoint (mirroring /login/code's post-login publish) emits
// provider.auth.changed carrying the provider's OWN id so the SPA scopes its
// refetch to the right auth card and flips live. A logout that could not drop
// the account is the canonical opaque 500 (the tool's stderr is logged
// server-side, not returned).
func (s *Server) handleProviderLogout(w http.ResponseWriter, r *http.Request) {
	p, ok := s.providerFromPath(w, r)
	if !ok {
		return
	}
	if err := p.Logout(r.Context()); err != nil {
		s.internalError(w, "logging out of "+p.ID(), err)
		return
	}
	s.bus.Publish(events.Event{
		Type:    provider.EventAuthChanged,
		Payload: provider.AuthChangedPayload{Type: provider.EventAuthChanged, Provider: p.ID()},
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
