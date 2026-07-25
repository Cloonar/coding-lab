package provider

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// stubProvider is the minimal AgentProvider for registry tests. seedMeta lets
// a test script SeedMeta() (e.g. ScrubPatterns/SeededPathPatterns for the
// union tests); the zero value returns SeedMeta{}, as before.
type stubProvider struct {
	id       string
	seedMeta SeedMeta
}

func (s stubProvider) ID() string                   { return s.id }
func (s stubProvider) DisplayName() string          { return s.id }
func (s stubProvider) Models() []ModelOption        { return nil }
func (s stubProvider) Efforts() []Option            { return nil }
func (s stubProvider) SpawnOptions() []OptionSpec   { return nil }
func (s stubProvider) SpawnArgv(SpawnSpec) []string { return nil }
func (s stubProvider) AuthFlow() AuthFlow           { return AuthFlow{Kind: AuthFlowExternal} }
func (s stubProvider) AuthStatus(context.Context, bool) (AuthStatus, error) {
	return AuthStatus{}, nil
}
func (s stubProvider) LoginStart(context.Context) (string, error)    { return "", nil }
func (s stubProvider) LoginSubmitCode(context.Context, string) error { return nil }
func (s stubProvider) Logout(context.Context) error                  { return nil }
func (s stubProvider) CaptureDeepLink(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (s stubProvider) SeedWorkspace(string, SeedOpts) error { return nil }
func (s stubProvider) SeedMeta() SeedMeta                   { return s.seedMeta }
func (s stubProvider) MasterStoreSpec() MasterStoreSpec {
	return MasterStoreSpec{EnvVar: "STUB_HOME", HostDir: "/stub", HomeSubdir: ".stub"}
}
func (s stubProvider) InjectCredentials(string) ([]string, error) { return nil, nil }
func (s stubProvider) RefreshCredentials(context.Context) error   { return nil }
func (s stubProvider) CredentialsSig(string) (string, time.Time)  { return "", time.Time{} }
func (s stubProvider) AdoptCredentials(string) error              { return nil }
func (s stubProvider) Commands(context.Context, string, string) ([]CommandSpec, error) {
	return nil, nil
}
func (s stubProvider) LocateTranscript(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (s stubProvider) ReadChat(string, string, string) (Chat, error) { return Chat{}, nil }
func (s stubProvider) Reply(context.Context, string, string) error   { return nil }
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

// --- issue #75 / ADR-0033: cross-provider scrub/seeded-path unions --------

func TestRegistry_unionScrubAndPathPatternsDedup(t *testing.T) {
	a := stubProvider{id: "a", seedMeta: SeedMeta{
		ScrubPatterns:      []string{"foo", "bar"},
		SeededPathPatterns: []string{"^a/", "^shared/"},
	}}
	b := stubProvider{id: "b", seedMeta: SeedMeta{
		ScrubPatterns:      []string{"bar", "baz"},      // "bar" duplicates a's
		SeededPathPatterns: []string{"^shared/", "^b/"}, // "^shared/" duplicates a's
	}}
	r, err := NewRegistry(a, b)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	wantScrub := []string{"foo", "bar", "baz"}
	if got := r.UnionScrubPatterns(); !reflect.DeepEqual(got, wantScrub) {
		t.Errorf("UnionScrubPatterns() = %v; want %v (registration order, declaration order, first-occurrence dedup)", got, wantScrub)
	}

	wantPaths := []string{"^a/", "^shared/", "^b/"}
	if got := r.UnionSeededPathPatterns(); !reflect.DeepEqual(got, wantPaths) {
		t.Errorf("UnionSeededPathPatterns() = %v; want %v", got, wantPaths)
	}
}

func TestRegistry_scrubRegexpsCompileCaseInsensitiveAndMatch(t *testing.T) {
	a := stubProvider{id: "a", seedMeta: SeedMeta{
		ScrubPatterns: []string{`co-authored-by:[[:space:]]*claude`},
	}}
	r, err := NewRegistry(a)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	res := r.ScrubRegexps()
	if len(res) != 1 {
		t.Fatalf("ScrubRegexps() len = %d; want 1", len(res))
	}
	if !res[0].MatchString("Co-Authored-By: Claude <x@y>") {
		t.Errorf("compiled regexp did not match a mixed-case sample; want case-insensitive match")
	}
}

func TestRegistry_rejectsProviderWithBREOnlyScrubPattern(t *testing.T) {
	// "foo(" is a valid BRE (a literal, unescaped paren) but an invalid RE2
	// (an unclosed group) — exactly the BRE∩RE2 dialect drift NewRegistry
	// must catch at boot (issue #75 / ADR-0033).
	a := stubProvider{id: "bad-provider", seedMeta: SeedMeta{
		ScrubPatterns: []string{"foo("},
	}}
	_, err := NewRegistry(a)
	if err == nil {
		t.Fatal("NewRegistry accepted an uncompilable (RE2) scrub pattern; want error")
	}
	if !strings.Contains(err.Error(), "bad-provider") {
		t.Errorf("error = %q; want it to name the offending provider id", err.Error())
	}
}

func TestRegistry_emptyRegistryUnionsAreNilSafe(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if got := r.UnionScrubPatterns(); len(got) != 0 {
		t.Errorf("UnionScrubPatterns() on empty registry = %v; want empty", got)
	}
	if got := r.UnionSeededPathPatterns(); len(got) != 0 {
		t.Errorf("UnionSeededPathPatterns() on empty registry = %v; want empty", got)
	}
	if got := r.ScrubRegexps(); len(got) != 0 {
		t.Errorf("ScrubRegexps() on empty registry = %v; want empty", got)
	}
}

func TestRegistry_unionsAndRegexpsReturnClones(t *testing.T) {
	a := stubProvider{id: "a", seedMeta: SeedMeta{
		ScrubPatterns:      []string{"foo"},
		SeededPathPatterns: []string{"^a/"},
	}}
	r, err := NewRegistry(a)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	scrub := r.UnionScrubPatterns()
	scrub[0] = "mutated"
	if got := r.UnionScrubPatterns()[0]; got != "foo" {
		t.Errorf("UnionScrubPatterns() shares backing array across calls: got %q", got)
	}

	paths := r.UnionSeededPathPatterns()
	paths[0] = "mutated"
	if got := r.UnionSeededPathPatterns()[0]; got != "^a/" {
		t.Errorf("UnionSeededPathPatterns() shares backing array across calls: got %q", got)
	}

	regexps := r.ScrubRegexps()
	regexps[0] = nil
	if got := r.ScrubRegexps()[0]; got == nil {
		t.Error("ScrubRegexps() shares backing array across calls: got nil after mutating a prior call's slice")
	}
}
