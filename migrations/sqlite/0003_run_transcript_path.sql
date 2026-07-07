-- 0003_run_transcript_path — embedded chat transcript pointer (sqlite dialect).
-- The chat reads an instance's conversation through the provider-native
-- transcript file (ADR-0016): runs gain the transcript's absolute path,
-- captured async by worktree-cwd match (the deep_link_url pattern) so ended
-- runs stay readable while the provider retains the file. Read-through only —
-- no message table; NULL means "not captured yet" and degrades gracefully.

-- +goose Up
ALTER TABLE runs ADD COLUMN transcript_path TEXT;

-- +goose Down
ALTER TABLE runs DROP COLUMN transcript_path;
