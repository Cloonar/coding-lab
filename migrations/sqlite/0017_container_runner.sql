-- 0017_container_runner — the repos.runner tracer-bullet slice (sqlite
-- dialect; issue #205 / the 2026-07-22 container-isolation design). runner is
-- NOT NULL DEFAULT 'host': every existing repo keeps running exactly as
-- before (the prlimit-wrapped host pane), and a fresh repo starts host too —
-- container is opt-in until the podman preflight is proven on the dev host
-- (issue #205's explicit out-of-scope: flipping the default). There is no DB
-- CHECK on the value, matching tracker_binding's precedent: the two-value
-- enum is validated app-side (reposvc.UpdateSettings), not in the schema.
--
-- container_memory/container_pids/container_nofile are the per-repo overrides
-- of the global container_memory/container_pids/container_nofile settings
-- (store.SettingContainerMemory &c., seeded "8g"/"4096"/"16384" — the
-- --memory/--pids-limit/--ulimit nofile values the grilled design pins).
-- NULL/absent on every column here means inherit the global default; a
-- non-NULL value pins this repo's container runs to it regardless of what the
-- global setting is or later becomes. All three stay meaningless for a
-- runner='host' repo — the prlimit wrapper (nofile only) is what host mode
-- still uses, and it is untouched by this migration.

-- +goose Up
ALTER TABLE repos ADD COLUMN runner TEXT NOT NULL DEFAULT 'host';
ALTER TABLE repos ADD COLUMN container_memory TEXT;
ALTER TABLE repos ADD COLUMN container_pids INTEGER;
ALTER TABLE repos ADD COLUMN container_nofile INTEGER;
-- +goose Down
ALTER TABLE repos DROP COLUMN container_nofile;
ALTER TABLE repos DROP COLUMN container_pids;
ALTER TABLE repos DROP COLUMN container_memory;
ALTER TABLE repos DROP COLUMN runner;
