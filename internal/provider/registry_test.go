package provider

import (
	"context"
	"testing"
)

// stubProvider is the minimal AgentProvider for registry tests.
type stubProvider struct{ id string }

func (s stubProvider) ID() string                   { return s.id }
func (s stubProvider) Models() []Option             { return nil }
func (s stubProvider) Efforts() []Option            { return nil }
func (s stubProvider) SpawnOptions() []OptionSpec   { return nil }
func (s stubProvider) SpawnArgv(SpawnSpec) []string { return nil }
func (s stubProvider) AuthStatus(context.Context, bool) (AuthStatus, error) {
	return AuthStatus{}, nil
}
func (s stubProvider) LoginStart(context.Context) (string, error)    { return "", nil }
func (s stubProvider) LoginSubmitCode(context.Context, string) error { return nil }
func (s stubProvider) CaptureDeepLink(context.Context, string, string) (string, error) {
	return "", nil
}
func (s stubProvider) SeedWorkspace(string, SeedOpts) error { return nil }
func (s stubProvider) LocateTranscript(context.Context, string, string) (string, error) {
	return "", nil
}
func (s stubProvider) ReadTranscript(string) (Chat, error)         { return Chat{}, nil }
func (s stubProvider) Reply(context.Context, string, string) error { return nil }
func (s stubProvider) AnswerDialog(context.Context, string, Dialog, DialogAnswer) error {
	return nil
}
func (s stubProvider) Interrupt(context.Context, string) error { return nil }

func TestRegistry_GetAndListOrder(t *testing.T) {
	a, b := stubProvider{id: "claude-code"}, stubProvider{id: "other"}
	r, err := NewRegistry(a, b)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	got, ok := r.Get("claude-code")
	if !ok || got.ID() != "claude-code" {
		t.Errorf("Get(claude-code) = %v, %v; want the provider", got, ok)
	}
	if _, ok := r.Get("nope"); ok {
		t.Error("Get(nope) found a provider; want miss")
	}

	list := r.List()
	if len(list) != 2 || list[0].ID() != "claude-code" || list[1].ID() != "other" {
		t.Errorf("List order = %v; want registration order [claude-code other]", ids(list))
	}
}

func TestRegistry_rejectsDuplicateAndEmptyIDs(t *testing.T) {
	if _, err := NewRegistry(stubProvider{id: "x"}, stubProvider{id: "x"}); err == nil {
		t.Error("duplicate id accepted; want error")
	}
	if _, err := NewRegistry(stubProvider{id: ""}); err == nil {
		t.Error("empty id accepted; want error")
	}
}

func TestHasOption(t *testing.T) {
	opts := []Option{{Value: "opus[1m]", Label: "Opus (1M)"}, {Value: "sonnet", Label: "Sonnet"}}
	if !HasOption(opts, "sonnet") {
		t.Error("HasOption(sonnet) = false; want true")
	}
	if HasOption(opts, "Sonnet") {
		t.Error("HasOption(Sonnet) = true; want false (values are exact)")
	}
	if HasOption(nil, "x") {
		t.Error("HasOption on empty catalog = true; want false")
	}
}

func ids(ps []AgentProvider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID()
	}
	return out
}
