package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Canonical settings keys (design §3a). Runtime-mutable knobs live here;
// flags only seed them.
const (
	SettingSpawnModelDefault    = "spawn_model_default"
	SettingSpawnEffortDefault   = "spawn_effort_default"
	SettingSpawnRemoteDefault   = "spawn_remote_default"
	SettingProviderDefault      = "provider_default"
	SettingMaxInstances         = "max_instances"
	SettingAFKBudgetMinutes     = "afk_budget_minutes"
	SettingAFKTickSeconds       = "afk_tick_seconds"
	SettingAFKScheduleSeconds   = "afk_schedule_seconds"
	SettingSweepIntervalMinutes = "sweep_interval_minutes"
	SettingGitAuthorName        = "git_author_name"
	SettingGitAuthorEmail       = "git_author_email"

	// Dialog auto-dismiss window (issue #124): how long a MANUAL session's
	// dialog waits before auto-dismissing, in minutes. 0 or absent = never
	// auto-dismiss — the adapter defeats upstream claude-code's 60s picker
	// self-resolve (compat §5); 1 restores upstream parity. AFK runs ignore
	// it: unattended auto-advance is a feature there. Deliberately NOT seeded
	// by SeedDefaultSettings — like the AFK-override keys below, absent is a
	// meaningful state (never), not a hole to fill.
	SettingDialogTimeoutMinutes = "dialog_timeout_minutes"

	// AFK-override spawn defaults (issue #19 / ADR-0021): the AFK-only layer
	// that resolves BEFORE the base spawn_model_default/spawn_effort_default
	// above. Empty (or absent) means inherit the base. spawn_options_afk holds
	// the provider-owned options bag as JSON (e.g. {"ultracode":"true"}). These
	// are intentionally NOT seeded by SeedDefaultSettings — absent = inherit —
	// so existing installs keep their current AFK behaviour until an override
	// is set.
	SettingSpawnModelDefaultAFK  = "spawn_model_default_afk"
	SettingSpawnEffortDefaultAFK = "spawn_effort_default_afk"
	SettingSpawnOptionsAFK       = "spawn_options_afk"

	// AFK-override provider default (issue #66): the AFK-only layer that
	// resolves BEFORE the base provider_default above (mirroring the
	// spawn_*_default_afk pair). Empty or absent = inherit the base chain, so
	// like the other AFK overrides it is intentionally NOT seeded by
	// SeedDefaultSettings.
	SettingSpawnProviderDefaultAFK = "spawn_provider_default_afk"

	// Remote-control spawn defaults (issue #163): does a session launch with the
	// provider's remote-control surface (claude's --remote-control, until now
	// hardcoded on)? spawn_remote_default above is the base layer under
	// repos.remote_default; spawn_remote_default_afk is its AFK-only override,
	// resolved FIRST for unattended runs and — like every other _afk key —
	// intentionally NOT seeded, because absent = inherit the base.
	//
	// The base key breaks the pattern of the ones above in one way: it IS seeded
	// (to "false"). It has to be. Every knob so far reads "" as unset, which is
	// safe only because "" is not a legal model/effort/provider id — but false IS
	// a legal remote value, so "unset" can never be spelled false. Hence the
	// layers below store NULL for inherit, and both keys are read through GetBool.
	SettingSpawnRemoteDefaultAFK = "spawn_remote_default_afk"

	// AFK seed-prompt override (issue #52 / ADR-0027): the global layer between
	// repos.afk_prompt and the built-in template (afk.SeedPromptTemplate).
	// Absent or blank = inherit the built-in — so like the AFK spawn defaults
	// above it is intentionally NOT seeded by SeedDefaultSettings, keeping every
	// existing install on the built-in prompt until an override is set. The
	// name is the plain "afk_prompt", NOT "..._afk": the "_afk" SUFFIX grammar
	// is reserved for the AFK override OF a base spawn default
	// (spawn_model_default → spawn_model_default_afk), and there is no base
	// prompt to override — so this matches the suffix-less afk_budget_minutes.
	SettingAFKPrompt = "afk_prompt"

	// Container resource-limit defaults (issue #205 / the 2026-07-22
	// container-isolation design): the global floor under repos.container_memory
	// /container_pids/container_nofile, applied to every container-mode run that
	// doesn't set its own per-repo override. The seeded values
	// (--memory=8g --pids-limit=4096 --ulimit nofile=16384:16384) are the ones
	// the design grilled and pinned; unlike the AFK-override keys above these ARE
	// seeded — a container run always needs a concrete limit, there is no lower
	// layer to inherit from.
	SettingContainerMemory = "container_memory"
	SettingContainerPids   = "container_pids"
	SettingContainerNofile = "container_nofile"
)

// containerMemoryRe is podman's --memory value grammar (issue #205): a
// positive integer, optionally suffixed with one b/k/m/g unit letter
// (case-insensitive) — e.g. "8g", "512m", "1073741824". Shared by the
// repo-level container_memory PATCH (reposvc.UpdateSettings) and the global
// container_memory setting PATCH (httpapi settings), so the per-repo override
// and the global default can never validate against two different grammars.
var containerMemoryRe = regexp.MustCompile(`^[1-9][0-9]*[bkmgBKMG]?$`)

// ValidContainerMemory reports whether s matches podman's --memory value
// grammar (see containerMemoryRe).
func ValidContainerMemory(s string) bool {
	return containerMemoryRe.MatchString(s)
}

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

// GetBool returns the value for key as a bool, falling back to def when the key
// is missing or blank. A present, non-blank value that is not exactly "true" or
// "false" is an error (fail loud, never silently coerce a typo to false — for a
// boolean knob false is a real answer, so a wrong one is indistinguishable from
// an intended one). Blank — not "false" — remains the "unset" spelling.
func (s *Store) GetBool(ctx context.Context, key string, def bool) (bool, error) {
	v, err := s.GetString(ctx, key, "")
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(v) {
	case "":
		return def, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("setting %q: not a boolean: %q", key, v)
}

// SeedDefaultSettings inserts the design §3a defaults for every missing key
// and never overwrites an existing row (--max-instances seeds the row on
// first start; thereafter settings wins). defaultProvider seeds the
// provider_default row (issue #66) — the caller passes the first registered
// provider's ID so the store stays provider-agnostic; "" seeds an
// empty-means-inherit row (the degraded no-provider boot). spawn_remote_default
// seeds "false" (issue #163): a boolean knob has no blank-means-unset spelling,
// so the base layer must state the default out loud — its AFK override stays
// unseeded, like the other _afk keys. The container limit trio (issue #205)
// is seeded the same way and for the same reason: a container-mode spawn
// always needs a concrete --memory/--pids-limit/--ulimit nofile, so there is
// no lower layer for "absent" to fall back to.
func (s *Store) SeedDefaultSettings(ctx context.Context, maxInstances int, defaultProvider string) error {
	defaults := map[string]string{
		SettingSpawnModelDefault:    "opus[1m]",
		SettingSpawnEffortDefault:   "max",
		SettingSpawnRemoteDefault:   "false",
		SettingProviderDefault:      defaultProvider,
		SettingMaxInstances:         strconv.Itoa(maxInstances),
		SettingAFKBudgetMinutes:     "120",
		SettingAFKTickSeconds:       "30",
		SettingAFKScheduleSeconds:   "45",
		SettingSweepIntervalMinutes: "10",
		SettingGitAuthorName:        "",
		SettingGitAuthorEmail:       "",
		SettingContainerMemory:      "8g",
		SettingContainerPids:        "4096",
		SettingContainerNofile:      "16384",
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
