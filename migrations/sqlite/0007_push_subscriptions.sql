-- 0007_push_subscriptions — Web Push subscription storage (sqlite dialect).
-- Issue #98: subscriptions are device-level trust, not tied to a user id —
-- they survive logout and are removed explicitly or when a push gateway
-- answers 404/410 for a dead endpoint. endpoint is UNIQUE so a browser
-- re-subscribing the same endpoint upserts keys/label in place instead of
-- accumulating duplicate rows.

-- +goose Up
CREATE TABLE push_subscriptions (
    id         TEXT PRIMARY KEY,
    endpoint   TEXT NOT NULL UNIQUE,
    p256dh     TEXT NOT NULL,
    auth       TEXT NOT NULL,
    label      TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE push_subscriptions;
