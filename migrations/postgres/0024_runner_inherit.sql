-- 0024_runner_inherit — repos.runner gains an inherit state (postgres
-- dialect; issue #55). Until now runner was NOT NULL DEFAULT 'host' (migration
-- 0017) and every new repo was stamped host; there was no global knob. From
-- here runner is nullable with NO default: NULL means "use the global runner
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
-- Down maps inheriting (NULL) rows to 'host' — the value they would have
-- been stamped before this migration — and restores NOT NULL DEFAULT 'host'.
--
-- Diverges from the sqlite dialect: postgres alters the column's constraints
-- in place, where sqlite has to swap the column for a new one (see that
-- file). Both leave every row's value untouched on the way up.

-- +goose Up
ALTER TABLE repos ALTER COLUMN runner DROP NOT NULL;
ALTER TABLE repos ALTER COLUMN runner DROP DEFAULT;
-- +goose Down
UPDATE repos SET runner = 'host' WHERE runner IS NULL;
ALTER TABLE repos ALTER COLUMN runner SET DEFAULT 'host';
ALTER TABLE repos ALTER COLUMN runner SET NOT NULL;
