package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// The store reads behind the readiness report and the repo summaries (issue
// #61): the credential version stamp, the per-repo issue counts and the
// one-query import map.

// A credential stamp names one VERSION of a credential: stable while the row
// is untouched, different after a rotation or a rename, different for
// another credential, and its own value for "no credential".
func TestCredentialStamp(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		t0 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

		if got := s.CredentialStampByID(ctx, nil); got != NoCredentialStamp {
			t.Fatalf("stamp of no credential = %q, want %q", got, NoCredentialStamp)
		}
		a, b := "cred_stamp_a", "cred_stamp_b"
		for _, id := range []string{a, b} {
			if _, err := s.CreateCredential(ctx, id, "name-"+id, CredentialKindSSHKey, []byte("sealed"), t0); err != nil {
				t.Fatalf("CreateCredential: %v", err)
			}
		}
		first := s.CredentialStampByID(ctx, &a)
		if first == "" || first == NoCredentialStamp {
			t.Fatalf("stamp of a real credential = %q", first)
		}
		if again := s.CredentialStampByID(ctx, &a); again != first {
			t.Fatalf("stamp is not stable: %q then %q", first, again)
		}
		// Same updated_at, another credential: a different stamp.
		if other := s.CredentialStampByID(ctx, &b); other == first {
			t.Fatalf("two credentials share the stamp %q", first)
		}
		// It is exactly CredentialStamp over the row — the form the tracker
		// registry and the evaluator compute from a row they already hold.
		row, err := s.CredentialByID(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if want := CredentialStamp(row.ID, row.UpdatedAt); first != want {
			t.Fatalf("CredentialStampByID = %q, CredentialStamp(row) = %q", first, want)
		}
		metas, err := s.Credentials(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range metas {
			if m.ID == a && CredentialStamp(m.ID, m.UpdatedAt) != first {
				t.Fatalf("stamp from the metadata listing = %q, want %q", CredentialStamp(m.ID, m.UpdatedAt), first)
			}
		}

		// A rotation moves it.
		if err := s.UpdateCredential(ctx, a, nil, []byte("resealed"), t0.Add(time.Second)); err != nil {
			t.Fatalf("UpdateCredential: %v", err)
		}
		rotated := s.CredentialStampByID(ctx, &a)
		if rotated == first {
			t.Fatalf("stamp unchanged by a rotation: %q", rotated)
		}
		// So does a rename (updated_at is stamped either way).
		renamed := "renamed"
		if err := s.UpdateCredential(ctx, a, &renamed, nil, t0.Add(2*time.Second)); err != nil {
			t.Fatalf("UpdateCredential: %v", err)
		}
		if got := s.CredentialStampByID(ctx, &a); got == rotated {
			t.Fatalf("stamp unchanged by a rename: %q", got)
		}

		// An id with no row has the empty stamp, which equals nothing real.
		missing := "cred_stamp_missing"
		if got := s.CredentialStampByID(ctx, &missing); got != "" {
			t.Fatalf("stamp of a missing credential = %q, want empty", got)
		}
	})
}

// OpenIssueCounts groups open issues by repo, optionally only those carrying
// one label; closed issues and other repos' labels never leak in.
func TestOpenIssueCounts(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
		a, b, empty := testRepo("count-alpha", now), testRepo("count-beta", now), testRepo("count-empty", now)
		for _, r := range []Repo{a, b, empty} {
			if _, err := s.CreateRepo(ctx, r); err != nil {
				t.Fatalf("create repo %s: %v", r.Name, err)
			}
		}
		labelID := func(repoID, name string) string {
			t.Helper()
			labels, err := s.LabelsByRepo(ctx, repoID)
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range labels {
				if l.Name == name {
					return l.ID
				}
			}
			t.Fatalf("repo %s has no label %s", repoID, name)
			return ""
		}
		file := func(repoID string, labels ...string) Issue {
			t.Helper()
			ids := make([]string, 0, len(labels))
			for _, l := range labels {
				ids = append(ids, labelID(repoID, l))
			}
			is, err := s.CreateIssueWithLabels(ctx, repoID, "t", "", ids, CommentAuthorOperator, nil, now)
			if err != nil {
				t.Fatalf("CreateIssueWithLabels: %v", err)
			}
			return is
		}

		// alpha: 4 open (2 ready, one of them ALSO needs-triage), 1 closed ready.
		file(a.ID)
		file(a.ID, "needs-triage")
		file(a.ID, "ready-for-agent")
		file(a.ID, "ready-for-agent", "needs-triage")
		closed := file(a.ID, "ready-for-agent")
		if _, err := s.UpdateIssue(ctx, a.ID, closed.Number, IssueUpdate{State: Set(IssueStateClosed)}, now); err != nil {
			t.Fatal(err)
		}
		// beta: 1 open, not ready.
		file(b.ID, "needs-info")

		got, err := s.OpenIssueCounts(ctx, "")
		if err != nil {
			t.Fatalf("OpenIssueCounts: %v", err)
		}
		if want := map[string]int{a.ID: 4, b.ID: 1}; !reflect.DeepEqual(got, want) {
			t.Fatalf("open counts = %v, want %v (a repo without open issues has no entry)", got, want)
		}
		got, err = s.OpenIssueCounts(ctx, "ready-for-agent")
		if err != nil {
			t.Fatalf("OpenIssueCounts(ready): %v", err)
		}
		if want := map[string]int{a.ID: 2}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ready counts = %v, want %v", got, want)
		}
		got, err = s.OpenIssueCounts(ctx, "no-such-label")
		if err != nil || len(got) != 0 {
			t.Fatalf("counts for an unknown label = %v, %v; want empty", got, err)
		}
	})
}

// AllRepoImports is RepoImports for every repo at once: the same targets in
// the same (name) order, and no entry for a repo that imports nothing.
func TestAllRepoImports(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s *Store) {
		ctx := context.Background()
		now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
		got, err := s.AllRepoImports(ctx)
		if err != nil || len(got) != 0 {
			t.Fatalf("AllRepoImports on an empty store = %v, %v", got, err)
		}

		a, b, c := testRepo("all-alpha", now), testRepo("all-beta", now), testRepo("all-charlie", now)
		for _, r := range []Repo{a, b, c} {
			if _, err := s.CreateRepo(ctx, r); err != nil {
				t.Fatalf("create repo %s: %v", r.Name, err)
			}
		}
		// a imports c then b (out of name order); b imports a (mutual).
		for _, pair := range [][2]string{{a.ID, c.ID}, {a.ID, b.ID}, {b.ID, a.ID}} {
			if err := s.AddRepoImport(ctx, pair[0], pair[1]); err != nil {
				t.Fatalf("AddRepoImport: %v", err)
			}
		}
		got, err = s.AllRepoImports(ctx)
		if err != nil {
			t.Fatalf("AllRepoImports: %v", err)
		}
		want := map[string][]string{a.ID: {b.ID, c.ID}, b.ID: {a.ID}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("AllRepoImports = %v, want %v", got, want)
		}
		// It agrees with the per-repo query, repo by repo.
		for _, r := range []Repo{a, b, c} {
			targets, err := s.RepoImports(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, tr := range targets {
				ids = append(ids, tr.ID)
			}
			if !reflect.DeepEqual(ids, got[r.ID]) {
				t.Errorf("%s: RepoImports = %v, AllRepoImports = %v", r.Name, ids, got[r.ID])
			}
		}
	})
}
