package instance

import (
	"context"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

func boolptr(b bool) *bool { return &b }

// ResolveRemote layers the remote-control knob (issue #163) on the same
// skip-layer model as model/effort — manual: request → repo.remote_default →
// spawn_remote_default → false; AFK: repo.afk_remote_default →
// spawn_remote_default_afk → the same base chain — but on a BOOLEAN, where
// `false` is a legal value and therefore can never spell "unset". The cases
// below pin exactly that: a layer that is present-and-FALSE beats a lower layer
// that is TRUE (the repo-off-over-global-on trap), while an ABSENT layer (nil
// column, blank settings row) inherits. The fixture's seeded base is
// spawn_remote_default=false; the fake provider advertises RemoteCapable.
func TestResolveRemote_kindAwareLayering(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	cases := []struct {
		name      string
		kind      string
		repo      store.Repo
		globalAFK string // "" = the row is blank → inherit (never "false"!)
		globalNRC string // spawn_remote_default; "" = blank row → the false floor
		req       *bool
		want      bool
	}{
		{
			name: "manual with no layers set is off", kind: store.RunKindManual,
			want: false,
		},
		{
			name: "manual inherits the global base", kind: store.RunKindManual,
			globalNRC: "true", want: true,
		},
		{
			// THE BOOL TRAP: repo says "explicitly off", the global says on. A
			// present-and-false layer is a VALUE, not a hole — it must win.
			name: "manual repo false beats a true global", kind: store.RunKindManual,
			repo: store.Repo{RemoteDefault: boolptr(false)}, globalNRC: "true",
			want: false,
		},
		{
			name: "manual repo true beats a false global", kind: store.RunKindManual,
			repo: store.Repo{RemoteDefault: boolptr(true)}, globalNRC: "false",
			want: true,
		},
		{
			// An explicit per-spawn pick beats everything — including an explicit
			// OFF over a repo+global that both say on.
			name: "explicit request true beats every layer", kind: store.RunKindManual,
			repo: store.Repo{RemoteDefault: boolptr(false)}, globalNRC: "false",
			req: boolptr(true), want: true,
		},
		{
			name: "explicit request false beats every layer", kind: store.RunKindManual,
			repo: store.Repo{RemoteDefault: boolptr(true)}, globalNRC: "true",
			req: boolptr(false), want: false,
		},
		{
			name: "afk with no layers set is off", kind: store.RunKindAFKAuto,
			want: false,
		},
		{
			// The AFK override layer resolves FIRST, so a blank _afk row must fall
			// through to the base chain, not answer false.
			name: "afk inherits the repo base when its override is absent", kind: store.RunKindAFKManual,
			repo: store.Repo{RemoteDefault: boolptr(true)},
			want: true,
		},
		{
			name: "afk inherits the global base when every override is absent", kind: store.RunKindAFKAuto,
			globalNRC: "true", want: true,
		},
		{
			name: "afk global override wins over the base", kind: store.RunKindAFKAuto,
			globalAFK: "true", globalNRC: "false", want: true,
		},
		{
			// The bool trap again, one layer up: an AFK override of false must beat
			// a repo base of true (unattended runs opting OUT of remote control).
			name: "afk global override false beats a true repo base", kind: store.RunKindAFKManual,
			globalAFK: "false", repo: store.Repo{RemoteDefault: boolptr(true)},
			want: false,
		},
		{
			name: "afk repo override false beats a true global afk override", kind: store.RunKindAFKAuto,
			repo:      store.Repo{AFKRemoteDefault: boolptr(false), RemoteDefault: boolptr(true)},
			globalAFK: "true", globalNRC: "true",
			want: false,
		},
		{
			name: "afk repo override true beats a false global afk override", kind: store.RunKindAFKAuto,
			repo:      store.Repo{AFKRemoteDefault: boolptr(true), RemoteDefault: boolptr(false)},
			globalAFK: "false", globalNRC: "false",
			want: true,
		},
		{
			// A manual run never consults the AFK layer, however loudly it shouts.
			name: "manual ignores the afk override layer", kind: store.RunKindManual,
			repo: store.Repo{AFKRemoteDefault: boolptr(true)}, globalAFK: "true",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// "" writes a BLANK row — the settings layer's only spelling of
			// "unset" (a "false" row would be a value, and would wrongly win).
			setGlobal(t, f, store.SettingSpawnRemoteDefaultAFK, tc.globalAFK)
			setGlobal(t, f, store.SettingSpawnRemoteDefault, tc.globalNRC)

			got, err := f.svc.ResolveRemote(ctx, f.prov, tc.repo, tc.kind, tc.req)
			if err != nil {
				t.Fatalf("ResolveRemote: %v", err)
			}
			if got != tc.want {
				t.Errorf("ResolveRemote = %v, want %v", got, tc.want)
			}
		})
	}
}

// The capability clamp (issue #163): a provider that does not implement
// provider.RemoteCapable resolves to false with EVERY layer — request, repo,
// AFK override, global — screaming true. The run row must truthfully record
// remote-ness (a codex run is never remote): the deep-link gate and the SPA's
// Open affordance both read that stored bool.
func TestResolveRemote_capabilityClamp(t *testing.T) {
	codexModel, codexEffort := "gpt-5-codex", "medium"
	nolink := providertest.NewNoLink() // implements neither DeepLinker nor RemoteCapable
	f := newFixtureWith(t, fixtureOpts{
		prov: nolink, providerID: nolink.ID(),
		modelDef: &codexModel, effortDef: &codexEffort,
	})
	ctx := context.Background()
	setGlobal(t, f, store.SettingSpawnRemoteDefault, "true")
	setGlobal(t, f, store.SettingSpawnRemoteDefaultAFK, "true")
	repo := store.Repo{RemoteDefault: boolptr(true), AFKRemoteDefault: boolptr(true)}

	for _, kind := range []string{store.RunKindManual, store.RunKindAFKManual, store.RunKindAFKAuto} {
		got, err := f.svc.ResolveRemote(ctx, nolink, repo, kind, boolptr(true))
		if err != nil {
			t.Fatalf("ResolveRemote(%s): %v", kind, err)
		}
		if got {
			t.Errorf("ResolveRemote(%s) = true for a provider without RemoteCapable, want false (clamped)", kind)
		}
	}

	// The clamp lands on the RUN ROW too — a start on this provider records
	// remote=false however the layers are configured.
	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Remote: boolptr(true)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	got, err := f.st.RunBySession(t.Context(), run.SessionName)
	if err != nil {
		t.Fatalf("RunBySession: %v", err)
	}
	if got.Remote {
		t.Error("run row records remote=true on a provider that cannot honor it")
	}
}

// A remote start spends the resolved bool twice: into the provider's SpawnSpec
// (the argv changes) and onto the run row — and THAT is what arms deep-link
// capture (ADR-0017), so the link lands.
func TestStart_remote_spawnsRemoteAndCapturesDeepLink(t *testing.T) {
	f := newFixture(t)
	f.prov.SetDeepLink("https://claude.ai/code/session_real")

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Remote: boolptr(true)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The resolved value reached the provider's SpawnSpec…
	specs := f.prov.SpawnSpecs()
	if len(specs) != 1 {
		t.Fatalf("SpawnArgv called %d times, want 1", len(specs))
	}
	if !specs[0].Remote {
		t.Error("SpawnSpec.Remote = false, want true (the resolved knob must reach the provider)")
	}
	// …and through it into the spawn command the runner recorded.
	sess, live := f.runner.Session(run.SessionName)
	if !live {
		t.Fatal("session not live after Start")
	}
	if !strings.Contains(strings.Join(sess.Argv, " "), "--remote-control "+run.SessionName) {
		t.Errorf("spawn argv = %v, want the provider's remote-control flag", sess.Argv)
	}
	// …and onto the run row, the only thing that still knows after a restart.
	got, err := f.st.RunBySession(t.Context(), run.SessionName)
	if err != nil {
		t.Fatalf("RunBySession: %v", err)
	}
	if !got.Remote {
		t.Error("run row records remote=false for a remote start")
	}
	// The gate is open, so capture arms and persists the real link.
	waitFor(t, "deep link captured", func() bool {
		r, err := f.st.RunBySession(t.Context(), run.SessionName)
		return err == nil && r.DeepLinkURL != nil && *r.DeepLinkURL == "https://claude.ai/code/session_real"
	})
}

// ArmCapture is the ONE choke point where remote-ness gates deep-link capture
// (issue #163) — it covers Start AND reconcile's boot re-adoption, which is why
// the gate reads the persisted run row and not a launch-time variable. A
// non-remote run arms nothing (arming would fire ADR-0017's loud capture-miss on
// every run); a remote one arms and lands the link.
func TestArmCapture_gatedOnRunRemote(t *testing.T) {
	f := newFixture(t)
	f.prov.SetDeepLink("https://claude.ai/code/session_real")

	// A non-remote run — the shape reconcile re-adopts after a restart.
	local, err := f.st.CreateRun(t.Context(), store.Run{
		ID: ids.NewID("run"), RepoID: f.repo.ID, Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/local", WorktreePath: "/wt/proj-local", SessionName: "proj~local",
		Model: "opus[1m]", Effort: "max", Remote: false,
		StartedAt: clockTime, Outcome: store.RunOutcomeActive,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f.svc.ArmCapture(local)
	time.Sleep(50 * time.Millisecond) // a wrongly-armed goroutine would have written by now
	if f.prov.CaptureCount() != 0 {
		t.Errorf("CaptureDeepLink ran %d times for a non-remote run, want 0", f.prov.CaptureCount())
	}
	if got, _ := f.st.RunBySession(t.Context(), local.SessionName); got.DeepLinkURL != nil {
		t.Errorf("deep_link_url = %v for a non-remote run, want NULL", *got.DeepLinkURL)
	}

	// The same re-adoption on a REMOTE run does arm — the run row is the only
	// thing that still knows, and it says yes.
	remote, err := f.st.CreateRun(t.Context(), store.Run{
		ID: ids.NewID("run"), RepoID: f.repo.ID, Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/remote", WorktreePath: "/wt/proj-remote", SessionName: "proj~remote",
		Model: "opus[1m]", Effort: "max", Remote: true,
		StartedAt: clockTime, Outcome: store.RunOutcomeActive,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	f.svc.ArmCapture(remote)
	waitFor(t, "deep link captured for the remote run", func() bool {
		r, err := f.st.RunBySession(t.Context(), remote.SessionName)
		return err == nil && r.DeepLinkURL != nil && *r.DeepLinkURL == "https://claude.ai/code/session_real"
	})
}
