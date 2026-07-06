// Package testutil provides the shared helpers for lab's test suites:
// tool-presence skips, the hermetic git environment (design §11), a fake
// clock for clock-injected services, and a temp sqlite store.
package testutil

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// RequireTool skips t when the named binary is not on PATH.
func RequireTool(t testing.TB, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("required tool %q not on PATH: %v", name, err)
	}
}

// HermeticGitEnv returns the environment entries that make git calls in
// tests hermetic (design §11): a throwaway HOME (also fixes the nix
// sandbox's /homeless-shelter), no user or system git config, and a fixed
// author/committer identity. Use it on top of the ambient environment:
//
//	cmd.Env = append(os.Environ(), testutil.HermeticGitEnv(dir)...)
//
// (later entries win, so these override any ambient values).
func HermeticGitEnv(dir string) []string {
	return []string{
		"HOME=" + dir,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=lab-test",
		"GIT_AUTHOR_EMAIL=lab-test@example.invalid",
		"GIT_COMMITTER_NAME=lab-test",
		"GIT_COMMITTER_EMAIL=lab-test@example.invalid",
	}
}

// FakeClock is a mutex-guarded manual clock. Inject its Now method into
// clock-injected services (design §2: no time.Now() inside decision logic).
type FakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// NewFakeClock returns a clock frozen at start.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{t: start}
}

// Now returns the current fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward by d (negative d moves it back).
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TempStore opens a fully migrated sqlite store on a file in t.TempDir()
// (design §11: file DBs by default, never a pooled :memory:) and closes it
// on cleanup.
func TempStore(t testing.TB) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), "sqlite:"+filepath.Join(t.TempDir(), "lab.db"), logx.New(io.Discard))
	if err != nil {
		t.Fatalf("open temp store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close temp store: %v", err)
		}
	})
	return s
}
