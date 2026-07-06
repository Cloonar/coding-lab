package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

// User is one row of users. Single-operator for now (brief D2), but the
// schema and accessors are already user-shaped.
type User struct {
	ID           string
	Username     string
	PasswordHash string // argon2id, PHC-encoded
	CreatedAt    time.Time
}

// CountUsers reports how many users exist (0 → first-run setup is open).
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// CreateUser inserts a new user with a fresh usr_ id, stamped with the
// store clock.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (User, error) {
	u := User{
		ID:           ids.NewID("usr"),
		Username:     username,
		PasswordHash: passwordHash,
		CreatedAt:    storedTime(s.Now()),
	}
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, ?, ?)`),
		u.ID, u.Username, u.PasswordHash, fmtTime(u.CreatedAt))
	if err != nil {
		return User{}, fmt.Errorf("create user %q: %w", username, err)
	}
	return u, nil
}

// UserByUsername returns the user with the given username, or ErrNotFound.
func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	u, err := s.userWhere(ctx, "username = ?", username)
	if err != nil {
		return User{}, fmt.Errorf("user by username %q: %w", username, err)
	}
	return u, nil
}

// UserByID returns the user with the given id, or ErrNotFound.
func (s *Store) UserByID(ctx context.Context, id string) (User, error) {
	u, err := s.userWhere(ctx, "id = ?", id)
	if err != nil {
		return User{}, fmt.Errorf("user by id %q: %w", id, err)
	}
	return u, nil
}

func (s *Store) userWhere(ctx context.Context, where string, arg any) (User, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT id, username, password_hash, created_at FROM users WHERE `+where), arg)
	var (
		u       User
		created string
	)
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	t, err := parseTime(created)
	if err != nil {
		return User{}, err
	}
	u.CreatedAt = t
	return u, nil
}
