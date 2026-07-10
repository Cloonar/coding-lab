package reposvc

// Pre-push guard wiring (issue #106 + D15 §9 measure 7): reposvc installs
// the guard on EVERY repo at clone completion — the secret leak scan is
// unconditional — and the incogni flag only decides the hook's CONTENT:
// toggling re-renders the attribution/seeded-path patterns in or out, never
// removes the hook, and core.hooksPath stays pinned throughout.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/seeder"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// readHook returns bare's pre-push hook body, failing t when unreadable.
func readHook(t *testing.T, bare string) string {
	t.Helper()
	body, err := os.ReadFile(seeder.PrePushHookPath(bare))
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	return string(body)
}

// hooksPath reads bare's LOCAL core.hooksPath; "" when unset.
func hooksPath(t *testing.T, home, bare string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", bare, "config", "--local", "--get", "core.hooksPath")
	cmd.Env = append(os.Environ(), testutil.HermeticGitEnv(home)...)
	out, _ := cmd.Output() // exit 1 when unset → ""
	return strings.TrimSpace(string(out))
}

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
	// The guard is two-part: the secret leak scan on every repo (issue #106)
	// plus the incogni scans, whose scrub patterns are the union of every
	// registered provider's declared patterns (ADR-0033, issue #75); with a
	// single registered provider (this env's default) the union degenerates
	// to exactly that provider's SeedMeta.
	body := readHook(t, bare)
	if !strings.Contains(body, "labctl secret scan") {
		t.Error("installed hook missing the secret leak scan (issue #106)")
	}
	if !strings.Contains(body, `co-authored-by:[[:space:]]*claude`) {
		t.Error("installed hook missing the provider-declared scrub pattern; reposvc→provider→hook wiring broken")
	}

	// Toggle-off re-renders the hook WITHOUT the incogni patterns: the hook
	// stays (the secret scan guards every repo), the patterns go, the pin
	// stays, and the flag flip persists.
	updated, err := e.svc.UpdateSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(false),
	})
	if err != nil {
		t.Fatalf("UpdateSettings incogni=false: %v", err)
	}
	if updated.Incogni {
		t.Error("incogni still true after toggle-off")
	}
	if !seeder.PrePushHookInstalled(bare) {
		t.Fatal("pre-push hook gone after incogni toggle-off; the secret-scan guard must stay on every repo")
	}
	body = readHook(t, bare)
	if !strings.Contains(body, "labctl secret scan") {
		t.Error("secret leak scan missing after incogni toggle-off")
	}
	if strings.Contains(body, "co-authored-by") {
		t.Error("incogni scrub patterns survived toggle-off")
	}
	if got := hooksPath(t, e.home, bare); got == "" {
		t.Error("core.hooksPath unpinned by incogni toggle-off; the pin protects the secret scan on every repo")
	}

	// Toggle-on renders the patterns back in.
	updated, err = e.svc.UpdateSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(true),
	})
	if err != nil {
		t.Fatalf("UpdateSettings incogni=true: %v", err)
	}
	if !updated.Incogni {
		t.Error("incogni still false after toggle-on")
	}
	if !strings.Contains(readHook(t, bare), `co-authored-by:[[:space:]]*claude`) {
		t.Error("incogni scrub patterns missing after toggle-on")
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

// A non-incogni add is guarded too (issue #106: secrets are per-repo,
// orthogonal to incogni): its hook carries the secret leak scan and none of
// the incogni patterns. And toggling incogni either way while the repo has
// no bare dir (clone failed) neither errors nor strands state: the hook
// arrives with the retried clone's completion, content per the re-read row.
func TestIncogniPrePushHookNonIncogniAndMissingBareDir(t *testing.T) {
	e := newTestEnv(t)
	origin := makeOrigin(t, e.home, "main", 1)

	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "file://" + origin})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	e.waitCloneStatus(t, repo.ID, store.CloneStatusReady)
	bare := e.svc.bareDir(repo.ID)
	if !seeder.PrePushHookInstalled(bare) {
		t.Fatal("pre-push hook missing on a non-incogni repo; the secret-scan guard must land with every clone")
	}
	body := readHook(t, bare)
	if !strings.Contains(body, "labctl secret scan") {
		t.Error("non-incogni hook missing the secret leak scan")
	}
	if strings.Contains(body, "co-authored-by") {
		t.Error("non-incogni hook carries incogni scrub patterns")
	}
	if got := hooksPath(t, e.home, bare); got == "" {
		t.Error("core.hooksPath not pinned on a non-incogni repo")
	}

	// A repo whose clone failed has no bare dir: the toggle-on PATCH must
	// still succeed (skip the install — clone completion will do it) ...
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

	// ... and so must the toggle-off PATCH (skip the re-render silently).
	updated, err = e.svc.UpdateSettings(t.Context(), broken.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(false),
	})
	if err != nil {
		t.Fatalf("UpdateSettings toggle-off on repo without bare dir: %v", err)
	}
	if updated.Incogni {
		t.Error("incogni not cleared on repo without bare dir")
	}
	if _, err := os.Stat(e.svc.bareDir(broken.ID)); !os.IsNotExist(err) {
		t.Errorf("toggling incogni on a repo without a bare dir left something behind: %v", err)
	}

	// The retried clone completes the deferred install: re-point the row's
	// remote at a real origin is not possible via UpdateSettings, so verify
	// via the incogni Add path instead — Retry over a now-working remote is
	// covered by TestRetryFlow; here the contract is only "PATCH must not
	// fail without a bare dir".
}

// Regression (M7 review): install pins the bare repo's LOCAL core.hooksPath
// to the absolute hooks dir so a global/system core.hooksPath (husky &c.)
// cannot route ANY agent push past the guard — with the secret scan on every
// repo (issue #106) the pin is permanent, never undone by an incogni toggle.
// StartupHeal's reconcile converges the guard on every ready repo: the hook
// exists unconditionally with content matching the incogni flag (a crash
// between a toggle and its hook re-render leaves the content out of sync; a
// repo cloned by an older lab lacks the hook entirely).
func TestIncogniHookPinsHooksPathAndReconciles(t *testing.T) {
	e := newTestEnv(t)
	origin := makeOrigin(t, e.home, "main", 1)
	repo, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "file://" + origin, Incogni: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	e.waitCloneStatus(t, repo.ID, store.CloneStatusReady)
	bare := e.svc.bareDir(repo.ID)

	if got := hooksPath(t, e.home, bare); got == "" {
		t.Error("core.hooksPath not pinned after guard install")
	} else if !filepath.IsAbs(got) {
		t.Errorf("core.hooksPath = %q, want an absolute path", got)
	}

	// Simulate a crash that flipped the row to non-incogni but never
	// re-rendered the hook: reconcile keeps the hook, strips the patterns,
	// keeps the pin — never removes.
	if _, err := e.st.UpdateRepoSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(false),
	}); err != nil {
		t.Fatalf("direct row update: %v", err)
	}
	e.svc.reconcileGuardHooks(t.Context())
	if !seeder.PrePushHookInstalled(bare) {
		t.Fatal("guard removed by reconciliation; it must exist on every ready repo")
	}
	body := readHook(t, bare)
	if !strings.Contains(body, "labctl secret scan") {
		t.Error("secret leak scan missing after reconcile with the flag false")
	}
	if strings.Contains(body, "co-authored-by") {
		t.Error("incogni patterns survived reconcile with the flag false")
	}
	if got := hooksPath(t, e.home, bare); got == "" {
		t.Error("core.hooksPath unpinned by reconcile; the pin stays on every repo")
	}

	// A ready non-incogni repo with NO hook at all (cloned by an older lab):
	// reconcile installs the secret-scan-only guard — this pass is also how
	// hook-content changes between lab versions roll out.
	if err := os.Remove(seeder.PrePushHookPath(bare)); err != nil {
		t.Fatalf("remove hook: %v", err)
	}
	e.svc.reconcileGuardHooks(t.Context())
	if !seeder.PrePushHookInstalled(bare) {
		t.Fatal("reconcile did not install the guard on a ready non-incogni repo without one")
	}
	if body := readHook(t, bare); !strings.Contains(body, "labctl secret scan") {
		t.Error("reconciled hook missing the secret leak scan")
	} else if strings.Contains(body, "co-authored-by") {
		t.Error("reconciled non-incogni hook carries incogni patterns")
	}

	// And the mirror: flag on, patterns missing → reconcile renders them in.
	if _, err := e.st.UpdateRepoSettings(t.Context(), repo.ID, store.RepoSettingsUpdate{
		Incogni: store.Set(true),
	}); err != nil {
		t.Fatalf("direct row update: %v", err)
	}
	e.svc.reconcileGuardHooks(t.Context())
	if !strings.Contains(readHook(t, bare), `co-authored-by:[[:space:]]*claude`) {
		t.Error("incogni patterns not rendered by reconciliation after the flag went true")
	}
}

// A ready repo carrying a FOREIGN pre-push hook (no lab marker): reconcile
// must not overwrite it — seeder.InstallPrePushHook refuses, reconcile logs
// and continues, and the remaining repos still converge.
func TestReconcileGuardHooksForeignHookSkipped(t *testing.T) {
	e := newTestEnv(t)
	origin := makeOrigin(t, e.home, "main", 1)

	// First repo (reconcile meets it first): its lab hook is replaced by a
	// user's own pre-push hook after the clone.
	foreign, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "file://" + origin, Name: "foreign-hook"})
	if err != nil {
		t.Fatalf("Add foreign-hook repo: %v", err)
	}
	e.waitCloneStatus(t, foreign.ID, store.CloneStatusReady)
	foreignHookPath := seeder.PrePushHookPath(e.svc.bareDir(foreign.ID))
	const userHook = "#!/bin/sh\n# the user's own hook\nexit 0\n"
	if err := os.WriteFile(foreignHookPath, []byte(userHook), 0o755); err != nil {
		t.Fatalf("write foreign hook: %v", err)
	}

	// Second ready repo missing its guard: reconcile must reach it despite
	// the foreign-hook failure on the first.
	other, err := e.svc.Add(t.Context(), AddParams{RemoteURL: "file://" + origin, Name: "unguarded"})
	if err != nil {
		t.Fatalf("Add unguarded repo: %v", err)
	}
	e.waitCloneStatus(t, other.ID, store.CloneStatusReady)
	otherBare := e.svc.bareDir(other.ID)
	if err := os.Remove(seeder.PrePushHookPath(otherBare)); err != nil {
		t.Fatalf("remove hook: %v", err)
	}

	e.svc.reconcileGuardHooks(t.Context())

	if body, err := os.ReadFile(foreignHookPath); err != nil || string(body) != userHook {
		t.Errorf("foreign pre-push hook not left byte-unchanged by reconcile: %q, %v", body, err)
	}
	if !seeder.PrePushHookInstalled(otherBare) {
		t.Error("reconcile stopped at the foreign-hook failure; later repos left unguarded")
	}
}

// TestIncogniHookUnionsAllRegisteredProviders pins the issue #75 / ADR-0033
// acceptance line: the guard's scrub AND seeded-path patterns are the UNION
// of every registered provider's declared patterns, never just the resolving
// repo's own effective provider. ADR-0030 made the agent CLI a per-session
// override — any registered provider can push through any repo's guard in a
// given session — so a guard keyed to one provider is exactly the race
// ADR-0033 closes. The repo's own provider is pinned to codex-fake, and
// claude-code's patterns must still show up in the installed hook.
func TestIncogniHookUnionsAllRegisteredProviders(t *testing.T) {
	claude := providertest.New() // default claude-shaped SeedMeta

	codex := providertest.New()
	codex.SetID("codex-fake")
	codex.SetSeedMeta(provider.SeedMeta{
		ContextFileName: "AGENTS.local.md",
		SkillsDir:       ".codex/skills",
		ExcludeEntries:  []string{".codex/", "AGENTS.local.md"},
		SeededPathPatterns: []string{
			`^\.codex/skills/`,
		},
		ScrubPatterns: []string{
			`co-authored-by:.*<[^>]*@openai\.com>`,
			`codex-session:`,
		},
	})

	e := newTestEnvProviders(t, claude, codex)
	origin := makeOrigin(t, e.home, "main", 1)

	// The repo's own EFFECTIVE provider is explicitly codex-fake — the exact
	// setup the old repo-provider-resolved hook would have gotten wrong: it
	// would have rendered ONLY codex's patterns. The union must still carry
	// claude-code's, because a per-session override (ADR-0030) could run
	// claude-code against this same repo.
	repo, err := e.svc.Add(t.Context(), AddParams{
		RemoteURL: "file://" + origin,
		Incogni:   true,
		Provider:  ptr("codex-fake"),
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if repo.Provider == nil || *repo.Provider != "codex-fake" {
		t.Fatalf("repo.Provider = %v, want codex-fake — the union premise requires the repo's OWN provider differ from claude-code", repo.Provider)
	}
	e.waitCloneStatus(t, repo.ID, store.CloneStatusReady)

	bare := e.svc.bareDir(repo.ID)
	hook := readHook(t, bare)

	for _, want := range []string{
		// claude-code's declared scrub patterns (providertest.New defaults) —
		// present even though the repo's own provider is codex-fake.
		`co-authored-by:[[:space:]]*claude`,
		`co-authored-by:.*<[^>]*@anthropic\.com>`,
		`generated with.*claude`,
		`claude-session:`,
		// codex-fake's declared scrub patterns.
		`co-authored-by:.*<[^>]*@openai\.com>`,
		`codex-session:`,
	} {
		if !strings.Contains(hook, want) {
			t.Errorf("installed hook missing scrub pattern %q; union of all registered providers not applied", want)
		}
	}
	for _, want := range []string{
		// claude-code's declared seeded-path patterns.
		`^\.claude/skills/`,
		`^\.claude/settings\.local\.json$`,
		`^CLAUDE\.local\.md$`,
		// codex-fake's declared seeded-path pattern.
		`^\.codex/skills/`,
	} {
		if !strings.Contains(hook, want) {
			t.Errorf("installed hook missing seeded-path pattern %q; union of all registered providers not applied", want)
		}
	}
}
