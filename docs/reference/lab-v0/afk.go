package main

import (
	"errors"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// noReadyNotice is the specific, non-error message shown when Start AFK run
// finds an empty ready-for-agent queue. It is surfaced distinctly from the
// generic action-error flash so the user knows nothing went wrong — there was
// simply nothing to claim.
const noReadyNotice = "No ready-for-agent issues to start."

// forgejoFor returns the (cached) Forgejo detection for a project path. The
// origin remote is read once per path and memoised: remotes don't change within
// a process lifetime, and a restart re-derives cheaply. A nil git seam or an
// unreadable origin is cached as "not Forgejo", so the menu degrades to its
// disabled line instead of erroring the whole render.
func (s *Server) forgejoFor(path string) forgejoInfo {
	s.forgejoMu.Lock()
	defer s.forgejoMu.Unlock()
	if info, ok := s.forgejoCache[path]; ok {
		return info
	}
	info := forgejoInfo{}
	if s.git != nil {
		if url, err := s.git.OriginURL(path); err == nil {
			info = detectForgejo(url)
		}
	}
	s.forgejoCache[path] = info
	return info
}

// setReadyCount records the ready-for-agent count the scheduler observed for a
// project on a sweep, in the in-memory cache the render path reads for the ⋯
// menu's "(N ready)" hint. Keyed by project name and never persisted (see
// Server.afkReady), so it re-derives within one sweep interval after a restart.
func (s *Server) setReadyCount(project string, n int) {
	s.afkReadyMu.Lock()
	defer s.afkReadyMu.Unlock()
	s.afkReady[project] = n
}

// readyCount returns the cached ready-for-agent count for a project and whether
// the scheduler has recorded one. known=false is "unknown" (no sweep yet) — the
// menu then shows a plain "Start AFK run" with no count. A locked map read with
// no tea/Tracker call, so it is safe on the per-poll render path (snapshot).
func (s *Server) readyCount(project string) (n int, known bool) {
	s.afkReadyMu.Lock()
	defer s.afkReadyMu.Unlock()
	n, known = s.afkReady[project]
	return n, known
}

// worktreeSep joins a project name and issue number into an AFK run's worktree
// directory name. It is deliberately NOT instanceSep ("~"): "~" is right for tmux
// session names but wrong for a filesystem path. This path becomes claude's
// working directory, and a "<name>~<digit>" component (e.g. "…~63") matches the
// Windows 8.3 short-name pattern (PROGRA~1), which claude flags as a path-
// confusion risk and forces manual approval for every file edit — silently
// stalling an unattended AFK run on its first edit, in any --permission-mode. A
// plain "-" sidesteps the heuristic and matches the name git itself derives for
// the worktree's admin dir under .git/worktrees/.
const worktreeSep = "-"

// worktreePath is the on-disk worktree backing an instance, under the worktrees
// root — deliberately outside the projects scan root so a worktree's .git file
// never surfaces as a bogus project card. The directory name is worktreeDir(id):
// <project>-<N> for an AFK run, <project>-<label> for a manual one (ADR-0017).
func (s *Server) worktreePath(id instanceID) string {
	return filepath.Join(s.worktreeRoot, worktreeDir(id))
}

// afkBranchPrefix is the namespace for every AFK-run branch. An issue's claim is
// the existence of refs/heads/afk/<N> (ADR-0013), so this one prefix is what the
// claim creates (launchAFKRun), what the oracle enumerates (Git.AFKBranches),
// and what the reaper matches a run's PR head against — defined once so they can
// never drift apart.
const afkBranchPrefix = "afk/"

// afkBranch is the branch an AFK run for issue n works on (and the head branch
// its PR carries).
func afkBranch(n int) string { return afkBranchPrefix + strconv.Itoa(n) }

// parseAFKBranch is the inverse of afkBranch: it reads the issue number out of an
// afk/<N> branch name. ok is false for any name not of that exact shape (a
// different prefix, or a non-numeric suffix), so a stray branch never registers
// as a claim.
func parseAFKBranch(branch string) (int, bool) {
	rest, ok := strings.CutPrefix(branch, afkBranchPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// claimableIssues filters a ready queue down to the issues not already claimed —
// those without a local afk/<N> branch (ADR-0013). claimed is the set
// Git.AFKBranches returns. A parked/failed run's surviving branch, or a run this
// same sweep just launched, keeps its issue out of the result, so selection
// drains *around* it with no tracker label and no flapping; pickLowestIssue then
// chooses the lowest of what remains.
func claimableIssues(ready []Issue, claimed map[int]bool) []Issue {
	out := make([]Issue, 0, len(ready))
	for _, is := range ready {
		if !claimed[is.Index] {
			out = append(out, is)
		}
	}
	return out
}

// afkSeedPrompt is the trailing argument handed to the spawned claude session.
// It encodes the ADR-0007 contract: lab has already selected, claimed, and set
// up the run, so the agent does nothing but resolve the one named issue and open
// a PR. The required elements (read #N, stay on afk/<N>, implement, verify,
// commit, push, open a PR with Closes #N) are all present; the wording is ours.
func afkSeedPrompt(n int) string {
	num := strconv.Itoa(n)
	return strings.Join([]string{
		"You are an autonomous AFK run. Resolve exactly one issue, open a pull request, and stop.",
		"",
		"1. Read issue #" + num + " from this project's own tracker: `tea issues " + num + " --comments`.",
		"2. You are already on a fresh branch `afk/" + num + "` in an isolated worktree — stay on it; do not create or switch branches.",
		"3. Implement what issue #" + num + " asks for.",
		"4. Verify it the way this project expects: run its tests, build, and linters if present, and fix anything you broke.",
		"5. Commit with a Conventional Commits message.",
		"6. Push the branch: `git push -u origin afk/" + num + "`.",
		"7. Open a pull request against the default branch with a Conventional-Commits-style title and `Closes #" + num + "` in the body (`tea pr create`).",
		"",
		"Work only inside this repository. Once the PR is open you are done — do not pick up any other issue.",
	}, "\n")
}

// handleAFKStart claims the lowest open ready-for-agent issue from the project's
// own tracker and launches a seeded claude --remote-control run that resolves it
// in an isolated worktree. The selection/claim/spawn core is launchAFKRun, shared
// verbatim with the auto scheduler; this handler is the manual entry point — it
// adds the HTTP guards and maps the launch outcome to a response.
func (s *Server) handleAFKStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.tracker == nil || s.git == nil {
		s.fail(w, r, errors.New("AFK runs are not configured"), http.StatusInternalServerError)
		return
	}
	// Same login guard as an ordinary Start: an AFK run is a remote-control
	// session and would hit the login wall immediately if logged out. Force past
	// the render cache so a silent token expiry can't let a doomed run spawn.
	if !s.forceAuthRefresh() {
		s.ok(w, r)
		return
	}
	project := strings.TrimPrefix(r.URL.Path, "/afk/start/")
	dir, err := s.projectDir(project)
	if err != nil {
		s.fail(w, r, err, http.StatusNotFound)
		return
	}
	// Only Forgejo-hosted projects can be claimed against. The UI renders the
	// menu line disabled for the rest, but a direct POST must be refused too.
	if !s.forgejoFor(dir).IsForgejo {
		s.fail(w, r, errors.New("not a git.cloonar.com repo"), http.StatusBadRequest)
		return
	}
	outcome, status, err := s.launchAFKRun(project, dir, false)
	if err != nil {
		s.fail(w, r, err, status)
		return
	}
	// Empty queue gets the specific notice; a spawn or an at-cap no-op both land
	// back on the index (the at-cap case mirrors the manual Start button).
	if outcome == afkLaunchNoReady {
		s.noReady(w, r)
		return
	}
	s.ok(w, r)
}

// handleAFKAuto flips a project's persisted autoEnabled toggle and re-renders.
// Like Start AFK run it is Forgejo-only — a direct POST against a non-Forgejo
// project is refused, not just hidden — since auto runs claim issues via tea,
// which has nothing to target off git.cloonar.com. Enabling kicks one scheduler
// sweep so a launch happens promptly instead of waiting up to a full tick; that
// sweep is race-safe against the ticker because the claim is single-flighted
// under afkMu (launchAFKRun re-checks one-auto-per-project there).
func (s *Server) handleAFKAuto(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.tracker == nil || s.git == nil {
		s.fail(w, r, errors.New("AFK runs are not configured"), http.StatusInternalServerError)
		return
	}
	project := strings.TrimPrefix(r.URL.Path, "/afk/auto/")
	dir, err := s.projectDir(project)
	if err != nil {
		s.fail(w, r, err, http.StatusNotFound)
		return
	}
	if !s.forgejoFor(dir).IsForgejo {
		s.fail(w, r, errors.New("not a git.cloonar.com repo"), http.StatusBadRequest)
		return
	}
	enabled := !s.store.AutoEnabled(project)
	if err := s.store.SetAutoEnabled(project, enabled); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	if enabled {
		go s.scheduleAFKRuns()
	}
	s.ok(w, r)
}

// handleAFKReset zeroes a project's consecutive-failure counter and re-arms its
// automatic loop — the human-initiated un-pause ADR-0007 mandates (it rejects any
// automatic un-pause). Mirrors handleAFKAuto: POST-only and Forgejo-only, so a
// direct POST against a non-Forgejo project is refused, not just hidden. Zeroing
// the counter clears any three-strikes pause; it then kicks one scheduler sweep so
// a re-armed project claims a ready issue promptly instead of waiting up to a full
// tick. The sweep self-gates on the auto toggle, so kicking it is safe whether
// auto is on or off. Reset deliberately does NOT touch the auto toggle — toggling
// auto off and on is not a way to clear the counter (ADR-0007).
func (s *Server) handleAFKReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if s.tracker == nil || s.git == nil {
		s.fail(w, r, errors.New("AFK runs are not configured"), http.StatusInternalServerError)
		return
	}
	project := strings.TrimPrefix(r.URL.Path, "/afk/reset/")
	dir, err := s.projectDir(project)
	if err != nil {
		s.fail(w, r, err, http.StatusNotFound)
		return
	}
	if !s.forgejoFor(dir).IsForgejo {
		s.fail(w, r, errors.New("not a git.cloonar.com repo"), http.StatusBadRequest)
		return
	}
	if err := s.store.ResetFailures(project); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	go s.scheduleAFKRuns()
	s.ok(w, r)
}

// afkLaunch is the result of a launchAFKRun attempt that returned no error: a run
// was spawned, the ready queue was empty, lab was at its global cap, or (auto
// only) the project already had an auto run in flight. Meaningful only when err
// is nil — the manual handler maps it to a response, the scheduler uses it to
// advance its per-tick accounting.
type afkLaunch int

const (
	afkLaunchSpawned      afkLaunch = iota // a seeded run was claimed and spawned
	afkLaunchNoReady                       // empty ready-for-agent queue — nothing claimed
	afkLaunchAtCap                         // global instance cap reached — nothing launched
	afkLaunchAutoInFlight                  // an auto run for this project is already live (auto only)
)

// launchAFKRun is the shared select→cap-check→claim→trust-seed→spawn core that
// both the manual Start handler and the auto scheduler drive, so there is exactly
// one claim path — ADR-0007's race-freedom rests on that claim being
// single-flighted. It holds afkMu across the whole select-through-spawn, so two
// concurrent launches (a manual click and a scheduler tick, or two scheduler
// sweeps) can never claim the same issue, and an auto launch re-checks
// one-auto-run-per-project HERE, under the lock and against fresh liveness, not
// just in the scheduler's pre-tick snapshot. auto selects the run's kind marker
// (afk-<N> vs afk-auto-<N>).
//
// The claim is the creation of the isolated worktree on a fresh afk/<N> branch
// (ADR-0013): selection skips any issue that already has such a branch, so
// creating it is what takes the issue out of the claimable set — no tracker label
// is touched. On any failure before a live session the worktree and its branch
// are torn back down, releasing the claim so the issue is selectable again; a
// claimed-but-unlaunched issue therefore never strands.
//
// On a clean return err is nil and afkLaunch says what happened; on failure err
// is set and failStatus carries the HTTP status the manual handler should surface
// (the scheduler ignores it and just logs). Post-launch side effects that are not
// part of the claim — stamping recency, kicking the deep-link scrape — happen
// here too, so both callers behave identically once a run is live.
func (s *Server) launchAFKRun(project, dir string, auto bool) (outcome afkLaunch, failStatus int, err error) {
	// Serialise select→claim→spawn: a concurrent launch must not pick the same
	// issue this one is about to claim (see Server.afkMu / ADR-0007).
	s.afkMu.Lock()
	defer s.afkMu.Unlock()
	// Pick before touching tmux or git: an empty queue is a no-op, not a spawn
	// and not a claim.
	issues, err := s.tracker.ReadyIssues(dir)
	if err != nil {
		return afkLaunchSpawned, http.StatusBadGateway, err
	}
	if len(issues) == 0 {
		return afkLaunchNoReady, 0, nil
	}
	// Exclude any issue already claimed by a local afk/<N> branch (ADR-0013): the
	// branch is the claim, so a parked/failed run's surviving branch — or a run a
	// concurrent sweep just launched — keeps its issue out of selection with no
	// tracker label. This read under afkMu is the authoritative claim check; the
	// scheduler's pre-tick one is only a hint.
	claimed, err := s.git.AFKBranches(dir)
	if err != nil {
		return afkLaunchSpawned, http.StatusInternalServerError, err
	}
	issue, ok := pickLowestIssue(claimableIssues(issues, claimed))
	if !ok {
		return afkLaunchNoReady, 0, nil
	}
	// One tmux listing serves the auto-in-flight gate, the cap guard, and slot
	// allocation — all from the same fresh, locked snapshot.
	live, err := s.sessions.List()
	if err != nil {
		return afkLaunchSpawned, http.StatusInternalServerError, err
	}
	// Serial per project for AUTO runs: refuse a second auto run when one is
	// already live. This is the authoritative check (the scheduler's pre-tick one
	// is just an optimisation) — being under afkMu and on fresh liveness, it makes
	// a toggle-on kick safe to race the ticker. Manual runs skip it: they are
	// additive, bounded only by the global cap (ADR-0007).
	if auto && autoRunProjects(live)[project] {
		return afkLaunchAutoInFlight, 0, nil
	}
	// Cap guard. Nothing is claimed yet, so an at-cap project simply no-ops.
	if s.liveInstanceCount(live) >= s.maxInstances {
		return afkLaunchAtCap, 0, nil
	}
	// An AFK run's identity is <project>~afk-<N> (or afk-auto-<N>): the issue
	// number alone keeps it unique per project — only one run per issue, enforced
	// by the afk/<N> claim branch — so no slot is needed (ADR-0017).
	id := instanceID{Project: project, Label: afkLabel(issue.Index, auto)}
	name := composeSessionName(id)

	// Claim: create the isolated worktree on a fresh afk/<N> branch. That branch
	// *is* the claim (ADR-0013) — durable on disk, restart-safe, invisible to
	// triage — so there is no tracker label to set. Any failure before a live
	// session tears the worktree and its branch back down, releasing the claim so
	// the issue can be selected again; nothing else needs restoring. Worktree dir
	// (<project>-<N>) and branch (afk/<N>) derive from the same instance identity
	// the manual path uses.
	wt := s.worktreePath(id)
	branch := instanceBranch(id)
	// Protect the run from the runtime sweep for the window between its worktree
	// being created and its session going live — same guard the manual path uses
	// (see startInstance / Server.starting / reconcile.go).
	s.markStarting(name)
	defer s.clearStarting(name)
	teardownClaim := func() {
		if err := s.git.RemoveWorktree(dir, wt); err != nil {
			log.Printf("afk %s #%d: rollback remove worktree: %v", project, issue.Index, err)
		}
		if err := s.git.DeleteBranch(dir, branch); err != nil {
			log.Printf("afk %s #%d: rollback delete branch: %v", project, issue.Index, err)
		}
	}
	if err := s.git.AddWorktree(dir, wt, branch); err != nil {
		// A failed add creates no worktree and no afk/<N> branch, so there is no
		// claim to tear down — the issue is selectable again as-is.
		return afkLaunchSpawned, http.StatusInternalServerError, err
	}
	if err := SeedTrust(s.claudeCfg, wt); err != nil {
		teardownClaim()
		return afkLaunchSpawned, http.StatusInternalServerError, err
	}
	// Reuse the ordinary spawn path: base remote-control argv with the seed
	// prompt appended as the trailing argument, run in the worktree.
	argv := append(s.sessions.baseStartArgv(), afkSeedPrompt(issue.Index))
	if err := s.sessions.StartCommand(name, wt, argv); err != nil {
		teardownClaim()
		return afkLaunchSpawned, http.StatusInternalServerError, err
	}
	// Live: from here it is treated exactly like an ordinary start — float the
	// project to the top, then scrape its deep link in the background.
	if err := s.store.StampOpened(project, time.Now()); err != nil {
		log.Printf("stamp opened %q: %v", project, err)
	}
	s.startCapture(name)
	return afkLaunchSpawned, 0, nil
}

// noReady ends a Start AFK run that found nothing to claim. The fetch path gets
// the specific notice in the body at a non-2xx status so the JS surfaces it in
// the flash (distinct text from the generic action error); the no-JS path lands
// on the index with the ?notice flag, which renders the same message.
func (s *Server) noReady(w http.ResponseWriter, r *http.Request) {
	if wantsFragment(r) {
		http.Error(w, noReadyNotice, http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/?notice=no-ready", http.StatusSeeOther)
}

// afkWatchInterval is how often the reaper sweeps live AFK runs; afkRunBudget is
// how long a run may go without producing a PR before it is failed as a timeout.
// These are the slice's two tunable knobs, defined together: the interval sits in
// ADR-0007's 30–60s watcher band, the budget is two hours (raised from #63's
// original ~45 minutes to give AFK agents room for longer runs).
const (
	afkWatchInterval = 30 * time.Second
	afkRunBudget     = 2 * time.Hour
)

// afkSweepInterval throttles the runtime worktree/branch sweep that piggybacks the
// reaper loop (ADR-0017): the reaper ticks every afkWatchInterval (30s), but the
// sweep — a per-project fetch plus a merged-branch GC — runs far less often, so it
// adds negligible load while still collecting merged lab//afk/ branches and their
// clean worktrees within minutes of a PR landing. Deliberately NOT every tick: a
// fetch per project every 30s would be wasteful for a janitorial pass.
const afkSweepInterval = 10 * time.Minute

// afkPauseThreshold is how many consecutive failed runs (death or timeout) pause
// a project's automatic loop: once consecutiveFailures reaches it the scheduler
// launches no further auto runs for that project, even while its auto toggle
// stays on, until a success or a UI Reset zeroes the counter (ADR-0007). Manual
// "Start AFK run" is never gated by it. A single tunable constant — the slice
// deliberately keeps the threshold in code rather than config.
const afkPauseThreshold = 3

// afkRun identifies one AFK run by its tmux session name and the project + issue
// + kind encoded in that name (<project>~<slot>~afk-<N> for manual,
// <project>~<slot>~afk-auto-<N> for auto). Auto lets the scheduler keep auto runs
// serial per project while ignoring manual ones, restart-proof from the name.
type afkRun struct {
	Name    string
	Project string
	Issue   int
	Auto    bool
}

// parseAFKRun recognises an AFK-run session name and returns the run it
// identifies. ok is false for any non-AFK session (an ordinary instance, the
// login session), so the watcher only ever acts on real AFK runs. Both manual
// and auto runs are recognised — the reaper treats them identically.
func parseAFKRun(name string) (afkRun, bool) {
	id := parseSessionName(name)
	n, auto, ok := parseAFKLabel(id.Label)
	if !ok {
		return afkRun{}, false
	}
	return afkRun{Name: name, Project: id.Project, Issue: n, Auto: auto}, true
}

// afkOutcome is the classification of an AFK run on a watcher tick: still running
// (the only non-terminal value), or one of the three terminal outcomes the
// reaper acts on.
type afkOutcome int

const (
	afkInProgress     afkOutcome = iota // alive, no PR, under budget — leave it
	afkSuccess                          // an open/merged afk/<N> PR exists
	afkFailureDeath                     // session gone, no PR
	afkFailureTimeout                   // alive, no PR, over budget
)

func (o afkOutcome) String() string {
	switch o {
	case afkSuccess:
		return "success"
	case afkFailureDeath:
		return "failure (death)"
	case afkFailureTimeout:
		return "failure (timeout)"
	default:
		return "in-progress"
	}
}

// classifyAFKRun is the unit-tested core of the reaper: it decides a run's
// outcome from three inputs and nothing else — no tmux, tea, or clock. A present
// PR is success regardless of liveness, so a run that opened its PR and then died
// is a success, not a death-failure. Otherwise a dead session is a death-failure,
// and a live session that has reached its budget without a PR is a timeout; any
// run still alive, PR-less, and under budget is simply in progress.
func classifyAFKRun(prPresent, sessionAlive bool, age, budget time.Duration) afkOutcome {
	switch {
	case prPresent:
		return afkSuccess
	case !sessionAlive:
		return afkFailureDeath
	case age >= budget:
		return afkFailureTimeout
	default:
		return afkInProgress
	}
}

// afkPRPresent reports whether pulls holds an open or merged PR whose head branch
// is afk/<n> — the "done" signal. A closed-and-unmerged afk/<n> PR deliberately
// does not count: treating its dead PR as done would falsely reap the run as a
// success instead of letting it fail on death or timeout.
func afkPRPresent(pulls []PullRequest, n int) bool {
	branch := afkBranch(n)
	for _, p := range pulls {
		if p.Head == branch && (p.State == pullOpen || p.State == pullMerged) {
			return true
		}
	}
	return false
}

// watchAFKRuns is lab's single long-lived background loop and its first
// server-side periodic worker. claude --remote-control never self-exits — it
// opens its PR and then idles holding its slot — so this is what actually
// completes a run (PR opened) or fails it (death / budget overrun), freeing the
// slot in every terminal case. Started once at startup; a build without the AFK
// seams wired returns immediately rather than spinning an idle ticker.
//
// It also drives the runtime worktree/branch sweep (ADR-0017), piggybacked here so
// lab keeps just one janitorial goroutine — but on its own afkSweepInterval
// throttle, far slower than the reaper tick, since the sweep fetches per project.
func (s *Server) watchAFKRuns() {
	if s.tracker == nil || s.git == nil {
		return
	}
	ticker := time.NewTicker(afkWatchInterval)
	defer ticker.Stop()
	// lastSweep is loop-local — only this one goroutine touches it, so the throttle
	// needs no lock (unlike the per-run reap state, which handleStop also mutates).
	lastSweep := time.Now()
	for now := range ticker.C {
		s.reapAFKRuns(now)
		if now.Sub(lastSweep) >= afkSweepInterval {
			lastSweep = now
			s.sweepMergedWorktrees()
		}
	}
}

// reapAFKRuns performs one watcher sweep at time now: it reconciles the in-memory
// budget clock against the live sessions, then classifies and reaps every tracked
// AFK run. now is a parameter (not time.Now) so a sweep — budget clock and all —
// is deterministic under test. Each project's PR list is fetched once per tick;
// a project whose dir or pulls can't be read this tick is skipped (its runs are
// reclassified next tick) rather than failing the whole sweep.
//
// The pull lists are fetched UNLOCKED (the slow part), but the per-run reap
// decision is remade under afkRunsMu in classifyAndClaimAFKRun from fresh
// liveness — never from this loop's snapshot. That is what keeps a manual Stop
// neutral against a concurrent sweep: a run the user stops mid-tick is forgotten
// under the lock and so is never claimed as a failure here, however stale the
// surrounding snapshot has become.
func (s *Server) reapAFKRuns(now time.Time) {
	if s.tracker == nil || s.git == nil {
		return
	}
	runs := s.trackAFKRuns(now)
	if len(runs) == 0 {
		return
	}
	// Group by project so each project's pulls are listed once per tick, in a
	// stable order (trackAFKRuns returns runs sorted by name).
	byProject := map[string][]afkRun{}
	order := []string{}
	for _, run := range runs {
		if _, ok := byProject[run.Project]; !ok {
			order = append(order, run.Project)
		}
		byProject[run.Project] = append(byProject[run.Project], run)
	}
	for _, project := range order {
		dir, err := s.projectDir(project)
		if err != nil {
			log.Printf("afk watcher: project %q: %v", project, err)
			continue
		}
		pulls, err := s.tracker.ListPulls(dir)
		if err != nil {
			log.Printf("afk watcher: list pulls for %q: %v", project, err)
			continue
		}
		for _, run := range byProject[project] {
			if outcome, alive, ok := s.classifyAndClaimAFKRun(run.Name, afkPRPresent(pulls, run.Issue), now); ok {
				s.reapAFKRun(run, outcome, alive)
			}
		}
	}
}

// trackAFKRuns reconciles the in-memory budget clock with the live tmux sessions
// and returns every AFK run to consider this tick: the live ones (each stamped
// now the first time it is seen) plus any still-tracked run that has since
// vanished (a death candidate). Runs come back sorted by name so a tick is
// deterministic. The authoritative liveness/age decision is NOT made here — it is
// remade per run in classifyAndClaimAFKRun under the same lock — so this pass only
// refreshes the clock and enumerates candidates.
//
// The session listing and the lazy stamp run together under afkRunsMu, the lock
// handleStop also takes to stop-and-forget a run, so a just-stopped session can't
// be re-stamped in the gap between the listing and the stamp.
func (s *Server) trackAFKRuns(now time.Time) []afkRun {
	s.afkRunsMu.Lock()
	defer s.afkRunsMu.Unlock()

	live, err := s.sessions.List()
	if err != nil {
		log.Printf("afk watcher: list sessions: %v", err)
		return nil
	}
	for _, name := range live {
		if _, ok := parseAFKRun(name); ok {
			if _, seen := s.afkStarts[name]; !seen {
				s.afkStarts[name] = now // lazy stamp: the budget clock starts here
			}
		}
	}
	names := make([]string, 0, len(s.afkStarts))
	for name := range s.afkStarts {
		names = append(names, name)
	}
	sort.Strings(names)
	runs := make([]afkRun, 0, len(names))
	for _, name := range names {
		run, ok := parseAFKRun(name)
		if !ok {
			// A tracked key that no longer parses as an AFK run can't happen via
			// the stamp path above; drop it defensively so it can't wedge a tick.
			delete(s.afkStarts, name)
			continue
		}
		runs = append(runs, run)
	}
	return runs
}

// classifyAndClaimAFKRun makes the reap decision for one run atomically against a
// concurrent manual Stop. Under afkRunsMu — the same lock stopInstance holds to
// kill-and-forget a run — it re-reads the run's tracking and FRESH liveness,
// classifies from that plus prPresent (pre-fetched unlocked; PR status doesn't
// race with a Stop), and, when the outcome is terminal, claims the run by removing
// it from the budget clock. ok is false (leave the run alone) when the run is no
// longer tracked — a manual Stop already neutralised it, or a prior tick reaped
// it — or when it is still in progress.
//
// Because stopInstance kills the session AND deletes the budget-clock entry under
// this one lock, this never observes a dead-but-tracked run from a half-done Stop:
// it sees either alive-and-tracked (Stop not started → a genuine fresh decision)
// or gone-and-untracked (Stop done → ok=false, neutral). A genuine crash leaves
// the entry in place, so a tracked run that is no longer alive is a real death.
func (s *Server) classifyAndClaimAFKRun(name string, prPresent bool, now time.Time) (outcome afkOutcome, alive, ok bool) {
	s.afkRunsMu.Lock()
	defer s.afkRunsMu.Unlock()

	start, tracked := s.afkStarts[name]
	if !tracked {
		return afkInProgress, false, false
	}
	alive, err := s.sessions.IsRunning(name)
	if err != nil {
		// Can't read liveness (a transient tmux failure): don't reap this tick.
		// The run stays tracked and is reclassified next tick.
		log.Printf("afk watcher: liveness for %q: %v", name, err)
		return afkInProgress, false, false
	}
	outcome = classifyAFKRun(prPresent, alive, now.Sub(start), afkRunBudget)
	if outcome == afkInProgress {
		return outcome, alive, false
	}
	delete(s.afkStarts, name) // claim: a racing Stop now sees the run as gone
	return outcome, alive, true
}

// reapAFKRun is the single internal chokepoint every terminal AFK-run outcome
// routes through to execute its side effects: stop the session if still alive,
// apply the one guarded teardown to its worktree + branch (ADR-0017), forget the
// run's stale deep link, update the project's consecutive-failure counter, and log
// the outcome. The run is already dropped from the budget clock by the time this
// runs — classifyAndClaim claims it under the lock — so this performs no clock
// bookkeeping.
//
// Teardown is now the SAME guarded rule for success and failure (ADR-0017 slice 2,
// superseding slice 1's remove-on-success / keep-on-failure): a dirty worktree is
// kept whole, a clean one removed and its afk/<N> branch deleted only once merged.
// So a success whose PR is still open keeps the branch until it lands (the runtime
// sweep GCs it after); a clean failure is reclaimed but its unmerged claim branch
// survives, so the issue stays parked and inspectable via that branch (ADR-0013).
// Outcome accounting is all that stays kind-specific.
//
// This is the ONLY place the run lifecycle writes the failure counter (#65): a
// success reap resets it to zero (re-arming a three-strikes auto-pause), a death
// or timeout failure increments it; at afkPauseThreshold the scheduler stops
// launching auto runs for the project. The counter is kind-agnostic — both manual
// and auto runs feed it — so a successful manual run re-arms a paused auto loop,
// which is intended. A user-initiated Stop never reaches here (classifyAndClaim
// refuses a stopped-and-forgotten run under afkRunsMu), so Stop stays neutral.
func (s *Server) reapAFKRun(run afkRun, outcome afkOutcome, alive bool) {
	if alive {
		if err := s.sessions.Stop(run.Name); err != nil {
			log.Printf("afk reap %s: stop session: %v", run.Name, err)
		}
	}
	if err := s.store.ForgetURL(run.Name); err != nil {
		log.Printf("afk reap %s: forget url: %v", run.Name, err)
	}
	// One guarded teardown for both outcomes — keeps a dirty worktree whole, removes
	// a clean one and deletes its merged branch (see the doc above).
	s.teardownInstance(run.Name)
	switch outcome {
	case afkSuccess:
		if err := s.store.ResetFailures(run.Project); err != nil {
			log.Printf("afk reap %s: reset failures: %v", run.Project, err)
		}
	case afkFailureDeath, afkFailureTimeout:
		if n, err := s.store.IncrementFailures(run.Project); err != nil {
			log.Printf("afk reap %s: increment failures: %v", run.Project, err)
		} else if n >= afkPauseThreshold {
			log.Printf("afk %s: auto loop paused after %d consecutive failures — reset from the UI to re-arm", run.Project, n)
		}
	}
	log.Printf("afk reap %s #%d: %s", run.Project, run.Issue, outcome)
}

// --- scheduler: automatic AFK runs (#64) ------------------------------------

// afkScheduleInterval is how often the scheduler sweeps auto-enabled projects to
// launch the next run. It sits in ADR-0007's 30–60s band and is deliberately far
// slower than the ~4s client fragment poll (a launch is a heavy claim+spawn, not
// a render) and independent of the reaper's own ~30s ticker.
const afkScheduleInterval = 45 * time.Second

// afkAutoDecision holds the facts the auto-launch predicate weighs for one
// project on one scheduler tick. Pulling them into a struct keeps shouldLaunchAuto
// pure and table-testable in isolation from tmux/tea/clock (mirroring how
// classifyAFKRun is tested).
type afkAutoDecision struct {
	AutoEnabled  bool // the per-project toggle is on
	UnderCap     bool // lab is below its global instance cap
	AutoInFlight bool // this project already has an auto run live
	ReadyExists  bool // a ready-for-agent issue is waiting to be claimed
	Paused       bool // the project has hit afkPauseThreshold consecutive failures
}

// shouldLaunchAuto decides whether the scheduler launches an auto AFK run for a
// project this tick: the toggle is on, lab is under its global cap, the project
// has no auto run already in flight, a ready-for-agent issue is waiting, and the
// project is not paused by the three-strikes auto-pause (#65). CRITICAL: the
// Paused term gates only this scheduler predicate — it is deliberately NOT in the
// shared launchAFKRun claim path, so a manual "Start AFK run" on a paused project
// still works.
func shouldLaunchAuto(d afkAutoDecision) bool {
	return d.AutoEnabled && d.UnderCap && !d.AutoInFlight && d.ReadyExists && !d.Paused
}

// afkStartDisplay is how the ⋯ menu's "Start AFK run" control renders for one
// project: the Suffix appended to its label (" (N ready)" or empty) and whether
// it is Greyed as a zero-queue hint. Greyed is a VISUAL hint only — the control
// always stays a real submit button (never the HTML disabled attribute / the
// non-clickable .disabled span), so a stale cached zero can't block the
// authoritative click. ADR-0007: the click re-checks live and is the source of
// truth; the count is a best-effort annotation, stale by design.
type afkStartDisplay struct {
	Suffix string
	Greyed bool
}

// afkStartHint is the pure, table-tested decision for the Start AFK run control
// (mirroring shouldLaunchAuto / classifyAFKRun — no tmux/tea/clock), mapping the
// three cache states to how the control renders:
//   - auto off, or no cached count (known=false): unknown → plain "Start AFK run",
//     no suffix, not greyed. Covers auto-off projects and auto-on projects the
//     scheduler has not swept yet.
//   - cached N>0: " (N ready)" suffix, not greyed.
//   - cached zero: " (0 ready)" suffix, greyed (the template keeps it clickable).
//
// The count is auto-feature UI, so it shows only when autoEnabled — a stale entry
// lingering in the cache for a since-disabled project yields no suffix.
func afkStartHint(autoEnabled bool, count int, known bool) afkStartDisplay {
	if !autoEnabled || !known {
		return afkStartDisplay{}
	}
	return afkStartDisplay{
		Suffix: " (" + strconv.Itoa(count) + " ready)",
		Greyed: count == 0,
	}
}

// AFKStart is the projectGroup→template bridge for the Start AFK run control: it
// feeds the group's auto toggle and cached count through afkStartHint so the
// template renders the suffix and greyed-at-zero class from one tested decision.
// A method (not precomputed fields) keeps the pure helper the single source of
// truth and lets the render test drive the whole mapping through the template.
func (g projectGroup) AFKStart() afkStartDisplay {
	return afkStartHint(g.AutoEnabled, g.ReadyCount, g.ReadyKnown)
}

// autoRunProjects returns the set of project names that currently have a live
// AUTO AFK run, derived purely from live session names (so it is correct across a
// lab restart that re-adopts running sessions). Manual runs never appear here,
// which is what keeps the auto loop serial per project — one auto run at a time —
// independently of any additive manual runs.
func autoRunProjects(live []string) map[string]bool {
	m := map[string]bool{}
	for _, name := range live {
		if run, ok := parseAFKRun(name); ok && run.Auto {
			m[run.Project] = true
		}
	}
	return m
}

// runAFKScheduler is lab's second long-lived background worker (after the
// reaper): on the afkScheduleInterval cadence it launches AFK runs for
// auto-enabled Forgejo projects, one auto run per project at a time, under the
// global cap. Started once at startup; like watchAFKRuns it returns immediately
// when the AFK seams are not wired rather than spinning an idle ticker.
func (s *Server) runAFKScheduler() {
	if s.tracker == nil || s.git == nil {
		return
	}
	ticker := time.NewTicker(afkScheduleInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.scheduleAFKRuns()
	}
}

// scheduleAFKRuns is one scheduler sweep: for every auto-enabled Forgejo project
// with a claimable issue (one whose afk/<N> branch does not yet exist), no auto
// run already in flight, and headroom under the global cap, it launches the next
// run through the shared launchAFKRun claim path. Serial per project (launchAFKRun
// re-checks one-auto-per-project under afkMu), it drains the queue one issue per
// launch, idles when nothing is claimable, and launches nothing — without
// erroring — at the cap. Driven both by the ticker
// and by a toggle-on kick; the two are race-safe because every claim is
// single-flighted under afkMu.
//
// Cheap gates run before any tea/tmux/auth work, so an all-manual fleet costs one
// scan per tick and nothing more. A per-project tracker error skips just that
// project (reclassified next tick), never failing the whole sweep — mirroring the
// reaper.
func (s *Server) scheduleAFKRuns() {
	if s.tracker == nil || s.git == nil {
		return
	}
	projects, err := Scan(s.root)
	if err != nil {
		log.Printf("afk scheduler: scan: %v", err)
		return
	}
	// Only auto-on, Forgejo-hosted projects can launch. Filter with cheap store +
	// cached-Forgejo lookups so a tick with nothing to do does no auth/session/
	// tracker work at all.
	var candidates []Project
	for _, p := range projects {
		if s.store.AutoEnabled(p.Name) && s.forgejoFor(p.Path).IsForgejo {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return
	}
	// A logged-out auto-launch would claim an issue into a remote-control session
	// that dies at the login wall — reaped as a death-failure, parking the issue
	// behind its surviving afk/<N> branch for nothing. Gate the whole tick on a
	// fresh login check, as the manual Start does per click.
	if !s.forceAuthRefresh() {
		return
	}
	live, err := s.sessions.List()
	if err != nil {
		log.Printf("afk scheduler: list sessions: %v", err)
		return
	}
	liveCount := s.liveInstanceCount(live)
	inFlight := autoRunProjects(live)
	for _, p := range candidates {
		// The claimable queue needs a tracker round-trip (the ready issues) plus a
		// local branch read (the existing claims); candidates are already auto-on,
		// so neither is a wasted query.
		issues, err := s.tracker.ReadyIssues(p.Path)
		if err != nil {
			log.Printf("afk scheduler: ready issues for %q: %v", p.Name, err)
			continue
		}
		claimed, err := s.git.AFKBranches(p.Path)
		if err != nil {
			log.Printf("afk scheduler: afk branches for %q: %v", p.Name, err)
			continue
		}
		// Count the *claimable* queue — ready issues without an existing afk/<N>
		// branch (ADR-0013) — not the raw ready count, so the "(N ready)" hint and
		// ReadyExists ignore parked/in-flight issues; a project whose only ready
		// issues are all already claimed reads zero and does not loop. Recorded
		// BEFORE the launch gate, so every swept auto-on project updates the
		// render-path cache whether or not this tick launches (the menu reads only
		// this cache, never tea).
		claimable := claimableIssues(issues, claimed)
		s.setReadyCount(p.Name, len(claimable))
		d := afkAutoDecision{
			// Re-read rather than assume true: candidates were auto-on at the cheap
			// pre-filter, but re-reading here honours a toggle-off that landed during
			// this same tick.
			AutoEnabled:  s.store.AutoEnabled(p.Name),
			UnderCap:     liveCount < s.maxInstances,
			AutoInFlight: inFlight[p.Name],
			ReadyExists:  len(claimable) > 0,
			// Three-strikes auto-pause: a project at/over the threshold launches no
			// further auto runs until a success or a UI Reset zeroes the counter.
			Paused: s.store.ConsecutiveFailures(p.Name) >= afkPauseThreshold,
		}
		if !shouldLaunchAuto(d) {
			continue
		}
		outcome, _, err := s.launchAFKRun(p.Name, p.Path, true)
		if err != nil {
			log.Printf("afk scheduler: launch %q: %v", p.Name, err)
			continue
		}
		switch outcome {
		case afkLaunchSpawned:
			liveCount++ // this run consumed a global slot; the next project sees it
		case afkLaunchAtCap:
			return // at the global cap mid-tick — launch nothing further
		}
		// afkLaunchNoReady / afkLaunchAutoInFlight: a manual start or another sweep
		// raced us between the predicate and the locked claim; just move on.
	}
}
