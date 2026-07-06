package testutil

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRequireToolSkipsWhenMissing(t *testing.T) {
	// A skipped subtest reports success without reaching the Fatal.
	ok := t.Run("missing", func(t *testing.T) {
		RequireTool(t, "definitely-not-a-real-tool-8b1f2c")
		t.Fatal("RequireTool did not skip for a missing tool")
	})
	if !ok {
		t.Fatal("subtest failed — RequireTool must skip, not fail")
	}
}

func TestRequireToolPassesWhenPresent(t *testing.T) {
	RequireTool(t, "sh") // present on any platform these tests run on
}

func TestHermeticGitEnvEntries(t *testing.T) {
	env := HermeticGitEnv("/tmp/x")
	want := []string{
		"HOME=/tmp/x",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
	}
	joined := strings.Join(env, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("HermeticGitEnv missing %q:\n%s", w, joined)
		}
	}
	for _, prefix := range []string{"GIT_AUTHOR_NAME=", "GIT_AUTHOR_EMAIL=", "GIT_COMMITTER_NAME=", "GIT_COMMITTER_EMAIL="} {
		found := false
		for _, e := range env {
			if strings.HasPrefix(e, prefix) && len(e) > len(prefix) {
				found = true
			}
		}
		if !found {
			t.Errorf("HermeticGitEnv missing a non-empty %s entry", prefix)
		}
	}
}

// TestHermeticGitEnvMakesGitHermetic proves the env list is sufficient for
// a real git commit in a bare temp dir, with the pinned identity, no matter
// what the host's git config says.
func TestHermeticGitEnvMakesGitHermetic(t *testing.T) {
	RequireTool(t, "git")
	dir := t.TempDir()
	env := append(os.Environ(), HermeticGitEnv(dir)...)

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, errb.String())
		}
		return out.String()
	}

	git("init", "-q")
	if err := os.WriteFile(dir+"/f.txt", []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "f.txt")
	git("commit", "-q", "-m", "test commit")

	got := strings.TrimSpace(git("log", "-1", "--format=%an <%ae>"))
	if got != "lab-test <lab-test@example.invalid>" {
		t.Fatalf("commit identity = %q, want the hermetic identity", got)
	}
}

func TestFakeClock(t *testing.T) {
	start := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	c := NewFakeClock(start)

	if !c.Now().Equal(start) {
		t.Fatalf("Now = %v, want %v", c.Now(), start)
	}
	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("Now moved on its own: %v", got)
	}

	c.Advance(90 * time.Minute)
	want := start.Add(90 * time.Minute)
	if !c.Now().Equal(want) {
		t.Fatalf("Now after Advance = %v, want %v", c.Now(), want)
	}
}

func TestTempStore(t *testing.T) {
	s := TempStore(t)
	n, err := s.CountUsers(context.Background())
	if err != nil {
		t.Fatalf("count users on temp store: %v", err)
	}
	if n != 0 {
		t.Fatalf("fresh temp store has %d users, want 0", n)
	}
}
