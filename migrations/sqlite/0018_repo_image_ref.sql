-- 0018_repo_image_ref — the repos.image_ref per-repo dev-image slice (sqlite
-- dialect; issue #207). image_ref is nullable: NULL means inherit the
-- globally configured default dev image (--container-image); a non-NULL
-- value is always a digest-pinned OCI ref, pinned by reposvc on save (NOT
-- here — the store stays dumb and persists whatever it is given). It only
-- matters while repos.runner = 'container' (migration 0017); a runner='host'
-- repo ignores it just like it ignores container_memory/container_pids/
-- container_nofile.

-- +goose Up
ALTER TABLE repos ADD COLUMN image_ref TEXT;
-- +goose Down
ALTER TABLE repos DROP COLUMN image_ref;
