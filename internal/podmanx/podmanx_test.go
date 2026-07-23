package podmanx

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// assertArgv fails with the first differing element — a plain DeepEqual
// message on a 40-element argv is unreadable.
func assertArgv(t *testing.T, got, want []string) {
	t.Helper()
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q\n got: %q\nwant: %q", i, got[i], want[i], got, want)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("argv length = %d, want %d\n got: %q\nwant: %q", len(got), len(want), got, want)
	}
}

func TestRunArgvGolden(t *testing.T) {
	s := RunSpec{
		Bin:         "/usr/bin/podman",
		Name:        "labrun-myrepo.afk-205-7876ed",
		Image:       "docker.io/library/debian:stable-slim",
		ToolsImage:  "git.cloonar.com/cloonar/agent-tools@sha256:deadbeef",
		WorktreeDir: "/var/lib/lab/worktrees/myrepo-1",
		BareDir:     "/var/lib/lab/repos/myrepo.git",
		AgentDir:    "/var/lib/lab/agent",
		HomeDir:     "/var/lib/lab/instances/run_1/home",
		RuntimeDir:  "/var/lib/lab/runtime/run_1",
		Memory:      "8g",
		Pids:        4096,
		Nofile:      16384,
		Env: []string{
			"PATH=" + PATH,
			"HOME=" + Home,
			"LAB_URL=unix:///var/lib/lab/agent/agent.sock",
		},
		ForwardEnv: []string{"LAB_TOKEN", "GIT_ASKPASS"},
		Argv:       []string{"claude", "--model", "opus"},
	}
	want := []string{
		"/usr/bin/podman", "run", "--rm", "-it",
		"--name", "labrun-myrepo.afk-205-7876ed",
		"--userns=keep-id",
		"--network=pasta",
		"--memory", "8g",
		"--pids-limit", "4096",
		"--ulimit", "nofile=16384:16384",
		"--mount", "type=image,src=git.cloonar.com/cloonar/agent-tools@sha256:deadbeef,dst=/opt/lab",
		"-v", "/var/lib/lab/worktrees/myrepo-1:/var/lib/lab/worktrees/myrepo-1",
		"-v", "/var/lib/lab/repos/myrepo.git:/var/lib/lab/repos/myrepo.git",
		"-v", "/var/lib/lab/agent:/var/lib/lab/agent",
		"-v", "/var/lib/lab/instances/run_1/home:/home/agent",
		"-v", "/var/lib/lab/runtime/run_1:/var/lib/lab/runtime/run_1",
		"-w", "/var/lib/lab/worktrees/myrepo-1",
		"--env", "PATH=" + PATH,
		"--env", "HOME=/home/agent",
		"--env", "LAB_URL=unix:///var/lib/lab/agent/agent.sock",
		"--env", "LAB_TOKEN",
		"--env", "GIT_ASKPASS",
		"docker.io/library/debian:stable-slim",
		"claude", "--model", "opus",
	}
	assertArgv(t, RunArgv(s), want)
}

// TestRunArgvTail pins the ordering contract of the trailing section: Env
// K=V entries in slice order, then ForwardEnv names in slice order, then
// the image, then the provider argv verbatim — including elements that
// look like flags, which must land after the image where podman no longer
// parses them.
func TestRunArgvTail(t *testing.T) {
	s := RunSpec{
		Bin:        "podman",
		Image:      "img",
		Env:        []string{"A=1", "B=2"},
		ForwardEnv: []string{"C", "D"},
		Argv:       []string{"tool", "--model", "opus", "-v"},
	}
	got := RunArgv(s)
	wantTail := []string{
		"--env", "A=1",
		"--env", "B=2",
		"--env", "C",
		"--env", "D",
		"img",
		"tool", "--model", "opus", "-v",
	}
	if len(got) < len(wantTail) {
		t.Fatalf("argv too short: %q", got)
	}
	assertArgv(t, got[len(got)-len(wantTail):], wantTail)
}

// podmanNameRe is podman's container-name alphabet (names(7) in podman:
// [a-zA-Z0-9][a-zA-Z0-9_.-]*), the constraint ContainerName exists to
// satisfy.
var podmanNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func TestContainerName(t *testing.T) {
	// Golden: "~" sanitizes to "."; suffix = first 6 hex chars of
	// sha256("myrepo~afk-205").
	if got, want := ContainerName("myrepo~afk-205"), "labrun-myrepo.afk-205-7876ed"; got != want {
		t.Errorf("ContainerName(myrepo~afk-205) = %q, want %q", got, want)
	}

	// Collision guard: inputs differing only in sanitized bytes must not
	// collide ("a~b" and "a.b" both sanitize to "a.b").
	a, b := ContainerName("a~b"), ContainerName("a.b")
	if a == b {
		t.Errorf("ContainerName(a~b) == ContainerName(a.b) == %q; sanitization collision", a)
	}
	if got, want := a, "labrun-a.b-941528"; got != want {
		t.Errorf("ContainerName(a~b) = %q, want %q", got, want)
	}
	if got, want := b, "labrun-a.b-2e7336"; got != want {
		t.Errorf("ContainerName(a.b) = %q, want %q", got, want)
	}

	// Every output matches podman's name regex, however hostile the input.
	for _, session := range []string{
		"myrepo~afk-205", "a~b", "a.b", "repo with spaces~x", "über~loop/../etc", "~",
	} {
		if got := ContainerName(session); !podmanNameRe.MatchString(got) {
			t.Errorf("ContainerName(%q) = %q: not a valid podman name", session, got)
		}
	}

	// Deterministic: Stop must be able to recompute the name with no
	// stored state.
	if x, y := ContainerName("myrepo~afk-205"), ContainerName("myrepo~afk-205"); x != y {
		t.Errorf("ContainerName not deterministic: %q vs %q", x, y)
	}
}

func TestRewriteHomeEnv(t *testing.T) {
	const hostHome = "/var/lib/lab/instances/run_1/home"
	in := []string{
		"HOME=" + hostHome,                           // exact match
		"CLAUDE_CONFIG_DIR=" + hostHome + "/.claude", // prefix match
		"PATH=/usr/bin:/bin",                         // unrelated value
		"NOTE=see " + hostHome + "/x for details",    // contains mid-string: untouched
		"SIBLING=" + hostHome + "2/x",                // shares string prefix, not path prefix
		"EMPTY=",
	}
	want := []string{
		"HOME=" + Home,
		"CLAUDE_CONFIG_DIR=" + Home + "/.claude",
		"PATH=/usr/bin:/bin",
		"NOTE=see " + hostHome + "/x for details",
		"SIBLING=" + hostHome + "2/x",
		"EMPTY=",
	}
	inCopy := append([]string(nil), in...)
	got := RewriteHomeEnv(in, hostHome)
	assertArgv(t, got, want)

	// Pure: the input slice is never mutated.
	assertArgv(t, in, inCopy)

	// An empty hostHome has no anchor and must rewrite nothing.
	assertArgv(t, RewriteHomeEnv([]string{"HOME=/x"}, ""), []string{"HOME=/x"})
}

// recordingRunner is the CmdRunner fake: records every invocation, answers
// from a script keyed by the space-joined command line, and treats an
// unscripted command as a test bug.
type recordingRunner struct {
	calls  [][]string
	script map[string]cmdResult
}

type cmdResult struct {
	out string
	err error
}

func (r *recordingRunner) run(_ context.Context, name string, args ...string) ([]byte, error) {
	argv := append([]string{name}, args...)
	r.calls = append(r.calls, argv)
	key := strings.Join(argv, " ")
	res, ok := r.script[key]
	if !ok {
		return nil, errors.New("unscripted command: " + key)
	}
	return []byte(res.out), res.err
}

func TestRemoveContainer(t *testing.T) {
	r := &recordingRunner{script: map[string]cmdResult{
		"podman rm --force --ignore --time 5 labrun-a.b-941528": {},
	}}
	if err := RemoveContainer(context.Background(), r.run, "podman", "labrun-a.b-941528"); err != nil {
		t.Fatalf("RemoveContainer: %v", err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %q, want exactly one", r.calls)
	}
	assertArgv(t, r.calls[0], []string{"podman", "rm", "--force", "--ignore", "--time", "5", "labrun-a.b-941528"})
}

func TestRemoveContainerError(t *testing.T) {
	boom := errors.New("boom")
	r := &recordingRunner{script: map[string]cmdResult{
		"podman rm --force --ignore --time 5 gone": {err: boom},
	}}
	err := RemoveContainer(context.Background(), r.run, "podman", "gone")
	if !errors.Is(err, boom) {
		t.Fatalf("RemoveContainer error = %v, want wrapped %v", err, boom)
	}
	if !strings.Contains(err.Error(), "gone") {
		t.Errorf("error %q does not name the container", err)
	}
}

func TestListRunContainers(t *testing.T) {
	const psCmd = "podman ps --all --filter name=labrun- --format {{.Names}}"

	t.Run("parses names, re-anchors the prefix", func(t *testing.T) {
		// podman's name filter matches substrings, so a foreign container
		// merely CONTAINING labrun- can come back; the client-side prefix
		// check must drop it (never a removal candidate). Blank lines vanish.
		r := &recordingRunner{script: map[string]cmdResult{
			psCmd: {out: "labrun-a.b-941528\n\nxlabrun-notours\nlabrun-c.d-2e7336\n"},
		}}
		got, err := ListRunContainers(context.Background(), r.run, "podman")
		if err != nil {
			t.Fatalf("ListRunContainers: %v", err)
		}
		assertArgv(t, got, []string{"labrun-a.b-941528", "labrun-c.d-2e7336"})
		if len(r.calls) != 1 {
			t.Fatalf("calls = %q, want exactly one", r.calls)
		}
	})

	t.Run("empty output is no containers", func(t *testing.T) {
		r := &recordingRunner{script: map[string]cmdResult{psCmd: {out: ""}}}
		got, err := ListRunContainers(context.Background(), r.run, "podman")
		if err != nil || got != nil {
			t.Fatalf("ListRunContainers = %q, %v; want nil, nil", got, err)
		}
	})

	t.Run("ps failure propagates", func(t *testing.T) {
		boom := errors.New("boom")
		r := &recordingRunner{script: map[string]cmdResult{psCmd: {err: boom}}}
		if _, err := ListRunContainers(context.Background(), r.run, "podman"); !errors.Is(err, boom) {
			t.Fatalf("error = %v, want wrapped %v", err, boom)
		}
	})
}

// The Gate's three observable states: nothing published (preflight still
// running), a published OK result, and a published failing result — the
// spawn refusal keys on exactly this progression.
func TestGate(t *testing.T) {
	var g Gate
	if r, ok := g.Result(); ok {
		t.Fatalf("fresh gate published %+v; want none", r)
	}
	g.Set(Result{Version: "5.0.0"})
	r, ok := g.Result()
	if !ok || !r.OK() || r.Version != "5.0.0" {
		t.Fatalf("Result() = %+v, %v; want the published OK result", r, ok)
	}
	// Last write wins — a re-preflight replaces the verdict wholesale.
	g.Set(Result{Failures: []Failure{{Check: CheckPasta, Detail: "gone", Hint: "install passt"}}})
	if r, ok := g.Result(); !ok || r.OK() {
		t.Fatalf("Result() after failing Set = %+v, %v; want the failing result", r, ok)
	}
}
