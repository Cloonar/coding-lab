-- 0025_nested_containers — the repos.nested_containers per-repo knob (postgres
-- dialect; issue #115 / ADR-0086). A tri-state shaped exactly like the Runner
-- pick since migration 0024: NULL means "use the global default" — the
-- nested_containers_default settings row (store.SettingNestedContainersDefault,
-- seeded 'false' by store.SeedDefaultSettings, not here), read live at every
-- spawn by the one resolver in internal/instance (EffectiveNestedContainers)
-- — and a non-NULL TRUE/FALSE pins the repo regardless of what the global
-- default is or later becomes.
--
-- Nullable with NO default, and that is the whole design: FALSE is a legal
-- pin here (a repo that must stay off while the default is on), so "inherit"
-- has no in-band spelling but NULL — the Go field is *bool, like
-- repos.remote_default (migration 0011). Every existing row reads NULL after
-- the ALTER, i.e. inherits the seeded-off default, so an upgrade changes no
-- run's argv by itself; reposvc.Add stamps nil for every new repo too.
--
-- BOOLEAN follows the baseline's bool convention (repos.incogni, and the
-- nullable remote_default pair). The value only matters while the repo's
-- effective Runner is container; a host-runner repo ignores it, the same way
-- it ignores image_ref and the container limits. The two matching settings
-- keys (nested_containers_default, nested_image_cache) are plain settings rows
-- and need no schema change.
--
-- Identical to the sqlite dialect: a bare ADD COLUMN BOOLEAN (and DROP
-- COLUMN) is spelled the same in both.

-- +goose Up
ALTER TABLE repos ADD COLUMN nested_containers BOOLEAN;
-- +goose Down
ALTER TABLE repos DROP COLUMN nested_containers;
