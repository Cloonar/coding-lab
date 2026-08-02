// Package pull is the /pull-base lab command's git orchestration: the service
// between gitx's pull primitives (PullBase/SummarizeRange) and the chat/HTTP
// layer that intercepts the command and messages the agent. Per pull it
// resolves the run's base branch, serializes against other pulls of the SAME
// run, materializes the repo's git credential, merges the freshly-fetched
// origin/<base> into the run's LIVE worktree under the repo's configured real
// author identity (D15 measure 5), re-materializes the run's read-only import
// snapshots in place (ADR-0063 — /pull-base is their ONLY refresh), and
// renders the compact digest the chat layer pastes to the agent: one repair
// verb for "make my world current", covering the run's whole world rather
// than just its base branch. It does NOT own command interception, tmux
// messaging, or error presentation — the chat/HTTP layer classifies the
// typed gitx errors (*gitx.ConflictError / gitx.ErrMergeConflict) this
// service passes through verbatim.
package pull

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/instancehome"
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

	// Homes locates each run's read-only import snapshots (issue #261) —
	// ImportsPath(runID) is the directory the spawn materialized them into and
	// the one /pull-base re-materializes them in. Nil is tolerated the way a
	// nil Bus is: a service without it simply refreshes nothing, warning once
	// per pull of a repo that actually declares imports (an import-less repo,
	// the common case, never notices). Production wires it in cmd/lab.
	Homes *instancehome.Manager
}

// Service orchestrates base-branch pulls into live run worktrees under a
// per-run mutex.
type Service struct {
	store    *store.Store
	git      *gitx.Engine
	vault    *vault.Vault
	mat      *vault.Materializer
	bus      *events.Bus
	homes    *instancehome.Manager
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
		homes:    o.Homes,
		reposDir: o.ReposDir,
		gitEnv:   o.GitEnv,
		log:      logger,
	}
}

// Result describes one PullBase outcome for the chat/HTTP layer. FastForward
// mirrors gitx.PullResult (with UpToDate false and FastForward false the pull
// created a real merge commit); Digest is the agent-facing text, empty when
// UpToDate.
//
// UpToDate is the "nothing to tell the agent" flag — the 200-notice path — and
// since ADR-0063 it spans the run's whole world, not just its base branch: it
// is true only when the base was already current AND every read-only import
// refreshed to the same commit it already had AND no refresh failed. A base
// that did not move while a snapshot did leaves UpToDate false with
// OldHead == NewHead, and the digest says exactly that.
type Result struct {
	Base        string // base branch name the pull targeted
	OldHead     string // worktree HEAD before
	NewHead     string // worktree HEAD after (== OldHead when the base did not move)
	UpToDate    bool
	FastForward bool
	Digest      string // agent-facing digest text; empty when UpToDate

	// Imports is one entry per read-only import this pull re-materialized, in
	// the declaration's name order — empty for a repo that declares none, and
	// for a declared target this run was not spawned with (ADR-0063: the mount
	// inventory is fixed at spawn, so there is nothing to refresh).
	Imports []ImportChange
}

// ImportChange is one read-only import's refresh outcome (ADR-0063), rendered
// as a single digest line. Old is the commit the snapshot carried BEFORE this
// refresh, read from the sidecar the spawn wrote — empty when that sidecar is
// missing or unreadable, or when the two commits cannot be related at all —
// and New the commit it carries now (empty on failure). Commits counts what
// arrived over old..new, and is 0 whenever Old is empty or equal to New.
// Failed marks a refresh that did not happen: Reason carries the first line of
// the cause, and the snapshot on disk is still the one the run was spawned
// with, because gitx destroys nothing until the new commit is known.
type ImportChange struct {
	Name    string
	Old     string
	New     string
	Commits int
	Failed  bool
	Reason  string
}

// moved reports whether this import's line is one the agent must act on: its
// snapshot's content changed under it, or the refresh failed and what it holds
// is older than it may assume. An unchanged import is neither, so a pull whose
// imports are all unchanged is as silent as a pull with no imports at all.
func (c ImportChange) moved() bool { return c.Failed || c.Old != c.New }

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
//     error, verbatim; gitx guarantees the worktree is left as it found it,
//     and the read-only imports are left alone too (see below).
//   - nothing at all changed — base already current AND every import
//     unchanged → Result with UpToDate=true, empty Digest, and NO run.changed
//     published.
//   - success → Result carrying the rendered digest, with run.changed
//     published so the behind-badge and runs rail refresh. "Success" includes
//     a pull that only moved a read-only import: the base can be current while
//     a sibling repo's snapshot is a week old, and the agent has to be told.
//
// The read-only import refresh (ADR-0063) runs after the base merge and only
// when that merge SUCCEEDED. A refused pull returns its typed error above with
// no digest to hang import lines on, and its failure semantics — a conflict
// aborted with the worktree exactly as gitx found it — must not be muddied by
// having rewritten the run's snapshots on the way out. Once the merge is in,
// the refresh runs regardless of whether it brought anything in, and can only
// add to the result: a broken refresh is reported on its own digest line and
// logged, never returned as an error.
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
	// The merge is in (or was a no-op): now the run's read-only imports, which
	// refresh on EVERY successful /pull-base — the base moving and a sibling's
	// snapshot moving are independent facts, and the operator asked for both to
	// be current. Nothing below can fail the pull.
	res.Imports = s.refreshImports(ctx, repo, run)
	movedImports := false
	for _, ic := range res.Imports {
		if ic.moved() {
			movedImports = true
			break
		}
	}

	if pr.UpToDate && !movedImports {
		// Nothing came in and nothing moved: no digest, no event, worktree and
		// snapshots exactly as they were.
		return res, nil
	}
	if pr.UpToDate {
		// The base was already current, but a snapshot moved (or its refresh
		// failed). There IS something to tell the agent, so this is no longer
		// the silent path: UpToDate drops to false and the digest is the
		// imports block under a first line that says plainly that nothing was
		// pulled (OldHead == NewHead, so there is no range to render).
		res.UpToDate = false
		res.Digest = renderDigest(res, gitx.RangeSummary{})
		s.publishRunChanged(run.RepoID, run.ID)
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

// refreshImports re-materializes, in place, every read-only import this run
// was SPAWNED with (ADR-0063) and returns one ImportChange per refreshed
// import in the declaration's name order — /pull-base is the feature's only
// refresh verb, so this is the one place a live run's snapshots ever move.
//
// What refreshes is decided by the FILESYSTEM, not by the declaration alone: a
// declared target with no snapshot directory is skipped SILENTLY, because the
// run's mount inventory was fixed at spawn — an import declared mid-run has no
// mount to fill, and writing one now would produce a directory no container
// can see. The mirror case needs no code at all: a snapshot whose declaration
// was retracted mid-run is simply never iterated, so it stays mounted and
// frozen. A settings change takes effect at the next spawn, one rule with no
// partial-effect middle ground.
//
// Nothing here can fail the pull, and nothing here is undone by a later
// failure: each import's snapshot and sidecar are rewritten as it succeeds, so
// a dead third target leaves the first two genuinely refreshed and says so in
// the digest. The refreshes run SEQUENTIALLY, unlike the spawn path's parallel
// materialization: a /pull-base is operator-paced and a repo's import list is
// single digits, so ordered code that is trivial to follow beats saving a
// fetch or two.
func (s *Service) refreshImports(ctx context.Context, repo store.Repo, run store.Run) []ImportChange {
	targets, err := s.store.RepoImports(ctx, run.RepoID)
	if err != nil {
		// A store read failing here means the whole process is in trouble, and
		// the pull that just landed is real regardless — so the failure is
		// logged and the digest simply carries no imports block.
		s.log.Warn("reading read-only imports to refresh", "component", "pull", "run", run.ID, "err", err)
		return nil
	}
	if len(targets) == 0 {
		return nil // the common case: one indexed query and nothing else
	}
	if s.homes == nil {
		s.log.Warn("read-only imports declared but no instance-home manager is wired; snapshots not refreshed",
			"component", "pull", "run", run.ID)
		return nil
	}

	importsDir := s.homes.ImportsPath(run.ID)
	changes := make([]ImportChange, 0, len(targets))
	for _, t := range targets {
		dest := filepath.Join(importsDir, t.Name)
		if fi, err := os.Stat(dest); err != nil || !fi.IsDir() {
			continue // declared after this run spawned: no mount to fill
		}
		changes = append(changes, s.refreshImport(ctx, repo, run, t, dest))
	}
	return changes
}

// refreshImport re-materializes ONE import snapshot in place and reports what
// it did. The ordering is the guarantee: gitx fetches and resolves before it
// clears anything, so a dead target (host down, credential rotated, branch
// gone) leaves the spawn's snapshot intact and the agent keeps a stale-but-real
// sibling rather than an empty directory; and the sidecar is only rewritten
// once the new tree is actually on disk.
//
// repo is the run's OWN repo — the consumer, whose runner decides whether the
// refreshed tree gets write-protected again — while target is the imported
// repo, whose reference clone, default branch and credential drive the fetch.
func (s *Service) refreshImport(ctx context.Context, repo store.Repo, run store.Run, target store.Repo, dest string) ImportChange {
	c := ImportChange{Name: target.Name, Old: snapshotCommit(dest)}

	// The TARGET's own credential — that is what makes the feature
	// credential-free — under an op id unique per (run, target): vault keys
	// materialized files by (credID, opID), so several imports sharing one
	// credential would otherwise write the same filenames and one cleanup would
	// unlink another's live key. Same reasoning as instance.importCredentialEnv.
	credEnv, cleanup, err := s.credentialEnv(ctx, target, "pull-"+run.ID+"-import-"+target.ID)
	if err != nil {
		return s.failedImport(c, run, err)
	}
	defer cleanup()

	env := append(append([]string{}, s.gitEnv...), credEnv...)
	bare := s.bareDir(target.ID)
	commit, err := s.git.MaterializeSnapshot(ctx, bare, dest, target.DefaultBranch, env)
	if err != nil {
		return s.failedImport(c, run, err)
	}
	c.New = commit

	// The sidecar goes first, so the NEXT /pull-base reports its range from
	// here even if something below trips. A failed rewrite is logged, never
	// reported as a failed refresh: the snapshot on disk really did move, and
	// telling the agent otherwise would send it back to a file that is already
	// fresh — the cost is one later digest line reporting a wider range.
	if err := os.WriteFile(dest+".commit", []byte(commit+"\n"), 0o600); err != nil {
		s.log.Warn("recording refreshed import snapshot commit", "component", "pull",
			"run", run.ID, "import", target.Name, "err", err)
	}

	// One call for the count: the fetch above just moved origin/<default> to
	// exactly the commit that was extracted, so CommitsBehind's
	// `<old>..origin/<default>` IS the range that arrived. An old commit the
	// target's object store no longer holds (a force-push, a rewritten history)
	// cannot be related to the new one at all, so the line drops to "refreshed
	// to <new>" rather than printing a range that resolves nowhere.
	if c.Old != "" && c.Old != c.New {
		n, err := s.git.CommitsBehind(ctx, bare, c.Old, target.DefaultBranch, env)
		if err != nil {
			s.log.Warn("counting refreshed import snapshot commits", "component", "pull",
				"run", run.ID, "import", target.Name, "from", c.Old, "err", err)
			c.Old = ""
		} else {
			c.Commits = n
		}
	}

	// Host runner: re-apply the advisory write protection the spawn applied
	// (ADR-0063) — the fresh extraction wrote git's own 0644/0755 modes back
	// over it. Best-effort exactly as at spawn: a failed chmod is logged, never
	// reported as a failed refresh, because the snapshot itself is correct and
	// that runner is full-host-access break-glass anyway. Under the container
	// runner the tree stays writable host-side on purpose: the `:ro` bind is
	// the enforcement, and this very code has to be able to rewrite it.
	if repo.Runner != store.RunnerContainer {
		if err := protectSnapshot(dest); err != nil {
			s.log.Warn("re-protecting refreshed import snapshot", "component", "pull",
				"run", run.ID, "import", target.Name, "err", err)
		}
	}
	return c
}

// failedImport marks c as a failed refresh and logs the cause. The digest gets
// only the first line — git's stderr can run to a paragraph and an import line
// has one line to spend — while the log keeps the whole error.
func (s *Service) failedImport(c ImportChange, run store.Run, err error) ImportChange {
	s.log.Warn("refreshing read-only import snapshot", "component", "pull",
		"run", run.ID, "import", c.Name, "err", err)
	c.Failed, c.New, c.Commits = true, "", 0
	c.Reason = firstLine(err.Error())
	return c
}

// snapshotCommit reads the commit sidecar written beside the snapshot
// directory by the spawn (internal/instance/launch.go) or by a previous
// refresh. Any failure — absent, unreadable, empty — reads as an unknown
// previous commit (""), which renders as "refreshed to <new>": lab's own
// bookkeeping going missing must not stop the refresh or fake a range.
func snapshotCommit(dest string) string {
	b, err := os.ReadFile(dest + ".commit")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// firstLine is the digest's one-line rendering of a refresh failure.
func firstLine(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if msg = strings.TrimSpace(msg); msg == "" {
		return "unknown error"
	}
	return msg
}

// protectSnapshot strips the write bits from a refreshed snapshot tree (dirs →
// 0555, files → their mode minus 0222, symlinks skipped so the chmod never
// follows one out of the snapshot) — a package-private copy of the identically
// named helper in internal/instance/launch.go, which applies it at spawn. One
// small copy per package beats a dependency edge on the launch service (cf.
// credentialEnv, which every git-touching service owns its own version of);
// the two must agree, because a snapshot's modes should not depend on whether
// it was last written by a spawn or by a /pull-base. Best-effort by design:
// see the call site.
func protectSnapshot(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return os.Chmod(path, info.Mode().Perm()&^0o222)
	})
}

// renderDigest renders the agent-facing digest for a pull that changed
// something — a pure function of the pull result and the range summary. Shape
// (compact on purpose; the caps above bound it to a few KB):
//
//	Lab pulled origin/<base> into this worktree: <old12>..<new12> (fast-forward | merge commit).
//	Incoming commits (<total>):
//	- <subject>            (newest first, verbatim — subjects carry PR/issue refs naturally)
//	… and <N> more         (only when subjects were capped)
//	Files changed:
//	<status>\t<path>
//	… and <N> more         (only when files were capped)
//	Prior reads of these files may be stale — … git log/git diff pointers.
//
// A pull whose base did NOT move (OldHead == NewHead — it is here only because
// a read-only import did) has no range, no commits and no files to report, so
// that whole block collapses to one honest line:
//
//	Lab refreshed this run's read-only imports; origin/<base> was already up to date.
//
// Either way the run's read-only imports (ADR-0063) append a block of their
// own — and ONLY when there are any, so an import-less repo's digest is
// byte-identical to what it rendered before the feature existed:
//
//	Imports refreshed:
//	- <name>: unchanged
//	- <name>: <old12>..<new12> (<N> commits)
//	- <name>: refreshed to <new12>          (previous commit unknown — no sidecar)
//	- <name>: refresh failed — <reason>     (the snapshot is intact, still the old one)
//	Prior reads of moved snapshots may be stale — …   (only when one moved or failed)
func renderDigest(r Result, sum gitx.RangeSummary) string {
	var b strings.Builder
	if r.OldHead == r.NewHead {
		fmt.Fprintf(&b, "Lab refreshed this run's read-only imports; origin/%s was already up to date.", r.Base)
		writeImports(&b, r.Imports)
		return b.String()
	}
	rng := shortSHA(r.OldHead) + ".." + shortSHA(r.NewHead)
	shape := "merge commit"
	if r.FastForward {
		shape = "fast-forward"
	}
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
	writeImports(&b, r.Imports)
	return b.String()
}

// writeImports appends the imports block to b, or nothing at all when the pull
// refreshed no imports. Every line it writes is prefixed with its own newline
// rather than terminated by one: the base block deliberately ends without a
// trailing newline, and so does this one, so the two compose without a blank
// line and an import-less digest keeps its exact former bytes.
func writeImports(b *strings.Builder, imports []ImportChange) {
	if len(imports) == 0 {
		return
	}
	b.WriteString("\nImports refreshed:")
	moved := false
	for _, c := range imports {
		b.WriteString("\n- " + c.Name + ": " + importLine(c))
		moved = moved || c.moved()
	}
	if moved {
		b.WriteString("\nPrior reads of moved snapshots may be stale — re-read anything you rely on from them.")
	}
}

// importLine renders one import's outcome. The four cases are exhaustive over
// ImportChange: a failure (the snapshot is still the spawn's), a refresh whose
// starting point is unknown (no sidecar, or a previous commit the target's
// object store no longer has — either way there is no range worth printing), a
// no-op, and a real move.
func importLine(c ImportChange) string {
	switch {
	case c.Failed:
		return "refresh failed — " + c.Reason
	case c.Old == "":
		return "refreshed to " + shortSHA(c.New)
	case c.Old == c.New:
		return "unchanged"
	default:
		return fmt.Sprintf("%s..%s (%d commits)", shortSHA(c.Old), shortSHA(c.New), c.Commits)
	}
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
