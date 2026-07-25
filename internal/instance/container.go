package instance

import (
	"context"
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

// refuseContainerSpawn is the container-mode spawn gate (issue #205), run
// BEFORE anything is created — a refused spawn must never park a claim (the
// AFK worktree IS the claim, so ordering this after AddWorktree would strand
// the issue behind a host misconfiguration). It doubles as the effective dev
// image resolver (issue #207) and returns that image on success: the two
// concerns share this pre-claim spot because the dev-image refusal, unlike
// every host/tools check the startup preflight owns, is PER-REPO — only the
// spawn knows the repo, so a "no image for this repo" verdict cannot be
// reached at boot the way the others are. Refusals, most-structural first: no
// wiring at all → the server was started without container config; preflight
// unfinished → the boot goroutine (image pulls can take minutes) has not
// published a verdict, retry; preflight failed → the full multi-failure
// message, so ONE refusal names everything the operator must fix; no tools
// image for THIS provider → the actionable per-provider flag; finally the
// effective dev image — the repo's image_ref override when set, else the
// global default — refused naming BOTH knobs when neither is set, since
// either one fixes it.
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
// you asked for, this deployment cannot spawn".
func (s *Service) refuseContainerSpawn(providerID string, repo store.Repo) (image string, err error) {
	if s.containerPreflight == nil {
		return "", badRequestf("container runner not configured on this server — set --container-tools-image (and a dev image via the repo's Runner settings or --container-image)")
	}
	r, done := s.containerPreflight()
	if !done {
		return "", badRequestf("container preflight has not finished — retry in a moment")
	}
	if !r.OK() {
		return "", badRequestf("%s", r.Error())
	}
	if s.containerToolsImages[providerID] == "" {
		return "", badRequestf("no agent-tools image configured for provider %s — set --container-tools-image %s=<ref>", providerID, providerID)
	}
	// Effective dev image (issue #207): the repo's own image_ref override
	// (digest-pinned by reposvc on save) wins, else the server's global
	// default. Neither set is the refusal preflight no longer owns — name both
	// knobs, since setting either one clears it.
	if repo.ImageRef != nil && *repo.ImageRef != "" {
		image = *repo.ImageRef
	} else {
		image = s.containerImage
	}
	if image == "" {
		return "", badRequestf("no dev image for this repo — set a dev image in the repo's Runner settings or configure a server default with --container-image")
	}
	return image, nil
}

// effectiveContainerLimits resolves a container run's resource caps: the
// repo's override column when set, else the global settings row, else the
// seeded default — the same repo-??-settings shape as EffectiveCap, except a
// settings READ error refuses the launch instead of warning: limits are the
// blast-radius contract of #205, and silently spawning uncapped (or
// default-capped against the operator's stored intent) on a flaky read
// would defeat it. Runs before the claim, so the refusal is free.
func (s *Service) effectiveContainerLimits(ctx context.Context, repo store.Repo) (memory string, pids, nofile int, err error) {
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
// strings are non-secret and ride --env K=V in the visible argv. Everything
// spawnEnv produces today is path/identity-shaped (GIT_SSH_COMMAND/
// GIT_ASKPASS name files whose CONTENTS are the secret — and those files
// live in the bind-mounted per-run runtime dir — GIT_AUTHOR_* are public
// identity) except LAB_TOKEN, the one credential-by-value.
//
// Per entry: LAB_TOKEN → forward by name. LAB_URL → REPLACED with sockURL:
// a container reaches lab only over the bind-mounted unix socket — a TCP
// --agent-url is deliberately unreachable, pasta's netns has no route back
// to the host's loopback (the #205 no-host-route isolation), so honoring it
// would strand labctl. Everything else passes as K=V, then
// podmanx.RewriteHomeEnv re-anchors HOME= and every host-home-derived value
// (CLAUDE_CONFIG_DIR, CODEX_HOME) at the container-side Home mount. Appended
// after the walk: PATH= podmanx.PATH (tools bin first, ADR-0051 — the
// lab-pinned CLI must win over the dev image's own) into env, and TERM into
// forward (the provider TUI needs the pane's real TERM; tmux sets it in the
// pane env, so name-forwarding is exactly right).
func containerEnv(spawnEnv []string, hostHome, sockURL string) (env, forward []string) {
	for _, kv := range spawnEnv {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "LAB_TOKEN":
			forward = append(forward, "LAB_TOKEN")
		case "LAB_URL":
			env = append(env, "LAB_URL="+sockURL)
		default:
			env = append(env, kv)
		}
	}
	env = podmanx.RewriteHomeEnv(env, hostHome)
	env = append(env, "PATH="+podmanx.PATH)
	forward = append(forward, "TERM")
	return env, forward
}

// secretForwardEnv extracts from spawnEnv the K=V entries of the name-only
// forwards — the ONLY entries a container pane's tmux `new-session -e` env
// carries (LAB_TOKEN=… in practice; TERM has no spawnEnv entry and reaches
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
