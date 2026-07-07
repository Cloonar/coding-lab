package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

// testRepo builds a minimal valid Repo the way the API layer would: every
// NOT NULL column set, defaults applied by the caller (design: CreateRepo
// persists what it is given).
func testRepo(name string, createdAt time.Time) Repo {
	return Repo{
		ID:                 ids.NewID("repo"),
		Name:               name,
		RemoteURL:          "forgejo@git.cloonar.com:Cloonar/" + name + ".git",
		TrackerBinding:     TrackerBindingBuiltin,
		ForgeKind:          "forgejo",
		DefaultBranch:      "main",
		Provider:           "claude-code",
		AFKBranchPattern:   "afk/<N>",
		ManualBranchPrefix: "lab/",
		CloneStatus:        CloneStatusCloning,
		CreatedAt:          createdAt,
	}
}

func strPtr(s string) *string { return &s }
func intPtr(n int) *int       { return &n }

func TestCreateRepoRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 6, 9, 0, 0, 987_654_321, time.UTC)

		full := testRepo("gamma", now)
		full.CredentialID = nil // set below via credentials
		full.ModelDefault = strPtr("opus[1m]")
		full.EffortDefault = strPtr("max")
		full.Incogni = true
		full.GitAuthorName = strPtr("Dominik Polakovics")
		full.GitAuthorEmail = strPtr("dominik.polakovics@cloonar.com")
		full.AFKBranchPattern = "issue-<N>"
		full.ManualBranchPrefix = "wip/"
		full.AFKAutoEnabled = true
		full.BudgetMinutes = intPtr(90)
		full.MaxInstancesOverride = intPtr(2)

		created, err := s.CreateRepo(ctx, full)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if want := now.Truncate(time.Millisecond); !created.CreatedAt.Equal(want) {
			t.Errorf("CreatedAt = %v, want %v", created.CreatedAt, want)
		}

		got, err := s.RepoByID(ctx, full.ID)
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if !reflect.DeepEqual(got, created) {
			t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, created)
		}

		// Nullable columns stay nil for a minimal repo.
		minimal := testRepo("delta", now)
		if _, err := s.CreateRepo(ctx, minimal); err != nil {
			t.Fatalf("create minimal: %v", err)
		}
		gotMin, err := s.RepoByID(ctx, minimal.ID)
		if err != nil {
			t.Fatalf("by id minimal: %v", err)
		}
		if gotMin.CredentialID != nil || gotMin.ForgeCredentialID != nil ||
			gotMin.ModelDefault != nil || gotMin.EffortDefault != nil ||
			gotMin.GitAuthorName != nil || gotMin.GitAuthorEmail != nil ||
			gotMin.BudgetMinutes != nil || gotMin.MaxInstancesOverride != nil ||
			gotMin.CloneError != nil || gotMin.LastOpenedAt != nil {
			t.Errorf("minimal repo has non-nil optionals: %+v", gotMin)
		}
		if gotMin.CloneStatus != CloneStatusCloning || gotMin.ConsecutiveFailures != 0 ||
			gotMin.NextIssueNumber != 0 || gotMin.NextCRNumber != 0 {
			t.Errorf("minimal repo defaults wrong: %+v", gotMin)
		}

		// Repos() lists both, ordered by name.
		repos, err := s.Repos(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(repos) != 2 || repos[0].Name != "delta" || repos[1].Name != "gamma" {
			names := make([]string, len(repos))
			for i, r := range repos {
				names[i] = r.Name
			}
			t.Errorf("list = %v, want [delta gamma]", names)
		}

		// UNIQUE name → typed error.
		if _, err := s.CreateRepo(ctx, testRepo("gamma", now)); !errors.Is(err, ErrNameTaken) {
			t.Errorf("duplicate name error = %v, want ErrNameTaken", err)
		}

		if _, err := s.RepoByID(ctx, "repo_00000000000000000000000000000000"); !errors.Is(err, ErrNotFound) {
			t.Errorf("by unknown id error = %v, want ErrNotFound", err)
		}
	})
}

func TestCreateRepoSeedsTriageLabels(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		repo := testRepo("labels", time.Date(2026, 7, 6, 9, 0, 0, 0, time.UTC))
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create: %v", err)
		}

		want := map[string]string{
			"needs-triage":    "#fbca04",
			"needs-info":      "#d4c5f9",
			"ready-for-agent": "#0e8a16",
			"ready-for-human": "#1d76db",
			"wontfix":         "#cccccc",
		}
		if got := repoLabels(t, s, repo.ID); !reflect.DeepEqual(got, want) {
			t.Errorf("seeded labels = %v, want %v", got, want)
		}

		// SeedRepoLabels is idempotent: reseeding leaves the five rows alone.
		if err := s.SeedRepoLabels(ctx, repo.ID); err != nil {
			t.Fatalf("reseed: %v", err)
		}
		if got := repoLabels(t, s, repo.ID); !reflect.DeepEqual(got, want) {
			t.Errorf("labels after reseed = %v, want %v", got, want)
		}

		// Repo delete cascades the labels (FK enforcement is on — the
		// sqlite open recipe's foreign_keys(1) is load-bearing here).
		if err := s.DeleteRepo(ctx, repo.ID); err != nil {
			t.Fatalf("delete repo: %v", err)
		}
		if got := repoLabels(t, s, repo.ID); len(got) != 0 {
			t.Errorf("labels after repo delete = %v, want none (cascade)", got)
		}
	})
}

func repoLabels(t *testing.T, s *Store, repoID string) map[string]string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), s.rebind(
		`SELECT name, color FROM labels WHERE repo_id = ?`), repoID)
	if err != nil {
		t.Fatalf("query labels: %v", err)
	}
	defer func() { _ = rows.Close() }()
	labels := make(map[string]string)
	for rows.Next() {
		var name, color string
		if err := rows.Scan(&name, &color); err != nil {
			t.Fatalf("scan label: %v", err)
		}
		if _, dup := labels[name]; dup {
			t.Fatalf("duplicate label %q for repo %s", name, repoID)
		}
		labels[name] = color
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("labels rows: %v", err)
	}
	return labels
}

func TestUpdateRepoSettings(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

		repo := testRepo("epsilon", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create: %v", err)
		}
		other := testRepo("zeta", now)
		if _, err := s.CreateRepo(ctx, other); err != nil {
			t.Fatalf("create other: %v", err)
		}

		// Partial update: only the set fields change.
		updated, err := s.UpdateRepoSettings(ctx, repo.ID, RepoSettingsUpdate{
			Name:          Set("epsilon-2"),
			Incogni:       Set(true),
			BudgetMinutes: Set(intPtr(45)),
			ModelDefault:  Set(strPtr("sonnet")),
		})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if updated.Name != "epsilon-2" || !updated.Incogni ||
			updated.BudgetMinutes == nil || *updated.BudgetMinutes != 45 ||
			updated.ModelDefault == nil || *updated.ModelDefault != "sonnet" {
			t.Errorf("updated = %+v, want the four set fields applied", updated)
		}
		if updated.AFKBranchPattern != "afk/<N>" || updated.ManualBranchPrefix != "lab/" ||
			updated.DefaultBranch != "main" || updated.TrackerBinding != TrackerBindingBuiltin {
			t.Errorf("unset fields drifted: %+v", updated)
		}

		// Nullable column back to NULL.
		updated, err = s.UpdateRepoSettings(ctx, repo.ID, RepoSettingsUpdate{
			BudgetMinutes: Set[*int](nil),
			ModelDefault:  Set[*string](nil),
		})
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if updated.BudgetMinutes != nil || updated.ModelDefault != nil {
			t.Errorf("cleared fields = %+v, want nil", updated)
		}

		// Every remaining PATCHable field in one pass.
		cred, err := s.CreateCredential(ctx, ids.NewID("cred"), "u-key",
			CredentialKindSSHKey, []byte("p"), now)
		if err != nil {
			t.Fatalf("create cred: %v", err)
		}
		forge, err := s.CreateCredential(ctx, ids.NewID("cred"), "u-forge",
			CredentialKindForgeToken, []byte("p"), now)
		if err != nil {
			t.Fatalf("create forge cred: %v", err)
		}
		updated, err = s.UpdateRepoSettings(ctx, repo.ID, RepoSettingsUpdate{
			CredentialID:         Set(&cred.ID),
			ForgeCredentialID:    Set(&forge.ID),
			TrackerBinding:       Set(TrackerBindingForge),
			DefaultBranch:        Set("trunk"),
			EffortDefault:        Set(strPtr("high")),
			GitAuthorName:        Set(strPtr("D. P.")),
			GitAuthorEmail:       Set(strPtr("dp@example.com")),
			AFKBranchPattern:     Set("issue-<N>"),
			ManualBranchPrefix:   Set("wip/"),
			AFKAutoEnabled:       Set(true),
			MaxInstancesOverride: Set(intPtr(3)),
		})
		if err != nil {
			t.Fatalf("full update: %v", err)
		}
		if updated.CredentialID == nil || *updated.CredentialID != cred.ID ||
			updated.ForgeCredentialID == nil || *updated.ForgeCredentialID != forge.ID ||
			updated.TrackerBinding != TrackerBindingForge || updated.DefaultBranch != "trunk" ||
			updated.EffortDefault == nil || *updated.EffortDefault != "high" ||
			updated.GitAuthorName == nil || *updated.GitAuthorName != "D. P." ||
			updated.GitAuthorEmail == nil || *updated.GitAuthorEmail != "dp@example.com" ||
			updated.AFKBranchPattern != "issue-<N>" || updated.ManualBranchPrefix != "wip/" ||
			!updated.AFKAutoEnabled ||
			updated.MaxInstancesOverride == nil || *updated.MaxInstancesOverride != 3 {
			t.Errorf("full update = %+v, want every field applied", updated)
		}

		// Empty update: no-op, still returns the row.
		unchanged, err := s.UpdateRepoSettings(ctx, repo.ID, RepoSettingsUpdate{})
		if err != nil {
			t.Fatalf("empty update: %v", err)
		}
		if !reflect.DeepEqual(unchanged, updated) {
			t.Errorf("empty update changed the row:\n got %+v\nwant %+v", unchanged, updated)
		}

		// Rename collision → ErrNameTaken; missing id → ErrNotFound.
		if _, err := s.UpdateRepoSettings(ctx, repo.ID, RepoSettingsUpdate{Name: Set("zeta")}); !errors.Is(err, ErrNameTaken) {
			t.Errorf("rename collision error = %v, want ErrNameTaken", err)
		}
		if _, err := s.UpdateRepoSettings(ctx, "repo_00000000000000000000000000000000",
			RepoSettingsUpdate{Name: Set("x")}); !errors.Is(err, ErrNotFound) {
			t.Errorf("update missing error = %v, want ErrNotFound", err)
		}
		if _, err := s.UpdateRepoSettings(ctx, "repo_00000000000000000000000000000000",
			RepoSettingsUpdate{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("empty update on missing error = %v, want ErrNotFound", err)
		}
	})
}

// The AFK-override spawn-default columns (issue #19 / ADR-0021): the two
// nullable strings and the JSON options bag round-trip through create + update,
// and a present-but-empty bag stays distinct from NULL.
func TestRepoAFKSpawnDefaultsRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 7, 9, 0, 0, 0, time.UTC)

		// Create with all three AFK columns populated.
		full := testRepo("afkdefaults", now)
		full.AFKModelDefault = strPtr("sonnet")
		full.AFKEffortDefault = strPtr("low")
		full.AFKOptions = map[string]string{"ultracode": "true"}
		created, err := s.CreateRepo(ctx, full)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		got, err := s.RepoByID(ctx, full.ID)
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if !reflect.DeepEqual(got, created) {
			t.Errorf("afk columns round trip mismatch:\n got %+v\nwant %+v", got, created)
		}

		// A fresh minimal repo has all three NULL.
		min := testRepo("afkminimal", now)
		if _, err := s.CreateRepo(ctx, min); err != nil {
			t.Fatalf("create minimal: %v", err)
		}
		gotMin, err := s.RepoByID(ctx, min.ID)
		if err != nil {
			t.Fatalf("by id minimal: %v", err)
		}
		if gotMin.AFKModelDefault != nil || gotMin.AFKEffortDefault != nil || gotMin.AFKOptions != nil {
			t.Errorf("minimal repo has non-nil AFK defaults: %+v", gotMin)
		}

		// Update: set the two strings and replace the bag.
		updated, err := s.UpdateRepoSettings(ctx, full.ID, RepoSettingsUpdate{
			AFKModelDefault:  Set(strPtr("fable")),
			AFKEffortDefault: Set(strPtr("high")),
			AFKOptions:       Set(map[string]string{"ultracode": "false"}),
		})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if updated.AFKModelDefault == nil || *updated.AFKModelDefault != "fable" ||
			updated.AFKEffortDefault == nil || *updated.AFKEffortDefault != "high" ||
			updated.AFKOptions["ultracode"] != "false" {
			t.Errorf("updated AFK defaults = %+v, want fable/high/{ultracode:false}", updated)
		}

		// A present-but-empty bag persists as an empty (non-nil) map — "explicitly
		// no options", distinct from NULL/inherit.
		updated, err = s.UpdateRepoSettings(ctx, full.ID, RepoSettingsUpdate{
			AFKOptions: Set(map[string]string{}),
		})
		if err != nil {
			t.Fatalf("update empty bag: %v", err)
		}
		if updated.AFKOptions == nil || len(updated.AFKOptions) != 0 {
			t.Errorf("empty bag = %+v, want a non-nil empty map", updated.AFKOptions)
		}

		// Clear back to NULL.
		updated, err = s.UpdateRepoSettings(ctx, full.ID, RepoSettingsUpdate{
			AFKModelDefault:  Set[*string](nil),
			AFKEffortDefault: Set[*string](nil),
			AFKOptions:       Set[map[string]string](nil),
		})
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if updated.AFKModelDefault != nil || updated.AFKEffortDefault != nil || updated.AFKOptions != nil {
			t.Errorf("cleared AFK defaults = %+v, want all nil", updated)
		}
	})
}

func TestUpdateRepoCloneStatusAndTouch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)

		repo := testRepo("eta", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create: %v", err)
		}

		// cloning → error carries the message.
		if err := s.UpdateRepoCloneStatus(ctx, repo.ID, CloneStatusError, "remote hung up"); err != nil {
			t.Fatalf("to error: %v", err)
		}
		got, err := s.RepoByID(ctx, repo.ID)
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got.CloneStatus != CloneStatusError || got.CloneError == nil || *got.CloneError != "remote hung up" {
			t.Errorf("after error: status %q, clone_error %v", got.CloneStatus, got.CloneError)
		}

		// Retry path: error → cloning clears clone_error; then ready.
		if err := s.UpdateRepoCloneStatus(ctx, repo.ID, CloneStatusCloning, ""); err != nil {
			t.Fatalf("to cloning: %v", err)
		}
		if err := s.UpdateRepoCloneStatus(ctx, repo.ID, CloneStatusReady, ""); err != nil {
			t.Fatalf("to ready: %v", err)
		}
		got, err = s.RepoByID(ctx, repo.ID)
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got.CloneStatus != CloneStatusReady || got.CloneError != nil {
			t.Errorf("after ready: status %q, clone_error %v", got.CloneStatus, got.CloneError)
		}

		if err := s.UpdateRepoCloneStatus(ctx, "repo_00000000000000000000000000000000",
			CloneStatusReady, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("clone status missing error = %v, want ErrNotFound", err)
		}

		// TouchRepoOpened stamps last_opened_at with stored precision.
		opened := now.Add(30 * time.Minute).Add(789 * time.Microsecond)
		if err := s.TouchRepoOpened(ctx, repo.ID, opened); err != nil {
			t.Fatalf("touch: %v", err)
		}
		got, err = s.RepoByID(ctx, repo.ID)
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got.LastOpenedAt == nil || !got.LastOpenedAt.Equal(opened.Truncate(time.Millisecond)) {
			t.Errorf("LastOpenedAt = %v, want %v", got.LastOpenedAt, opened.Truncate(time.Millisecond))
		}
		if err := s.TouchRepoOpened(ctx, "repo_00000000000000000000000000000000", opened); !errors.Is(err, ErrNotFound) {
			t.Errorf("touch missing error = %v, want ErrNotFound", err)
		}
	})
}

// TestRepoCredentialFKViolationMapsToErrCredentialGone locks in the typed
// mapping for the checkCredentialKind race: the credential passed the
// caller's kind check but was deleted before the repo write landed, so the
// FK violation must surface as ErrCredentialGone (API: 409), never as a
// generic error (API: 500). Runs on both backends — the sqlite extended
// result code and the postgres 23503 code take different detection paths.
func TestRepoCredentialFKViolationMapsToErrCredentialGone(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 6, 16, 0, 0, 0, time.UTC)
		dangling := "cred_00000000000000000000000000000000"

		// CreateRepo with a dangling git credential.
		repo := testRepo("fk-create", now)
		repo.CredentialID = strPtr(dangling)
		if _, err := s.CreateRepo(ctx, repo); !errors.Is(err, ErrCredentialGone) {
			t.Errorf("create with dangling credential_id error = %v, want ErrCredentialGone", err)
		}

		// CreateRepo with a dangling forge credential.
		repo = testRepo("fk-create-forge", now)
		repo.ForgeCredentialID = strPtr(dangling)
		if _, err := s.CreateRepo(ctx, repo); !errors.Is(err, ErrCredentialGone) {
			t.Errorf("create with dangling forge_credential_id error = %v, want ErrCredentialGone", err)
		}

		// UpdateRepoSettings pointing either column at a dangling credential.
		existing := testRepo("fk-update", now)
		if _, err := s.CreateRepo(ctx, existing); err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.UpdateRepoSettings(ctx, existing.ID, RepoSettingsUpdate{
			CredentialID: Set(strPtr(dangling)),
		}); !errors.Is(err, ErrCredentialGone) {
			t.Errorf("update with dangling credential_id error = %v, want ErrCredentialGone", err)
		}
		if _, err := s.UpdateRepoSettings(ctx, existing.ID, RepoSettingsUpdate{
			ForgeCredentialID: Set(strPtr(dangling)),
		}); !errors.Is(err, ErrCredentialGone) {
			t.Errorf("update with dangling forge_credential_id error = %v, want ErrCredentialGone", err)
		}

		// The failed updates left the row untouched.
		got, err := s.RepoByID(ctx, existing.ID)
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got.CredentialID != nil || got.ForgeCredentialID != nil {
			t.Errorf("failed FK updates changed the row: %+v", got)
		}
	})
}

func TestDeleteRepo(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 6, 14, 0, 0, 0, time.UTC)

		repo := testRepo("theta", now)
		if _, err := s.CreateRepo(ctx, repo); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := s.DeleteRepo(ctx, repo.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := s.RepoByID(ctx, repo.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("by id after delete error = %v, want ErrNotFound", err)
		}
		if err := s.DeleteRepo(ctx, repo.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("second delete error = %v, want ErrNotFound", err)
		}
	})
}

func TestHealInterruptedClones(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 6, 15, 0, 0, 0, time.UTC)

		// Two repos interrupted mid-clone, one already ready, one already
		// failed: healing touches only the 'cloning' rows.
		intact := testRepo("heal-intact", now)
		gone := testRepo("heal-gone", now)
		alreadyReady := testRepo("heal-ready", now)
		alreadyReady.CloneStatus = CloneStatusReady
		alreadyErr := testRepo("heal-error", now)
		alreadyErr.CloneStatus = CloneStatusError
		alreadyErr.CloneError = strPtr("remote hung up")
		for _, r := range []Repo{intact, gone, alreadyReady, alreadyErr} {
			if _, err := s.CreateRepo(ctx, r); err != nil {
				t.Fatalf("create %s: %v", r.Name, err)
			}
		}

		probed := make(map[string]bool)
		ready, failed, err := s.HealInterruptedClones(ctx, func(id string) bool {
			probed[id] = true
			return id == intact.ID // only the intact bare clone answers rev-parse
		})
		if err != nil {
			t.Fatalf("heal: %v", err)
		}
		if len(ready) != 1 || ready[0] != intact.ID {
			t.Errorf("ready = %v, want [%s]", ready, intact.ID)
		}
		if len(failed) != 1 || failed[0] != gone.ID {
			t.Errorf("failed = %v, want [%s]", failed, gone.ID)
		}
		if probed[alreadyReady.ID] || probed[alreadyErr.ID] {
			t.Errorf("probe called for non-cloning repos: %v", probed)
		}

		healed, err := s.RepoByID(ctx, intact.ID)
		if err != nil {
			t.Fatalf("by id intact: %v", err)
		}
		if healed.CloneStatus != CloneStatusReady || healed.CloneError != nil {
			t.Errorf("intact repo = %q/%v, want ready/nil", healed.CloneStatus, healed.CloneError)
		}
		broken, err := s.RepoByID(ctx, gone.ID)
		if err != nil {
			t.Fatalf("by id gone: %v", err)
		}
		if broken.CloneStatus != CloneStatusError || broken.CloneError == nil ||
			*broken.CloneError != CloneErrorInterrupted {
			t.Errorf("gone repo = %q/%v, want error/%q", broken.CloneStatus, broken.CloneError, CloneErrorInterrupted)
		}
		untouched, err := s.RepoByID(ctx, alreadyErr.ID)
		if err != nil {
			t.Fatalf("by id already-error: %v", err)
		}
		if untouched.CloneError == nil || *untouched.CloneError != "remote hung up" {
			t.Errorf("pre-existing clone_error rewritten: %v", untouched.CloneError)
		}

		// Nothing left to heal → both slices empty.
		ready, failed, err = s.HealInterruptedClones(ctx, func(string) bool {
			t.Error("probe called with no cloning repos")
			return false
		})
		if err != nil {
			t.Fatalf("second heal: %v", err)
		}
		if len(ready) != 0 || len(failed) != 0 {
			t.Errorf("second heal = %v/%v, want empty", ready, failed)
		}
	})
}
