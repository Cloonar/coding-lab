-- 0013_lander_run_kind — widen runs.kind to admit 'lander' (postgres dialect;
-- issue #181 / ADR-0048). A lander run validates a claim's PR on the EXISTING
-- head branch; it is a fourth run kind beside manual/afk_manual/afk_auto.
--
-- The baseline's column-level CHECK got postgres's auto-generated name
-- runs_kind_check (<table>_<column>_check); the re-ADD names it explicitly so
-- the Down (and any later widening) never has to guess again.

-- +goose Up
ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('manual', 'afk_manual', 'afk_auto', 'lander'));

-- +goose Down
-- Fails loudly on any 'lander' row: ADD CONSTRAINT validates existing data,
-- so narrowing under live lander rows is not silently lossy.
ALTER TABLE runs DROP CONSTRAINT runs_kind_check;
ALTER TABLE runs ADD CONSTRAINT runs_kind_check
    CHECK (kind IN ('manual', 'afk_manual', 'afk_auto'));
