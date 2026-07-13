package claudecode

import (
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

func TestSpawnArgv(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		session, model, eff, prompt string
		options                     map[string]string
		want                        string
		wantLast                    string // expected trailing positional ("" → none)
	}{
		{
			// Pinned M3 constant: {claude} --remote-control <session>
			// --permission-mode auto [--model M] [--effort E].
			name: "full", session: "repo~dom-20260706-0910", model: "opus[1m]", eff: "max",
			want: "claude --remote-control repo~dom-20260706-0910 --permission-mode auto --model opus[1m] --effort max",
		},
		{
			name: "empty model omitted", session: "r~x", model: "", eff: "max",
			want: "claude --remote-control r~x --permission-mode auto --effort max",
		},
		{
			name: "empty effort omitted", session: "r~x", model: "sonnet", eff: "",
			want: "claude --remote-control r~x --permission-mode auto --model sonnet",
		},
		{
			// AFK seed prompt: pinned v0 mechanism — trailing positional AFTER
			// the --model/--effort flags, present before the process starts (no
			// post-spawn keystroke race). One argv element even with spaces.
			name: "seed prompt is the trailing positional", session: "r~afk-7", model: "sonnet", eff: "high", prompt: "resolve issue 7 and open a PR",
			want:     "claude --remote-control r~afk-7 --permission-mode auto --model sonnet --effort high resolve issue 7 and open a PR",
			wantLast: "resolve issue 7 and open a PR",
		},
		{
			// Manual spawns pass "" — no trailing argument at all.
			name: "empty prompt omitted", session: "r~x", model: "sonnet", eff: "high",
			want: "claude --remote-control r~x --permission-mode auto --model sonnet --effort high",
		},
		{
			// ultracode on + a non-empty prompt: the directive line is prepended
			// to the seed prompt, still ONE trailing positional (issue #19).
			name: "ultracode prepends the directive to the seed prompt", session: "r~afk-7", model: "opus[1m]", eff: "max",
			prompt: "resolve #7", options: map[string]string{"ultracode": "true"},
			want:     "claude --remote-control r~afk-7 --permission-mode auto --model opus[1m] --effort max " + ultracodeDirective + "\n\nresolve #7",
			wantLast: ultracodeDirective + "\n\nresolve #7",
		},
		{
			// ultracode on but an EMPTY prompt (manual): natural no-op — no
			// trailing positional at all (ultracode is AFK-only).
			name: "ultracode no-op on empty prompt", session: "r~x", model: "sonnet", eff: "high",
			options: map[string]string{"ultracode": "true"},
			want:    "claude --remote-control r~x --permission-mode auto --model sonnet --effort high",
		},
		{
			// ultracode explicitly off leaves the seed prompt untouched.
			name: "ultracode false leaves the prompt untouched", session: "r~afk-7", model: "sonnet", eff: "high",
			prompt: "resolve #7", options: map[string]string{"ultracode": "false"},
			want:     "claude --remote-control r~afk-7 --permission-mode auto --model sonnet --effort high resolve #7",
			wantLast: "resolve #7",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := SpawnArgv("claude", provider.SpawnSpec{
				SessionName:   tc.session,
				Model:         tc.model,
				Effort:        tc.eff,
				Options:       tc.options,
				InitialPrompt: tc.prompt,
			})
			if got := strings.Join(argv, " "); got != tc.want {
				t.Errorf("SpawnArgv = %q; want %q", got, tc.want)
			}
			// The (possibly ultracode-transformed) prompt is exactly one trailing
			// argv element even with spaces/newlines — never split across the pty.
			if tc.wantLast != "" && argv[len(argv)-1] != tc.wantLast {
				t.Errorf("last argv element = %q; want %q as one positional", argv[len(argv)-1], tc.wantLast)
			}
		})
	}
}

func TestCatalogs_pinnedValuesAndCopies(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())

	wantModels := []provider.Option{
		{Value: "opus[1m]", Label: "Opus (1M)"},
		{Value: "sonnet", Label: "Sonnet"},
		{Value: "fable", Label: "Fable"},
		{Value: "haiku", Label: "Haiku"},
	}
	wantEfforts := []string{"low", "medium", "high", "xhigh", "max"}
	gotModels := p.Models()
	if len(gotModels) != len(wantModels) {
		t.Fatalf("Models() = %+v; want %+v", gotModels, wantModels)
	}
	for i := range wantModels {
		if gotModels[i].Option != wantModels[i] {
			t.Errorf("Models()[%d] = %+v; want %+v", i, gotModels[i].Option, wantModels[i])
		}
		// Issue #156 enrichment: claude clamps unsupported combos itself, so
		// EVERY model offers the full five-entry effort list, and none reports
		// a per-model default (first-entry keeps "low" as the all-unset
		// fallback).
		if len(gotModels[i].Efforts) != len(wantEfforts) {
			t.Fatalf("Models()[%d].Efforts = %+v; want values %v", i, gotModels[i].Efforts, wantEfforts)
		}
		for j, w := range wantEfforts {
			if gotModels[i].Efforts[j].Value != w {
				t.Errorf("Models()[%d].Efforts[%d].Value = %q; want %q", i, j, gotModels[i].Efforts[j].Value, w)
			}
		}
		if gotModels[i].DefaultEffort != "" {
			t.Errorf("Models()[%d].DefaultEffort = %q; want \"\" (claude reports no per-model default)", i, gotModels[i].DefaultEffort)
		}
	}

	gotEfforts := p.Efforts()
	if len(gotEfforts) != len(wantEfforts) {
		t.Fatalf("Efforts() = %+v; want values %v", gotEfforts, wantEfforts)
	}
	for i, w := range wantEfforts {
		if gotEfforts[i].Value != w {
			t.Errorf("Efforts()[%d].Value = %q; want %q", i, gotEfforts[i].Value, w)
		}
	}

	// The settings defaults (opus[1m] / max) must be members of the
	// catalogs, so the unset → defaults path always yields a valid spawn.
	if !provider.HasModelOption(gotModels, "opus[1m]") || !provider.HasOption(gotEfforts, "max") {
		t.Error("settings defaults opus[1m]/max missing from the catalogs")
	}

	// Returned slices are DEEP copies (issue #156) — mutating a returned
	// model's value, nested Efforts, or DefaultEffort must not poison the
	// catalog.
	gotModels[0].Value = "mutated"
	gotModels[0].Efforts[0].Value = "mutated"
	gotModels[0].DefaultEffort = "mutated"
	fresh := p.Models()
	if fresh[0].Value != "opus[1m]" {
		t.Error("Models() exposed internal catalog storage")
	}
	if fresh[0].Efforts[0].Value != "low" || fresh[1].Efforts[0].Value != "low" {
		t.Error("Models() exposed the shared per-model Efforts storage")
	}
	if fresh[0].DefaultEffort != "" {
		t.Error("Models() exposed internal DefaultEffort storage")
	}
}

func TestProviderID(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())
	if p.ID() != "claude-code" || ID != "claude-code" {
		t.Errorf("ID = %q / const %q; want claude-code", p.ID(), ID)
	}
}

func TestNew_requiredOptions(t *testing.T) {
	base := func() Options {
		return Options{
			ClaudeBin:   "claude",
			ConfigPath:  "/tmp/x/.claude.json",
			RegistryDir: "/tmp/x/sessions",
			LoginDir:    "/tmp/x",
			Runner:      newFakeRunner(),
			Bus:         events.NewBus(),
		}
	}
	if _, err := New(base()); err != nil {
		t.Fatalf("New with all options: %v", err)
	}
	// ClaudeBin/ConfigPath are no longer required (issue #78: adapters keep
	// their own defaults when no config entry exists) — only the fields
	// below still fail construction when empty/nil.
	for name, mut := range map[string]func(*Options){
		"RegistryDir": func(o *Options) { o.RegistryDir = "" },
		"LoginDir":    func(o *Options) { o.LoginDir = "" },
		"Runner":      func(o *Options) { o.Runner = nil },
		"Bus":         func(o *Options) { o.Bus = nil },
	} {
		o := base()
		mut(&o)
		if _, err := New(o); err == nil {
			t.Errorf("New without %s succeeded; want error", name)
		}
	}
}

// TestNew_claudeBinConfigPathDefaults pins issue #78's adapter-owned
// defaults: an empty ClaudeBin/ConfigPath no longer errors, it falls back to
// a PATH-resolved "claude" / LoginDir/.claude.json — and an explicit value
// still wins over the default.
func TestNew_claudeBinConfigPathDefaults(t *testing.T) {
	for _, tc := range []struct {
		name           string
		claudeBin      string
		configPath     string
		loginDir       string
		wantClaudeBin  string
		wantConfigPath string
	}{
		{
			name:           "empty ClaudeBin defaults to PATH-resolved claude",
			claudeBin:      "",
			configPath:     "/tmp/x/.claude.json",
			loginDir:       "/tmp/x",
			wantClaudeBin:  "claude",
			wantConfigPath: "/tmp/x/.claude.json",
		},
		{
			name:           "empty ConfigPath defaults to LoginDir/.claude.json",
			claudeBin:      "claude",
			configPath:     "",
			loginDir:       "/tmp/x",
			wantClaudeBin:  "claude",
			wantConfigPath: filepath.Join("/tmp/x", ".claude.json"),
		},
		{
			name:           "both empty default independently",
			claudeBin:      "",
			configPath:     "",
			loginDir:       "/tmp/y",
			wantClaudeBin:  "claude",
			wantConfigPath: filepath.Join("/tmp/y", ".claude.json"),
		},
		{
			name:           "explicit ClaudeBin wins over the default",
			claudeBin:      "/opt/custom/claude",
			configPath:     "/tmp/x/.claude.json",
			loginDir:       "/tmp/x",
			wantClaudeBin:  "/opt/custom/claude",
			wantConfigPath: "/tmp/x/.claude.json",
		},
		{
			name:           "explicit ConfigPath wins over the default",
			claudeBin:      "claude",
			configPath:     "/etc/claude/custom.json",
			loginDir:       "/tmp/x",
			wantClaudeBin:  "claude",
			wantConfigPath: "/etc/claude/custom.json",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(Options{
				ClaudeBin:   tc.claudeBin,
				ConfigPath:  tc.configPath,
				RegistryDir: "/tmp/x/sessions",
				LoginDir:    tc.loginDir,
				Runner:      newFakeRunner(),
				Bus:         events.NewBus(),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if p.claudeBin != tc.wantClaudeBin {
				t.Errorf("claudeBin = %q; want %q", p.claudeBin, tc.wantClaudeBin)
			}
			if p.configPath != tc.wantConfigPath {
				t.Errorf("configPath = %q; want %q", p.configPath, tc.wantConfigPath)
			}
		})
	}
}
