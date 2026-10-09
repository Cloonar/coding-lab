package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/instancehome"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// expireTranscripts is the expiry half of transcript retention (issue #81
// decision 6): the instance service's pre-wipe retain step moves an ending
// run's transcript into <TranscriptsDir>/<runID>/, and this step removes it
// again once the transcript_retention_days window past the run's ended_at has
// closed — bounding how long a copy that may carry secret values (issue #108)
// outlives its run. It runs in StartupReconcile and the throttled
// RuntimeSweep, each time right AFTER the instance-home sweep, so an orphan
// tree reaped there is retained first and the window then applies to it.
//
// Two passes, both best-effort:
//
//  1. By row. Every ended run that still carries a transcript path and ended
//     before now − days (store.EndedRunsWithTranscriptBefore) loses
//     <TranscriptsDir>/<runID> and then its path, nulled through the same
//     setter the retain step wrote it with — so chat shows the existing "no
//     longer available" state. Expiry keys on ended_at, NEVER on the file's
//     mtime, so shortening the window applies retroactively at the next sweep
//     (intended), and days == 0 expires every ended run's copy. A failed
//     removal keeps the path, so the next sweep retries the pair. Only
//     <TranscriptsDir>/<runID> is ever removed: a row whose path points
//     elsewhere — a pre-#81 run naming a file in its long-wiped HOME, or a
//     retain that was skipped — merely has that stale path nulled, which is
//     correct and harmless; the file it names is never touched.
//  2. Orphan guard. Every run-shaped directory (instancehome.IsRunID) under
//     TranscriptsDir whose run row no longer exists (store.ErrNotFound) is
//     removed — a repo or run deleted after its transcript was retained.
//     Nothing else is touched: non-run-shaped entries, plain files, and the
//     dir of any run whose row exists — even one whose path is NULL, because
//     the retain step creates the dir BEFORE persisting the path, and
//     removing it in that window would race a retain in flight.
//
// A missing TranscriptsDir is normal (nothing retained yet). The step is off
// when TranscriptsDir is "". A store failure reading the window returns that
// error (skip this pass rather than act on a guessed window); an invalid
// hand-edited window warns and proceeds on the default the store returns with
// it — a stalled expiry would keep secret-bearing copies past their window.
// Per-entry failures never abort the pass; they are joined and returned for
// the caller to log, like instancehome.SweepAll's.
func (s *Service) expireTranscripts(ctx context.Context) error {
	if s.transcriptsDir == "" {
		return nil
	}
	days, err := s.store.TranscriptRetentionDays(ctx)
	if err != nil {
		if !errors.Is(err, store.ErrInvalidSetting) {
			return fmt.Errorf("retention window: %w", err)
		}
		s.log.Warn("transcript expiry: invalid retention window, using the default",
			"component", "reconcile", "days", days, "err", err)
	}

	var errs []error
	cutoff := s.now().Add(-time.Duration(days) * 24 * time.Hour)
	expired, err := s.store.EndedRunsWithTranscriptBefore(ctx, cutoff)
	if err != nil {
		errs = append(errs, err)
	}
	for _, run := range expired {
		if instancehome.IsRunID(run.ID) {
			if err := os.RemoveAll(filepath.Join(s.transcriptsDir, run.ID)); err != nil {
				errs = append(errs, fmt.Errorf("expire transcript of run %s: %w", run.ID, err))
				continue // keep the path: the next sweep retries the pair
			}
		}
		if err := s.store.UpdateRunTranscriptPath(ctx, run.ID, ""); err != nil {
			errs = append(errs, fmt.Errorf("expire transcript of run %s: %w", run.ID, err))
		}
	}

	entries, err := os.ReadDir(s.transcriptsDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("transcripts dir: %w", err))
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !instancehome.IsRunID(name) {
			continue
		}
		_, err := s.store.RunByID(ctx, name)
		switch {
		case err == nil:
			continue
		case !errors.Is(err, store.ErrNotFound):
			errs = append(errs, err)
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.transcriptsDir, name)); err != nil {
			errs = append(errs, fmt.Errorf("remove orphaned transcript dir %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
