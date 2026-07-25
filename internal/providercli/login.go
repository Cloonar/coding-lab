package providercli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// LoginRunner decorates a tmuxx.SessionRunner so provider LOGIN sessions
// (tmuxx.IsLoginSession) spawn their pane command as `podman run` against
// the master credential store, while every other session — and every method
// other than Start/Stop — passes through untouched. The adapters' login
// flows (scrape, paste, poll, teardown) never learn the pane is a container
// (ADR-0057's seam pin). Delegation is explicit method-by-method, NOT
// interface embedding: a future SessionRunner method must fail compilation
// here — forcing the pass-through-or-decorate decision — instead of silently
// passing through an embedded field.
type LoginRunner struct {
	inner tmuxx.SessionRunner
	cfg   Config
}

var _ tmuxx.SessionRunner = (*LoginRunner)(nil)

// NewLoginRunner wraps inner with the container-login decoration.
func NewLoginRunner(inner tmuxx.SessionRunner, cfg Config) *LoginRunner {
	return &LoginRunner{inner: inner, cfg: cfg}
}

// scratchDir is the per-attempt scratch HOME of a login session — derived
// from the session name alone so Stop can recompute it with no stored state,
// the same no-stored-state rule the container name follows (ADR-0052).
func (l *LoginRunner) scratchDir(name string) string {
	return filepath.Join(l.cfg.LoginHomeRoot, name)
}

// Start delegates non-login sessions untouched. For a login session it
// becomes the containerized pane spawn: refusals first (each wrapped in
// provider.ErrLoginUnavailable so httpapi maps it to a 400 carrying the
// actionable text verbatim — never the bare sentinel), then the dev-image
// ensure, the mount sources, and finally inner.Start with the rendered
// `podman run` argv as the pane command. dir passes through unchanged: the
// pane's cwd hosts only the podman CLIENT, and tmuxx.WithoutNofileCap is
// appended because prlimit-capping that client would aim the cap at the
// wrong process — the agent's descriptor bound is the container's own
// --ulimit nofile (the #205 rationale).
func (l *LoginRunner) Start(ctx context.Context, name, dir string, argv []string, extraEnv []string, opts ...tmuxx.StartOpt) error {
	if !tmuxx.IsLoginSession(name) {
		return l.inner.Start(ctx, name, dir, argv, extraEnv, opts...)
	}

	// Refusals, most-structural first (instance.refuseContainerSpawn's
	// discipline): an unpublished preflight verdict, then the red verdict /
	// missing image knobs structuralRefusal orders.
	r, done := l.cfg.Preflight()
	if !done {
		return fmt.Errorf("%w: %s", provider.ErrLoginUnavailable, preflightPendingText)
	}
	if msg := l.cfg.structuralRefusal(r); msg != "" {
		return fmt.Errorf("%w: %s", provider.ErrLoginUnavailable, msg)
	}

	// EnsureImage failure is a refusal too: its text already names the ref,
	// podman's own explanation, and the operator action, so the sentinel wrap
	// adds only the 400 routing. Double-%w keeps the pull error inspectable.
	if err := podmanx.EnsureImage(ctx, l.cfg.runner(), l.cfg.PodmanBin, l.cfg.Image); err != nil {
		return fmt.Errorf("%w: %w", provider.ErrLoginUnavailable, err)
	}

	// The mount source must exist before podman binds it — and a host that
	// has never logged in (no master store yet) is exactly the login case.
	spec := l.cfg.Spec()
	if err := os.MkdirAll(spec.HostDir, 0o700); err != nil {
		return fmt.Errorf("creating master store dir %s: %w", spec.HostDir, err)
	}

	// Fresh scratch HOME per attempt (the teardown-wipes pin): a leftover
	// from a crashed attempt must not leak state into a new one, so wipe
	// then recreate. Start is only reached when the session is not live —
	// the adapters' LoginStart guards on IsRunning — so the wipe can never
	// hit a live attempt's home; concurrent LoginStarts were racy before
	// this feature and remain benign.
	scratch := l.scratchDir(name)
	if err := os.RemoveAll(scratch); err != nil {
		return fmt.Errorf("wiping login scratch home %s: %w", scratch, err)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return fmt.Errorf("creating login scratch home %s: %w", scratch, err)
	}

	// A settings read failure is a lab fault, not operator misconfig — plain
	// error, never the ErrLoginUnavailable sentinel.
	memory, pids, nofile, err := l.cfg.Limits(ctx)
	if err != nil {
		return err
	}

	storeDst := podmanx.Home + "/" + spec.HomeSubdir
	rs := podmanx.RunSpec{
		Bin: l.cfg.PodmanBin,
		// The labrun- namespace, derived from the session name: the startup
		// orphan sweep accounts for a live login session's container by
		// construction, with no stored state (ADR-0052's rule).
		Name:       podmanx.ContainerName(name),
		Image:      l.cfg.Image,
		ToolsImage: l.cfg.ToolsImage,
		HomeDir:    scratch,
		StoreDir:   spec.HostDir,
		StoreDst:   storeDst,
		Workdir:    podmanx.Home,
		Memory:     memory,
		Pids:       pids,
		Nofile:     nofile,
		// The explicit config-dir pin (spec.EnvVar) is the issue's acceptance
		// criterion: the CLI must resolve the store at the mount point and
		// never derive some other path from its HOME-based default.
		Env: []string{
			"PATH=" + podmanx.PATH,
			"HOME=" + podmanx.Home,
			spec.EnvVar + "=" + storeDst,
		},
		// The login TUI needs the pane's real TERM; tmux sets it in the pane
		// env, so name-forwarding is exactly right (the run shape's precedent).
		ForwardEnv: []string{"TERM"},
		Argv:       argv, // the adapter's login argv, verbatim
	}
	// Clip before append: opts is the caller's slice, and growing it in
	// place could scribble over the caller's backing array.
	return l.inner.Start(ctx, name, dir, podmanx.RunArgv(rs), extraEnv,
		append(slices.Clip(opts), tmuxx.WithoutNofileCap())...)
}

// Stop delegates non-login sessions untouched. For a login session the tmux
// kill runs first — it SIGHUPs the attached podman client, which sig-proxies
// into the container and --rm reaps it, the normal path — and then, even
// when that kill errored, ALWAYS the `podman rm` backstop (a login CLI may
// ignore SIGHUP, ADR-0052's lesson) and the scratch-home wipe. Both
// follow-ups log-warn and never fail the stop (instance.removeRunContainer's
// posture; the boot orphan sweep is the backstop's backstop), and both are
// idempotent — rm --ignore, RemoveAll on an absent path — so Stop stays
// idempotent by construction. Returns inner.Stop's error.
func (l *LoginRunner) Stop(ctx context.Context, name string) error {
	if !tmuxx.IsLoginSession(name) {
		return l.inner.Stop(ctx, name)
	}
	stopErr := l.inner.Stop(ctx, name)
	if err := podmanx.RemoveContainer(ctx, l.cfg.runner(), l.cfg.PodmanBin, podmanx.ContainerName(name)); err != nil {
		l.cfg.logger().Warn("removing login container", "component", "providercli", "session", name, "err", err)
	}
	if err := os.RemoveAll(l.scratchDir(name)); err != nil {
		l.cfg.logger().Warn("removing login scratch home", "component", "providercli", "session", name, "err", err)
	}
	return stopErr
}

// IsRunning delegates: liveness is tmux's, container pane or not.
func (l *LoginRunner) IsRunning(ctx context.Context, name string) (bool, error) {
	return l.inner.IsRunning(ctx, name)
}

// List delegates.
func (l *LoginRunner) List(ctx context.Context) ([]string, error) {
	return l.inner.List(ctx)
}

// SendKeys delegates: keystrokes reach the login TUI through the pane's pty
// exactly as on a host pane — the container changes what runs, not how the
// pane is driven.
func (l *LoginRunner) SendKeys(ctx context.Context, name, text string, enter bool) error {
	return l.inner.SendKeys(ctx, name, text, enter)
}

// SendNamedKeys delegates.
func (l *LoginRunner) SendNamedKeys(ctx context.Context, name string, keys ...string) error {
	return l.inner.SendNamedKeys(ctx, name, keys...)
}

// PasteText delegates.
func (l *LoginRunner) PasteText(ctx context.Context, name, text string) error {
	return l.inner.PasteText(ctx, name, text)
}

// CapturePane delegates: the adapters' OAuth-URL scrape reads the same pane
// content whether the TUI runs on the host or in the container.
func (l *LoginRunner) CapturePane(ctx context.Context, name string) (string, error) {
	return l.inner.CapturePane(ctx, name)
}
