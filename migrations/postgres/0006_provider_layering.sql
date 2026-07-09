-- 0006_provider_layering — three-level agent-CLI selection (postgres dialect).
-- Issue #66 layers the effective provider like model/effort (ADR-0021):
-- per-spawn pick → repos.provider → global provider_default → first
-- registered, with a symmetric AFK override (repos.afk_provider_default →
-- spawn_provider_default_afk) consulted first for AFK runs. repos.provider
-- becomes nullable (NULL = inherit); every existing row is reset to NULL
-- because today's values were stamped by code at create time, never chosen
-- by an operator — NULL preserves behaviour via the seeded global default.

-- +goose Up
ALTER TABLE repos ALTER COLUMN provider DROP NOT NULL;
ALTER TABLE repos ALTER COLUMN provider DROP DEFAULT;
UPDATE repos SET provider = NULL;
ALTER TABLE repos ADD COLUMN afk_provider_default TEXT;

-- +goose Down
ALTER TABLE repos DROP COLUMN afk_provider_default;
UPDATE repos SET provider = 'claude-code' WHERE provider IS NULL;
ALTER TABLE repos ALTER COLUMN provider SET DEFAULT 'claude-code';
ALTER TABLE repos ALTER COLUMN provider SET NOT NULL;
