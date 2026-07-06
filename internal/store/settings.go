package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Canonical settings keys (design §3a). Runtime-mutable knobs live here;
// flags only seed them.
const (
	SettingSpawnModelDefault    = "spawn_model_default"
	SettingSpawnEffortDefault   = "spawn_effort_default"
	SettingMaxInstances         = "max_instances"
	SettingAFKBudgetMinutes     = "afk_budget_minutes"
	SettingAFKTickSeconds       = "afk_tick_seconds"
	SettingAFKScheduleSeconds   = "afk_schedule_seconds"
	SettingSweepIntervalMinutes = "sweep_interval_minutes"
	SettingGitAuthorName        = "git_author_name"
	SettingGitAuthorEmail       = "git_author_email"
)

// GetSetting returns the raw value for key, or ErrNotFound.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT value FROM settings WHERE key = ?`), key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("setting %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("setting %q: %w", key, err)
	}
	return v, nil
}

// SetSetting upserts key to value.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`), key, value)
	if err != nil {
		return fmt.Errorf("set setting %q: %w", key, err)
	}
	return nil
}

// AllSettings returns every settings row as a map.
func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("all settings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	all := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("all settings: %w", err)
		}
		all[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("all settings: %w", err)
	}
	return all, nil
}

// GetString returns the value for key, falling back to def when the key is
// missing or its value is blank (v0 rule: blank fields fall back to defaults
// field-by-field; a present non-blank value is returned untouched).
func (s *Store) GetString(ctx context.Context, key, def string) (string, error) {
	v, err := s.GetSetting(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return def, nil
	}
	if err != nil {
		return "", err
	}
	if v == "" {
		return def, nil
	}
	return v, nil
}

// GetInt returns the value for key as an int, falling back to def when the
// key is missing or blank. A present, non-blank, non-integer value is an
// error (fail loud, never silently rewrite).
func (s *Store) GetInt(ctx context.Context, key string, def int) (int, error) {
	v, err := s.GetString(ctx, key, "")
	if err != nil {
		return 0, err
	}
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("setting %q: not an integer: %q", key, v)
	}
	return n, nil
}

// SeedDefaultSettings inserts the design §3a defaults for every missing key
// and never overwrites an existing row (--max-instances seeds the row on
// first start; thereafter settings wins).
func (s *Store) SeedDefaultSettings(ctx context.Context, maxInstances int) error {
	defaults := map[string]string{
		SettingSpawnModelDefault:    "opus[1m]",
		SettingSpawnEffortDefault:   "max",
		SettingMaxInstances:         strconv.Itoa(maxInstances),
		SettingAFKBudgetMinutes:     "120",
		SettingAFKTickSeconds:       "30",
		SettingAFKScheduleSeconds:   "45",
		SettingSweepIntervalMinutes: "10",
		SettingGitAuthorName:        "",
		SettingGitAuthorEmail:       "",
	}
	for key, value := range defaults {
		_, err := s.db.ExecContext(ctx, s.rebind(
			`INSERT INTO settings (key, value) VALUES (?, ?)
			 ON CONFLICT (key) DO NOTHING`), key, value)
		if err != nil {
			return fmt.Errorf("seed setting %q: %w", key, err)
		}
	}
	return nil
}
