// Package pull is the /pull-base lab command's git orchestration: the service
// between gitx's pull primitives (PullBase/SummarizeRange) and the chat/HTTP
// layer that intercepts the command and messages the agent. Per pull it
// resolves the run's base branch, serializes against other pulls of the SAME
// run, materializes the repo's git credential, merges the freshly-fetched
// origin/<base> into the run's LIVE worktree under the repo's configured real
// author identity (D15 measure 5), and renders the compact digest the chat
// layer pastes to the agent. It does NOT own command interception, tmux
// messaging, or error presentation — the chat/HTTP layer classifies the
// typed gitx errors (*gitx.ConflictError / gitx.ErrMergeConflict) this
// service passes through verbatim.
package pull

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// EventRunChanged is published after a pull that moved the worktree's HEAD —
// the same wire string the instance/reconcile services publish ({type,
// repoID} envelope), so the behind-badge and runs rail refetch without caring
// which service drove the change. Each service owns its own constant (no
// shared coupling).
const EventRunChanged = "run.changed"

// NoAuthorIdentityMessage answers a DIVERGED pull that has no git author
// identity to commit its merge under. A diverged pull authors a merge commit
// with the repo's configured REAL identity (D15 measure 5), never a fallback
// bot — so with neither the repo fields nor the global settings set there is
// nothing lab may author as, and the merge is refused rather than committing
// as nobody. A fast-forward or up-to-date pull authors no commit, so this
// refusal fires ONLY when that merge commit would actually be authored
// (#151) — and a clear refusal beats git's opaque "empty ident name" failure
// mid-merge.
const NoAuthorIdentityMessage = "git author identity is not configured; set git_author_name and git_author_email in settings (or on the repository)"

// ErrNoAuthorIdentity marks that refusal (message: NoAuthorIdentityMessage).
var ErrNoAuthorIdentity = errors.New(NoAuthorIdentityMessage)

// Digest caps: the newest maxSubjects commit subjects and the first maxFiles
// name-status lines make it into the digest; everything beyond folds into an
// "… and N more" line. The digest is pasted into a tmux composer, so its
// total size must stay far below the paste limit (~100k bytes) — these caps
// bound it to a few KB.
const (
	maxSubjects = 20
	maxFiles    = 50
)

// repoScopedPayload is the SSE envelope run.changed carries — the same shape
// reconcile/instance publish (duplicated per package by design, brief §8.1).
// RunID names the one run the event concerns (issue #175): a pull always
// lands in exactly one run's worktree, so both publish sites carry it.
type repoScopedPayload struct {
	Type   string `json:"type"`
	RepoID string `json:"repoID"`
	RunID  string `json:"runID,omitempty"`
}

// Options is everything the service needs. Store, Git and ReposDir are
// required; Bus may be nil (some tests — publishes become no-ops);
// Vault/Materializer may be nil for repos with no credential (a
// filesystem/local origin needs none — see credentialEnv); GitEnv is the
// per-subprocess env prefix (nil in production, hermetic in tests); Logger
// defaults to slog.Default.
type Options struct {
	Store        *store.Store
	Git          *gitx.Engine
	Vault        *vault.Vault
	Materializer *vault.Materializer
	Bus          *events.Bus
	ReposDir     string
	GitEnv       []string
	Logger       *slog.Logger
}

// Service orchestrates base-branch pulls into live run worktrees under a
// per-run mutex.
type Service struct {
	store    *store.Store
	git      *gitx.Engine
	vault    *vault.Vault
	mat      *vault.Materializer
	bus      *events.Bus
	reposDir string
	gitEnv   []string
	log      *slog.Logger

	// mu serializes pulls per run: two racing /pull-base sends against the
	// same worktree must not interleave their fetch/merge windows (git would
	// refuse the second merge mid-merge, or worse, race the abort). Keyed by
	// run id; pulls of DIFFERENT runs proceed concurrently. Entries are never
	// evicted (run-id cardinality is bounded by real runs, bytes are trivial).
	mu keyedMutex
}

// New builds a Service from o, defaulting Logger.
func New(o Options) *Service {
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store:    o.Store,
		git:      o.Git,
		vault:    o.Vault,
		mat:      o.Materializer,
		bus:      o.Bus,
		reposDir: o.ReposDir,
		gitEnv:   o.GitEnv,
		log:      logger,
	}
}

// Result describes one PullBase outcome for the chat/HTTP layer. UpToDate and
// FastForward mirror gitx.PullResult (when both are false the pull created a
// real merge commit); Digest is the agent-facing text, empty when UpToDate.
type Result struct {
	Base        string // base branch name the pull targeted
	OldHead     string // worktree HEAD before
	NewHead     string // worktree HEAD after (== OldHead when UpToDate)
	UpToDate    bool
	FastForward bool
	Digest      string // agent-facing digest text; empty when UpToDate
}

// PullBase merges the current origin/<base> into the run's live worktree and
// digests what came in. The contract the chat/HTTP layer relies on:
//
//   - unknown repo → store.ErrNotFound (nothing is done).
//   - a DIVERGED pull with no git author identity → ErrNoAuthorIdentity (the
//     merge commit has nobody to author it). Up-to-date and fast-forward
//     pulls author no commit and need no identity (#151); ff-ness is
//     unknowable before the fetch, so this refusal necessarily lands AFTER
//     the fetch — still before the merge touches the worktree.
//   - git refuses the pull (fetch failure, dirty-clobber refusal, or a
//     conflict as *gitx.ConflictError matching gitx.ErrMergeConflict) → that
//     error, verbatim; gitx guarantees the worktree is left as it found it.
//   - already up to date → Result with UpToDate=true, empty Digest, and NO
//     run.changed published (nothing changed).
//   - success → Result carrying the rendered digest, with run.changed
//     published so the behind-badge and runs rail refresh.
//
// Everything from taking the per-run lock onward runs on a
// cancellation-immune context: a caller (a phone, a dropped HTTP connection)
// vanishing once the merge starts must not kill git halfway through rewriting
// the live worktree.
func (s *Service) PullBase(ctx context.Context, run store.Run) (Result, error) {
	repo, err := s.store.RepoByID(ctx, run.RepoID)
	if err != nil {
		return Result{}, err
	}
	// The run's base seam: today every run pulls the repo's default branch;
	// per-run base branches (#130/#131) will resolve run.base_branch here.
	base := repo.DefaultBranch

	// Serialize against a concurrent pull of the SAME run before the
	// seconds-wide git window opens.
	unlock := s.mu.lock(run.ID)
	defer unlock()

	// Not abortable once we commit to it (see the doc comment).
	ctx = context.WithoutCancel(ctx)

	// Resolve the identity a merge commit would be authored under, but do NOT
	// refuse an empty result up front: a fast-forward pull authors no commit
	// and needs no identity, and ff-ness is unknowable before the fetch. gitx
	// makes that call after the fetch (#151); an empty identity only bites on
	// the diverged path, mapped below.
	name, email, err := s.authorIdentity(ctx, repo)
	if err != nil {
		return Result{}, err
	}

	credEnv, cleanup, err := s.credentialEnv(ctx, repo, "pull-"+run.ID)
	if err != nil {
		return Result{}, err
	}
	defer cleanup()

	env := append(append([]string{}, s.gitEnv...), credEnv...)
	pr, err := s.git.PullBase(ctx, s.bareDir(run.RepoID), run.WorktreePath, base, name, email, env)
	if err != nil {
		// A diverged pull with no author identity comes back as
		// gitx.ErrAuthorIdentityRequired (the merge commit had nobody to
		// author it) — re-flag it as this package's ErrNoAuthorIdentity, the
		// sentinel the HTTP layer maps to a 409 (#151). Every other typed
		// refusal passes through verbatim; the chat/HTTP layer classifies
		// *gitx.ConflictError / ErrMergeConflict itself. The worktree is
		// untouched (gitx's failure contract).
		if errors.Is(err, gitx.ErrAuthorIdentityRequired) {
			return Result{}, ErrNoAuthorIdentity
		}
		return Result{}, err
	}
	res := Result{
		Base:        base,
		OldHead:     pr.OldHead,
		NewHead:     pr.NewHead,
		UpToDate:    pr.UpToDate,
		FastForward: pr.FastForward,
	}
	if pr.UpToDate {
		// Nothing came in: no digest, no event, worktree untouched.
		return res, nil
	}

	sum, err := s.git.SummarizeRange(ctx, run.WorktreePath, pr.OldHead, pr.NewHead, maxSubjects, maxFiles, env)
	if err != nil {
		// The merge already landed in the worktree — publish so the badge
		// refreshes even though the digest is lost, and say so in the error.
		s.publishRunChanged(run.RepoID, run.ID)
		return Result{}, fmt.Errorf("pull of origin/%s landed %s..%s but digesting the range failed: %w",
			base, shortSHA(pr.OldHead), shortSHA(pr.NewHead), err)
	}
	res.Digest = renderDigest(res, sum)
	s.publishRunChanged(run.RepoID, run.ID)
	return res, nil
}

// renderDigest renders the agent-facing digest for a pull that moved HEAD — a
// pure function of the pull result and the range summary. Shape (compact on
// purpose; the caps above bound it to a few KB):
//
//	Lab pulled origin/<base> into this worktree: <old12>..<new12> (fast-forward | merge commit).
//	Incoming commits (<total>):
//	- <subject>            (newest first, verbatim — subjects carry PR/issue refs naturally)
//	… and <N> more         (only when subjects were capped)
//	Files changed:
//	<status>\t<path>
//	… and <N> more         (only when files were capped)
//	Prior reads of these files may be stale — … git log/git diff pointers.
func renderDigest(r Result, sum gitx.RangeSummary) string {
	rng := shortSHA(r.OldHead) + ".." + shortSHA(r.NewHead)
	shape := "merge commit"
	if r.FastForward {
		shape = "fast-forward"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Lab pulled origin/%s into this worktree: %s (%s).\n", r.Base, rng, shape)
	fmt.Fprintf(&b, "Incoming commits (%d):\n", sum.TotalCommits)
	for _, subject := range sum.Subjects {
		b.WriteString("- " + subject + "\n")
	}
	if n := sum.TotalCommits - len(sum.Subjects); n > 0 {
		fmt.Fprintf(&b, "… and %d more\n", n)
	}
	b.WriteString("Files changed:\n")
	for _, fc := range sum.Files {
		b.WriteString(fc.Status + "\t" + fc.Path + "\n")
	}
	if n := sum.TotalFiles - len(sum.Files); n > 0 {
		fmt.Fprintf(&b, "… and %d more\n", n)
	}
	fmt.Fprintf(&b, "Prior reads of these files may be stale — re-read anything you rely on before acting on it. For detail: git log %s --oneline, git diff %s", rng, rng)
	return b.String()
}

// shortSHA is the digest's sha abbreviation: the first 12 characters.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// bareDir is the repo's bare reference clone (design §7) — the same derivation
// as reposvc.bareDir / crmerge.bareDir, on the ReposDir given.
func (s *Service) bareDir(repoID string) string {
	return filepath.Join(s.reposDir, repoID+".git")
}

// authorIdentity resolves the merge commit's author/committer identity: the
// repo's git_author_name/email override the global settings pair,
// field-by-field (the same layering as crmerge and the spawned-session
// authorEnv in internal/instance). Empty results mean "not configured" —
// harmless for a fast-forward (no commit is authored), but a diverged pull is
// refused rather than authoring as nobody or as a bot (gitx makes that call
// after the fetch — #151).
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

// credentialEnv materializes the repo's GIT credential for one pull op
// (design §6) and returns the git env entries plus a cleanup removing exactly
// this op's files. A repo without a credential yields no env and a no-op
// cleanup (a filesystem/local remote needs none). Mirrors the per-service
// credentialEnv helpers in crmerge, reposvc and instance.
func (s *Service) credentialEnv(ctx context.Context, repo store.Repo, opID string) (env []string, cleanup func(), err error) {
	noop := func() {}
	if repo.CredentialID == nil {
		return nil, noop, nil
	}
	if s.vault == nil || s.mat == nil {
		return nil, noop, errors.New("credentialed pull without a vault/materializer wired")
	}
	cred, err := s.store.CredentialByID(ctx, *repo.CredentialID)
	if err != nil {
		return nil, noop, err
	}
	remove := func() {
		if err := s.mat.Cleanup(cred.ID, opID); err != nil {
			s.log.Warn("cleaning materialized credential", "component", "pull", "err", err)
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

// publishRunChanged emits run.changed naming the one run the pull landed in
// (issue #175). A nil bus (some tests) is a no-op.
func (s *Service) publishRunChanged(repoID, runID string) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(events.Event{Type: EventRunChanged, Payload: repoScopedPayload{Type: EventRunChanged, RepoID: repoID, RunID: runID}})
}

// keyedMutex is a per-key mutual exclusion helper: lock(key) blocks while
// another holder owns the same key and returns the unlock func. Entries are
// tiny and never evicted — key cardinality is bounded by real run ids.
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
