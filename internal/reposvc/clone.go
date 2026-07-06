package reposvc

// The async clone job (brief M2: add-repo flow). One goroutine per repo,
// single-flight per repo id: materialize the GIT credential → CloneBare with
// progress forwarded to the bus (gitx throttles to ~4/s) → on success detect
// the default branch and mark ready; on failure record a bounded stderr tail
// as clone_error (never credential bytes — the vault materializes secrets
// into files, so neither argv nor stderr ever carries them) and clean up the
// partial dir (CloneBare) and the materialized files (deferred Cleanup).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// maxCloneErrorLen bounds repos.clone_error. gitx already tails stderr to
// 40 lines; this is a final byte-level bound. The tail is kept because
// git's fatal lines sit at the end.
const maxCloneErrorLen = 4096

// storeWriteTimeout bounds the job's own store writes: the job context is
// cancelled by force-delete, so completion writes use a fresh context that
// must not hang forever either.
const storeWriteTimeout = 10 * time.Second

type cloneJob struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startCloneJob registers the repo's clone job synchronously — Add/Retry
// return only after the job is visible in the registry, so a Delete racing
// right behind the creating request always sees it — and runs the clone in
// a fresh goroutine. A second call while a job is registered is a no-op.
func (s *Service) startCloneJob(repo store.Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startCloneJobLocked(repo)
}

// startCloneJobLocked is startCloneJob for callers already holding s.mu —
// Retry folds the registration into the same critical section as its
// single-flight check and status write, so no second Retry can interleave.
func (s *Service) startCloneJobLocked(repo store.Repo) {
	if _, running := s.jobs[repo.ID]; running {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &cloneJob{cancel: cancel, done: make(chan struct{})}
	s.jobs[repo.ID] = job
	s.metrics.CloneStarted() // paired with the CloneFinished defer in runClone
	go s.runClone(ctx, job, repo)
}

// runClone drives one clone job to a terminal clone_status. Ordering is
// load-bearing: the final status write happens BEFORE the job leaves the
// registry, so anyone observing "no job running" also observes the final
// status (Retry relies on this); job.done closes last, so a force-delete
// that awaited it knows the git process is gone before removing the dir.
func (s *Service) runClone(ctx context.Context, job *cloneJob, repo store.Repo) {
	defer close(job.done)
	defer func() {
		s.mu.Lock()
		delete(s.jobs, repo.ID)
		s.mu.Unlock()
	}()
	defer job.cancel()
	defer s.metrics.CloneFinished() // in-flight gauge: one Dec per started job

	err := s.clone(ctx, repo)
	result, counted := cloneOutcome(err, ctx.Err())
	if !counted {
		// Cancelled by a force-delete — checked BEFORE the success branch,
		// because the cancel can land after the clone already succeeded and
		// that job must not count as ready for a repo that is going away.
		// The row is gone (or about to be) and CloneBare removed any
		// partial dir: write nothing, count no lab_clone_jobs_total result.
		return
	}
	s.metrics.CloneJob(result)
	if err == nil {
		return
	}

	s.log.Warn("clone failed", "component", "reposvc", "repo", repo.ID, "err", err)
	wctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if uerr := s.store.UpdateRepoCloneStatus(wctx, repo.ID, store.CloneStatusError, errorTail(err, maxCloneErrorLen)); uerr != nil {
		if !errors.Is(uerr, store.ErrNotFound) { // NotFound: deleted mid-clone
			s.log.Error("recording clone failure", "component", "reposvc", "repo", repo.ID, "err", uerr)
		}
		return
	}
	s.publishRepoChanged(repo.ID)
}

// cloneOutcome classifies a finished clone job for lab_clone_jobs_total.
// Cancellation (force-delete) counts neither result WHATEVER err says — a
// cancel can land after the clone already succeeded, and a job for a repo
// that is being deleted must not count as ready (the metric Help pins
// "cancelled jobs count neither").
func cloneOutcome(err, ctxErr error) (result string, counted bool) {
	switch {
	case ctxErr != nil:
		return "", false
	case err == nil:
		return store.CloneStatusReady, true
	default:
		return store.CloneStatusError, true
	}
}

// clone materializes the credential, runs the bare clone with progress
// events, and on success records the detected default branch and the ready
// status.
func (s *Service) clone(ctx context.Context, repo store.Repo) error {
	credEnv, cleanup, err := s.credentialEnv(ctx, repo.CredentialID, repo.ID)
	if err != nil {
		return fmt.Errorf("prepare git credential: %w", err)
	}
	defer cleanup() // per-op materialization: files removed at op end (design §6)
	env := append(append([]string{}, s.gitEnv...), credEnv...)

	dest := s.bareDir(repo.ID)
	progress := func(phase string, percent int, line string) {
		s.bus.Publish(events.Event{Type: EventCloneProgress, Payload: cloneProgressPayload{
			Type:    EventCloneProgress,
			RepoID:  repo.ID,
			Phase:   phase,
			Percent: percent,
			Line:    line,
		}})
	}
	if err := s.git.CloneBare(ctx, repo.RemoteURL, dest, env, progress); err != nil {
		return err
	}

	branch := s.git.DefaultBranch(ctx, dest, env)

	wctx, cancel := context.WithTimeout(context.Background(), storeWriteTimeout)
	defer cancel()
	if _, err := s.store.UpdateRepoSettings(wctx, repo.ID, store.RepoSettingsUpdate{
		DefaultBranch: store.Set(branch),
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // force-deleted between clone and completion
		}
		return fmt.Errorf("record default branch: %w", err)
	}

	// Incogni pre-push guard (D15 §9 measure 7): "installed at repo add"
	// lands here — the bare dir only exists once the clone completes. The
	// row is re-read so an incogni PATCH during the clone is honored (an
	// in-flight clone makes the PATCH itself skip the install). Failure
	// fails the clone BEFORE ready: an incogni repo never reaches ready
	// unguarded, and Retry recreates dir + hook together.
	fresh, err := s.store.RepoByID(wctx, repo.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("re-reading repo for hook install: %w", err)
	}
	if fresh.Incogni {
		if err := s.installIncogniHook(wctx, repo.ID); err != nil {
			return fmt.Errorf("installing incogni pre-push hook: %w", err)
		}
	}

	if err := s.store.UpdateRepoCloneStatus(wctx, repo.ID, store.CloneStatusReady, ""); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("record clone success: %w", err)
	}
	s.publishRepoChanged(repo.ID)
	return nil
}

// credentialEnv materializes the repo's GIT credential (design §6) and
// returns the exact env entries for git plus a cleanup removing the
// materialized files. opID keys the materialized filenames (clone ops pass
// the repo ID): concurrent ops sharing one credential each hold their own
// files, and cleanup — the error path included — removes exactly this
// op's files, never another op's live ones. A nil credID yields no env
// and a no-op cleanup. On error nothing of this op is left behind and the
// returned cleanup is a no-op. Error strings never contain payload bytes
// (vault sanitizes decrypt errors).
func (s *Service) credentialEnv(ctx context.Context, credID *string, opID string) (env []string, cleanup func(), err error) {
	noop := func() {}
	if credID == nil {
		return nil, noop, nil
	}
	cred, err := s.store.CredentialByID(ctx, *credID)
	if err != nil {
		return nil, noop, err
	}
	remove := func() {
		if err := s.mat.Cleanup(cred.ID, opID); err != nil {
			s.log.Warn("cleaning materialized credential", "component", "reposvc", "err", err)
		}
	}
	switch cred.Kind {
	case store.CredentialKindSSHKey:
		var p vault.SSHKeyPayload
		if err := s.vault.DecryptPayload(cred.EncryptedPayload, &p); err != nil {
			return nil, noop, err
		}
		keyPath, sshAskpass, err := s.mat.MaterializeSSHKey(cred.ID, opID, p)
		if err != nil {
			remove()
			return nil, noop, err
		}
		if sshAskpass != "" {
			// Passphrase-protected key: wire the materialized SSH askpass
			// helper so a TTY-less ssh can decrypt it (design D9).
			return vault.SSHEnvWithPassphrase(keyPath, s.mat.KnownHostsPath(), sshAskpass), remove, nil
		}
		return vault.SSHEnv(keyPath, s.mat.KnownHostsPath()), remove, nil
	case store.CredentialKindHTTPSToken:
		var p vault.HTTPSTokenPayload
		if err := s.vault.DecryptPayload(cred.EncryptedPayload, &p); err != nil {
			return nil, noop, err
		}
		askpass, err := s.mat.MaterializeAskpass(cred.ID, opID, p)
		if err != nil {
			remove()
			return nil, noop, err
		}
		return vault.HTTPSEnv(askpass), remove, nil
	default:
		// forge_token never authenticates git (design §3a); Add/PATCH
		// enforce this — defense in depth for hand-edited rows.
		return nil, noop, fmt.Errorf("credential kind %s cannot authenticate git", cred.Kind)
	}
}

// errorTail bounds an error message to its last max bytes (git's fatal
// lines sit at the end of the stderr tail).
func errorTail(err error, max int) string {
	msg := err.Error()
	if len(msg) <= max {
		return msg
	}
	return msg[len(msg)-max:]
}
