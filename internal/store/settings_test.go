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

func TestSeedDefaultSettings(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		if err := s.SeedDefaultSettings(ctx, 6); err != nil {
			t.Fatalf("seed: %v", err)
		}
		all, err := s.AllSettings(ctx)
		if err != nil {
			t.Fatalf("all: %v", err)
		}
		want := map[string]string{
			SettingSpawnModelDefault:    "opus[1m]",
			SettingSpawnEffortDefault:   "max",
			SettingMaxInstances:         "6",
			SettingAFKBudgetMinutes:     "120",
			SettingAFKTickSeconds:       "30",
			SettingAFKScheduleSeconds:   "45",
			SettingSweepIntervalMinutes: "10",
			SettingGitAuthorName:        "",
			SettingGitAuthorEmail:       "",
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
	})
}

// TestSeedDefaultSettingsIdempotent pins the settings-win rule: --max-instances
// seeds the row on first start; thereafter the settings value wins, and
// re-seeding only fills holes.
func TestSeedDefaultSettingsIdempotent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()

		if err := s.SeedDefaultSettings(ctx, 6); err != nil {
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

		// Restart with a different --max-instances: existing rows win, the
		// hole is refilled.
		if err := s.SeedDefaultSettings(ctx, 10); err != nil {
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
	})
}
