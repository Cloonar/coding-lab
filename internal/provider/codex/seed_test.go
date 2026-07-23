package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// A missing config.toml is created (parent dirs included) containing exactly
// the appended trust table.
func TestSeedTrust_createsFreshConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "deep", ".codex", "config.toml")
	if err := SeedTrust(cfg, "/work/wt-1"); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}
	want := "\n[projects.\"/work/wt-1\"]\ntrust_level = \"trusted\"\n"
	if got := readFile(t, cfg); got != want {
		t.Errorf("fresh config = %q; want %q", got, want)
	}
	info, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("config mode = %o; want 0644", perm)
	}
}

// Appending preserves existing content byte-for-byte as a prefix — the file
// is codex's own config, append-only, never rewritten or reordered.
func TestSeedTrust_appendsPreservingContent(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.toml")
	existing := "model = \"gpt-5.5\"\n\n[projects.\"/other/dir\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(cfg, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SeedTrust(cfg, "/work/wt-2"); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}
	got := readFile(t, cfg)
	if !strings.HasPrefix(got, existing) {
		t.Errorf("existing content not preserved as prefix:\n%q", got)
	}
	if want := existing + "\n[projects.\"/work/wt-2\"]\ntrust_level = \"trusted\"\n"; got != want {
		t.Errorf("appended config = %q; want %q", got, want)
	}
}

// A file without a trailing newline gets one before the block so the header
// starts on its own line (the containment guard depends on it).
func TestSeedTrust_repairsMissingTrailingNewline(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte(`model = "gpt-5.5"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SeedTrust(cfg, "/work/wt-3"); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}
	want := "model = \"gpt-5.5\"\n\n[projects.\"/work/wt-3\"]\ntrust_level = \"trusted\"\n"
	if got := readFile(t, cfg); got != want {
		t.Errorf("config = %q; want %q", got, want)
	}
}

// The guard is the header line, NOT the trust value: codex auto-writes trust
// entries for directories it has run in (and may set any trust_level), and
// an existing entry — whatever its value — means hands off.
func TestSeedTrust_skipsWhenHeaderPresent(t *testing.T) {
	for name, content := range map[string]string{
		"seeded trusted":                "\n[projects.\"/work/wt-4\"]\ntrust_level = \"trusted\"\n",
		"codex-written untrusted":       "[projects.\"/work/wt-4\"]\ntrust_level = \"none\"\n",
		"codex-written with extra keys": "preferred_auth_method = \"chatgpt\"\n[projects.\"/work/wt-4\"]\ntrust_level = \"trusted\"\nsomething = 1\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(cfg, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := SeedTrust(cfg, "/work/wt-4"); err != nil {
				t.Fatalf("SeedTrust: %v", err)
			}
			if got := readFile(t, cfg); got != content {
				t.Errorf("config mutated despite existing header:\n got %q\nwant %q", got, content)
			}
		})
	}
}

// Backslashes and double quotes in the worktree path are TOML-escaped in the
// header, and the guard still recognizes the escaped header on re-seed.
func TestSeedTrust_escapesTOMLSpecials(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.toml")
	dir := `/work/we"ird\name`
	if err := SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust: %v", err)
	}
	want := "\n[projects.\"/work/we\\\"ird\\\\name\"]\ntrust_level = \"trusted\"\n"
	got := readFile(t, cfg)
	if got != want {
		t.Errorf("escaped config = %q; want %q", got, want)
	}
	if err := SeedTrust(cfg, dir); err != nil {
		t.Fatalf("SeedTrust re-seed: %v", err)
	}
	if again := readFile(t, cfg); again != got {
		t.Errorf("re-seed mutated the escaped entry:\n got %q\nwant %q", again, got)
	}
}

// The bridge is created when the global AGENTS.md is missing, appended when
// present but unmarked, and left alone when the marker is already there.
func TestSeedAgentsBridge_createAppendSkip(t *testing.T) {
	t.Run("create when missing", func(t *testing.T) {
		agents := filepath.Join(t.TempDir(), ".codex", "AGENTS.md")
		if err := SeedAgentsBridge(agents); err != nil {
			t.Fatalf("SeedAgentsBridge: %v", err)
		}
		got := readFile(t, agents)
		if got != agentsBridgeBlock {
			t.Errorf("fresh AGENTS.md = %q; want exactly the bridge block %q", got, agentsBridgeBlock)
		}
		if !strings.HasPrefix(got, agentsBridgeMarker+"\n") {
			t.Error("bridge block must start with the marker line")
		}
	})
	t.Run("append when unmarked", func(t *testing.T) {
		agents := filepath.Join(t.TempDir(), "AGENTS.md")
		existing := "# My global instructions\n\nAlways be terse.\n"
		if err := os.WriteFile(agents, []byte(existing), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := SeedAgentsBridge(agents); err != nil {
			t.Fatalf("SeedAgentsBridge: %v", err)
		}
		got := readFile(t, agents)
		if !strings.HasPrefix(got, existing) {
			t.Errorf("operator's own instructions not preserved:\n%q", got)
		}
		if want := existing + "\n" + agentsBridgeBlock; got != want {
			t.Errorf("appended AGENTS.md = %q; want %q", got, want)
		}
	})
	t.Run("skip when marked", func(t *testing.T) {
		agents := filepath.Join(t.TempDir(), "AGENTS.md")
		existing := "intro\n\n" + agentsBridgeBlock + "\ntrailing operator text\n"
		if err := os.WriteFile(agents, []byte(existing), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := SeedAgentsBridge(agents); err != nil {
			t.Fatalf("SeedAgentsBridge: %v", err)
		}
		if got := readFile(t, agents); got != existing {
			t.Errorf("marked AGENTS.md mutated:\n got %q\nwant %q", got, existing)
		}
	})
}

// SeedWorkspace writes NOTHING inside the worktree (both grants are HOME-global,
// landing under the run's private HOME per issue #202), leaves the master store
// untouched, is idempotent (a second run changes no bytes), and Incogni is a
// no-op — codex 0.133 writes no attribution at the source, so there is nothing
// to disable.
func TestSeedWorkspace_globalOnlyIdempotentIncogniNoop(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	worktree := t.TempDir()
	home := t.TempDir()
	codexHome := instanceCodexHome(home)
	cfgPath := filepath.Join(codexHome, "config.toml")
	agentsPath := filepath.Join(codexHome, "AGENTS.md")

	if err := p.SeedWorkspace(worktree, provider.SeedOpts{Home: home}); err != nil {
		t.Fatalf("SeedWorkspace: %v", err)
	}
	entries, err := os.ReadDir(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("SeedWorkspace wrote into the worktree: %v", entries)
	}

	cfg := readFile(t, cfgPath)
	agents := readFile(t, agentsPath)
	if !strings.Contains(cfg, projectsHeader(worktree)) {
		t.Errorf("instance config.toml missing the worktree trust table:\n%q", cfg)
	}
	if !strings.Contains(agents, agentsBridgeMarker) {
		t.Errorf("instance AGENTS.md missing the bridge marker:\n%q", agents)
	}
	// The MASTER config.toml is never touched — the grant stays under the HOME.
	if _, err := os.Stat(p.configPath); !os.IsNotExist(err) {
		t.Errorf("master config.toml written; the trust grant must stay under the instance HOME")
	}

	// Second run — Incogni on — must change nothing anywhere.
	if err := p.SeedWorkspace(worktree, provider.SeedOpts{Home: home, Incogni: true}); err != nil {
		t.Fatalf("SeedWorkspace (incogni re-run): %v", err)
	}
	if got := readFile(t, cfgPath); got != cfg {
		t.Error("second SeedWorkspace mutated config.toml")
	}
	if got := readFile(t, agentsPath); got != agents {
		t.Error("second SeedWorkspace mutated the global AGENTS.md")
	}
	entries, err = os.ReadDir(worktree)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("incogni SeedWorkspace wrote into the worktree: %v", entries)
	}
}

// An empty Home skips both HOME-global grants — a run with no per-run home gets
// no global write and never touches the master store (issue #202).
func TestSeedWorkspace_emptyHomeSkipsGlobalGrants(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	worktree := t.TempDir()

	if err := p.SeedWorkspace(worktree, provider.SeedOpts{}); err != nil {
		t.Fatalf("SeedWorkspace: %v", err)
	}
	if entries, _ := os.ReadDir(worktree); len(entries) != 0 {
		t.Errorf("empty-Home SeedWorkspace wrote into the worktree: %v", entries)
	}
	if _, err := os.Stat(p.configPath); !os.IsNotExist(err) {
		t.Errorf("empty-Home SeedWorkspace wrote the master config.toml")
	}
}

// TestSeedWorkspace_masterStoreDecoyUntouched proves the isolation claim with
// teeth against codex's OWN master-store resolution: $CODEX_HOME is pointed at
// a decoy dir already carrying an operator's own config.toml/AGENTS.md (so a
// wrongly-targeted write would corrupt real content, not just create a file).
// SeedWorkspace(Home=home) must land both grants under <home>/.codex and leave
// the decoy files byte-for-byte untouched — whether Home is set or empty.
func TestSeedWorkspace_masterStoreDecoyUntouched(t *testing.T) {
	master := t.TempDir()
	t.Setenv("CODEX_HOME", master)
	decoyCfg := filepath.Join(master, "config.toml")
	decoyAgents := filepath.Join(master, "AGENTS.md")
	const decoyCfgContent = "model = \"gpt-5.5\"\n"
	const decoyAgentsContent = "# operator's own global codex instructions\n"
	if err := os.WriteFile(decoyCfg, []byte(decoyCfgContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(decoyAgents, []byte(decoyAgentsContent), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("Home set", func(t *testing.T) {
		p, _ := testProvider(t, newFakeRunner())
		worktree := t.TempDir()
		home := t.TempDir()

		if err := p.SeedWorkspace(worktree, provider.SeedOpts{Home: home}); err != nil {
			t.Fatalf("SeedWorkspace: %v", err)
		}
		if got := readFile(t, decoyCfg); got != decoyCfgContent {
			t.Errorf("decoy master config.toml mutated:\n got %q\nwant %q", got, decoyCfgContent)
		}
		if got := readFile(t, decoyAgents); got != decoyAgentsContent {
			t.Errorf("decoy master AGENTS.md mutated:\n got %q\nwant %q", got, decoyAgentsContent)
		}
		// The grants DID land, just under the instance home, not the decoy.
		codexHome := instanceCodexHome(home)
		if got := readFile(t, filepath.Join(codexHome, "config.toml")); !strings.Contains(got, projectsHeader(worktree)) {
			t.Errorf("instance config.toml missing the trust table:\n%q", got)
		}
		if got := readFile(t, filepath.Join(codexHome, "AGENTS.md")); !strings.Contains(got, agentsBridgeMarker) {
			t.Errorf("instance AGENTS.md missing the bridge marker:\n%q", got)
		}
	})

	t.Run("Home empty", func(t *testing.T) {
		p, _ := testProvider(t, newFakeRunner())
		worktree := t.TempDir()

		if err := p.SeedWorkspace(worktree, provider.SeedOpts{}); err != nil {
			t.Fatalf("SeedWorkspace: %v", err)
		}
		if got := readFile(t, decoyCfg); got != decoyCfgContent {
			t.Errorf("decoy master config.toml mutated despite empty Home:\n got %q\nwant %q", got, decoyCfgContent)
		}
		if got := readFile(t, decoyAgents); got != decoyAgentsContent {
			t.Errorf("decoy master AGENTS.md mutated despite empty Home:\n got %q\nwant %q", got, decoyAgentsContent)
		}
	})
}
