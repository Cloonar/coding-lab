// Package podmanx builds the podman half of a containerized run
// (issue #205): the exact `podman run` argv that becomes a session's tmux
// pane command, the deterministic container name Stop can recompute with no
// stored state, the HOME rewrite that re-anchors host-derived env values at
// the container-side mount point, the `podman rm` backstop, and the startup
// preflight (preflight.go) that refuses container spawns on a host that
// cannot run them. The same argv renderer also serves issue #206's
// provider-login shapes — see RunSpec for the three shapes it renders.
//
// The tracer-bullet split (#205): tmux stays host-side and untouched —
// containerizing a run means the pane command BECOMES `podman run -it --rm
// …`, nothing more. tmux still owns liveness, attach, send-keys, and
// capture; the podman client process in the pane owns the container's
// lifetime (tmux kill-session SIGHUPs the attached client, which
// sig-proxies into the container, and --rm reaps it — RemoveContainer is
// the backstop for a CLI that ignores SIGHUP). Secret env still travels via
// tmux `new-session -e K=V` (values out of `ps`, issue #204's mechanism);
// the pane's podman client then forwards those into the container by
// NAME-ONLY --env K flags, so no secret value ever appears in the `podman
// run` argv either.
//
// Everything here is pure construction over a RunSpec plus one injectable
// exec seam (CmdRunner): no podman is ever spawned at test time, and the
// wiring task (the instance layer) supplies real paths and a real runner.
package podmanx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// ToolsDst is the container-side mount point of the agent-tools image —
// hard-wired, not configurable: ADR-0051 rewrites the claude binary's ELF
// interpreter (PT_INTERP) at image-build time to the absolute path
// /opt/lab/lib/ld-musl-x86_64.so.1, so an image mounted anywhere else
// leaves claude unable to even start. Changing this destination is an
// image rebuild, never a runner flag.
const ToolsDst = "/opt/lab"

// Home is the container-side instance HOME: the host-side per-run home
// (internal/instancehome, issue #202) is bind-mounted here, giving the
// agent a stable, host-layout-independent HOME while the server keeps
// reading the same tree at its host path. RewriteHomeEnv translates env
// values the provider derived from the host path into this view.
const Home = "/home/agent"

// PATH is the container-side PATH: the injected tools bin (ADR-0051's
// /opt/lab/bin, carrying the provider CLI and static labctl) ahead of the
// plain FHS default — the lab-pinned CLI must win over any same-named
// binary the operator's dev image happens to carry, while the dev image's
// own userland (its business per ADR-0051, lab imposes none) stays
// reachable behind it.
const PATH = "/opt/lab/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// RunSpec is everything RunArgv needs to render a containerized command.
// One renderer serves three shapes: the run session (#205 — the full mount
// inventory, interactive), the login-session pane (#206 — only the tools
// image, a per-attempt scratch home, and the provider's master credential
// store; interactive, the login TUI runs on the pane's pty), and the
// non-interactive provider CLI poke (#206 auth status/logout/refresh —
// the store bind alone, no host home, NoTTY, because a poke has no pane
// and writes nothing worth keeping outside the store). All paths are host
// paths; the run shape's mount inventory (#205 design) binds each one
// back at its host-identical container path — except the instance home,
// which moves to Home — so path-valued state (worktree .git links into the
// bare repo, git cred file paths, LAB_URL's socket path) stays valid
// inside without rewriting.
type RunSpec struct {
	Bin        string // podman binary (config PodmanBin, PATH-resolved by exec)
	Name       string // container name, from ContainerName(session)
	Image      string // operator-chosen dev image the session runs in
	ToolsImage string // agent-tools image ref, @sha256-digest-pinned (ADR-0051)

	// WorktreeDir is the run's worktree: rw bind at its host-identical path
	// (the agent edits, the server diffs the same tree) and, absent Workdir,
	// the container workdir, mirroring tmux's `new-session -c dir`. Empty
	// omits the bind (the login/CLI shapes, issue #206).
	WorktreeDir string
	// BareDir is the bare repo: rw bind at its host-identical path because
	// the worktree's .git file points into it by absolute host path — the
	// worktree alone would be a broken repo inside. Empty omits the bind
	// (the login/CLI shapes, issue #206).
	BareDir string
	// AgentDir is the directory holding agent.sock, not the socket file
	// itself: a bind of the socket inode would go stale when a server
	// restart re-creates the socket, while a directory mount makes the new
	// inode visible to a container that outlives the restart. Empty omits
	// the bind (the login/CLI shapes, issue #206).
	AgentDir string
	// HomeDir is the host home mounted at Home — the one deliberately
	// non-host-identical mount (see Home). The run shape always sets it
	// (the instance home, instancehome.HomePath); the login shape sets a
	// per-attempt scratch home; the CLI shape leaves it EMPTY, which omits
	// the bind — a poke's HOME is the container's own writable layer, and
	// the nested store bind (StoreDst under Home) creates the /home/agent
	// parents in that layer by itself.
	HomeDir string
	// RuntimeDir is the per-run runtime dir (git cred files, dialog spool,
	// the --settings file), rw bind at its host-identical path: the git env
	// and provider argv reference these files by absolute host path. Empty
	// omits the bind (the login/CLI shapes, issue #206).
	RuntimeDir string
	// StoreDir is the provider's master credential store on the host,
	// bound rw at StoreDst when BOTH are set — the login/CLI shapes
	// (issue #206) put it under Home (e.g. Home + "/.claude"). Nesting
	// inside the login shape's home bind is safe because podman orders
	// nested binds by destination, so the store lands on top of the home
	// bind regardless of -v order; in the home-less CLI shape the bind
	// target itself creates the /home/agent parents in the container's
	// writable layer. Setting one of the pair without the other is a
	// programmer error: RunArgv omits the bind entirely rather than
	// rendering a half-formed -v, so the mistake surfaces as a visibly
	// missing store, never as an argv podman misparses.
	StoreDir string
	// StoreDst is the container-side mount point of StoreDir (see there);
	// both empty in the run shape.
	StoreDst string

	Memory string // e.g. "8g" — value owned by the caller, not defaulted here
	Pids   int    // --pids-limit
	Nofile int    // soft+hard RLIMIT_NOFILE; replaces the host prlimit wrapper

	// Workdir is the container workdir when non-empty; empty falls back to
	// WorktreeDir, so existing run-shape callers are untouched. The
	// login/CLI shapes set it to Home. When both are empty, -w is omitted
	// entirely and the image's own workdir applies.
	Workdir string
	// NoTTY drops -it: the non-interactive CLI shape (#206 auth
	// status/logout/refresh pokes) runs under no pane and must not demand
	// a tty — plain `podman run` stdin semantics apply instead. The zero
	// value keeps the interactive default the run and login shapes share.
	NoTTY bool

	// Env holds K=V pairs for non-secret container-side values (PATH, HOME,
	// LAB_URL, GIT_* paths), emitted as --env K=V in order. Values appear in
	// the pane argv, so nothing secret belongs here.
	Env []string
	// ForwardEnv holds variable NAMES only, emitted as --env K in order:
	// podman copies each value from the pane environment tmux seeded via
	// `new-session -e` (issue #204), so secrets reach the container without
	// ever entering an argv.
	ForwardEnv []string
	// Argv is the provider CLI command exec'd inside the container,
	// appended verbatim after the image.
	Argv []string
}

// RunArgv renders s into the complete `podman run` argv — the containerized
// pane command (run and login shapes) or the non-interactive CLI command
// (NoTTY); see RunSpec for the three shapes. Flag rationale, in argv order:
//
//   - --rm: the container is run-scoped state; the exiting client reaps it
//     so a clean stop leaves nothing for RemoveContainer to do.
//   - -it (dropped under NoTTY): the provider CLI is a TUI on the pane's
//     pty; without a tty and open stdin it drops to non-interactive mode.
//     The CLI shape has no pane and must not demand a tty, so NoTTY leaves
//     plain `podman run` semantics.
//   - --name: deterministic (ContainerName) so Stop's backstop can address
//     the container with no stored state.
//   - --userns=keep-id: the agent runs as the service uid inside, so files
//     it writes into the shared mounts (worktree, home, runtime) are owned
//     by the same uid host-side and the server can tail/diff them without
//     any chown dance.
//   - --network=pasta: full egress with no route back to the host's
//     loopback services — the agent talks to lab only through the mounted
//     unix socket, never a host port. (Caveat: pasta still maps
//     host.containers.internal to the host's global address, so a host
//     service bound to a wildcard/LAN address stays reachable — only
//     loopback-bound services are unreachable.)
//   - --cgroup-manager=systemd (a podman GLOBAL flag, hence its position
//     before "run" — ADR-0060): each container becomes its own transient
//     scope, libpod-<id>.scope, under the lab user's systemd user manager,
//     which performs ALL cgroup placement. The hand-rolled delegated-subtree
//     layouts of ADR-0058/0059 are retired — both died on the same kernel
//     wall: cgroup v2's delegation containment rule makes an unprivileged
//     cross-unit attach require write access to the common ancestor's
//     cgroup.procs (system.slice, root-owned), so podman's cgroupfs manager
//     could never move a container into any subtree lab set up, however
//     correctly delegated. The user manager instead FORWARDS the attach to
//     PID 1, so the ancestor rule never applies. No --cgroup-parent, ever:
//     podman's rootless default parent under the user manager is correct,
//     and a knob would re-open exactly the placement business lab is getting
//     out of. The manager is pinned explicitly because the choice must not
//     depend on podman's environment sniffing (house rule, ADR-0058).
//   - --memory/--pids-limit/--ulimit nofile: per-run blast-radius caps; the
//     ulimit replaces the host-side prlimit wrapper, which is retired for
//     container runs (the pane command is now the podman client, and
//     capping THAT process would be aiming at the wrong target). Under the
//     systemd manager the caps land as per-scope properties on
//     libpod-<id>.scope (MemoryMax/TasksMax → the scope cgroup's memory.max
//     and pids.max), verified end-to-end by the preflight spawn probe.
//   - --mount type=image: the read-only agent-tools injection at ToolsDst
//     (ADR-0051 — the destination is a hard contract, see ToolsDst).
//   - the -v binds and -w: the mount inventory documented on RunSpec.
//     Every bind renders only when its field is set — the run-only dirs,
//     the home (empty in the CLI shape), and the store pair, which follows
//     immediately after the home bind (nesting is by destination, see
//     StoreDir, so adjacency is for readability, not correctness). -w
//     prefers Workdir, falls back to WorktreeDir, and vanishes when both
//     are empty.
//
// Then --env K=V per Env entry, --env K per ForwardEnv entry (order
// preserved — later entries win under podman just as with exec env), the
// image, and the provider argv verbatim.
func RunArgv(s RunSpec) []string {
	args := []string{s.Bin, "--cgroup-manager=systemd", "run", "--rm"}
	if !s.NoTTY {
		args = append(args, "-it")
	}
	args = append(args,
		"--name", s.Name,
		"--userns=keep-id",
		"--network=pasta",
		"--memory", s.Memory,
		"--pids-limit", strconv.Itoa(s.Pids),
		"--ulimit", fmt.Sprintf("nofile=%d:%d", s.Nofile, s.Nofile),
		"--mount", "type=image,src="+s.ToolsImage+",dst="+ToolsDst,
	)
	if s.WorktreeDir != "" {
		args = append(args, "-v", s.WorktreeDir+":"+s.WorktreeDir)
	}
	if s.BareDir != "" {
		args = append(args, "-v", s.BareDir+":"+s.BareDir)
	}
	if s.AgentDir != "" {
		args = append(args, "-v", s.AgentDir+":"+s.AgentDir)
	}
	if s.HomeDir != "" {
		args = append(args, "-v", s.HomeDir+":"+Home)
	}
	if s.StoreDir != "" && s.StoreDst != "" {
		args = append(args, "-v", s.StoreDir+":"+s.StoreDst)
	}
	if s.RuntimeDir != "" {
		args = append(args, "-v", s.RuntimeDir+":"+s.RuntimeDir)
	}
	if s.Workdir != "" {
		args = append(args, "-w", s.Workdir)
	} else if s.WorktreeDir != "" {
		args = append(args, "-w", s.WorktreeDir)
	}
	for _, kv := range s.Env {
		args = append(args, "--env", kv)
	}
	for _, k := range s.ForwardEnv {
		args = append(args, "--env", k)
	}
	args = append(args, s.Image)
	return append(args, s.Argv...)
}

// ContainerName derives the run's container name from its tmux session
// name. Sessions are named "<repo>~<label>", and '~' (or anything else a
// repo/label may carry) is outside podman's name alphabet
// ([a-zA-Z0-9][a-zA-Z0-9_.-]*), so every byte outside [A-Za-z0-9_.-] is
// replaced with '.'. That replacement is lossy — "a~b" and "a.b" sanitize
// identically — so a "-" plus the first 6 hex chars of sha256(session) is
// appended as the collision guard: sessions differing only in sanitized
// bytes still get distinct containers. The "labrun-" prefix guarantees a
// legal leading character and namespaces lab's containers in `podman ps`.
// Deterministic by construction: Stop recomputes the name from the session
// alone, with no stored state to lose across a server restart.
func ContainerName(session string) string {
	sum := sha256.Sum256([]byte(session))
	var b strings.Builder
	b.WriteString("labrun-")
	for i := 0; i < len(session); i++ {
		switch c := session[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '_', c == '.', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('.')
		}
	}
	b.WriteByte('-')
	b.WriteString(hex.EncodeToString(sum[:3]))
	return b.String()
}

// RewritePathPrefix re-anchors one path: a path that IS from, or lives under
// it (from + "/"), moves onto to; ok reports whether a rewrite happened.
// Matching is exact-or-path-prefix, never substring — a sibling dir sharing
// the string prefix ("/…/home2") and a path merely containing from
// mid-string are both left alone, because rewriting those would corrupt
// paths that stay host-identical inside the container. This ONE function is
// the prefix discipline every mount re-anchoring in the tree rides
// (RewriteHomeEnv, RewriteEnvPrefix, providercli's workdir translation), so
// an edge-case hardening lands everywhere at once. An empty from returns
// (path, false): "" would prefix-match everything.
func RewritePathPrefix(path, from, to string) (string, bool) {
	if from == "" {
		return path, false
	}
	if path == from {
		return to, true
	}
	if rest, ok := strings.CutPrefix(path, from+"/"); ok {
		return to + "/" + rest, true
	}
	return path, false
}

// RewriteEnvPrefix re-anchors env values: for each K=V entry whose value
// RewritePathPrefix moves from → to, the entry is rewritten; everything else
// — including entries without '=' — passes through untouched. Pure: returns
// a new slice, the input is never mutated.
func RewriteEnvPrefix(env []string, from, to string) []string {
	out := make([]string, len(env))
	copy(out, env)
	for i, kv := range out {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if nv, ok := RewritePathPrefix(v, from, to); ok {
			out[i] = k + "=" + nv
		}
	}
	return out
}

// RewriteHomeEnv re-anchors host-home-derived values at the container-side
// Home: RewriteEnvPrefix with the instance home as the anchor. The wiring
// task feeds it the provider env, whose HOME= and config-dir values
// (CLAUDE_CONFIG_DIR=, CODEX_HOME=) were derived from the host-side instance
// home the container mounts at Home instead.
func RewriteHomeEnv(env []string, hostHome string) []string {
	return RewriteEnvPrefix(env, hostHome, Home)
}

// CmdRunner is the injectable exec seam RemoveContainer and Preflight run
// podman through — a bare func, not an interface, because one method is
// all there is and a test fake is then just a closure.
type CmdRunner func(ctx context.Context, name string, args ...string) (stdout []byte, err error)

// ExecRunner returns the real CmdRunner over exec.CommandContext. It uses
// Output, not CombinedOutput: Preflight parses `podman version --format`
// stdout, and podman routinely writes warnings (cgroup, config
// deprecations) to stderr — mixed streams would corrupt the parse. Output
// still captures stderr in the ExitError, which is folded into the
// returned error here so a failing command quotes podman's own explanation
// in preflight Failure details.
func ExecRunner() CmdRunner {
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out, err := exec.CommandContext(ctx, name, args...).Output()
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return out, err
	}
}

// RemoveContainer force-removes the named container: `podman rm --force
// --ignore --time 5 <name>`. It is Stop's backstop, not its main path —
// tmux kill-session SIGHUPs the attached podman client, which sig-proxies
// into the container and --rm reaps it; but a provider CLI that ignores
// SIGHUP would leave the container running (and its name claimed) after
// the session is gone. --force kills a still-running container after the
// 5s --time grace; --ignore makes the already-gone case (the normal one)
// exit 0, so this is idempotent exactly like tmuxx's Stop.
func RemoveContainer(ctx context.Context, run CmdRunner, bin, name string) error {
	if _, err := run(ctx, bin, "rm", "--force", "--ignore", "--time", "5", name); err != nil {
		return fmt.Errorf("podman rm %s: %w", name, err)
	}
	return nil
}

// EnsureImage guarantees the dev image is present locally before a container
// spawn commits to it: `podman image exists <ref>` is the cheap local probe,
// and a miss triggers exactly one `podman pull <ref>` — the same
// exists-then-pull shape as preflight's tools-image check, but run per-spawn
// because the effective dev image is per-repo (issue #207) and only the spawn
// knows which repo, so it cannot be a startup-once check like the tools refs.
// A local hit returns nil with no pull; a successful pull returns nil. A
// failed pull returns an error naming BOTH the ref and podman's own
// explanation, plus the operator action — the wiring layer surfaces this text
// verbatim in the spawn refusal, so it must say what to fix (the ref, or this
// host's access to its registry), never a bare "pull failed". Called before
// the claim so a cold pull that fails parks no issue behind it.
func EnsureImage(ctx context.Context, run CmdRunner, bin, ref string) error {
	if _, err := run(ctx, bin, "image", "exists", ref); err == nil {
		return nil
	}
	if _, err := run(ctx, bin, "pull", ref); err != nil {
		return fmt.Errorf("pulling dev image %s: %w (check the ref and registry access from this host)", ref, err)
	}
	return nil
}

// ListRunContainers returns the names of every lab run container podman
// knows: `podman ps --all --filter name=labrun- --format {{.Names}}`. It
// feeds the startup orphan sweep (issue #205) — a hard server-host crash, or
// a pane kill racing the client's own --rm reap, can leave a container alive
// with no session; the sweep removes every listed name that no live session's
// ContainerName accounts for. --all includes created/exited-but-unreaped
// containers, whose claimed names would still block a relaunch. podman's name
// filter is a substring/regex match, not an anchor, so the labrun- PREFIX is
// re-checked here — a container someone else named "xlabrun-y" must never
// enter a removal candidate list.
func ListRunContainers(ctx context.Context, run CmdRunner, bin string) ([]string, error) {
	out, err := run(ctx, bin, "ps", "--all", "--filter", "name=labrun-", "--format", "{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("podman ps: %w", err)
	}
	var names []string
	for line := range strings.Lines(string(out)) {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "labrun-") {
			names = append(names, line)
		}
	}
	return names, nil
}
