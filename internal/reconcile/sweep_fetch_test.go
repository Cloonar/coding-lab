package reconcile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
)

// The runtime sweep fetches every ready repo with NO git credential (it runs
// on s.gitEnv alone), so on a private remote that fetch fails every time, by
// design — the sweep then works from the last-known origin refs. The
// readiness report (issue #61) is built from recorded fetch outcomes, so if
// the sweep's fetch were ever recorded, every private repo would show a
// failing git credential a few minutes after each start. It must never reach
// the fetch observer: not when it succeeds, and above all not when it fails.
func TestRuntimeSweep_fetchNeverReachesTheFetchObserver(t *testing.T) {
	f := newRecFixture(t)
	var reported []gitx.FetchAttribution
	f.git.SetFetchObserver(func(a gitx.FetchAttribution, _ error) { reported = append(reported, a) })

	origin := strings.TrimPrefix(f.repo.RemoteURL, "file://")
	originRef := func() string {
		return recGitCmd(t, f.home, f.bare(), "rev-parse", "refs/remotes/origin/main")
	}

	// Positive control for the fetch itself: the sweep really does fetch —
	// a commit landing on origin shows up in the reference repo.
	before := originRef()
	if err := os.WriteFile(filepath.Join(origin, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recGitCmd(t, f.home, origin, "add", ".")
	recGitCmd(t, f.home, origin, "commit", "-q", "-m", "new")
	f.svc.RuntimeSweep(t.Context())
	if after := originRef(); after == before {
		t.Fatal("the sweep did not fetch: origin/main did not move")
	}
	if len(reported) != 0 {
		t.Fatalf("the sweep's successful fetch was reported: %+v", reported)
	}

	// The case that matters: the credential-less fetch FAILS (here: the
	// remote is gone; on a private remote: authentication). Still nothing.
	if err := os.RemoveAll(origin); err != nil {
		t.Fatal(err)
	}
	if err := f.git.Fetch(t.Context(), f.bare(), f.env); err == nil {
		t.Fatal("precondition: a fetch against the removed origin should fail")
	}
	f.svc.RuntimeSweep(t.Context())
	if len(reported) != 0 {
		t.Fatalf("the sweep's failing fetch was reported: %+v", reported)
	}

	// Positive control for the observer: the very same engine does report a
	// fetch a caller attributed to the repo's credential.
	attr := gitx.FetchAttribution{RepoID: f.repo.ID, Credential: "none"}
	_ = f.git.Fetch(gitx.AttributeFetch(t.Context(), attr), f.bare(), f.env)
	if len(reported) != 1 || reported[0] != attr {
		t.Fatalf("an attributed fetch reported %+v, want exactly %+v", reported, attr)
	}
}
