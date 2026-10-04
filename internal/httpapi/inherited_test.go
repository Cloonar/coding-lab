package httpapi

// Tests for POST /api/v1/repos/{id}/inherited (issue #61): every overridable
// repo field's inherited value, from the spawn path's own resolvers. Each step
// asserts the response twice — against literal expectations for the chain it
// exercises, and key by key against what the real spawn resolvers answer for
// the same repo (spawnTruth calls them directly), so the endpoint and the
// spawn can never drift apart.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// inheritedKeys is every RepoInherited key (web/src/api/repos.ts) — all of
// them present on every response, value or null.
var inheritedKeys = []string{
	"afk_effort_default", "afk_model_default", "afk_options", "afk_provider_default", "afk_remote_default",
	"budget_minutes", "container_memory", "container_nofile", "container_pids", "effort_default",
	"git_author_email", "git_author_name", "image_ref", "lander_effort", "lander_model", "lander_provider",
	"max_instances_override", "model_default", "provider", "remote_default", "runner",
}

// spawnTruth asks the spawn path's resolvers what each RepoInherited key is
// for repo — each on a copy with that one field nulled — rendered the way the
// response decodes (float64 numbers, nil for a resolver error).
func spawnTruth(t *testing.T, x *instTestServer, repo store.Repo) map[string]any {
	t.Helper()
	ctx := context.Background()
	inst := x.srv.instances
	with := func(f func(*store.Repo)) store.Repo {
		c := repo
		f(&c)
		return c
	}
	id := func(p provider.AgentProvider, err error) any {
		if err != nil {
			return nil
		}
		return p.ID()
	}
	out := map[string]any{}
	for _, k := range inheritedKeys {
		out[k] = nil
	}

	out["provider"] = id(inst.ResolveProvider(ctx, with(func(r *store.Repo) { r.Provider = nil }), store.RunKindManual, ""))
	out["afk_provider_default"] = id(inst.ResolveProvider(ctx, with(func(r *store.Repo) { r.AFKProviderDefault = nil }), store.RunKindAFKManual, ""))
	out["lander_provider"] = id(afk.LanderChainProvider(ctx, inst, with(func(r *store.Repo) { r.LanderProvider = nil }), store.RunKindEscalate))

	if prov, err := inst.ResolveProvider(ctx, repo, store.RunKindManual, ""); err == nil {
		if m, _, err := inst.ResolveModelEffort(ctx, prov, with(func(r *store.Repo) { r.ModelDefault = nil }), store.RunKindManual, "", ""); err == nil {
			out["model_default"] = m
		}
		if _, e, err := inst.ResolveModelEffort(ctx, prov, with(func(r *store.Repo) { r.EffortDefault = nil }), store.RunKindManual, "", ""); err == nil {
			out["effort_default"] = e
		}
		if v, err := inst.ResolveRemote(ctx, prov, with(func(r *store.Repo) { r.RemoteDefault = nil }), store.RunKindManual, nil); err == nil {
			out["remote_default"] = v
		}
	}
	// Any AFK-class kind walks the same chain; the scheduled kind is used here
	// on purpose, a different one than the endpoint's.
	if prov, err := inst.ResolveProvider(ctx, repo, store.RunKindScheduled, ""); err == nil {
		if m, _, err := inst.ResolveModelEffort(ctx, prov, with(func(r *store.Repo) { r.AFKModelDefault = nil }), store.RunKindScheduled, "", ""); err == nil {
			out["afk_model_default"] = m
		}
		if _, e, err := inst.ResolveModelEffort(ctx, prov, with(func(r *store.Repo) { r.AFKEffortDefault = nil }), store.RunKindScheduled, "", ""); err == nil {
			out["afk_effort_default"] = e
		}
		if v, err := inst.ResolveRemote(ctx, prov, with(func(r *store.Repo) { r.AFKRemoteDefault = nil }), store.RunKindScheduled, nil); err == nil {
			out["afk_remote_default"] = v
		}
		if bag, err := inst.ResolveSpawnOptions(ctx, prov, with(func(r *store.Repo) { r.AFKOptions = nil }), store.RunKindScheduled); err == nil {
			m := map[string]any{}
			for k, v := range bag {
				m[k] = v
			}
			out["afk_options"] = m
		}
	}
	if prov, err := afk.LanderChainProvider(ctx, inst, repo, store.RunKindEscalate); err == nil {
		if m, _, err := afk.LanderModelEffort(ctx, inst, prov, with(func(r *store.Repo) { r.LanderModel, r.LanderEffort = nil, nil }), store.RunKindEscalate); err == nil {
			out["lander_model"] = m
		}
		if _, e, err := afk.LanderModelEffort(ctx, inst, prov, with(func(r *store.Repo) { r.LanderEffort = nil }), store.RunKindEscalate); err == nil {
			out["lander_effort"] = e
		}
	}

	out["budget_minutes"] = float64(afk.EffectiveBudget(ctx, x.st, logx.New(io.Discard), with(func(r *store.Repo) { r.BudgetMinutes = nil })) / time.Minute)
	out["max_instances_override"] = float64(inst.EffectiveCap(ctx, with(func(r *store.Repo) { r.MaxInstancesOverride = nil })))
	if name, _, err := inst.AuthorIdentity(ctx, with(func(r *store.Repo) { r.GitAuthorName = nil })); err == nil {
		out["git_author_name"] = name
	}
	if _, email, err := inst.AuthorIdentity(ctx, with(func(r *store.Repo) { r.GitAuthorEmail = nil })); err == nil {
		out["git_author_email"] = email
	}
	if v, err := instance.EffectiveRunner(ctx, x.st, with(func(r *store.Repo) { r.Runner = nil })); err == nil {
		out["runner"] = v
	}
	if v, err := inst.DevImage(ctx, with(func(r *store.Repo) { r.ImageRef = nil })); err == nil {
		out["image_ref"] = v
	} else if errors.Is(err, instance.ErrNoDevImage) {
		out["image_ref"] = ""
	}
	if mem, pids, nofile, err := inst.EffectiveContainerLimits(ctx, with(func(r *store.Repo) {
		r.ContainerMemory, r.ContainerPids, r.ContainerNofile = nil, nil, nil
	})); err == nil {
		out["container_memory"], out["container_pids"], out["container_nofile"] = mem, float64(pids), float64(nofile)
	}
	return out
}

// inheritedCheck POSTs drafts (nil = no body at all) and asserts the response
// carries every key, equals spawnTruth for draftRepo key by key, and holds the
// literal values in want.
func inheritedCheck(t *testing.T, x *instTestServer, drafts any, draftRepo store.Repo, want map[string]any) map[string]any {
	t.Helper()
	resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/inherited", drafts, csrfHeaders(x.ts.URL))
	wantStatus(t, resp, http.StatusOK)
	got := decodeBody(t, resp)
	for _, k := range inheritedKeys {
		if !hasKey(got, k) {
			t.Errorf("response lacks key %q", k)
		}
	}
	if len(got) != len(inheritedKeys) {
		t.Errorf("response has %d keys, want exactly %d: %v", len(got), len(inheritedKeys), got)
	}
	truth := spawnTruth(t, x, draftRepo)
	for _, k := range inheritedKeys {
		if !reflect.DeepEqual(got[k], truth[k]) {
			t.Errorf("%s = %#v, but the spawn resolvers say %#v", k, got[k], truth[k])
		}
	}
	for k, v := range want {
		if !reflect.DeepEqual(got[k], v) {
			t.Errorf("%s = %#v, want %#v", k, got[k], v)
		}
	}
	return got
}

func newInheritedServer(t *testing.T) *instTestServer {
	t.Helper()
	// fake-b: other models, and NO effort knob (an empty efforts catalog).
	b := providertest.New()
	b.SetID("fake-b")
	b.SetCatalogs(
		[]provider.Option{{Value: "b-fast", Label: "B Fast"}, {Value: "b-deep", Label: "B Deep"}},
		[]provider.Option{},
	)
	// codex-fake: not remote-capable, so remote control clamps to off.
	return newInstanceServerWith(t, b, providertest.NewNoLink())
}

func (x *instTestServer) putGlobal(t *testing.T, key, value string) {
	t.Helper()
	if err := x.st.SetSetting(context.Background(), key, value); err != nil {
		t.Fatal(err)
	}
}

func (x *instTestServer) storedRepo(t *testing.T) store.Repo {
	t.Helper()
	r, err := x.st.RepoByID(context.Background(), x.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (x *instTestServer) storeRepoSettings(t *testing.T, u store.RepoSettingsUpdate) store.Repo {
	t.Helper()
	r, err := x.st.UpdateRepoSettings(context.Background(), x.repo.ID, u)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ptrOf[T any](v T) *T { return &v }

// TestRepoInherited_agentChains covers the provider / model / effort chains
// and the draft semantics (absent = saved, null or "" = inherit, a value =
// the draft): the agent inherits the global default; a draft agent moves the
// inherited models and efforts and the inherited AFK agent (with no global
// AFK default set); a global AFK default beats it; an unknown draft id skips
// like a stale stored one.
func TestRepoInherited_agentChains(t *testing.T) {
	x := newInheritedServer(t)
	saved := x.storedRepo(t)

	// The saved repo against the seeded globals: provider_default
	// claude-code, spawn_model_default opus[1m], spawn_effort_default max.
	seeded := map[string]any{
		"provider": "claude-code", "afk_provider_default": "claude-code", "lander_provider": "claude-code",
		"model_default": "opus[1m]", "effort_default": "max",
		"afk_model_default": "opus[1m]", "afk_effort_default": "max",
		"lander_model": "opus[1m]", "lander_effort": "max",
		"remote_default": false, "afk_remote_default": false,
		"afk_options":    map[string]any{},
		"budget_minutes": float64(120), "max_instances_override": float64(6),
		"git_author_name": "", "git_author_email": "",
		"runner": store.RunnerHost, "image_ref": "",
		"container_memory": store.DefaultContainerMemory,
		"container_pids":   float64(store.DefaultContainerPids), "container_nofile": float64(store.DefaultContainerNofile),
	}
	t.Run("no body resolves the saved repo", func(t *testing.T) {
		inheritedCheck(t, x, nil, saved, seeded)
	})
	t.Run("an empty object resolves the saved repo", func(t *testing.T) {
		inheritedCheck(t, x, map[string]any{}, saved, seeded)
	})

	t.Run("a draft agent moves the dependent chains", func(t *testing.T) {
		d := saved
		d.Provider = ptrOf("fake-b")
		inheritedCheck(t, x, map[string]any{"provider": "fake-b"}, d, map[string]any{
			// The agent's own inherited value ignores the draft…
			"provider": "claude-code",
			// …while every chain that reads it follows: the AFK agent (no
			// global AFK default) and the lander agent fall through to it…
			"afk_provider_default": "fake-b", "lander_provider": "fake-b",
			// …and models/efforts come from fake-b's catalogs: the seeded
			// opus[1m] skip-layers to fake-b's first model, and a provider
			// without an effort knob resolves to "" (the flag is omitted).
			"model_default": "b-fast", "effort_default": "",
			"afk_model_default": "b-fast", "afk_effort_default": "",
			"lander_model": "b-fast", "lander_effort": "",
		})
	})

	t.Run("a global AFK agent default beats the draft agent", func(t *testing.T) {
		x.putGlobal(t, store.SettingSpawnProviderDefaultAFK, "claude-code")
		t.Cleanup(func() { x.putGlobal(t, store.SettingSpawnProviderDefaultAFK, "") })
		d := saved
		d.Provider = ptrOf("fake-b")
		inheritedCheck(t, x, map[string]any{"provider": "fake-b"}, d, map[string]any{
			"afk_provider_default": "claude-code", "afk_model_default": "opus[1m]",
			"model_default": "b-fast", "lander_provider": "fake-b",
		})
	})

	t.Run("a draft model flows into the AFK and lander models", func(t *testing.T) {
		d := saved
		d.Provider, d.ModelDefault = ptrOf("fake-b"), ptrOf("b-deep")
		inheritedCheck(t, x, map[string]any{"provider": "fake-b", "model_default": "b-deep"}, d, map[string]any{
			"model_default": "b-fast", "afk_model_default": "b-deep", "lander_model": "b-deep",
		})
	})

	t.Run("an unknown draft id skips like a stale stored one", func(t *testing.T) {
		d := saved
		d.Provider = ptrOf("ghost")
		inheritedCheck(t, x, map[string]any{"provider": " ghost "}, d, map[string]any{
			"afk_provider_default": "claude-code", "lander_provider": "claude-code", "model_default": "opus[1m]",
		})
	})

	// A saved agent: absent keeps it, null and "" inherit past it.
	saved = x.storeRepoSettings(t, store.RepoSettingsUpdate{Provider: store.Set(ptrOf("fake-b"))})
	t.Run("absent keeps the saved agent", func(t *testing.T) {
		inheritedCheck(t, x, map[string]any{}, saved, map[string]any{"afk_provider_default": "fake-b", "model_default": "b-fast"})
	})
	for _, v := range []any{nil, "", "  "} {
		t.Run(fmt.Sprintf("draft %#v inherits past the saved agent", v), func(t *testing.T) {
			d := saved
			d.Provider = nil
			inheritedCheck(t, x, map[string]any{"provider": v}, d, map[string]any{"afk_provider_default": "claude-code", "model_default": "opus[1m]"})
		})
	}
	saved = x.storeRepoSettings(t, store.RepoSettingsUpdate{Provider: store.Set[*string](nil)})

	t.Run("the lander default layer", func(t *testing.T) {
		x.putGlobal(t, store.SettingSpawnModelDefaultLander, "sonnet")
		t.Cleanup(func() { x.putGlobal(t, store.SettingSpawnModelDefaultLander, "") })
		inheritedCheck(t, x, nil, saved, map[string]any{"lander_model": "sonnet", "model_default": "opus[1m]"})
	})
}

// TestRepoInherited_remoteControl: a draft repo remote_default flows into the
// inherited AFK remote control while no global AFK value is set; an explicit
// global AFK false wins over it; a provider without remote control clamps
// both to off.
func TestRepoInherited_remoteControl(t *testing.T) {
	x := newInheritedServer(t)
	saved := x.storedRepo(t)
	d := saved
	d.RemoteDefault = ptrOf(true)

	inheritedCheck(t, x, map[string]any{"remote_default": true}, d, map[string]any{
		"remote_default":     false, // its own inherited value: the global false
		"afk_remote_default": true,
	})

	x.putGlobal(t, store.SettingSpawnRemoteDefaultAFK, "false")
	inheritedCheck(t, x, map[string]any{"remote_default": true}, d, map[string]any{
		"remote_default": false, "afk_remote_default": false,
	})
	x.putGlobal(t, store.SettingSpawnRemoteDefaultAFK, "")

	x.putGlobal(t, store.SettingSpawnRemoteDefault, "true")
	inheritedCheck(t, x, map[string]any{"remote_default": false}, func() store.Repo { c := saved; c.RemoteDefault = ptrOf(false); return c }(), map[string]any{
		"remote_default":     true, // the global true; the draft false is the field's own value
		"afk_remote_default": false,
	})

	// The repo's own AFK value never answers its own inherited value.
	saved = x.storeRepoSettings(t, store.RepoSettingsUpdate{AFKRemoteDefault: store.Set(ptrOf(false))})
	d = saved
	d.RemoteDefault = ptrOf(true)
	inheritedCheck(t, x, map[string]any{"remote_default": true}, d, map[string]any{
		"remote_default": true, "afk_remote_default": true,
	})
	x.putGlobal(t, store.SettingSpawnRemoteDefault, "false")

	d2 := d
	d2.Provider = ptrOf("codex-fake")
	inheritedCheck(t, x, map[string]any{"remote_default": true, "provider": "codex-fake"}, d2, map[string]any{
		"remote_default": false, "afk_remote_default": false,
	})
}

// TestRepoInherited_globalsAndOverrides: the runner, dev image, container
// limits, AFK budget, instance cap and git author follow the global settings,
// and the repo's OWN overrides — set here on every one of them — never leak
// into their inherited values.
func TestRepoInherited_globalsAndOverrides(t *testing.T) {
	x := newInheritedServer(t)
	x.putGlobal(t, store.SettingRunnerDefault, store.RunnerContainer)
	x.putGlobal(t, store.SettingDevImageDefault, "registry.example/dev:1@sha256:abc")
	x.putGlobal(t, store.SettingContainerMemory, "4g")
	x.putGlobal(t, store.SettingContainerPids, "100")
	x.putGlobal(t, store.SettingContainerNofile, "200")
	x.putGlobal(t, store.SettingAFKBudgetMinutes, "45")
	x.putGlobal(t, store.SettingMaxInstances, "3")
	x.putGlobal(t, store.SettingGitAuthorName, "Lab Bot")
	x.putGlobal(t, store.SettingGitAuthorEmail, "bot@example.com")
	x.putGlobal(t, store.SettingSpawnOptionsAFK, `{"ultracode":"true","foreign":"x"}`)

	saved := x.storeRepoSettings(t, store.RepoSettingsUpdate{
		Runner:               store.Set(ptrOf(store.RunnerHost)),
		ImageRef:             store.Set(ptrOf("registry.example/own:2@sha256:def")),
		ContainerMemory:      store.Set(ptrOf("1g")),
		ContainerPids:        store.Set(ptrOf(7)),
		ContainerNofile:      store.Set(ptrOf(8)),
		BudgetMinutes:        store.Set(ptrOf(10)),
		MaxInstancesOverride: store.Set(ptrOf(1)),
		GitAuthorName:        store.Set(ptrOf("Own Name")),
		GitAuthorEmail:       store.Set(ptrOf("own@example.com")),
		AFKOptions:           store.Set(map[string]string{"ultracode": "false"}),
	})
	inheritedCheck(t, x, nil, saved, map[string]any{
		"runner": store.RunnerContainer, "image_ref": "registry.example/dev:1@sha256:abc",
		"container_memory": "4g", "container_pids": float64(100), "container_nofile": float64(200),
		"budget_minutes": float64(45), "max_instances_override": float64(3),
		"git_author_name": "Lab Bot", "git_author_email": "bot@example.com",
		// The global bag filtered to the provider's declared schema.
		"afk_options": map[string]any{"ultracode": "true"},
	})
}

// TestRepoInherited_resolverErrorsYieldNull: one field's resolver failing
// nulls that field (and whatever reads the same failed resolution) and never
// fails the response.
func TestRepoInherited_resolverErrorsYieldNull(t *testing.T) {
	x := newInheritedServer(t)
	x.putGlobal(t, store.SettingRunnerDefault, "bogus") // EffectiveRunner refuses a non-Runner
	x.putGlobal(t, store.SettingSpawnOptionsAFK, "{")   // ResolveSpawnOptions refuses malformed JSON
	x.putGlobal(t, store.SettingContainerPids, "lots")  // the limits read refuses a garbled int
	saved := x.storeRepoSettings(t, store.RepoSettingsUpdate{
		// A stored lander agent the registry lacks is a STRICT request: the
		// lander launch fails, so the lander model/effort cannot resolve —
		// while the lander agent's own inherited value (with it nulled) can.
		LanderProvider: store.Set(ptrOf("ghost")),
	})
	got := inheritedCheck(t, x, nil, saved, map[string]any{
		"runner": nil, "afk_options": nil,
		"container_memory": nil, "container_pids": nil, "container_nofile": nil,
		"lander_model": nil, "lander_effort": nil,
		"lander_provider": "claude-code", "provider": "claude-code", "model_default": "opus[1m]",
	})
	if got["image_ref"] != "" {
		t.Errorf("image_ref = %#v, want \"\" (nothing configured is not an error)", got["image_ref"])
	}
}

// TestRepoInherited_refusals: only draft keys are accepted (a 400 naming the
// key), a draft decodes like the PATCH (a 400 naming the key), a body that is
// not JSON is a plain 400, an unknown repo a 404 — and nothing is written.
func TestRepoInherited_refusals(t *testing.T) {
	x := newInheritedServer(t)
	h := csrfHeaders(x.ts.URL)
	before := x.storedRepo(t)
	for _, tc := range []struct {
		name  string
		body  any
		field string
	}{
		{"a field no chain reads", map[string]any{"runner": "host"}, "runner"},
		{"an unknown key", map[string]any{"nonsense": 1}, "nonsense"},
		{"a wrong-typed draft", map[string]any{"remote_default": "yes"}, "remote_default"},
		{"a non-string agent", map[string]any{"provider": 7}, "provider"},
		{"sorted: the first bad key wins", map[string]any{"zzz": 1, "afk_model_default": 3}, "afk_model_default"},
		{"not an object", "nope", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := x.do("POST", "/api/v1/repos/"+x.repo.ID+"/inherited", tc.body, h)
			wantStatus(t, resp, http.StatusBadRequest)
			got := decodeBody(t, resp)
			if got["error"] == "" {
				t.Error("400 without an error message")
			}
			if tc.field == "" {
				if hasKey(got, "field") {
					t.Errorf("field = %v, want none", got["field"])
				}
			} else if got["field"] != tc.field {
				t.Errorf("field = %v, want %q", got["field"], tc.field)
			}
		})
	}
	resp := x.do("POST", "/api/v1/repos/repo_00000000000000000000000000000000/inherited", map[string]any{}, h)
	wantStatus(t, resp, http.StatusNotFound)
	_ = decodeBody(t, resp)

	// A dry run: the drafts above never reached the row.
	resp = x.do("POST", "/api/v1/repos/"+x.repo.ID+"/inherited", map[string]any{"provider": "fake-b", "remote_default": true}, h)
	wantStatus(t, resp, http.StatusOK)
	_ = decodeBody(t, resp)
	after := x.storedRepo(t)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the inherited endpoint wrote the repo:\nbefore %+v\nafter  %+v", before, after)
	}
	if !slices.IsSorted(inheritedDraftKeys) {
		t.Errorf("inheritedDraftKeys not sorted: %v", inheritedDraftKeys)
	}
}
