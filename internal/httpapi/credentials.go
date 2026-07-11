package httpapi

// Credentials CRUD (pinned M2 contract; design §3a/§6/§12). Payloads are
// write-only: they are validated (vault.ValidateKindPayload — messages are
// safe to echo), sealed, and stored; no response ever carries payload bytes
// back. Delete is refused with 409 while any repo references the credential
// via either FK column.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// ValidateNewCredentials enforces the username/password shape for a new
// operator account. It is shared by the first-run setup endpoint and startup
// seeding of the initial operator user (issue #134) so the two paths can
// never drift apart on what counts as a valid username or password.
func ValidateNewCredentials(username, password string) error {
	if len(username) < 1 || len(username) > 64 {
		return errors.New("username must be 1-64 characters")
	}
	if len(password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	return nil
}

// validateForgeHostShape enforces the flavor-aware host SHAPE of a forge_token
// payload at create/rotate time (ADR-0015): https-only, no userinfo/query, and
// a path allowed only for the github flavor. It reuses tracker.NormalizeForgeHost
// — the very routine the registry builds the BaseURL with — so a host that
// passes create can never be rejected at resolve time. Non-forge_token kinds
// pass through; the caller has already run vault.ValidateKindPayload, so the
// bytes decode cleanly and the flavor enum is already vetted.
func validateForgeHostShape(kind string, payload []byte) error {
	if kind != vault.KindForgeToken {
		return nil
	}
	var p vault.ForgeTokenPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil // shape already vetted by ValidateKindPayload; nothing to add
	}
	_, err := tracker.NormalizeForgeHost(p.ForgeFlavor(), p.Host)
	return err
}

// credentialJSON is the metadata shape create/PATCH answer with — never the
// payload (design §12: no secret readback).
func credentialJSON(c store.Credential) map[string]any {
	return map[string]any{
		"id":         c.ID,
		"name":       c.Name,
		"kind":       c.Kind,
		"created_at": store.FormatTime(c.CreatedAt),
		"updated_at": store.FormatTime(c.UpdatedAt),
	}
}

type credentialCreateRequest struct {
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// handleCredentialCreate is POST /api/v1/credentials.
func (s *Server) handleCredentialCreate(w http.ResponseWriter, r *http.Request) {
	var req credentialCreateRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := vault.ValidateKindPayload(req.Kind, req.Payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateForgeHostShape(req.Kind, req.Payload); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sealed, err := s.vault.EncryptPayload(req.Payload)
	if err != nil {
		s.internalError(w, "encrypting credential payload", err)
		return
	}
	c, err := s.store.CreateCredential(r.Context(), ids.NewID("cred"), name, req.Kind, sealed, s.now())
	if err != nil {
		if errors.Is(err, store.ErrNameTaken) {
			writeError(w, http.StatusConflict, store.ErrNameTaken.Error())
			return
		}
		s.internalError(w, "creating credential", err)
		return
	}
	writeJSON(w, http.StatusCreated, credentialJSON(c))
}

// handleCredentialList is GET /api/v1/credentials.
func (s *Server) handleCredentialList(w http.ResponseWriter, r *http.Request) {
	metas, err := s.store.Credentials(r.Context())
	if err != nil {
		s.internalError(w, "listing credentials", err)
		return
	}
	items := make([]map[string]any, 0, len(metas))
	for _, m := range metas {
		items = append(items, map[string]any{
			"id":         m.ID,
			"name":       m.Name,
			"kind":       m.Kind,
			"created_at": store.FormatTime(m.CreatedAt),
			"updated_at": store.FormatTime(m.UpdatedAt),
			"referenced": m.Referenced > 0,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": items})
}

type credentialPatchRequest struct {
	Name    *string         `json:"name"`
	Payload json.RawMessage `json:"payload"`
}

// handleCredentialUpdate is PATCH /api/v1/credentials/{id}: rename and/or
// rotate the payload (validated against the credential's immutable kind).
// The rotated payload is sealed and stored, never read back.
func (s *Server) handleCredentialUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req credentialPatchRequest
	if decodeJSON(w, r, &req) != nil {
		return
	}
	if req.Name == nil && req.Payload == nil {
		// An empty PATCH would still stamp updated_at, which reads as a
		// rotation that never happened — reject instead.
		writeError(w, http.StatusBadRequest, "nothing to update: provide name and/or payload")
		return
	}

	var namePtr *string
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, http.StatusBadRequest, "name must not be empty")
			return
		}
		namePtr = &name
	}

	var sealed []byte
	if req.Payload != nil {
		cred, err := s.store.CredentialByID(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not found")
				return
			}
			s.internalError(w, "loading credential", err)
			return
		}
		if err := vault.ValidateKindPayload(cred.Kind, req.Payload); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := validateForgeHostShape(cred.Kind, req.Payload); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if sealed, err = s.vault.EncryptPayload(req.Payload); err != nil {
			s.internalError(w, "encrypting credential payload", err)
			return
		}
	}

	if err := s.store.UpdateCredential(r.Context(), id, namePtr, sealed, s.now()); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not found")
		case errors.Is(err, store.ErrNameTaken):
			writeError(w, http.StatusConflict, store.ErrNameTaken.Error())
		default:
			s.internalError(w, "updating credential", err)
		}
		return
	}

	c, err := s.store.CredentialByID(r.Context(), id)
	if err != nil {
		s.internalError(w, "loading credential", err)
		return
	}
	writeJSON(w, http.StatusOK, credentialJSON(c))
}

// handleCredentialDelete is DELETE /api/v1/credentials/{id}: 204, or 409
// with the pinned "credential is referenced by N repositor(y|ies)" message
// while any repo references it.
func (s *Server) handleCredentialDelete(w http.ResponseWriter, r *http.Request) {
	err := s.store.DeleteCredential(r.Context(), r.PathValue("id"))
	if err != nil {
		var ref *store.ReferencedError
		switch {
		case errors.As(err, &ref):
			writeError(w, http.StatusConflict, ref.Error())
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not found")
		default:
			s.internalError(w, "deleting credential", err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
