package instance

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

// retainFixture is a Service with just what RetainTranscript touches — a real
// sqlite store, the provider registry over a scriptable Fake, the per-run
// tree manager, the clock, and the transcripts root — no git fixture needed
// (revokeService's shape).
type retainFixture struct {
	svc            *Service
	st             *store.Store
	prov           *providertest.Fake
	transcriptsDir string
	repoID         string
	logs           *syncBuffer
}

func newRetainFixture(t *testing.T) *retainFixture {
	t.Helper()
	st := testutil.TempStore(t)
	if err := st.SeedDefaultSettings(t.Context(), 6, "claude-code"); err != nil {
		t.Fatalf("SeedDefaultSettings: %v", err)
	}
	repo, err := st.CreateRepo(t.Context(), store.Repo{
		ID: ids.NewID("repo"), Name: "proj", RemoteURL: "file:///nowhere",
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: clockTime,
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	prov := providertest.New()
	reg, err := provider.NewRegistry(prov)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	buf := &syncBuffer{}
	transcriptsDir := filepath.Join(t.TempDir(), "transcripts")
	return &retainFixture{
		svc: &Service{
			store:          st,
			providers:      reg,
			homes:          newTestHomes(t),
			log:            slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
			now:            testutil.NewFakeClock(clockTime).Now,
			transcriptsDir: transcriptsDir,
		},
		st: st, prov: prov, transcriptsDir: transcriptsDir, repoID: repo.ID, logs: buf,
	}
}

// run inserts a run row whose transcript path points into its instance HOME
// (path "" = never located) and, when endedAt is non-nil, ended at that time.
func (f *retainFixture) run(t *testing.T, path string, endedAt *time.Time) store.Run {
	t.Helper()
	id := ids.NewID("run")
	r := store.Run{
		ID: id, RepoID: f.repoID, Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/x", WorktreePath: "/wt/" + id, SessionName: "proj~" + id, Model: "opus[1m]", Effort: "max",
		StartedAt: clockTime.Add(-time.Hour), Outcome: store.RunOutcomeActive,
	}
	if path != "" {
		p := filepath.Join(f.svc.homes.HomePath(id), path)
		r.TranscriptPath = &p
	}
	if endedAt != nil {
		r.EndedAt, r.Outcome = endedAt, store.RunOutcomeStopped
	}
	r, err := f.st.CreateRun(t.Context(), r)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return r
}

func (f *retainFixture) storedPath(t *testing.T, id string) *string {
	t.Helper()
	r, err := f.st.RunByID(t.Context(), id)
	if err != nil {
		t.Fatalf("RunByID: %v", err)
	}
	return r.TranscriptPath
}

func (f *retainFixture) destExists(id string) bool {
	_, err := os.Stat(filepath.Join(f.transcriptsDir, id))
	return !errors.Is(err, fs.ErrNotExist)
}

// The happy path: the provider is handed the run's worktree, its instance
// HOME, the stored transcript path and a fresh 0700 <transcripts>/<runID>/,
// and the path it returns is what the row now reads.
func TestRetainTranscript_persistsRetainedPath(t *testing.T) {
	f := newRetainFixture(t)
	run := f.run(t, ".claude/projects/x/s.jsonl", nil)
	dest := filepath.Join(f.transcriptsDir, run.ID)
	retained := filepath.Join(dest, "s.jsonl")
	f.prov.SetRetainResult(retained, nil)

	f.svc.RetainTranscript(t.Context(), run.ID)

	calls := f.prov.RetainCalls()
	want := providertest.RetainCall{Worktree: run.WorktreePath, Home: f.svc.homes.HomePath(run.ID),
		TranscriptPath: *run.TranscriptPath, DestDir: dest}
	if len(calls) != 1 || calls[0] != want {
		t.Fatalf("RetainTranscript calls = %+v, want exactly [%+v]", calls, want)
	}
	if got := f.storedPath(t, run.ID); got == nil || *got != retained {
		t.Errorf("stored transcript path = %v, want %q", got, retained)
	}
	for _, dir := range []string{f.transcriptsDir, dest} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %o, want 0700", dir, perm)
		}
	}

	// A second fire for the same run (its row already names the retained
	// copy) neither calls the provider again nor warns.
	f.svc.RetainTranscript(t.Context(), run.ID)
	if n := len(f.prov.RetainCalls()); n != 1 {
		t.Errorf("RetainTranscript calls after a second fire = %d, want 1", n)
	}
	if strings.Contains(f.logs.String(), "level=WARN") {
		t.Errorf("unexpected warning: %s", f.logs.String())
	}
}

// Every no-op case: nothing reaches the provider, no retention dir appears,
// and nothing is logged as a failure.
func TestRetainTranscript_noOps(t *testing.T) {
	longAgo := clockTime.Add(-31 * 24 * time.Hour)
	recent := clockTime.Add(-29 * 24 * time.Hour)
	cases := []struct {
		name  string
		setup func(t *testing.T, f *retainFixture) string
	}{
		{"no run row (rollback / true orphan)", func(_ *testing.T, _ *retainFixture) string {
			return ids.NewID("run")
		}},
		{"no transcript path", func(t *testing.T, f *retainFixture) string {
			return f.run(t, "", nil).ID
		}},
		{"retention 0", func(t *testing.T, f *retainFixture) string {
			if err := f.st.SetSetting(t.Context(), store.SettingTranscriptRetentionDays, "0"); err != nil {
				t.Fatal(err)
			}
			return f.run(t, "t.jsonl", nil).ID
		}},
		{"ended before the window", func(t *testing.T, f *retainFixture) string {
			return f.run(t, "t.jsonl", &longAgo).ID
		}},
		{"transcripts dir unset", func(t *testing.T, f *retainFixture) string {
			f.svc.transcriptsDir = ""
			return f.run(t, "t.jsonl", nil).ID
		}},
		{"run id not run-shaped", func(_ *testing.T, _ *retainFixture) string {
			return "../escape"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetainFixture(t)
			f.prov.SetRetainResult("/should/not/be/persisted", nil)
			id := tc.setup(t, f)
			f.svc.RetainTranscript(t.Context(), id)
			if calls := f.prov.RetainCalls(); len(calls) != 0 {
				t.Errorf("provider called: %+v", calls)
			}
			if f.destExists(id) {
				t.Error("retention dir created for a no-op")
			}
			if strings.Contains(f.logs.String(), "level=WARN") {
				t.Errorf("unexpected warning: %s", f.logs.String())
			}
		})
	}

	// The window boundary's other side: an ended run still inside it is
	// retained.
	t.Run("ended inside the window", func(t *testing.T) {
		f := newRetainFixture(t)
		run := f.run(t, "t.jsonl", &recent)
		f.prov.SetRetainResult(filepath.Join(f.transcriptsDir, run.ID, "t.jsonl"), nil)
		f.svc.RetainTranscript(t.Context(), run.ID)
		if n := len(f.prov.RetainCalls()); n != 1 {
			t.Errorf("provider calls = %d, want 1", n)
		}
	})
}

// A provider failure and a provider with nothing to keep both leave the row
// untouched and the (empty) retention dir removed; the failure warns.
func TestRetainTranscript_providerFailureOrNothingKept(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantWarn bool
	}{
		{"error", errors.New("boom"), true},
		{"nothing to keep", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetainFixture(t)
			run := f.run(t, "t.jsonl", nil)
			f.prov.SetRetainResult("", tc.err)

			f.svc.RetainTranscript(t.Context(), run.ID)

			if n := len(f.prov.RetainCalls()); n != 1 {
				t.Fatalf("provider calls = %d, want 1", n)
			}
			if got := f.storedPath(t, run.ID); got == nil || *got != *run.TranscriptPath {
				t.Errorf("stored transcript path = %v, want untouched %q", got, *run.TranscriptPath)
			}
			if f.destExists(run.ID) {
				t.Error("retention dir left behind")
			}
			if warned := strings.Contains(f.logs.String(), "level=WARN"); warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v: %s", warned, tc.wantWarn, f.logs.String())
			}
		})
	}
}

// An invalid hand-edited window warns and proceeds on the default — the
// retain still happens.
func TestRetainTranscript_invalidSettingUsesDefault(t *testing.T) {
	f := newRetainFixture(t)
	if err := f.st.SetSetting(t.Context(), store.SettingTranscriptRetentionDays, "forever"); err != nil {
		t.Fatal(err)
	}
	run := f.run(t, "t.jsonl", nil)
	retained := filepath.Join(f.transcriptsDir, run.ID, "t.jsonl")
	f.prov.SetRetainResult(retained, nil)

	f.svc.RetainTranscript(t.Context(), run.ID)

	if got := f.storedPath(t, run.ID); got == nil || *got != retained {
		t.Errorf("stored transcript path = %v, want %q", got, retained)
	}
	if !strings.Contains(f.logs.String(), "level=WARN") {
		t.Error("no warning for the invalid setting")
	}
}

// An unknown provider is a warning and no retention dir.
func TestRetainTranscript_unknownProvider(t *testing.T) {
	f := newRetainFixture(t)
	run := f.run(t, "t.jsonl", nil)
	reg, err := provider.NewRegistry(providertest.NewNoLink())
	if err != nil {
		t.Fatal(err)
	}
	f.svc.providers = reg

	f.svc.RetainTranscript(t.Context(), run.ID)

	if f.destExists(run.ID) {
		t.Error("retention dir created for an unknown provider")
	}
	if !strings.Contains(f.logs.String(), "level=WARN") {
		t.Error("no warning for the unknown provider")
	}
}
