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

// UpdateUserPasswordHash sets the stored hash for an existing user, or
// returns ErrNotFound when id does not exist. This exists for the boot-time
// seed-reconcile path (issue #137): when the configured PHC hash differs
// byte-wise from the stored one, the seed writes the configured hash here
// rather than refusing to start or silently diverging from config. It
// deliberately does NOT touch web_sessions — rotating the seeded hash must
// not log the operator out of an existing session.
func (s *Store) UpdateUserPasswordHash(ctx context.Context, id, passwordHash string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE users SET password_hash = ? WHERE id = ?`), passwordHash, id)
	if err != nil {
		return fmt.Errorf("update user password hash %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update user password hash %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("update user password hash %q: %w", id, ErrNotFound)
	}
	return nil
}

// Usernames lists every username, ordered for determinism. It exists for the
// seed-reconcile misconfiguration error (issue #137): when the configured
// seed username matches no existing user — a typo, or a rename since the
// last boot — the caller names the users that DO exist so the operator can
// fix config instead of guessing.
func (s *Store) Usernames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT username FROM users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("usernames: %w", err)
	}
	defer func() { _ = rows.Close() }()

	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("usernames: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usernames: %w", err)
	}
	return names, nil
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
