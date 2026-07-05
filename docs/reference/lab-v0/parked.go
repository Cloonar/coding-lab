package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// parked.go is lab's Parked view (ADR-0017 slice 3). Slices 1–2 made every
// instance run in its own worktree and gave teardown one guarded rule that keeps
// dirty/unmerged work while GC-ing clean/merged work — so dirty or clean-but-
// unmerged lab/<label> and afk/<N> branches/worktrees survive teardown ("parked").
// This file is the UI to see and clean up that parked work:
//
//   - gatherParked enumerates the parked set, reusing the reconciliation
//     derivation (managedBranch + ownedBranches): a managed lab//afk/ branch no
//     live session owns, paired with its worktree if one exists. It is the cheap,
//     local-only-git read behind both the collapsed per-card count (snapshot path)
//     and the lazy detail endpoint.
//   - handleParked is the lazy per-project endpoint, distinct from /fragment, that
//     computes the expensive per-entry detail (ahead, unpushed, age, dirty, a
//     best-effort PR badge) on demand when the strip is expanded — so none of that
//     work ever runs on the ~4s poll.
//   - handleDiscard force-removes a parked worktree (if any) and force-deletes its
//     branch regardless of dirty/merged state, deliberately BYPASSING the guarded
//     teardown — the one place lab destroys unmerged/dirty work, behind the UI's
//     two-step confirm. Discarding an afk/<N> entry deletes its claim branch, so
//     the issue re-enters the claimable set (a manual requeue; ADR-0013).

// parkedRef is one parked branch: a managed lab//afk/ branch no live (or mid-Start)
// session owns, with the worktree backing it (Worktree "" for a bare branch — a
// merged-but-not-yet-GC'd afk/<N> claim, or a clean instance whose worktree was
// reclaimed but whose unmerged branch was kept). This is the cheap shape both the
// count and the detail view derive from.
type parkedRef struct {
	Branch   string
	Worktree string
}

// gatherParked enumerates a project's parked refs: every managed (lab//afk/) branch
// not owned by a live or mid-Start session, each paired with its worktree if one
// exists. It reuses the reconciliation derivation — managedBranch + ownedBranches —
// over the same local-only git reads the sweep uses (for-each-ref + worktree list),
// so the parked set can never drift from what reconciliation considers managed.
//
// The live session list is a parameter, not listed here, so the snapshot path can
// share its single tmux listing for every project's count (the lazy endpoint passes
// its own fresh listing). Unlike gatherRefs this makes no race-safety ordering
// promise — it never tears anything down, only reads — so a just-started instance
// briefly shown as parked (the documented count/list skew) self-heals on the next
// poll/re-open. No network call: for-each-ref and worktree list are local, so this
// is cheap enough to run per project on the snapshot/poll path.
func (s *Server) gatherParked(repoDir, project string, live []string) ([]parkedRef, error) {
	branches, err := s.git.Branches(repoDir, labBranchPrefix, afkBranchPrefix)
	if err != nil {
		return nil, err
	}
	wts, err := s.git.Worktrees(repoDir)
	if err != nil {
		return nil, err
	}
	owned := ownedBranches(append(s.startingSnapshot(), live...), project)
	wtByBranch := map[string]string{}
	for _, wt := range wts {
		if managedBranch(wt.Branch) {
			wtByBranch[wt.Branch] = wt.Path
		}
	}
	var parked []parkedRef
	for _, b := range branches {
		if owned[b] {
			continue // a live instance occupies it — not parked
		}
		parked = append(parked, parkedRef{Branch: b, Worktree: wtByBranch[b]})
	}
	return parked, nil
}

// parkedItem is one Parked-view row as the lazy fragment renders it: the branch,
// its kind (manual lab/ vs afk/<N>), the worktree (and whether it is dirty) when one
// exists, how far ahead of mainline it is, how many of those commits are unpushed,
// the tip's age, and a best-effort PR badge. The per-entry stats are best-effort —
// a single branch's stat hiccup degrades just its field, never the whole strip
// (enumeration failure is what fails the strip loud, see parkedView).
type parkedItem struct {
	Branch      string
	AFK         bool // afk/<N> claim branch vs a manual lab/<label> branch
	Issue       int  // the N, when AFK
	HasWorktree bool
	Worktree    string // on-disk path, copyable; "" when the branch has no worktree
	Dirty       bool   // uncommitted changes — meaningful only when HasWorktree
	Ahead       int    // commits beyond origin/<default>
	Unpushed    int    // commits not on origin — what a Discard destroys
	Age         string // humanised tip age ("3h", "2d", …); "" when unknown
	PRState     string // "open"/"merged"/"closed" when a PR matches the head; "" otherwise
}

// parkedView is the model the lazy "parkedBody" fragment renders. Err non-empty is
// the fail-loud path: enumeration (or a missing git seam) failed, so the strip shows
// the error rather than a misleading empty list. Items empty with no Err is a
// genuine "nothing parked" (the work was discarded or GC'd since the count was
// taken — the accepted count/list skew).
type parkedView struct {
	Project string
	Items   []parkedItem
	Err     string
}

// parkedRoutes registers the Parked view's two routes on the shared mux.
// handleParked serves the lazy per-project detail fragment; handleDiscard
// force-discards one parked entry. Both sit under /parked/, with the more specific
// /parked/discard/ winning ServeMux's longest-prefix match for the POST.
func (s *Server) parkedRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/parked/", s.handleParked)
	mux.HandleFunc("/parked/discard/", s.handleDiscard)
}

// handleParked is the lazy per-project endpoint the strip fetches on expand,
// deliberately separate from the /fragment poll so its expensive per-entry work
// (git stats per branch, plus one tea ListPulls for the PR badges) never runs on
// the ~4s poll. GET only. It always answers with the rendered "parkedBody" fragment
// — entries, a fail-loud error, or an empty "nothing parked" — which the client
// injects into the strip's data-static body.
func (s *Server) handleParked(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	project := strings.TrimPrefix(r.URL.Path, "/parked/")
	dir, err := s.projectDir(project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.renderParked(w, s.parkedView(project, dir))
}

// renderParked writes the parkedBody fragment for a view. Shared by the lazy
// endpoint and by handleDiscard's refreshed response.
func (s *Server) renderParked(w http.ResponseWriter, view parkedView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "parkedBody", view); err != nil {
		log.Printf("parked fragment execute: %v", err)
	}
}

// parkedView assembles the full Parked detail for a project: enumerate the parked
// refs, then enrich each with its git stats and a best-effort PR badge. Enumeration
// failure (or a missing git seam) is fail-loud — the view carries the error so the
// strip shows it instead of an empty list. The PR list is one tea call, made only
// for Forgejo projects (tea has nothing to target elsewhere, and a never-pushed
// lab/ branch never matches a PR head anyway) and treated best-effort: a tea failure
// drops the badges but still renders every entry.
func (s *Server) parkedView(project, dir string) parkedView {
	if s.git == nil {
		return parkedView{Project: project, Err: "worktrees are not configured"}
	}
	live, err := s.sessions.List()
	if err != nil {
		return parkedView{Project: project, Err: err.Error()}
	}
	refs, err := s.gatherParked(dir, project, live)
	if err != nil {
		return parkedView{Project: project, Err: err.Error()}
	}
	var pulls []PullRequest
	if s.tracker != nil && s.forgejoFor(dir).IsForgejo {
		if pulls, err = s.tracker.ListPulls(dir); err != nil {
			log.Printf("parked %s: list pulls: %v (PR badges omitted)", project, err)
			pulls = nil
		}
	}
	items := make([]parkedItem, 0, len(refs))
	for _, ref := range refs {
		items = append(items, s.parkedItem(dir, ref, pulls))
	}
	return parkedView{Project: project, Items: items}
}

// parkedItem enriches one parked ref into a rendered row. The kind comes from the
// branch name (parseAFKBranch). Worktree-only facts (dirty) are read only when a
// worktree exists. The git stats and PR match are best-effort per field: a stat
// error logs and leaves that field at its zero value rather than failing the row,
// since one odd branch should not blank the whole strip (the strip already failed
// loud if it could not even enumerate).
func (s *Server) parkedItem(dir string, ref parkedRef, pulls []PullRequest) parkedItem {
	it := parkedItem{Branch: ref.Branch}
	if n, ok := parseAFKBranch(ref.Branch); ok {
		it.AFK, it.Issue = true, n
	}
	if ref.Worktree != "" {
		it.HasWorktree, it.Worktree = true, ref.Worktree
		if dirty, err := s.git.WorktreeDirty(ref.Worktree); err != nil {
			log.Printf("parked %s: worktree status: %v", ref.Branch, err)
		} else {
			it.Dirty = dirty
		}
	}
	if ahead, err := s.git.CommitsAhead(dir, ref.Branch); err != nil {
		log.Printf("parked %s: commits ahead: %v", ref.Branch, err)
	} else {
		it.Ahead = ahead
	}
	if unpushed, err := s.git.UnpushedCount(dir, ref.Branch); err != nil {
		log.Printf("parked %s: unpushed count: %v", ref.Branch, err)
	} else {
		it.Unpushed = unpushed
	}
	if t, err := s.git.LastCommitTime(dir, ref.Branch); err != nil {
		log.Printf("parked %s: last commit time: %v", ref.Branch, err)
	} else {
		it.Age = humanizeAge(s.now().Sub(t))
	}
	it.PRState = pullState(pulls, ref.Branch)
	return it
}

// pullState returns the state of the pull request whose head is branch — the
// best-effort PR badge, matched client-side on head exactly as the reaper does
// (afkPRPresent). An open or merged PR wins over a closed one sharing the head (the
// live PR is the interesting one); a branch with no matching PR — every never-pushed
// lab/ branch — yields "" and so renders no badge.
func pullState(pulls []PullRequest, branch string) string {
	best := ""
	for _, p := range pulls {
		if p.Head != branch {
			continue
		}
		if p.State == pullOpen || p.State == pullMerged {
			return p.State
		}
		best = p.State // closed-and-unmerged: keep looking for a live one
	}
	return best
}

// humanizeAge renders a tip age coarsely for the Parked view — minute, hour, day,
// then week resolution, which is all the "how stale is this parked branch" glance
// needs. A sub-minute or negative age (clock skew, a commit dated in the future)
// reads as "just now".
func humanizeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		days := int(d.Hours()) / 24
		if days < 7 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dw", days/7)
	}
}

// handleDiscard force-discards one parked entry: the branch arrives as a POST form
// field (branch names contain "/", so it can't ride in the path), and lab removes
// the worktree (if any) and deletes the branch UNGUARDED — regardless of dirty or
// merged state. This deliberately bypasses teardownGuarded: it is the one action
// that destroys unmerged/dirty parked work, gated only by the UI's two-step confirm
// and the ahead/unpushed warning the view surfaced. The branch must be a managed
// lab//afk/ branch, so a crafted POST can never force-delete main or a human's own
// branch. On success it answers with the refreshed parkedBody so the open strip
// updates at once (the collapsed count catches up via the normal morph).
func (s *Server) handleDiscard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.git == nil {
		http.Error(w, "worktrees are not configured", http.StatusInternalServerError)
		return
	}
	project := strings.TrimPrefix(r.URL.Path, "/parked/discard/")
	dir, err := s.projectDir(project)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	branch := r.FormValue("branch")
	// Safety: only ever force-delete a branch lab itself created. A non-managed
	// branch (main, a human's feature branch) is never a parked entry, so a request
	// to discard one is malformed — refuse it rather than nuke an unrelated branch.
	if !managedBranch(branch) {
		http.Error(w, "not a parked branch: "+branch, http.StatusBadRequest)
		return
	}
	if err := s.discardParked(dir, branch); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderParked(w, s.parkedView(project, dir))
}

// discardParked is the unguarded teardown handleDiscard performs: force-remove the
// branch's worktree if it has one (worktree first — git refuses to delete a branch
// still checked out somewhere), then force-delete the branch. Both git calls are
// already force operations (RemoveWorktree --force, DeleteBranch -D), so a dirty
// worktree or an unmerged branch is removed all the same — that is the point. It
// does NOT consult WorktreeDirty/BranchMerged and does NOT route through
// teardownGuarded. Discarding an afk/<N> branch removes the claim, returning the
// issue to the claimable set (ADR-0013).
func (s *Server) discardParked(dir, branch string) error {
	wts, err := s.git.Worktrees(dir)
	if err != nil {
		return err
	}
	for _, wt := range wts {
		if wt.Branch == branch {
			if err := s.git.RemoveWorktree(dir, wt.Path); err != nil {
				return fmt.Errorf("remove worktree %s: %w", wt.Path, err)
			}
			break
		}
	}
	if err := s.git.DeleteBranch(dir, branch); err != nil {
		return fmt.Errorf("delete branch %s: %w", branch, err)
	}
	return nil
}
