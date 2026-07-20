// Package agentapi is lab's agent-facing API (/agent/v1). Authentication is
// the run token only — no cookies, no CSRF (design §5). Every request is
// scoped to the authenticated run's repo: the repo id comes from the run row
// the token joins to, never from the URL, so a run can only ever reach its
// own repo's tracker surface (brief §8.2, D10).
package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// runTokenPrefix is the wire prefix of run tokens (ids.NewToken("run")).
const runTokenPrefix = "lab_run_"

// TrackerResolver resolves a repo-scoped Tracker — the seam the handlers go
// through so tests can inject fakes. *tracker.Registry satisfies it.
type TrackerResolver interface {
	TrackerFor(ctx context.Context, repo store.Repo) (tracker.Tracker, error)
}

// Server carries the agent API's dependencies.
type Server struct {
	store    *store.Store
	vault    *vault.Vault
	trackers TrackerResolver
	bus      *events.Bus
	log      *slog.Logger
	now      func() time.Time
	scrub    []*regexp.Regexp
}

// New builds an agent API server. vlt decrypts repo-secret blobs at exec time
// for the POST /secrets/values endpoint (the store only ever sees ciphertext;
// agentapi holds the only decrypt key on the agent surface). trackers resolves
// each repo's Tracker (production: *tracker.Registry); bus carries the
// cr.changed event a builtin PR create publishes (nil → no events, some unit
// tests run without a bus); now is the injected clock for the token validity
// rule (nil → time.Now).
// scrub is the compiled cross-provider union of every registered provider's
// declared attribution ScrubPatterns — the per-line predicate the incogni
// body sanitizer runs (production: providerReg.ScrubRegexps(), ADR-0033).
// A nil/empty scrub means no markers are known (no registered providers), so
// incogni bodies pass through unstripped — the same content-inert degradation
// as the pre-push hook on an empty registry. agentapi takes plain compiled
// regexps and never imports internal/provider, staying provider-agnostic.
func New(st *store.Store, vlt *vault.Vault, trackers TrackerResolver, bus *events.Bus, logger *slog.Logger, now func() time.Time, scrub []*regexp.Regexp) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	return &Server{store: st, vault: vlt, trackers: trackers, bus: bus, log: logger, now: now, scrub: scrub}
}

// Handler returns the /agent/v1 tree wrapped in run-token auth.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Brief §8.2 routes plus GET /agent/v1/issues (design §5 — the gap the
	// brief itself creates via `labctl issue list`), extended by the ADR-0014
	// triage surface: issue create, edit, label ops, close.
	mux.HandleFunc("GET /agent/v1/issue", s.handleClaimedIssue)
	mux.HandleFunc("GET /agent/v1/issues", s.handleIssueList)
	mux.HandleFunc("POST /agent/v1/issues", s.handleIssueCreate)
	mux.HandleFunc("GET /agent/v1/issues/{n}", s.handleIssueGet)
	mux.HandleFunc("PATCH /agent/v1/issues/{n}", s.handleIssueEdit)
	mux.HandleFunc("POST /agent/v1/issues/{n}/comments", s.handleCommentCreate)
	mux.HandleFunc("POST /agent/v1/issues/{n}/labels", s.handleIssueLabelAdd)
	mux.HandleFunc("DELETE /agent/v1/issues/{n}/labels", s.handleIssueLabelRemove)
	mux.HandleFunc("POST /agent/v1/issues/{n}/close", s.handleIssueClose)
	mux.HandleFunc("GET /agent/v1/labels", s.handleLabelList)
	mux.HandleFunc("POST /agent/v1/labels", s.handleLabelEnsure)
	mux.HandleFunc("POST /agent/v1/prs", s.handlePRCreate)
	mux.HandleFunc("GET /agent/v1/prs", s.handlePRList)
	mux.HandleFunc("GET /agent/v1/prs/{n}", s.handlePRGet)
	mux.HandleFunc("GET /agent/v1/prs/{n}/checks", s.handlePRChecks)
	mux.HandleFunc("POST /agent/v1/prs/{n}/merge", s.handlePRMerge)
	mux.HandleFunc("POST /agent/v1/prs/{n}/reject", s.handlePRReject)
	mux.HandleFunc("POST /agent/v1/prs/{n}/approve", s.handlePRApprove)
	mux.HandleFunc("POST /agent/v1/prs/{n}/rerequest", s.handlePRRerequest)
	mux.HandleFunc("POST /agent/v1/prs/{n}/escalate", s.handlePREscalate)
	mux.HandleFunc("POST /agent/v1/prs/{n}/comments", s.handlePRComment)
	mux.HandleFunc("GET /agent/v1/secrets", s.handleSecretList)
	mux.HandleFunc("POST /agent/v1/secrets/values", s.handleSecretValues)
	mux.HandleFunc("POST /agent/v1/secrets/scan", s.handleSecretScan)
	mux.HandleFunc("/agent/v1/", func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, http.StatusNotFound, "not found")
	})

	return s.AuthMiddleware(mux)
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
