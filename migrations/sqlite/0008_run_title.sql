-- 0008_run_title — user-set run title overlay (sqlite dialect).
-- A run can be renamed from the UI (issue #111): runs gain a nullable title
-- that is a pure display overlay — NULL means no override and the display
-- falls back to the session name. Never part of identity: session_name,
-- branch, worktree, and tmux stay immutable.

-- +goose Up
ALTER TABLE runs ADD COLUMN title TEXT;

-- +goose Down
ALTER TABLE runs DROP COLUMN title;
