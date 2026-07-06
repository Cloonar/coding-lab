package gitx

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// gitCmd runs one git command against the fixture repos with the hermetic
// env (design §11), failing the test on error.
func gitCmd(t *testing.T, home, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), testutil.HermeticGitEnv(home)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// makeOrigin builds a fixture origin repo on the given initial branch with
// `commits` commits and returns its path.
func makeOrigin(t *testing.T, home, branch string, commits int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	gitCmd(t, home, "", "init", "-q", "-b", branch, dir)
	for i := range commits {
		name := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(name, fmt.Appendf(nil, "content %d\n", i), 0o644); err != nil {
			t.Fatalf("write fixture file: %v", err)
		}
		gitCmd(t, home, dir, "add", ".")
		gitCmd(t, home, dir, "commit", "-q", "-m", fmt.Sprintf("c%d", i))
	}
	return dir
}

type progressCall struct {
	phase   string
	percent int
	line    string
}

func TestCloneBare_realGit(t *testing.T) {
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	origin := makeOrigin(t, home, "main", 5)
	dest := filepath.Join(t.TempDir(), "repo.git")
	env := testutil.HermeticGitEnv(home)
	eng := New("git")

	var calls []progressCall
	err := eng.CloneBare(t.Context(), "file://"+origin, dest, env, func(phase string, percent int, line string) {
		calls = append(calls, progressCall{phase, percent, line})
	})
	if err != nil {
		t.Fatalf("CloneBare: %v", err)
	}

	if got := gitCmd(t, home, dest, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("rev-parse --is-bare-repository = %q, want true", got)
	}
	// The remote-tracking layout the merged checks and the M3 worktree fork
	// base rely on must exist right after the clone.
	wantSHA := gitCmd(t, home, origin, "rev-parse", "main")
	if got := gitCmd(t, home, dest, "rev-parse", "refs/remotes/origin/main"); got != wantSHA {
		t.Errorf("refs/remotes/origin/main = %s, want %s", got, wantSHA)
	}

	// Progress fired, with sane phases/percentages; a file:// clone forces
	// the smart transport, so Receiving objects always appears.
	if len(calls) == 0 {
		t.Fatal("progress callback never fired")
	}
	sawReceiving, saw100 := false, false
	for _, c := range calls {
		switch c.phase {
		case PhaseCounting, PhaseCompressing, PhaseReceiving:
		default:
			t.Errorf("unexpected progress phase %q (line %q)", c.phase, c.line)
		}
		if c.percent < 0 || c.percent > 100 {
			t.Errorf("progress percent %d out of range (line %q)", c.percent, c.line)
		}
		if c.line == "" {
			t.Error("progress callback got an empty line")
		}
		sawReceiving = sawReceiving || c.phase == PhaseReceiving
		saw100 = saw100 || c.percent == 100
	}
	if !sawReceiving {
		t.Errorf("no receiving-phase progress among %d calls: %+v", len(calls), calls)
	}
	if !saw100 {
		t.Errorf("no 100%% progress among %d calls: %+v", len(calls), calls)
	}

	if got := eng.DefaultBranch(t.Context(), dest, env); got != "main" {
		t.Errorf("DefaultBranch = %q, want main", got)
	}
}

func TestDefaultBranch_masterOriginAndFallbacks(t *testing.T) {
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	origin := makeOrigin(t, home, "master", 1)
	dest := filepath.Join(t.TempDir(), "repo.git")
	env := testutil.HermeticGitEnv(home)
	eng := New("git")

	if err := eng.CloneBare(t.Context(), "file://"+origin, dest, env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}

	// Tier 1: ls-remote --symref asks the origin itself.
	if got := eng.DefaultBranch(t.Context(), dest, env); got != "master" {
		t.Errorf("DefaultBranch (ls-remote) = %q, want master", got)
	}

	// Tier 2: origin unreachable → the bare repo's own HEAD symref answers.
	gitCmd(t, home, dest, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	if got := eng.DefaultBranch(t.Context(), dest, env); got != "master" {
		t.Errorf("DefaultBranch (HEAD symref fallback) = %q, want master", got)
	}

	// Tier 3: HEAD detached as well → the v0 "main" fallback.
	sha := gitCmd(t, home, dest, "rev-parse", "refs/heads/master")
	gitCmd(t, home, dest, "update-ref", "--no-deref", "HEAD", sha)
	if got := eng.DefaultBranch(t.Context(), dest, env); got != "main" {
		t.Errorf("DefaultBranch (final fallback) = %q, want main", got)
	}
}

func TestFetch_realGit(t *testing.T) {
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	origin := makeOrigin(t, home, "main", 2)
	dest := filepath.Join(t.TempDir(), "repo.git")
	env := testutil.HermeticGitEnv(home)
	eng := New("git")

	if err := eng.CloneBare(t.Context(), "file://"+origin, dest, env, nil); err != nil {
		t.Fatalf("CloneBare: %v", err)
	}

	// Origin advances under the clone's feet; Fetch must advance the local
	// remote-tracking ref.
	if err := os.WriteFile(filepath.Join(origin, "more.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	gitCmd(t, home, origin, "add", ".")
	gitCmd(t, home, origin, "commit", "-q", "-m", "advance")
	wantSHA := gitCmd(t, home, origin, "rev-parse", "main")

	if err := eng.Fetch(t.Context(), dest, env); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := gitCmd(t, home, dest, "rev-parse", "refs/remotes/origin/main"); got != wantSHA {
		t.Errorf("refs/remotes/origin/main after Fetch = %s, want %s", got, wantSHA)
	}
	// The best-effort set-head leaves origin/HEAD tracking the default branch.
	if got := gitCmd(t, home, dest, "symbolic-ref", "refs/remotes/origin/HEAD"); got != "refs/remotes/origin/main" {
		t.Errorf("origin/HEAD after Fetch = %q, want refs/remotes/origin/main", got)
	}

	// Fetch failure surfaces git's stderr verbatim.
	gitCmd(t, home, dest, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	err := eng.Fetch(t.Context(), dest, env)
	if err == nil {
		t.Fatal("Fetch against a missing origin succeeded, want error")
	}
	if !strings.Contains(err.Error(), "does not appear to be a git repository") {
		t.Errorf("Fetch error does not surface git stderr: %v", err)
	}
}

func TestCloneBare_failureSurfacesStderrAndLeavesNoDir(t *testing.T) {
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	dest := filepath.Join(t.TempDir(), "repo.git")
	eng := New("git")

	err := eng.CloneBare(t.Context(), filepath.Join(t.TempDir(), "no-such-repo"), dest,
		testutil.HermeticGitEnv(home), nil)
	if err == nil {
		t.Fatal("CloneBare from a missing origin succeeded, want error")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("clone error does not surface git stderr verbatim: %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("failed clone left destDir behind: stat = %v", statErr)
	}
}

func TestCloneBare_cancelRemovesPartialDir(t *testing.T) {
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	origin := makeOrigin(t, home, "main", 1)

	// A ~16MB incompressible blob keeps the transfer busy long enough for
	// the cancellation (fired on the FIRST progress line) to land mid-clone,
	// so git is killed and leaves a partial destDir for CloneBare to remove.
	blob := make([]byte, 16<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(filepath.Join(origin, "blob.bin"), blob, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	gitCmd(t, home, origin, "add", ".")
	gitCmd(t, home, origin, "commit", "-q", "-m", "blob")

	dest := filepath.Join(t.TempDir(), "repo.git")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := New("git").CloneBare(ctx, "file://"+origin, dest, testutil.HermeticGitEnv(home),
		func(string, int, string) { cancel() })
	if err == nil {
		t.Fatal("cancelled CloneBare succeeded, want error")
	}
	if !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Errorf("cancelled clone error = %v, want it to wrap context.Canceled", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("cancelled clone left partial destDir behind: stat = %v", statErr)
	}
}

// TestProbeBareRepo_anchored: the probe must judge exactly the given dir —
// an empty dir nested inside a git checkout must probe false (no walking
// up to the ancestor .git), GIT_DIR in the process environment must be
// ignored (the scrubbed base env never forwards it), and a real bare repo
// in the same nested spot must probe true.
func TestProbeBareRepo_anchored(t *testing.T) {
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	e := New("git")

	// A checkout whose subdirectories all have an ancestor .git.
	checkout := t.TempDir()
	gitCmd(t, home, "", "init", "-q", checkout)
	t.Setenv("GIT_DIR", filepath.Join(checkout, ".git"))

	empty := filepath.Join(checkout, "repos", "empty.git")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if e.ProbeBareRepo(context.Background(), empty, testutil.HermeticGitEnv(home)) {
		t.Error("empty dir nested in a checkout probed true (unanchored rev-parse resolved the ancestor .git or $GIT_DIR)")
	}

	real := filepath.Join(checkout, "repos", "real.git")
	gitCmd(t, home, "", "init", "-q", "--bare", real)
	if !e.ProbeBareRepo(context.Background(), real, testutil.HermeticGitEnv(home)) {
		t.Error("real bare repo probed false")
	}
}

func TestRunTimeoutErrorShape(t *testing.T) {
	testutil.RequireTool(t, "git")
	eng := New("git")
	eng.timeout = time.Nanosecond
	_, err := eng.run(t.Context(), "", nil, "version")
	if err == nil {
		t.Fatal("run with 1ns timeout succeeded, want timeout error")
	}
	if want := "git version: timed out after 1ns"; err.Error() != want {
		t.Errorf("timeout error = %q, want %q", err.Error(), want)
	}
}
