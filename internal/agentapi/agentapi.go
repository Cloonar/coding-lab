// Package agentapi is lab's agent-facing API (/agent/v1). Authentication is
// the run token only — no cookies, no CSRF (design §5). M1 ships the auth
// middleware and a stub router; the real handlers land with the M5 engine.
package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// runTokenPrefix is the wire prefix of run tokens (ids.NewToken("run")).
const runTokenPrefix = "lab_run_"

// Server carries the agent API's dependencies.
type Server struct {
	store *store.Store
	log   *slog.Logger
	now   func() time.Time
}

// New builds an agent API server. now is the injected clock for the token
// validity rule (nil → time.Now).
func New(st *store.Store, logger *slog.Logger, now func() time.Time) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Server{store: st, log: logger, now: now}
}

// Handler returns the /agent/v1 tree wrapped in run-token auth.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Brief §8.2 routes plus GET /agent/v1/issues (design §5). All are M5
	// stubs behind real auth so the surface and its security are testable
	// now and labctl's transport has a stable contract to land against.
	stub := s.handleNotImplemented
	mux.HandleFunc("GET /agent/v1/issue", stub)
	mux.HandleFunc("GET /agent/v1/issues", stub)
	mux.HandleFunc("GET /agent/v1/issues/{n}", stub)
	mux.HandleFunc("POST /agent/v1/issues/{n}/comments", stub)
	mux.HandleFunc("POST /agent/v1/prs", stub)
	mux.HandleFunc("/agent/v1/", func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, http.StatusNotFound, "not found")
	})

	return s.AuthMiddleware(mux)
}

func (s *Server) handleNotImplemented(w http.ResponseWriter, _ *http.Request) {
	jsonError(w, http.StatusNotImplemented, "agent API lands in M5")
}

type runCtxKey struct{}

// RunFromContext returns the authenticated run for a request that passed
// AuthMiddleware.
func RunFromContext(ctx context.Context) (store.RunTokenInfo, bool) {
	info, ok := ctx.Value(runCtxKey{}).(store.RunTokenInfo)
	return info, ok
}

// AuthMiddleware enforces the §3a run-token validity rule: the Bearer token
// must hash to a stored run token whose joined run has outcome='active' and
// which is unexpired (expires_at IS NULL OR expires_at > now). Everything
// else — missing header, wrong prefix, unknown token, terminal run, expiry —
// is the same opaque 401.
func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			jsonError(w, http.StatusUnauthorized, "run token required")
			return
		}
		token = strings.TrimSpace(token)
		if !strings.HasPrefix(token, runTokenPrefix) {
			jsonError(w, http.StatusUnauthorized, "run token required")
			return
		}

		info, err := s.store.RunTokenByHash(r.Context(), ids.HashToken(token))
		if errors.Is(err, store.ErrNotFound) {
			jsonError(w, http.StatusUnauthorized, "invalid run token")
			return
		}
		if err != nil {
			s.log.Error("looking up run token", "component", "agentapi", "err", err)
			jsonError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if info.Outcome != "active" {
			jsonError(w, http.StatusUnauthorized, "invalid run token")
			return
		}
		if info.ExpiresAt != nil && !info.ExpiresAt.After(s.now()) {
			jsonError(w, http.StatusUnauthorized, "invalid run token")
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), runCtxKey{}, info)))
	})
}

// jsonError is the canonical error shape; local copy to keep agentapi free
// of an httpapi dependency.
func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
