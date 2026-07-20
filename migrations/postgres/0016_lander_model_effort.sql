-- 0016_lander_model_effort — per-repo lander model/effort overrides (postgres
-- dialect; issue #189). Both are nullable with no default: NULL means the
-- lander run inherits the normal model/effort resolution (repo base default
-- → global base default → provider default), exactly as lander_provider
-- (0012) already does for the provider knob. A non-NULL value is a strict
-- per-spawn request — an unknown model/effort fails the launch rather than
-- silently falling back. No existing repo gains an override from this
-- migration; every repo keeps inheriting as before.
--
-- Identical to the sqlite dialect — a bare ALTER TABLE ... ADD COLUMN TEXT
-- (and DROP COLUMN) is spelled the same in both, so no statement here
-- diverges.

-- +goose Up
ALTER TABLE repos ADD COLUMN lander_model TEXT;
ALTER TABLE repos ADD COLUMN lander_effort TEXT;

-- +goose Down
ALTER TABLE repos DROP COLUMN lander_effort;
ALTER TABLE repos DROP COLUMN lander_model;
