package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/config"
	"git.cloonar.com/Cloonar/coding-lab/internal/httpapi"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// openTestStore opens a real sqlite store in a fresh temp dir so the seed path
// exercises the actual migrations + CreateUser/CountUsers/UserByUsername/
// UpdateUserPasswordHash it uses in run().
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

// TestSeedInitialUserInlineHash: fresh DB + an inline PHC hash creates the user
// with the hash stored BYTE-IDENTICALLY (seeding takes a pre-hashed password —
// issue #137 — so it must not re-hash), and the stored hash verifies via the
// normal login verifier, pinning that a seeded hash works on the login path.
func TestSeedInitialUserInlineHash(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const user, pass = "operator", "some password"
	hash := httpapi.HashPassword(pass)

	cfg := config.Config{SeedUser: user, SeedPasswordHash: hash}
	if err := seedInitialUser(ctx, st, cfg, discardLogger()); err != nil {
		t.Fatalf("seedInitialUser: %v", err)
	}
	u, err := st.UserByUsername(ctx, user)
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if u.Username != user {
		t.Fatalf("Username = %q, want %q", u.Username, user)
	}
	if u.PasswordHash != hash {
		t.Fatalf("stored hash was rewritten; want byte-identical to the configured hash")
	}
	ok, err := httpapi.VerifyPassword(u.PasswordHash, pass)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(stored, %q) = %v, %v; want true, nil", pass, ok, err)
	}
}

// TestSeedInitialUserHashFileOneNewline: a hash file with exactly one trailing
// newline is stored with the newline stripped — byte-compared against the
// minted hash (strings.CutSuffix removes exactly one "\n").
func TestSeedInitialUserHashFileOneNewline(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const user = "operator"
	hash := httpapi.HashPassword("some password")
	path := writeFile(t, "hash", hash+"\n")

	cfg := config.Config{SeedUser: user, SeedPasswordHashFile: path}
	if err := seedInitialUser(ctx, st, cfg, discardLogger()); err != nil {
		t.Fatalf("seedInitialUser: %v", err)
	}
	u, err := st.UserByUsername(ctx, user)
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if u.PasswordHash != hash {
		t.Fatalf("stored hash = %q, want %q (exactly one trailing newline stripped)", u.PasswordHash, hash)
	}
}

// TestSeedInitialUserFileWinsOverInline: when both sources are set, the file's
// hash is the one stored — byte-compared against the file's minted hash, with
// the distinct inline hash confirmed absent (file wins).
func TestSeedInitialUserFileWinsOverInline(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const user = "operator"
	inlineHash := httpapi.HashPassword("inline secret")
	fileHash := httpapi.HashPassword("file secret")
	path := writeFile(t, "hash", fileHash+"\n")

	cfg := config.Config{SeedUser: user, SeedPasswordHash: inlineHash, SeedPasswordHashFile: path}
	if err := seedInitialUser(ctx, st, cfg, discardLogger()); err != nil {
		t.Fatalf("seedInitialUser: %v", err)
	}
	u, err := st.UserByUsername(ctx, user)
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if u.PasswordHash != fileHash {
		t.Fatalf("stored hash = %q, want the file's hash %q (file wins)", u.PasswordHash, fileHash)
	}
	if u.PasswordHash == inlineHash {
		t.Fatal("stored hash equals the inline hash; the file source should have won")
	}
}

// TestSeedInitialUserReconcileUpdate: seeding once with hash A then again with
// hash B rotates the stored hash to exactly B, with the user count unchanged.
// An existing web session for the user survives the rotation — the store's
// UpdateUserPasswordHash deliberately never touches web_sessions (issue #137),
// so a hash rotation must not log a live operator out.
func TestSeedInitialUserReconcileUpdate(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const user = "operator"
	hashA := httpapi.HashPassword("password A")
	hashB := httpapi.HashPassword("password B")

	if err := seedInitialUser(ctx, st, config.Config{SeedUser: user, SeedPasswordHash: hashA}, discardLogger()); err != nil {
		t.Fatalf("first seedInitialUser: %v", err)
	}
	u, err := st.UserByUsername(ctx, user)
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}

	// A live session for the seeded user, created before the rotation so we can
	// assert it survives (sessions outlive a seed-hash rotation).
	const sessID = "sess_reconcile_survivor"
	now := time.Now()
	if err := st.CreateWebSession(ctx, store.WebSession{
		ID:        sessID,
		UserID:    u.ID,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateWebSession: %v", err)
	}

	if err := seedInitialUser(ctx, st, config.Config{SeedUser: user, SeedPasswordHash: hashB}, discardLogger()); err != nil {
		t.Fatalf("second seedInitialUser: %v", err)
	}
	u, err = st.UserByUsername(ctx, user)
	if err != nil {
		t.Fatalf("UserByUsername after rotation: %v", err)
	}
	if u.PasswordHash != hashB {
		t.Fatalf("stored hash = %q, want the rotated hash %q", u.PasswordHash, hashB)
	}
	if n, err := st.CountUsers(ctx); err != nil || n != 1 {
		t.Fatalf("CountUsers = %d, %v; want 1, nil (rotation, not a new user)", n, err)
	}
	if _, err := st.WebSession(ctx, sessID); err != nil {
		t.Fatalf("WebSession after rotation = %v; want the session to survive the hash rotation", err)
	}
}

// TestSeedInitialUserReconcileNoop: seeding twice with the same hash is a no-op
// on the second pass — no error, the stored hash is unchanged, and the count
// stays 1.
func TestSeedInitialUserReconcileNoop(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const user = "operator"
	hash := httpapi.HashPassword("some password")
	cfg := config.Config{SeedUser: user, SeedPasswordHash: hash}

	if err := seedInitialUser(ctx, st, cfg, discardLogger()); err != nil {
		t.Fatalf("first seedInitialUser: %v", err)
	}
	if err := seedInitialUser(ctx, st, cfg, discardLogger()); err != nil {
		t.Fatalf("second seedInitialUser: %v", err)
	}
	u, err := st.UserByUsername(ctx, user)
	if err != nil {
		t.Fatalf("UserByUsername: %v", err)
	}
	if u.PasswordHash != hash {
		t.Fatalf("stored hash changed on a no-op reseed: got %q, want %q", u.PasswordHash, hash)
	}
	if n, err := st.CountUsers(ctx); err != nil || n != 1 {
		t.Fatalf("CountUsers = %d, %v; want 1, nil", n, err)
	}
}

// TestSeedInitialUserMismatchRefusesStart: when config names a seed user the DB
// does not have but other users do exist, seeding refuses to start rather than
// silently create a second account or leave the configured credential dead
// (config is the credential's source of truth — issue #137). The error names
// BOTH the configured seed user and the existing one, and no user is created.
func TestSeedInitialUserMismatchRefusesStart(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if _, err := st.CreateUser(ctx, "already-here", httpapi.HashPassword("preexisting")); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	cfg := config.Config{SeedUser: "operator", SeedPasswordHash: httpapi.HashPassword("some password")}
	err := seedInitialUser(ctx, st, cfg, discardLogger())
	if err == nil {
		t.Fatal("seedInitialUser with a mismatched seed user = nil; want an error")
	}
	for _, want := range []string{"operator", "already-here"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
	if n, _ := st.CountUsers(ctx); n != 1 {
		t.Fatalf("CountUsers = %d; want 1 (no user created on a mismatch)", n)
	}
}

// TestSeedInitialUserMissingHashFileNonEmptyDB pins the #135 reversal: the hash
// file is read on EVERY boot, before any DB gate, so a missing file fails loud
// (naming the path) even on a NON-EMPTY DB. The pre-#137 seed gated on an empty
// DB first and returned nil here; under reconcile, config is the source of
// truth, so an unreadable configured hash is a hard error.
func TestSeedInitialUserMissingHashFileNonEmptyDB(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if _, err := st.CreateUser(ctx, "someone", httpapi.HashPassword("preexisting")); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	path := filepath.Join(t.TempDir(), "absent")
	cfg := config.Config{SeedUser: "operator", SeedPasswordHashFile: path}
	err := seedInitialUser(ctx, st, cfg, discardLogger())
	if err == nil {
		t.Fatal("seedInitialUser with a missing hash file on a non-empty DB = nil; want error (#135 reversal)")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error %q does not mention the path %q", err, path)
	}
	if n, _ := st.CountUsers(ctx); n != 1 {
		t.Fatalf("CountUsers = %d; want 1 (no user created on a failed seed)", n)
	}
}

// TestSeedInitialUserMalformedHash: a structurally invalid hash refuses startup
// (shared parser with the login path — issue #137) and creates no user. The
// hash never reaches the DB, so validation must happen before CreateUser.
func TestSeedInitialUserMalformedHash(t *testing.T) {
	tests := []struct {
		name string
		hash string
	}{
		// Five '$' parts instead of six.
		{"truncated", "$argon2id$v=19$m=65536,t=3,p=4$abc"},
		// argon2i is not argon2id.
		{"wrong scheme", "$argon2i$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$c29tZWtleQ"},
		// '!' is outside the base64 alphabet, in the salt field...
		{"bad base64 salt", "$argon2id$v=19$m=65536,t=3,p=4$!!!bad$c29tZWtleQ"},
		// ...and in the key field.
		{"bad base64 key", "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$!!!bad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			st := openTestStore(t)
			cfg := config.Config{SeedUser: "operator", SeedPasswordHash: tt.hash}
			if err := seedInitialUser(ctx, st, cfg, discardLogger()); err == nil {
				t.Fatal("seedInitialUser = nil; want a malformed-hash error")
			}
			if n, _ := st.CountUsers(ctx); n != 0 {
				t.Fatalf("CountUsers = %d; want 0 (no user on a malformed hash)", n)
			}
		})
	}
}

// TestSeedInitialUserUsernameTooLong: a 65-char username with an otherwise
// valid hash is rejected by the shared username validator, on an empty DB,
// before any user is created.
func TestSeedInitialUserUsernameTooLong(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	cfg := config.Config{
		SeedUser:         strings.Repeat("u", 65),
		SeedPasswordHash: httpapi.HashPassword("some password"),
	}
	if err := seedInitialUser(ctx, st, cfg, discardLogger()); err == nil {
		t.Fatal("seedInitialUser = nil; want a username-length error")
	}
	if n, _ := st.CountUsers(ctx); n != 0 {
		t.Fatalf("CountUsers = %d; want 0 (no user on a validation failure)", n)
	}
}
