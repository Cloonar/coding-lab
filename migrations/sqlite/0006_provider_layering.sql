-- 0006_provider_layering — three-level agent-CLI selection (sqlite dialect).
-- Issue #66 layers the effective provider like model/effort (ADR-0021):
-- per-spawn pick → repos.provider → global provider_default → first
-- registered, with a symmetric AFK override (repos.afk_provider_default →
-- spawn_provider_default_afk) consulted first for AFK runs. repos.provider
-- becomes nullable (NULL = inherit); every existing row is reset to NULL
-- because today's values were stamped by code at create time, never chosen
-- by an operator — NULL preserves behaviour via the seeded global default.
-- sqlite cannot drop NOT NULL in place; since all values become NULL anyway,
-- DROP COLUMN + re-ADD as nullable TEXT is equivalent (the baseline declares
-- no index or constraint on provider).

-- +goose Up
ALTER TABLE repos DROP COLUMN provider;
ALTER TABLE repos ADD COLUMN provider TEXT;
ALTER TABLE repos ADD COLUMN afk_provider_default TEXT;

-- +goose Down
ALTER TABLE repos DROP COLUMN afk_provider_default;
ALTER TABLE repos DROP COLUMN provider;
ALTER TABLE repos ADD COLUMN provider TEXT NOT NULL DEFAULT 'claude-code';
