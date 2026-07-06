package claudecode

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// loginRunner wraps the shared tmuxx.Fake (design §4d: the fake lives next
// to the interface) with the two extras the login-flow tests need: a
// fresh-start counter (the Fake's Start is idempotent, so double-spawns
// would be invisible) and an after-SendKeys hook (to flip the fake
// claude's persisted auth state, like the real `claude auth login` does
// when it accepts a code).
type loginRunner struct {
	*tmuxx.Fake

	mu         sync.Mutex
	starts     int
	onSendKeys func(name, text string, enter bool)
}

func newLoginRunner() *loginRunner { return &loginRunner{Fake: tmuxx.NewFake()} }

func (r *loginRunner) Start(ctx context.Context, name, dir string, argv []string, extraEnv []string) error {
	live, err := r.IsRunning(ctx, name)
	if err != nil {
		return err
	}
	if err := r.Fake.Start(ctx, name, dir, argv, extraEnv); err != nil {
		return err
	}
	if !live {
		r.mu.Lock()
		r.starts++
		r.mu.Unlock()
	}
	return nil
}

func (r *loginRunner) SendKeys(ctx context.Context, name, text string, enter bool) error {
	if err := r.Fake.SendKeys(ctx, name, text, enter); err != nil {
		return err
	}
	r.mu.Lock()
	hook := r.onSendKeys
	r.mu.Unlock()
	if hook != nil {
		hook(name, text, enter)
	}
	return nil
}

func (r *loginRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

// newFakeRunner returns the plain shared tmuxx fake for tests that don't
// need the login extras.
func newFakeRunner() *tmuxx.Fake { return tmuxx.NewFake() }

// testProvider builds a Provider over run with tiny poll timeouts.
// Callers shrink/override further where the timing is the contract.
func testProvider(t *testing.T, run tmuxx.SessionRunner) (*Provider, *events.Bus) {
	t.Helper()
	bus := events.NewBus()
	p, err := New(Options{
		ClaudeBin:   "claude-not-invoked", // tests that exec set a real script
		ConfigPath:  filepath.Join(t.TempDir(), ".claude.json"),
		RegistryDir: filepath.Join(t.TempDir(), "sessions"),
		LoginDir:    t.TempDir(),
		Runner:      run,
		Bus:         bus,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.captureTimeout = 500 * time.Millisecond
	p.loginTimeout = 500 * time.Millisecond
	p.loginPoll = 10 * time.Millisecond
	p.bridgeTimeout = 300 * time.Millisecond
	return p, bus
}

// fakeClaude writes an executable sh script standing in for the claude
// binary and returns its path.
func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	testutil.RequireTool(t, "sh")
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
