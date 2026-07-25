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
	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/presence"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/claudecode"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/codex"
	"git.cloonar.com/Cloonar/coding-lab/internal/providercli"
	"git.cloonar.com/Cloonar/coding-lab/internal/pull"
	"git.cloonar.com/Cloonar/coding-lab/internal/push"
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
)

// version is stamped via -ldflags "-X main.version=…".
var version = "dev"

const usage = `lab — phone-first control panel for Claude Code agents

Usage: lab [flags]
       lab hash-password   read a password from stdin (or prompt, echo off) and print its argon2id PHC hash

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
  -container-image string  global default dev image for containerized sessions;
                           per-repo Dev image overrides it, neither set refuses
                           the spawn (LAB_CONTAINER_IMAGE)
  -container-tools-image provider=ref[,provider=ref…]
                           agent-tools injection image per provider id,
                           @sha256-pinned per ADR-0051 (LAB_CONTAINER_TOOLS_IMAGE)
`

func main() {
	// The first subcommand on cmd/lab (issue #137). Dispatched on the literal
	// first arg, BEFORE config.Parse (inside run()) ever sees os.Args, so it
	// never collides with the server's own flag parsing and never needs a DB,
	// vault, or any of the rest of the server bootstrap.
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		os.Exit(runHashPassword(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
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
	// Per-run private HOME lifecycle (issue #202): <state>/instances holds one
	// private HOME per run — the isolation seam a run's provider credential copy,
	// config, and transcripts live under. New does no I/O (the dirs are created
	// lazily at launch), so it is wired unconditionally beside the vault
	// materializer and shared by the instance/afk/reconcile services (Materialize
	// at launch, Wipe at stop/rollback, SweepAll at boot/runtime) and the
	// chat/httpapi read paths (the pure HomePath).
	homes := instancehome.New(filepath.Join(cfg.StateDir, "instances"))

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

	// /pull-base lab command service (issue #149): merges origin/<base> into a
	// run's LIVE worktree and renders the agent-facing digest; the HTTP reply
	// path intercepts the command and delegates here. Same store/git/vault/
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
	// Pull failures are RETRIED (issue #220): deploy.yml refuses to bump the
	// host pin until the default tools refs resolve, but a registry blip or
	// a workflow_dispatch re-release can still leave a startup pull that
	// only time heals — every other preflight check reports host state only
	// a redeploy (and thus a fresh boot) changes. So the loop re-runs
	// preflight while a pull failure is present and republishes through the
	// gate: container spawns unblock the moment the registry serves the
	// ref, no restart needed. Repeat verdicts log only when the failure set
	// changes; a steady-state outage stays one warning, not one per minute.
	var containerPreflight func() (podmanx.Result, bool)
	if cfg.ContainerImage != "" || len(cfg.ContainerToolsImages) > 0 {
		gate := &podmanx.Gate{}
		containerPreflight = gate.Result
		go func() {
			const pullRetryEvery = time.Minute
			var prev []podmanx.Failure
			for {
				res := podmanx.Preflight(ctx, podmanx.PreflightConfig{
					PodmanBin:   cfg.PodmanBin,
					ToolsImages: cfg.ContainerToolsImages,
				}, podmanx.RealDeps())
				gate.Set(res)
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
				if !res.HasPullFailure() {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(pullRetryEvery):
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
		// store — never a host-CLI fallback. Per-provider Config: the
		// master-store declaration is the package-level resolver (the same one
		// the adapter's own method uses), the tools image is that provider's
		// ref (missing → actionable refusal at use, mirroring run spawns), the
		// dev image is the global default only (login is repo-less), and
		// limits read the global container_* rows with the seeded fallbacks
		// (the effectiveContainerLimits posture, minus the repo override that
		// cannot apply). Without container config the adapters keep the raw
		// runner and a nil CLI (→ provider.HostCLI inside New): host-mode
		// login stays byte-for-byte unchanged. The instance/reconcile/afk
		// services below always get the raw runner — the run-spawn seam is
		// their own podman handling, untouched here.
		claudeRunner := tmuxx.SessionRunner(runner)
		codexRunner := tmuxx.SessionRunner(runner)
		var claudeCLI, codexCLI provider.CLIRunner
		if containerPreflight != nil {
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
			claudeCfg := providercli.Config{
				ProviderID:    claudecode.ID,
				PodmanBin:     cfg.PodmanBin,
				Preflight:     containerPreflight,
				Image:         cfg.ContainerImage,
				ToolsImage:    cfg.ContainerToolsImages[claudecode.ID],
				Spec:          claudecode.MasterStore,
				LoginHomeRoot: loginHomes,
				Limits:        limits,
				Logger:        logger,
			}
			claudeRunner = providercli.NewLoginRunner(runner, claudeCfg)
			claudeCLI = providercli.NewContainerCLI(claudeCfg)
			codexCfg := providercli.Config{
				ProviderID: codex.ID,
				PodmanBin:  cfg.PodmanBin,
				Preflight:  containerPreflight,
				Image:      cfg.ContainerImage,
				ToolsImage: cfg.ContainerToolsImages[codex.ID],
				// The same loginDir codex.New is handed below, so closure and
				// adapter resolve one store.
				Spec:          func() provider.MasterStoreSpec { return codex.MasterStore(home) },
				LoginHomeRoot: loginHomes,
				Limits:        limits,
				Logger:        logger,
			}
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
			// Container runner wiring (issue #205): the spawn seam that turns a
			// Runner=container repo's pane command into `podman run`, gated on
			// the preflight verdict above (nil = structurally unavailable).
			PodmanBin:            cfg.PodmanBin,
			ContainerImage:       cfg.ContainerImage,
			ContainerToolsImages: cfg.ContainerToolsImages,
			ContainerPreflight:   containerPreflight,
			AgentSockDir:         agentapi.SocketDir(cfg.StateDir),
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
		homes.SetPreWipeHook(func(runID string) { credrotateSvc.AdoptCheck(ctx, runID) })

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
		Homes:           homes,
		Tracker:         trackerReg,
		AFK:             afkSvc,
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

	logger.Info("lab starting",
		"component", "main",
		"version", version,
		"addr", cfg.Addr,
		"agent_sock", sock,
		"db", dbBackend(cfg.DB),
		"state_dir", cfg.StateDir)

	errCh := make(chan error, 2)
	go func() { errCh <- srv.ListenAndServe() }()
	go func() { errCh <- agentSrv.Serve(agentLn) }()

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
