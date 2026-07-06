// Package httpapi is lab's operator API (/api/v1) plus the cross-cutting
// HTTP middleware chain (design §5): recover → request-id → logging →
// metrics → auth → CSRF → handler. It also mounts /healthz, /readyz and
// /metrics (outside auth+CSRF — probes must work with the DB down), the
// SSE endpoint, the agent API (which brings its own run-token auth and no
// CSRF), and the SPA.
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/metrics"
	"git.cloonar.com/Cloonar/coding-lab/internal/reposvc"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
	"git.cloonar.com/Cloonar/coding-lab/internal/webui"
)

// defaultHeartbeat is the SSE heartbeat cadence (design §5).
const defaultHeartbeat = 25 * time.Second

// Options configures a Server. Store and Bus are required; everything else
// has sane defaults.
type Options struct {
	Store  *store.Store
	Bus    *events.Bus
	Logger *slog.Logger

	Metrics *metrics.Metrics // default: a fresh registry

	// Vault seals credential payloads (M2). Nil leaves the credentials
	// routes unmounted (they 404) — some test servers run without one.
	Vault *vault.Vault
	// Repos is the repository lifecycle service (M2). Nil leaves the repo
	// routes unmounted.
	Repos *reposvc.Service

	// BaseURL is --base-url; its origin anchors CSRF Origin checks and the
	// Secure-cookie decision. Empty means "derive from the request".
	BaseURL string

	ProxyAuth       bool
	ProxyAuthHeader string
	TrustedProxies  []netip.Prefix

	// AgentHandler is the /agent/v1 tree (run-token auth, no CSRF). Nil
	// leaves it unmounted.
	AgentHandler http.Handler
	// UIHandler serves non-API paths; defaults to webui.Handler().
	UIHandler http.Handler

	// HeartbeatInterval overrides the SSE heartbeat (tests); 0 → 25s.
	HeartbeatInterval time.Duration
	// Now overrides the clock (tests); nil → time.Now.
	Now func() time.Time
}

// Server carries the operator API's dependencies and configuration.
type Server struct {
	store   *store.Store
	bus     *events.Bus
	log     *slog.Logger
	metrics *metrics.Metrics
	vault   *vault.Vault
	repos   *reposvc.Service

	baseOrigin      string // canonical origin of --base-url, "" when unset
	baseOriginHTTPS bool

	proxyAuth   bool
	proxyHeader string
	trusted     []netip.Prefix

	agent http.Handler
	ui    http.Handler

	heartbeat time.Duration
	now       func() time.Time

	limiter  *loginLimiter
	argon    argonParams
	argonSem chan struct{} // global argon2 concurrency cap (memory bound)

	// shutdownCtx is the server-scoped context long-lived handlers (SSE)
	// select on; CloseStreams cancels it so graceful shutdown can drain.
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc

	dummyOnce sync.Once
	dummyPHC  string

	setupMu sync.Mutex

	proxyLoggedMu sync.Mutex
	proxyLogged   map[string]struct{}
}

// New validates o and builds a Server.
func New(o Options) (*Server, error) {
	if o.Store == nil {
		return nil, fmt.Errorf("httpapi: Options.Store is required")
	}
	if o.Bus == nil {
		return nil, fmt.Errorf("httpapi: Options.Bus is required")
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	m := o.Metrics
	if m == nil {
		m = metrics.New()
	}
	ui := o.UIHandler
	if ui == nil {
		ui = webui.Handler()
	}
	heartbeat := o.HeartbeatInterval
	if heartbeat <= 0 {
		heartbeat = defaultHeartbeat
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}

	s := &Server{
		store:       o.Store,
		bus:         o.Bus,
		log:         logger,
		metrics:     m,
		vault:       o.Vault,
		repos:       o.Repos,
		proxyAuth:   o.ProxyAuth,
		proxyHeader: o.ProxyAuthHeader,
		trusted:     o.TrustedProxies,
		agent:       o.AgentHandler,
		ui:          ui,
		heartbeat:   heartbeat,
		now:         now,
		limiter:     newLoginLimiter(loginLimiterEntries),
		argon:       defaultArgonParams,
		argonSem:    make(chan struct{}, argonConcurrency),
		proxyLogged: make(map[string]struct{}),
	}
	s.shutdownCtx, s.shutdownCancel = context.WithCancel(context.Background())

	if o.BaseURL != "" {
		u, err := url.Parse(o.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("httpapi: parse base url: %w", err)
		}
		origin, ok := canonicalOrigin(o.BaseURL)
		if !ok {
			return nil, fmt.Errorf("httpapi: base url %q: want an absolute http(s) URL", o.BaseURL)
		}
		s.baseOrigin = origin
		s.baseOriginHTTPS = u.Scheme == "https"
	}

	// Startup warning (design §5): if this configuration can never produce
	// a Secure cookie — no in-process TLS, base URL not https, and no
	// trusted proxy that could assert X-Forwarded-Proto — say so loudly.
	if !s.baseOriginHTTPS && len(s.trusted) == 0 {
		s.log.Warn("session cookies will never carry the Secure flag with this configuration; "+
			"set --base-url to an https URL or terminate TLS at a proxy listed in --trusted-proxies",
			"component", "httpapi", "base_url", o.BaseURL)
	}

	return s, nil
}

// Handler assembles the full middleware chain and route table.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()

	api.HandleFunc("POST /api/v1/auth/setup", s.handleSetup)
	api.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	api.HandleFunc("POST /api/v1/auth/logout", s.requireAuth(s.handleLogout))
	api.HandleFunc("GET /api/v1/auth/state", s.handleAuthState)
	api.HandleFunc("GET /api/v1/me", s.requireAuth(s.handleMe))
	api.HandleFunc("GET /api/v1/events", s.requireAuth(s.handleEvents))

	// M2 surfaces (operator auth; CSRF guards the mutations). Mounted only
	// when their dependencies were provided.
	if s.vault != nil {
		api.HandleFunc("POST /api/v1/credentials", s.requireAuth(s.handleCredentialCreate))
		api.HandleFunc("GET /api/v1/credentials", s.requireAuth(s.handleCredentialList))
		api.HandleFunc("PATCH /api/v1/credentials/{id}", s.requireAuth(s.handleCredentialUpdate))
		api.HandleFunc("DELETE /api/v1/credentials/{id}", s.requireAuth(s.handleCredentialDelete))
	}
	if s.repos != nil {
		api.HandleFunc("POST /api/v1/repos", s.requireAuth(s.handleRepoCreate))
		api.HandleFunc("GET /api/v1/repos", s.requireAuth(s.handleRepoList))
		api.HandleFunc("GET /api/v1/repos/{id}", s.requireAuth(s.handleRepoGet))
		api.HandleFunc("PATCH /api/v1/repos/{id}", s.requireAuth(s.handleRepoUpdate))
		api.HandleFunc("DELETE /api/v1/repos/{id}", s.requireAuth(s.handleRepoDelete))
		api.HandleFunc("POST /api/v1/repos/{id}/clone/retry", s.requireAuth(s.handleRepoCloneRetry))
	}

	// Unknown API paths get JSON 404s, not the SPA shell.
	api.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	api.Handle("/", s.ui)

	// Operator surface: auth resolution then CSRF around the whole tree,
	// with the pattern recorder innermost so the metrics middleware sees
	// the api mux's route even though authMiddleware clones the request.
	operator := metrics.RecordPattern(api)
	operator = s.csrfMiddleware(operator)
	operator = s.authMiddleware(operator)

	root := http.NewServeMux()
	if s.agent != nil {
		// The agent API mounts its own run-token auth and no CSRF at all
		// (design §5), so it bypasses the operator middleware entirely.
		root.Handle("/agent/v1/", s.agent)
	}
	// Probe and scrape endpoints live OUTSIDE auth+CSRF: liveness must
	// answer 200 even when the DB (and thus identity resolution) is down.
	// They still sit inside the recover/request-id/logging/metrics chain.
	root.HandleFunc("GET /healthz", s.handleHealthz)
	root.HandleFunc("GET /readyz", s.handleReadyz)
	root.Handle("GET /metrics", s.metrics.Handler())
	root.Handle("/", operator)

	var h http.Handler = root
	h = s.metrics.Middleware(h)
	h = s.loggingMiddleware(h)
	h = s.requestIDMiddleware(h)
	h = s.recoverMiddleware(h)
	return h
}

// CloseStreams cancels the server-scoped context that long-lived handlers
// (SSE) select on. Wire it via http.Server.RegisterOnShutdown so graceful
// shutdown drains event streams promptly instead of timing out on them.
func (s *Server) CloseStreams() { s.shutdownCancel() }
