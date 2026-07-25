// Package providercli is container-mode execution of provider CLIs against
// the master credential store (issue #206, ADR-0057): two consumers of one
// Config. LoginRunner decorates the tmuxx.SessionRunner so a provider LOGIN
// session's pane command becomes `podman run` — the interactive login TUI
// running against the rw-mounted master store — while every other session
// and every other method pass through untouched. ContainerCLI implements
// provider.CLIRunner so every non-interactive provider-CLI invocation (auth
// status, logout, the ADR-0055 refresh poke, codex's catalog probe) becomes
// an ephemeral non-tty `podman run --rm` against the same store. Both render
// through podmanx.RunSpec's login/CLI shapes; podman knowledge lives HERE so
// it stays out of the provider packages (the CLIRunner seam's stated split —
// adapters only ever describe WHAT to run, never where).
//
// There is never a fallback from container to host (ADR-0057): the wiring
// layer builds this package only when container config exists, and every
// structural gap — unfinished or red preflight, a missing image knob — is a
// loud, actionable refusal. LoginRunner wraps its refusals in
// provider.ErrLoginUnavailable (httpapi maps the sentinel to a 400 carrying
// the wrapped text verbatim); ContainerCLI returns plain could-not-run
// errors, because the CLIRunner contract reserves the direct *exec.ExitError
// for the inner CLI's own exit and treats everything else as could-not-run —
// the login sentinel belongs to LoginStart alone.
package providercli

import (
	"context"
	"fmt"
	"log/slog"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// Config is the per-provider container-login/CLI configuration the wiring
// layer assembles once and both consumers read. All fields are data or
// injectable seams — nothing here spawns podman at construction time.
type Config struct {
	// ProviderID names the provider in refusal texts and in ephemeral
	// CLI-container names; it is also the --container-tools-image key the
	// tools-image refusal points the operator at.
	ProviderID string
	// PodmanBin is the podman binary (PATH-resolved by exec).
	PodmanBin string
	// Run is the EnsureImage/RemoveContainer transport; nil defaults to
	// podmanx.ExecRunner(). It is deliberately NOT how ContainerCLI runs the
	// CLI container itself — that needs stdout and stderr separated (the
	// CLIRunner contract), which CmdRunner's single stream cannot carry.
	Run podmanx.CmdRunner
	// Preflight is the startup gate's accessor (podmanx.Gate.Result). Never
	// nil: the wiring layer only builds this package when container config
	// exists, and a container-wired server always runs the boot preflight.
	Preflight func() (podmanx.Result, bool)
	// Image is the global default dev image (--container-image). Login and
	// the CLI pokes are repo-less, machine-level state, so the per-repo
	// dev-image resolution (ADR-0053) never applies; "" refuses.
	Image string
	// ToolsImage is this provider's agent-tools ref (ADR-0051) — the only
	// CLI distribution a container-mode host has; "" refuses.
	ToolsImage string
	// Spec resolves the provider's master-store declaration FRESH per call:
	// the adapters' resolvers read the override env vars ($CLAUDE_CONFIG_DIR,
	// $CODEX_HOME) at call time, so a held resolution could disagree with the
	// environment — the issue #211 override-support criterion rides on this.
	Spec func() provider.MasterStoreSpec
	// LoginHomeRoot is <state>/logins: each login attempt gets a scratch
	// HOME at <LoginHomeRoot>/<session>, wiped fresh per attempt and on Stop.
	LoginHomeRoot string
	// Limits reads the global container_* settings rows. Global only: login
	// and the CLI pokes are repo-less, so no repo override can apply
	// (ADR-0057's no-new-knobs pin).
	Limits func(ctx context.Context) (memory string, pids, nofile int, err error)
	// Logger; nil defaults to slog.Default().
	Logger *slog.Logger
}

// runner returns the configured exec transport, defaulting to the real one.
func (c Config) runner() podmanx.CmdRunner {
	if c.Run != nil {
		return c.Run
	}
	return podmanx.ExecRunner()
}

// logger returns the configured logger, defaulting to slog.Default().
func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// preflightPendingText is the shared unfinished-preflight refusal — the same
// text instance.refuseContainerSpawn uses for run spawns, so the operator
// reads one message for one condition regardless of which surface hit it.
const preflightPendingText = "container preflight has not finished — retry in a moment"

// structuralRefusal returns the actionable text for the first structural
// problem with a containerized invocation, "" when the config clears —
// ordered most-structural first, mirroring instance.refuseContainerSpawn's
// discipline: a red preflight verdict (its Error() already names every fix),
// then the per-provider tools image, then the dev image. The dev-image text
// deliberately names ONE knob, unlike the run refusal that names two: login
// and the CLI pokes are repo-less, so no repo Runner setting can ever
// provide the image — only --container-image can. The preflight-PENDING case
// is not here because the two consumers treat it differently: LoginRunner
// refuses instantly (an operator can simply retry), ContainerCLI waits out
// the boot race first.
func (c Config) structuralRefusal(r podmanx.Result) string {
	if !r.OK() {
		return r.Error()
	}
	if c.ToolsImage == "" {
		return fmt.Sprintf("no agent-tools image configured for provider %s — set --container-tools-image %s=<ref>", c.ProviderID, c.ProviderID)
	}
	if c.Image == "" {
		return "no dev image configured — set --container-image"
	}
	return ""
}
