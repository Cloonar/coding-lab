package reposvc

// Tests for the read-only imports service surface (issue #261 / ADR-0063):
// Imports/AddImport/RemoveImport, plus the interaction between the store's
// delete-time importers guard and Delete's force flag. Uses newTestEnv (the
// real store via testutil.TempStore) and creates the extra repos it needs
// directly through the store, the same idiom deleteguard_test.go's readyRepo
// uses — these tests never touch clone machinery, so a bare CreateRepo row
// in clone_status "ready" is enough.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// readyRepoNamed is deleteguard_test.go's readyRepo idiom, parameterized
// over the name so ordering (RepoImports sorts by name) can be tested.
func (e *testEnv) readyRepoNamed(t *testing.T, name string) store.Repo {
	t.Helper()
	r, err := e.st.CreateRepo(context.Background(), store.Repo{
		ID: ids.NewID("repo"), Name: name, RemoteURL: "/tmp/" + name,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestImportsAddListRemoveRoundTrip(t *testing.T) {
	e := newTestEnv(t)
	log := collectEvents(t, e.bus)
	consumer := e.readyRepoNamed(t, "consumer")
	zeta := e.readyRepoNamed(t, "zeta")
	alpha := e.readyRepoNamed(t, "alpha")

	if got, err := e.svc.Imports(t.Context(), consumer.ID); err != nil || len(got) != 0 {
		t.Fatalf("Imports before any Add = %v, %v, want empty, nil", got, err)
	}

	if target, err := e.svc.AddImport(t.Context(), consumer.ID, zeta.ID); err != nil || target.ID != zeta.ID {
		t.Fatalf("AddImport(zeta) = %v, %v, want %s, nil", target, err, zeta.ID)
	}
	if target, err := e.svc.AddImport(t.Context(), consumer.ID, alpha.ID); err != nil || target.ID != alpha.ID {
		t.Fatalf("AddImport(alpha) = %v, %v, want %s, nil", target, err, alpha.ID)
	}

	got, err := e.svc.Imports(t.Context(), consumer.ID)
	if err != nil {
		t.Fatalf("Imports: %v", err)
	}
	if len(got) != 2 || got[0].ID != alpha.ID || got[1].ID != zeta.ID {
		t.Fatalf("Imports = %v, want [alpha, zeta] ordered by name", got)
	}

	// Re-adding an existing pair is idempotent.
	if _, err := e.svc.AddImport(t.Context(), consumer.ID, alpha.ID); err != nil {
		t.Fatalf("re-add AddImport(alpha): %v", err)
	}
	if got, err := e.svc.Imports(t.Context(), consumer.ID); err != nil || len(got) != 2 {
		t.Fatalf("Imports after re-add = %v, %v, want still 2", got, err)
	}

	if err := e.svc.RemoveImport(t.Context(), consumer.ID, alpha.ID); err != nil {
		t.Fatalf("RemoveImport(alpha): %v", err)
	}
	got, err = e.svc.Imports(t.Context(), consumer.ID)
	if err != nil || len(got) != 1 || got[0].ID != zeta.ID {
		t.Fatalf("Imports after remove = %v, %v, want [zeta]", got, err)
	}

	// Removing an absent pair is not an error (idempotent).
	if err := e.svc.RemoveImport(t.Context(), consumer.ID, alpha.ID); err != nil {
		t.Fatalf("re-remove RemoveImport(alpha): %v", err)
	}

	// repo.changed published on every Add and Remove call (3 adds + 2
	// removes above). collectEvents drains the bus on its own goroutine, so
	// poll briefly rather than racing its next read.
	const wantChanged = 5
	deadline := time.Now().Add(2 * time.Second)
	var changed int
	for {
		changed = 0
		for _, ev := range log.snapshot() {
			if ev.Type != EventRepoChanged {
				continue
			}
			p, ok := ev.Payload.(repoChangedPayload)
			if !ok || p.RepoID != consumer.ID {
				t.Fatalf("bad repo.changed payload: %#v", ev.Payload)
			}
			changed++
		}
		if changed >= wantChanged || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if changed != wantChanged {
		t.Errorf("saw %d repo.changed events for consumer, want %d (3 adds + 2 removes)", changed, wantChanged)
	}
}

func TestImportsAddSelfImportRejected(t *testing.T) {
	e := newTestEnv(t)
	repo := e.readyRepoNamed(t, "solo")

	_, err := e.svc.AddImport(t.Context(), repo.ID, repo.ID)
	var bad *BadRequestError
	if err == nil || !asBadRequest(err, &bad) {
		t.Fatalf("AddImport(self) error = %v, want BadRequestError", err)
	}
	if !strings.Contains(bad.Error(), "cannot import itself") {
		t.Errorf("error = %q, want it to mention 'cannot import itself'", bad.Error())
	}
}

func TestImportsAddUnknownTargetRejected(t *testing.T) {
	e := newTestEnv(t)
	repo := e.readyRepoNamed(t, "solo")
	unknown := "repo_00000000000000000000000000000000"

	_, err := e.svc.AddImport(t.Context(), repo.ID, unknown)
	var bad *BadRequestError
	if err == nil || !asBadRequest(err, &bad) {
		t.Fatalf("AddImport(unknown target) error = %v, want BadRequestError", err)
	}
	if !strings.Contains(bad.Error(), "unknown target") {
		t.Errorf("error = %q, want it to mention 'unknown target'", bad.Error())
	}
}

func TestImportsUnknownRepoNotFound(t *testing.T) {
	e := newTestEnv(t)
	target := e.readyRepoNamed(t, "target")
	unknown := "repo_00000000000000000000000000000000"

	if _, err := e.svc.Imports(t.Context(), unknown); !isErr(err, store.ErrNotFound) {
		t.Errorf("Imports(unknown repo) err = %v, want ErrNotFound", err)
	}
	if _, err := e.svc.AddImport(t.Context(), unknown, target.ID); !isErr(err, store.ErrNotFound) {
		t.Errorf("AddImport(unknown repo) err = %v, want ErrNotFound", err)
	}
	if err := e.svc.RemoveImport(t.Context(), unknown, target.ID); !isErr(err, store.ErrNotFound) {
		t.Errorf("RemoveImport(unknown repo) err = %v, want ErrNotFound", err)
	}
}

func TestImportsMutualImportsLegal(t *testing.T) {
	e := newTestEnv(t)
	server := e.readyRepoNamed(t, "server")
	client := e.readyRepoNamed(t, "client")

	if _, err := e.svc.AddImport(t.Context(), server.ID, client.ID); err != nil {
		t.Fatalf("server imports client: %v", err)
	}
	if _, err := e.svc.AddImport(t.Context(), client.ID, server.ID); err != nil {
		t.Fatalf("client imports server: %v", err)
	}

	serverImports, err := e.svc.Imports(t.Context(), server.ID)
	if err != nil || len(serverImports) != 1 || serverImports[0].ID != client.ID {
		t.Errorf("server's imports = %v, %v, want [client]", serverImports, err)
	}
	clientImports, err := e.svc.Imports(t.Context(), client.ID)
	if err != nil || len(clientImports) != 1 || clientImports[0].ID != server.ID {
		t.Errorf("client's imports = %v, %v, want [server]", clientImports, err)
	}
}

// TestDelete_refusedWhileImportersEvenWithForce covers ADR-0063's pinned
// decision that force does NOT bypass the importers guard (unlike the
// clone-in-progress and live-instances guards in deleteguard_test.go): the
// guard protects another repo's declared world, not lab's own recoverable
// state, and it lives in the store — below the layer force is interpreted
// at — so there is nothing for reposvc.Delete to thread force through even
// if it wanted to.
func TestDelete_refusedWhileImportersEvenWithForce(t *testing.T) {
	e := newTestEnv(t)
	target := e.readyRepoNamed(t, "target")
	consumer := e.readyRepoNamed(t, "consumer")
	if _, err := e.svc.AddImport(t.Context(), consumer.ID, target.ID); err != nil {
		t.Fatalf("AddImport: %v", err)
	}

	for _, force := range []bool{false, true} {
		err := e.svc.Delete(t.Context(), target.ID, force)
		if !errors.Is(err, store.ErrHasImporters) {
			t.Fatalf("Delete(force=%v) err = %v, want ErrHasImporters", force, err)
		}
		var impErr *store.ImportersError
		if !errors.As(err, &impErr) || len(impErr.Importers) != 1 || impErr.Importers[0] != consumer.Name {
			t.Errorf("Delete(force=%v) ImportersError = %#v, want Importers = [%q]", force, impErr, consumer.Name)
		}
	}
	if _, err := e.st.RepoByID(t.Context(), target.ID); err != nil {
		t.Errorf("target repo was deleted despite an importer: %v", err)
	}

	// Removing the import declaration clears the guard.
	if err := e.svc.RemoveImport(t.Context(), consumer.ID, target.ID); err != nil {
		t.Fatalf("RemoveImport: %v", err)
	}
	if err := e.svc.Delete(t.Context(), target.ID, false); err != nil {
		t.Fatalf("Delete after RemoveImport: %v", err)
	}
	if _, err := e.st.RepoByID(t.Context(), target.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("target repo row survived delete: %v", err)
	}
}
