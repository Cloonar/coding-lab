-- 0004_afk_spawn_defaults — AFK-override spawn defaults + provider options bag
-- (postgres dialect). Issue #19 / ADR-0021 splits AFK-run spawn defaults from
-- the manual pre-fill: repos gain optional AFK-override slots that resolve
-- BEFORE the base model_default/effort_default (NULL = inherit the base), plus a
-- provider-owned options bag stored as JSON (e.g. {"ultracode":"true"}). All
-- three are nullable and non-breaking: an existing repo keeps its current AFK
-- behaviour until an override is set. The matching global settings keys
-- (spawn_model_default_afk / spawn_effort_default_afk / spawn_options_afk) are
-- plain settings rows and need no schema change.

-- +goose Up
ALTER TABLE repos ADD COLUMN afk_model_default TEXT;
ALTER TABLE repos ADD COLUMN afk_effort_default TEXT;
ALTER TABLE repos ADD COLUMN afk_options TEXT;

-- +goose Down
ALTER TABLE repos DROP COLUMN afk_options;
ALTER TABLE repos DROP COLUMN afk_effort_default;
ALTER TABLE repos DROP COLUMN afk_model_default;
