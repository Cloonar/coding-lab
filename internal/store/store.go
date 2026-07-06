// Package store owns database access: opening SQLite or PostgreSQL from a
// DSN, applying the embedded goose migrations, and typed accessors over the
// schema.
//
// Cross-cutting conventions it enforces (design §2):
//   - Timestamps are TEXT, fixed-width, always UTC. fmtTime/parseTime are the
//     only helpers; lexicographic order on stored values equals chronological
//     order, which every ORDER BY / range comparison relies on.
//   - Queries are written with `?` placeholders; rebind rewrites them to $N
//     for postgres. Never put `?` inside SQL string literals.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // database/sql driver "sqlite"

	"git.cloonar.com/Cloonar/coding-lab/migrations"
)

// ErrNotFound is the sentinel for a requested row that does not exist.
var ErrNotFound = errors.New("not found")

type dialect int

const (
	dialectSQLite dialect = iota
	dialectPostgres
)

// Store is the repository layer over one open database.
type Store struct {
	db  *sql.DB
	dia dialect
	log *slog.Logger

	// Now is the clock used for rows the store stamps itself (CreateUser).
	// Tests may replace it; it must never be nil.
	Now func() time.Time
}

// Open connects to the database named by dsn — `sqlite:<path>` or
// `postgres://…` / `postgresql://…` — and applies all pending embedded
// migrations. The sqlite path uses the pinned open recipe (design §3a):
// foreign_keys(1), busy_timeout(5000), WAL, immediate transactions, and a
// single connection (single-writer discipline). Postgres keeps the default
// pool.
func Open(ctx context.Context, dsn string, logger *slog.Logger) (*Store, error) {
	if logger == nil {
		logger = slog.Default()
	}

	var (
		db           *sql.DB
		dia          dialect
		gooseDialect goose.Dialect
		migrationFS  fs.FS
		err          error
	)
	switch {
	case strings.HasPrefix(dsn, "sqlite:"):
		path := strings.TrimPrefix(dsn, "sqlite:")
		if path == "" {
			return nil, errors.New("open store: empty sqlite path in dsn")
		}
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("open store: create db dir: %w", err)
			}
		}
		// Pinned DSN recipe. Without foreign_keys(1) every ON DELETE CASCADE
		// silently no-ops on sqlite.
		sqliteDSN := "file:" + path +
			"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
		db, err = sql.Open("sqlite", sqliteDSN)
		if err != nil {
			return nil, fmt.Errorf("open store: sqlite: %w", err)
		}
		db.SetMaxOpenConns(1)
		dia, gooseDialect, migrationFS = dialectSQLite, goose.DialectSQLite3, migrations.SQLite
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		db, err = sql.Open("pgx", dsn)
		if err != nil {
			return nil, fmt.Errorf("open store: postgres: %w", err)
		}
		dia, gooseDialect, migrationFS = dialectPostgres, goose.DialectPostgres, migrations.Postgres
	default:
		// Never echo any part of the DSN: a keyword-form DSN has no scheme
		// at all ("host=… password=…"), so even the prefix before the first
		// colon can carry the password into logs.
		return nil, errors.New("open store: unsupported db scheme (expected sqlite: or postgres://)")
	}

	s := &Store{db: db, dia: dia, log: logger, Now: time.Now}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open store: ping: %w", err)
	}
	if err := s.migrate(ctx, gooseDialect, migrationFS); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context, d goose.Dialect, fsys fs.FS) error {
	p, err := goose.NewProvider(d, s.db, fsys)
	if err != nil {
		return fmt.Errorf("init migrations: %w", err)
	}
	results, err := p.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	for _, r := range results {
		s.log.Info("applied migration", "component", "store", "version", r.Source.Version)
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// rebind rewrites `?` placeholders to `$1…$N` for postgres and returns the
// query unchanged for sqlite. Queries must never contain `?` inside string
// literals (design §2).
func (s *Store) rebind(query string) string {
	if s.dia != dialectPostgres {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

// timeFormat renders exactly 3 fractional digits and 'Z' for UTC: 24 bytes,
// fixed width, so lexicographic == chronological.
const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// fmtTime is the single formatter for stored timestamps.
func fmtTime(t time.Time) string { return t.UTC().Format(timeFormat) }

// FormatTime renders t exactly as the store persists it (the single pinned
// layout, design §2). API responses use it so callers see stored values
// byte-for-byte — nothing formats timestamps ad hoc.
func FormatTime(t time.Time) string { return fmtTime(t) }

// parseTime is the single parser for stored timestamps.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse stored time %q: %w", s, err)
	}
	return t, nil
}

// storedTime is what a time.Time becomes after a fmtTime round trip:
// millisecond precision, UTC. Accessors return created rows in this form so
// callers always see exactly what the database holds.
func storedTime(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

// fmtNullTime renders an optional timestamp for binding (nil → NULL).
func fmtNullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

// parseNullTime converts a scanned nullable timestamp column.
func parseNullTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid {
		return nil, nil
	}
	t, err := parseTime(ns.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
