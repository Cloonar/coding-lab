package afk

// The Schedule producer and its launch path (issue #247 / ADR-0062): per-repo
// cron cadences fire scheduled runs — a fourth unattended run kind gathered
// by the ONE spawn pass at stage rank lander > fix > scheduled > new AFK
// work, never an independent launch path (a second launch path is exactly
// the cap race ADR-0049 removed).
//
// Due-ness is observed, never scheduled: each pass lists the enabled
// Schedules once and walks each cadence's cron matches in (high-water mark,
// now]. A schedule ID's first sighting seeds its mark at that pass's now —
// arming the cadence forward-only — so matches nobody was awake to observe
// never become candidates: downtime loses firings by construction (no
// catch-up; the startup missed-slot Warn, derived from last_fired_at, is
// their only trace), and a re-enabled or re-created Schedule can never
// re-fire a slot an earlier incarnation consumed. The one owed firing per
// Schedule lives
// in the in-memory schedulePending memo: an at-cap firing retries every pass
// until launched or superseded by its own next due time (dropped loudly); a
// firing that comes due while the Schedule's previous run is still live is
// skipped loudly and CONSUMED, never queued (a queue of pending firings
// behind a slow run is auto-requeue-shaped runaway cost).
//
// Startup re-adoption (internal/reconcile) needs no schedule awareness, by
// design: the scheduled branch is exactly the repo's manual branch prefix +
// label, which reconcile's instanceBranch fallback already derives for any
// non-AFK label, and readopt is kind-agnostic — a scheduled session dead at
// startup classifies death through EndRun directly, bypassing reapRun, so it
// earns NO schedule strike. That is intended behavior, not an omission: the
// per-Schedule counter measures the reaper-observed lifecycle, and a restart
// is lab's doing, not the Schedule's.

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/cronx"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// scheduleLabelPrefix is the scheduled run's instance-label namespace, the
// sibling of the AFK labelPrefix and lander/fix/escalate prefixes —
// [a-z0-9-] only, never "~", and never parsed by ParseLabel or reconcile's
// adopt-label parsers, so the reconcile branch fallback derives manual
// prefix + label for it (the pinned branch grammar).
const scheduleLabelPrefix = "sched-"

// scheduleLabelStamp is the firing stamp's time layout (server-local
// yyyymmdd-hhmm): minute granularity matches cron's, so two firings of one
// Schedule can never collide (skip-on-overlap and the one-pending memo keep
// a minute from firing twice).
const scheduleLabelStamp = "20060102-1504"

// scheduleIDSuffixLen is how much of the schedule ID the label carries: the
// last 6 hex characters are enough to disambiguate a repo's Schedules from
// each other in tmux/branch listings — the stamp disambiguates firings, and
// the runs row's schedule_id column, not the label, is the real link.
const scheduleIDSuffixLen = 6

// scheduleMatchCap bounds every cron-match walk (a pass window is seconds to
// minutes, so hitting it means a pathological clock jump): the walk keeps
// only the latest match anyway, and the startup missed-slot count degrades
// to "many".
const scheduleMatchCap = 1000

// ScheduleLabel renders a scheduled run's instance label:
// sched-<last 6 hex of the schedule ID>-<yyyymmdd-hhmm firing stamp>, e.g.
// sched-3f9a1c-20260730-0600. The branch is exactly the repo's manual branch
// prefix + this label — the same composition a manual instance uses
// (instance.Start), which is what reconcile's instanceBranch fallback derives
// for any unknown label, so restart re-adoption works with zero naming code.
func ScheduleLabel(scheduleID string, firedAt time.Time) string {
	suffix := scheduleID
	if len(suffix) > scheduleIDSuffixLen {
		suffix = suffix[len(suffix)-scheduleIDSuffixLen:]
	}
	return scheduleLabelPrefix + suffix + "-" + firedAt.Format(scheduleLabelStamp)
}

// cronMatchesIn walks expr's matches in the half-open window (from, to],
// bounded by limit: latest is the newest match found, n how many, capped
// whether the walk gave up early (latest is then merely the newest WITHIN
// the bound — callers treat capped as "many" or log it, never as exact).
func cronMatchesIn(expr cronx.Expr, from, to time.Time, limit int) (latest time.Time, n int, capped bool) {
	t := from
	for {
		next, ok := expr.Next(t)
		if !ok || next.After(to) {
			return latest, n, false
		}
		latest, n = next, n+1
		if n >= limit {
			return latest, n, true
		}
		t = next
	}
}

// scheduleCandidates is the Schedule producer (issue #247 / ADR-0062),
// newWorkCandidates' sibling: one EnabledSchedules listing per pass (already
// filtered to enabled AND not paused), one cron-match walk per Schedule over
// the in-memory high-water mark, and at most one StageScheduled candidate
// per Schedule whose closure drives the locked launchScheduled path. It
// gathers only — it never launches and never decides the cap. repos,
// liveCount, and loggedIn are the pass's shared listing, liveness snapshot,
// and forced-auth memo.
//
// Every touch of schedulePending/scheduleChecked here (and in the emitted
// closures, which spawnPass runs synchronously) happens inside SpawnOnce
// under spawnMu — the fields' documented invariant.
func (s *Service) scheduleCandidates(ctx context.Context, repos []store.Repo, liveCount int, loggedIn map[string]bool) []spawnCandidate {
	schedules, err := s.store.EnabledSchedules(ctx)
	if err != nil {
		s.log.Warn("spawn: list schedules", "component", "afk", "err", err)
		return nil
	}

	byRepoID := make(map[string]store.Repo, len(repos))
	for _, r := range repos {
		byRepoID[r.ID] = r
	}

	// Startup missed-slot sweep, once, on the engine's first pass: every
	// enabled Schedule that HAS fired before reports how many cron slots fell
	// in (last_fired_at, engine start] — firings lost to downtime, gone by
	// construction (no catch-up). Loud because nothing else will ever say it;
	// a Schedule never fired (LastFiredAt nil) has no baseline and logs
	// nothing.
	if !s.scheduleMissedLogged {
		s.scheduleMissedLogged = true
		s.logMissedSlots(schedules, byRepoID)
	}

	// Cleanup: a Schedule no longer in the enabled listing (deleted,
	// disabled, or paused meanwhile) must not leak memo entries or fire a
	// stale pending later — drop both maps' strays.
	listed := make(map[string]bool, len(schedules))
	for _, sched := range schedules {
		listed[sched.ID] = true
	}
	for id := range s.schedulePending {
		if !listed[id] {
			delete(s.schedulePending, id)
		}
	}
	for id := range s.scheduleChecked {
		if !listed[id] {
			delete(s.scheduleChecked, id)
		}
	}

	now := s.now()
	var out []spawnCandidate
	for _, sched := range schedules {
		repo, ok := byRepoID[sched.RepoID]
		if !ok || repo.CloneStatus != store.CloneStatusReady {
			// Not launchable this pass; the high-water mark deliberately does
			// NOT advance, so once the repo is ready the window's matches
			// collapse to one owed firing (the latest), never a queue.
			continue
		}
		expr, err := cronx.Parse(sched.Cadence)
		if err != nil {
			// The API validated at write; a parse error here is belt-and-
			// braces (a hand-edited row, a grammar change): skip loudly.
			s.log.Error("spawn: schedule cadence unparseable", "component", "afk",
				"repo", repo.Name, "schedule", sched.Name, "cadence", sched.Cadence, "err", err)
			continue
		}
		lastChecked, sighted := s.scheduleChecked[sched.ID]
		if !sighted {
			// First sighting arms the cadence forward-only: seed the mark at
			// now and walk nothing this pass. A match between engine start and
			// this pass is lost (at most one tick — the no-catch-up rule
			// applied consistently); the alternative, an engine-start floor,
			// would fire a Schedule created hours after boot for a stale slot
			// and let a re-enabled or re-created Schedule re-fire slots a
			// prior incarnation already consumed (the cleanup above dropped
			// its mark).
			s.scheduleChecked[sched.ID] = now
			continue
		}
		if !now.After(lastChecked) {
			// Clock step-back (an NTP correction): moving the mark backwards
			// would re-open consumed windows once the clock recovers, so the
			// mark stays and the schedule skips this pass. The forward walk
			// resumes as soon as now passes the mark again.
			continue
		}
		s.scheduleChecked[sched.ID] = now
		latest, n, capped := cronMatchesIn(expr, lastChecked, now, scheduleMatchCap)
		if capped {
			s.log.Warn("spawn: schedule match walk capped; keeping only the latest match",
				"component", "afk", "repo", repo.Name, "schedule", sched.Name, "cap", scheduleMatchCap)
		}
		if n > 0 {
			// The LATEST match is the one owed firing. A pending held from a
			// previous pass (an at-cap retry) is superseded by its own next
			// due time — dropped LOUDLY (ADR-0062). Fresh multiple matches
			// within one window (an every-minute cadence at a 45s tick) are
			// normal and collapse to the latest silently.
			if prior, held := s.schedulePending[sched.ID]; held {
				s.log.Warn("schedule firing superseded by its own next due time",
					"component", "afk", "repo", repo.Name, "schedule", sched.Name,
					"old_due", prior, "new_due", latest)
			}
			s.schedulePending[sched.ID] = latest
		}
		due, pending := s.schedulePending[sched.ID]
		if !pending {
			continue
		}

		// Skip-on-overlap, FIRST (before any cap or auth consideration): a
		// firing owed while the Schedule's previous run is still live is
		// consumed and logged loudly, never queued (ADR-0062). The locked
		// launch re-checks this — the producer's read is the cheap first
		// line, the lock the authority.
		if prev, err := s.store.ActiveRunForSchedule(ctx, sched.ID); err == nil {
			delete(s.schedulePending, sched.ID)
			s.log.Warn("schedule firing skipped: previous run still live",
				"component", "afk", "repo", repo.Name, "schedule", sched.Name,
				"due", due, "run", prev.ID, "session", prev.SessionName)
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			// Ambiguous read: keep the pending and retry next pass rather
			// than consuming a firing on missing data.
			s.log.Warn("spawn: active run for schedule", "component", "afk",
				"repo", repo.Name, "schedule", sched.Name, "err", err)
			continue
		}

		// At-cap production veto against the pass snapshot — a load-shedding
		// hint, never enforcement (the pass owns the cap): the pending firing
		// survives and retries next pass, the same semantics the locked
		// launch's own at-cap answer produces.
		if liveCount >= s.instances.EffectiveCap(ctx, repo) {
			continue
		}
		// The firing-effective provider (the per-Schedule override as the
		// topmost skip-layer rung) gates the shared forced-auth memo like
		// every producer; the locked launch re-resolves it authoritatively.
		// Logged out → no candidate this pass, pending KEPT (it retries).
		prov, err := s.instances.ResolveScheduleProvider(ctx, repo, sched.Provider)
		if err != nil {
			s.log.Warn("spawn: resolving schedule provider", "component", "afk",
				"repo", repo.Name, "schedule", sched.Name, "err", err)
			continue
		}
		if _, checked := loggedIn[prov.ID()]; !checked {
			st, _ := prov.AuthStatus(ctx, true)
			loggedIn[prov.ID()] = st.LoggedIn
		}
		if !loggedIn[prov.ID()] {
			s.log.Info("spawn: schedule provider logged out; firing retries next pass",
				"component", "afk", "repo", repo.Name, "schedule", sched.Name, "provider", prov.ID())
			continue
		}

		scheduleID := sched.ID
		out = append(out, spawnCandidate{
			stage: StageScheduled,
			repo:  repo,
			label: "sched " + repo.Name + "/" + sched.Name,
			launch: func(ctx context.Context) spawnOutcome {
				// Only the locked launch knows whether the firing was spent:
				// spawned and every deterministic skip consume it; at-cap and
				// the transient reads (store, resolver, session listing) keep
				// it for the next pass's retry — one flaky read must not
				// swallow a weekly firing. launchScheduled logs each cause.
				outcome, consumed := s.launchScheduled(ctx, scheduleID)
				if consumed {
					delete(s.schedulePending, scheduleID)
				}
				return outcome
			},
		})
	}
	return out
}

// logMissedSlots is the once-at-startup missed-firing Warn: for each enabled
// Schedule with a last_fired_at baseline, count the cron matches that fell
// while the server was down — in (last_fired_at, engine start] — and say so
// loudly, because no catch-up means nothing else ever will (ADR-0062).
func (s *Service) logMissedSlots(schedules []store.Schedule, byRepoID map[string]store.Repo) {
	for _, sched := range schedules {
		if sched.LastFiredAt == nil {
			continue
		}
		expr, err := cronx.Parse(sched.Cadence)
		if err != nil {
			continue // the per-pass walk below logs the parse error loudly
		}
		_, n, capped := cronMatchesIn(expr, *sched.LastFiredAt, s.scheduleEngineStart, scheduleMatchCap)
		if n == 0 {
			continue
		}
		missed := strconv.Itoa(n)
		if capped {
			missed = "many"
		}
		repoName := sched.RepoID
		if repo, ok := byRepoID[sched.RepoID]; ok {
			repoName = repo.Name
		}
		s.log.Warn("schedule missed firing slot(s) while the server was down; the next cron match fires normally",
			"component", "afk", "repo", repoName, "schedule", sched.Name, "missed", missed)
	}
}

// launchScheduled is the locked launch path of a Schedule firing, launch()'s
// sibling MINUS issue selection (a scheduled run has no issue, no claim, and
// no done-signal): re-read everything under the engine lock, re-check the
// overlap (the lock is the authority — the producer's check was a hint),
// resolve the knobs with the per-Schedule overrides as the topmost
// skip-layer rung, guard the cap on fresh liveness, and drive the shared
// instance.Launch core with the scheduled identity — label
// sched-<id-suffix>-<stamp>, branch = the repo's manual branch prefix +
// label (the exact manual-instance composition, so reconcile's fallback
// derives it unchanged), the 30-minute default budget or the per-Schedule
// override, and the composed Schedule prompt VERBATIM (no token
// interpolation, no afk_prompt override — those are AFK-issue prompts).
//
// The answer is (outcome, consumed): consumed says whether the pending
// firing was spent, and the producer's closure deletes the memo entry only
// then. Consumed (pinned): spawned; vanished (ErrNotFound), disabled or
// paused, repo gone or not clone-ready, overlap, and a hard non-cap launch
// error — retrying a deterministic failure every tick is auto-retry-shaped
// runaway; the next cron match tries again. Kept (consumed=false, each
// cause Warn-logged): at-cap (the pass's retry semantics) and every
// transient read — non-NotFound store errors, resolver errors, a failed
// session listing — because consuming a weekly firing over one flaky read
// loses real work. On success the firing is stamped durable via
// MarkScheduleFired.
func (s *Service) launchScheduled(ctx context.Context, scheduleID string) (outcome spawnOutcome, consumed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Re-read the Schedule under the lock: deleted, disabled, or paused
	// since the gather → the firing dissolves; an unreadable row is a
	// transient the next pass retries.
	sched, err := s.store.ScheduleByID(ctx, scheduleID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.log.Warn("schedule launch: schedule vanished", "component", "afk", "schedule", scheduleID, "err", err)
			return spawnSkipped, true
		}
		s.log.Warn("schedule launch: read schedule; firing kept for retry",
			"component", "afk", "schedule", scheduleID, "err", err)
		return spawnSkipped, false
	}
	if !sched.Enabled || sched.Paused {
		s.log.Info("schedule launch: schedule disabled or paused meanwhile; firing dropped",
			"component", "afk", "schedule", sched.Name)
		return spawnSkipped, true
	}
	// Re-read the repo under the lock: branch prefix, budgets, caps, and the
	// clone state must be current, not a gather snapshot.
	repo, err := s.store.RepoByID(ctx, sched.RepoID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.log.Warn("schedule launch: repo vanished", "component", "afk", "schedule", sched.Name, "err", err)
			return spawnSkipped, true
		}
		s.log.Warn("schedule launch: read repo; firing kept for retry",
			"component", "afk", "schedule", sched.Name, "err", err)
		return spawnSkipped, false
	}
	if repo.CloneStatus != store.CloneStatusReady {
		s.log.Warn("schedule launch: repo not ready; firing dropped",
			"component", "afk", "repo", repo.Name, "schedule", sched.Name)
		return spawnSkipped, true
	}

	// Overlap re-check under the lock — the authority (the producer checked,
	// but a manual pass or a racing tick may have fired meanwhile). Skip-on-
	// overlap consumes the firing, loudly, never queues it (ADR-0062); an
	// ambiguous read keeps it — never consume a firing on missing data.
	if prev, err := s.store.ActiveRunForSchedule(ctx, sched.ID); err == nil {
		s.log.Warn("schedule firing skipped: previous run still live",
			"component", "afk", "repo", repo.Name, "schedule", sched.Name,
			"run", prev.ID, "session", prev.SessionName)
		return spawnSkipped, true
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("schedule launch: active run for schedule; firing kept for retry",
			"component", "afk", "repo", repo.Name, "schedule", sched.Name, "err", err)
		return spawnSkipped, false
	}

	// A firing with nothing to say must not launch: it would burn its whole
	// budget on an empty brief and classify success forever. The composed
	// prompt goes empty when the Schedule's prompt cleared under concurrent
	// edits, or when every selected flow key is unknown to this binary (a
	// retired catalog entry surviving in the row). TrimSpace is the API's own
	// emptiness test — deliberately no deeper unicode chase here.
	seed := ComposeSchedulePrompt(sched.Prompt, sched.Flows, sched.Name)
	if strings.TrimSpace(seed) == "" {
		s.log.Error("schedule composed an empty prompt; firing skipped — check the schedule's prompt and flows",
			"component", "afk", "repo", repo.Name, "schedule", sched.Name)
		return spawnSkipped, true
	}

	// Knob resolution (ADR-0062 rides ADR-0021/0030 unchanged): the
	// per-Schedule provider/model/effort overrides are one more DEFAULT
	// layer above the AFK overrides, skip-layer — a stale stored value
	// degrades the firing to the inherited chain, never breaks it. Options
	// bag and remote are plain AFK resolution (no per-Schedule override
	// exists): ultracode from afk_options applying to a scheduled prompt is
	// correct and pinned. All opaque here, exactly like launch().
	prov, err := s.instances.ResolveScheduleProvider(ctx, repo, sched.Provider)
	if err != nil {
		s.log.Warn("schedule launch: resolve provider; firing kept for retry",
			"component", "afk", "repo", repo.Name, "schedule", sched.Name, "err", err)
		return spawnSkipped, false
	}
	model, effort, err := s.instances.ResolveScheduleModelEffort(ctx, prov, repo, sched.Model, sched.Effort)
	if err != nil {
		s.log.Warn("schedule launch: resolve model/effort; firing kept for retry",
			"component", "afk", "repo", repo.Name, "schedule", sched.Name, "err", err)
		return spawnSkipped, false
	}
	options, err := s.instances.ResolveSpawnOptions(ctx, prov, repo, store.RunKindScheduled)
	if err != nil {
		s.log.Warn("schedule launch: resolve options; firing kept for retry",
			"component", "afk", "repo", repo.Name, "schedule", sched.Name, "err", err)
		return spawnSkipped, false
	}
	remote, err := s.instances.ResolveRemote(ctx, prov, repo, store.RunKindScheduled, nil)
	if err != nil {
		s.log.Warn("schedule launch: resolve remote; firing kept for retry",
			"component", "afk", "repo", repo.Name, "schedule", sched.Name, "err", err)
		return spawnSkipped, false
	}

	// Cap guard on fresh liveness, exactly like launch(): nothing is created
	// yet, so at-cap is a pure no-op the pass retries (the pending firing
	// survives at the producer). An unreadable listing is the same transient
	// stance — retry, never consume.
	live, err := s.runner.List(ctx)
	if err != nil {
		s.log.Warn("schedule launch: list sessions; firing kept for retry",
			"component", "afk", "repo", repo.Name, "schedule", sched.Name, "err", err)
		return spawnSkipped, false
	}
	if instance.LiveInstanceCount(live) >= s.instances.EffectiveCap(ctx, repo) {
		return spawnAtCap, false
	}

	// The scheduled identity: label sched-<id-suffix>-<stamp>; branch is
	// EXACTLY manual prefix + label (the manual-instance composition,
	// instance.Start — reconcile's instanceBranch fallback derives the same
	// string for the unknown label, so re-adoption and parked-work handling
	// work by construction). Session and worktree through the same gitx
	// helpers every kind uses. Budget: per-Schedule override else the
	// 30-minute default; the token dies runTokenSlack past the deadline
	// (§3a, the AFK rule).
	label := ScheduleLabel(sched.ID, s.now())
	branch := repo.ManualBranchPrefix + label
	budget := time.Duration(defaultScheduleBudgetMinutes) * time.Minute
	if sched.BudgetMinutes != nil && *sched.BudgetMinutes > 0 {
		budget = time.Duration(*sched.BudgetMinutes) * time.Minute
	}
	deadline := s.now().Add(budget)
	tokenExpiry := deadline.Add(runTokenSlack)

	run, err := s.instances.Launch(ctx, instance.LaunchSpec{
		Repo:           repo,
		Provider:       prov,
		Kind:           store.RunKindScheduled,
		IssueNumber:    nil, // no issue, no claim — the firing link is ScheduleID
		ScheduleID:     &sched.ID,
		SessionName:    gitx.ComposeSessionName(repo.Name, label),
		Branch:         branch,
		WorktreePath:   filepath.Join(s.worktreeRoot, gitx.WorktreeDir(repo.Name, label)),
		Model:          model,
		Effort:         effort,
		Remote:         remote,
		Options:        options,
		BudgetDeadline: &deadline,
		TokenExpiry:    &tokenExpiry,
		// The composed prompt, VERBATIM (ADR-0062): the Schedule's freeform
		// prompt plus the selected flows' blocks in catalog order — composed
		// and emptiness-guarded above. No ResolveAFKPrompt — the afk_prompt
		// overrides are AFK-issue prompts, not schedule prompts — and no
		// <N>/<BRANCH> interpolation: a schedule prompt has no issue and
		// names no branch.
		SeedPrompt: seed,
	})
	if err != nil {
		// Defensive ErrOverCap carve-out: instance.Launch raises no
		// ErrOverCap today (the guard above is this path's cap authority),
		// but a future at-cap answer from the shared core must retry like
		// that guard, never consume the firing. Every OTHER launch error
		// consumes it (pinned): retrying a deterministic launch failure
		// every tick is auto-retry-shaped runaway — the next cron match
		// tries again.
		if errors.Is(err, instance.ErrOverCap) {
			return spawnAtCap, false
		}
		s.log.Warn("schedule launch failed; firing consumed", "component", "afk",
			"repo", repo.Name, "schedule", sched.Name, "err", err)
		return spawnSkipped, true
	}
	// The firing is durable: last_fired_at is the startup missed-slot log's
	// baseline. A write failure only degrades that log — never the run.
	if err := s.store.MarkScheduleFired(ctx, sched.ID, s.now()); err != nil {
		s.log.Warn("schedule launch: mark fired", "component", "afk",
			"repo", repo.Name, "schedule", sched.Name, "err", err)
	}
	s.log.Info("schedule fired", "component", "afk",
		"repo", repo.Name, "schedule", sched.Name, "session", run.SessionName)
	return spawnSpawned, true
}
