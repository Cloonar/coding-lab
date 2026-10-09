package reconcile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// withTranscripts points the fixture's service at a fresh <state>/transcripts
// (issue #81) and returns it. The directory itself is NOT created — a missing
// dir is a case under test.
func (f *recFixture) withTranscripts() string {
	f.t.Helper()
	dir := filepath.Join(f.t.TempDir(), "transcripts")
	f.svc.transcriptsDir = dir
	return dir
}

// endedRun inserts a run that ended at endedAt; retained=true gives it a
// retained transcript at <dir>/<runID>/t.jsonl (dir and file created), false
// a NULL transcript path.
func (f *recFixture) endedRun(dir string, endedAt time.Time, retained bool) store.Run {
	f.t.Helper()
	id := ids.NewID("run")
	r := store.Run{
		ID: id, RepoID: f.repo.ID, Kind: store.RunKindManual, Provider: "claude-code",
		Branch: "lab/" + id, WorktreePath: "/wt/" + id, SessionName: "proj~" + id, Model: "opus[1m]", Effort: "max",
		StartedAt: endedAt.Add(-time.Hour), EndedAt: &endedAt, Outcome: store.RunOutcomeStopped,
	}
	if retained {
		p := writeRetained(f.t, dir, id)
		r.TranscriptPath = &p
	}
	r, err := f.st.CreateRun(f.t.Context(), r)
	if err != nil {
		f.t.Fatalf("CreateRun: %v", err)
	}
	return r
}

// writeRetained creates <dir>/<name>/t.jsonl and returns its path.
func writeRetained(t *testing.T, dir, name string) string {
	t.Helper()
	runDir := filepath.Join(dir, name)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(runDir, "t.jsonl")
	if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *recFixture) transcriptPath(id string) *string {
	f.t.Helper()
	r, err := f.st.RunByID(f.t.Context(), id)
	if err != nil {
		f.t.Fatalf("RunByID: %v", err)
	}
	return r.TranscriptPath
}

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return !errors.Is(err, fs.ErrNotExist)
}

// The by-row pass: a run past the (default 30-day) window loses its dir and
// its path; a run inside it keeps both. Expiry keys on ended_at alone — the
// expired run's file is brand new on disk.
func TestExpireTranscripts_window(t *testing.T) {
	f := newRecFixture(t)
	dir := f.withTranscripts()
	expired := f.endedRun(dir, recClock.Add(-31*24*time.Hour), true)
	kept := f.endedRun(dir, recClock.Add(-29*24*time.Hour), true)

	if err := f.svc.expireTranscripts(t.Context()); err != nil {
		t.Fatalf("expireTranscripts: %v", err)
	}

	if pathExists(filepath.Join(dir, expired.ID)) {
		t.Error("expired run's transcript dir survived")
	}
	if p := f.transcriptPath(expired.ID); p != nil {
		t.Errorf("expired run's transcript path = %q, want NULL", *p)
	}
	if !pathExists(*kept.TranscriptPath) {
		t.Error("in-window run's retained transcript was removed")
	}
	if p := f.transcriptPath(kept.ID); p == nil || *p != *kept.TranscriptPath {
		t.Errorf("in-window run's transcript path = %v, want %q", p, *kept.TranscriptPath)
	}
}

// Setting 0 is the off switch: every ended run's retained copy goes at the
// next sweep, however recently it ended — through StartupReconcile, which
// carries the step.
func TestExpireTranscripts_zeroExpiresEveryEndedRun(t *testing.T) {
	f := newRecFixture(t)
	dir := f.withTranscripts()
	if err := f.st.SetSetting(t.Context(), store.SettingTranscriptRetentionDays, "0"); err != nil {
		t.Fatal(err)
	}
	justEnded := f.endedRun(dir, recClock.Add(-time.Minute), true)

	if err := f.svc.StartupReconcile(t.Context()); err != nil {
		t.Fatalf("StartupReconcile: %v", err)
	}

	if pathExists(filepath.Join(dir, justEnded.ID)) {
		t.Error("retained transcript survived a 0-day window")
	}
	if p := f.transcriptPath(justEnded.ID); p != nil {
		t.Errorf("transcript path = %q, want NULL", *p)
	}
}

// A row whose path points OUTSIDE the transcripts dir (a pre-#81 run naming a
// file in its long-wiped HOME) has the stale path nulled, and the file it
// names is never touched.
func TestExpireTranscripts_pathOutsideDirOnlyNulled(t *testing.T) {
	f := newRecFixture(t)
	dir := f.withTranscripts()
	outside := writeRetained(t, t.TempDir(), "home")
	run := f.endedRun(dir, recClock.Add(-40*24*time.Hour), false)
	if err := f.st.UpdateRunTranscriptPath(t.Context(), run.ID, outside); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.expireTranscripts(t.Context()); err != nil {
		t.Fatalf("expireTranscripts: %v", err)
	}

	if p := f.transcriptPath(run.ID); p != nil {
		t.Errorf("transcript path = %q, want NULL", *p)
	}
	if !pathExists(outside) {
		t.Error("expiry removed a file outside the transcripts dir")
	}
}

// The orphan guard: a run-shaped dir with no run row is removed; a
// non-run-shaped dir, a plain file, and the dir of a run whose row exists
// (even with a NULL path — a retain in flight looks exactly like that) are
// never touched. Through RuntimeSweep, which carries the step.
func TestExpireTranscripts_orphanGuard(t *testing.T) {
	f := newRecFixture(t)
	dir := f.withTranscripts()
	orphan := ids.NewID("run")
	writeRetained(t, dir, orphan)
	stray := writeRetained(t, dir, "lost+found")
	plain := filepath.Join(dir, ids.NewID("run"))
	if err := os.WriteFile(plain, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	inFlight := f.endedRun(dir, recClock.Add(-time.Minute), false)
	inFlightFile := writeRetained(t, dir, inFlight.ID)

	f.svc.RuntimeSweep(t.Context())

	if pathExists(filepath.Join(dir, orphan)) {
		t.Error("orphaned run-shaped transcript dir survived")
	}
	for _, p := range []string{stray, plain, inFlightFile} {
		if !pathExists(p) {
			t.Errorf("%s was removed; the orphan guard must never touch it", p)
		}
	}
}

// Nothing retained yet: a missing transcripts dir is not an error, and an
// unset dir disables the step entirely.
func TestExpireTranscripts_missingOrUnsetDir(t *testing.T) {
	f := newRecFixture(t)
	f.withTranscripts()
	if err := f.svc.expireTranscripts(t.Context()); err != nil {
		t.Errorf("expireTranscripts on a missing dir: %v", err)
	}

	f.svc.transcriptsDir = ""
	run := f.endedRun(t.TempDir(), recClock.Add(-400*24*time.Hour), true)
	if err := f.svc.expireTranscripts(t.Context()); err != nil {
		t.Errorf("expireTranscripts with the step off: %v", err)
	}
	if p := f.transcriptPath(run.ID); p == nil {
		t.Error("transcript path nulled with the step off")
	}
}
