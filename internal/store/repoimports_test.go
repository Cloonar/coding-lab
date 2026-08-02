package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// TestRepoImports_roundTrip covers the add/list/remove lifecycle from both
// sides of the relation (RepoImports and RepoImporters), ordering by name,
// and mutual imports (A imports B and B imports A both legally present at
// once).
func TestRepoImports_roundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)

		a := testRepo("import-alpha", now)
		b := testRepo("import-beta", now)
		c := testRepo("import-charlie", now)
		for _, r := range []Repo{a, b, c} {
			if _, err := s.CreateRepo(ctx, r); err != nil {
				t.Fatalf("create repo %s: %v", r.Name, err)
			}
		}

		// a imports both c and b (inserted out of name order, to prove the
		// ORDER BY, not insertion order, drives the list).
		if err := s.AddRepoImport(ctx, a.ID, c.ID); err != nil {
			t.Fatalf("add import a->c: %v", err)
		}
		if err := s.AddRepoImport(ctx, a.ID, b.ID); err != nil {
			t.Fatalf("add import a->b: %v", err)
		}

		imports, err := s.RepoImports(ctx, a.ID)
		if err != nil {
			t.Fatalf("RepoImports: %v", err)
		}
		if len(imports) != 2 {
			t.Fatalf("RepoImports length = %d, want 2", len(imports))
		}
		if imports[0].Name != "import-beta" || imports[1].Name != "import-charlie" {
			t.Errorf("RepoImports order = [%s, %s], want [import-beta, import-charlie]", imports[0].Name, imports[1].Name)
		}

		// RepoImporters from the target side.
		importers, err := s.RepoImporters(ctx, b.ID)
		if err != nil {
			t.Fatalf("RepoImporters: %v", err)
		}
		if len(importers) != 1 || importers[0].Name != "import-alpha" {
			t.Errorf("RepoImporters(b) = %v, want [import-alpha]", importers)
		}

		// Mutual imports: b also imports a. Both directions present at once.
		if err := s.AddRepoImport(ctx, b.ID, a.ID); err != nil {
			t.Fatalf("add mutual import b->a: %v", err)
		}
		aImports, err := s.RepoImports(ctx, a.ID)
		if err != nil {
			t.Fatalf("RepoImports(a) after mutual: %v", err)
		}
		if len(aImports) != 2 {
			t.Errorf("RepoImports(a) after mutual length = %d, want 2", len(aImports))
		}
		bImports, err := s.RepoImports(ctx, b.ID)
		if err != nil {
			t.Fatalf("RepoImports(b) after mutual: %v", err)
		}
		if len(bImports) != 1 || bImports[0].Name != "import-alpha" {
			t.Errorf("RepoImports(b) after mutual = %v, want [import-alpha]", bImports)
		}

		// Remove one import, list again.
		if err := s.RemoveRepoImport(ctx, a.ID, c.ID); err != nil {
			t.Fatalf("remove import a->c: %v", err)
		}
		imports, err = s.RepoImports(ctx, a.ID)
		if err != nil {
			t.Fatalf("RepoImports after remove: %v", err)
		}
		if len(imports) != 1 || imports[0].Name != "import-beta" {
			t.Errorf("RepoImports(a) after remove = %v, want [import-beta]", imports)
		}
	})
}

// TestAddRepoImport_idempotent covers double add (no error, single row) and
// double remove (no error, absent pair).
func TestAddRepoImport_idempotent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)

		a := testRepo("idem-alpha", now)
		b := testRepo("idem-beta", now)
		for _, r := range []Repo{a, b} {
			if _, err := s.CreateRepo(ctx, r); err != nil {
				t.Fatalf("create repo %s: %v", r.Name, err)
			}
		}

		if err := s.AddRepoImport(ctx, a.ID, b.ID); err != nil {
			t.Fatalf("first add: %v", err)
		}
		if err := s.AddRepoImport(ctx, a.ID, b.ID); err != nil {
			t.Fatalf("second add (idempotent): %v", err)
		}
		if n := count(t, s, "repo_imports"); n != 1 {
			t.Errorf("repo_imports rows after double add = %d, want 1", n)
		}

		if err := s.RemoveRepoImport(ctx, a.ID, b.ID); err != nil {
			t.Fatalf("first remove: %v", err)
		}
		if err := s.RemoveRepoImport(ctx, a.ID, b.ID); err != nil {
			t.Fatalf("second remove (idempotent, absent pair): %v", err)
		}
		if n := count(t, s, "repo_imports"); n != 0 {
			t.Errorf("repo_imports rows after double remove = %d, want 0", n)
		}
	})
}

// TestAddRepoImport_selfRejected covers the store's defensive self-import
// guard: repoID == targetID never writes a row, even though the API layer is
// expected to reject it with a 400 before the store ever sees the call.
func TestAddRepoImport_selfRejected(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 20, 11, 0, 0, 0, time.UTC)

		a := testRepo("self-alpha", now)
		if _, err := s.CreateRepo(ctx, a); err != nil {
			t.Fatalf("create repo: %v", err)
		}

		if err := s.AddRepoImport(ctx, a.ID, a.ID); err == nil {
			t.Fatal("AddRepoImport(a, a) = nil error, want a rejection")
		}
		if n := count(t, s, "repo_imports"); n != 0 {
			t.Errorf("repo_imports rows after rejected self-import = %d, want 0", n)
		}
	})
}

// TestAddRepoImport_unknownRepo covers ErrNotFound on both an unknown
// importer and an unknown target.
func TestAddRepoImport_unknownRepo(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

		known := testRepo("unknown-known", now)
		if _, err := s.CreateRepo(ctx, known); err != nil {
			t.Fatalf("create repo: %v", err)
		}
		const missing = "repo_00000000000000000000000000000000"

		if err := s.AddRepoImport(ctx, missing, known.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown importer err = %v, want ErrNotFound", err)
		}
		if err := s.AddRepoImport(ctx, known.ID, missing); !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown target err = %v, want ErrNotFound", err)
		}
		if n := count(t, s, "repo_imports"); n != 0 {
			t.Errorf("repo_imports rows after rejected inserts = %d, want 0", n)
		}
	})
}

// TestRepoImports_cascadeOnImporterDelete mirrors
// TestRepoSecrets_cascadeOnRepoDelete: deleting the IMPORTING repo removes
// its repo_imports rows via ON DELETE CASCADE on repo_imports.repo_id.
func TestRepoImports_cascadeOnImporterDelete(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 20, 13, 0, 0, 0, time.UTC)

		importer := testRepo("cascade-importer", now)
		target := testRepo("cascade-target", now)
		for _, r := range []Repo{importer, target} {
			if _, err := s.CreateRepo(ctx, r); err != nil {
				t.Fatalf("create repo %s: %v", r.Name, err)
			}
		}
		if err := s.AddRepoImport(ctx, importer.ID, target.ID); err != nil {
			t.Fatalf("add import: %v", err)
		}
		if n := count(t, s, "repo_imports"); n != 1 {
			t.Fatalf("repo_imports rows = %d, want 1", n)
		}

		if err := s.DeleteRepo(ctx, importer.ID); err != nil {
			t.Fatalf("delete importer: %v", err)
		}
		if n := count(t, s, "repo_imports"); n != 0 {
			t.Errorf("repo_imports rows after importer delete = %d, want 0", n)
		}
	})
}

// TestDeleteRepo_importsGuard covers the delete refusal while a repo is
// still imported (errors.Is ErrHasImporters, errors.As recovers the
// importer names sorted) and success once every import is removed.
func TestDeleteRepo_importsGuard(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 20, 14, 0, 0, 0, time.UTC)

		target := testRepo("guard-target", now)
		importerZ := testRepo("guard-importer-z", now)
		importerA := testRepo("guard-importer-a", now)
		for _, r := range []Repo{target, importerZ, importerA} {
			if _, err := s.CreateRepo(ctx, r); err != nil {
				t.Fatalf("create repo %s: %v", r.Name, err)
			}
		}
		// Added out of name order, to prove the refusal names them sorted
		// rather than in insertion order.
		if err := s.AddRepoImport(ctx, importerZ.ID, target.ID); err != nil {
			t.Fatalf("add import z: %v", err)
		}
		if err := s.AddRepoImport(ctx, importerA.ID, target.ID); err != nil {
			t.Fatalf("add import a: %v", err)
		}

		err := s.DeleteRepo(ctx, target.ID)
		if !errors.Is(err, ErrHasImporters) {
			t.Fatalf("delete imported repo err = %v, want ErrHasImporters", err)
		}
		var impErr *ImportersError
		if !errors.As(err, &impErr) {
			t.Fatalf("errors.As(err, *ImportersError) failed for %v", err)
		}
		want := []string{"guard-importer-a", "guard-importer-z"}
		if !reflect.DeepEqual(impErr.Importers, want) {
			t.Errorf("ImportersError.Importers = %v, want %v", impErr.Importers, want)
		}

		// The target repo must survive the refused delete.
		if _, err := s.RepoByID(ctx, target.ID); err != nil {
			t.Errorf("RepoByID after refused delete: %v", err)
		}

		// Remove both imports; delete now succeeds.
		if err := s.RemoveRepoImport(ctx, importerZ.ID, target.ID); err != nil {
			t.Fatalf("remove import z: %v", err)
		}
		if err := s.RemoveRepoImport(ctx, importerA.ID, target.ID); err != nil {
			t.Fatalf("remove import a: %v", err)
		}
		if err := s.DeleteRepo(ctx, target.ID); err != nil {
			t.Fatalf("delete after removing imports: %v", err)
		}
	})
}
