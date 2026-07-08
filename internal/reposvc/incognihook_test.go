package reposvc

// Incogni pre-push guard wiring (D15 §9 measure 7): installed by reposvc
// when the incogni flag turns on — repo add (via clone completion) and
// PATCH toggle-on — and removed on toggle-off.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

func TestIncogniPrePushHookAddAndToggle(t *testing.T) {
	e := newTestEnv(t)
	origin := makeOrigin(t, e.home, "main", 1)

	// Add with incogni=true: the guard lands with the clone (the bare dir
	// is born at clone completion — that IS "installed at repo add").
	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "file://" + origin, Incogni: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	e.waitCloneStatus(t, repo.ID, store.CloneStatusReady)
	bare := e.svc.bareDir(repo.ID)
	if !seeder.PrePushHookInstalled(bare) {
		t.Fatal("pre-push hook missing after an incogni repo's clone completed")
	}
	if fi, err := os.Stat(seeder.PrePushHookPath(bare)); err != nil || fi.Mode()&0o111 == 0 {
		t.Fatalf("hook not executable: %v %v", fi, err)
	}
	// The guard's scrub patterns came from the repo's provider (issue #51
	// decision 8): reposvc resolved repos.provider → SeedMeta → hook script.
	if body, err := os.ReadFile(seeder.PrePushHookPath(bare)); err != nil {
		t.Fatalf("read hook: %v", err)
	} else if !strings.Contains(string(body), `co-authored-by:[[:space:]]*claude`) {
		t.Error("installed hook missing the provider-declared scrub pattern; reposvc→provider→hook wiring broken")
	}

	// Toggle-off removes it; the flag flip persists.
	updated, err := e.svc.UpdateSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(false),
	})
	if err != nil {
		t.Fatalf("UpdateSettings incogni=false: %v", err)
	}
	if updated.Incogni {
		t.Error("incogni still true after toggle-off")
	}
	if seeder.PrePushHookInstalled(bare) {
		t.Error("pre-push hook still installed after incogni toggle-off")
	}

	// Toggle-on reinstalls it.
	updated, err = e.svc.UpdateSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(true),
	})
	if err != nil {
		t.Fatalf("UpdateSettings incogni=true: %v", err)
	}
	if !updated.Incogni {
		t.Error("incogni still false after toggle-on")
	}
	if !seeder.PrePushHookInstalled(bare) {
		t.Error("pre-push hook missing after incogni toggle-on")
	}

	// Toggle-on again: idempotent over lab's own hook.
	if _, err := e.svc.UpdateSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(true),
	}); err != nil {
		t.Fatalf("UpdateSettings repeat incogni=true: %v", err)
	}
	if !seeder.PrePushHookInstalled(bare) {
		t.Error("pre-push hook missing after repeated toggle-on")
	}
}

// A non-incogni add stays unguarded, and toggling incogni while the repo
// has no bare dir (clone failed) neither errors nor strands state: the
// hook arrives with the retried clone's completion.
func TestIncogniPrePushHookNonIncogniAndMissingBareDir(t *testing.T) {
	e := newTestEnv(t)
	origin := makeOrigin(t, e.home, "main", 1)

	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "file://" + origin})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	e.waitCloneStatus(t, repo.ID, store.CloneStatusReady)
	if seeder.PrePushHookInstalled(e.svc.bareDir(repo.ID)) {
		t.Error("pre-push hook installed on a non-incogni repo")
	}

	// A repo whose clone failed has no bare dir: the toggle-on PATCH must
	// still succeed (skip the install — clone completion will do it).
	broken, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "file:///nonexistent/nowhere.git"})
	if err != nil {
		t.Fatalf("Add broken: %v", err)
	}
	e.waitCloneStatus(t, broken.ID, store.CloneStatusError)
	updated, err := e.svc.UpdateSettings(t.Context(), broken.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(true),
	})
	if err != nil {
		t.Fatalf("UpdateSettings on repo without bare dir: %v", err)
	}
	if !updated.Incogni {
		t.Error("incogni not set on repo without bare dir")
	}

	// The retried clone completes the deferred install: re-point the row's
	// remote at a real origin is not possible via UpdateSettings, so verify
	// via the incogni Add path instead — Retry over a now-working remote is
	// covered by TestRetryFlow; here the contract is only "PATCH must not
	// fail without a bare dir".
}

// Regression (M7 review): install pins the bare repo's LOCAL core.hooksPath
// to the absolute hooks dir so a global/system core.hooksPath (husky &c.)
// cannot route agent pushes past the guard; toggle-off unpins it; and
// StartupHeal reconciles the guard against the incogni flag for every ready
// repo (a crash between a toggle and its hook op leaves them out of sync).
func TestIncogniHookPinsHooksPathAndReconciles(t *testing.T) {
	e := newTestEnv(t)
	origin := makeOrigin(t, e.home, "main", 1)
	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "file://" + origin, Incogni: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	e.waitCloneStatus(t, repo.ID, store.CloneStatusReady)
	bare := e.svc.bareDir(repo.ID)

	hooksPath := func() string {
		cmd := exec.Command("git", "-C", bare, "config", "--local", "--get", "core.hooksPath")
		cmd.Env = append(os.Environ(), testutil.HermeticGitEnv(e.home)...)
		out, _ := cmd.Output() // exit 1 when unset → ""
		return strings.TrimSpace(string(out))
	}
	if got := hooksPath(); got == "" {
		t.Error("core.hooksPath not pinned after incogni install")
	} else if !filepath.IsAbs(got) {
		t.Errorf("core.hooksPath = %q, want an absolute path", got)
	}

	// Simulate a crash that flipped the row to non-incogni but never removed
	// the guard: reconcile at startup must remove it.
	if _, err := e.st.UpdateRepoSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(false),
	}); err != nil {
		t.Fatalf("direct row update: %v", err)
	}
	e.svc.reconcileIncogniHooks(t.Context())
	if seeder.PrePushHookInstalled(bare) {
		t.Error("guard survived reconciliation after the flag went false")
	}
	if got := hooksPath(); got != "" {
		t.Errorf("core.hooksPath still pinned after reconcile removal: %q", got)
	}

	// And the mirror: flag on, guard missing → reconcile installs it.
	if _, err := e.st.UpdateRepoSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(true),
	}); err != nil {
		t.Fatalf("direct row update: %v", err)
	}
	e.svc.reconcileIncogniHooks(t.Context())
	if !seeder.PrePushHookInstalled(bare) {
		t.Error("guard not installed by reconciliation after the flag went true")
	}
}
