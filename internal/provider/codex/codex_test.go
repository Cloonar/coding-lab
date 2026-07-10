package codex

import (
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

func TestSpawnArgv(t *testing.T) {
	const base = `codex --ask-for-approval never --sandbox danger-full-access -c project_doc_fallback_filenames=["AGENTS.local.md"]`
	for _, tc := range []struct {
		name               string
		model, eff, prompt string
		want               string
		wantLast           string // expected trailing positional ("" → none)
	}{
		{
			// Pinned 0.133.0 shape: never-ask + full sandbox + the
			// AGENTS.local.md fallback, model flag, explicit effort.
			name: "full", model: "gpt-5.5", eff: "high",
			want: base + " --model gpt-5.5 -c model_reasoning_effort=high",
		},
		{
			name: "empty model omitted", model: "", eff: "low",
			want: base + " -c model_reasoning_effort=low",
		},
		{
			// codex exec defaults effort to NONE, so an empty effort injects
			// medium instead of omitting the flag (pinned spike finding).
			name: "empty effort injects medium", model: "gpt-5.4-mini", eff: "",
			want: base + " --model gpt-5.4-mini -c model_reasoning_effort=medium",
		},
		{
			// AFK seed prompt: trailing positional AFTER the flags, one argv
			// element even with spaces.
			name: "seed prompt is the trailing positional", model: "gpt-5.5", eff: "medium",
			prompt:   "resolve issue 87 and open a PR",
			want:     base + " --model gpt-5.5 -c model_reasoning_effort=medium resolve issue 87 and open a PR",
			wantLast: "resolve issue 87 and open a PR",
		},
		{
			// Manual spawns pass "" — no trailing argument at all.
			name: "empty prompt omitted", model: "gpt-5.5", eff: "medium",
			want: base + " --model gpt-5.5 -c model_reasoning_effort=medium",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := SpawnArgv("codex", provider.SpawnSpec{
				SessionName:   "r~x", // unused: codex has no --remote-control equivalent
				Model:         tc.model,
				Effort:        tc.eff,
				InitialPrompt: tc.prompt,
			})
			if got := strings.Join(argv, " "); got != tc.want {
				t.Errorf("SpawnArgv = %q; want %q", got, tc.want)
			}
			if tc.wantLast != "" && argv[len(argv)-1] != tc.wantLast {
				t.Errorf("last argv element = %q; want %q as one positional", argv[len(argv)-1], tc.wantLast)
			}
			// The -c values are single argv elements — no shell quoting.
			for i, arg := range argv {
				if arg == "-c" && i+1 >= len(argv) {
					t.Errorf("dangling -c at argv[%d]", i)
				}
			}
			// The pinned no-attribution invariant: commit_attribution is an
			// UNKNOWN config field on 0.133 (hard error under --strict-config)
			// and attribution is absent at source — it must never be emitted.
			for i, arg := range argv {
				if strings.Contains(arg, "commit_attribution") {
					t.Errorf("argv[%d] = %q; commit_attribution must not be emitted (unknown field on 0.133)", i, arg)
				}
			}
		})
	}
}

func TestCatalogs_pinnedValuesAndCopies(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())

	wantModels := []provider.Option{
		{Value: "gpt-5.5", Label: "GPT-5.5"},
		{Value: "gpt-5.4-mini", Label: "GPT-5.4-Mini"},
	}
	gotModels := p.Models()
	if len(gotModels) != len(wantModels) {
		t.Fatalf("Models() = %+v; want %+v", gotModels, wantModels)
	}
	for i := range wantModels {
		if gotModels[i] != wantModels[i] {
			t.Errorf("Models()[%d] = %+v; want %+v", i, gotModels[i], wantModels[i])
		}
	}

	wantEfforts := []provider.Option{
		{Value: "low", Label: "Low"},
		{Value: "medium", Label: "Medium"},
		{Value: "high", Label: "High"},
		{Value: "xhigh", Label: "Extra high"},
	}
	gotEfforts := p.Efforts()
	if len(gotEfforts) != len(wantEfforts) {
		t.Fatalf("Efforts() = %+v; want %+v", gotEfforts, wantEfforts)
	}
	for i := range wantEfforts {
		if gotEfforts[i] != wantEfforts[i] {
			t.Errorf("Efforts()[%d] = %+v; want %+v", i, gotEfforts[i], wantEfforts[i])
		}
	}

	// The effort-default injection target must be a catalog member.
	if !provider.HasOption(gotEfforts, defaultEffort) {
		t.Errorf("defaultEffort %q missing from the effort catalog", defaultEffort)
	}

	// codex declares no generic spawn options.
	if opts := p.SpawnOptions(); len(opts) != 0 {
		t.Errorf("SpawnOptions() = %+v; want none declared", opts)
	}

	// Returned slices are copies — a caller mutation must not poison the
	// catalog.
	gotModels[0].Value = "mutated"
	if p.Models()[0].Value != "gpt-5.5" {
		t.Error("Models() exposed internal catalog storage")
	}
}

func TestProviderIdentity(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	if p.ID() != "codex" || ID != "codex" {
		t.Errorf("ID = %q / const %q; want codex", p.ID(), ID)
	}
	if p.DisplayName() != "Codex" {
		t.Errorf("DisplayName = %q; want Codex", p.DisplayName())
	}
	flow := p.AuthFlow()
	if flow.Kind != provider.AuthFlowDeviceCode {
		t.Errorf("AuthFlow().Kind = %q; want %q", flow.Kind, provider.AuthFlowDeviceCode)
	}
	if flow.Instructions == "" {
		t.Error("AuthFlow().Instructions is empty; want the device-code operator guidance")
	}
}

func TestNew_requiredOptions(t *testing.T) {
	base := func() Options {
		return Options{
			CodexBin:    "codex",
			ConfigPath:  "/tmp/x/config.toml",
			SessionsDir: "/tmp/x/sessions",
			AgentsFile:  "/tmp/x/AGENTS.md",
			LoginDir:    "/tmp/x",
			Runner:      newFakeRunner(),
			Bus:         events.NewBus(),
		}
	}
	if _, err := New(base()); err != nil {
		t.Fatalf("New with all options: %v", err)
	}
	for name, mut := range map[string]func(*Options){
		"LoginDir": func(o *Options) { o.LoginDir = "" },
		"Runner":   func(o *Options) { o.Runner = nil },
		"Bus":      func(o *Options) { o.Bus = nil },
	} {
		o := base()
		mut(&o)
		if _, err := New(o); err == nil {
			t.Errorf("New without %s succeeded; want error", name)
		}
	}
}

// The path defaults resolve under codex's own home ($CODEX_HOME when set,
// else LoginDir/.codex); explicit values always win (issue #78:
// adapter-owned defaults).
func TestNew_pathDefaults(t *testing.T) {
	t.Run("defaults under LoginDir/.codex", func(t *testing.T) {
		t.Setenv("CODEX_HOME", "")
		p, err := New(Options{LoginDir: "/home/x", Runner: newFakeRunner(), Bus: events.NewBus()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		home := filepath.Join("/home/x", ".codex")
		if p.codexBin != "codex" {
			t.Errorf("codexBin = %q; want PATH-resolved codex", p.codexBin)
		}
		if p.configPath != filepath.Join(home, "config.toml") {
			t.Errorf("configPath = %q; want %q", p.configPath, filepath.Join(home, "config.toml"))
		}
		if p.sessionsDir != filepath.Join(home, "sessions") {
			t.Errorf("sessionsDir = %q; want %q", p.sessionsDir, filepath.Join(home, "sessions"))
		}
		if p.agentsFile != filepath.Join(home, "AGENTS.md") {
			t.Errorf("agentsFile = %q; want %q", p.agentsFile, filepath.Join(home, "AGENTS.md"))
		}
	})
	t.Run("CODEX_HOME wins over LoginDir/.codex", func(t *testing.T) {
		t.Setenv("CODEX_HOME", "/srv/codex-home")
		p, err := New(Options{LoginDir: "/home/x", Runner: newFakeRunner(), Bus: events.NewBus()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if p.configPath != filepath.Join("/srv/codex-home", "config.toml") {
			t.Errorf("configPath = %q; want under $CODEX_HOME", p.configPath)
		}
	})
	t.Run("explicit paths win over every default", func(t *testing.T) {
		t.Setenv("CODEX_HOME", "/srv/codex-home")
		p, err := New(Options{
			CodexBin:    "/opt/custom/codex",
			ConfigPath:  "/etc/codex/custom.toml",
			SessionsDir: "/var/rollouts",
			AgentsFile:  "/etc/codex/AGENTS.md",
			LoginDir:    "/home/x",
			Runner:      newFakeRunner(),
			Bus:         events.NewBus(),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if p.codexBin != "/opt/custom/codex" || p.configPath != "/etc/codex/custom.toml" ||
			p.sessionsDir != "/var/rollouts" || p.agentsFile != "/etc/codex/AGENTS.md" {
			t.Errorf("explicit paths lost to defaults: %+v", p)
		}
	})
}
