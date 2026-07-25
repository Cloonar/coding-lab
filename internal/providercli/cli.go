package providercli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// Preflight-wait bounds (see ContainerCLI.awaitPreflight): poll the gate at
// this cadence, give up after the ceiling. They seed ContainerCLI fields
// rather than being read directly only so tests can shrink them.
const (
	defaultPreflightPoll    = 200 * time.Millisecond
	defaultPreflightCeiling = 10 * time.Second
)

// ContainerCLI is the container-mode provider.CLIRunner (issue #206): each
// invocation runs as an ephemeral non-tty `podman run --rm` against the
// rw-mounted master credential store, so a container-wired host needs no
// provider CLI on its PATH — the agent-tools image is the CLI distribution
// (ADR-0057). Construct with NewContainerCLI.
type ContainerCLI struct {
	cfg Config
	// execStreams is the two-stream exec seam the podman client runs
	// through — distinct from podmanx.CmdRunner, which returns stdout only:
	// the CLIRunner contract hands callers stdout AND stderr with the
	// process error passed through raw, and folding streams or wrapping the
	// error at the transport would destroy both halves. Tests inject a fake.
	execStreams func(ctx context.Context, argv []string) (stdout, stderr []byte, err error)
	// preflightPoll/preflightCeiling bound awaitPreflight; construction-set
	// to the defaults above, shrunk by tests.
	preflightPoll    time.Duration
	preflightCeiling time.Duration
}

var _ provider.CLIRunner = (*ContainerCLI)(nil)

// NewContainerCLI returns a ContainerCLI over the real os/exec transport.
func NewContainerCLI(cfg Config) *ContainerCLI {
	return &ContainerCLI{
		cfg:              cfg,
		execStreams:      execStreams,
		preflightPoll:    defaultPreflightPoll,
		preflightCeiling: defaultPreflightCeiling,
	}
}

// execStreams is the real two-stream transport: separate buffers, and the
// process error returned raw — classification is Run's, not the transport's.
func execStreams(ctx context.Context, argv []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// Run implements provider.CLIRunner: wait out the preflight, refuse on a
// structural gap (plain errors — CLI callers treat any non-ExitError as
// could-not-run; the ErrLoginUnavailable sentinel belongs to LoginStart
// alone), ensure the dev image, render the CLI shape, execute, classify.
func (c *ContainerCLI) Run(ctx context.Context, inv provider.CLIInvocation) ([]byte, []byte, error) {
	if len(inv.Argv) == 0 {
		return nil, nil, errors.New("providercli: CLIInvocation.Argv is empty")
	}
	r, err := c.awaitPreflight(ctx)
	if err != nil {
		return nil, nil, err
	}
	if msg := c.cfg.structuralRefusal(r); msg != "" {
		return nil, nil, errors.New(msg)
	}
	// Restart-safety guard (ADR-0058), re-run per invocation: a cgroup
	// layout podman dirtied after boot would wedge the next service restart,
	// so it hard-refuses here — a could-not-run error, never a CLI verdict.
	if err := r.Cgroups.Verify(); err != nil {
		return nil, nil, fmt.Errorf("container cgroup layout unsafe: %w", err)
	}
	if err := podmanx.EnsureImage(ctx, c.cfg.runner(), c.cfg.PodmanBin, c.cfg.Image); err != nil {
		return nil, nil, err
	}
	spec := c.cfg.Spec()
	// The mount source must exist; a poke on a never-logged-in host (the
	// spawn gate's auth check, exactly) still needs the bind to render.
	if err := os.MkdirAll(spec.HostDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("creating master store dir %s: %w", spec.HostDir, err)
	}
	memory, pids, nofile, err := c.cfg.Limits(ctx)
	if err != nil {
		return nil, nil, err
	}

	storeDst := podmanx.Home + "/" + spec.HomeSubdir
	rs := podmanx.RunSpec{
		Bin:        c.cfg.PodmanBin,
		Name:       c.containerName(),
		Image:      c.cfg.Image,
		ToolsImage: c.cfg.ToolsImage,
		// HomeDir stays empty (the CLI shape): a poke's HOME is the
		// container's own writable layer, and the store bind target alone
		// creates the /home/agent parents (RunSpec.HomeDir's contract).
		StoreDir:     spec.HostDir,
		StoreDst:     storeDst,
		Workdir:      c.workdir(inv.Dir, spec.HostDir, storeDst),
		NoTTY:        true, // no pane, no pty — plain `podman run` stdin semantics
		CgroupParent: r.CgroupParent(),
		Memory:       memory,
		Pids:         pids,
		Nofile:       nofile,
		// Pins first, the invocation's own env after: later --env wins under
		// podman, so an adapter's own config-dir pin — the same value once
		// rewritten — stays authoritative over these defaults, preserving
		// CLIInvocation.Env's override semantics. The spec.EnvVar pin is the
		// issue's acceptance criterion: never rely on the CLI's HOME-derived
		// default store path. No ForwardEnv: non-interactive, no pane whose
		// environment could source a value, no secrets to carry.
		Env: append([]string{
			"PATH=" + podmanx.PATH,
			"HOME=" + podmanx.Home,
			spec.EnvVar + "=" + storeDst,
		}, podmanx.RewriteEnvPrefix(inv.Env, spec.HostDir, storeDst)...),
		Argv: inv.Argv,
	}

	stdout, stderr, err := c.execStreams(ctx, podmanx.RunArgv(rs))
	// MaxStdout is enforced post-hoc, before exit classification — the host
	// path kills the process the moment the cap is crossed, so an overflow
	// there preempts any exit verdict, and the ordering here mirrors that.
	// Honest caveat: the podman client buffered the whole stream regardless,
	// so this cap is the seam's haywire-binary guard (never a silent
	// truncation), not a memory bound — the host path streams, this path
	// checks.
	if inv.MaxStdout > 0 && int64(len(stdout)) > inv.MaxStdout {
		return stdout, stderr, fmt.Errorf("stdout exceeds %d bytes", inv.MaxStdout)
	}
	if err != nil {
		return stdout, stderr, classify(err, stderr)
	}
	return stdout, stderr, nil
}

// awaitPreflight waits out a still-running preflight instead of failing on
// it (unlike LoginRunner.Start, which refuses instantly — an operator
// retries a click; a CLI caller would degrade): the pending state is a
// boot-time race — codex's construction-time catalog probe, a spawn right
// after a restart — where the verdict is normally seconds away. Bounded by
// ctx AND the ceiling: a preflight still unfinished after ~10s is no longer
// a race, and the pending error surfaces. A published verdict returns
// immediately, red or green — the caller judges it.
func (c *ContainerCLI) awaitPreflight(ctx context.Context) (podmanx.Result, error) {
	deadline := time.Now().Add(c.preflightCeiling)
	for {
		if r, done := c.cfg.Preflight(); done {
			return r, nil
		}
		if time.Now().After(deadline) {
			return podmanx.Result{}, errors.New(preflightPendingText)
		}
		select {
		case <-ctx.Done():
			return podmanx.Result{}, ctx.Err()
		case <-time.After(c.preflightPoll):
		}
	}
}

// containerName mints a unique labrun- name per invocation:
// "cli-<provider>-<16 hex>" through podmanx.ContainerName. The labrun-
// prefix puts ephemeral CLI containers in the same namespace the startup
// orphan sweep reaps, so a wedged one is removed at the next boot with no
// stored state (ADR-0052's rule). A boot sweep racing an IN-FLIGHT CLI
// container and force-removing it is accepted self-healing: the caller sees
// a could-not-run error and retries or degrades — never a false CLI verdict.
func (c *ContainerCLI) containerName() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails (crypto/rand.Read's own contract since Go 1.24)
	return podmanx.ContainerName("cli-" + c.cfg.ProviderID + "-" + hex.EncodeToString(b[:]))
}

// workdir translates the invocation's host working directory into the
// container: "" means the container home (a poke with no dir preference); a
// dir equal to or under the master store is re-anchored at StoreDst via
// podmanx.RewritePathPrefix — the one exact-or-path-prefix discipline every
// mount re-anchoring shares, so a sibling dir merely sharing the string
// prefix never lands inside the store. Anything else falls back to the home
// with a warn, because no other host dir is mounted and honoring it is
// impossible — unreachable today (the adapters only ever pass "" or the
// store), kept defensive rather than fatal.
func (c *ContainerCLI) workdir(dir, hostDir, storeDst string) string {
	if dir == "" {
		return podmanx.Home
	}
	if wd, ok := podmanx.RewritePathPrefix(dir, hostDir, storeDst); ok {
		return wd
	}
	c.cfg.logger().Warn("provider CLI dir is outside the master store — running at the container home",
		"component", "providercli", "provider", c.cfg.ProviderID, "dir", dir)
	return podmanx.Home
}

// classify sorts an execStreams error into the CLIRunner contract's two
// bins. podman reserves exit codes 125 (podman's own failure), 126 (the
// command could not be invoked), and 127 (the command was not found) for
// the VEHICLE: those mean podman could not pull/create/exec — so they are
// wrapped, with a stderr tail saying why, to NOT be a direct
// *exec.ExitError. A missing image must never read as codex's definitive
// "logged out". Every other exit code is the inner CLI's own (podman
// proxies it) and passes through RAW, preserving the direct-ExitError
// contract. Accepted caveat: an inner CLI legitimately exiting 125-127
// would be misread as a podman failure — those codes are podman-reserved
// by convention, and the misread degrades to could-not-run, never to a
// false CLI verdict. Non-exit errors (context cancelled, podman binary
// missing) are already could-not-run shaped and pass through as-is.
func classify(err error, stderr []byte) error {
	ee, ok := err.(*exec.ExitError) // direct, deliberately: the transport returns raw errors
	if !ok {
		return err
	}
	switch ee.ExitCode() {
	case 125, 126, 127:
		return fmt.Errorf("podman run failed (exit %d): %w%s", ee.ExitCode(), err, stderrTail(stderr))
	}
	return err
}

// stderrTail renders the tail of podman's stderr for the vehicle-failure
// wrap — the part that says WHY (manifest unknown, cannot connect, exec
// format error) — bounded so a chatty podman cannot balloon the error. The
// cut point advances to a UTF-8 rune boundary so the tail never opens
// mid-sequence (provider.TruncateDetail's precedent, mirrored).
func stderrTail(stderr []byte) string {
	s := strings.TrimSpace(string(stderr))
	if s == "" {
		return ""
	}
	const max = 400
	if len(s) > max {
		n := len(s) - max
		for n < len(s) && s[n]&0xC0 == 0x80 {
			n++
		}
		s = "…" + s[n:]
	}
	return ": " + s
}
