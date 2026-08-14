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
	"unicode"
)

// Defaults for the flags that have fixed defaults (path-shaped defaults
// derive from --state-dir at parse time).
const (
	DefaultAddr            = ":8080"
	DefaultProxyAuthHeader = "Remote-User"
	DefaultMaxInstances    = 6
	DefaultSessionNofile   = 16384
	DefaultOneCLIDashboard = OneCLIDashboardOff
)

// OneCLI dashboard exposure modes (issue #26 / ADR-0067). The dashboard is
// OneCLI's own Next.js app on its dashboard/API port, and these are the three
// ways lab can put it in front of an operator. A fourth — proxying it under a
// path prefix on lab's own origin — was researched and REJECTED in ADR-0067
// (no basePath support upstream, and both apps claim /settings), so do not
// add one here.
const (
	OneCLIDashboardOff       = "off"
	OneCLIDashboardPort      = "port"
	OneCLIDashboardSubdomain = "subdomain"
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

	// OneCLIURL is the base URL lab itself uses to reach the OneCLI
	// sidecar's REST API — its dashboard/API port, default 10254 — typically
	// loopback (e.g. http://127.0.0.1:10254). "" means the OneCLI
	// integration is off. Deliberately separate from OneCLIGatewayURL (issue
	// #23): the address lab uses to reach the REST API and the address a
	// container must use to reach the gateway proxy are two different
	// addresses, not the same one at a different path. Must be set together
	// with OneCLIAPIKeyFile — see the pairing check at the bottom of Parse.
	OneCLIURL string
	// OneCLIAPIKeyFile is a path to a 0600 file holding the OneCLI API key.
	// "" when unset. Parse stays pure (package doc comment): this file is
	// read — and its permissions checked — at startup in cmd/lab, not here,
	// exactly as SeedPasswordHashFile is read at seed time rather than at
	// Parse. Must be set together with OneCLIURL (issue #23): a key file
	// with no URL to send it to is dead config, and a URL with no key
	// cannot authenticate.
	OneCLIAPIKeyFile string
	// OneCLIGatewayURL is the OneCLI gateway proxy URL — its default port
	// 10255 — that a later epic (#24) injects into runs as HTTPS_PROXY. It
	// is deliberately separate from OneCLIURL: the address lab itself uses
	// (loopback) is not the address a rootless-podman container can use,
	// because host.containers.internal is pinned to 127.0.0.1 inside lab's
	// containers (ADR-0052) — a container needs a host-reachable address
	// instead. Independently settable from OneCLIURL / OneCLIAPIKeyFile:
	// it is consumed by #24's run wiring and by the gateway reachability
	// probe, neither of which needs the REST API, so it is deliberately NOT
	// part of the OneCLIURL/OneCLIAPIKeyFile pairing rule — do not "fix" it
	// into that pairing.
	OneCLIGatewayURL string
	// OneCLICAFile is a path to the PEM file holding the OneCLI gateway's
	// interception CA certificate on the host. Consumed by the run wiring
	// (issue #24), which composes it with the host's system CA bundle into a
	// per-run trust bundle and points the run's SSL_CERT_FILE,
	// NODE_EXTRA_CA_CERTS, REQUESTS_CA_BUNDLE, and GIT_SSL_CAINFO at that
	// bundle — never at the interception CA alone, which would break every
	// direct HTTPS call the run's NO_PROXY keeps off the gateway. For a
	// container-runner run the same file reaches the container through the
	// per-run runtime dir's existing host-identical bind mount, so this is
	// one setting rather than a host path plus a separate container path.
	// It is a config setting rather than something fetched from the OneCLI
	// sidecar because the certificate must exist as a host FILE regardless —
	// the env vars above point at a path, and a container bind mount needs a
	// source path, neither of which a fetched-at-runtime value would give —
	// and fetching it over the REST API would mean binding new OneCLI API
	// surface, which ADR-0067 pins closed ("the client binding is
	// deliberately partial"). Like OneCLIGatewayURL, it is deliberately NOT
	// part of the OneCLIURL/OneCLIAPIKeyFile pairing rule and must not be
	// "fixed" into it. Unlike OneCLIGatewayURL, though, it is not fully
	// independent: when OneCLIGatewayURL is set and this is empty, a spawn
	// refuses rather than starting a run whose HTTPS is silently broken —
	// enforced in internal/instance, not here (Parse stays pure; see the
	// package doc comment).
	OneCLICAFile string

	// OneCLIDashboard is the resolved OneCLI dashboard exposure mode: always
	// one of OneCLIDashboardOff, OneCLIDashboardPort, or
	// OneCLIDashboardSubdomain after Parse — never "" (an empty flag or env
	// value resolves to "off", same as leaving it unset; see the pick call
	// and the comment beside it). off is the default: nothing about the
	// dashboard is exposed, and OneCLIDashboardAddr/OneCLIDashboardURL below
	// must both be empty in that mode. See ADR-0067 for why there are
	// exactly three modes and not a fourth path-prefix mode.
	OneCLIDashboard string
	// OneCLIDashboardAddr is the listen address for lab's SECOND listener in
	// port mode — a separately authenticated reverse proxy in front of the
	// dashboard, distinct from Addr (lab's main listener) and from OneCLIURL
	// (the existing address lab already uses to reach OneCLI's own
	// dashboard/API port, and the proxy's upstream target in this mode: see
	// the comment on OneCLIDashboardURL for why this task does not add a
	// second OneCLI-side address). "" unless OneCLIDashboard is "port".
	OneCLIDashboardAddr string
	// OneCLIDashboardURL is the browser-facing dashboard origin. Required in
	// subdomain mode, where lab has no listener of its own to derive an
	// origin from — the operator's reverse proxy fronts the dashboard on a
	// domain only the operator knows, and delegates auth back to lab
	// (forward-auth), so lab must be told what that origin is. Optional in
	// port mode, where lab would otherwise derive the browser-facing origin
	// from BaseURL's host plus OneCLIDashboardAddr's port; setting this
	// OVERRIDES that derivation, for the operator who terminates TLS for the
	// second port on a different external port than the one lab listens on
	// internally (e.g. behind a load balancer that remaps ports). This is
	// NOT paired with OneCLIURL the way OneCLIAPIKeyFile is: the reverse-
	// proxy target in port mode is the existing OneCLIURL, not a second
	// setting — ADR-0067 pins the OneCLI client/address surface closed, so
	// there is nothing here for a new setting to pair with.
	OneCLIDashboardURL string

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

	// ContainerImage is the global DEFAULT dev image containerized sessions
	// run in (issue #205), overridable per repo via repos.image_ref (issue
	// #207): a repo with its own image_ref ignores this, one without inherits
	// it. lab deliberately ships no dev image of its own — the operator owns
	// the container userland, lab injects only the agent tools (ADR-0051). ""
	// means no global default, which is NO LONGER a preflight failure (#207):
	// a deployment where every container repo pins its own image_ref is valid.
	// The refusal moved to the spawn instead — a container launch whose repo
	// has neither an image_ref override nor this global default is refused
	// there (only the spawn knows the repo), never at startup.
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

	// SessionCookieDomain is the Domain attribute set on lab's session
	// cookie. "" (the default) omits Domain entirely, which makes the cookie
	// host-only — correct for every topology except one: OneCLIDashboard
	// subdomain mode puts the dashboard at a DIFFERENT host,
	// onecli.<domain>, fronted by the operator's own reverse proxy which
	// delegates auth to lab (forward-auth checking lab's session cookie); a
	// host-only cookie scoped to lab's own host would never reach it, so
	// that mode needs the parent domain instead. Widening a cookie's scope
	// is a real trade-off — every host under the domain now receives it on
	// every request — so this is never derived automatically from BaseURL
	// or OneCLIDashboardURL, only ever set explicitly by the operator who
	// means to make that trade.
	SessionCookieDomain string

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
// LAB_SEED_PASSWORD_HASH, LAB_SEED_PASSWORD_HASH_FILE, LAB_ONECLI_URL,
// LAB_ONECLI_API_KEY_FILE, LAB_ONECLI_GATEWAY_URL, LAB_ONECLI_CA_FILE,
// LAB_ONECLI_DASHBOARD, LAB_ONECLI_DASHBOARD_ADDR, LAB_ONECLI_DASHBOARD_URL,
// LAB_SESSION_COOKIE_DOMAIN. providerIDs is the caller's list of registered
// provider ids: the generic per-provider
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

		oneCLIURL        = fs.String("onecli-url", "", "OneCLI sidecar REST API base URL, e.g. http://127.0.0.1:10254; empty disables the OneCLI integration; must be set with --onecli-api-key-file (env LAB_ONECLI_URL)")
		oneCLIAPIKeyFile = fs.String("onecli-api-key-file", "", "path to a 0600 file holding the OneCLI API key; must be set with --onecli-url (env LAB_ONECLI_API_KEY_FILE)")
		oneCLIGatewayURL = fs.String("onecli-gateway-url", "", "OneCLI gateway proxy URL injected into runs as HTTPS_PROXY, e.g. http://10.88.0.1:10255 — NOT host.containers.internal, which container runs pin to 127.0.0.1 (env LAB_ONECLI_GATEWAY_URL)")
		oneCLICAFile     = fs.String("onecli-ca-file", "", "path to the PEM file holding the OneCLI gateway's interception CA certificate on the host; composed with the host's system CA bundle into a run's trust bundle (env LAB_ONECLI_CA_FILE)")

		oneCLIDashboard     = fs.String("onecli-dashboard", "", "OneCLI dashboard exposure: off (default, nothing exposed), port (lab reverse-proxies it on its own authenticated listener) or subdomain (your reverse proxy fronts it and delegates auth to lab) (env LAB_ONECLI_DASHBOARD)")
		oneCLIDashboardAddr = fs.String("onecli-dashboard-addr", "", "listen address for --onecli-dashboard=port, e.g. :8443 (env LAB_ONECLI_DASHBOARD_ADDR)")
		oneCLIDashboardURL  = fs.String("onecli-dashboard-url", "", "browser-facing dashboard origin, e.g. https://onecli.example.com; required for --onecli-dashboard=subdomain, an optional override in port mode (env LAB_ONECLI_DASHBOARD_URL)")

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

		sessionCookieDomain = fs.String("session-cookie-domain", "", "Domain attribute for lab's session cookie, e.g. example.com; empty (default) keeps the cookie host-only (env LAB_SESSION_COOKIE_DOMAIN)")

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

	cfg.OneCLIURL = pick("onecli-url", *oneCLIURL, "LAB_ONECLI_URL", "")
	if cfg.OneCLIURL != "" {
		if err := validateHTTPURL("--onecli-url", cfg.OneCLIURL); err != nil {
			return Config{}, err
		}
	}
	cfg.OneCLIAPIKeyFile = pick("onecli-api-key-file", *oneCLIAPIKeyFile, "LAB_ONECLI_API_KEY_FILE", "")
	// Half a OneCLI REST config = dead config (issue #23), modeled on the
	// --seed-user / seed-password-hash-source pairing rule above: a URL with
	// no key file cannot authenticate, and a key file with no URL has
	// nowhere to send it. Both unset (the default) leaves the integration
	// off; both set is valid.
	if (cfg.OneCLIURL != "") != (cfg.OneCLIAPIKeyFile != "") {
		return Config{}, fmt.Errorf("--onecli-url and --onecli-api-key-file must be set together or not at all")
	}

	// --onecli-gateway-url is deliberately NOT part of the pairing above: it
	// is consumed by #24's run wiring and by the gateway reachability probe,
	// neither of which needs the OneCLI REST API, so it stays independently
	// settable. Do not "fix" it into the OneCLIURL/OneCLIAPIKeyFile pairing.
	cfg.OneCLIGatewayURL = pick("onecli-gateway-url", *oneCLIGatewayURL, "LAB_ONECLI_GATEWAY_URL", "")
	if cfg.OneCLIGatewayURL != "" {
		if err := validateHTTPURL("--onecli-gateway-url", cfg.OneCLIGatewayURL); err != nil {
			return Config{}, err
		}
	}

	// --onecli-ca-file: a path is a path, exactly like --master-key-file (see
	// its field doc). No filesystem access and no pairing/presence validation
	// here — Parse does shape validation only. The file is read later, by the
	// run wiring that needs it (issue #24), and the OneCLIGatewayURL-without-
	// OneCLICAFile refusal lives there too (internal/instance), not in this
	// pure function.
	cfg.OneCLICAFile = pick("onecli-ca-file", *oneCLICAFile, "LAB_ONECLI_CA_FILE", "")

	// --- OneCLI dashboard exposure (issue #26 / ADR-0067) -------------------
	// Resolved immediately after the OneCLI REST/gateway/CA block above,
	// because the validation below reads cfg.OneCLIURL (rule 4) and
	// cfg.BaseURL (rule 6), both already settled by this point (BaseURL at
	// the top of Parse, OneCLIURL just above).
	cfg.OneCLIDashboard = pick("onecli-dashboard", *oneCLIDashboard, "LAB_ONECLI_DASHBOARD", OneCLIDashboardOff)
	if cfg.OneCLIDashboard == "" {
		// pick's fallback only fires when the flag was never set at all, so an
		// explicit --onecli-dashboard="" would otherwise slip through here as
		// "" rather than "off" — the same intent as leaving it unset, so fold
		// it in too.
		cfg.OneCLIDashboard = OneCLIDashboardOff
	}
	cfg.OneCLIDashboardAddr = pick("onecli-dashboard-addr", *oneCLIDashboardAddr, "LAB_ONECLI_DASHBOARD_ADDR", "")
	// --onecli-dashboard-url is deliberately NOT part of a pairing rule with
	// --onecli-url the way --onecli-api-key-file is paired with it above: the
	// reverse-proxy target in port mode is the existing --onecli-url — there
	// is no second OneCLI-side address to introduce, ADR-0067 pins that
	// surface closed — so this setting only ever names a BROWSER-facing
	// origin (the override of the derived-from-BaseURL default in port mode,
	// or the required origin in subdomain mode). Its own presence rules are
	// enforced below, on their own terms.
	cfg.OneCLIDashboardURL = pick("onecli-dashboard-url", *oneCLIDashboardURL, "LAB_ONECLI_DASHBOARD_URL", "")

	switch cfg.OneCLIDashboard {
	case OneCLIDashboardOff, OneCLIDashboardPort, OneCLIDashboardSubdomain:
		// recognized
	default:
		return Config{}, fmt.Errorf("--onecli-dashboard %q: want one of off, port, subdomain", cfg.OneCLIDashboard)
	}
	if cfg.OneCLIDashboardAddr != "" && cfg.OneCLIDashboard != OneCLIDashboardPort {
		return Config{}, fmt.Errorf("--onecli-dashboard-addr is set but --onecli-dashboard is %q: the listen address is only used in port mode", cfg.OneCLIDashboard)
	}
	if cfg.OneCLIDashboardURL != "" && cfg.OneCLIDashboard == OneCLIDashboardOff {
		return Config{}, fmt.Errorf("--onecli-dashboard-url is set but --onecli-dashboard is off: nothing is exposed for it to name")
	}
	if cfg.OneCLIDashboard != OneCLIDashboardOff && cfg.OneCLIURL == "" {
		return Config{}, fmt.Errorf("--onecli-dashboard=%s requires the OneCLI integration: set --onecli-url and --onecli-api-key-file", cfg.OneCLIDashboard)
	}
	if cfg.OneCLIDashboard == OneCLIDashboardPort && cfg.OneCLIDashboardAddr == "" {
		return Config{}, fmt.Errorf("--onecli-dashboard=port requires --onecli-dashboard-addr, the address lab's authenticated dashboard proxy listens on")
	}
	if cfg.OneCLIDashboard == OneCLIDashboardPort && cfg.BaseURL == "" {
		return Config{}, fmt.Errorf("--onecli-dashboard=port requires --base-url, the origin the proxy sends unauthenticated browsers to for login")
	}
	if cfg.OneCLIDashboard == OneCLIDashboardSubdomain && cfg.OneCLIDashboardURL == "" {
		return Config{}, fmt.Errorf("--onecli-dashboard=subdomain requires --onecli-dashboard-url, the browser-facing origin your reverse proxy fronts (e.g. https://onecli.example.com)")
	}
	if cfg.OneCLIDashboardURL != "" {
		if err := validateHTTPURL("--onecli-dashboard-url", cfg.OneCLIDashboardURL); err != nil {
			return Config{}, err
		}
	}

	cfg.SessionCookieDomain = pick("session-cookie-domain", *sessionCookieDomain, "LAB_SESSION_COOKIE_DOMAIN", "")
	if cfg.SessionCookieDomain != "" {
		// A cookie Domain is a bare domain, never an origin: no scheme (it's
		// not a URL), no port (cookies don't carry one), no path (Domain and
		// Path are separate cookie attributes). A single leading dot
		// (.example.com) IS legal, per RFC 6265 §4.1.2.3 — modern browsers
		// ignore it and match identically to the dotless form — so it is
		// accepted verbatim below, never stripped: normalizing it here would
		// just be extra code reproducing what the browser already does. "."
		// alone, though, names no domain at all, so it is rejected like any
		// value carrying a scheme, port, or path.
		if cfg.SessionCookieDomain == "." || strings.ContainsAny(cfg.SessionCookieDomain, "/:") || strings.ContainsFunc(cfg.SessionCookieDomain, unicode.IsSpace) {
			return Config{}, fmt.Errorf("--session-cookie-domain %q: want a bare domain like example.com, with no scheme, port or path", cfg.SessionCookieDomain)
		}
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
