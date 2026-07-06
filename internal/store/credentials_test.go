package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

func TestCredentialsCRUD(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		created := time.Date(2026, 7, 6, 10, 0, 0, 123_456_789, time.UTC)

		c, err := s.CreateCredential(ctx, ids.NewID("cred"), "deploy-key",
			CredentialKindSSHKey, []byte("nonce||ciphertext"), created)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if want := created.Truncate(time.Millisecond); !c.CreatedAt.Equal(want) || !c.UpdatedAt.Equal(want) {
			t.Errorf("timestamps = %v/%v, want both %v", c.CreatedAt, c.UpdatedAt, want)
		}

		// Duplicate name → typed error.
		if _, err := s.CreateCredential(ctx, ids.NewID("cred"), "deploy-key",
			CredentialKindHTTPSToken, []byte("x"), created); !errors.Is(err, ErrNameTaken) {
			t.Errorf("duplicate name error = %v, want ErrNameTaken", err)
		}

		// ByID returns the payload (server-internal read; the API never
		// serializes it).
		got, err := s.CredentialByID(ctx, c.ID)
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got.Name != "deploy-key" || got.Kind != CredentialKindSSHKey {
			t.Errorf("by id = %+v, want the created credential", got)
		}
		if !bytes.Equal(got.EncryptedPayload, []byte("nonce||ciphertext")) {
			t.Errorf("payload = %q, want the stored ciphertext", got.EncryptedPayload)
		}
		if !got.CreatedAt.Equal(c.CreatedAt) || !got.UpdatedAt.Equal(c.UpdatedAt) {
			t.Errorf("by id timestamps = %v/%v, want %v/%v", got.CreatedAt, got.UpdatedAt, c.CreatedAt, c.UpdatedAt)
		}

		if _, err := s.CredentialByID(ctx, "cred_00000000000000000000000000000000"); !errors.Is(err, ErrNotFound) {
			t.Errorf("by unknown id error = %v, want ErrNotFound", err)
		}

		// List: metadata only, referenced count 0.
		second, err := s.CreateCredential(ctx, ids.NewID("cred"), "api-token",
			CredentialKindForgeToken, []byte("other"), created)
		if err != nil {
			t.Fatalf("create second: %v", err)
		}
		metas, err := s.Credentials(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(metas) != 2 {
			t.Fatalf("list length = %d, want 2", len(metas))
		}
		// ORDER BY name: api-token before deploy-key.
		if metas[0].Name != "api-token" || metas[1].Name != "deploy-key" {
			t.Errorf("list order = [%s, %s], want [api-token, deploy-key]", metas[0].Name, metas[1].Name)
		}
		for _, m := range metas {
			if m.Referenced != 0 {
				t.Errorf("credential %s Referenced = %d, want 0", m.Name, m.Referenced)
			}
		}

		// Rename only: payload untouched, updated_at stamped.
		renamed := "deploy-key-2"
		later := created.Add(time.Hour)
		if err := s.UpdateCredential(ctx, c.ID, &renamed, nil, later); err != nil {
			t.Fatalf("rename: %v", err)
		}
		got, err = s.CredentialByID(ctx, c.ID)
		if err != nil {
			t.Fatalf("by id after rename: %v", err)
		}
		if got.Name != "deploy-key-2" {
			t.Errorf("name after rename = %q, want deploy-key-2", got.Name)
		}
		if !bytes.Equal(got.EncryptedPayload, []byte("nonce||ciphertext")) {
			t.Errorf("payload changed on rename: %q", got.EncryptedPayload)
		}
		if want := later.Truncate(time.Millisecond); !got.UpdatedAt.Equal(want) {
			t.Errorf("UpdatedAt after rename = %v, want %v", got.UpdatedAt, want)
		}
		if !got.CreatedAt.Equal(c.CreatedAt) {
			t.Errorf("CreatedAt changed on rename: %v", got.CreatedAt)
		}

		// Rotate only: name untouched, payload replaced (never read back —
		// the accessor takes only the new ciphertext).
		if err := s.UpdateCredential(ctx, c.ID, nil, []byte("rotated"), later.Add(time.Hour)); err != nil {
			t.Fatalf("rotate: %v", err)
		}
		got, err = s.CredentialByID(ctx, c.ID)
		if err != nil {
			t.Fatalf("by id after rotate: %v", err)
		}
		if got.Name != "deploy-key-2" {
			t.Errorf("name changed on rotate: %q", got.Name)
		}
		if !bytes.Equal(got.EncryptedPayload, []byte("rotated")) {
			t.Errorf("payload after rotate = %q, want rotated", got.EncryptedPayload)
		}

		// Rename onto an existing name → typed error.
		collide := "api-token"
		if err := s.UpdateCredential(ctx, c.ID, &collide, nil, later); !errors.Is(err, ErrNameTaken) {
			t.Errorf("rename collision error = %v, want ErrNameTaken", err)
		}

		// Update of a missing id → ErrNotFound.
		if err := s.UpdateCredential(ctx, "cred_00000000000000000000000000000000", &renamed, nil, later); !errors.Is(err, ErrNotFound) {
			t.Errorf("update missing error = %v, want ErrNotFound", err)
		}

		// Unreferenced delete succeeds; the row is gone.
		if err := s.DeleteCredential(ctx, second.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := s.CredentialByID(ctx, second.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("by id after delete error = %v, want ErrNotFound", err)
		}
		if err := s.DeleteCredential(ctx, second.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("second delete error = %v, want ErrNotFound", err)
		}
	})
}

func TestDeleteCredentialReferenced(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 6, 11, 0, 0, 0, time.UTC)

		gitCred, err := s.CreateCredential(ctx, ids.NewID("cred"), "git-key",
			CredentialKindSSHKey, []byte("a"), now)
		if err != nil {
			t.Fatalf("create git cred: %v", err)
		}
		forgeCred, err := s.CreateCredential(ctx, ids.NewID("cred"), "forge-token",
			CredentialKindForgeToken, []byte("b"), now)
		if err != nil {
			t.Fatalf("create forge cred: %v", err)
		}

		// Repo A references BOTH columns; repo B only the git credential.
		repoA := testRepo("alpha", now)
		repoA.CredentialID = &gitCred.ID
		repoA.ForgeCredentialID = &forgeCred.ID
		repoA.TrackerBinding = TrackerBindingForge
		if _, err := s.CreateRepo(ctx, repoA); err != nil {
			t.Fatalf("create repo A: %v", err)
		}
		repoB := testRepo("beta", now)
		repoB.CredentialID = &gitCred.ID
		if _, err := s.CreateRepo(ctx, repoB); err != nil {
			t.Fatalf("create repo B: %v", err)
		}

		// Deleting the git credential is refused with the repo count.
		err = s.DeleteCredential(ctx, gitCred.ID)
		if !errors.Is(err, ErrReferenced) {
			t.Fatalf("delete git cred error = %v, want ErrReferenced", err)
		}
		var refErr *ReferencedError
		if !errors.As(err, &refErr) {
			t.Fatalf("delete git cred error = %v, want *ReferencedError", err)
		}
		if refErr.Repos != 2 {
			t.Errorf("git cred Repos = %d, want 2", refErr.Repos)
		}
		if want := "credential is referenced by 2 repositories"; refErr.Error() != want {
			t.Errorf("message = %q, want %q", refErr.Error(), want)
		}

		// The forge FK column blocks deletion just like the git one.
		err = s.DeleteCredential(ctx, forgeCred.ID)
		if !errors.As(err, &refErr) {
			t.Fatalf("delete forge cred error = %v, want *ReferencedError", err)
		}
		if refErr.Repos != 1 {
			t.Errorf("forge cred Repos = %d, want 1", refErr.Repos)
		}
		if want := "credential is referenced by 1 repository"; refErr.Error() != want {
			t.Errorf("message = %q, want %q", refErr.Error(), want)
		}

		// Both rows survived the refusals.
		if _, err := s.CredentialByID(ctx, gitCred.ID); err != nil {
			t.Errorf("git cred gone after refused delete: %v", err)
		}

		// List counts match: a repo referencing via both columns counts once.
		metas, err := s.Credentials(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		counts := make(map[string]int, len(metas))
		for _, m := range metas {
			counts[m.Name] = m.Referenced
		}
		if counts["git-key"] != 2 || counts["forge-token"] != 1 {
			t.Errorf("referenced counts = %v, want git-key:2 forge-token:1", counts)
		}

		// Dropping the referencing repos unblocks the delete.
		if err := s.DeleteRepo(ctx, repoA.ID); err != nil {
			t.Fatalf("delete repo A: %v", err)
		}
		if err := s.DeleteRepo(ctx, repoB.ID); err != nil {
			t.Fatalf("delete repo B: %v", err)
		}
		if err := s.DeleteCredential(ctx, gitCred.ID); err != nil {
			t.Errorf("delete git cred after repos gone: %v", err)
		}
		if err := s.DeleteCredential(ctx, forgeCred.ID); err != nil {
			t.Errorf("delete forge cred after repos gone: %v", err)
		}
	})
}
