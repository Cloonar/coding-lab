package store

// Web Push subscription accessors (issue #98). push_subscriptions holds one
// row per subscribed browser endpoint; there is deliberately no user_id FK —
// a subscription is device-level trust (the browser holds the keys, not an
// account), so it survives logout and outlives any single session. Rows are
// removed explicitly (operator/API action) or reactively when a push gateway
// answers 404/410 for a dead endpoint (the async sender's cleanup path).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

// PushSubscription is one row of push_subscriptions: the endpoint plus the
// two keys the Web Push protocol needs to encrypt a payload for it.
type PushSubscription struct {
	ID        string
	Endpoint  string
	P256dh    string
	Auth      string
	Label     string
	CreatedAt time.Time
}

// UpsertPushSubscription inserts a new subscription, or — when endpoint
// already has a row — updates its keys and label in place while preserving
// the row's id and created_at. Browsers reuse the same endpoint across
// re-subscribes (key rotation, permission re-grant), so this keeps one row
// per endpoint rather than accumulating stale duplicates. The candidate id
// and created_at are only bound for the insert branch; ON CONFLICT DO UPDATE
// never touches those columns, so RETURNING always reflects the row's real
// (possibly pre-existing) identity.
func (s *Store) UpsertPushSubscription(ctx context.Context, endpoint, p256dh, auth, label string) (PushSubscription, error) {
	sub := PushSubscription{
		ID:        ids.NewID("psub"),
		Endpoint:  endpoint,
		P256dh:    p256dh,
		Auth:      auth,
		Label:     label,
		CreatedAt: storedTime(s.Now()),
	}
	row := s.db.QueryRowContext(ctx, s.rebind(
		`INSERT INTO push_subscriptions (id, endpoint, p256dh, auth, label, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (endpoint) DO UPDATE
		 SET p256dh = excluded.p256dh, auth = excluded.auth, label = excluded.label
		 RETURNING id, endpoint, p256dh, auth, label, created_at`),
		sub.ID, sub.Endpoint, sub.P256dh, sub.Auth, sub.Label, fmtTime(sub.CreatedAt))

	var created string
	if err := row.Scan(&sub.ID, &sub.Endpoint, &sub.P256dh, &sub.Auth, &sub.Label, &created); err != nil {
		return PushSubscription{}, fmt.Errorf("upsert push subscription: %w", err)
	}
	var err error
	if sub.CreatedAt, err = parseTime(created); err != nil {
		return PushSubscription{}, fmt.Errorf("upsert push subscription: %w", err)
	}
	return sub, nil
}

// PushSubscriptions lists every subscription, newest first — the operator
// UI's device list order.
func (s *Store) PushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, endpoint, p256dh, auth, label, created_at
		 FROM push_subscriptions
		 ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("push subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var subs []PushSubscription
	for rows.Next() {
		var (
			sub     PushSubscription
			created string
		)
		if err := rows.Scan(&sub.ID, &sub.Endpoint, &sub.P256dh, &sub.Auth, &sub.Label, &created); err != nil {
			return nil, fmt.Errorf("push subscriptions: %w", err)
		}
		if sub.CreatedAt, err = parseTime(created); err != nil {
			return nil, fmt.Errorf("push subscriptions: %w", err)
		}
		subs = append(subs, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("push subscriptions: %w", err)
	}
	return subs, nil
}

// PushSubscriptionByID returns the subscription with the given id, or
// ErrNotFound.
func (s *Store) PushSubscriptionByID(ctx context.Context, id string) (PushSubscription, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT id, endpoint, p256dh, auth, label, created_at
		 FROM push_subscriptions WHERE id = ?`), id)

	var (
		sub     PushSubscription
		created string
	)
	if err := row.Scan(&sub.ID, &sub.Endpoint, &sub.P256dh, &sub.Auth, &sub.Label, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PushSubscription{}, fmt.Errorf("push subscription %q: %w", id, ErrNotFound)
		}
		return PushSubscription{}, fmt.Errorf("push subscription %q: %w", id, err)
	}
	var err error
	if sub.CreatedAt, err = parseTime(created); err != nil {
		return PushSubscription{}, fmt.Errorf("push subscription %q: %w", id, err)
	}
	return sub, nil
}

// DeletePushSubscription removes a subscription by id. ErrNotFound when no
// row matches (mirrors DeleteAPIToken).
func (s *Store) DeletePushSubscription(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`DELETE FROM push_subscriptions WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete push subscription: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete push subscription: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("delete push subscription: %w", ErrNotFound)
	}
	return nil
}

// DeletePushSubscriptionByEndpoint removes a subscription by endpoint.
// Unlike DeletePushSubscription this is idempotent — an absent endpoint is
// NOT an error — because the async sender calls it reactively on a gateway
// 404/410, and two in-flight sends to the same dead endpoint may race to
// clean it up.
func (s *Store) DeletePushSubscriptionByEndpoint(ctx context.Context, endpoint string) error {
	if _, err := s.db.ExecContext(ctx, s.rebind(
		`DELETE FROM push_subscriptions WHERE endpoint = ?`), endpoint); err != nil {
		return fmt.Errorf("delete push subscription by endpoint: %w", err)
	}
	return nil
}
