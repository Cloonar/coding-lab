package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// maxJSONBody bounds request bodies on JSON endpoints.
const maxJSONBody = 1 << 20 // 1 MiB

// writeJSON writes v as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the canonical error shape {"error":"<message>"}.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// decodeJSON reads the request body into dst. On failure it writes a 400 and
// returns the error (the caller just returns).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return fmt.Errorf("decode json body: %w", err)
	}
	return nil
}
