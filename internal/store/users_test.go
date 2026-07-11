package store

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"
)

func TestUsers(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		fixed := time.Date(2026, 7, 5, 12, 34, 56, 789_123_456, time.UTC)
		s.Now = func() time.Time { return fixed }

		n, err := s.CountUsers(ctx)
		if err != nil {
			t.Fatalf("count on empty: %v", err)
		}
		if n != 0 {
			t.Fatalf("count on empty = %d, want 0", n)
		}

		u, err := s.CreateUser(ctx, "admin", "$argon2id$fakehash")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if ok, _ := regexp.MatchString(`^usr_[0-9a-f]{32}$`, u.ID); !ok {
			t.Errorf("user id = %q, want usr_<32 hex>", u.ID)
		}
		if want := fixed.Truncate(time.Millisecond); !u.CreatedAt.Equal(want) {
			t.Errorf("CreatedAt = %v, want %v", u.CreatedAt, want)
		}

		if n, _ = s.CountUsers(ctx); n != 1 {
			t.Fatalf("count after create = %d, want 1", n)
		}

		byName, err := s.UserByUsername(ctx, "admin")
		if err != nil {
			t.Fatalf("by username: %v", err)
		}
		if byName.ID != u.ID || byName.PasswordHash != "$argon2id$fakehash" {
			t.Errorf("by username = %+v, want the created user", byName)
		}
		if !byName.CreatedAt.Equal(u.CreatedAt) {
			t.Errorf("by username CreatedAt = %v, want %v", byName.CreatedAt, u.CreatedAt)
		}

		byID, err := s.UserByID(ctx, u.ID)
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if byID.Username != "admin" {
			t.Errorf("by id username = %q, want admin", byID.Username)
		}

		if _, err := s.UserByUsername(ctx, "ghost"); !errors.Is(err, ErrNotFound) {
			t.Errorf("by unknown username error = %v, want ErrNotFound", err)
		}
		if _, err := s.UserByID(ctx, "usr_00000000000000000000000000000000"); !errors.Is(err, ErrNotFound) {
			t.Errorf("by unknown id error = %v, want ErrNotFound", err)
		}

		if _, err := s.CreateUser(ctx, "admin", "otherhash"); err == nil {
			t.Error("duplicate username accepted, want UNIQUE violation")
		}

		if err := s.UpdateUserPasswordHash(ctx, u.ID, "$argon2id$rotatedhash"); err != nil {
			t.Fatalf("update password hash: %v", err)
		}
		rehashed, err := s.UserByUsername(ctx, "admin")
		if err != nil {
			t.Fatalf("by username after rehash: %v", err)
		}
		if rehashed.PasswordHash != "$argon2id$rotatedhash" {
			t.Errorf("password hash after update = %q, want $argon2id$rotatedhash", rehashed.PasswordHash)
		}

		if err := s.UpdateUserPasswordHash(ctx, "usr_00000000000000000000000000000000", "whatever"); !errors.Is(err, ErrNotFound) {
			t.Errorf("update password hash for unknown id error = %v, want ErrNotFound", err)
		}

		if _, err := s.CreateUser(ctx, "bob", "$argon2id$bobhash"); err != nil {
			t.Fatalf("create second user: %v", err)
		}
		names, err := s.Usernames(ctx)
		if err != nil {
			t.Fatalf("usernames: %v", err)
		}
		if want := []string{"admin", "bob"}; !equalStrings(names, want) {
			t.Errorf("usernames = %v, want %v", names, want)
		}
	})
}

// TestUsersUsernamesEmpty covers Usernames on an empty table separately: the
// main TestUsers flow always has at least one user by the time it would
// check this.
func TestUsersUsernamesEmpty(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		names, err := s.Usernames(ctx)
		if err != nil {
			t.Fatalf("usernames on empty: %v", err)
		}
		if len(names) != 0 {
			t.Errorf("usernames on empty = %v, want empty", names)
		}
	})
}
