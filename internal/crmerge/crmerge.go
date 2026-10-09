// Package crmerge is the shared change-request merge orchestration (ADR-0011),
// lifted out of the operator HTTP handler so BOTH surfaces that land a
// built-in CR call ONE implementation: the operator route
// (POST /api/v1/repos/{id}/crs/{n}/merge) and the agent seam (labctl pr merge
// → the built-in tracker's MergePull). Reusing the service — rather than
// reimplementing it agent-side — is what keeps the ADR-0011 invariants intact
// on both paths: merge and close serialize per CR under a keyed mutex; the git
// window is cancellation-immune once opened (a dropped connection after the
// push must not strand a merged origin unrecorded); every `Closes #N` built-in
// issue is best-effort closed; cr.changed/issue.changed publish on the bus;
// and a re-merge of an already-merged head converges (gitx.CRMerge is a no-op
// on it, and the store's open-state guard is the real double-merge protection).
//
// With tracker.MergeOptions.DeleteHead the service also deletes the CR's head
// ref on ORIGIN once the merge is durably recorded (ADR-0081, issue #90) — a
// best-effort push-delete whose outcome is reported, never an error; the
// LOCAL head branch is left to guarded teardown and the sweep (pass B GCs it
// once the CR leaves open), exactly as before.
//
// The service is built-in-only by construction — a forge-bound repo's PRs are
// merged on the forge (the forgejo/github tracker bindings own that path). It
// speaks CR NUMBERS, loads the repo/CR itself, and returns the merged store.CR
// so the operator route renders it and the built-in tracker maps it to a
// PullRef.
package crmerge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// Service is the built-in tracker's CR-merge seam (tracker.CRMerger): the
// registry hands it to builtin.New, whose MergePull routes here.
var _ tracker.CRMerger = (*Service)(nil)

// Event names published on a CR mutation — the same wire strings the operator
// API and agent API use ({type, repoID} envelope), so any client already
// refetching on cr.changed/issue.changed keeps working whichever surface
// drove the merge. EventRunChanged mirrors reconcile.EventRunChanged /
// instance.EventRunChanged (issue #149): a merge that advances the repo's
// base branch makes every run forked off it more behind, so the SPA's
// commits_behind badges need the same refetch signal a run mutation gives
// them.
const (
	EventCRChanged    = "cr.changed"
	EventIssueChanged = "issue.changed"
	EventRunChanged   = "run.changed"
)

// NoAuthorIdentityMessage answers a merge attempted before any git author
// identity exists. The merge commit is authored with the repo's configured
// REAL identity (D15 measure 5), never a fallback bot — so with neither the
// repo fields nor the global settings set there is nothing lab may author as;
// refusing up front beats an opaque git failure halfway through the merge.
const NoAuthorIdentityMessage = "git author identity is not configured; set git_author_name and git_author_email in settings (or on the repository)"

// ErrNoAuthorIdentity marks that refusal (message: NoAuthorIdentityMessage).
// Both surfaces map it to a 409 configuration conflict.
var ErrNoAuthorIdentity = errors.New(NoAuthorIdentityMessage)

// Config is everything the service needs. Store, Git, Bus and ReposDir are
// required; Vault/Materializer may be nil for repos with no credential (a
// filesystem/local origin needs none — see credentialEnv); GitEnv is the
// per-subprocess env prefix (nil in production, hermetic in tests); Now
// defaults to time.Now; Logger to slog.Default.
type Config struct {
	Store        *store.Store
	Git          *gitx.Engine
	Vault        *vault.Vault
	Materializer *vault.Materializer
	Bus          *events.Bus
	ReposDir     string
	GitEnv       []string
	Now          func() time.Time
	Logger       *slog.Logger
}

// Service orchestrates built-in CR merge and close under a per-CR mutex.
type Service struct {
	store    *store.Store
	git      *gitx.Engine
	vault    *vault.Vault
	mat      *vault.Materializer
	bus      *events.Bus
	reposDir string
	gitEnv   []string
	now      func() time.Time
	log      *slog.Logger

	// mu serializes merge and close per change request: the open-state check
	// and the (seconds-wide, network-bound) git merge must not race a
	// concurrent close, or origin ends up merged while the CR row reads
	// closed-unmerged with no recovery path. Keyed by CR id; entries are
	// never evicted (a CR id count is bounded by real CRs, bytes are trivial).
	mu keyedMutex
}

// New builds a Service from cfg, defaulting Now and Logger.
func New(cfg Config) *Service {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store:    cfg.Store,
		git:      cfg.Git,
		vault:    cfg.Vault,
		mat:      cfg.Materializer,
		bus:      cfg.Bus,
		reposDir: cfg.ReposDir,
		gitEnv:   cfg.GitEnv,
		now:      now,
		log:      logger,
	}
}

// Merge lands the repo's open change request `number` on its base branch and
// returns the merged CR plus the head outcome. The pinned contract both
// callers rely on:
//
//   - unknown repo or CR number → store.ErrNotFound (nothing is done).
//   - CR not open → store.ErrCRNotOpen wrapped with the actual state, and NO
//     git runs. The caller decides whether an already-merged CR is a
//     convergent success (the agent seam re-reads and returns it) or a
//     conflict (the operator route answers 409).
//   - no git author identity → ErrNoAuthorIdentity, before any git runs.
//   - git refuses the merge (gitx.ErrHeadMissing / ErrPushRejected /
//     ErrMergeConflict) → that typed error, verbatim; nothing is recorded, so
//     the CR stays open and a retry converges.
//   - success → the merged CR (state merged, merge_commit/merged_at stamped,
//     every Closes #N issue best-effort closed, cr.changed/issue.changed/
//     run.changed published) and the head outcome (below).
//
// Every error return carries the zero HeadResult: nothing was merged, so no
// head delete was attempted.
//
// Head delete (ADR-0081): only AFTER store.MergeCR has recorded the row —
// before that, the head is the retry path of an unrecorded merge (ADR-0024)
// — and only with opts.DeleteHead, Merge runs a best-effort
// `git push origin :refs/heads/<head>` from the bare clone with the SAME
// materialized credential as the merge (its cleanup is deferred to Merge's
// return, so it is still on disk). The per-CR mutex is released first: a
// concurrent close is refused on state by then, so the network round-trip
// need not hold it. Outcomes: tracker.HeadDeleted when the ref is absent on
// origin afterwards (deleted now, or never pushed — gitx.DeleteRemoteBranch
// asks origin rather than parsing git's prose); tracker.HeadFailed with
// git's own words on a refusal or network failure, logged at warn — the
// merge is the irreversible part and STAYS a success; tracker.HeadKept with
// tracker.HeadKeptSettingOff without the option. The local head branch is
// never touched here.
//
// Everything from the decision to merge onward runs on a cancellation-immune
// context: a caller (a phone, a labctl process) dropping the connection after
// the push but before the bookkeeping must not strand a pushed merge
// unrecorded or skip gitx's refresh fetch. The head delete runs on that same
// context.
func (s *Service) Merge(ctx context.Context, repoID string, number int, opts tracker.MergeOptions) (store.CR, tracker.HeadResult, error) {
	repo, err := s.store.RepoByID(ctx, repoID)
	if err != nil {
		return store.CR{}, tracker.HeadResult{}, err
	}
	cr, err := s.store.CRByRepoNumber(ctx, repoID, number)
	if err != nil {
		return store.CR{}, tracker.HeadResult{}, err
	}

	// Serialize against a concurrent close/merge of the SAME CR before the
	// seconds-wide git window opens (ADR-0011). Released early once the row
	// is recorded (the head delete does not need it); the deferred call is
	// the error paths' release, and OnceFunc makes the double call safe.
	unlock := sync.OnceFunc(s.mu.lock(cr.ID))
	defer unlock()

	// Not abortable once we commit to it (see the doc comment).
	ctx = context.WithoutCancel(ctx)

	// Re-read under the lock so a mutation that won the lock first is seen.
	cr, err = s.store.CRByRepoNumber(ctx, repoID, number)
	if err != nil {
		return store.CR{}, tracker.HeadResult{}, err
	}
	if cr.State != store.CRStateOpen {
		return store.CR{}, tracker.HeadResult{}, fmt.Errorf("%w (state %q)", store.ErrCRNotOpen, cr.State)
	}

	name, email, err := s.authorIdentity(ctx, repo)
	if err != nil {
		return store.CR{}, tracker.HeadResult{}, err
	}
	if name == "" || email == "" {
		return store.CR{}, tracker.HeadResult{}, ErrNoAuthorIdentity
	}

	// The merge's two fetches report their outcome to the readiness recorder
	// (issue #61), attributed to the credential version read here — BEFORE
	// the row is materialized, so a rotation landing in between makes the
	// record stale, never wrong.
	fetchCtx := gitx.AttributeFetch(ctx, gitx.FetchAttribution{
		RepoID: repo.ID, Credential: s.store.CredentialStampByID(ctx, repo.CredentialID),
	})
	credEnv, cleanup, err := s.credentialEnv(ctx, repo, "crmerge-"+cr.ID)
	if err != nil {
		return store.CR{}, tracker.HeadResult{}, err
	}
	// Deferred to Merge's return — AFTER the head delete below, which pushes
	// with this same credential.
	defer cleanup()

	env := append(append([]string{}, s.gitEnv...), credEnv...)
	message := fmt.Sprintf("Merge change request #%d: %s", cr.Number, cr.Title)
	mergeCommit, err := s.git.CRMerge(fetchCtx, s.bareDir(repoID),
		cr.BaseBranch, cr.HeadBranch, message, name, email, env)
	if err != nil {
		// gitx's typed refusals (head missing, push rejected, merge conflict)
		// pass through verbatim; the callers map them to a 409 whose body is
		// the backend's own words. Nothing is recorded — the CR stays open.
		return store.CR{}, tracker.HeadResult{}, err
	}

	// The merge is on origin. Record it, close the closes-issues, publish. If
	// a concurrent merge won the store race (store.ErrCRNotOpen), git has
	// already converged and the error tells the caller the truth.
	merged, err := s.store.MergeCR(ctx, repoID, cr.Number, mergeCommit, s.now())
	if err != nil {
		return store.CR{}, tracker.HeadResult{}, err
	}
	// Recorded: the CR is no longer open, so a concurrent close/merge is
	// refused on state — the rest needs no serialization.
	unlock()

	closedAny := false
	for _, n := range merged.Closes {
		if _, err := s.store.UpdateIssue(ctx, repoID, n,
			store.IssueUpdate{State: store.Set(store.IssueStateClosed)}, s.now()); err != nil {
			s.log.Warn("closing issue for merged change request",
				"component", "crmerge", "repo", repoID, "cr", merged.Number, "issue", n, "err", err)
			continue
		}
		closedAny = true
	}
	if closedAny {
		s.publish(EventIssueChanged, repoID)
	}
	s.publish(EventCRChanged, repoID)
	// Every run forked off this base just became more behind (issue #149):
	// the SPA refetches instances/run detail on run.changed to refresh their
	// commits_behind badges instantly, without waiting for the next fetch
	// cadence.
	s.publish(EventRunChanged, repoID)

	// The merge is durably recorded — only now may the head leave origin
	// (ADR-0081). After the publishes, so the SPA's refetch never waits on
	// the delete's network round-trip.
	head := tracker.HeadResult{Outcome: tracker.HeadKept, Reason: tracker.HeadKeptSettingOff}
	if opts.DeleteHead {
		head = s.deleteHead(ctx, repo, merged.Number, merged.HeadBranch, merged.BaseBranch, env)
	}
	return merged, head, nil
}

// DeleteHead runs Merge's best-effort origin head delete on its own, for a CR
// that is ALREADY merged — the built-in tracker's convergent re-merge path
// (MergePull on a merged CR), so a retried `labctl pr merge` reports where
// the head truthfully stands: an earlier delete that succeeded reads
// HeadDeleted again (the ref is absent), one that failed is retried. It
// loads the repo and materializes its credential for this one op (a fresh op
// id — there is no per-CR mutex to make "crmerge-<cr>" unique here). Never an
// error: a repo/credential failure is HeadFailed with the reason, logged at
// warn like a refused push. The local branch is untouched. Runs on a
// cancellation-immune context like Merge's tail.
func (s *Service) DeleteHead(ctx context.Context, repoID, head string) tracker.HeadResult {
	ctx = context.WithoutCancel(ctx)
	repo, err := s.store.RepoByID(ctx, repoID)
	if err != nil {
		return s.headFailed(repoID, 0, head, err)
	}
	credEnv, cleanup, err := s.credentialEnv(ctx, repo, ids.NewID("crhead"))
	if err != nil {
		return s.headFailed(repoID, 0, head, err)
	}
	defer cleanup()
	env := append(append([]string{}, s.gitEnv...), credEnv...)
	return s.deleteHead(ctx, repo, 0, head, repo.DefaultBranch, env)
}

// deleteHead is the shared best-effort origin delete behind Merge and
// DeleteHead. crNumber is 0 when unknown (DeleteHead) and only labels the
// log line. Built-in has no forks, so the only guard is a belt-and-braces
// refusal to delete the base/default branch itself — a CR's head is a
// managed run branch in practice, but deleting origin's mainline over a
// malformed row would be unrecoverable. Everything else defers to
// gitx.DeleteRemoteBranch's "absent afterwards" contract.
func (s *Service) deleteHead(ctx context.Context, repo store.Repo, crNumber int, head, base string, env []string) tracker.HeadResult {
	if head == "" || head == base || head == repo.DefaultBranch {
		reason := fmt.Sprintf("head %q is the base branch", head)
		s.log.Warn("keeping change request head on origin",
			"component", "crmerge", "repo", repo.ID, "cr", crNumber, "head", head, "reason", reason)
		return tracker.HeadResult{Outcome: tracker.HeadKept, Reason: reason}
	}
	if err := s.git.DeleteRemoteBranch(ctx, s.bareDir(repo.ID), head, env); err != nil {
		return s.headFailed(repo.ID, crNumber, head, err)
	}
	s.log.Info("deleted change request head on origin",
		"component", "crmerge", "repo", repo.ID, "cr", crNumber, "head", head)
	return tracker.HeadResult{Outcome: tracker.HeadDeleted}
}

// headFailed logs a failed origin head delete at warn and returns its
// HeadFailed outcome carrying the error's own words.
func (s *Service) headFailed(repoID string, crNumber int, head string, err error) tracker.HeadResult {
	s.log.Warn("deleting change request head on origin failed; the merge stands",
		"component", "crmerge", "repo", repoID, "cr", crNumber, "head", head, "err", err)
	return tracker.HeadResult{Outcome: tracker.HeadFailed, Reason: err.Error()}
}

// Close transitions the repo's open change request `number` to closed
// (closed-unmerged: the head branch and its parked work are untouched; a
// closed CR reads as "no PR" to the reaper). It shares Merge's per-CR mutex
// so a close never lands inside a concurrent merge's git window.
func (s *Service) Close(ctx context.Context, repoID string, number int) (store.CR, error) {
	target, err := s.store.CRByRepoNumber(ctx, repoID, number)
	if err != nil {
		return store.CR{}, err
	}
	unlock := s.mu.lock(target.ID)
	defer unlock()

	cr, err := s.store.CloseCR(ctx, repoID, number, s.now())
	if err != nil {
		return store.CR{}, err
	}
	s.publish(EventCRChanged, repoID)
	return cr, nil
}

// bareDir is the repo's bare reference clone (design §7) — the same derivation
// as reposvc.bareDir / the old httpapi crBareDir, on the ReposDir given.
func (s *Service) bareDir(repoID string) string {
	return filepath.Join(s.reposDir, repoID+".git")
}

// authorIdentity resolves the merge commit's author/committer identity: the
// repo's git_author_name/email override the global settings pair,
// field-by-field (the same layering as the spawned-session authorEnv in
// internal/instance). Empty results mean "not configured" — the caller
// refuses the merge rather than authoring as nobody or as a bot.
func (s *Service) authorIdentity(ctx context.Context, repo store.Repo) (name, email string, err error) {
	if repo.GitAuthorName != nil {
		name = strings.TrimSpace(*repo.GitAuthorName)
	}
	if repo.GitAuthorEmail != nil {
		email = strings.TrimSpace(*repo.GitAuthorEmail)
	}
	if name == "" {
		if name, err = s.store.GetString(ctx, store.SettingGitAuthorName, ""); err != nil {
			return "", "", err
		}
	}
	if email == "" {
		if email, err = s.store.GetString(ctx, store.SettingGitAuthorEmail, ""); err != nil {
			return "", "", err
		}
	}
	// Whitespace-only values are "not configured": git strips whitespace
	// idents and rejects the commit with an opaque "empty ident name".
	return strings.TrimSpace(name), strings.TrimSpace(email), nil
}

// credentialEnv materializes the repo's GIT credential for one merge op
// (design §6) and returns the git env entries plus a cleanup removing exactly
// this op's files. A repo without a credential yields no env and a no-op
// cleanup (a filesystem/local remote needs none). Mirrors the per-service
// credentialEnv helpers in reposvc and instance.
func (s *Service) credentialEnv(ctx context.Context, repo store.Repo, opID string) (env []string, cleanup func(), err error) {
	noop := func() {}
	if repo.CredentialID == nil {
		return nil, noop, nil
	}
	if s.vault == nil || s.mat == nil {
		return nil, noop, errors.New("credentialed merge without a vault/materializer wired")
	}
	cred, err := s.store.CredentialByID(ctx, *repo.CredentialID)
	if err != nil {
		return nil, noop, err
	}
	remove := func() {
		if err := s.mat.Cleanup(cred.ID, opID); err != nil {
			s.log.Warn("cleaning materialized credential", "component", "crmerge", "err", err)
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
		// forge_token never authenticates git (design §3a).
		return nil, noop, fmt.Errorf("credential kind %s cannot authenticate git", cred.Kind)
	}
}

// publish emits a repo-scoped event on the bus. A nil bus (some tests) is a
// no-op.
func (s *Service) publish(eventType, repoID string) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(events.Event{Type: eventType, Payload: struct {
		Type   string `json:"type"`
		RepoID string `json:"repoID"`
	}{Type: eventType, RepoID: repoID}})
}

// keyedMutex is a per-key mutual exclusion helper: lock(key) blocks while
// another holder owns the same key and returns the unlock func. Entries are
// tiny and never evicted — key cardinality is bounded by real CR ids.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (k *keyedMutex) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*sync.Mutex{}
	}
	m, ok := k.locks[key]
	if !ok {
		m = &sync.Mutex{}
		k.locks[key] = m
	}
	k.mu.Unlock()
	m.Lock()
	return m.Unlock
}
