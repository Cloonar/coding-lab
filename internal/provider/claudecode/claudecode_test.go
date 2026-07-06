package claudecode

import (
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

func TestSpawnArgv(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		session, model, eff, prompt string
		want                        string
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
			want: "claude --remote-control r~afk-7 --permission-mode auto --model sonnet --effort high resolve issue 7 and open a PR",
		},
		{
			// Manual spawns pass "" — no trailing argument at all.
			name: "empty prompt omitted", session: "r~x", model: "sonnet", eff: "high",
			want: "claude --remote-control r~x --permission-mode auto --model sonnet --effort high",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv := SpawnArgv("claude", tc.session, tc.model, tc.eff, tc.prompt)
			if got := strings.Join(argv, " "); got != tc.want {
				t.Errorf("SpawnArgv = %q; want %q", got, tc.want)
			}
			// The seed prompt is exactly one trailing argv element even with
			// spaces — never split, never fragmented across the pty.
			if tc.prompt != "" && argv[len(argv)-1] != tc.prompt {
				t.Errorf("last argv element = %q; want the seed prompt %q as one positional", argv[len(argv)-1], tc.prompt)
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
	gotModels := p.Models()
	if len(gotModels) != len(wantModels) {
		t.Fatalf("Models() = %+v; want %+v", gotModels, wantModels)
	}
	for i := range wantModels {
		if gotModels[i] != wantModels[i] {
			t.Errorf("Models()[%d] = %+v; want %+v", i, gotModels[i], wantModels[i])
		}
	}

	wantEfforts := []string{"low", "medium", "high", "xhigh", "max"}
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
	if !provider.HasOption(gotModels, "opus[1m]") || !provider.HasOption(gotEfforts, "max") {
		t.Error("settings defaults opus[1m]/max missing from the catalogs")
	}

	// Returned slices are copies — a caller mutation must not poison the
	// catalog.
	gotModels[0].Value = "mutated"
	if p.Models()[0].Value != "opus[1m]" {
		t.Error("Models() exposed internal catalog storage")
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
	for name, mut := range map[string]func(*Options){
		"ClaudeBin":   func(o *Options) { o.ClaudeBin = "" },
		"ConfigPath":  func(o *Options) { o.ConfigPath = "" },
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
