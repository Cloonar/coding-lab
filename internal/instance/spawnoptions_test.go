package instance

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

func strptr(s string) *string { return &s }

func setGlobal(t *testing.T, f *fixture, key, val string) {
	t.Helper()
	if err := f.st.SetSetting(context.Background(), key, val); err != nil {
		t.Fatalf("SetSetting %s: %v", key, err)
	}
}

// ResolveModelEffort is run-kind-aware (issue #19 / ADR-0021): manual layers
// request → repo base → global base; AFK consults the AFK-override layer first
// (repo.afk_* → global spawn_*_default_afk), each empty = inherit, then the same
// base. The seeded global base is opus[1m]/max.
func TestResolveModelEffort_kindAwareLayering(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	cases := []struct {
		name                            string
		kind                            string
		repo                            store.Repo
		globalAFKModel, globalAFKEffort string
		reqModel, reqEffort             string
		wantModel, wantEffort           string
		wantErr                         bool
	}{
		{
			name: "manual falls back to the global base", kind: store.RunKindManual,
			wantModel: "opus[1m]", wantEffort: "max",
		},
		{
			name: "manual repo base wins over global base", kind: store.RunKindManual,
			repo:      store.Repo{ModelDefault: strptr("sonnet"), EffortDefault: strptr("high")},
			wantModel: "sonnet", wantEffort: "high",
		},
		{
			name: "manual per-spawn request wins over repo base", kind: store.RunKindManual,
			repo: store.Repo{ModelDefault: strptr("sonnet")}, reqModel: "fable",
			wantModel: "fable", wantEffort: "max",
		},
		{
			name: "afk with no override falls back to the base", kind: store.RunKindAFKAuto,
			wantModel: "opus[1m]", wantEffort: "max",
		},
		{
			name: "afk global override wins over the base", kind: store.RunKindAFKManual,
			globalAFKModel: "sonnet", globalAFKEffort: "low",
			wantModel: "sonnet", wantEffort: "low",
		},
		{
			name: "afk repo override wins over the global afk override", kind: store.RunKindAFKAuto,
			globalAFKModel: "sonnet", globalAFKEffort: "low",
			repo:      store.Repo{AFKModelDefault: strptr("fable"), AFKEffortDefault: strptr("high")},
			wantModel: "fable", wantEffort: "high",
		},
		{
			// Empty AFK overrides inherit — resolution degrades to the repo base.
			name: "afk empty override inherits the repo base", kind: store.RunKindAFKAuto,
			repo:      store.Repo{ModelDefault: strptr("haiku"), EffortDefault: strptr("low")},
			wantModel: "haiku", wantEffort: "low",
		},
		{
			name: "afk override outside the catalog is a bad request", kind: store.RunKindAFKAuto,
			repo: store.Repo{AFKModelDefault: strptr("gpt-9")}, wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset the AFK-override globals for each case (empty = unset).
			setGlobal(t, f, store.SettingSpawnModelDefaultAFK, tc.globalAFKModel)
			setGlobal(t, f, store.SettingSpawnEffortDefaultAFK, tc.globalAFKEffort)

			model, effort, err := f.svc.ResolveModelEffort(ctx, f.prov, tc.repo, tc.kind, tc.reqModel, tc.reqEffort)
			if tc.wantErr {
				var bad *BadRequestError
				if !errors.As(err, &bad) {
					t.Fatalf("err = %v, want *BadRequestError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveModelEffort: %v", err)
			}
			if model != tc.wantModel || effort != tc.wantEffort {
				t.Errorf("= %q/%q, want %q/%q", model, effort, tc.wantModel, tc.wantEffort)
			}
		})
	}
}

// ResolveSpawnOptions: manual carries no options; AFK resolves the repo bag
// (falling back to the global bag), filtered + validated to the provider's
// declared schema (issue #19).
func TestResolveSpawnOptions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Manual → an empty (non-nil) bag, regardless of what's configured.
	setGlobal(t, f, store.SettingSpawnOptionsAFK, `{"ultracode":"true"}`)
	if got, err := f.svc.ResolveSpawnOptions(ctx, f.prov, store.Repo{}, store.RunKindManual); err != nil || len(got) != 0 || got == nil {
		t.Fatalf("manual options = %v (%v), want empty non-nil", got, err)
	}

	// AFK, nothing configured → empty bag.
	setGlobal(t, f, store.SettingSpawnOptionsAFK, "")
	if got, err := f.svc.ResolveSpawnOptions(ctx, f.prov, store.Repo{}, store.RunKindAFKAuto); err != nil || len(got) != 0 {
		t.Fatalf("afk empty options = %v (%v), want empty", got, err)
	}

	// AFK, global bag → resolved.
	setGlobal(t, f, store.SettingSpawnOptionsAFK, `{"ultracode":"true"}`)
	if got, err := f.svc.ResolveSpawnOptions(ctx, f.prov, store.Repo{}, store.RunKindAFKAuto); err != nil || got["ultracode"] != "true" {
		t.Fatalf("afk global options = %v (%v), want ultracode=true", got, err)
	}

	// AFK, a present repo bag (even conflicting) wins over the global bag.
	repo := store.Repo{AFKOptions: map[string]string{"ultracode": "false"}}
	if got, err := f.svc.ResolveSpawnOptions(ctx, f.prov, repo, store.RunKindAFKAuto); err != nil || got["ultracode"] != "false" {
		t.Fatalf("afk repo-override options = %v (%v), want ultracode=false", got, err)
	}

	// AFK, a present-but-empty repo bag means "explicitly no options" — it still
	// wins over a non-empty global bag.
	repo = store.Repo{AFKOptions: map[string]string{}}
	if got, err := f.svc.ResolveSpawnOptions(ctx, f.prov, repo, store.RunKindAFKAuto); err != nil || len(got) != 0 {
		t.Fatalf("afk empty repo bag = %v (%v), want empty (overrides global)", got, err)
	}

	// AFK, a global key the provider does not declare is FILTERED OUT (a global
	// bag may span providers), not an error.
	setGlobal(t, f, store.SettingSpawnOptionsAFK, `{"warp_drive":"true"}`)
	if got, err := f.svc.ResolveSpawnOptions(ctx, f.prov, store.Repo{}, store.RunKindAFKAuto); err != nil || len(got) != 0 {
		t.Fatalf("afk undeclared key = %v (%v), want filtered to empty", got, err)
	}

	// AFK, a declared key with a bad value survives the filter and fails
	// validation → a bad request.
	setGlobal(t, f, store.SettingSpawnOptionsAFK, `{"ultracode":"maybe"}`)
	_, err := f.svc.ResolveSpawnOptions(ctx, f.prov, store.Repo{}, store.RunKindAFKAuto)
	var bad *BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("bad option value err = %v, want *BadRequestError", err)
	}
}

// Launch threads the resolved options bag through LaunchSpec.Options into the
// provider's SpawnSpec (issue #19), and the dialog-capture --settings flag still
// lands among the flags, BEFORE the trailing seed prompt.
func TestLaunch_threadsOptionsAndKeepsSettingsBeforePrompt(t *testing.T) {
	f := newFixture(t)
	const seed = "resolve #7 and open a PR"
	n := 7
	name := "proj~afk-7"
	run, err := f.svc.Launch(t.Context(), LaunchSpec{
		Repo:         f.repo,
		Provider:     f.prov,
		Kind:         store.RunKindAFKManual,
		IssueNumber:  &n,
		SessionName:  name,
		Branch:       "afk/7",
		WorktreePath: filepath.Join(f.worktreeRoot, "proj-7"),
		Model:        "opus[1m]",
		Effort:       "max",
		Options:      map[string]string{"ultracode": "true"},
		SeedPrompt:   seed,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// The resolved bag reached the provider's SpawnSpec verbatim.
	specs := f.prov.SpawnSpecs()
	if len(specs) != 1 {
		t.Fatalf("SpawnArgv called %d times, want 1", len(specs))
	}
	if specs[0].Options["ultracode"] != "true" || specs[0].InitialPrompt != seed {
		t.Errorf("threaded spec = %+v, want Options[ultracode]=true + InitialPrompt=%q", specs[0], seed)
	}

	// The --settings flag is among the flags, immediately before the trailing
	// seed positional (the fake provider does not apply ultracode, so the last
	// argv element is the raw seed).
	sess, live := f.runner.Session(name)
	if !live {
		t.Fatal("session not live after Launch")
	}
	argv := sess.Argv
	if last := argv[len(argv)-1]; last != seed {
		t.Errorf("last argv = %q, want the seed prompt %q as the trailing positional", last, seed)
	}
	joined := strings.Join(argv, " ")
	settingsPath := filepath.Join(f.runtime, "settings."+run.ID+".json")
	if !strings.Contains(joined, "--settings "+settingsPath+" "+seed) {
		t.Errorf("argv = %q, want --settings before the trailing prompt", joined)
	}
}
