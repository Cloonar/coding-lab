package store

// Web Push subscription accessor tests (issue #98): upsert-by-endpoint
// semantics (insert vs. update-in-place), list order, and the two delete
// paths' differing not-found behaviour (by-id errors, by-endpoint doesn't).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUpsertPushSubscriptionInsertsNew(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()

		sub, err := st.UpsertPushSubscription(ctx, "https://push.example/ep1", "p256dh-1", "auth-1", "laptop")
		if err != nil {
			t.Fatalf("UpsertPushSubscription: %v", err)
		}
		if !strings.HasPrefix(sub.ID, "psub_") {
			t.Errorf("id = %q, want psub_ prefix", sub.ID)
		}
		if sub.Endpoint != "https://push.example/ep1" || sub.P256dh != "p256dh-1" || sub.Auth != "auth-1" || sub.Label != "laptop" {
			t.Errorf("sub = %+v, want fields to match inputs", sub)
		}
		if sub.CreatedAt.IsZero() || sub.CreatedAt.After(time.Now().Add(time.Second)) {
			t.Errorf("created_at = %v, want a sane recent timestamp", sub.CreatedAt)
		}

		if n := count(t, st, "push_subscriptions"); n != 1 {
			t.Fatalf("row count = %d, want 1", n)
		}
	})
}

func TestUpsertPushSubscriptionSameEndpointUpdatesInPlace(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()

		first, err := st.UpsertPushSubscription(ctx, "https://push.example/ep1", "p256dh-1", "auth-1", "laptop")
		if err != nil {
			t.Fatalf("UpsertPushSubscription (initial): %v", err)
		}

		// Distinct clock tick so a bug that re-stamps created_at is visible.
		st.Now = func() time.Time { return time.Now().Add(time.Hour) }

		second, err := st.UpsertPushSubscription(ctx, "https://push.example/ep1", "p256dh-2", "auth-2", "phone")
		if err != nil {
			t.Fatalf("UpsertPushSubscription (re-subscribe): %v", err)
		}

		if second.ID != first.ID {
			t.Errorf("id changed on re-subscribe: first %q, second %q", first.ID, second.ID)
		}
		if !second.CreatedAt.Equal(first.CreatedAt) {
			t.Errorf("created_at changed on re-subscribe: first %v, second %v", first.CreatedAt, second.CreatedAt)
		}
		if second.P256dh != "p256dh-2" || second.Auth != "auth-2" || second.Label != "phone" {
			t.Errorf("second = %+v, want updated keys/label", second)
		}

		if n := count(t, st, "push_subscriptions"); n != 1 {
			t.Fatalf("row count after re-subscribe = %d, want 1 (no duplicate)", n)
		}

		got, err := st.PushSubscriptionByID(ctx, first.ID)
		if err != nil {
			t.Fatalf("PushSubscriptionByID: %v", err)
		}
		if got.P256dh != "p256dh-2" || got.Auth != "auth-2" || got.Label != "phone" {
			t.Errorf("stored row = %+v, want updated keys/label", got)
		}
	})
}

func TestPushSubscriptionsListsNewestFirst(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()

		st.Now = func() time.Time { return time.Now() }
		first, err := st.UpsertPushSubscription(ctx, "https://push.example/ep-a", "k1", "a1", "one")
		if err != nil {
			t.Fatalf("UpsertPushSubscription(a): %v", err)
		}
		st.Now = func() time.Time { return time.Now().Add(time.Minute) }
		second, err := st.UpsertPushSubscription(ctx, "https://push.example/ep-b", "k2", "a2", "two")
		if err != nil {
			t.Fatalf("UpsertPushSubscription(b): %v", err)
		}

		subs, err := st.PushSubscriptions(ctx)
		if err != nil {
			t.Fatalf("PushSubscriptions: %v", err)
		}
		if len(subs) != 2 || subs[0].ID != second.ID || subs[1].ID != first.ID {
			t.Fatalf("PushSubscriptions = %+v, want [second first]", subs)
		}
	})
}

func TestPushSubscriptionByIDNotFound(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()
		if _, err := st.PushSubscriptionByID(ctx, "psub_doesnotexist"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("PushSubscriptionByID err = %v, want ErrNotFound", err)
		}
	})
}

func TestDeletePushSubscription(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()

		sub, err := st.UpsertPushSubscription(ctx, "https://push.example/ep1", "k", "a", "")
		if err != nil {
			t.Fatalf("UpsertPushSubscription: %v", err)
		}

		if err := st.DeletePushSubscription(ctx, sub.ID); err != nil {
			t.Fatalf("DeletePushSubscription: %v", err)
		}
		if _, err := st.PushSubscriptionByID(ctx, sub.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("PushSubscriptionByID after delete err = %v, want ErrNotFound", err)
		}

		// Deleting an id that no longer exists is an error (mirrors
		// DeleteAPIToken — this path is an explicit operator action, not the
		// idempotent gateway-cleanup path).
		if err := st.DeletePushSubscription(ctx, sub.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("double DeletePushSubscription err = %v, want ErrNotFound", err)
		}
	})
}

func TestDeletePushSubscriptionByEndpointIsIdempotent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()

		sub, err := st.UpsertPushSubscription(ctx, "https://push.example/ep1", "k", "a", "")
		if err != nil {
			t.Fatalf("UpsertPushSubscription: %v", err)
		}

		if err := st.DeletePushSubscriptionByEndpoint(ctx, sub.Endpoint); err != nil {
			t.Fatalf("DeletePushSubscriptionByEndpoint: %v", err)
		}
		if _, err := st.PushSubscriptionByID(ctx, sub.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("PushSubscriptionByID after delete err = %v, want ErrNotFound", err)
		}

		// A gateway 404/410 for an endpoint that's already gone (two racing
		// sends, or a stale cached endpoint) must not surface as an error.
		if err := st.DeletePushSubscriptionByEndpoint(ctx, sub.Endpoint); err != nil {
			t.Fatalf("second DeletePushSubscriptionByEndpoint err = %v, want nil (idempotent)", err)
		}
		if err := st.DeletePushSubscriptionByEndpoint(ctx, "https://push.example/never-existed"); err != nil {
			t.Fatalf("DeletePushSubscriptionByEndpoint on unknown endpoint err = %v, want nil (idempotent)", err)
		}
	})
}
