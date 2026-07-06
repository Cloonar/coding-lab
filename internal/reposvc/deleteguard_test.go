package reposvc

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// guardEnv builds a Service with the M3 delete-guard seams wired to scriptable
// callbacks (no clone machinery — the guard is exercised on a ready repo row).
type guardEnv struct {
	svc       *Service
	st        *store.Store
	live      int
	stopCalls int
}

func newGuardEnv(t *testing.T) *guardEnv {
	t.Helper()
	st := testutil.TempStore(t)
	mat, err := vault.NewMaterializer(filepath.Join(t.TempDir(), "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.New(make([]byte, vault.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	g := &guardEnv{st: st}
	svc, err := New(Options{
		Store: st, Vault: v, Materializer: mat, Git: gitx.New("git"), Bus: events.NewBus(),
		Logger: logx.New(io.Discard), ReposDir: filepath.Join(t.TempDir(), "repos"),
		LiveInstances: func(context.Context, string) (int, error) { return g.live, nil },
		StopInstances: func(context.Context, string) (int, error) { g.stopCalls++; g.live = 0; return 1, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	g.svc = svc
	return g
}

func (g *guardEnv) readyRepo(t *testing.T) store.Repo {
	t.Helper()
	r, err := g.st.CreateRepo(context.Background(), store.Repo{
		ID: ids.NewID("repo"), Name: "proj", RemoteURL: "/tmp/x",
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		Provider: DefaultProvider, AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDelete_refusedWhileLiveInstances(t *testing.T) {
	g := newGuardEnv(t)
	repo := g.readyRepo(t)
	g.live = 2

	if err := g.svc.Delete(context.Background(), repo.ID, false); !errors.Is(err, ErrHasLiveInstances) {
		t.Fatalf("Delete err = %v, want ErrHasLiveInstances", err)
	}
	if _, err := g.st.RepoByID(context.Background(), repo.ID); err != nil {
		t.Errorf("repo was deleted despite live instances: %v", err)
	}
	if g.stopCalls != 0 {
		t.Errorf("unforced refusal must not tear down instances (StopInstances called %d times)", g.stopCalls)
	}
}

func TestDelete_forceTearsDownInstancesFirst(t *testing.T) {
	g := newGuardEnv(t)
	repo := g.readyRepo(t)
	g.live = 2

	if err := g.svc.Delete(context.Background(), repo.ID, true); err != nil {
		t.Fatalf("force Delete: %v", err)
	}
	if g.stopCalls != 1 {
		t.Errorf("force delete did not tear down instances first (StopInstances called %d times)", g.stopCalls)
	}
	if _, err := g.st.RepoByID(context.Background(), repo.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("repo row survived force delete: %v", err)
	}
}

func TestDelete_noGuardWhenNoLiveInstances(t *testing.T) {
	g := newGuardEnv(t)
	repo := g.readyRepo(t)
	g.live = 0 // no live instances → deletes without force, no teardown

	if err := g.svc.Delete(context.Background(), repo.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if g.stopCalls != 0 {
		t.Errorf("StopInstances called with no live instances")
	}
}
