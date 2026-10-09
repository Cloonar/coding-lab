package store

import (
	"context"
	"errors"
	"testing"
)

func TestSettingsRawAccessors(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		if _, err := s.GetSetting(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Errorf("get missing = %v, want ErrNotFound", err)
		}

		if err := s.SetSetting(ctx, "max_instances", "6"); err != nil {
			t.Fatalf("set: %v", err)
		}
		if v, err := s.GetSetting(ctx, "max_instances"); err != nil || v != "6" {
			t.Errorf("get = %q, %v; want 6, nil", v, err)
		}

		// Upsert overwrites.
		if err := s.SetSetting(ctx, "max_instances", "3"); err != nil {
			t.Fatalf("set again: %v", err)
		}
		if v, _ := s.GetSetting(ctx, "max_instances"); v != "3" {
			t.Errorf("get after upsert = %q, want 3", v)
		}

		if err := s.SetSetting(ctx, "git_author_name", "Lab Bot"); err != nil {
			t.Fatalf("set second key: %v", err)
		}
		all, err := s.AllSettings(ctx)
		if err != nil {
			t.Fatalf("all: %v", err)
		}
		if len(all) != 2 || all["max_instances"] != "3" || all["git_author_name"] != "Lab Bot" {
			t.Errorf("all = %v, want the two written keys", all)
		}
	})
}

func TestSettingsTypedHelpers(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		// Missing key -> default.
		if v, err := s.GetInt(ctx, "afk_budget_minutes", 120); err != nil || v != 120 {
			t.Errorf("GetInt missing = %d, %v; want 120, nil", v, err)
		}
		if v, err := s.GetString(ctx, "spawn_model_default", "opus[1m]"); err != nil || v != "opus[1m]" {
			t.Errorf("GetString missing = %q, %v; want opus[1m], nil", v, err)
		}

		// Blank value -> default, field by field (v0 spawn-config rule).
		if err := s.SetSetting(ctx, "spawn_model_default", ""); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetString(ctx, "spawn_model_default", "opus[1m]"); err != nil || v != "opus[1m]" {
			t.Errorf("GetString blank = %q, %v; want default opus[1m], nil", v, err)
		}
		if err := s.SetSetting(ctx, "afk_tick_seconds", ""); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetInt(ctx, "afk_tick_seconds", 30); err != nil || v != 30 {
			t.Errorf("GetInt blank = %d, %v; want default 30, nil", v, err)
		}

		// Present value wins over the default.
		if err := s.SetSetting(ctx, "max_instances", "4"); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetInt(ctx, "max_instances", 6); err != nil || v != 4 {
			t.Errorf("GetInt present = %d, %v; want 4, nil", v, err)
		}
		if err := s.SetSetting(ctx, "spawn_effort_default", "high"); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetString(ctx, "spawn_effort_default", "max"); err != nil || v != "high" {
			t.Errorf("GetString present = %q, %v; want high, nil", v, err)
		}

		// Malformed non-blank int fails loud, never silently rewrites.
		if err := s.SetSetting(ctx, "max_instances", "lots"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetInt(ctx, "max_instances", 6); err == nil {
			t.Error("GetInt on malformed value succeeded, want error")
		}
	})
}

// TestSettingsGetBool pins GetBool's discipline (issue #163). It is GetInt's
// rules with the boolean trap spelled out: blank/missing → the default, but a
// stored "false" is a VALUE, not an absence, and garbage is a loud error — for
// a boolean, silently coercing a typo to false would be indistinguishable from
// an operator's deliberate off.
func TestSettingsGetBool(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		// Missing key -> default, either way.
		if v, err := s.GetBool(ctx, SettingSpawnRemoteDefault, true); err != nil || !v {
			t.Errorf("GetBool missing = %v, %v; want true, nil", v, err)
		}
		if v, err := s.GetBool(ctx, SettingSpawnRemoteDefault, false); err != nil || v {
			t.Errorf("GetBool missing = %v, %v; want false, nil", v, err)
		}

		// Blank value -> default (blank, not "false", is the unset spelling).
		if err := s.SetSetting(ctx, SettingSpawnRemoteDefault, ""); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetBool(ctx, SettingSpawnRemoteDefault, true); err != nil || !v {
			t.Errorf("GetBool blank = %v, %v; want default true, nil", v, err)
		}

		// Present values win over the default — false included.
		if err := s.SetSetting(ctx, SettingSpawnRemoteDefault, "false"); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetBool(ctx, SettingSpawnRemoteDefault, true); err != nil || v {
			t.Errorf("GetBool false = %v, %v; want stored false, nil", v, err)
		}
		if err := s.SetSetting(ctx, SettingSpawnRemoteDefault, "true"); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetBool(ctx, SettingSpawnRemoteDefault, false); err != nil || !v {
			t.Errorf("GetBool true = %v, %v; want stored true, nil", v, err)
		}

		// Malformed non-blank value fails loud, never silently coerces.
		for _, bad := range []string{"yes", "1", "TRUE", "off"} {
			if err := s.SetSetting(ctx, SettingSpawnRemoteDefault, bad); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetBool(ctx, SettingSpawnRemoteDefault, false); err == nil {
				t.Errorf("GetBool on %q succeeded, want error", bad)
			}
		}
	})
}

func TestSeedDefaultSettings(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		if err := s.SeedDefaultSettings(ctx, 6, "claude-code"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		all, err := s.AllSettings(ctx)
		if err != nil {
			t.Fatalf("all: %v", err)
		}
		want := map[string]string{
			SettingSpawnModelDefault:    "opus[1m]",
			SettingSpawnEffortDefault:   "max",
			SettingSpawnRemoteDefault:   "false",
			SettingProviderDefault:      "claude-code",
			SettingMaxInstances:         "6",
			SettingAFKBudgetMinutes:     "120",
			SettingAFKTickSeconds:       "30",
			SettingAFKScheduleSeconds:   "45",
			SettingSweepIntervalMinutes: "10",
			SettingGitAuthorName:        "",
			SettingGitAuthorEmail:       "",
			SettingContainerMemory:      "8g",
			SettingContainerPids:        "4096",
			SettingContainerNofile:      "16384",
			SettingRunnerDefault:        "host",

			SettingTranscriptRetentionDays: "30",
		}
		if len(all) != len(want) {
			t.Errorf("seeded %d keys, want %d: %v", len(all), len(want), all)
		}
		for k, v := range want {
			got, ok := all[k]
			if !ok {
				t.Errorf("seed missing key %s", k)
				continue
			}
			if got != v {
				t.Errorf("seeded %s = %q, want %q", k, got, v)
			}
		}

		// The AFK overrides are deliberately unseeded — absent = inherit the base
		// (issue #163 for the remote pair, #19/#66 for the rest). A seeded
		// spawn_remote_default_afk row would pin every AFK run to the base value
		// and make "inherit" unspellable.
		for _, k := range []string{
			SettingSpawnRemoteDefaultAFK,
			SettingSpawnModelDefaultAFK,
			SettingSpawnEffortDefaultAFK,
			SettingSpawnModelDefaultLander,
			SettingSpawnEffortDefaultLander,
			SettingSpawnProviderDefaultAFK,
			SettingSpawnOptionsAFK,
			SettingAFKPrompt,
			SettingDialogTimeoutMinutes,
		} {
			if v, ok := all[k]; ok {
				t.Errorf("seeded %s = %q, want the key absent (inherit)", k, v)
			}
		}

		// The seeded base reads back through GetBool as a real false, and the
		// unseeded AFK key falls back to whatever default the resolver passes.
		if v, err := s.GetBool(ctx, SettingSpawnRemoteDefault, true); err != nil || v {
			t.Errorf("GetBool(spawn_remote_default) = %v, %v; want seeded false, nil", v, err)
		}
		if v, err := s.GetBool(ctx, SettingSpawnRemoteDefaultAFK, true); err != nil || !v {
			t.Errorf("GetBool(spawn_remote_default_afk) = %v, %v; want the caller's default true, nil", v, err)
		}
	})
}

// TestSeedDefaultSettingsIdempotent pins the settings-win rule: --max-instances
// seeds the row on first start; thereafter the settings value wins, and
// re-seeding only fills holes.
func TestSeedDefaultSettingsIdempotent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		if err := s.SeedDefaultSettings(ctx, 6, "claude-code"); err != nil {
			t.Fatalf("first seed: %v", err)
		}
		// Operator changes two knobs at runtime.
		if err := s.SetSetting(ctx, SettingMaxInstances, "3"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetSetting(ctx, SettingSpawnEffortDefault, "high"); err != nil {
			t.Fatal(err)
		}
		// A knob row disappears (e.g. hand-edited DB).
		if _, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM settings WHERE key = ?`), SettingAFKBudgetMinutes); err != nil {
			t.Fatal(err)
		}

		// Restart with a different --max-instances (and a different first
		// provider): existing rows win, the hole is refilled.
		if err := s.SeedDefaultSettings(ctx, 10, "other-provider"); err != nil {
			t.Fatalf("second seed: %v", err)
		}

		if v, _ := s.GetInt(ctx, SettingMaxInstances, 0); v != 3 {
			t.Errorf("max_instances after reseed = %d, want operator value 3", v)
		}
		if v, _ := s.GetString(ctx, SettingSpawnEffortDefault, ""); v != "high" {
			t.Errorf("spawn_effort_default after reseed = %q, want operator value high", v)
		}
		if v, _ := s.GetInt(ctx, SettingAFKBudgetMinutes, 0); v != 120 {
			t.Errorf("afk_budget_minutes refilled = %d, want 120", v)
		}
		if v, _ := s.GetString(ctx, SettingProviderDefault, ""); v != "claude-code" {
			t.Errorf("provider_default after reseed = %q, want the first seed's claude-code", v)
		}
	})
}

// TestSeedDefaultSettings_runnerDefault pins the global runner default's seed
// contract (issue #55): a fresh database ends up with runner_default = host
// without operator action — so an upgrade spawns exactly as before — and an
// operator's existing value is never overwritten by a later seed (every boot
// re-runs SeedDefaultSettings, so an overwrite would silently flip every
// inheriting repo back to host on the next restart).
func TestSeedDefaultSettings_runnerDefault(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		if err := s.SeedDefaultSettings(ctx, 6, "claude-code"); err != nil {
			t.Fatalf("first seed: %v", err)
		}
		if v, err := s.GetSetting(ctx, SettingRunnerDefault); err != nil || v != RunnerHost {
			t.Fatalf("fresh runner_default = %q, %v; want the seeded %q", v, err, RunnerHost)
		}

		if err := s.SetSetting(ctx, SettingRunnerDefault, RunnerContainer); err != nil {
			t.Fatalf("set runner_default: %v", err)
		}
		if err := s.SeedDefaultSettings(ctx, 6, "claude-code"); err != nil {
			t.Fatalf("re-seed: %v", err)
		}
		if v, err := s.GetSetting(ctx, SettingRunnerDefault); err != nil || v != RunnerContainer {
			t.Errorf("runner_default after re-seed = %q, %v; want the operator's %q to survive", v, err, RunnerContainer)
		}
	})
}

// TestSeedDefaultSettings_transcriptRetention pins issue #81's seed contract:
// a fresh database retains for DefaultTranscriptRetentionDays without
// operator action, and an operator's value — including the 0 off switch —
// survives every later boot's re-seed.
func TestSeedDefaultSettings_transcriptRetention(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		if err := s.SeedDefaultSettings(ctx, 6, "claude-code"); err != nil {
			t.Fatalf("first seed: %v", err)
		}
		if n, err := s.TranscriptRetentionDays(ctx); err != nil || n != DefaultTranscriptRetentionDays {
			t.Fatalf("fresh retention = %d, %v; want the seeded %d", n, err, DefaultTranscriptRetentionDays)
		}

		if err := s.SetSetting(ctx, SettingTranscriptRetentionDays, "0"); err != nil {
			t.Fatalf("set retention: %v", err)
		}
		if err := s.SeedDefaultSettings(ctx, 6, "claude-code"); err != nil {
			t.Fatalf("re-seed: %v", err)
		}
		if n, err := s.TranscriptRetentionDays(ctx); err != nil || n != 0 {
			t.Errorf("retention after re-seed = %d, %v; want the operator's 0 to survive", n, err)
		}
	})
}

// TestTranscriptRetentionDays pins the typed reader (issue #81): absent and
// blank read as the default with no error; 0 and the cap are real values; a
// garbled or out-of-range row returns the default WITH an ErrInvalidSetting
// error, so a caller can warn and still act on a sane window.
func TestTranscriptRetentionDays(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		if n, err := s.TranscriptRetentionDays(ctx); err != nil || n != DefaultTranscriptRetentionDays {
			t.Errorf("absent = %d, %v; want %d, nil", n, err, DefaultTranscriptRetentionDays)
		}

		for _, tc := range []struct {
			raw  string
			want int
		}{
			{"", DefaultTranscriptRetentionDays},
			{"   ", DefaultTranscriptRetentionDays},
			{"0", 0},
			{"7", 7},
			{" 14 ", 14},
			{"365", MaxTranscriptRetentionDays},
		} {
			if err := s.SetSetting(ctx, SettingTranscriptRetentionDays, tc.raw); err != nil {
				t.Fatal(err)
			}
			if n, err := s.TranscriptRetentionDays(ctx); err != nil || n != tc.want {
				t.Errorf("stored %q = %d, %v; want %d, nil", tc.raw, n, err, tc.want)
			}
		}

		for _, bad := range []string{"-1", "366", "lots", "1.5"} {
			if err := s.SetSetting(ctx, SettingTranscriptRetentionDays, bad); err != nil {
				t.Fatal(err)
			}
			n, err := s.TranscriptRetentionDays(ctx)
			if !errors.Is(err, ErrInvalidSetting) {
				t.Errorf("stored %q err = %v, want ErrInvalidSetting", bad, err)
			}
			if n != DefaultTranscriptRetentionDays {
				t.Errorf("stored %q = %d, want the default %d alongside the error", bad, n, DefaultTranscriptRetentionDays)
			}
		}
	})
}
