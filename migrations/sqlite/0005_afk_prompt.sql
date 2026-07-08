-- 0005_afk_prompt — AFK seed-prompt override (sqlite dialect). Issue #52 /
-- ADR-0027 layers an operator override over the built-in AFK seed prompt:
-- repos.afk_prompt → global settings key afk_prompt → the built-in template
-- (NULL/blank = inherit the next layer). A non-empty override REPLACES the
-- whole built-in prompt, with optional <N>/<BRANCH> tokens interpolated at
-- spawn. The column is nullable and non-breaking: an existing repo keeps the
-- built-in prompt until an override is set. The matching global settings key
-- (afk_prompt) is a plain settings row and needs no schema change.

-- +goose Up
ALTER TABLE repos ADD COLUMN afk_prompt TEXT;

-- +goose Down
ALTER TABLE repos DROP COLUMN afk_prompt;
