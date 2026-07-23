package reconcile

import (
	"context"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
)

// containerWired reports whether the container runner is configured on this
// server (issue #205) — the gate for the Discard backstop and the startup
// orphan sweep. Keyed on ContainerPreflight non-nil (cmd/lab wires it only
// with container config present) plus a podman binary to shell out to; on a
// host-only deployment no lab container can exist, so running podman there
// would only manufacture warnings.
func (s *Service) containerWired() bool {
	return s.podmanBin != "" && s.containerPreflight != nil
}

// removeRunContainer is the `podman rm` backstop behind the Discard kill
// (issue #205), mirroring its instance/afk siblings: tmux kill-session
// SIGHUPs a container pane's attached podman client, which sig-proxies in
// and --rm reaps — the forced rm covers a provider CLI that ignores SIGHUP.
// Deterministic name (podmanx.ContainerName over the session), --ignore
// no-op for host-mode sessions; errors log-warn, never fail the discard.
func (s *Service) removeRunContainer(ctx context.Context, session string) {
	if !s.containerWired() {
		return
	}
	if err := podmanx.RemoveContainer(ctx, s.podmanRun, s.podmanBin, podmanx.ContainerName(session)); err != nil {
		s.log.Warn("removing run container", "component", "reconcile", "session", session, "err", err)
	}
}

// sweepOrphanContainers removes every labrun- container no live tmux session
// accounts for — the startup half of #205's container janitorial story. The
// per-stop backstops (instance/afk/discard) cover a CLI ignoring SIGHUP at
// kill time; this sweep covers what no stop path could: a hard server-host
// crash (nothing ran any teardown) and a pane kill racing the client's own
// --rm reap. Normal stops leave nothing here, so every removal is worth a
// warn — it means a container leaked past its pane. Names are recomputed
// from the live session list (podmanx.ContainerName is deterministic), so
// the sweep needs no stored state and is safe against sessions of every
// mode: a host session's derived name simply never appears in podman's
// list. Best-effort throughout, like every janitorial pass in this package.
func (s *Service) sweepOrphanContainers(ctx context.Context) {
	if !s.containerWired() {
		return
	}
	containers, err := podmanx.ListRunContainers(ctx, s.podmanRun, s.podmanBin)
	if err != nil {
		s.log.Warn("container sweep: list run containers", "component", "reconcile", "err", err)
		return
	}
	if len(containers) == 0 {
		return
	}
	// Snapshot STRICTLY BEFORE List (the startguard ordering rule): a session
	// transitioning starting→live between the two reads is then seen at least
	// once. Mid-Launch sessions are not in tmux's list yet but their
	// containers (if the spawn already ran) must not be reaped from under the
	// launcher — the same §4b courtesy every sweep in this package extends.
	starting := s.guard.Snapshot()
	live, err := s.runner.List(ctx)
	if err != nil {
		s.log.Warn("container sweep: list sessions", "component", "reconcile", "err", err)
		return
	}
	// Every live/starting session keeps its derived container — login
	// sessions included for uniformity (they never have one, so their names
	// simply never match a listed container).
	owned := make(map[string]bool, len(live)+len(starting))
	for _, session := range append(starting, live...) {
		owned[podmanx.ContainerName(session)] = true
	}
	for _, name := range containers {
		if owned[name] {
			continue
		}
		s.log.Warn("container sweep: removing orphaned run container", "component", "reconcile", "container", name)
		if err := podmanx.RemoveContainer(ctx, s.podmanRun, s.podmanBin, name); err != nil {
			s.log.Warn("container sweep: remove", "component", "reconcile", "container", name, "err", err)
		}
	}
}
