package instance

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// agentSockName is the socket filename inside Options.AgentSockDir — the
// tail of agentapi.SocketPath, restated here so the container-side LAB_URL
// rewrite needs no agentapi import for one basename. The runner mounts the
// DIRECTORY (RunSpec.AgentDir doc), so the container-side path is the host
// path unchanged: <AgentSockDir>/agent.sock.
const agentSockName = "agent.sock"

// Container-limit fallbacks — store's single-source defaults (the same
// consts SeedDefaultSettings writes), reachable only when a row is missing
// or garbled (the same last-resort posture as defaultMaxInstances).
const (
	defaultContainerMemory = store.DefaultContainerMemory
	defaultContainerPids   = store.DefaultContainerPids
	defaultContainerNofile = store.DefaultContainerNofile
)

// ContainerGateStage names where the container-mode spawn gate stopped, in
// the order the gate checks — most structural first.
type ContainerGateStage int

const (
	// ContainerGateOpen: every check passed; a container spawn may proceed
	// (its dev image is still pulled-if-missing by the caller).
	ContainerGateOpen ContainerGateStage = iota
	// ContainerGateNotConfigured: the server was started without container
	// config at all (no preflight is wired).
	ContainerGateNotConfigured
	// ContainerGatePreflightPending: the startup preflight has not published
	// a verdict yet (image pulls can take minutes) — retry.
	ContainerGatePreflightPending
	// ContainerGatePreflightFailed: the preflight finished and the host
	// cannot run containers; ContainerGate.Preflight holds every failure.
	ContainerGatePreflightFailed
	// ContainerGateNoToolsImage: no agent-tools image is configured for the
	// spawn's provider, so its CLI would not exist inside the container.
	ContainerGateNoToolsImage
	// ContainerGateNoDevImage: the effective dev image cannot be resolved —
	// none of its three layers is set (errors.Is(Err, ErrNoDevImage)), or the
	// dev_image_default setting could not be read.
	ContainerGateNoDevImage
)

// ContainerGate is the container-mode spawn gate's verdict for one provider
// on one repo. Stage says where it stopped; Err is the actionable refusal
// (nil exactly when Stage is ContainerGateOpen); Image is the effective dev
// image of an open gate; Preflight is the finished verdict behind a
// ContainerGatePreflightFailed.
type ContainerGate struct {
	Stage     ContainerGateStage
	Image     string
	Preflight podmanx.Result
	Err       error
}

// ContainerGate evaluates the container-mode spawn gate (issue #205) for a
// spawn of providerID on repo. It is the ONE implementation of that gate:
// refuseContainerSpawn turns its Err into the spawn's 400, and the readiness
// report (issue #61) reads the same verdict to say whether a container run
// could start — so what a page shows as "not ready" and what a spawn is
// refused for can never drift apart. Pure: it reads the in-memory preflight
// verdict, this service's configuration and the store, and starts no
// process — the dev image's pull-if-missing is the caller's separate step.
//
// The dev image is resolved here, not by the caller, because unlike every
// host/tools check the startup preflight owns it is PER-REPO: only a spawn
// (or a report) for one repo knows which image it needs. EffectiveDevImage is
// the one resolver. Refusals, most-structural first: no wiring at all → the
// server was started without container config; preflight unfinished → the
// boot goroutine (image pulls can take minutes) has not published a verdict,
// retry; preflight failed → the full multi-failure message, so ONE refusal
// names everything the operator must fix; no tools image for THIS provider →
// the actionable per-provider flag; finally the effective dev image — the
// repo's image_ref, else the global default dev image (the dev_image_default
// setting, ADR-0071), else the deployed fallback (--container-image) —
// refused naming all THREE knobs when none is set, since any one fixes it,
// and refused naming dev_image_default when that setting cannot be read
// (never a silent drop to the flag image).
func (s *Service) ContainerGate(ctx context.Context, providerID string, repo store.Repo) ContainerGate {
	if s.containerPreflight == nil {
		return ContainerGate{Stage: ContainerGateNotConfigured,
			Err: errors.New("container runner not configured on this server — set --container-tools-image (and a dev image: the repo's Dev image in its Runner settings, the global default dev image in Settings → Runner, or --container-image)")}
	}
	r, done := s.containerPreflight()
	if !done {
		return ContainerGate{Stage: ContainerGatePreflightPending,
			Err: errors.New("container preflight has not finished — retry in a moment")}
	}
	if !r.OK() {
		return ContainerGate{Stage: ContainerGatePreflightFailed, Preflight: r, Err: errors.New(r.Error())}
	}
	if s.containerToolsImages[providerID] == "" {
		return ContainerGate{Stage: ContainerGateNoToolsImage,
			Err: fmt.Errorf("no agent-tools image configured for provider %s — set --container-tools-image %s=<ref>", providerID, providerID)}
	}
	// Effective dev image (issues #207, #55): repo image_ref → the
	// dev_image_default setting → the --container-image flag, resolved by the
	// one resolver. Its errors (none of the three set, or the setting
	// unreadable) already carry the actionable text.
	image, err := s.DevImage(ctx, repo)
	if err != nil {
		return ContainerGate{Stage: ContainerGateNoDevImage, Err: err}
	}
	return ContainerGate{Stage: ContainerGateOpen, Image: image}
}

// refuseContainerSpawn is the container-mode spawn gate (issue #205) as
// Launch runs it — ContainerGate's verdict, with a closed gate turned into
// the spawn's refusal. It runs BEFORE anything is created: a refused spawn
// must never park a claim (the AFK worktree IS the claim, so ordering this
// after AddWorktree would strand the issue behind a host misconfiguration).
// On success it returns the run's effective dev image (issues #207, #55).
//
// Error mapping — the documented choice (issue #205): every refusal is a
// *BadRequestError → 400 via httpapi's writeInstanceError. Of the two
// existing mappings that surface a DYNAMIC message verbatim to the UI,
// BadRequestError is the client-error one; StartFailedError would render
// these as 500s, misfiling an operator-fixable host/config mismatch as a lab
// fault. The 409 sentinel family (ErrRepoNotReady & co.) only carries fixed
// messages, and the actionable text here is the whole point — a dedicated
// 409 case would mean extending httpapi, which this wiring task's scope
// pins closed. Precedent: the unknown-model/provider 400s, equally "what
// you asked for, this deployment cannot spawn". An unreadable
// dev_image_default follows the same mapping as an unresolvable
// runner_default (Launch's EffectiveRunner refusal): a 400 whose text names
// the setting, refused before the claim.
func (s *Service) refuseContainerSpawn(ctx context.Context, providerID string, repo store.Repo) (image string, err error) {
	g := s.ContainerGate(ctx, providerID, repo)
	if g.Err != nil {
		return "", badRequestf("%s", g.Err)
	}
	return g.Image, nil
}

// DevImage is EffectiveDevImage over this service's own store and its copy of
// the --container-image flag — the exact dev image chain the container gate
// above resolves for a spawn of repo. Exported so the repo settings page's
// inherited values (issue #61) ask the same chain with the same fallback
// rather than a second copy of the flag. Pure: a store read, no pull.
func (s *Service) DevImage(ctx context.Context, repo store.Repo) (string, error) {
	return EffectiveDevImage(ctx, s.store, repo, s.containerImage)
}

// EffectiveContainerLimits resolves a container run's resource caps: the
// repo's override column when set, else the global settings row, else the
// seeded default — the same repo-??-settings shape as EffectiveCap, except a
// settings READ error refuses the launch instead of warning: limits are the
// blast-radius contract of #205, and silently spawning uncapped (or
// default-capped against the operator's stored intent) on a flaky read
// would defeat it. Runs before the claim, so the refusal is free. Exported
// for the repo settings page's inherited values (issue #61), which read the
// limits a repo without its own overrides would run with from here.
func (s *Service) EffectiveContainerLimits(ctx context.Context, repo store.Repo) (memory string, pids, nofile int, err error) {
	if repo.ContainerMemory != nil {
		memory = *repo.ContainerMemory
	} else if memory, err = s.store.GetString(ctx, store.SettingContainerMemory, defaultContainerMemory); err != nil {
		return "", 0, 0, err
	}
	if repo.ContainerPids != nil {
		pids = *repo.ContainerPids
	} else if pids, err = s.store.GetInt(ctx, store.SettingContainerPids, defaultContainerPids); err != nil {
		return "", 0, 0, err
	}
	if repo.ContainerNofile != nil {
		nofile = *repo.ContainerNofile
	} else if nofile, err = s.store.GetInt(ctx, store.SettingContainerNofile, defaultContainerNofile); err != nil {
		return "", 0, 0, err
	}
	return memory, pids, nofile, nil
}

// containerEnv translates a session's spawn env (spawnEnv's output) into the
// container run's --env split (issue #205). Pure — Launch calls it once per
// container spawn, tests drive it exhaustively.
//
// The secret/non-secret rule, applied HERE and nowhere else: values that are
// tokens/keys are secret and travel by NAME only (--env K — podman copies
// the value from the pane environment tmux seeded via `new-session -e`, so
// it never enters any argv); values that are paths, URLs, or identity
// strings are non-secret and ride --env K=V in the visible argv.
//
// TWO entries in spawnEnv's output are credentials by value, and both are
// forwarded by name: LAB_TOKEN, and the credential gateway's
// HTTPS_PROXY/https_proxy pair (issue #24 / ADR-0067), whose URLs carry the
// repo's agent-identity token as userinfo. The proxy pair is recognized
// through gateway.go's isProxySecretEnv rather than re-listed here — one
// classification, one place, no second copy to drift. Everything else
// spawnEnv produces is path/identity-shaped: GIT_SSH_COMMAND/GIT_ASKPASS name
// files whose CONTENTS are the secret (those files live in the bind-mounted
// per-run runtime dir), GIT_AUTHOR_* are public identity, and the gateway
// bundle's own NO_PROXY/no_proxy (hostnames) and four CA variables (a path)
// are public by construction and deliberately VISIBLE in the argv, so an
// operator reading a run's command line can see what it trusts and what it
// exempts.
//
// Per entry: LAB_TOKEN and the proxy pair → forward by name. LAB_URL →
// REPLACED with sockURL: a container reaches lab only over the bind-mounted
// unix socket — a TCP --agent-url is deliberately unreachable, pasta's netns
// has no route back to the host's loopback OR wildcard-bound services (the
// #205/#216 no-host-route isolation), so honoring it would strand labctl.
// Everything
// else passes as K=V, then
// podmanx.RewriteHomeEnv re-anchors HOME= and every host-home-derived value
// (CLAUDE_CONFIG_DIR, CODEX_HOME) at the container-side Home mount. That
// rewrite is anchored on the instance HOME (<state>/instances/<run>/home) and
// therefore leaves the trust-bundle path in the four CA variables ALONE: the
// bundle lives in the run's RUNTIME dir (<state>/instances/<run>/runtime),
// a sibling, which the runner binds rw at its host-identical path — so the
// same absolute path resolves to the same file inside and outside, exactly as
// the git credential files beside it already do. No new mount, no new podman
// flag, and nothing here to translate. Appended
// after the walk: PATH= podmanx.PATH (tools bin first, ADR-0051 — the
// lab-pinned CLI must win over the dev image's own) into env, and TERM into
// forward (the provider TUI needs the pane's real TERM; tmux sets it in the
// pane env, so name-forwarding is exactly right).
//
// EXACTLY ONE PATH, and it is this function's (issue #39 / ADR-0068). A PATH
// entry arriving in spawnEnv is DROPPED in the walk rather than passed as
// K=V: it could only be a host-shaped value — the host's PATH names host
// directories that mean nothing inside the dev image — and a second PATH in
// the podman argv would leave which one wins to podman's --env ordering.
// pathPrefix is what goes in front of podmanx.PATH: "" for every unwired run
// — so an unwired container's env is byte-identical to before #39 — and a
// Warpgate-wired run's wrapper dir plus ":" (bastionContainerPATHPrefix),
// which is a runtime-dir path and therefore valid inside the container
// unchanged, like the trust bundle above.
func containerEnv(spawnEnv []string, hostHome, sockURL, pathPrefix string) (env, forward []string) {
	for _, kv := range spawnEnv {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case name == "LAB_TOKEN" || isProxySecretEnv(name):
			forward = append(forward, name)
		case name == "LAB_URL":
			env = append(env, "LAB_URL="+sockURL)
		case name == "PATH":
			// Never forwarded inward; composed below.
		default:
			env = append(env, kv)
		}
	}
	env = podmanx.RewriteHomeEnv(env, hostHome)
	env = append(env, "PATH="+pathPrefix+podmanx.PATH)
	forward = append(forward, "TERM")
	return env, forward
}

// secretForwardEnv extracts from spawnEnv the K=V entries of the name-only
// forwards — the ONLY entries a container pane's tmux `new-session -e` env
// carries (in practice LAB_TOKEN, plus the gateway's HTTPS_PROXY/https_proxy
// on a gateway-wired run; TERM has no spawnEnv entry and reaches
// the pane from tmux itself). The principled split of #205: the podman argv
// carries every non-secret value, tmux -e carries every secret value, and
// nothing rides both. Handing the full spawnEnv to tmux instead would leak
// host-shaped GIT_*/HOME entries into the pane where they'd shadow nothing
// but confuse everything — the container env is podman's alone to build.
func secretForwardEnv(spawnEnv, forward []string) []string {
	names := make(map[string]bool, len(forward))
	for _, k := range forward {
		names[k] = true
	}
	var out []string
	for _, kv := range spawnEnv {
		if name, _, ok := strings.Cut(kv, "="); ok && names[name] {
			out = append(out, kv)
		}
	}
	return out
}

// containerSockURL is the container-side LAB_URL: the agent socket inside
// the bind-mounted AgentSockDir, host-path-identical (the runner mounts the
// dir at its host path).
func (s *Service) containerSockURL() string {
	return "unix://" + filepath.Join(s.agentSockDir, agentSockName)
}

// removeRunContainer is the `podman rm` backstop behind a session kill
// (issue #205): tmux kill-session SIGHUPs the attached podman client, which
// sig-proxies into the container and --rm reaps it — but a provider CLI that
// ignores SIGHUP leaves the container running with its name claimed, so
// every Stop/rollback follows the tmux kill with a forced rm. Container
// names are deterministic (podmanx.ContainerName over the session name), so
// no stored state is needed; for a host-mode session the named container
// never existed and --ignore makes this an exit-0 no-op. Skipped when
// container wiring is absent (nothing podman-shaped configured — shelling
// out would only manufacture warnings); errors log-warn, never fail the
// stop (the orphan sweep at next boot is the backstop's backstop).
func (s *Service) removeRunContainer(ctx context.Context, session string) {
	if s.podmanBin == "" || s.containerPreflight == nil {
		return
	}
	if err := podmanx.RemoveContainer(ctx, s.podmanRun, s.podmanBin, podmanx.ContainerName(session)); err != nil {
		s.log.Warn("removing run container", "component", "instance", "session", session, "err", err)
	}
}
