package codex

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// newFakeRunner returns the shared tmuxx fake (design §4d: the fake lives
// next to the interface).
func newFakeRunner() *tmuxx.Fake { return tmuxx.NewFake() }

// testProvider builds a Provider with every path pointed into t.TempDir()
// (hermetic: no env, no real HOME) and tiny poll timeouts. Callers
// shrink/override further where the timing is the contract.
func testProvider(t *testing.T, run tmuxx.SessionRunner) (*Provider, *events.Bus) {
	t.Helper()
	bus := events.NewBus()
	p, err := New(Options{
		CodexBin:    "codex-not-invoked", // tests that exec set a real script
		ConfigPath:  filepath.Join(t.TempDir(), "config.toml"),
		SessionsDir: filepath.Join(t.TempDir(), "sessions"),
		AgentsFile:  filepath.Join(t.TempDir(), "AGENTS.md"),
		LoginDir:    t.TempDir(),
		Runner:      run,
		Bus:         bus,
		Logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.captureTimeout = 500 * time.Millisecond
	p.loginPoll = 10 * time.Millisecond
	p.loginWindow = 2 * time.Second
	p.keyDelay = 0 // no paste→Enter sleep in unit tests
	return p, bus
}

// fakeCodex writes an executable sh script standing in for the codex binary
// and returns its path.
func fakeCodex(t *testing.T, script string) string {
	t.Helper()
	testutil.RequireTool(t, "sh")
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
