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
	"slices"
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

// aliasProviderID is the single provider id the deprecated -claude /
// -claude-config / LAB_CLAUDE_CONFIG aliases write to. It exists solely to
// keep those pre-#78 spellings working as aliases for the generic
// -provider-bin / -provider-config maps and MUST NOT grow: it is the only
// provider-named identifier this package is permitted to carry (issue #78),
// and a new provider adds nothing here.
const aliasProviderID = "claude-code"

// Config is the fully resolved process configuration.
type Config struct {
	Addr          string // listen address
	StateDir      string // root of lab's state (db, master key, repos, worktrees, runtime)
	DB            string // DSN: sqlite:<path> or postgres://…
	MasterKeyFile string // path to the vault master key file
	VAPIDKeyFile  string // path to the web push VAPID key file (RFC 8292)

	// ProviderBin maps a provider id to a binary-path override. A missing
	// entry means the adapter uses its own default (a PATH lookup); config.go
	// no longer owns any provider default (issue #78). Always non-nil after
	// Parse and holds entries only for explicitly configured ids — set via
	// -provider-bin, LAB_PROVIDER_BIN_<ID>, or the deprecated -claude alias.
	ProviderBin map[string]string
	// ProviderConfig maps a provider id to a config-file-path override. A
	// missing entry means the adapter uses its own default. Always non-nil
	// after Parse and holds entries only for explicitly configured ids — set
	// via -provider-config, LAB_PROVIDER_CONFIG_<ID>, or the deprecated
	// -claude-config / LAB_CLAUDE_CONFIG alias.
	ProviderConfig map[string]string

	TmuxBin    string
	GitBin     string
	PrlimitBin string
	PodmanBin  string

	// ContainerImage is the dev image containerized sessions run in
	// (issue #205). "" means unconfigured — container mode then refuses to
	// spawn (the podmanx preflight owns that refusal, not Parse), because
	// lab deliberately ships no dev image of its own: the operator owns the
	// container userland, lab injects only the agent tools (ADR-0051).
	ContainerImage string

	// ContainerToolsImages maps a provider id to its agent-tools OCI image
	// ref, the read-only /opt/lab injection of ADR-0051. Refs should be
	// @sha256-pinned — the digest, not the tag, is the consumer contract (a
	// same-tag re-push silently moves the tag) — but pinning is documented,
	// not enforced, so an operator can point at a local tag during
	// bring-up. Always non-nil after Parse; empty means unconfigured
	// (container mode refuses to spawn, again via preflight). Set via
	// --container-tools-image or LAB_CONTAINER_TOOLS_IMAGE, value
	// provider=ref[,provider=ref…]; the flag value replaces the env value
	// wholesale (single-value pick precedence, unlike the per-entry
	// -provider-* merge).
	ContainerToolsImages map[string]string

	MaxInstances  int // seeds the max_instances settings row on first start
	SessionNofile int // RLIMIT_NOFILE for spawned sessions; 0 disables

	ProxyAuth       bool
	ProxyAuthHeader string
	TrustedProxies  []netip.Prefix

	BaseURL string // external base URL; "" when unset

	// AgentURL is the session-facing base URL handed to labctl as LAB_URL,
	// independent of BaseURL. "" when unset; the session-URL helper then
	// defaults to the agent unix socket (unix://<state-dir>/agent/agent.sock,
	// issue #201; relocated into its own dir by #205). It exists as the explicit override for deployments where
	// the default socket won't do — an http(s) URL for off-host sessions, or
	// a unix:///abs/path naming a different socket. Machine traffic never
	// routes through BaseURL: an external (possibly SSO-fronted) origin is
	// exactly the issue #30 failure mode.
	AgentURL string

	// SeedUser is the username of the initial operator user to seed at
	// startup. "" means no seeding. Must be set together with a seed password
	// hash source (SeedPasswordHash or SeedPasswordHashFile).
	SeedUser string
	// SeedPasswordHash is a PHC-encoded argon2id hash of the initial operator
	// password, given inline. "" when unset. Inline is safe by design: a
	// hash is not a secret (issue #137).
	SeedPasswordHash string
	// SeedPasswordHashFile is a path to a file holding the PHC-encoded
	// argon2id hash of the initial operator password. "" when unset. Parse
	// stays pure (package doc comment): this file is read at seed time in
	// cmd/lab, not here. When both SeedPasswordHash and SeedPasswordHashFile
	// are set, file-wins precedence is resolved at seed time, not here.
	SeedPasswordHashFile string
}

// providerMapFlag is the flag.Value behind the repeatable -provider-bin and
// -provider-config flags. Each occurrence is an id=path pair; occurrences
// accumulate into m. It validates every id against the caller's registered
// providerIDs so a boot-time typo is caught immediately rather than silently
// ignored, which is why Parse takes providerIDs and this package stays
// provider-name-free (issue #78).
type providerMapFlag struct {
	providerIDs []string
	m           map[string]string
}

func (p *providerMapFlag) String() string { return "" }

func (p *providerMapFlag) Set(value string) error {
	id, path, ok := strings.Cut(value, "=")
	if !ok || id == "" || path == "" {
		return fmt.Errorf("%q: want id=path", value)
	}
	if !slices.Contains(p.providerIDs, id) {
		return fmt.Errorf("unknown provider id %q; registered ids: %s", id, strings.Join(p.providerIDs, ", "))
	}
	if _, dup := p.m[id]; dup {
		return fmt.Errorf("provider id %q set more than once", id)
	}
	p.m[id] = path
	return nil
}

// Parse resolves args (flags without the program name) and environment into
// a Config. Env overrides defaults, flags override env. Recognized env vars:
// LAB_ADDR, LAB_DB, LAB_STATE_DIR, LAB_MASTER_KEY_FILE, LAB_VAPID_KEY_FILE,
// LAB_PROVIDER_BIN_<ID>, LAB_PROVIDER_CONFIG_<ID>, LAB_CLAUDE_CONFIG (a deprecated alias for
// LAB_PROVIDER_CONFIG_CLAUDE_CODE), LAB_CONTAINER_IMAGE, LAB_CONTAINER_TOOLS_IMAGE,
// LAB_BASE_URL, LAB_AGENT_URL, LAB_SEED_USER,
// LAB_SEED_PASSWORD_HASH, LAB_SEED_PASSWORD_HASH_FILE. providerIDs
// is the caller's list of registered provider ids: the generic per-provider
// flags are validated against it (an unknown id is a parse error), and the
// LAB_PROVIDER_*_<ID> env forms are read only for ids it contains.
func Parse(args []string, getenv func(string) string, providerIDs []string) (Config, error) {
	fs := flag.NewFlagSet("lab", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var (
		addr          = fs.String("addr", "", "listen address (default \":8080\"; env LAB_ADDR)")
		stateDir      = fs.String("state-dir", "", "state directory (default ~/.local/state/lab; env LAB_STATE_DIR)")
		db            = fs.String("db", "", "database DSN: sqlite:<path> or postgres://… (default sqlite:<state-dir>/lab.db; env LAB_DB)")
		masterKeyFile = fs.String("master-key-file", "", "vault master key file (default <state-dir>/master.key; env LAB_MASTER_KEY_FILE)")
		vapidKeyFile  = fs.String("vapid-key-file", "", "web push VAPID key file (default <state-dir>/vapid.key; env LAB_VAPID_KEY_FILE)")

		tmuxBin    = fs.String("tmux", "tmux", "tmux binary (PATH lookup by default)")
		gitBin     = fs.String("git", "git", "git binary (PATH lookup by default)")
		prlimitBin = fs.String("prlimit", "prlimit", "prlimit binary (PATH lookup by default)")
		podmanBin  = fs.String("podman", "podman", "podman binary (PATH lookup by default)")

		containerImage      = fs.String("container-image", "", "dev image containerized sessions run in; empty refuses container spawns (env LAB_CONTAINER_IMAGE)")
		containerToolsImage = fs.String("container-tools-image", "", "agent-tools image refs, provider=ref[,provider=ref…], @sha256-pinned per ADR-0051 (env LAB_CONTAINER_TOOLS_IMAGE)")

		maxInstances  = fs.Int("max-instances", DefaultMaxInstances, "global live-instance cap; seeds the settings row on first start")
		sessionNofile = fs.Int("session-nofile", DefaultSessionNofile, "RLIMIT_NOFILE (soft+hard) for spawned sessions; 0 disables")

		proxyAuth       = fs.Bool("proxy-auth", false, "accept the proxy auth header from trusted proxies")
		proxyAuthHeader = fs.String("proxy-auth-header", DefaultProxyAuthHeader, "header carrying the proxy-authenticated username")
		trustedProxies  = fs.String("trusted-proxies", "", "comma-separated CIDRs of trusted reverse proxies")

		baseURL  = fs.String("base-url", "", "external base URL, e.g. https://lab.example.com (env LAB_BASE_URL)")
		agentURL = fs.String("agent-url", "", "session-facing base URL handed to labctl as LAB_URL, http(s) or unix:///abs/path; defaults to unix://<state-dir>/agent/agent.sock (env LAB_AGENT_URL)")

		seedUser             = fs.String("seed-user", "", "username of the initial operator user to seed at startup (env LAB_SEED_USER)")
		seedPasswordHash     = fs.String("seed-password-hash", "", "PHC-encoded argon2id hash of the initial operator password, given inline (env LAB_SEED_PASSWORD_HASH)")
		seedPasswordHashFile = fs.String("seed-password-hash-file", "", "path to a file holding the PHC-encoded argon2id hash of the initial operator password (env LAB_SEED_PASSWORD_HASH_FILE)")
	)

	// Generic per-provider host overrides (issue #78): repeatable id=path
	// flags accumulating into maps, each id validated against providerIDs.
	binOverrides := &providerMapFlag{providerIDs: providerIDs, m: map[string]string{}}
	configOverrides := &providerMapFlag{providerIDs: providerIDs, m: map[string]string{}}
	fs.Var(binOverrides, "provider-bin", "provider binary path override, repeatable, id=path (env LAB_PROVIDER_BIN_<ID>)")
	fs.Var(configOverrides, "provider-config", "provider config-file path override, repeatable, id=path (env LAB_PROVIDER_CONFIG_<ID>)")

	// --- Deprecated alias shim (issue #78) ----------------------------------
	// The ONLY provider-named flags left in this file. These spellings predate
	// the generic -provider-bin / -provider-config maps and survive solely as
	// aliases for the claude-code entry of those maps (see aliasProviderID).
	// Do NOT add more provider-named flags here — a new provider is configured
	// entirely through the generic map surface above.
	claudeBin := fs.String("claude", "", "deprecated alias for -provider-bin claude-code=<path>")
	claudeConfig := fs.String("claude-config", "", "deprecated alias for -provider-config claude-code=<path> (env LAB_CLAUDE_CONFIG)")
	// ------------------------------------------------------------------------

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
	cfg.VAPIDKeyFile = pick("vapid-key-file", *vapidKeyFile, "LAB_VAPID_KEY_FILE", filepath.Join(sd, "vapid.key"))

	// Per-provider host overrides. For each map, generic env fills an entry
	// for a registered id, then the generic flag overrides it (flag > env).
	// The maps stay non-nil and carry entries only for explicitly configured
	// ids.
	cfg.ProviderBin = resolveProviderMap(providerIDs, getenv, "LAB_PROVIDER_BIN_", binOverrides.m)
	cfg.ProviderConfig = resolveProviderMap(providerIDs, getenv, "LAB_PROVIDER_CONFIG_", configOverrides.m)

	// Deprecated alias shim (issue #78). Per-entry precedence is
	// generic flag > generic env > alias flag > alias env: the alias fills the
	// claude-code entry only when the generic forms (flag or env) left it
	// unset, and between the alias flag and LAB_CLAUDE_CONFIG the flag wins
	// (the existing pick rule). An empty alias value never creates an entry.
	// -claude has no env form; -claude-config aliases LAB_CLAUDE_CONFIG.
	if _, ok := cfg.ProviderBin[aliasProviderID]; !ok {
		if v := pick("claude", *claudeBin, "", ""); v != "" {
			cfg.ProviderBin[aliasProviderID] = v
		}
	}
	if _, ok := cfg.ProviderConfig[aliasProviderID]; !ok {
		if v := pick("claude-config", *claudeConfig, "LAB_CLAUDE_CONFIG", ""); v != "" {
			cfg.ProviderConfig[aliasProviderID] = v
		}
	}

	cfg.BaseURL = pick("base-url", *baseURL, "LAB_BASE_URL", "")
	if cfg.BaseURL != "" {
		if err := validateHTTPURL("--base-url", cfg.BaseURL); err != nil {
			return Config{}, err
		}
	}

	cfg.AgentURL = pick("agent-url", *agentURL, "LAB_AGENT_URL", "")
	if cfg.AgentURL != "" {
		if err := validateAgentURL("--agent-url", cfg.AgentURL); err != nil {
			return Config{}, err
		}
	}

	cfg.TmuxBin = *tmuxBin
	cfg.GitBin = *gitBin
	cfg.PrlimitBin = *prlimitBin
	cfg.PodmanBin = *podmanBin

	cfg.ContainerImage = pick("container-image", *containerImage, "LAB_CONTAINER_IMAGE", "")
	toolsImages, err := parseToolsImages(pick("container-tools-image", *containerToolsImage, "LAB_CONTAINER_TOOLS_IMAGE", ""), providerIDs)
	if err != nil {
		return Config{}, err
	}
	cfg.ContainerToolsImages = toolsImages

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

	cfg.SeedUser = pick("seed-user", *seedUser, "LAB_SEED_USER", "")
	cfg.SeedPasswordHash = pick("seed-password-hash", *seedPasswordHash, "LAB_SEED_PASSWORD_HASH", "")
	cfg.SeedPasswordHashFile = pick("seed-password-hash-file", *seedPasswordHashFile, "LAB_SEED_PASSWORD_HASH_FILE", "")
	// Half a seed = typo'd deploy (issue #134): a user with no password hash
	// source silently never gets seeded, and a password hash source with no
	// user is dead config. Both sources together (with a user) is valid —
	// file-wins precedence is resolved at seed time in cmd/lab, not here.
	// The hash itself is safe to pass inline: unlike a plaintext password, a
	// PHC-encoded argon2id hash is not a secret (issue #137).
	hasPasswordHashSource := cfg.SeedPasswordHash != "" || cfg.SeedPasswordHashFile != ""
	if (cfg.SeedUser != "") != hasPasswordHashSource {
		return Config{}, fmt.Errorf("--seed-user and a seed password hash source (--seed-password-hash-file or --seed-password-hash) must be set together")
	}

	return cfg, nil
}

// resolveProviderMap builds a per-provider override map: for each registered
// id a non-empty <envPrefix><ID> env value seeds the entry (ID is the id
// upper-cased with '-' → '_', e.g. claude-code → CLAUDE_CODE), then the
// generic-flag entries override those (flag > env). The result is always
// non-nil and carries entries only for explicitly configured ids.
func resolveProviderMap(providerIDs []string, getenv func(string) string, envPrefix string, flagVals map[string]string) map[string]string {
	out := map[string]string{}
	for _, id := range providerIDs {
		key := envPrefix + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
		if v := getenv(key); v != "" {
			out[id] = v
		}
	}
	for id, path := range flagVals {
		out[id] = path // generic flag beats generic env
	}
	return out
}

// parseToolsImages parses a --container-tools-image value —
// provider=ref pairs, comma-separated — into a map, validating each
// provider id against the registered providerIDs exactly as
// providerMapFlag does for the -provider-* flags: a boot-time typo dies at
// Parse, not as a mysteriously image-less provider at first container
// spawn. Refs should be @sha256-pinned (ADR-0051: the digest, not the tag,
// is the consumer contract) but pinning is documented, not enforced. The
// result is always non-nil; an empty value yields an empty map — the
// podmanx preflight, not Parse, owns the "container mode unconfigured"
// refusal.
func parseToolsImages(value string, providerIDs []string) (map[string]string, error) {
	out := map[string]string{}
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, ref, ok := strings.Cut(part, "=")
		if !ok || id == "" || ref == "" {
			return nil, fmt.Errorf("--container-tools-image %q: want provider=ref[,provider=ref…]", part)
		}
		if !slices.Contains(providerIDs, id) {
			return nil, fmt.Errorf("--container-tools-image: unknown provider id %q; registered ids: %s", id, strings.Join(providerIDs, ", "))
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("--container-tools-image: provider id %q set more than once", id)
		}
		out[id] = ref
	}
	return out, nil
}

// validateAgentURL admits everything validateHTTPURL does plus the agent
// socket scheme labctl understands (issue #201): unix:// followed by an
// absolute socket path. Only --agent-url gets this — --base-url names an
// origin browsers must reach, and a socket is not one.
func validateAgentURL(flag, value string) error {
	if sock, ok := strings.CutPrefix(value, "unix://"); ok {
		if !strings.HasPrefix(sock, "/") {
			return fmt.Errorf("%s %q: unix:// socket path must be absolute (unix:///abs/path)", flag, value)
		}
		return nil
	}
	return validateHTTPURL(flag, value)
}

// validateHTTPURL rejects a value that is not an absolute http(s) URL. Used
// for --base-url and (via validateAgentURL) --agent-url: both name origins
// that must carry a scheme and host (relative or other-scheme values are a
// misconfiguration caught at startup, not silently accepted).
func validateHTTPURL(flag, value string) error {
	// A bare host:port (scheme omitted) trips url.Parse's "first path segment
	// cannot contain colon"; fold that into the same actionable message rather
	// than leaking the parser's wording. || short-circuits, so u is only
	// dereferenced when err is nil.
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s %q: want an absolute http(s) URL", flag, value)
	}
	return nil
}
