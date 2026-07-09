// Command lab is the server binary: config → store → event bus → HTTP API,
// with graceful shutdown (tmux sessions survive by design — shutdown only
// stops the HTTP listener).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/agentapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/chat"
	"git.cloonar.com/Cloonar/coding-lab/internal/config"
	"git.cloonar.com/Cloonar/coding-lab/internal/crmerge"
	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/httpapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/metrics"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/claudecode"
	"git.cloonar.com/Cloonar/coding-lab/internal/reconcile"
	"git.cloonar.com/Cloonar/coding-lab/internal/reposvc"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/builtin"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/forgejo"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/github"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// version is stamped via -ldflags "-X main.version=…".
var version = "dev"

const usage = `lab — phone-first control panel for Claude Code agents

Usage: lab [flags]

Flags (env overrides in parentheses; flag > env > default):
  -addr string             listen address (LAB_ADDR; default ":8080")
  -state-dir string        state directory (LAB_STATE_DIR; default ~/.local/state/lab)
  -db string               sqlite:<path> or postgres://… (LAB_DB; default sqlite:<state-dir>/lab.db)
  -master-key-file string  vault master key file (LAB_MASTER_KEY_FILE; default <state-dir>/master.key)
  -claude, -tmux, -git, -prlimit string
                           binary paths (PATH lookup by default)
  -claude-config string    claude's global config file (LAB_CLAUDE_CONFIG; default ~/.claude.json)
  -max-instances int       global live-instance cap; seeds the settings row on first start (default 6)
  -session-nofile int      RLIMIT_NOFILE for spawned sessions; 0 disables (default 16384)
  -proxy-auth              accept the proxy auth header from trusted proxies
  -proxy-auth-header string  header carrying the proxy-authenticated username (default "Remote-User")
  -trusted-proxies string  comma-separated CIDRs of trusted reverse proxies
  -base-url string         external base URL, e.g. https://lab.example.com (LAB_BASE_URL)
  -agent-url string        session-facing base URL handed to labctl as LAB_URL;
                           defaults to --base-url, else http://127.0.0.1:<port> (LAB_AGENT_URL)
`

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := config.Parse(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(os.Stderr, usage)
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "lab: %v\n", err)
		return 2
	}

	logger := logx.New(os.Stdout)

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		logger.Error("creating state dir", "component", "main", "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DB, logger)
	if err != nil {
		logger.Error("opening store", "component", "main", "err", err)
		return 1
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Error("closing store", "component", "main", "err", err)
		}
	}()

	bus := events.NewBus()
	m := metrics.New()

	// Vault (design §6): load-or-generate the master key, refuse loose
	// perms/malformed content, and prepare the runtime materialization dir.
	masterKey, err := loadOrGenerateMasterKey(cfg.MasterKeyFile, logger)
	if err != nil {
		logger.Error("master key", "component", "main", "err", err)
		return 1
	}
	vlt, err := vault.New(masterKey)
	if err != nil {
		logger.Error("opening vault", "component", "main", "err", err)
		return 1
	}
	mat, err := vault.NewMaterializer(filepath.Join(cfg.StateDir, "runtime"))
	if err != nil {
		logger.Error("preparing runtime dir", "component", "main", "err", err)
		return 1
	}

	// Tracker registry (M4): resolves a repo-scoped Tracker per binding. The
	// backend constructors are injected here — cmd/lab is the one place that
	// imports tracker + builtin + forgejo + github, so no import cycle forms.
	// Each forge factory is a one-line adapter (New returns the concrete
	// client); the HTTP client is explicit with the pinned 30s timeout.
	trackerReg := tracker.NewRegistry(st, vlt, &http.Client{Timeout: 30 * time.Second},
		builtin.New,
		func(c tracker.ForgejoConfig) tracker.Tracker {
			return forgejo.New(c.HTTPClient, c.BaseURL, c.Token, c.Owner, c.Repo)
		},
		func(c tracker.GitHubConfig) tracker.Tracker {
			return github.New(c.HTTPClient, c.BaseURL, c.Token, c.Owner, c.Repo)
		})
	// lab_tracker_requests_total (M8): every tracker resolved through the
	// registry reports (binding, op, ok) — never error text or token bytes.
	trackerReg.SetObserver(m.TrackerRequest)

	gitEngine := gitx.New(cfg.GitBin)
	reposDir := filepath.Join(cfg.StateDir, "repos")
	worktreeRoot := filepath.Join(cfg.StateDir, "worktrees")

	// Shared CR-merge service (ADR-0011): the operator merge/close routes and
	// the agent surface's built-in MergePull both land through this one
	// orchestration. Injected into the tracker registry so the built-in
	// tracker can reuse it (SetCRMerger is read lazily at TrackerFor time, so
	// wiring it here — after the registry was built — is fine), and handed to
	// the HTTP API for the operator /crs routes.
	mergeSvc := crmerge.New(crmerge.Config{
		Store:        st,
		Git:          gitEngine,
		Vault:        vlt,
		Materializer: mat,
		Bus:          bus,
		ReposDir:     reposDir,
		Now:          time.Now,
		Logger:       logger,
	})
	trackerReg.SetCRMerger(mergeSvc)

	// M3 instance/AFK stack. It needs claude's global config path (resolved
	// from HOME); with none, the instance/parked/provider routes stay unmounted
	// and lab still serves the M2 surface.
	var (
		instanceSvc  *instance.Service
		reconcileSvc *reconcile.Service
		providerReg  *provider.Registry
		afkSvc       *afk.Service
		chatSvc      *chat.Service
	)
	if cfg.ClaudeConfig == "" {
		logger.Warn("claude config path unresolved (HOME unset and no --claude-config); instance features disabled",
			"component", "main")
	} else {
		home := os.Getenv("HOME")
		runner := tmuxx.New(cfg.TmuxBin, tmuxx.WithNofileCap(cfg.PrlimitBin, cfg.SessionNofile))
		claudeProvider, perr := claudecode.New(claudecode.Options{
			ClaudeBin:   cfg.ClaudeBin,
			ConfigPath:  cfg.ClaudeConfig,
			RegistryDir: filepath.Join(home, ".claude", "sessions"),
			ProjectsDir: filepath.Join(home, ".claude", "projects"),
			LoginDir:    home,
			Runner:      runner,
			Bus:         bus,
			Logger:      logger,
		})
		if perr != nil {
			logger.Error("building claude provider", "component", "main", "err", perr)
			return 1
		}
		providerReg, err = provider.NewRegistry(claudeProvider)
		if err != nil {
			logger.Error("building provider registry", "component", "main", "err", err)
			return 1
		}
		guard := startguard.New()
		instanceSvc, err = instance.New(instance.Options{
			Store:        st,
			Git:          gitEngine,
			Runner:       runner,
			Providers:    providerReg,
			Vault:        vlt,
			Materializer: mat,
			Guard:        guard,
			Bus:          bus,
			Logger:       logger,
			ReposDir:     reposDir,
			WorktreeRoot: worktreeRoot,
			LabURL:       labURL(cfg),
			CaptureCtx:   ctx,
		})
		if err != nil {
			logger.Error("building instance service", "component", "main", "err", err)
			return 1
		}
		reconcileSvc, err = reconcile.New(reconcile.Options{
			Store:        st,
			Git:          gitEngine,
			Runner:       runner,
			Guard:        guard,
			Materializer: mat,
			Bus:          bus,
			Logger:       logger,
			ReposDir:     reposDir,
			ArmCapture:   instanceSvc.ArmCapture,
			AFKRunEnded:  m.AFKRunEnded,
		})
		if err != nil {
			logger.Error("building reconcile service", "component", "main", "err", err)
			return 1
		}
		// AFK engine (M5): the scheduler/reaper/claim core. Its neutral Stop
		// is delegated from the instance service (design §4c), and its reaper
		// loop carries the throttled runtime sweep (v0's single janitorial
		// goroutine).
		afkSvc, err = afk.New(afk.Options{
			Store:        st,
			Git:          gitEngine,
			Runner:       runner,
			Trackers:     trackerReg,
			Instances:    instanceSvc,
			Materializer: mat,
			Bus:          bus,
			Guard:        guard,
			Logger:       logger,
			ReposDir:     reposDir,
			WorktreeRoot: worktreeRoot,
			Sweep:        reconcileSvc.RuntimeSweep,
			Metrics:      m,
		})
		if err != nil {
			logger.Error("building afk engine", "component", "main", "err", err)
			return 1
		}
		instanceSvc.SetAFKStopper(afkSvc)

		// Embedded chat (issue #7): the transcript tailer + read/act brain.
		// It self-syncs its tailer set to the active runs off the event bus,
		// and feeds the instance list its conversational-state field.
		chatSvc, err = chat.New(chat.Options{
			Store:      st,
			Providers:  providerReg,
			Bus:        bus,
			Logger:     logger,
			Ctx:        ctx,
			RuntimeDir: mat.Dir(),
		})
		if err != nil {
			logger.Error("building chat service", "component", "main", "err", err)
			return 1
		}
		instanceSvc.SetChatState(chatSvc)

		// lab_instances_active (M8): a scrape-time gauge over the live
		// tmux+active-runs view — registered only with the instance stack up
		// (without it no runner exists; the absent series says so).
		m.RegisterInstances(instanceSvc.LiveCounts)
	}

	// Agent API (M5/M6): run-token-authenticated tracker surface, repo-scoped
	// by the run row; resolves trackers through the same registry and
	// publishes cr.changed when a builtin PR create opens a change request.
	// Built AFTER the provider registry so the incogni body sanitizer runs the
	// compiled cross-provider union of every provider's declared ScrubPatterns
	// (ADR-0033); a nil registry (degraded no-provider boot) yields no scrub,
	// so incogni bodies pass through unstripped — content-inert like the hook.
	var scrub []*regexp.Regexp
	if providerReg != nil {
		scrub = providerReg.ScrubRegexps()
	}
	agent := agentapi.New(st, trackerReg, bus, logger, time.Now, scrub)

	// Seed the settings AFTER the provider registry exists: provider_default
	// is seeded to the FIRST registered provider's ID (issue #66) so the store
	// stays provider-agnostic ("claude-code" today). The degraded no-provider
	// boot seeds an empty row — empty means inherit, and the spawn surface is
	// unmounted in that mode anyway. Nothing before this point reads settings.
	defaultProviderID := ""
	if providerReg != nil {
		if list := providerReg.List(); len(list) > 0 {
			defaultProviderID = list[0].ID()
		}
	}
	if err := st.SeedDefaultSettings(ctx, cfg.MaxInstances, defaultProviderID); err != nil {
		logger.Error("seeding default settings", "component", "main", "err", err)
		return 1
	}

	repoOpts := reposvc.Options{
		Store:        st,
		Vault:        vlt,
		Materializer: mat,
		Git:          gitEngine,
		Bus:          bus,
		Logger:       logger,
		ReposDir:     reposDir,
		Metrics:      m,
		// Providers backs the incogni pre-push hook: it screens the union of
		// every registered provider's declared scrub patterns (ADR-0033). nil in
		// the degraded boot where no provider is configured (ClaudeConfig
		// unresolved above), which renders a content-inert guard.
		Providers: providerReg,
	}
	if instanceSvc != nil {
		// Preserve live-session credential files across the restart heal — the
		// authoritative keep-set sweep runs in reconcile.StartupReconcile below,
		// after re-adoption. And wire the delete guard to live instances.
		repoOpts.CredentialKeep = func(string) bool { return true }
		repoOpts.LiveInstances = instanceSvc.LiveInstances
		repoOpts.StopInstances = instanceSvc.StopAll
	}
	repoSvc, err := reposvc.New(repoOpts)
	if err != nil {
		logger.Error("building repo service", "component", "main", "err", err)
		return 1
	}

	// Heal interrupted clones BEFORE serving (design §3a/§6), then run startup
	// reconciliation (re-adoption + orphan teardown + the keep-set credential
	// sweep) synchronously before any scheduler — no Start can race it.
	if err := repoSvc.StartupHeal(ctx); err != nil {
		logger.Error("startup heal", "component", "main", "err", err)
		return 1
	}
	if reconcileSvc != nil {
		if err := reconcileSvc.StartupReconcile(ctx); err != nil {
			logger.Error("startup reconcile", "component", "main", "err", err)
			return 1
		}
	}

	api, err := httpapi.New(httpapi.Options{
		Store:           st,
		Bus:             bus,
		Logger:          logger,
		Metrics:         m,
		Vault:           vlt,
		Repos:           repoSvc,
		Instances:       instanceSvc,
		Reconcile:       reconcileSvc,
		Chat:            chatSvc,
		Providers:       providerReg,
		Tracker:         trackerReg,
		AFK:             afkSvc,
		Git:             gitEngine,
		Materializer:    mat,
		ReposDir:        reposDir,
		CRMerge:         mergeSvc,
		BaseURL:         cfg.BaseURL,
		ProxyAuth:       cfg.ProxyAuth,
		ProxyAuthHeader: cfg.ProxyAuthHeader,
		TrustedProxies:  cfg.TrustedProxies,
		AgentHandler:    agent.Handler(),
	})
	if err != nil {
		logger.Error("building http api", "component", "main", "err", err)
		return 1
	}

	// The AFK reaper and scheduler loops (M5). The reaper tick also carries
	// the throttled runtime sweep on its sweep_interval_minutes cadence —
	// reconcile.SweepLoop is superseded by this wiring. Both loops re-read
	// their intervals from settings each tick and stop when ctx is cancelled
	// at shutdown.
	if afkSvc != nil {
		go afkSvc.ReaperLoop(ctx)
		go afkSvc.SchedulerLoop(ctx)
	}
	// The chat tailer: one goroutine that keeps a per-live-instance transcript
	// tailer set in sync with the active runs and publishes run.messages.changed.
	if chatSvc != nil {
		go chatSvc.Run(ctx)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Shutdown only closes listeners and waits for handlers; open SSE
	// streams would hold it until its deadline. This hook cancels the
	// server-scoped context those streams select on, so they drain first.
	srv.RegisterOnShutdown(api.CloseStreams)

	logger.Info("lab starting",
		"component", "main",
		"version", version,
		"addr", cfg.Addr,
		"db", dbBackend(cfg.DB),
		"state_dir", cfg.StateDir)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case err := <-errCh:
		logger.Error("http server failed", "component", "main", "err", err)
		return 1
	case <-ctx.Done():
		logger.Info("shutting down", "component", "main")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "component", "main", "err", err)
			return 1
		}
		// Cancel running clone jobs; interrupted repos heal on next start.
		repoSvc.Close()
		return 0
	}
}

// loadOrGenerateMasterKey implements the design §6 first-start bootstrap:
// stat-then-Generate — Generate itself refuses to overwrite an existing key
// file, so a lost race can never clobber one.
func loadOrGenerateMasterKey(path string, logger *slog.Logger) ([]byte, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		key, genErr := vault.Generate(path)
		if genErr != nil {
			return nil, genErr
		}
		logger.Info("generated vault master key", "component", "main", "path", path)
		return key, nil
	}
	return vault.Load(path)
}

// labURL is the LAB_URL handed to spawned sessions. Precedence: the dedicated
// agent URL when set, else the external base URL, else http://127.0.0.1:<port>
// (labctl runs on the same host). The agent URL wins over the base URL so a
// deployment behind an SSO/auth proxy can keep machine traffic on loopback
// while the base URL still names the external origin (issue #30).
func labURL(cfg config.Config) string {
	if cfg.AgentURL != "" {
		return cfg.AgentURL
	}
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil || port == "" {
		port = "8080"
	}
	return "http://127.0.0.1:" + port
}

// dbBackend names the backend for the startup line without ever echoing the
// DSN (postgres DSNs carry passwords).
func dbBackend(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "sqlite:"):
		return "sqlite"
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return "postgres"
	default:
		return "unknown"
	}
}
