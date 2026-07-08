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

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/chat"
	"git.cloonar.com/Cloonar/coding-lab/internal/crmerge"
	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/metrics"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/reconcile"
	"git.cloonar.com/Cloonar/coding-lab/internal/reposvc"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
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

	// Instances is the manual instance lifecycle service (M3). Nil leaves the
	// instance + stop-all routes unmounted.
	Instances *instance.Service
	// Reconcile owns the parked view + unguarded discard (M3). Nil leaves the
	// parked routes unmounted.
	Reconcile *reconcile.Service

	// Chat is the embedded-chat brain (issue #7): read an instance's
	// transcript, reply/answer/interrupt, and the tailer's conversational
	// state. Nil leaves the /runs/{id}/messages + reply/answer/interrupt
	// routes unmounted.
	Chat *chat.Service
	// Providers is the agent-provider registry (M3): the providers catalog and
	// the claude auth/login endpoints. Nil leaves those routes unmounted.
	Providers *provider.Registry

	// Tracker resolves a per-repo issue/PR Tracker for the operator issue and
	// label surface (M4): the built-in store-backed tracker for builtin repos,
	// the Forgejo REST client for forge-bound ones. Nil leaves the issue +
	// label routes unmounted.
	Tracker *tracker.Registry

	// AFK is the unattended-run engine (M5): manual AFK start, the auto
	// toggle, the failure-counter reset, and the claimable-count hint on the
	// ready endpoint. Nil leaves the AFK routes unmounted (and the ready
	// endpoint falls back to the raw ready count).
	AFK *afk.Service

	// Git + ReposDir back the change-request surface (M6): the CR detail
	// diff runs against <ReposDir>/<repoID>.git. Both nil/empty leaves the
	// /crs read routes unmounted. Materializer supplies the per-op credential
	// files a credentialed merge pushes with (Vault decrypts).
	Git          *gitx.Engine
	Materializer *vault.Materializer
	ReposDir     string
	// CRMerge is the shared CR-merge/close orchestration (ADR-0011) the
	// operator merge/close routes delegate to — the same service the agent
	// surface's built-in MergePull reuses. Nil leaves the /crs merge+close
	// routes unmounted (the list/detail reads still mount on Git+ReposDir).
	CRMerge *crmerge.Service
	// GitEnv is appended to every git subprocess the CR surface runs, before
	// the per-credential env. Production leaves it nil; tests pass
	// testutil.HermeticGitEnv (the reposvc convention).
	GitEnv []string

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
	store     *store.Store
	bus       *events.Bus
	log       *slog.Logger
	metrics   *metrics.Metrics
	vault     *vault.Vault
	repos     *reposvc.Service
	instances *instance.Service
	reconcile *reconcile.Service
	chat      *chat.Service
	providers *provider.Registry
	tracker   *tracker.Registry
	afk       *afk.Service
	git       *gitx.Engine
	mat       *vault.Materializer
	reposDir  string
	gitEnv    []string
	crmerge   *crmerge.Service

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
		instances:   o.Instances,
		reconcile:   o.Reconcile,
		chat:        o.Chat,
		providers:   o.Providers,
		tracker:     o.Tracker,
		afk:         o.AFK,
		git:         o.Git,
		mat:         o.Materializer,
		reposDir:    o.ReposDir,
		gitEnv:      o.GitEnv,
		crmerge:     o.CRMerge,
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

	// M3 instance lifecycle (operator auth; CSRF guards the mutations).
	if s.instances != nil {
		api.HandleFunc("POST /api/v1/repos/{id}/instances", s.requireAuth(s.handleInstanceCreate))
		api.HandleFunc("GET /api/v1/instances", s.requireAuth(s.handleInstanceList))
		api.HandleFunc("DELETE /api/v1/instances/{session}", s.requireAuth(s.handleInstanceDelete))
		api.HandleFunc("POST /api/v1/repos/{id}/stop-all", s.requireAuth(s.handleStopAll))
		api.HandleFunc("GET /api/v1/runs", s.requireAuth(s.handleRunsList))
		api.HandleFunc("GET /api/v1/runs/{id}", s.requireAuth(s.handleRunGet))
	}

	// Embedded chat (issue #7): the transcript read + reply/answer/interrupt
	// surface (operator auth; CSRF guards the mutations). Mounted with the
	// chat brain.
	if s.chat != nil {
		api.HandleFunc("GET /api/v1/runs/{id}/messages", s.requireAuth(s.handleRunMessages))
		api.HandleFunc("POST /api/v1/runs/{id}/reply", s.requireAuth(s.handleRunReply))
		api.HandleFunc("POST /api/v1/runs/{id}/answer", s.requireAuth(s.handleRunAnswer))
		api.HandleFunc("POST /api/v1/runs/{id}/interrupt", s.requireAuth(s.handleRunInterrupt))
	}
	if s.reconcile != nil {
		api.HandleFunc("GET /api/v1/repos/{id}/parked", s.requireAuth(s.handleParkedList))
		api.HandleFunc("POST /api/v1/repos/{id}/parked/discard", s.requireAuth(s.handleParkedDiscard))
	}
	if s.providers != nil {
		api.HandleFunc("GET /api/v1/providers", s.requireAuth(s.handleProvidersList))
		api.HandleFunc("GET /api/v1/providers/claude/auth/status", s.requireAuth(s.handleClaudeAuthStatus))
		api.HandleFunc("POST /api/v1/providers/claude/auth/login/start", s.requireAuth(s.handleClaudeLoginStart))
		api.HandleFunc("POST /api/v1/providers/claude/auth/login/code", s.requireAuth(s.handleClaudeLoginCode))
	}

	// M4 tracker surface: the operator's issue and label read/mutate views
	// (operator auth; CSRF guards the mutations). Read views serve both
	// bindings via the Tracker; the mutations are built-in only and 409 on a
	// forge-bound repo.
	if s.tracker != nil {
		api.HandleFunc("GET /api/v1/repos/{id}/issues", s.requireAuth(s.handleIssueList))
		api.HandleFunc("POST /api/v1/repos/{id}/issues", s.requireAuth(s.handleIssueCreate))
		api.HandleFunc("GET /api/v1/repos/{id}/ready", s.requireAuth(s.handleReadyList))
		api.HandleFunc("GET /api/v1/repos/{id}/issues/{n}", s.requireAuth(s.handleIssueGet))
		api.HandleFunc("PATCH /api/v1/repos/{id}/issues/{n}", s.requireAuth(s.handleIssueUpdate))
		api.HandleFunc("POST /api/v1/repos/{id}/issues/{n}/comments", s.requireAuth(s.handleIssueComment))
		api.HandleFunc("PUT /api/v1/repos/{id}/issues/{n}/labels", s.requireAuth(s.handleIssueLabels))
		api.HandleFunc("GET /api/v1/repos/{id}/labels", s.requireAuth(s.handleLabelList))
		api.HandleFunc("POST /api/v1/repos/{id}/labels", s.requireAuth(s.handleLabelCreate))
		api.HandleFunc("PATCH /api/v1/repos/{id}/labels/{lid}", s.requireAuth(s.handleLabelUpdate))
		api.HandleFunc("DELETE /api/v1/repos/{id}/labels/{lid}", s.requireAuth(s.handleLabelDelete))
	}

	// M6 change-request surface (operator auth; CSRF guards the mutations).
	// Builtin-bound repos only — forge repos 409 on every /crs route. Mounted
	// when the git engine and repos dir were provided (diff and merge read
	// the bare reference clone).
	if s.git != nil && s.reposDir != "" {
		api.HandleFunc("GET /api/v1/repos/{id}/crs", s.requireAuth(s.handleCRList))
		api.HandleFunc("GET /api/v1/repos/{id}/crs/{n}", s.requireAuth(s.handleCRGet))
		// Merge and close delegate to the shared crmerge.Service; mounted only
		// when it was wired (production always wires it alongside Git+ReposDir).
		if s.crmerge != nil {
			api.HandleFunc("POST /api/v1/repos/{id}/crs/{n}/merge", s.requireAuth(s.handleCRMerge))
			api.HandleFunc("POST /api/v1/repos/{id}/crs/{n}/close", s.requireAuth(s.handleCRClose))
		}
	}

	// M5 AFK operator surface (operator auth; CSRF guards the mutations).
	// Mounted only when the engine was built (like the instance routes, it
	// needs the full instance stack behind it).
	if s.afk != nil {
		api.HandleFunc("POST /api/v1/repos/{id}/afk/start", s.requireAuth(s.handleAFKStart))
		api.HandleFunc("PUT /api/v1/repos/{id}/afk/auto", s.requireAuth(s.handleAFKAuto))
		api.HandleFunc("POST /api/v1/repos/{id}/afk/reset", s.requireAuth(s.handleAFKReset))
	}

	// M5 PAT CRUD (D7) and settings — store-backed, always mounted.
	api.HandleFunc("GET /api/v1/tokens", s.requireAuth(s.handleTokenList))
	api.HandleFunc("POST /api/v1/tokens", s.requireAuth(s.handleTokenCreate))
	api.HandleFunc("DELETE /api/v1/tokens/{id}", s.requireAuth(s.handleTokenDelete))
	api.HandleFunc("GET /api/v1/settings", s.requireAuth(s.handleSettingsGet))
	api.HandleFunc("PATCH /api/v1/settings", s.requireAuth(s.handleSettingsPatch))

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
