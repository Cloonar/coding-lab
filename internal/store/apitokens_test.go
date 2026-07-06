package store

// PAT list/delete accessors (M5 tokens CRUD): user scoping, list order, and
// the delete-revokes-lookup property the API's deleted-PAT-401 rests on.

import (
	"context"
	"errors"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

func TestAPITokensByUserAndDelete(t *testing.T) {
	forEachBackend(t, func(t *testing.T, st *Store) {
		ctx := context.Background()

		alice, err := st.CreateUser(ctx, "alice", "phc")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		bob, err := st.CreateUser(ctx, "bob", "phc")
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		mint := func(userID, name string) (APIToken, string) {
			t.Helper()
			_, hash := ids.NewToken("pat")
			tok, err := st.CreateAPIToken(ctx, userID, name, hash)
			if err != nil {
				t.Fatalf("CreateAPIToken(%s): %v", name, err)
			}
			return tok, hash
		}

		first, _ := mint(alice.ID, "first")
		st.Now = func() time.Time { return time.Now().Add(time.Minute) } // distinct created_at
		second, _ := mint(alice.ID, "second")
		other, otherHash := mint(bob.ID, "bobs")

		// List is user-scoped, newest first.
		toks, err := st.APITokensByUser(ctx, alice.ID)
		if err != nil {
			t.Fatalf("APITokensByUser: %v", err)
		}
		if len(toks) != 2 || toks[0].ID != second.ID || toks[1].ID != first.ID {
			t.Fatalf("alice tokens = %+v, want [second first]", toks)
		}

		// Another user's token is invisible to delete (no cross-user
		// revocation).
		if err := st.DeleteAPIToken(ctx, alice.ID, other.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-user delete err = %v, want ErrNotFound", err)
		}
		if _, err := st.APITokenByHash(ctx, otherHash); err != nil {
			t.Fatalf("bob's token vanished after alice's failed delete: %v", err)
		}

		// Own delete removes the row; the hash lookup (the auth path) misses.
		if err := st.DeleteAPIToken(ctx, alice.ID, first.ID); err != nil {
			t.Fatalf("DeleteAPIToken: %v", err)
		}
		toks, err = st.APITokensByUser(ctx, alice.ID)
		if err != nil {
			t.Fatalf("APITokensByUser: %v", err)
		}
		if len(toks) != 1 || toks[0].ID != second.ID {
			t.Fatalf("alice tokens after delete = %+v, want [second]", toks)
		}
		if err := st.DeleteAPIToken(ctx, alice.ID, first.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("double delete err = %v, want ErrNotFound", err)
		}
	})
}
