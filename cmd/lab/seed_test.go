package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/config"
	"git.cloonar.com/Cloonar/coding-lab/internal/httpapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// openTestStore opens a real sqlite store in a fresh temp dir so the seed path
// exercises the actual migrations + CreateUser/CountUsers it uses in run().
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, "sqlite:"+filepath.Join(t.TempDir(), "lab.db"), logx.New(io.Discard))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// writeFile writes body into a fresh file under t.TempDir() and returns its path.
func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func discardLogger() *slog.Logger { return logx.New(io.Discard) }

// TestSeedInitialUserNoConfig: with no seed config, seeding is a no-op and the
// DB stays at zero users (config.Parse guarantees all-or-nothing, so SeedUser=""
// is the "nothing configured" signal).
func TestSeedInitialUserNoConfig(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := seedInitialUser(ctx, st, config.Config{}, discardLogger()); err != nil {
		t.Fatalf("seedInitialUser: %v", err)
	}
	if n, err := st.CountUsers(ctx); err != nil || n != 0 {
		t.Fatalf("CountUsers = %d, %v; want 0, nil", n, err)
	}
}

// TestSeedInitialUserInlinePassword: fresh DB + inline password creates the
// user, and the stored hash verifies via the normal login verifier — pinning
// the acceptance that the seeded hash works on the standard login path.
func TestSeedInitialUserInlinePassword(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const user, pass = "operator", "correct horse"

	if err := seedInitialUser(ctx, st, config.Config{SeedUser: user, SeedPassword: pass}, discardLogger()); err != nil {
		t.Fatalf("seedInitialUser: %v", err)
	}
	u, err := st.UserByUsername(ctx, user)
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if u.Username != user {
		t.Fatalf("Username = %q, want %q", u.Username, user)
	}
	ok, err := httpapi.VerifyPassword(u.PasswordHash, pass)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(stored, %q) = %v, %v; want true, nil", pass, ok, err)
	}
}

// TestSeedInitialUserPasswordFileOneNewline: a password file with exactly one
// trailing newline is stored with the newline stripped — VerifyPassword
// succeeds on the stripped value and fails on the newline-included value.
func TestSeedInitialUserPasswordFileOneNewline(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const user, pass = "operator", "filesecret"
	path := writeFile(t, "pw", pass+"\n")

	if err := seedInitialUser(ctx, st, config.Config{SeedUser: user, SeedPasswordFile: path}, discardLogger()); err != nil {
		t.Fatalf("seedInitialUser: %v", err)
	}
	u, err := st.UserByUsername(ctx, user)
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if ok, err := httpapi.VerifyPassword(u.PasswordHash, pass); err != nil || !ok {
		t.Fatalf("VerifyPassword(stored, stripped) = %v, %v; want true, nil", ok, err)
	}
	if ok, _ := httpapi.VerifyPassword(u.PasswordHash, pass+"\n"); ok {
		t.Fatal("VerifyPassword(stored, value+newline) = true; newline should have been stripped")
	}
}

// TestSeedInitialUserPasswordFileTwoNewlines: two trailing newlines → exactly
// one is stripped, so the stored value keeps the second newline.
func TestSeedInitialUserPasswordFileTwoNewlines(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const user, base = "operator", "twoNLpass"
	path := writeFile(t, "pw", base+"\n\n")

	if err := seedInitialUser(ctx, st, config.Config{SeedUser: user, SeedPasswordFile: path}, discardLogger()); err != nil {
		t.Fatalf("seedInitialUser: %v", err)
	}
	u, err := st.UserByUsername(ctx, user)
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if ok, err := httpapi.VerifyPassword(u.PasswordHash, base+"\n"); err != nil || !ok {
		t.Fatalf("VerifyPassword(stored, value+one newline) = %v, %v; want true, nil (exactly one stripped)", ok, err)
	}
	if ok, _ := httpapi.VerifyPassword(u.PasswordHash, base); ok {
		t.Fatal("VerifyPassword(stored, fully-stripped) = true; only one newline should be stripped")
	}
}

// TestSeedInitialUserFileWinsOverInline: when both sources are set, the file
// value is the one stored.
func TestSeedInitialUserFileWinsOverInline(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const user, inline, fromFile = "operator", "inlinePassword", "filePassword"
	path := writeFile(t, "pw", fromFile+"\n")

	cfg := config.Config{SeedUser: user, SeedPassword: inline, SeedPasswordFile: path}
	if err := seedInitialUser(ctx, st, cfg, discardLogger()); err != nil {
		t.Fatalf("seedInitialUser: %v", err)
	}
	u, err := st.UserByUsername(ctx, user)
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if ok, err := httpapi.VerifyPassword(u.PasswordHash, fromFile); err != nil || !ok {
		t.Fatalf("VerifyPassword(stored, file value) = %v, %v; want true, nil (file wins)", ok, err)
	}
	if ok, _ := httpapi.VerifyPassword(u.PasswordHash, inline); ok {
		t.Fatal("VerifyPassword(stored, inline value) = true; the file source should have won")
	}
}

// TestSeedInitialUserExistingUsersSkips: on a non-empty DB the seed is a no-op —
// no new user, the existing hash is byte-unchanged, and (the key ordering
// property) a NONEXISTENT SeedPasswordFile does NOT error, because the
// bootstrap gate fires before the file is ever read.
func TestSeedInitialUserExistingUsersSkips(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	existing, err := st.CreateUser(ctx, "already-here", httpapi.HashPassword("preexisting"))
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	cfg := config.Config{SeedUser: "operator", SeedPasswordFile: filepath.Join(t.TempDir(), "does-not-exist")}
	if err := seedInitialUser(ctx, st, cfg, discardLogger()); err != nil {
		t.Fatalf("seedInitialUser on non-empty DB = %v; want nil (gate before file read)", err)
	}
	if n, err := st.CountUsers(ctx); err != nil || n != 1 {
		t.Fatalf("CountUsers = %d, %v; want 1, nil (no new user)", n, err)
	}
	got, err := st.UserByUsername(ctx, "already-here")
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if got.PasswordHash != existing.PasswordHash {
		t.Fatal("existing user's password hash changed; seed must not touch existing users")
	}
	if _, err := st.UserByUsername(ctx, "operator"); err == nil {
		t.Fatal("seeded user was created despite an existing user")
	}
}

// TestSeedInitialUserMissingPasswordFile: fresh DB + a missing password file
// fails loud, naming the path (empty-DB qualifier of the acceptance).
func TestSeedInitialUserMissingPasswordFile(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	path := filepath.Join(t.TempDir(), "absent")
	cfg := config.Config{SeedUser: "operator", SeedPasswordFile: path}
	err := seedInitialUser(ctx, st, cfg, discardLogger())
	if err == nil {
		t.Fatal("seedInitialUser with missing password file = nil; want error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not mention the path %q", err, path)
	}
	if n, _ := st.CountUsers(ctx); n != 0 {
		t.Fatalf("CountUsers = %d; want 0 (no user on failed seed)", n)
	}
}

// TestSeedInitialUserValidationErrors: the shared validator rejects a too-short
// password and a too-long username, on an empty DB, before any user is created.
func TestSeedInitialUserValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
	}{
		{
			name: "7-char password",
			cfg:  config.Config{SeedUser: "operator", SeedPassword: "1234567"},
		},
		{
			name: "65-char username",
			cfg:  config.Config{SeedUser: strings.Repeat("u", 65), SeedPassword: "longenough"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			st := openTestStore(t)
			if err := seedInitialUser(ctx, st, tt.cfg, discardLogger()); err == nil {
				t.Fatal("seedInitialUser = nil; want validation error")
			}
			if n, _ := st.CountUsers(ctx); n != 0 {
				t.Fatalf("CountUsers = %d; want 0 (no user on validation failure)", n)
			}
		})
	}
}
