package instance

// retain_transcript.go is the write half of transcript retention (issue #81).
// Since issue #202 a run's provider-native transcript lives inside its
// private HOME, and every teardown path wipes that whole tree — so without
// this step every ended run's chat reads "Transcript no longer available", by
// construction. RetainTranscript is the pre-wipe hook body that moves the
// file out first; cmd/lab composes it into instancehome's pre-wipe chain
// AFTER the issue #222 adopt-check, which makes it total across all six wipe
// sites (Stop, AFK stop, the reaper, the parked discard, launch rollback and
// the orphan sweeps — the startup one after downtime included) with no
// per-site code. The read side (chat.Service.Read on an ended run) needs no
// change: it reads whatever runs.transcript_path names. The other half —
// expiring a retained copy once the window closes — is reconcile's
// (internal/reconcile/transcripts.go).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/instancehome"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// transcriptRetainTimeout bounds one retain step — the store reads, the
// provider's move, and the path write together. It runs synchronously inside
// the pre-wipe chain, ahead of a Stop's or a sweep's RemoveAll, so it must not
// hold a teardown hostage; but a same-filesystem rename is instant and only
// the cross-device copy fallback of a long session's jsonl takes a moment,
// so thirty seconds is generous without being unbounded. A retain it cuts
// short is one warning; the wipe proceeds.
const transcriptRetainTimeout = 30 * time.Second

// RetainTranscript moves runID's transcript out of its instance HOME into
// <TranscriptsDir>/<runID>/ and repoints runs.transcript_path at the retained
// copy (issue #81 decision 5). It is the pre-wipe hook body: cmd/lab adds it
// to instancehome's chain, so it runs synchronously immediately before the
// run's tree is removed, and the tree — HOME included — is still on disk.
//
// It NEVER blocks or fails the wipe: it returns nothing, and every failure is
// one warning (component, run, err) and a return — the wipe that called it
// happens regardless, taking the unretained transcript with it, which is
// exactly the pre-#81 behaviour. In order:
//
//   - runID not run-shaped (instancehome.IsRunID) → no-op. It becomes a path
//     under TranscriptsDir, and reconcile's expiry only ever removes
//     run-shaped dirs there, so a dir this step created for any other name
//     would never be expired.
//   - TranscriptsDir "" → no-op (retention not wired).
//   - No run row (store.ErrNotFound) → no-op, silently: that is what a launch
//     rollback (row already deleted) and a true orphan tree look like.
//   - No transcript path on the row → no-op: nothing was ever located.
//   - transcript_retention_days 0 → no-op (the off switch). An invalid
//     hand-edited value warns and proceeds on the default; a store failure
//     warns and skips. A run whose ended_at is already past the window — an
//     orphan reaped by the startup sweep after long downtime — is skipped:
//     the next expiry sweep would remove the copy anyway (and nulls the
//     stale path).
//   - The run's provider unknown to the registry → warn, return.
//   - <TranscriptsDir> and <TranscriptsDir>/<runID> are created 0700
//     (tightened if they already existed looser — the transcript may hold
//     secret values the agent saw, issue #108), then the provider's
//     RetainTranscript moves the file in. An error → warn, remove the dest
//     dir if still empty; "" (nothing to keep) → remove the dest dir, leave
//     the row untouched.
//   - The returned path is persisted through UpdateRunTranscriptPath. If
//     that write fails the copy is removed again: expiry keys on the row, so
//     a copy no row points at would leak past every window until the orphan
//     guard happened on it — and only once the row itself were gone.
//
// Like RevokeBastionKey it runs on context.WithoutCancel(ctx), bounded by
// transcriptRetainTimeout: a wipe during shutdown (the process signal context
// already cancelled) must not strand the transcript the run is about to lose.
func (s *Service) RetainTranscript(ctx context.Context, runID string) {
	if !instancehome.IsRunID(runID) || s.transcriptsDir == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), transcriptRetainTimeout)
	defer cancel()

	run, err := s.store.RunByID(ctx, runID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("retaining the run's transcript: load run; the wipe proceeds without it",
				"component", "instance", "run", runID, "err", err)
		}
		return
	}
	if run.TranscriptPath == nil || *run.TranscriptPath == "" {
		return
	}
	dest := filepath.Join(s.transcriptsDir, runID)
	if strings.HasPrefix(*run.TranscriptPath, dest+string(filepath.Separator)) {
		return // already retained — the tree outlived one wipe; nothing left in HOME to move
	}

	days, err := s.store.TranscriptRetentionDays(ctx)
	if err != nil {
		if !errors.Is(err, store.ErrInvalidSetting) {
			s.log.Warn("retaining the run's transcript: read the retention window; the wipe proceeds without it",
				"component", "instance", "run", runID, "err", err)
			return
		}
		s.log.Warn("retaining the run's transcript: invalid retention window, using the default",
			"component", "instance", "run", runID, "days", days, "err", err)
	}
	if days == 0 {
		return
	}
	if run.EndedAt != nil && run.EndedAt.Before(s.now().Add(-time.Duration(days)*24*time.Hour)) {
		return
	}

	prov, ok := s.providers.Get(run.Provider)
	if !ok {
		s.log.Warn("retaining the run's transcript: unknown provider; the wipe proceeds without it",
			"component", "instance", "run", runID, "provider", run.Provider, "err", errors.New("provider not registered"))
		return
	}

	for _, dir := range []string{s.transcriptsDir, dest} {
		if err := mkdirTight(dir); err != nil {
			s.log.Warn("retaining the run's transcript: create the retention dir; the wipe proceeds without it",
				"component", "instance", "run", runID, "dir", dir, "err", err)
			return
		}
	}

	retained, err := prov.RetainTranscript(ctx, run.WorktreePath, s.homes.HomePath(runID), *run.TranscriptPath, dest)
	if err != nil {
		// Best-effort, and only while still empty: provider.RetainFile removes
		// a partial copy itself, so a failed retain leaves dest as created.
		_ = os.Remove(dest)
		s.log.Warn("retaining the run's transcript failed; the wipe proceeds without it",
			"component", "instance", "run", runID, "provider", run.Provider, "err", err)
		return
	}
	if retained == "" {
		_ = os.Remove(dest)
		return
	}
	if err := s.store.UpdateRunTranscriptPath(ctx, runID, retained); err != nil {
		_ = os.RemoveAll(dest)
		s.log.Warn("retaining the run's transcript: persist the retained path; copy removed",
			"component", "instance", "run", runID, "err", err)
		return
	}
	s.log.Info("retained the run's transcript", "component", "instance", "run", runID, "path", retained, "days", days)
}

// mkdirTight creates dir (and any missing parents) and ensures it ends up
// exactly mode 0700 even if it already existed looser — instancehome's
// helper of the same name, re-implemented rather than exported (MkdirAll
// alone leaves an existing directory's mode untouched).
func mkdirTight(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}
