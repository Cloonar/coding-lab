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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/agentapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/chat"
	"git.cloonar.com/Cloonar/coding-lab/internal/config"
	"git.cloonar.com/Cloonar/coding-lab/internal/credrotate"
	"git.cloonar.com/Cloonar/coding-lab/internal/crmerge"
	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/httpapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/imageref"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/instancehome"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/metrics"
	"git.cloonar.com/Cloonar/coding-lab/internal/onecli"
	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/presence"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/claudecode"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/codex"
	"git.cloonar.com/Cloonar/coding-lab/internal/providercli"
	"git.cloonar.com/Cloonar/coding-lab/internal/pull"
	"git.cloonar.com/Cloonar/coding-lab/internal/push"
	"git.cloonar.com/Cloonar/coding-lab/internal/readiness"
	"git.cloonar.com/Cloonar/coding-lab/internal/reconcile"
	"git.cloonar.com/Cloonar/coding-lab/internal/reposvc"
	"git.cloonar.com/Cloonar/coding-lab/internal/secrets"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/builtin"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/forgejo"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/github"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/secretscan"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
	"git.cloonar.com/Cloonar/coding-lab/internal/warpgate"
)

// version is stamped via -ldflags "-X main.version=…".
var version = "dev"

const usage = `lab — phone-first control panel for Claude Code agents

Usage: lab [flags]
       lab hash-password   read a password from stdin (or prompt, echo off) and print its argon2id PHC hash
       lab autoland rearm -repo <id> -pull <n> [-url <base>] [-token-file <path>]
                           return an escalated PR to the autoland poller's view, budgets restored
                           (operator PAT; LAB_URL / LAB_PAT / LAB_PAT_FILE)

Flags (env overrides in parentheses; flag > env > default):
  -addr string             listen address (LAB_ADDR; default ":8080")
  -state-dir string        state directory (LAB_STATE_DIR; default ~/.local/state/lab)
  -db string               sqlite:<path> or postgres://… (LAB_DB; default sqlite:<state-dir>/lab.db)
  -master-key-file string  vault master key file (LAB_MASTER_KEY_FILE; default <state-dir>/master.key)
  -vapid-key-file string   web push VAPID key file (LAB_VAPID_KEY_FILE; default <state-dir>/vapid.key)
  -seed-user string        initial operator user, reconciled at every startup (LAB_SEED_USER)
  -seed-password-hash-file path
                           file holding the seed user's PHC argon2id hash; one trailing newline stripped (LAB_SEED_PASSWORD_HASH_FILE; wins over -seed-password-hash)
  -seed-password-hash string
                           seed user's PHC argon2id hash inline, from lab hash-password (LAB_SEED_PASSWORD_HASH)
  -provider-bin id=path    per-provider agent binary, repeatable (LAB_PROVIDER_BIN_<ID>; adapter default: PATH lookup)
  -provider-config id=path per-provider config file, repeatable (LAB_PROVIDER_CONFIG_<ID>; adapter default, claude-code: ~/.claude.json)
  -tmux, -git, -prlimit string
                           binary paths (PATH lookup by default)
  -claude, -claude-config  deprecated aliases for the claude-code -provider-bin/-provider-config entries (LAB_CLAUDE_CONFIG likewise); the generic form wins
  -max-instances int       global live-instance cap; seeds the settings row on first start (default 6)
  -session-nofile int      RLIMIT_NOFILE for spawned sessions; 0 disables (default 16384)
  -proxy-auth              accept the proxy auth header from trusted proxies
  -proxy-auth-header string  header carrying the proxy-authenticated username (default "Remote-User")
  -trusted-proxies string  comma-separated CIDRs of trusted reverse proxies
  -base-url string         external base URL, e.g. https://lab.example.com (LAB_BASE_URL)
  -agent-url string        session-facing base URL handed to labctl as LAB_URL,
                           http(s) or unix:///abs/path; defaults to
                           unix://<state-dir>/agent/agent.sock (LAB_AGENT_URL)
  -container-image string  deployed fallback dev image for containerized sessions:
                           the last layer, after the repo's Dev image and the
                           global default dev image (Settings → Runner); none of
                           the three set refuses the spawn. Also the provider
                           login image (LAB_CONTAINER_IMAGE)
  -container-tools-image provider=ref[,provider=ref…]
                           agent-tools injection image per provider id,
                           @sha256-pinned per ADR-0051 (LAB_CONTAINER_TOOLS_IMAGE)
  -onecli-url string       OneCLI sidecar REST API base, e.g. http://127.0.0.1:10254;
                           set together with -onecli-api-key-file; unset leaves
                           the integration off (LAB_ONECLI_URL)
  -onecli-api-key-file string
                           file holding the OneCLI API key, 0600 or stricter;
                           never generated (LAB_ONECLI_API_KEY_FILE)
  -onecli-gateway-url string
                           OneCLI gateway proxy URL handed to runs as HTTPS_PROXY,
                           e.g. http://10.88.0.1:10255; independent of the pair
                           above (LAB_ONECLI_GATEWAY_URL)
  -onecli-ca-file string   path to the PEM file holding the OneCLI gateway's
                           interception CA certificate on the host; composed
                           into a run's trust bundle (LAB_ONECLI_CA_FILE)
  -onecli-dashboard string
                           OneCLI dashboard exposure: off (default, nothing
                           exposed), port (lab reverse-proxies it on its own
                           authenticated listener) or subdomain (your reverse
                           proxy fronts it and delegates auth to lab)
                           (LAB_ONECLI_DASHBOARD)
  -onecli-dashboard-addr string
                           listen address for -onecli-dashboard=port, e.g.
                           :8443 (LAB_ONECLI_DASHBOARD_ADDR)
  -onecli-dashboard-url string
                           browser-facing dashboard origin, e.g.
                           https://onecli.example.com; required for
                           -onecli-dashboard=subdomain, an optional override
                           in port mode (LAB_ONECLI_DASHBOARD_URL)
  -warpgate-url string     Warpgate admin API base URL, e.g. https://localhost:8888;
                           set together with -warpgate-admin-token-file; unset leaves
                           the integration off (LAB_WARPGATE_URL)
  -warpgate-admin-token-file string
                           file holding the Warpgate admin API token, 0600 or
                           stricter; never generated (LAB_WARPGATE_ADMIN_TOKEN_FILE)
  -warpgate-ssh-addr string  host:port a run uses to reach Warpgate's SSH listener,
                           e.g. 10.88.0.1:2222; independent of the pair above
                           (LAB_WARPGATE_SSH_ADDR)
  -warpgate-ca-file string  path to the PEM file holding the certificate or CA lab
                           must trust for the admin API; unset uses the system roots
                           (LAB_WARPGATE_CA_FILE)
  -warpgate-ssh-host-key string
                           trusted public host key of Warpgate's SSH listener,
                           e.g. "ssh-ed25519 AAAA…" (ssh-keyscan output works too;
                           several keys separated by newlines or commas); unset
                           accepts every key the listener presents; requires
                           -warpgate-ssh-addr (LAB_WARPGATE_SSH_HOST_KEY)
  -session-cookie-domain string
                           Domain attribute for lab's session cookie, e.g.
                           example.com; empty (default) keeps the cookie
                           host-only (LAB_SESSION_COOKIE_DOMAIN)
`

func main() {
	// The first subcommand on cmd/lab (issue #137). Dispatched on the literal
	// first arg, BEFORE config.Parse (inside run()) ever sees os.Args, so it
	// never collides with the server's own flag parsing and never needs a DB,
	// vault, or any of the rest of the server bootstrap.
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		os.Exit(runHashPassword(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	// `lab autoland rearm` (issue #188) joins it on exactly the same terms: an
	// operator-side verb that talks to a RUNNING lab over its human-
	// authenticated HTTP API, so it must never touch config.Parse, a DB, a
	// vault, or the server bootstrap below. Dispatching here, on the literal
	// first arg, is what guarantees that. It lives on cmd/lab rather than
	// labctl on purpose — see autoland.go's file comment; the placement is the
	// security boundary that keeps re-arm out of the run-token surface.
	if len(os.Args) > 1 && os.Args[1] == "autoland" {
		os.Exit(runAutoland(os.Args[2:], os.Getenv, os.Stdout, os.Stderr))
	}
	os.Exit(run())
}

func run() int {
	// cmd/lab is the one place that names the registered providers: the
	// providerIDs list validates the generic -provider-bin/-provider-config
	// flags (issue #78 / ADR-0034). A future provider adds its ID here
	// alongside its adapter construction below.
	cfg, err := config.Parse(os.Args[1:], os.Getenv, []string{claudecode.ID, codex.ID})
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

	// Reconcile the initial operator user (issue #137) before the listener
	// opens so a declarative deploy never shows the setup page; store.Open
	// already ran migrations. This runs on EVERY boot, not just the first:
	// on an empty DB it creates the seed user, on a DB that already has it
	// the stored hash is rewritten when config changed (a no-op otherwise),
	// and if the DB has other users but not the configured seed user it
	// refuses to start rather than silently create a second account or leave
	// a configured credential permanently dead.
	if err := seedInitialUser(ctx, st, cfg, logger); err != nil {
		logger.Error("seeding initial user", "component", "main", "err", err)
		return 1
	}

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
	// OneCLI credential gateway (issue #23 / ADR-0067): the REST client lab
	// itself speaks to the sidecar with, built beside the vault because it is
	// the same kind of thing — a credential read at startup, from a file the
	// operator provisions, that decides what lab may do for the rest of the
	// process's life.
	//
	// The integration is entirely OFF when unconfigured. --onecli-url and
	// --onecli-api-key-file are enforced as a pair by config.Parse, so an empty
	// URL means the operator asked for nothing: the client stays nil, the key
	// file is never opened, and every OneCLI-aware surface reports "off" rather
	// than an error (ADR-0067: an unconfigured lab is not an unhealthy lab).
	//
	// When it IS configured both failures below are FATAL, in the same shape as
	// the master key above, and that is the point of the file contract rather
	// than an accident of style. LoadAPIKey is a verbatim mirror of vault.Load's
	// master-key-file rule (ADR-0006): a OneCLI project key is authority over
	// every credential in the project, so a key file readable by group or other
	// is a silent, permanent compromise — degrading to "OneCLI off" would leave
	// that file exactly as exposed while hiding the reason, and an operator who
	// deployed the flags would learn about it from a health page instead of a
	// failed start. New's own refusals (unparseable base URL, a key with an
	// embedded newline) are fatal for the mirror-image reason: they must not
	// resurface as a confusing 4xx during a spawn hours later.
	//
	// Nothing is logged on success, deliberately: a startup line echoing the
	// configured URLs would be handy, but a proxy URL may legitimately carry
	// userinfo credentials and this file has no business deciding how to redact
	// one. GET /api/v1/onecli/health is the visibility surface, and it redacts
	// (internal/httpapi/onecli.go).
	var oneCLIClient *onecli.Client
	if cfg.OneCLIURL != "" {
		apiKey, keyErr := onecli.LoadAPIKey(cfg.OneCLIAPIKeyFile)
		if keyErr != nil {
			logger.Error("onecli api key", "component", "main", "err", keyErr)
			return 1
		}
		oneCLIClient, err = onecli.New(onecli.Options{BaseURL: cfg.OneCLIURL, APIKey: apiKey})
		if err != nil {
			logger.Error("building onecli client", "component", "main", "err", err)
			return 1
		}
	}
	// The same client as the instance service's gateway seam (issue #24), and
	// it is declared as the INTERFACE with an explicit nil-pointer guard for a
	// reason worth stating: assigning a nil *onecli.Client straight into an
	// interface-typed field yields a NON-nil interface holding a nil pointer.
	// instance.gatewayActive() would then read "gateway configured" on a lab
	// that configured nothing, and every spawn on that lab would refuse — the
	// exact opposite of ADR-0067's "off when unset". Do not collapse this back
	// into a one-line field assignment.
	var oneCLIGateway instance.GatewayAPI
	if oneCLIClient != nil {
		oneCLIGateway = oneCLIClient
	}
	// The same client again as the repo lifecycle's agent seam (issue #35: an
	// agent is created with its repo, converged at startup and deleted with it),
	// and guarded the same way for the reason spelled out just above — a nil
	// *onecli.Client in an interface field is a NON-nil interface, which would
	// make reposvc.oneCLIActive() true on a lab that configured no OneCLI and
	// turn three silent no-ops into a warning per repo create, boot and delete.
	var oneCLIAgents reposvc.OneCLIAgents
	if oneCLIClient != nil {
		oneCLIAgents = oneCLIClient
	}

	// Warpgate SSH bastion (issue #39 / ADR-0068): the admin REST client and
	// the host-key pin, on the OneCLI pattern above. Off when unconfigured:
	// --warpgate-url and --warpgate-admin-token-file are a pair config.Parse
	// enforces, so an empty URL means no token file is ever opened. A token
	// file that is unreadable, loose (0600 rule, shared with the OneCLI key and
	// the master key via fsx.ReadSecretFile) or empty, and a --warpgate-ca-file
	// that holds no certificate, are fatal for the same reason the OneCLI key
	// is: degrading to "Warpgate off" would silently drop every repo's SSH
	// targets.
	//
	// The host-key pin exists whenever --warpgate-ssh-addr is set, REST pair
	// or not: it needs only the address and the optional
	// --warpgate-ssh-host-key (config stores it canonical, so a parse failure
	// here is a bug, not operator input), and health reports its state
	// either way. Runs are wired only when all three are set, which the
	// instance service decides (bastionActive).
	var warpgateClient *warpgate.Client
	if cfg.WarpgateURL != "" {
		token, tokenErr := warpgate.LoadAdminToken(cfg.WarpgateAdminTokenFile)
		if tokenErr != nil {
			logger.Error("warpgate admin token", "component", "main", "err", tokenErr)
			return 1
		}
		warpgateClient, err = warpgate.New(warpgate.Options{BaseURL: cfg.WarpgateURL, Token: token, CAFile: cfg.WarpgateCAFile})
		if err != nil {
			logger.Error("building warpgate client", "component", "main", "err", err)
			return 1
		}
	}
	var warpgateHostKeys *warpgate.HostKeyPin
	if cfg.WarpgateSSHAddr != "" {
		trusted, keyErr := warpgate.ParseAuthorizedKeys(cfg.WarpgateSSHHostKey)
		if keyErr != nil {
			logger.Error("warpgate ssh host key", "component", "main", "err", keyErr)
			return 1
		}
		warpgateHostKeys = warpgate.NewHostKeyPin(cfg.WarpgateSSHAddr, trusted, nil)
	}
	// The same nil-pointer guard as the OneCLI seams above, once per consumer
	// interface: a nil *warpgate.Client or *warpgate.HostKeyPin assigned
	// straight into an interface field is a NON-nil interface, which would
	// read "configured" everywhere on a lab that configured nothing.
	var (
		warpgateBastion    instance.BastionAPI
		warpgateIdentities reposvc.WarpgateIdentities
		warpgateHTTP       httpapi.WarpgateAPI
	)
	if warpgateClient != nil {
		warpgateBastion = warpgateClient
		warpgateIdentities = warpgateClient
		warpgateHTTP = warpgateClient
	}
	var (
		warpgateSpawnHostKeys instance.BastionHostKeys
		warpgateHTTPHostKeys  httpapi.WarpgateHostKeyPin
	)
	if warpgateHostKeys != nil {
		warpgateSpawnHostKeys = warpgateHostKeys
		warpgateHTTPHostKeys = warpgateHostKeys
	}
	// Per-run private HOME lifecycle (issue #202): <state>/instances holds one
	// private HOME per run — the isolation seam a run's provider credential copy,
	// config, and transcripts live under. New does no I/O (the dirs are created
	// lazily at launch), so it is wired unconditionally beside the vault
	// materializer and shared by the instance/afk/reconcile services (Materialize
	// at launch, Wipe at stop/rollback, SweepAll at boot/runtime) and the
	// chat/httpapi read paths (the pure HomePath).
	homes := instancehome.New(filepath.Join(cfg.StateDir, "instances"))
	// Retained transcripts (issue #81): <state>/transcripts holds one 0700
	// <runID>/ dir per ended run whose provider-native transcript the pre-wipe
	// retain step moved out of its HOME, kept for transcript_retention_days
	// and expired by the reconcile sweeps. Disposable (outside the backup
	// set) and created lazily by the first retain, like <state>/instances.
	transcriptsDir := filepath.Join(cfg.StateDir, "transcripts")

	// Web push (issue #98): load-or-generate the VAPID keypair with the same
	// first-start bootstrap and key-file contract as the master key, then wire
	// it straight into the sender. The log line stays — it's the operator's
	// own copy of the non-secret public key, handy without hitting the API.
	vapidKey, err := loadOrGenerateVAPIDKey(cfg.VAPIDKeyFile, logger)
	if err != nil {
		logger.Error("vapid key", "component", "main", "err", err)
		return 1
	}
	logger.Info("web push vapid key loaded", "component", "main", "path", cfg.VAPIDKeyFile, "public_key", vapidKey.PublicKeyB64())
	// Fire-and-forget by design (internal/push/sender.go): no Flush is wired
	// into shutdown below. A Flush could block graceful shutdown for up to
	// sendTimeout (30s) on an airgapped or unreachable gateway — worse than
	// dropping whatever sends are still in flight when the process exits.
	//
	// Presence-based suppression (issue #160): one in-memory registry, fed by
	// the SSE handler + presence beacon (httpapi) and read by Broadcast, so a
	// device with the app visible is skipped at send time. In-memory on
	// purpose — a restart empties it and everyone is notified again.
	presenceReg := presence.NewRegistry()
	pushSender := push.NewSender(st, vapidKey, presenceReg, logger)

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

	// The readiness recorder (issue #61): the one memory of what lab's own
	// operations last observed. The readiness report and the repo summaries
	// are built from it, so a page view never asks a forge, a git remote, a
	// provider CLI or podman. It has exactly four feeds, each wired at the
	// seam where the operation already runs:
	//   - every list read of a forge-bound repo's tracker (here);
	//   - every credentialed fetch, and every completed clone, of the ONE git
	//     engine all services share (below);
	//   - each spawn-time pull-if-missing of a dev image (the instance
	//     service's ImageEnsured);
	//   - each computed claimable count (the AFK engine's OnClaimable).
	// It publishes repo.changed when a recorded verdict flips or a count
	// changes — never per read — for the repo the outcome concerns and for
	// every repo whose report reads the same record: the importers of a
	// fetched repo, and the container-Runner repos for a dev image record
	// (keyed by ref, shared by every repo resolving to it). In memory only: a
	// restart forgets it, and a check with no record is left out of the
	// report rather than guessed.
	readinessRec := readiness.NewRecorder(bus, nil)
	readinessRec.SetFanout(readinessFanout(st))
	trackerReg.SetReadObserver(readinessRec.ObserveTrackerRead)

	gitEngine := gitx.New(cfg.GitBin)
	// Only a fetch attributed to a repo's own git credential is reported
	// (gitx.AttributeFetch). The reconcile sweep's credential-less fetch,
	// which fails on every private remote by design, never reaches this.
	gitEngine.SetFetchObserver(readinessRec.ObserveFetch)
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

	// /pull-base lab command service (issue #149): merges origin/<base> into a
	// run's LIVE worktree, re-materializes its read-only import snapshots
	// (issue #261), and renders the agent-facing digest; the HTTP reply path
	// intercepts the command and delegates here. Same store/git/vault/
	// materializer/bus plumbing as crmerge — the two are siblings on the bare
	// reference clones.
	pullSvc := pull.New(pull.Options{
		Store:        st,
		Git:          gitEngine,
		Vault:        vlt,
		Materializer: mat,
		Bus:          bus,
		ReposDir:     reposDir,
		Logger:       logger,
		// Plus the instancehome Manager (issue #261): /pull-base is the only
		// refresh for a run's read-only import snapshots, and this is what
		// locates them.
		Homes: homes,
	})

	// Container runner preflight (issue #205): with any container config
	// present, verify the host in a startup GOROUTINE — an unresolved tools
	// ref means an image pull, possibly minutes, and boot must not block on
	// it — and publish the verdict through an atomic gate. Container spawns
	// consult the gate per launch: refused with "retry in a moment" until the
	// verdict lands, with the full failure list if the host cannot serve, and
	// allowed once OK — no restart needed for the unblock. containerPreflight
	// stays nil when no container config exists: that nil IS the "container
	// mode structurally unavailable" signal for instance/afk/reconcile, and
	// it also disables their podman-rm backstops on host-only deployments.
	//
	// Retryable failures are RETRIED (issue #220, ADR-0060): most preflight
	// checks report host state only a redeploy (and thus a fresh boot)
	// changes, but two classes heal with no host change at all. A tools-image
	// pull: deploy.yml refuses to bump the host pin until the default tools
	// refs resolve, but a registry blip or a workflow_dispatch re-release can
	// still leave a startup pull that only time heals. And an unreachable
	// user manager: logind brings user@<uid>.service up asynchronously for a
	// lingering user, and lab cannot order on it (the uid is unknown at nix
	// eval time), so a just-booted host may briefly fail the check and then
	// heal itself. So the loop re-runs preflight while a retryable failure is
	// present and republishes through the gate: container spawns unblock the
	// moment the registry serves the ref (or the manager comes up), no
	// restart needed. Repeat verdicts log only when the failure set changes;
	// a steady-state outage stays one warning, not one per minute.
	var containerPreflight func() (podmanx.Result, bool)
	if cfg.ContainerImage != "" || len(cfg.ContainerToolsImages) > 0 {
		// lab OWNS the container runtime env (ADR-0060): rootless podman's
		// systemd cgroup manager reaches the lab user's user@<uid>.service
		// manager through XDG_RUNTIME_DIR and the user-bus address, and the
		// uid is unknown at nix eval time, so the service unit cannot carry
		// them. Set process-wide, they reach all three consumers at once:
		// lab's own podman execs (ExecRunner/execStreams leave cmd.Env nil →
		// process env), the tmux server (tmuxx's baseline filters
		// os.Environ()), and every pane (the server global env, re-pinned at
		// adoption — see tmuxx.syncGlobalEnv). Deliberately overwrites
		// anything inherited: a stale /run/lab value from the retired
		// ADR-0057 layout must lose. Podman's runroot/tmpdir follow this env
		// to /run/user/<uid>, whose lifetime linger owns — up from boot,
		// independent of lab.service.
		rd := fmt.Sprintf("/run/user/%d", os.Getuid())
		if err := os.Setenv("XDG_RUNTIME_DIR", rd); err != nil {
			logger.Error("setting XDG_RUNTIME_DIR", "component", "main", "err", err)
			return 1
		}
		if err := os.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+rd+"/bus"); err != nil {
			logger.Error("setting DBUS_SESSION_BUS_ADDRESS", "component", "main", "err", err)
			return 1
		}
		gate := &podmanx.Gate{}
		containerPreflight = gate.Result
		go func() {
			const retryEvery = time.Minute
			var prev []podmanx.Failure
			for {
				res := podmanx.Preflight(ctx, podmanx.PreflightConfig{
					PodmanBin:   cfg.PodmanBin,
					ToolsImages: cfg.ContainerToolsImages,
				}, podmanx.RealDeps())
				gate.Set(res)
				// A verdict just landed or changed, and with it the dev image
				// check of every container-Runner repo (issue #61: pending
				// until now, or failing with another failure set). Nothing
				// else would tell an open page, so announce those repos —
				// once per CHANGED verdict, like the logging below.
				if prev == nil || !slices.Equal(res.Failures, prev) {
					announceContainerRepos(ctx, st, readinessRec)
				}
				if res.OK() {
					// Warnings ride an OK verdict (e.g. running on a cached
					// tools image because the registry was unreachable) —
					// spawns proceed, the operator should still know.
					for _, w := range res.Warnings {
						logger.Warn("container preflight warning", "component", "main", "warning", w)
					}
					logger.Info("container preflight passed", "component", "main", "podman_version", res.Version)
					return
				}
				// Every failure logged individually — the operator fixes the
				// host once, not one restart per failure (podmanx.Preflight
				// collects them all for the same reason).
				if !slices.Equal(res.Failures, prev) {
					for _, w := range res.Warnings {
						logger.Warn("container preflight warning", "component", "main", "warning", w)
					}
					for _, f := range res.Failures {
						logger.Warn("container preflight failed", "component", "main",
							"check", f.Check, "detail", f.Detail, "hint", f.Hint)
					}
					prev = res.Failures
				}
				if !res.HasRetryableFailure() {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(retryEvery):
				}
			}
		}()
	}

	// M3 instance/AFK stack. The claude-code adapter derives its default global
	// config path from HOME (issue #78 / ADR-0034), so with HOME unset AND no
	// explicit -provider-config claude-code=… entry there is nothing to hand it:
	// the instance/parked/provider routes stay unmounted and lab still serves
	// the M2 surface.
	var (
		instanceSvc   *instance.Service
		reconcileSvc  *reconcile.Service
		providerReg   *provider.Registry
		afkSvc        *afk.Service
		chatSvc       *chat.Service
		credrotateSvc *credrotate.Service
	)
	home := os.Getenv("HOME")
	if home == "" && cfg.ProviderConfig[claudecode.ID] == "" {
		logger.Warn("claude config path unresolved (HOME unset and no -provider-config claude-code=…); instance features disabled",
			"component", "main")
	} else {
		runner := tmuxx.New(cfg.TmuxBin, tmuxx.WithNofileCap(cfg.PrlimitBin, cfg.SessionNofile))
		// Containerized provider login + CLI surface (issue #206 / ADR-0057):
		// with container config present, each adapter's login pane and
		// non-interactive CLI invocations run in containers against its master
		// store — never a host-CLI fallback (providerCLIConfigs documents the
		// per-provider Config, and why its dev image is the flag image alone).
		// Without container config the adapters keep the raw runner and a nil
		// CLI (→ provider.HostCLI inside New): host-mode login stays
		// byte-for-byte unchanged. The instance/reconcile/afk services below
		// always get the raw runner — the run-spawn seam is their own podman
		// handling, untouched here.
		claudeRunner := tmuxx.SessionRunner(runner)
		codexRunner := tmuxx.SessionRunner(runner)
		var claudeCLI, codexCLI provider.CLIRunner
		if containerPreflight != nil {
			claudeCfg, codexCfg := providerCLIConfigs(cfg, st, containerPreflight, home, logger)
			claudeRunner = providercli.NewLoginRunner(runner, claudeCfg)
			claudeCLI = providercli.NewContainerCLI(claudeCfg)
			codexRunner = providercli.NewLoginRunner(runner, codexCfg)
			codexCLI = providercli.NewContainerCLI(codexCfg)
		}
		claudeProvider, perr := claudecode.New(claudecode.Options{
			ClaudeBin:  cfg.ProviderBin[claudecode.ID],
			ConfigPath: cfg.ProviderConfig[claudecode.ID],
			LoginDir:   home,
			Runner:     claudeRunner,
			CLI:        claudeCLI,
			Bus:        bus,
			Logger:     logger,
		})
		if perr != nil {
			logger.Error("building claude provider", "component", "main", "err", perr)
			return 1
		}
		// The codex adapter (issue #87) shares the runner/bus and derives its
		// own path defaults from $CODEX_HOME / HOME/.codex (issue #78:
		// adapter-owned defaults), so only the generic -provider-bin/-config
		// overrides are threaded through.
		codexProvider, perr := codex.New(codex.Options{
			CodexBin:   cfg.ProviderBin[codex.ID],
			ConfigPath: cfg.ProviderConfig[codex.ID],
			LoginDir:   home,
			Runner:     codexRunner,
			CLI:        codexCLI,
			Bus:        bus,
			Logger:     logger,
		})
		if perr != nil {
			logger.Error("building codex provider", "component", "main", "err", perr)
			return 1
		}
		providerReg, err = provider.NewRegistry(claudeProvider, codexProvider)
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
			Homes:        homes,
			Guard:        guard,
			Bus:          bus,
			Logger:       logger,
			ReposDir:     reposDir,
			WorktreeRoot: worktreeRoot,
			LabURL:       labURL(cfg),
			CaptureCtx:   ctx,
			// The pre-wipe retain step's root (issue #81); the step itself is
			// installed on the homes' pre-wipe chain below.
			TranscriptsDir: transcriptsDir,
			// Container runner wiring (issue #205): the spawn seam that turns a
			// Runner=container repo's pane command into `podman run`, gated on
			// the preflight verdict above (nil = structurally unavailable).
			PodmanBin:            cfg.PodmanBin,
			ContainerImage:       cfg.ContainerImage,
			ContainerToolsImages: cfg.ContainerToolsImages,
			ContainerPreflight:   containerPreflight,
			AgentSockDir:         agentapi.SocketDir(cfg.StateDir),
			// The readiness recorder learns whether a dev image is present
			// from the spawn that pulled it (issue #61) — the only place
			// image presence is ever checked.
			ImageEnsured: readinessRec.ObserveImage,
			// OneCLI credential-gateway run wiring (issue #24 / ADR-0067): the
			// pre-claim fail-closed precheck, the per-run trust bundle, and the
			// proxy env bundle every run kind gets. All three are zero when the
			// integration is unconfigured, which leaves the launch path exactly
			// as it was.
			OneCLI:           oneCLIGateway,
			OneCLIGatewayURL: cfg.OneCLIGatewayURL,
			OneCLICAFile:     cfg.OneCLICAFile,
			// Warpgate SSH-bastion run wiring (issue #39 / ADR-0068): a
			// target-bearing spawn registers a per-run key, writes the run's
			// ssh config, known_hosts and ssh/scp/sftp wrappers, and refuses
			// before the claim when Warpgate or its host key cannot be
			// verified. All three must be set for a run to be wired; with any
			// of them zero every spawn is unchanged.
			Warpgate:         warpgateBastion,
			WarpgateSSHAddr:  cfg.WarpgateSSHAddr,
			WarpgateHostKeys: warpgateSpawnHostKeys,
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
			Homes:        homes,
			Bus:          bus,
			Logger:       logger,
			ReposDir:     reposDir,
			ArmCapture:   instanceSvc.ArmCapture,
			AFKRunEnded:  m.AFKRunEnded,
			// The transcript expiry step's root (issue #81) — the same dir the
			// instance service retains into.
			TranscriptsDir: transcriptsDir,
			// Container backstops (issue #205): the Discard kill's podman rm
			// and the startup orphaned-container sweep.
			PodmanBin:          cfg.PodmanBin,
			ContainerPreflight: containerPreflight,
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
			Homes:        homes,
			Bus:          bus,
			Guard:        guard,
			Logger:       logger,
			ReposDir:     reposDir,
			WorktreeRoot: worktreeRoot,
			Sweep:        reconcileSvc.RuntimeSweep,
			Metrics:      m,
			// The repo list's claimable count is whatever the engine or an
			// operator view last computed (issue #61) — remembered here, so
			// the list costs no forge request per repo.
			OnClaimable: readinessRec.ObserveClaimable,
			// Container backstops (issue #205): podman rm behind the engine's
			// session kills (neutral Stop, reap, zombie drain).
			PodmanBin:          cfg.PodmanBin,
			ContainerPreflight: containerPreflight,
			// Web push on the reaper's done-signal (issue #100): a closure over
			// the push sender so afk never imports push. Broadcast is
			// async/fire-and-forget, so the reaper never blocks on gateway I/O.
			Notify: func(n afk.Notification) {
				pushSender.Broadcast(push.Payload{Title: n.Title, Body: n.Body, Tag: n.Tag, Route: n.Route})
			},
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
			Store:     st,
			Providers: providerReg,
			Bus:       bus,
			Logger:    logger,
			Ctx:       ctx,
			// A run's live-signal spools live in its PRIVATE runtime dir
			// (issue #205): RuntimePath is pure, so it is handed straight in
			// as the closure the tailer/read paths resolve per run — the
			// same idiom as HomeFor below. The global runtime dir no longer
			// carries any run-scoped file.
			RuntimeDirFor: homes.RuntimePath,
			// Web push on the needs-input/question edge (issue #99): a closure
			// over the push sender so chat never imports push. Broadcast is
			// async/fire-and-forget, so the tailer's tick loop never blocks on
			// gateway I/O.
			Notify: func(n chat.Notification) {
				pushSender.Broadcast(push.Payload{Title: n.Title, Body: n.Body, Tag: n.Tag, Route: n.Route})
			},
			// Transcript exposure detection (issue #108): a closure over the
			// secrets source so chat builds per-repo redactors without ever
			// touching the vault or an encrypted blob itself. The vault always
			// exists here (main bails before this point when vault.New fails),
			// so the seam is wired unconditionally; a repo with no secrets
			// still short-circuits inside the Source (nil redactor).
			Secrets: (&secrets.Source{Values: st.AllRepoSecretValues, Decrypt: vlt.Decrypt}).Redactor,
			// A run's transcript/user-commands resolve strictly under its private
			// instance HOME (issue #202): HomePath is pure, so it is handed straight
			// in as the closure the chat seams thread through LocateTranscript.
			HomeFor: homes.HomePath,
		})
		if err != nil {
			logger.Error("building chat service", "component", "main", "err", err)
			return 1
		}
		instanceSvc.SetChatState(chatSvc)

		// Credential rotation loop (issue #222): the single refresher per
		// provider grant. It runs against each provider's MASTER store, fans
		// rotations out to every live instance's private HOME, and adopts back
		// any instance that self-refreshed — so a per-run OAuth snapshot can
		// never fork the token family and log the host out. Built here beside
		// the other per-run-home services because it needs the same providerReg
		// and instancehome Manager; its Loop is started with the other loops
		// below. AdoptCheck is wired into the wipe paths directly below.
		credrotateSvc, err = credrotate.New(credrotate.Options{
			Providers: providerReg,
			Store:     st,
			Homes:     homes,
			Logger:    logger,
		})
		if err != nil {
			logger.Error("building credrotate service", "component", "main", "err", err)
			return 1
		}
		// Wire the pre-wipe adopt-check (issue #222 decision 4: adopt-check
		// before every wipe) into instancehome.Manager. EVERY wipe path in the
		// repo funnels through Manager.Wipe or Manager.SweepAll — the
		// instancehome package doc lists the full call-site set (stop,
		// rollback, afk stop, the reaper, the parked sweep, the startup and
		// throttled orphan sweeps) — so this single hook makes the decision
		// total across all of them. Installed HERE, before StartupReconcile
		// runs below, so the startup sweep of orphan homes left behind by a
		// lab restart is covered too — closing the restart-after-downtime race
		// issue #222 exists to fix. ctx is the process signal context: during
		// shutdown AdoptCheck degrades to a fast no-op (its store lookup fails
		// and it returns immediately, best-effort by design).
		//
		// The same single hook revokes the run's Warpgate key (issue #39 /
		// ADR-0068): every wipe path is exactly where a run's SSH access must
		// end, so the run key rides the adopt-check's totality instead of a
		// second list of call sites. RevokeBastionKey reads the run's marker
		// from the runtime dir the wipe is about to remove, is a no-op on a lab
		// without Warpgate or a run that was never wired, and detaches from ctx
		// so shutdown cannot strand a key.
		//
		// Since issue #81 the seam is a CHAIN, run in registration order, and
		// the transcript retain step rides it as the second hook (decision 5):
		// moving the run's provider-native transcript out of HOME into
		// <state>/transcripts/<runID>/ is exactly the "one last look before the
		// tree is gone" this seam exists for, so all six wipe sites — Stop, AFK
		// stop, the reaper, the parked discard, launch rollback, and the orphan
		// sweeps, the startup one after downtime included — retain with no
		// per-site code. Ordered AFTER the adopt-check (and the key revocation)
		// so a slow cross-device copy never delays adopting a self-refreshed
		// credential family or revoking live SSH access. Like the hook above it
		// must be installed before StartupReconcile, and like RevokeBastionKey
		// it detaches from ctx, so a wipe during shutdown still retains.
		homes.AddPreWipeHook(func(runID string) {
			credrotateSvc.AdoptCheck(ctx, runID)
			instanceSvc.RevokeBastionKey(ctx, runID)
		})
		homes.AddPreWipeHook(func(runID string) { instanceSvc.RetainTranscript(ctx, runID) })

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
	// Secret-leak guard (issue #107): the agent surface — and ONLY the agent
	// surface — resolves its trackers through secretscan.NewResolver, so every
	// run-token PR/issue/comment create is scanned against the repo's own secret
	// values and rejected (400, naming the secret) before it can reach the
	// forge. This wrapping HERE is the whole run-token-only property: the
	// operator API keeps the bare Config{Tracker: trackerReg} above, and the AFK
	// engine keeps afk.Options{Trackers: trackerReg}, so operator writes and
	// internal reaper reads never pay the scan. And because the wrap sits above
	// the registry — the one seam both bindings resolve through — a single
	// decorator covers the forge and builtin bindings identically.
	agent := agentapi.New(st, vlt, secretscan.NewResolver(trackerReg, st, vlt), bus, logger, time.Now, scrub)

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
		// the degraded boot where no provider is configured (HOME unset and no
		// provider config entry resolved above), which renders a content-inert
		// guard.
		Providers: providerReg,
		// PinImageRef digest-pins a repo's dev image ref on save (issue #207 /
		// ADR-0053). The zero-value Resolver's nil Client means the package's
		// https-only default client — that exact construction is the production
		// wiring the imageref package documents.
		PinImageRef: (&imageref.Resolver{}).Pin,
		// OneCLI makes repo create/startup/delete the touchpoints that keep each
		// repo's credential-gateway agent in step with its row (issue #35). Nil
		// on a lab with no --onecli-url, where all three are silent no-ops.
		OneCLI: oneCLIAgents,
		// Warpgate gives each repo its SSH-bastion user and role on the same
		// three touchpoints, and startup also refreshes the repo_ssh_targets
		// cache the spawn path reads (issue #39 / ADR-0068). Nil without
		// --warpgate-url.
		Warpgate: warpgateIdentities,
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
	// Warpgate startup passes (issue #39 / ADR-0068), both off the boot path:
	// a bastion that is slow to come up must not delay serving.
	//   - The orphan run-key sweep runs AFTER StartupReconcile, so re-adopted
	//     runs count as live and orphan trees have already been wiped (and
	//     their keys revoked) through the pre-wipe hook. It is safe beside
	//     spawns that start while it runs: a key whose run's tree is still on
	//     disk is skipped. A pass cut short — typically a Warpgate still
	//     starting beside lab — is retried with backoff (bastionSweepRetry)
	//     until one completes or lab shuts down; each failed pass already
	//     logged its one warning, so the retries add none of their own.
	//   - The host-key check compares the listener's keys to
	//     --warpgate-ssh-host-key, here at startup rather than at the first
	//     target-bearing spawn, and logs a mismatch loudly; with no trusted
	//     key configured it logs the observed fingerprints once, so the
	//     setting can be filled in from them. Health shows the same state.
	if instanceSvc != nil {
		go func() {
			attempts, done := retryUntilComplete(ctx, instanceSvc.SweepBastionKeys,
				bastionSweepRetryFirst, bastionSweepRetryMax, waitCtx)
			if done && attempts > 1 {
				logger.Info("warpgate orphan run-key sweep completed after retries", "component", "main", "attempts", attempts)
			}
		}()
	}
	if warpgateHostKeys != nil {
		go func() {
			hk := warpgateHostKeys.Check(ctx)
			switch hk.State {
			case warpgate.HostKeyPinned:
				logger.Info("warpgate ssh host key matches --warpgate-ssh-host-key", "component", "main", "addr", cfg.WarpgateSSHAddr, "trusted", hk.Pinned, "observed", hk.Observed)
			case warpgate.HostKeyUnpinned:
				logger.Info("no trusted warpgate ssh host key configured; runs trust whatever the listener presents — set --warpgate-ssh-host-key to pin one of the observed keys",
					"component", "main", "addr", cfg.WarpgateSSHAddr, "observed", hk.Observed)
			case warpgate.HostKeyMismatch:
				logger.Warn("warpgate ssh host key does not match --warpgate-ssh-host-key; target-bearing spawns are refused until the setting names a key the listener presents",
					"component", "main", "addr", cfg.WarpgateSSHAddr, "trusted", hk.Pinned, "observed", hk.Observed)
			default:
				logger.Warn("warpgate ssh host key could not be checked at startup", "component", "main",
					"addr", cfg.WarpgateSSHAddr, "state", hk.State, "err", hk.Error)
			}
		}()
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
		Homes:           homes,
		Tracker:         trackerReg,
		AFK:             afkSvc,
		Readiness:       readinessRec,
		Push:            pushSender,
		Presence:        presenceReg,
		Git:             gitEngine,
		Materializer:    mat,
		ReposDir:        reposDir,
		CRMerge:         mergeSvc,
		Pull:            pullSvc,
		BaseURL:         cfg.BaseURL,
		ProxyAuth:       cfg.ProxyAuth,
		ProxyAuthHeader: cfg.ProxyAuthHeader,
		TrustedProxies:  cfg.TrustedProxies,
		AgentHandler:    agent.Handler(),
		// The deployed fallback dev image (issue #55 / ADR-0071), reported
		// read-only in the settings response as dev_image_fallback.
		DevImageFallback: cfg.ContainerImage,
		// The session cookie's Domain (issue #26): "" in every deployment but
		// --onecli-dashboard=subdomain, which needs the parent domain so lab's
		// session reaches onecli.<domain> and forward-auth can see it.
		SessionCookieDomain: cfg.SessionCookieDomain,
		// OneCLI health visibility (issue #23 / ADR-0067). All three are
		// zero when the integration is unconfigured, and the health endpoint
		// mounts anyway to say so — see internal/httpapi/onecli.go.
		OneCLI:           oneCLIClient,
		OneCLIAPIURL:     cfg.OneCLIURL,
		OneCLIGatewayURL: cfg.OneCLIGatewayURL,
		// The dashboard exposure (issue #26). config.Parse already validated
		// the mode word and its companions; httpapi resolves them once into the
		// browser-facing URL its exposure endpoint reports, and refuses to
		// build on any combination config would not have produced.
		OneCLIDashboardMode: cfg.OneCLIDashboard,
		OneCLIDashboardAddr: cfg.OneCLIDashboardAddr,
		OneCLIDashboardURL:  cfg.OneCLIDashboardURL,
		// Warpgate SSH-bastion health and the per-repo SSH target picker
		// (issue #39 / ADR-0068). All zero when unconfigured;
		// the routes mount anyway and say so.
		Warpgate:         warpgateHTTP,
		WarpgateAPIURL:   cfg.WarpgateURL,
		WarpgateSSHAddr:  cfg.WarpgateSSHAddr,
		WarpgateHostKeys: warpgateHTTPHostKeys,
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
	// The reconcile-owned dead-session sweep (issue #93): ends active manual
	// runs whose tmux session is gone — the runtime sibling of readopt. Its own
	// short tick, not the throttled sweep_interval_minutes cadence, so a dead
	// run flips to ended within seconds. AFK kinds stay the reaper's.
	if reconcileSvc != nil {
		go reconcileSvc.DeadSessionLoop(ctx)
	}
	// The chat tailer: one goroutine that keeps a per-live-instance transcript
	// tailer set in sync with the active runs and publishes run.messages.changed.
	if chatSvc != nil {
		go chatSvc.Run(ctx)
	}
	// The credential rotation loop (issue #222): one goroutine that keeps every
	// provider's master credential family live and fanned out across the fleet —
	// the single refresher that stops per-run OAuth snapshots from forking and
	// logging the host out. Its own scan cadence, stopped when ctx is cancelled.
	if credrotateSvc != nil {
		go credrotateSvc.Loop(ctx)
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

	// The agent unix socket (issue #201): the SAME run-token-authenticated
	// handler the TCP listener mounts under /agent/v1, served on a second
	// listener at <state-dir>/agent/agent.sock (its own mountable dir since
	// issue #205 — see agentapi.SocketDir). It is the default LAB_URL
	// transport (see labURL), so session traffic never hairpins through
	// whatever proxy fronts the TCP address. No CloseStreams hook: the agent
	// API has no SSE.
	sock := agentapi.SocketPath(cfg.StateDir)
	agentLn, err := agentapi.ListenSocket(sock)
	if err != nil {
		logger.Error("listening on agent socket", "component", "main", "path", sock, "err", err)
		return 1
	}
	// Back-compat symlink at the pre-#205 path <state>/agent.sock: sessions
	// spawned by an older server carry that path in LAB_URL and outlive the
	// upgrade (tmux survives restarts). A warn, never fatal — only those
	// pre-upgrade sessions depend on it.
	if err := agentapi.LegacySocketSymlink(cfg.StateDir); err != nil {
		logger.Warn("installing legacy agent.sock symlink", "component", "main", "err", err)
	}
	agentSrv := &http.Server{
		Handler:           agent.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The OneCLI dashboard proxy (issue #26): --onecli-dashboard=port makes lab
	// itself the authenticated way in, on a THIRD listener carrying a
	// whole-origin reverse proxy to the sidecar's dashboard. Nil in every other
	// mode, which is the ordinary case — off is the default. It is a plain TCP
	// listener like the main one, so TLS is terminated the same way (in front of
	// lab, by the operator's proxy); lab terminates none itself, here or there.
	// No CloseStreams hook: the dashboard has no SSE (ADR-0067 verified the
	// approvals flow long-polls over fetch), so nothing holds Shutdown open.
	var dashSrv *http.Server
	dashHandler, err := api.OneCLIDashboardProxy()
	if err != nil {
		logger.Error("building the onecli dashboard proxy", "component", "main", "err", err)
		return 1
	}
	if dashHandler != nil {
		dashSrv = &http.Server{
			Addr:              cfg.OneCLIDashboardAddr,
			Handler:           dashHandler,
			ReadHeaderTimeout: 10 * time.Second,
		}
	}

	logger.Info("lab starting",
		"component", "main",
		"version", version,
		"addr", cfg.Addr,
		"agent_sock", sock,
		"db", dbBackend(cfg.DB),
		"state_dir", cfg.StateDir,
		"onecli_dashboard", cfg.OneCLIDashboard,
		"onecli_dashboard_addr", cfg.OneCLIDashboardAddr)

	// Buffered for every listener, so a goroutine whose server returns after
	// another already reported never blocks forever on an unread channel.
	errCh := make(chan error, 3)
	go func() { errCh <- srv.ListenAndServe() }()
	go func() { errCh <- agentSrv.Serve(agentLn) }()
	if dashSrv != nil {
		go func() { errCh <- dashSrv.ListenAndServe() }()
	}

	select {
	case err := <-errCh:
		logger.Error("http server failed", "component", "main", "err", err)
		return 1
	case <-ctx.Done():
		logger.Info("shutting down", "component", "main")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		failed := false
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "component", "main", "err", err)
			failed = true
		}
		// Shutdown closes the unix listener, which unlinks the socket file
		// (ListenSocket handles the crash case where it never got to).
		if err := agentSrv.Shutdown(shutdownCtx); err != nil {
			logger.Error("agent socket shutdown failed", "component", "main", "err", err)
			failed = true
		}
		if dashSrv != nil {
			if err := dashSrv.Shutdown(shutdownCtx); err != nil {
				logger.Error("onecli dashboard proxy shutdown failed", "component", "main", "err", err)
				failed = true
			}
		}
		// Cancel running clone jobs; interrupted repos heal on next start.
		repoSvc.Close()
		if failed {
			return 1
		}
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

// loadOrGenerateVAPIDKey mirrors loadOrGenerateMasterKey for the web push
// VAPID keypair (issue #98): stat-then-Generate — GenerateKey itself refuses
// to overwrite an existing key file, so a lost race can never clobber one and
// invalidate the subscriptions minted against the current public key.
func loadOrGenerateVAPIDKey(path string, logger *slog.Logger) (push.Key, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		key, genErr := push.GenerateKey(path)
		if genErr != nil {
			return push.Key{}, genErr
		}
		logger.Info("generated web push vapid key", "component", "main", "path", path)
		return key, nil
	}
	return push.LoadKey(path)
}

// providerCLIConfigs builds the per-provider container login/CLI configs
// (issue #206 / ADR-0057) the claude-code and codex adapters' LoginRunner and
// ContainerCLI read: the master-store declaration is the package-level
// resolver (the same one the adapter's own method uses), the tools image is
// that provider's ref (missing → actionable refusal at use, mirroring run
// spawns), and limits read the global container_* rows with the seeded
// fallbacks (the effectiveContainerLimits posture, minus the repo override
// that cannot apply to a repo-less login).
//
// The dev image is cfg.ContainerImage — the --container-image flag — and
// nothing else, deliberately (ADR-0057, ADR-0071): login and the CLI pokes are
// repo-less, machine-level state, so neither a repo's image_ref nor the global
// default dev image (the dev_image_default setting) ever applies, and st is
// handed in for the limits alone — the setting is never read here. A
// deployment that relies on the setting with the flag unset therefore has no
// login image, which providercli refuses naming --container-image.
func providerCLIConfigs(cfg config.Config, st *store.Store, preflight func() (podmanx.Result, bool), home string, logger *slog.Logger) (claudeCfg, codexCfg providercli.Config) {
	loginHomes := filepath.Join(cfg.StateDir, "logins")
	// One limits closure shared by both providers. The fallbacks are
	// store's single-source defaults (what SeedDefaultSettings writes)
	// — the same last-resort posture as instance's fallbacks.
	limits := func(ctx context.Context) (string, int, int, error) {
		memory, err := st.GetString(ctx, store.SettingContainerMemory, store.DefaultContainerMemory)
		if err != nil {
			return "", 0, 0, err
		}
		pids, err := st.GetInt(ctx, store.SettingContainerPids, store.DefaultContainerPids)
		if err != nil {
			return "", 0, 0, err
		}
		nofile, err := st.GetInt(ctx, store.SettingContainerNofile, store.DefaultContainerNofile)
		if err != nil {
			return "", 0, 0, err
		}
		return memory, pids, nofile, nil
	}
	claudeCfg = providercli.Config{
		ProviderID:    claudecode.ID,
		PodmanBin:     cfg.PodmanBin,
		Preflight:     preflight,
		Image:         cfg.ContainerImage,
		ToolsImage:    cfg.ContainerToolsImages[claudecode.ID],
		Spec:          claudecode.MasterStore,
		LoginHomeRoot: loginHomes,
		Limits:        limits,
		Logger:        logger,
	}
	codexCfg = providercli.Config{
		ProviderID: codex.ID,
		PodmanBin:  cfg.PodmanBin,
		Preflight:  preflight,
		Image:      cfg.ContainerImage,
		ToolsImage: cfg.ContainerToolsImages[codex.ID],
		// The same loginDir codex.New is handed in run(), so closure and
		// adapter resolve one store.
		Spec:          func() provider.MasterStoreSpec { return codex.MasterStore(home) },
		LoginHomeRoot: loginHomes,
		Limits:        limits,
		Logger:        logger,
	}
	return claudeCfg, codexCfg
}

// announceContainerRepos publishes repo.changed for every repo whose
// readiness depends on the container preflight (issue #61) — containerRepoIDs.
// The preflight's verdict lives in an in-memory gate no page is told about,
// so the goroutine that publishes it calls this when the verdict lands or
// changes: a repo home showing "preflight has not finished" then refetches
// and moves on. Best-effort — a failed listing announces nothing, and the
// next page load reads the truth anyway.
func announceContainerRepos(ctx context.Context, st *store.Store, rec *readiness.Recorder) {
	repoIDs, err := containerRepoIDs(ctx, st)
	if err != nil {
		return
	}
	rec.Announce(repoIDs...)
}

// readinessFanout is the readiness recorder's Fanout over the store: the
// importers of a repo (store.RepoImporters) and the container-Runner repos
// (containerRepoIDs).
func readinessFanout(st *store.Store) readiness.Fanout {
	return readiness.Fanout{
		Importers: func(ctx context.Context, repoID string) ([]string, error) {
			importers, err := st.RepoImporters(ctx, repoID)
			if err != nil {
				return nil, err
			}
			out := make([]string, 0, len(importers))
			for _, r := range importers {
				out = append(out, r.ID)
			}
			return out, nil
		},
		ImageRepos: func(ctx context.Context) ([]string, error) { return containerRepoIDs(ctx, st) },
	}
}

// containerRepoIDs lists the repos whose dev image check depends on
// container-side state — the preflight verdict, a dev image record: those
// whose effective Runner is container, plus any whose Runner cannot be
// resolved right now (counted in rather than silently skipped). It is also
// the readiness recorder's Fanout.ImageRepos.
func containerRepoIDs(ctx context.Context, st *store.Store) ([]string, error) {
	repos, err := st.Repos(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range repos {
		if runner, err := instance.EffectiveRunner(ctx, st, r); err != nil || runner == store.RunnerContainer {
			out = append(out, r.ID)
		}
	}
	return out, nil
}

// labURL is the LAB_URL handed to spawned sessions. An explicit --agent-url
// wins verbatim; otherwise the agent unix socket, which always exists (run
// listens on it before serving) and keeps machine traffic off the TCP
// address entirely. BaseURL deliberately plays no part: routing agent
// traffic through the external origin was exactly the SSO-proxy failure
// mode of issue #30.
func labURL(cfg config.Config) string {
	if cfg.AgentURL != "" {
		return cfg.AgentURL
	}
	return "unix://" + agentapi.SocketPath(cfg.StateDir)
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

// The backoff between startup Warpgate orphan run-key sweeps that did not
// complete (issue #39 / ADR-0068): 1m, 2m, 4m, 8m, then every 15m. The first
// wait is long enough for a Warpgate started beside lab to finish booting,
// and the cap keeps an outage that lasts all day at a few dozen quiet passes
// rather than a tight loop.
const (
	bastionSweepRetryFirst = time.Minute
	bastionSweepRetryMax   = 15 * time.Minute
)

// retryUntilComplete runs pass until it reports a complete pass or ctx is
// done, waiting firstDelay between the first two attempts and doubling the
// wait each time after, capped at maxDelay — the startup Warpgate key sweep's
// retry (SweepBastionKeys reports whether its pass completed). wait sleeps
// for d unless ctx ends first, reporting whether the full wait elapsed;
// production passes waitCtx, tests a recorder. Returns how many passes ran
// and whether the last one completed. Silent by design: each failed pass
// logs its own one warning, and the caller logs once when a retried pass
// finally completes.
func retryUntilComplete(ctx context.Context, pass func(context.Context) bool, firstDelay, maxDelay time.Duration, wait func(context.Context, time.Duration) bool) (attempts int, completed bool) {
	delay := firstDelay
	for {
		if ctx.Err() != nil {
			return attempts, false
		}
		attempts++
		if pass(ctx) {
			return attempts, true
		}
		if !wait(ctx, delay) {
			return attempts, false
		}
		delay = min(2*delay, maxDelay)
	}
}

// waitCtx sleeps for d, or until ctx is done; true when the full d elapsed.
func waitCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
