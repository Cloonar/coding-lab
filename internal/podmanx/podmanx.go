// Package podmanx builds the podman half of a containerized run
// (issue #205): the exact `podman run` argv that becomes a session's tmux
// pane command, the deterministic container name Stop can recompute with no
// stored state, the HOME rewrite that re-anchors host-derived env values at
// the container-side mount point, the `podman rm` backstop, and the startup
// preflight (preflight.go) that refuses container spawns on a host that
// cannot run them.
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

// RunSpec is everything RunArgv needs to render a containerized session's
// pane command. All paths are host paths; the mount inventory (#205 design)
// binds each one back at its host-identical container path — except the
// instance home, which moves to Home — so path-valued state (worktree
// .git links into the bare repo, git cred file paths, LAB_URL's socket
// path) stays valid inside without rewriting.
type RunSpec struct {
	Bin        string // podman binary (config PodmanBin, PATH-resolved by exec)
	Name       string // container name, from ContainerName(session)
	Image      string // operator-chosen dev image the session runs in
	ToolsImage string // agent-tools image ref, @sha256-digest-pinned (ADR-0051)

	// WorktreeDir is the run's worktree: rw bind at its host-identical path
	// (the agent edits, the server diffs the same tree) and the container
	// workdir, mirroring tmux's `new-session -c dir`.
	WorktreeDir string
	// BareDir is the bare repo: rw bind at its host-identical path because
	// the worktree's .git file points into it by absolute host path — the
	// worktree alone would be a broken repo inside.
	BareDir string
	// AgentDir is the directory holding agent.sock, not the socket file
	// itself: a bind of the socket inode would go stale when a server
	// restart re-creates the socket, while a directory mount makes the new
	// inode visible to a container that outlives the restart.
	AgentDir string
	// HomeDir is the host instance home (instancehome.HomePath), mounted at
	// Home — the one deliberately non-host-identical mount (see Home).
	HomeDir string
	// RuntimeDir is the per-run runtime dir (git cred files, dialog spool,
	// the --settings file), rw bind at its host-identical path: the git env
	// and provider argv reference these files by absolute host path.
	RuntimeDir string

	Memory string // e.g. "8g" — value owned by the caller, not defaulted here
	Pids   int    // --pids-limit
	Nofile int    // soft+hard RLIMIT_NOFILE; replaces the host prlimit wrapper

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
// pane command. Flag rationale, in argv order:
//
//   - --rm: the container is run-scoped state; the exiting client reaps it
//     so a clean stop leaves nothing for RemoveContainer to do.
//   - -it: the provider CLI is a TUI on the pane's pty; without a tty and
//     open stdin it drops to non-interactive mode.
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
//   - --cgroups=split: place the container in lab's OWN delegated cgroup
//     (<lab's cgroup>/libpod-payload-<id>) instead of podman's rootless
//     default of user.slice. Without this the container lands in a subtree
//     the startup preflight never checks, so its --memory/--pids-limit caps
//     could be silently absent on a green preflight (the delegation check in
//     preflight.go reads lab's own cgroup — split makes that the SAME
//     subtree the container uses). Split reuses the current cgroup for both
//     conmon and payload; it requires cgroup-v2 with memory+pids delegated
//     to the lab unit (systemd Delegate=yes), which the preflight enforces.
//   - --memory/--pids-limit/--ulimit nofile: per-run blast-radius caps; the
//     ulimit replaces the host-side prlimit wrapper, which is retired for
//     container runs (the pane command is now the podman client, and
//     capping THAT process would be aiming at the wrong target). These caps
//     only bind because --cgroups=split lands the payload in lab's delegated
//     cgroup subtree.
//   - --mount type=image: the read-only agent-tools injection at ToolsDst
//     (ADR-0051 — the destination is a hard contract, see ToolsDst).
//   - the -v binds and -w: the mount inventory documented on RunSpec.
//
// Then --env K=V per Env entry, --env K per ForwardEnv entry (order
// preserved — later entries win under podman just as with exec env), the
// image, and the provider argv verbatim.
func RunArgv(s RunSpec) []string {
	args := []string{
		s.Bin, "run", "--rm", "-it",
		"--name", s.Name,
		"--userns=keep-id",
		"--network=pasta",
		"--cgroups=split",
		"--memory", s.Memory,
		"--pids-limit", strconv.Itoa(s.Pids),
		"--ulimit", fmt.Sprintf("nofile=%d:%d", s.Nofile, s.Nofile),
		"--mount", "type=image,src=" + s.ToolsImage + ",dst=" + ToolsDst,
		"-v", s.WorktreeDir + ":" + s.WorktreeDir,
		"-v", s.BareDir + ":" + s.BareDir,
		"-v", s.AgentDir + ":" + s.AgentDir,
		"-v", s.HomeDir + ":" + Home,
		"-v", s.RuntimeDir + ":" + s.RuntimeDir,
		"-w", s.WorktreeDir,
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

// RewriteHomeEnv re-anchors host-home-derived values at the container-side
// Home: for each K=V entry whose value IS hostHome or lives under it
// (hostHome + "/"), that prefix becomes Home. The wiring task feeds it the
// provider env, whose HOME= and config-dir values (CLAUDE_CONFIG_DIR=,
// CODEX_HOME=) were derived from the host-side instance home the container
// mounts at Home instead. Matching is exact-or-path-prefix, never
// substring: a sibling dir sharing the string prefix ("/…/home2") and a
// value merely containing the home mid-string are both left alone —
// rewriting those would corrupt paths that stay host-identical inside the
// container. Pure: returns a new slice, the input is never mutated.
func RewriteHomeEnv(env []string, hostHome string) []string {
	out := make([]string, len(env))
	copy(out, env)
	if hostHome == "" {
		return out // no anchor to rewrite from; "" would prefix-match everything
	}
	for i, kv := range out {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if v == hostHome {
			out[i] = k + "=" + Home
		} else if rest, ok := strings.CutPrefix(v, hostHome+"/"); ok {
			out[i] = k + "=" + Home + "/" + rest
		}
	}
	return out
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
