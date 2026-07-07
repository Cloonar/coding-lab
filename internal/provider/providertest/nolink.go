package providertest

import (
	"context"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
)

// NoLinkFake is a controllable AgentProvider WITHOUT a web surface: it
// implements the mandatory provider.AgentProvider seam but neither
// provider.DeepLinker nor provider.ConnectingReporter (ADR-0017). It stands in
// for a headless-CLI provider (a future Codex, #2) so tests can prove that lab
// arms no deep-link capture for such a provider and keeps its runs'
// deep_link_url NULL, and that its instance rows fall through to the
// tmux-attach affordance. Safe for concurrent use.
type NoLinkFake struct {
	mu sync.Mutex

	id      string
	models  []provider.Option
	efforts []provider.Option

	seeded []string // worktrees passed to SeedWorkspace, in order
}

// Only the mandatory seam — deliberately NOT DeepLinker/ConnectingReporter, so
// a type assertion for either capability fails (the link-less path).
var _ provider.AgentProvider = (*NoLinkFake)(nil)

// NewNoLink returns a logged-in link-less fake with the id "codex-fake" and a
// small model/effort catalog, distinct from the claude-code Fake so both can
// register in one registry.
func NewNoLink() *NoLinkFake {
	return &NoLinkFake{
		id:      "codex-fake",
		models:  []provider.Option{{Value: "gpt-5-codex", Label: "GPT-5 Codex"}},
		efforts: []provider.Option{{Value: "medium", Label: "medium"}},
	}
}

func (f *NoLinkFake) ID() string                 { return f.id }
func (f *NoLinkFake) Models() []provider.Option  { return f.models }
func (f *NoLinkFake) Efforts() []provider.Option { return f.efforts }

// SpawnOptions declares no spawn options — a stand-in for a provider that
// hasn't adopted the generic bag yet (issue #19). Its resolved options bag is
// always empty.
func (f *NoLinkFake) SpawnOptions() []provider.OptionSpec { return nil }

// SpawnArgv mirrors a minimal headless-CLI argv (no --remote-control, no web
// bridge). The seed prompt still rides as the trailing positional.
func (f *NoLinkFake) SpawnArgv(spec provider.SpawnSpec) []string {
	argv := []string{"codex", "run", spec.SessionName}
	if spec.Model != "" {
		argv = append(argv, "--model", spec.Model)
	}
	if spec.Effort != "" {
		argv = append(argv, "--effort", spec.Effort)
	}
	if spec.InitialPrompt != "" {
		argv = append(argv, spec.InitialPrompt)
	}
	return argv
}

func (f *NoLinkFake) AuthStatus(context.Context, bool) (provider.AuthStatus, error) {
	return provider.AuthStatus{LoggedIn: true, Email: "codex@example.invalid", Method: "cli", CheckedAt: time.Now()}, nil
}

func (f *NoLinkFake) LoginStart(context.Context) (string, error)    { return "", nil }
func (f *NoLinkFake) LoginSubmitCode(context.Context, string) error { return nil }

func (f *NoLinkFake) SeedWorkspace(worktree string, _ provider.SeedOpts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seeded = append(f.seeded, worktree)
	return nil
}

// --- chat surface (mandatory on the seam; trivial for a link-less fake) ----

func (f *NoLinkFake) LocateTranscript(context.Context, string, string) (string, error) {
	return "", nil
}
func (f *NoLinkFake) ReadTranscript(string) (provider.Chat, error) { return provider.Chat{}, nil }
func (f *NoLinkFake) Reply(context.Context, string, string) error  { return nil }
func (f *NoLinkFake) AnswerDialog(context.Context, string, provider.Dialog, provider.DialogAnswer) error {
	return nil
}
func (f *NoLinkFake) Interrupt(context.Context, string) error { return nil }

// Seeded returns the worktrees SeedWorkspace was called with (the end-to-end
// spawn assertion).
func (f *NoLinkFake) Seeded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seeded...)
}
