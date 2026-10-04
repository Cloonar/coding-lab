-- 0024_runner_inherit — repos.runner gains an inherit state (sqlite dialect;
-- issue #55). Until now runner was NOT NULL DEFAULT 'host' (migration 0017)
-- and every new repo was stamped host; there was no global knob. From here
-- runner is nullable with NO default: NULL means "use the global runner
-- default" — the runner_default settings row (store.SettingRunnerDefault,
-- seeded 'host'), read live at every spawn by the one effective-Runner
-- resolver in internal/instance — and a non-NULL host/container pins the repo
-- regardless of what the global default is or later becomes.
--
-- Every existing row keeps its value exactly, so an existing repo's explicit
-- host or container becomes a pin and an upgrade changes nothing by itself;
-- only repos created after this migration start NULL (reposvc.Add stamps
-- nil). Still no DB CHECK on the value — the host|container enum stays
-- app-side (reposvc.UpdateSettings), as 0017 decided.
--
-- sqlite cannot alter a column's constraints in place, and rebuilding repos
-- would mean the 0013-style foreign_keys-OFF dance across every table that
-- REFERENCES it. A column swap avoids touching the table at all: add the new
-- nullable column, copy every value across, drop the old column, rename the
-- new one into its place. The column moves to the END of the table, which is
-- harmless — the store never relies on column order (every repo SELECT and
-- INSERT names its columns through store.repoColumns, never SELECT *).
--
-- Down maps inheriting (NULL) rows to 'host' — the value they would have
-- been stamped before this migration — and restores NOT NULL DEFAULT 'host'
-- through the same swap.

-- +goose Up
ALTER TABLE repos ADD COLUMN runner_new TEXT;
UPDATE repos SET runner_new = runner;
ALTER TABLE repos DROP COLUMN runner;
ALTER TABLE repos RENAME COLUMN runner_new TO runner;
-- +goose Down
ALTER TABLE repos ADD COLUMN runner_old TEXT NOT NULL DEFAULT 'host';
UPDATE repos SET runner_old = COALESCE(runner, 'host');
ALTER TABLE repos DROP COLUMN runner;
ALTER TABLE repos RENAME COLUMN runner_old TO runner;
