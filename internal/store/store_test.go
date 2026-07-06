package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/migrations"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// forEachBackend runs one suite body against sqlite (always) and postgres
// (only when LAB_TEST_POSTGRES_DSN is set — CI provides it from M2 on).
func forEachBackend(t *testing.T, fn func(t *testing.T, s *Store)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		fn(t, openTestSQLite(t))
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("LAB_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("LAB_TEST_POSTGRES_DSN not set")
		}
		fn(t, openTestPostgres(t, dsn))
	})
}

func openTestSQLite(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), "sqlite:"+filepath.Join(t.TempDir(), "lab.db"), discardLogger())
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openTestPostgres opens a store in a throwaway schema so parallel tests and
// reruns never see each other's tables.
func openTestPostgres(t *testing.T, baseDSN string) *Store {
	t.Helper()

	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	schema := "labtest_" + hex.EncodeToString(b[:])

	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("open postgres admin conn: %v", err)
	}
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = admin.Close()
	})

	sep := "?"
	if strings.Contains(baseDSN, "?") {
		sep = "&"
	}
	s, err := Open(context.Background(), baseDSN+sep+"search_path="+schema, discardLogger())
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func count(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestOpenRejectsUnknownScheme(t *testing.T) {
	_, err := Open(context.Background(), "mysql://user:secretpw@host/db", discardLogger())
	if err == nil {
		t.Fatal("Open accepted an unsupported scheme")
	}
	if !strings.Contains(err.Error(), "unsupported db scheme") {
		t.Errorf("error = %q, want the generic unsupported-scheme message", err)
	}
	if strings.Contains(err.Error(), "secretpw") {
		t.Errorf("error %q leaks the DSN password", err)
	}
	if strings.Contains(err.Error(), "mysql") {
		t.Errorf("error %q echoes DSN content", err)
	}
}

func TestOpenRejectsKeywordDSNWithoutLeakingIt(t *testing.T) {
	// A libpq keyword/value DSN has no scheme at all: everything —
	// password included — sits before any colon, so the error must never
	// echo any part of the DSN.
	const dsn = "host=db.example.com user=lab password=hunter2 dbname=lab"
	_, err := Open(context.Background(), dsn, discardLogger())
	if err == nil {
		t.Fatal("Open accepted a keyword-form DSN")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %q leaks the DSN password", err)
	}
	if strings.Contains(err.Error(), "db.example.com") {
		t.Errorf("error %q echoes DSN content", err)
	}
	if !strings.Contains(err.Error(), "unsupported db scheme (expected sqlite: or postgres://)") {
		t.Errorf("error = %q, want the generic unsupported-scheme message", err)
	}
}

func TestOpenRejectsEmptySQLitePath(t *testing.T) {
	if _, err := Open(context.Background(), "sqlite:", discardLogger()); err == nil {
		t.Fatal("Open accepted an empty sqlite path")
	}
}

func TestOpenCreatesParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "lab.db")
	s, err := Open(context.Background(), "sqlite:"+path, discardLogger())
	if err != nil {
		t.Fatalf("open with missing parent dir: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file not created: %v", err)
	}
}

func TestOpenIsIdempotentAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lab.db")
	ctx := context.Background()

	s1, err := Open(ctx, "sqlite:"+path, discardLogger())
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := s1.CreateUser(ctx, "admin", "hash"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopening runs goose again; the applied baseline must be a no-op and
	// data must survive.
	s2, err := Open(ctx, "sqlite:"+path, discardLogger())
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer func() { _ = s2.Close() }()
	n, err := s2.CountUsers(ctx)
	if err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 1 {
		t.Fatalf("users after reopen = %d, want 1", n)
	}
}

func TestRebind(t *testing.T) {
	tests := []struct {
		dia   dialect
		query string
		want  string
	}{
		{dialectSQLite, "SELECT * FROM users WHERE id = ? AND username = ?", "SELECT * FROM users WHERE id = ? AND username = ?"},
		{dialectPostgres, "SELECT * FROM users WHERE id = ? AND username = ?", "SELECT * FROM users WHERE id = $1 AND username = $2"},
		{dialectPostgres, "INSERT INTO t (a, b, c) VALUES (?, ?, ?)", "INSERT INTO t (a, b, c) VALUES ($1, $2, $3)"},
		{dialectPostgres, "SELECT 1", "SELECT 1"},
		{dialectPostgres, "UPDATE repos SET next_issue_number = next_issue_number + 1 WHERE id = ? RETURNING next_issue_number",
			"UPDATE repos SET next_issue_number = next_issue_number + 1 WHERE id = $1 RETURNING next_issue_number"},
	}
	for _, tt := range tests {
		s := &Store{dia: tt.dia}
		if got := s.rebind(tt.query); got != tt.want {
			t.Errorf("rebind(%q) on dialect %d = %q, want %q", tt.query, tt.dia, got, tt.want)
		}
	}
}

// TestMigrationParity walks both embedded migration trees and asserts they
// contain the identical set of version numbers (design §3a).
func TestMigrationParity(t *testing.T) {
	sqlite := migrationVersions(t, migrations.SQLite)
	postgres := migrationVersions(t, migrations.Postgres)

	if len(sqlite) == 0 {
		t.Fatal("no sqlite migrations embedded")
	}
	for v := range sqlite {
		if !postgres[v] {
			t.Errorf("migration version %s exists for sqlite but not postgres", v)
		}
	}
	for v := range postgres {
		if !sqlite[v] {
			t.Errorf("migration version %s exists for postgres but not sqlite", v)
		}
	}
}

func migrationVersions(t *testing.T, fsys fs.FS) map[string]bool {
	t.Helper()
	versions := make(map[string]bool)
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".sql") {
			return nil
		}
		name := filepath.Base(path)
		version, _, ok := strings.Cut(name, "_")
		if !ok || version == "" {
			t.Fatalf("migration %q has no NNNN_ version prefix", name)
		}
		if versions[version] {
			t.Fatalf("duplicate migration version %q", version)
		}
		versions[version] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk migrations: %v", err)
	}
	return versions
}
