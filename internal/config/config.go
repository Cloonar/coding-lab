// Package config parses lab's flags and environment into a Config
// (brief §8.5). Precedence: flag > env > default. Parse is pure: it touches
// no globals and reads the environment only through the injected getenv.
package config

import (
	"flag"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
)

// Defaults for the flags that have fixed defaults (path-shaped defaults
// derive from --state-dir at parse time).
const (
	DefaultAddr            = ":8080"
	DefaultProxyAuthHeader = "Remote-User"
	DefaultMaxInstances    = 6
	DefaultSessionNofile   = 16384
)

// Config is the fully resolved process configuration.
type Config struct {
	Addr          string // listen address
	StateDir      string // root of lab's state (db, master key, repos, worktrees, runtime)
	DB            string // DSN: sqlite:<path> or postgres://…
	MasterKeyFile string // path to the vault master key file

	ClaudeBin  string
	TmuxBin    string
	GitBin     string
	PrlimitBin string

	MaxInstances  int // seeds the max_instances settings row on first start
	SessionNofile int // RLIMIT_NOFILE for spawned sessions; 0 disables

	ProxyAuth       bool
	ProxyAuthHeader string
	TrustedProxies  []netip.Prefix

	BaseURL string // external base URL; "" when unset
}

// Parse resolves args (flags without the program name) and environment into
// a Config. Env overrides defaults, flags override env. Recognized env vars:
// LAB_ADDR, LAB_DB, LAB_STATE_DIR, LAB_MASTER_KEY_FILE, LAB_BASE_URL.
func Parse(args []string, getenv func(string) string) (Config, error) {
	fs := flag.NewFlagSet("lab", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		addr          = fs.String("addr", "", "listen address (default \":8080\"; env LAB_ADDR)")
		stateDir      = fs.String("state-dir", "", "state directory (default ~/.local/state/lab; env LAB_STATE_DIR)")
		db            = fs.String("db", "", "database DSN: sqlite:<path> or postgres://… (default sqlite:<state-dir>/lab.db; env LAB_DB)")
		masterKeyFile = fs.String("master-key-file", "", "vault master key file (default <state-dir>/master.key; env LAB_MASTER_KEY_FILE)")

		claudeBin  = fs.String("claude", "claude", "claude binary (PATH lookup by default)")
		tmuxBin    = fs.String("tmux", "tmux", "tmux binary (PATH lookup by default)")
		gitBin     = fs.String("git", "git", "git binary (PATH lookup by default)")
		prlimitBin = fs.String("prlimit", "prlimit", "prlimit binary (PATH lookup by default)")

		maxInstances  = fs.Int("max-instances", DefaultMaxInstances, "global live-instance cap; seeds the settings row on first start")
		sessionNofile = fs.Int("session-nofile", DefaultSessionNofile, "RLIMIT_NOFILE (soft+hard) for spawned sessions; 0 disables")

		proxyAuth       = fs.Bool("proxy-auth", false, "accept the proxy auth header from trusted proxies")
		proxyAuthHeader = fs.String("proxy-auth-header", DefaultProxyAuthHeader, "header carrying the proxy-authenticated username")
		trustedProxies  = fs.String("trusted-proxies", "", "comma-separated CIDRs of trusted reverse proxies")

		baseURL = fs.String("base-url", "", "external base URL, e.g. https://lab.example.com (env LAB_BASE_URL)")
	)

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}

	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	// pick resolves flag > env > fallback for one value.
	pick := func(name, flagVal, envKey, fallback string) string {
		if set[name] {
			return flagVal
		}
		if envKey != "" {
			if v := getenv(envKey); v != "" {
				return v
			}
		}
		return fallback
	}

	var cfg Config

	cfg.Addr = pick("addr", *addr, "LAB_ADDR", DefaultAddr)
	if cfg.Addr == "" {
		return Config{}, fmt.Errorf("--addr must not be empty")
	}

	sd := pick("state-dir", *stateDir, "LAB_STATE_DIR", "")
	if sd == "" {
		home := getenv("HOME")
		if home == "" {
			return Config{}, fmt.Errorf("cannot derive the default state dir: HOME is unset; pass --state-dir or LAB_STATE_DIR")
		}
		sd = filepath.Join(home, ".local", "state", "lab")
	}
	cfg.StateDir = sd

	cfg.DB = pick("db", *db, "LAB_DB", "sqlite:"+filepath.Join(sd, "lab.db"))
	if cfg.DB == "" {
		return Config{}, fmt.Errorf("--db must not be empty")
	}
	cfg.MasterKeyFile = pick("master-key-file", *masterKeyFile, "LAB_MASTER_KEY_FILE", filepath.Join(sd, "master.key"))
	cfg.BaseURL = pick("base-url", *baseURL, "LAB_BASE_URL", "")

	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil {
			return Config{}, fmt.Errorf("--base-url: %w", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("--base-url %q: want an absolute http(s) URL", cfg.BaseURL)
		}
	}

	cfg.ClaudeBin = *claudeBin
	cfg.TmuxBin = *tmuxBin
	cfg.GitBin = *gitBin
	cfg.PrlimitBin = *prlimitBin

	cfg.MaxInstances = *maxInstances
	if cfg.MaxInstances < 1 {
		return Config{}, fmt.Errorf("--max-instances %d: must be at least 1", cfg.MaxInstances)
	}
	cfg.SessionNofile = *sessionNofile
	if cfg.SessionNofile < 0 {
		return Config{}, fmt.Errorf("--session-nofile %d: must be >= 0 (0 disables the cap)", cfg.SessionNofile)
	}

	cfg.ProxyAuth = *proxyAuth
	cfg.ProxyAuthHeader = *proxyAuthHeader
	if cfg.ProxyAuth && cfg.ProxyAuthHeader == "" {
		return Config{}, fmt.Errorf("--proxy-auth requires a non-empty --proxy-auth-header")
	}

	for part := range strings.SplitSeq(*trustedProxies, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return Config{}, fmt.Errorf("--trusted-proxies: %q is not a CIDR (e.g. 10.0.0.0/8): %w", part, err)
		}
		cfg.TrustedProxies = append(cfg.TrustedProxies, p)
	}

	return cfg, nil
}
