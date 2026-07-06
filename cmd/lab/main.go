// Command lab is the server binary: config → store → event bus → HTTP API,
// with graceful shutdown (tmux sessions survive by design — shutdown only
// stops the HTTP listener).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/agentapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/config"
	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/httpapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/metrics"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
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
  -max-instances int       global live-instance cap; seeds the settings row on first start (default 6)
  -session-nofile int      RLIMIT_NOFILE for spawned sessions; 0 disables (default 16384)
  -proxy-auth              accept the proxy auth header from trusted proxies
  -proxy-auth-header string  header carrying the proxy-authenticated username (default "Remote-User")
  -trusted-proxies string  comma-separated CIDRs of trusted reverse proxies
  -base-url string         external base URL, e.g. https://lab.example.com (LAB_BASE_URL)
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

	if err := st.SeedDefaultSettings(ctx, cfg.MaxInstances); err != nil {
		logger.Error("seeding default settings", "component", "main", "err", err)
		return 1
	}

	bus := events.NewBus()
	m := metrics.New()
	agent := agentapi.New(st, logger, time.Now)

	api, err := httpapi.New(httpapi.Options{
		Store:           st,
		Bus:             bus,
		Logger:          logger,
		Metrics:         m,
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
		return 0
	}
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
