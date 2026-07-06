package httpapi

// PAT CRUD (D7, pinned M5 contract). The secret is shown exactly ONCE in the
// create response; thereafter only id/name/timestamps are readable — the DB
// holds the sha256 hex, never the token. Deletion revokes immediately: the
// Bearer auth path resolves api_tokens by hash on every request.

import (
	"errors"
	"net/http"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// maxTokenNameLen bounds PAT names (a display label, not a payload).
const maxTokenNameLen = 128

// tokenResponse is one GET /api/v1/tokens row. The secret never appears here.
type tokenResponse struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
}

func tokenJSON(t store.APIToken) tokenResponse {
	resp := tokenResponse{
		ID:        t.ID,
		Name:      t.Name,
		CreatedAt: store.FormatTime(t.CreatedAt),
	}
	if t.LastUsedAt != nil {
		u := store.FormatTime(*t.LastUsedAt)
		resp.LastUsedAt = &u
	}
	return resp
}

// handleTokenList is GET /api/v1/tokens: the caller's PATs, newest first.
func (s *Server) handleTokenList(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r.Context())
	toks, err := s.store.APITokensByUser(r.Context(), id.user.ID)
	if err != nil {
		s.internalError(w, "listing api tokens", err)
		return
	}
	items := make([]tokenResponse, 0, len(toks))
	for _, t := range toks {
		items = append(items, tokenJSON(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": items})
}

type tokenCreateRequest struct {
	Name string `json:"name"`
}

// handleTokenCreate is POST /api/v1/tokens {name}: mint a `lab_pat_…` token
// and answer 201 with it — the ONLY time the secret is ever readable.
func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var req tokenCreateRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(name) > maxTokenNameLen {
		writeError(w, http.StatusBadRequest, "name is too long")
		return
	}
	id := identityFrom(r.Context())
	token, hash := ids.NewToken("pat")
	tok, err := s.store.CreateAPIToken(r.Context(), id.user.ID, name, hash)
	if err != nil {
		s.internalError(w, "creating api token", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         tok.ID,
		"name":       tok.Name,
		"token":      token,
		"created_at": store.FormatTime(tok.CreatedAt),
	})
}

// handleTokenDelete is DELETE /api/v1/tokens/{id}: 204; 404 when the id names
// none of the caller's tokens. Revocation is immediate — the next Bearer use
// of the deleted PAT resolves no identity and 401s.
func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r.Context())
	if err := s.store.DeleteAPIToken(r.Context(), id.user.ID, r.PathValue("id")); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.internalError(w, "deleting api token", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
