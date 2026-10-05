package instance

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// The seams the readiness report (issue #61) reads the spawn path through:
// the container gate's verdict as a value, the outcome of the spawn-time
// pull-if-missing, and the attribution of the spawn's fetches.

// ContainerGate is the gate refuseContainerSpawn enforces, as a verdict: the
// same stage order, the same refusal text — and it starts no process, which
// is what lets a page view ask it.
func TestContainerGate_stagesMatchTheSpawnRefusal(t *testing.T) {
	cases := []struct {
		name  string
		wire  func(f *fixture)
		stage ContainerGateStage
	}{
		{"unconfigured server", func(f *fixture) { f.svc.containerPreflight = nil }, ContainerGateNotConfigured},
		{"preflight pending", func(f *fixture) {
			f.svc.containerPreflight = func() (podmanx.Result, bool) { return podmanx.Result{}, false }
		}, ContainerGatePreflightPending},
		{"preflight failed", func(f *fixture) {
			f.svc.containerPreflight = func() (podmanx.Result, bool) {
				return podmanx.Result{Failures: []podmanx.Failure{
					{Check: podmanx.CheckPasta, Detail: "pasta not found on PATH", Hint: "install passt (provides pasta)"},
				}}, true
			}
		}, ContainerGatePreflightFailed},
		{"missing tools image for provider", func(f *fixture) {
			f.svc.containerToolsImages = map[string]string{}
		}, ContainerGateNoToolsImage},
		{"no dev image at any layer", func(f *fixture) { f.svc.containerImage = "" }, ContainerGateNoDevImage},
		{"open", func(*fixture) {}, ContainerGateOpen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			rec := f.enableContainer(t)
			tc.wire(f)
			repo, err := f.st.RepoByID(t.Context(), f.repo.ID)
			if err != nil {
				t.Fatal(err)
			}

			g := f.svc.ContainerGate(t.Context(), "claude-code", repo)
			if g.Stage != tc.stage {
				t.Fatalf("stage = %d (err %v), want %d", g.Stage, g.Err, tc.stage)
			}
			if (g.Err == nil) != (tc.stage == ContainerGateOpen) {
				t.Fatalf("Err = %v at stage %d: it must be nil exactly when the gate is open", g.Err, g.Stage)
			}
			if tc.stage == ContainerGateOpen && g.Image != testDevImage {
				t.Errorf("open gate image = %q, want the effective dev image %q", g.Image, testDevImage)
			}
			if tc.stage != ContainerGateOpen && g.Image != "" {
				t.Errorf("closed gate carries an image: %q", g.Image)
			}
			if tc.stage == ContainerGatePreflightFailed && (len(g.Preflight.Failures) != 1 || g.Preflight.Failures[0].Check != podmanx.CheckPasta) {
				t.Errorf("preflight verdict not carried: %+v", g.Preflight)
			}
			if tc.stage == ContainerGateNoDevImage && !errors.Is(g.Err, ErrNoDevImage) {
				t.Errorf("no-dev-image Err = %v, want errors.Is ErrNoDevImage", g.Err)
			}

			// One gate, two consumers: the spawn's refusal is this verdict's
			// Err, word for word.
			image, refusal := f.svc.refuseContainerSpawn(t.Context(), "claude-code", repo)
			switch {
			case g.Err == nil && (refusal != nil || image != g.Image):
				t.Errorf("refuseContainerSpawn = %q, %v; the open gate said %q", image, refusal, g.Image)
			case g.Err != nil && (refusal == nil || refusal.Error() != g.Err.Error()):
				t.Errorf("refusal = %v, gate Err = %v — they must be the same text", refusal, g.Err)
			}
			var bad *BadRequestError
			if g.Err != nil && !errors.As(refusal, &bad) {
				t.Errorf("refusal type = %T, want *BadRequestError", refusal)
			}

			// Asking the gate runs nothing: no podman call was recorded.
			if calls := rec.recorded(); len(calls) != 0 {
				t.Errorf("the gate ran podman: %q", calls)
			}
		})
	}
}

// ensureLog is an Options.ImageEnsured recording what it was told.
type ensureLog struct{ got []string }

func (l *ensureLog) observe(repoID, ref string, err error) {
	l.got = append(l.got, fmt.Sprintf("%s %s %v", repoID, ref, err))
}

// Launch reports the outcome of the dev image's pull-if-missing — the only
// evidence lab ever has of whether an image is present.
func TestStart_reportsDevImageEnsure(t *testing.T) {
	t.Run("present image", func(t *testing.T) {
		f := newFixture(t)
		f.enableContainer(t)
		log := &ensureLog{}
		f.svc.imageEnsured = log.observe
		if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if want := []string{fmt.Sprintf("%s %s <nil>", f.repo.ID, testDevImage)}; fmt.Sprint(log.got) != fmt.Sprint(want) {
			t.Fatalf("reported %v, want %v", log.got, want)
		}
	})

	t.Run("pull fails: reported, and the spawn is refused before the claim", func(t *testing.T) {
		f := newFixture(t)
		rec := f.enableContainer(t)
		log := &ensureLog{}
		f.svc.imageEnsured = log.observe
		rec.errs[testPodmanBin+" image exists "+testDevImage] = errors.New("exit status 1")
		rec.errs[testPodmanBin+" pull "+testDevImage] = errors.New("exit status 125: manifest unknown")

		_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
		var bad *BadRequestError
		if !errors.As(err, &bad) {
			t.Fatalf("Start = %v, want the pull refusal", err)
		}
		if len(log.got) != 1 || !strings.HasPrefix(log.got[0], f.repo.ID+" "+testDevImage+" pulling dev image ") ||
			!strings.Contains(log.got[0], "manifest unknown") {
			t.Fatalf("reported %v, want one failure naming the pull error", log.got)
		}
		if dirExists(filepath.Join(f.worktreeRoot, "proj-20260608-1530")) {
			t.Error("the refused spawn created a worktree")
		}
	})

	t.Run("host Runner: nothing to ensure, nothing reported", func(t *testing.T) {
		f := newFixture(t)
		f.wireContainer() // container wiring present, repo still on host
		log := &ensureLog{}
		f.svc.imageEnsured = log.observe
		if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if len(log.got) != 0 {
			t.Fatalf("a host spawn reported an image ensure: %v", log.got)
		}
	})

	t.Run("a closed gate never reaches the ensure", func(t *testing.T) {
		f := newFixture(t)
		f.enableContainer(t)
		f.svc.containerToolsImages = map[string]string{}
		log := &ensureLog{}
		f.svc.imageEnsured = log.observe
		if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID}); err == nil {
			t.Fatal("Start succeeded through a closed gate")
		}
		if len(log.got) != 0 {
			t.Fatalf("reported %v, want nothing", log.got)
		}
	})

	t.Run("an ensure that died with its caller is not reported", func(t *testing.T) {
		f := newFixture(t)
		rec := f.enableContainer(t)
		log := &ensureLog{}
		f.svc.imageEnsured = log.observe
		ctx, cancel := context.WithCancel(t.Context())
		// The probe misses and the pull "fails" because the caller went away
		// mid-pull.
		rec.errs[testPodmanBin+" image exists "+testDevImage] = errors.New("exit status 1")
		rec.errs[testPodmanBin+" pull "+testDevImage] = context.Canceled
		f.svc.podmanRun = func(c context.Context, name string, args ...string) ([]byte, error) {
			if len(args) > 0 && args[0] == "pull" {
				cancel()
			}
			return rec.run(c, name, args...)
		}
		// Launch directly: Start's own preflight would trip on the cancelled
		// context before the ensure.
		_, err := f.svc.Launch(ctx, LaunchSpec{
			Repo: f.mustRepo(t), Provider: f.prov, Kind: store.RunKindManual,
			SessionName: "proj~x", Branch: "lab/x", WorktreePath: filepath.Join(f.worktreeRoot, "proj-x"),
		})
		if err == nil {
			t.Fatal("Launch succeeded despite the failed pull")
		}
		if len(log.got) != 0 {
			t.Fatalf("a cancelled ensure was reported: %v", log.got)
		}
	})
}

// mustRepo re-reads the fixture repo row (enableContainer changed it).
func (f *fixture) mustRepo(t *testing.T) store.Repo {
	t.Helper()
	repo, err := f.st.RepoByID(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatalf("RepoByID: %v", err)
	}
	return repo
}

// fetchReports is a gitx.FetchObserver recording attributions and outcomes.
type fetchReports struct {
	attrs []gitx.FetchAttribution
	errs  []error
}

func (r *fetchReports) observe(a gitx.FetchAttribution, err error) {
	r.attrs = append(r.attrs, a)
	r.errs = append(r.errs, err)
}

// Every spawn's worktree fetch is attributed to the repo and to the version
// of its git credential — so its outcome can be recorded, and recognized as
// stale once that credential changes.
func TestStart_attributesTheWorktreeFetch(t *testing.T) {
	f := newFixture(t)
	reports := &fetchReports{}
	f.svc.git.SetFetchObserver(reports.observe)

	// No git credential: the "none" stamp.
	if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "one"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	want := gitx.FetchAttribution{RepoID: f.repo.ID, Credential: store.NoCredentialStamp}
	if len(reports.attrs) != 1 || reports.attrs[0] != want || reports.errs[0] != nil {
		t.Fatalf("reported %+v / %v, want one success for %+v", reports.attrs, reports.errs, want)
	}

	// With a git credential: the stamp of that credential's current version.
	sealed, err := f.svc.vault.EncryptPayload(vault.HTTPSTokenPayload{Username: "op", Token: "tok"})
	if err != nil {
		t.Fatalf("EncryptPayload: %v", err)
	}
	cred, err := f.st.CreateCredential(t.Context(), ids.NewID("cred"), "https token",
		store.CredentialKindHTTPSToken, sealed, f.clock.Now())
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}
	if _, err := f.st.UpdateRepoSettings(t.Context(), f.repo.ID, store.RepoSettingsUpdate{CredentialID: store.Set(&cred.ID)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "two"}); err != nil {
		t.Fatalf("Start with a credential: %v", err)
	}
	want.Credential = store.CredentialStamp(cred.ID, cred.UpdatedAt)
	if len(reports.attrs) != 2 || reports.attrs[1] != want || reports.errs[1] != nil {
		t.Fatalf("second report = %+v / %v, want a success for %+v", reports.attrs, reports.errs, want)
	}

	// The remote stops answering: the spawn fails, and the failure is
	// reported under the same attribution with git's own words.
	gitCmd(t, f.home, f.bare(), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	_, err = f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "three"})
	var startFailed *StartFailedError
	if !errors.As(err, &startFailed) {
		t.Fatalf("Start against a dead remote = %v, want StartFailedError", err)
	}
	if len(reports.attrs) != 3 || reports.attrs[2] != want {
		t.Fatalf("third report = %+v, want %+v", reports.attrs, want)
	}
	if reports.errs[2] == nil || !strings.Contains(reports.errs[2].Error(), "does not appear to be a git repository") {
		t.Fatalf("reported error = %v, want git's own explanation", reports.errs[2])
	}
}

// A read-only import's snapshot fetch is attributed to the TARGET (its
// reference repo, its credential) on behalf of the importing repo.
func TestStart_attributesImportFetchesToTheTarget(t *testing.T) {
	f := newFixture(t)
	target, _ := f.addImportTarget(t, "libcore", store.CloneStatusReady)
	reports := &fetchReports{}
	f.svc.git.SetFetchObserver(reports.observe)

	if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "ok"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	importFetch := gitx.FetchAttribution{RepoID: target.ID, Credential: store.NoCredentialStamp, OnBehalfOf: f.repo.ID}
	ownFetch := gitx.FetchAttribution{RepoID: f.repo.ID, Credential: store.NoCredentialStamp}
	if len(reports.attrs) != 2 || reports.attrs[0] != importFetch || reports.attrs[1] != ownFetch {
		t.Fatalf("reported %+v, want the import's fetch then the repo's own", reports.attrs)
	}
	if reports.errs[0] != nil || reports.errs[1] != nil {
		t.Fatalf("errors = %v, want none", reports.errs)
	}

	// The target's remote dies: the spawn is refused before the claim, and
	// the one fetch that ran is the target's — reported as its failure.
	gitCmd(t, f.home, filepath.Join(f.reposDir, target.ID+".git"),
		"remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "refused"}); err == nil {
		t.Fatal("Start succeeded despite the dead import target")
	}
	if len(reports.attrs) != 3 || reports.attrs[2] != importFetch || reports.errs[2] == nil {
		t.Fatalf("after the refusal: %+v / %v, want one more report — the target's failed fetch", reports.attrs, reports.errs)
	}
}
