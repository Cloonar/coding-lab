package agentapi

// The repo-secrets agent surface (issue #104): the two run-token-scoped
// endpoints behind `labctl secret list` and `labctl secret exec`. Repo scope
// comes STRICTLY from the authenticated run's row (via s.runRepo), never a
// URL — a run can only ever reach its own repo's secrets, so a name that
// belongs to another repo simply does not exist here (a 404, never a leak).
//
// The list endpoint returns names + descriptions ONLY: no ids, no timestamps,
// and never a value — the store's metadata reads never even select the
// encrypted column. The values endpoint decrypts per request (rotation is
// live: nothing is cached) and returns plaintext keyed by name; a decrypt
// failure is an opaque 500 whose log line carries the name and repo only,
// never the blob or the plaintext.

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// secretMeta is one row of GET /agent/v1/secrets — name and description only,
// deliberately no id/timestamps/value (the store's metadata reads never carry
// the blob, and this shape never adds one back).
type secretMeta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// handleSecretList is GET /agent/v1/secrets: the run's repo's secrets as
// metadata (name + description), ordered by name (the store's order). Never a
// value.
func (s *Server) handleSecretList(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	secrets, err := s.store.RepoSecrets(r.Context(), repo.ID)
	if err != nil {
		s.internalError(w, "listing repo secrets", err)
		return
	}
	items := make([]secretMeta, 0, len(secrets))
	for _, sec := range secrets {
		items = append(items, secretMeta{Name: sec.Name, Description: sec.Description})
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": items})
}

type secretValuesRequest struct {
	Names []string `json:"names"`
}

// handleSecretValues is POST /agent/v1/secrets/values {"names":[…]} → 200
// {"values":{name:plaintext,…}}. It is the exec-time fetch behind `labctl
// secret exec`: values are decrypted fresh every call (live rotation, no
// cache). Rules: empty/missing names → 400; ANY requested name absent from
// this repo → 404 naming the missing (sorted) and returning NO values (a
// partial miss yields nothing, so a caller never half-injects an env); a
// decrypt failure → opaque 500 logged with the name + repo only.
func (s *Server) handleSecretValues(w http.ResponseWriter, r *http.Request) {
	_, repo, ok := s.runRepo(w, r)
	if !ok {
		return
	}
	var req secretValuesRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Names) == 0 {
		jsonError(w, http.StatusBadRequest, "names is required")
		return
	}

	blobs, err := s.store.RepoSecretValues(r.Context(), repo.ID, req.Names)
	if err != nil {
		s.internalError(w, "loading repo secret values", err)
		return
	}

	// A miss on any requested name is a 404 naming exactly the absent ones
	// (sorted, deduped) — a cross-repo name is simply absent from this scope,
	// so scope violations fall through this same "unknown" door with no values.
	var missing []string
	seen := make(map[string]bool, len(req.Names))
	for _, name := range req.Names {
		if _, ok := blobs[name]; ok || seen[name] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		jsonError(w, http.StatusNotFound, "unknown secret(s): "+strings.Join(missing, ", "))
		return
	}

	values := make(map[string]string, len(blobs))
	for name, blob := range blobs {
		plain, err := s.vault.Decrypt(blob)
		if err != nil {
			// Never echo the blob or plaintext — name + repo only (design §12).
			s.internalError(w, "decrypting repo secret", fmt.Errorf("secret %q repo %q: %w", name, repo.ID, err))
			return
		}
		values[name] = string(plain)
	}
	writeJSON(w, http.StatusOK, map[string]any{"values": values})
}
