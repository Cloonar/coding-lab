package provider_test

import (
	"path/filepath"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/claudecode"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/codex"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// conformanceProviders is the per-adapter registration table of the Tier-1
// hermetic conformance suite (issue #80 acceptance: adding an adapter to lab
// means adding ONE entry here). Each make constructs its provider with
// everything pointed into t.TempDir() — ConfigPath especially, or the seeding
// checks would write the operator's real global config — plus the tmuxx fake
// runner and a fresh bus, and hands the suite the adapter's ground-truth
// Fixture: real attribution lines its CLI writes (issue #75) and near-miss
// clean lines its patterns must not catch.
var conformanceProviders = []struct {
	name string
	make func(t *testing.T) (provider.AgentProvider, providertest.Fixture)
}{
	{
		name: "claude-code",
		make: func(t *testing.T) (provider.AgentProvider, providertest.Fixture) {
			p, err := claudecode.New(claudecode.Options{
				ClaudeBin:   "claude-not-invoked", // the suite never spawns the CLI
				ConfigPath:  filepath.Join(t.TempDir(), ".claude.json"),
				RegistryDir: filepath.Join(t.TempDir(), "sessions"),
				LoginDir:    t.TempDir(),
				Runner:      tmuxx.NewFake(),
				Bus:         events.NewBus(),
			})
			if err != nil {
				t.Fatalf("claudecode.New: %v", err)
			}
			// The canonical live marker samples (the vectors pinned in
			// claudecode's seedmeta test and agentapi's sanitizer test).
			return p, providertest.Fixture{
				AttributionSamples: []string{
					"Co-Authored-By: Claude <noreply@anthropic.com>",
					"Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>",
					"🤖 Generated with [Claude Code](https://claude.com/claude-code)",
					"Claude-Session: https://claude.ai/code/session_x",
				},
				CleanSamples: []string{
					"Co-Authored-By: Alice <alice@example.com>",
					"Docs generated with pandoc.",
				},
			}
		},
	},
	{
		name: "codex",
		make: func(t *testing.T) (provider.AgentProvider, providertest.Fixture) {
			p, err := codex.New(codex.Options{
				CodexBin:    "codex-not-invoked", // the suite never spawns the CLI
				ConfigPath:  filepath.Join(t.TempDir(), "config.toml"),
				SessionsDir: filepath.Join(t.TempDir(), "sessions"),
				AgentsFile:  filepath.Join(t.TempDir(), "AGENTS.md"),
				LoginDir:    t.TempDir(),
				Runner:      tmuxx.NewFake(),
				Bus:         events.NewBus(),
			})
			if err != nil {
				t.Fatalf("codex.New: %v", err)
			}
			// codex 0.133 writes NO attribution at the source (the
			// codex_git_commit feature is off) — these are the DEFENSIVE marker
			// shapes the declared ScrubPatterns must catch if a future version
			// turns attribution on (issue #87 / ADR-0033).
			return p, providertest.Fixture{
				AttributionSamples: []string{
					"Co-authored-by: Codex <noreply@openai.com>",
					"Co-authored-by: ChatGPT Codex <bot@openai.com>",
					"Generated with Codex",
				},
				CleanSamples: []string{
					"Co-authored-by: Alice <alice@example.com>",
					"The openai.com docs describe the responses API.",
				},
			}
		},
	},
}

// TestConformance runs the Tier-1 suite against every registered adapter in
// ordinary CI — hermetically (no agent CLI, no tmux server, no network).
func TestConformance(t *testing.T) {
	for _, entry := range conformanceProviders {
		t.Run(entry.name, func(t *testing.T) {
			p, fx := entry.make(t)
			providertest.Conformance(t, p, fx)
		})
	}
}
