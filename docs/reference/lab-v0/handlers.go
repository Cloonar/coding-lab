package main

import (
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

//go:embed templates/index.html
var indexHTML string

// loginSession is the fixed tmux session name for the global `claude auth
// login` flow. Fixed rather than per-project because login is one
// machine-level fact: once logged in, every project's remote-control session is
// authenticated.
const loginSession = "lab-login"

// maxLoginCodeLen caps a pasted OAuth code before it reaches tmux send-keys.
// The real value is a short URL-safe string; this just stops a runaway paste.
const maxLoginCodeLen = 4096

// defaultMaxInstances is the fallback concurrent-instance cap. NewServer seeds
// it so tests get a sane value; main overrides it from -max-instances.
const defaultMaxInstances = 6

type Server struct {
	root      string
	claudeCfg string
	sessions  *Sessions
	store     *Store
	auth      *Auth
	tmpl      *template.Template

	// tracker and git are the AFK-run seams over the `tea` and `git` CLIs
	// (issue claim, worktree lifecycle, Forgejo detection). main wires the real
	// shellout implementations; tests substitute fakes. Both nil disables AFK
	// runs — the menu degrades to its disabled line.
	tracker Tracker
	git     Git
	// worktreeRoot is where AFK-run worktrees live (<state>/lab/worktrees),
	// deliberately outside the projects scan root so a worktree's .git file is
	// never mistaken for a project.
	worktreeRoot string

	// forgejoCache memoises Forgejo detection per project path (remotes don't
	// change within a process lifetime). Guarded by forgejoMu since snapshot
	// runs it from the render path on every poll.
	forgejoMu    sync.Mutex
	forgejoCache map[string]forgejoInfo

	// afkMu serialises an AFK run's select→claim→spawn so two concurrent starts
	// can't both grab the same issue (the second would otherwise claim it, fail
	// its worktree, and roll the label back out from under the first). ADR-0007
	// makes this single-threaded claim the source of the run's race-freedom; the
	// later auto-scheduler (#64) takes over the role. Starts are rare and
	// user-initiated, so the brief global serialisation is invisible in practice.
	afkMu sync.Mutex

	// afkStarts is the in-memory budget clock for live AFK runs, keyed by tmux
	// session name and stamped lazily the first time the watcher sees a run (see
	// watchAFKRuns). Never persisted: a lab restart re-adopts in-flight runs from
	// their session names with the clock reset to the restart, which ADR-0007
	// accepts. afkRunsMu guards it AND serialises the watcher's stamp + per-run
	// reap decision (classifyAndClaimAFKRun) against handleStop's atomic
	// kill-and-forget (stopInstance), so a manually-stopped run is never reaped
	// through the chokepoint as a failure.
	afkRunsMu sync.Mutex
	afkStarts map[string]time.Time

	// afkReadyMu guards afkReady, the in-memory ready-for-agent count cache the
	// auto scheduler writes each sweep and the render path reads for the ⋯ menu's
	// "Start AFK run (N ready)" hint. Keyed by project name (like AutoEnabled /
	// ConsecutiveFailures in the store). Deliberately NOT persisted: a count
	// carried across a restart would be stale and misleading, so the map starts
	// empty and re-derives within one sweep interval — every project reads
	// "unknown" until its first post-restart sweep. A present entry is a real
	// observed count (zero included); an absent one is "unknown". The render path
	// only ever reads it (a locked map lookup), so no tea call is added there.
	afkReadyMu sync.Mutex
	afkReady   map[string]int

	// maxInstances caps concurrent project instances (the login session is
	// excluded). Start / New instance are disabled once the live count reaches
	// it; set from the -max-instances flag, with a default seeded by NewServer.
	maxInstances int

	// loginArgv spawns `claude auth login --claudeai`; loginDir is its cwd
	// ($HOME). --claudeai skips the subscription-vs-console picker. --model,
	// --effort and --permission-mode don't apply here — those are
	// remote-control flags.
	loginArgv []string
	loginDir  string

	// now is the clock, a field so a test can pin it. handleStart reads it to
	// stamp a manual instance's <timestamp> label (and the recency mark), so an
	// otherwise time-dependent session name is deterministic under test — and a
	// same-minute collision deterministically exercises the minute-bump in
	// uniqueManualLabel. NewServer seeds time.Now.
	now func() time.Time

	// captureTimeout bounds the synchronous pane-scrape of the login /oauth
	// link. bridgeTimeout bounds the background registry poll for a spawned
	// session's remote-control deep link (startCapture / registry.go): generous,
	// because it must cover claude's full boot plus the bridge connect and the
	// poll blocks nothing — while a miss strands the row on the generic link
	// until the next lab restart re-captures it. loginTimeout bounds
	// the post-code poll of auth status; loginPoll is the gap between polls.
	// authTTL is how long a login-status result stays fresh before re-running.
	// Fields so tests can shrink them without touching the production values.
	captureTimeout time.Duration
	bridgeTimeout  time.Duration
	loginTimeout   time.Duration
	loginPoll      time.Duration
	authTTL        time.Duration

	// registryDir is claude's per-process session registry (~/.claude/sessions),
	// the source startCapture reads a session's deep link from — see registry.go.
	// NewServer derives it from loginDir, which main wires to $HOME; a field so
	// tests can point it at a scratch dir.
	registryDir string

	// authMu guards the lazy login-status cache. refreshAuthLocked runs the
	// ~0.75s status command while holding it; for a single-user dev tool that
	// brief render-path serialisation is acceptable.
	authMu      sync.Mutex
	authState   bool
	authChecked time.Time

	// captureMu guards capturing, the set of session names with an in-flight
	// background deep-link scrape (see startCapture). While a name is in the
	// set the index renders that instance as "connecting…" instead of offering
	// a link; the set is in-memory only, so a lab restart re-derives it from the
	// startup heal rather than persisting it.
	captureMu sync.Mutex
	capturing map[string]bool

	// startingMu guards starting, the set of session names whose worktree + branch
	// have been created but whose tmux session is not live yet. A Start creates the
	// worktree BEFORE the session (see startInstance / launchAFKRun), so without this
	// the runtime worktree sweep (reconcile.go) could read a not-yet-live instance as
	// an orphan and GC its merged, still-clean worktree out from under it — a fresh
	// branch forked from origin/<default> reads as "merged", so ownership, not the
	// merged-check, is the only guard. The sweep treats a starting session's branch
	// as owned; entries are added before AddWorktree and cleared once the session is
	// live (or the Start has rolled back). In-memory only, like capturing.
	startingMu sync.Mutex
	starting   map[string]bool

	// loginMu guards the in-memory OAuth URL scraped during a login attempt.
	// Never persisted — re-scraped from the live pane if lab restarts mid-login.
	loginMu  sync.Mutex
	loginURL string
}

// instanceView is one live instance of a project as the index renders it. Name
// is the full tmux session name and doubles as the deep-link store key.
// Connecting is true while the background deep-link scrape is still in flight
// (URL not yet known): the row shows "connecting…" rather than a link. Once the
// scrape lands a URL it renders that; once it gives up with no URL the row falls
// back to the generic claude.ai link at render time (URL stays "").
type instanceView struct {
	Name       string
	URL        string
	Connecting bool

	// Label and Time are a manual instance's rendered identity, split from its
	// <userlabel>-<timestamp> / <timestamp> label by parseManualLabel: Label is
	// the user-supplied portion ("" when unlabelled) and Time is the HH:MM the
	// timestamp encodes. Ident joins them ("<label> · 15:30" / "15:30"). Both are
	// empty for an AFK row, which renders AFK #Issue instead.
	Label string
	Time  string

	// AFK marks an unattended AFK run (instance label afk-<N>); Issue is the N
	// it resolves. The card renders these as an "AFK #N" badge instead of the
	// manual instance's "<label> · 15:30" identifier.
	AFK   bool
	Issue int
}

// Ident is a manual instance's identity-chip text: "<label> · 15:30" when
// labelled, "15:30" when unlabelled, or just "<label>" for a label with no
// parseable timestamp (a legacy/hand-made session). AFK rows don't call this —
// the template renders "AFK #Issue" for them.
func (v instanceView) Ident() string {
	switch {
	case v.Label != "" && v.Time != "":
		return v.Label + " · " + v.Time
	case v.Label != "":
		return v.Label
	default:
		return v.Time
	}
}

// projectGroup is one project row plus its live instances (in slot order). An
// empty Instances slice is an idle project — the row still renders so it can be
// started again.
type projectGroup struct {
	Name      string
	Path      string
	Instances []instanceView

	// Forgejo is true when the project's origin is on git.cloonar.com, so its
	// ⋯ menu offers an enabled Start AFK run; otherwise the menu shows the
	// disabled "needs a git.cloonar.com repo" line.
	Forgejo bool

	// RepoURL is the project's Forgejo repo home (https://git.cloonar.com/<owner>/<repo>),
	// the target of the ⋯ menu's "Repository ↗" link. Empty for a non-Forgejo
	// project, so the template renders the row only inside the {{if .Forgejo}}
	// block. Populated from forgejoInfo.RepoURL() at snapshot, sharing forgejoHost
	// as the single host with Forgejo above.
	RepoURL string

	// AutoEnabled is the persisted automatic-AFK toggle. The ⋯ menu renders it
	// (Forgejo projects only) as a form-button whose label reads On/Off — server
	// text the morph syncs, never an <input type=checkbox> (which the morph would
	// treat as client-owned and never repaint from the server).
	AutoEnabled bool

	// ConsecutiveFailures is the persisted count of back-to-back failed AFK runs
	// (Forgejo projects only); Paused is the derived three-strikes flag
	// (count >= afkPauseThreshold). When Paused the scheduler launches no auto
	// runs and the ⋯ menu surfaces "Auto paused · N fails" plus a Reset
	// form-button — both server-rendered text/forms the morph syncs, never
	// client-owned state.
	ConsecutiveFailures int
	Paused              bool

	// ReadyCount is the scheduler's cached ready-for-agent count for this project;
	// ReadyKnown distinguishes the three render states a bare int cannot — unknown
	// (no cache entry: the scheduler hasn't swept this project yet), cached N>0, and
	// cached zero. snapshot fills both from the in-memory afkReady cache (read-only,
	// no tea), for Forgejo projects only. The display decision (whether to show a
	// suffix, and whether to grey the control at zero) lives in afkStartHint, which
	// also gates on AutoEnabled so a cold auto-off project shows no count — see
	// AFKStart, which the template calls.
	ReadyCount int
	ReadyKnown bool

	// ParkedCount is how many parked lab//afk/ branches this project has — managed
	// branches no live session owns (gatherParked). The card renders a collapsed
	// "Parked (N)" strip only when it is > 0; the strip's per-entry detail is fetched
	// lazily on expand (handleParked), off this poll path. Filled by snapshot from
	// the same local-only git read for every project (Forgejo or not — manual lab/
	// branches park anywhere), so the count costs no tea/network call (ADR-0017
	// slice 3).
	ParkedCount int

	// openedAt drives the recency sort only; not rendered. Zero => unstamped.
	openedAt time.Time
}

// pageData is the index template's model: the project groups, the global
// instance-cap state, and the machine-wide Claude login state that decides
// which banner (if any) renders.
type pageData struct {
	Groups       []projectGroup
	AtCap        bool // live instance count has reached MaxInstances
	LiveCount    int  // live project instances (login excluded) — the N in "N/M"
	MaxInstances int  // shown in the cap hint
	LoggedIn     bool
	LoginActive  bool   // a lab-login session exists — login is in progress
	LoginURL     string // scraped OAuth authorize link, shown while LoginActive
	LoginError   bool   // transient "that didn't work" hint after a failed attempt
	ActionError  bool   // no-JS flash: a Start/Stop/etc. failed (?error=action)
	Notice       string // no-JS flash: a specific, non-error message (e.g. no ready issues)

	// Spawn-config control (#156): the persisted global model + effort that govern
	// new spawns, plus the closed allowlists that back the two dropdowns. Rendered
	// in the static page shell OUTSIDE #live (like the filter input) so a
	// background poll never resets a half-made selection; the #live fragment path
	// ignores these fields entirely.
	SpawnModel    string
	SpawnEffort   string
	ModelOptions  []modelOption
	EffortOptions []string
}

func NewServer(root, claudeCfg string, sessions *Sessions, store *Store, auth *Auth, loginArgv []string, loginDir string) *Server {
	tmpl := template.Must(template.New("index").Parse(indexHTML))
	return &Server{
		root:           root,
		claudeCfg:      claudeCfg,
		sessions:       sessions,
		store:          store,
		auth:           auth,
		tmpl:           tmpl,
		maxInstances:   defaultMaxInstances,
		capturing:      map[string]bool{},
		starting:       map[string]bool{},
		forgejoCache:   map[string]forgejoInfo{},
		afkStarts:      map[string]time.Time{},
		afkReady:       map[string]int{},
		now:            time.Now,
		loginArgv:      loginArgv,
		loginDir:       loginDir,
		registryDir:    filepath.Join(loginDir, ".claude", "sessions"),
		captureTimeout: 3 * time.Second,
		bridgeTimeout:  30 * time.Second,
		loginTimeout:   20 * time.Second,
		loginPoll:      1 * time.Second,
		authTTL:        30 * time.Second,
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/fragment", s.handleFragment)
	mux.HandleFunc("/start/", s.handleStart)
	mux.HandleFunc("/afk/start/", s.handleAFKStart)
	mux.HandleFunc("/afk/auto/", s.handleAFKAuto)
	mux.HandleFunc("/afk/reset/", s.handleAFKReset)
	mux.HandleFunc("/stop/", s.handleStop)
	mux.HandleFunc("/stop-all/", s.handleStopAll)
	// Global spawn-config setter (#156): exact path (no trailing slash) — it is a
	// single global setting, not a per-project resource like /start/<name>.
	mux.HandleFunc("/spawn-config", s.handleSpawnConfig)
	mux.HandleFunc("/login/start", s.handleLoginStart)
	mux.HandleFunc("/login/code", s.handleLoginCode)
	// Parked view (ADR-0017 slice 3): the lazy per-project detail endpoint and the
	// per-entry Discard, both under /parked/ (see parked.go).
	s.parkedRoutes(mux)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := s.indexData(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		log.Printf("template execute: %v", err)
	}
}

// handleFragment serves just the #live partial (status line, login banner,
// filter, project list) that the client-side poller swaps into the page. GET
// only — it does the same snapshot as the index but renders the fragment alone,
// so a poll is one cheap round-trip with no full-page chrome.
func (s *Server) handleFragment(w http.ResponseWriter, r *http.Request) {
	s.renderFragment(w, r)
}

// fragmentHeader is the request header lab's own fetch() sets. When present,
// action handlers answer with the rendered #live fragment (success) or a bare
// error message (failure) instead of the 303 redirect the no-JS form posts rely
// on. See ok / fail.
const fragmentHeader = "X-Lab-Fragment"

func wantsFragment(r *http.Request) bool { return r.Header.Get(fragmentHeader) == "1" }

// renderFragment writes the #live partial for the current state. Shared by the
// poll endpoint and by ok() after a successful fetch-driven action.
func (s *Server) renderFragment(w http.ResponseWriter, r *http.Request) {
	data, err := s.indexData(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "live", data); err != nil {
		log.Printf("fragment execute: %v", err)
	}
}

// ok ends a successful (or harmlessly-refused) action. A fetch-driven request
// gets the freshly-rendered #live fragment so the page updates in place; a plain
// form post falls back to the 303-redirect-to-index the no-JS path expects.
func (s *Server) ok(w http.ResponseWriter, r *http.Request) {
	if wantsFragment(r) {
		s.renderFragment(w, r)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// fail ends a failed action. The fetch path gets the real error message as a
// plain-text body at status so the sticky banner can show it (the client then
// re-polls to resync); the no-JS path is bounced to the index with a generic
// ?error=action flash that renders the same banner server-side.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error, status int) {
	if wantsFragment(r) {
		http.Error(w, err.Error(), status)
		return
	}
	http.Redirect(w, r, "/?error=action", http.StatusSeeOther)
}

// failLogin is fail's login-flow sibling: the no-JS path lands on the login
// banner's "that didn't work" hint (?error=login) instead of the generic action
// flash, since the user is mid-login and the banner is where the retry lives.
func (s *Server) failLogin(w http.ResponseWriter, r *http.Request, err error, status int) {
	if wantsFragment(r) {
		http.Error(w, err.Error(), status)
		return
	}
	http.Redirect(w, r, "/?error=login", http.StatusSeeOther)
}

// indexData assembles the full page model: the project snapshot plus the login
// banner state. The login session is only inspected when logged out, so the
// logged-in render does no extra tmux work.
func (s *Server) indexData(r *http.Request) (pageData, error) {
	groups, atCap, err := s.snapshot()
	if err != nil {
		return pageData{}, err
	}
	liveCount := 0
	for _, g := range groups {
		liveCount += len(g.Instances)
	}
	// The global spawn setting + its allowlists, read once per render. The fragment
	// path drops them (the selector is outside #live); the full page renders the
	// two dropdowns with the persisted value pre-selected (#156).
	spawnModel, spawnEffort := s.store.SpawnConfig()
	data := pageData{
		Groups:        groups,
		AtCap:         atCap,
		LiveCount:     liveCount,
		MaxInstances:  s.maxInstances,
		LoggedIn:      s.loggedIn(),
		LoginError:    r.URL.Query().Get("error") == "login",
		ActionError:   r.URL.Query().Get("error") == "action",
		SpawnModel:    spawnModel,
		SpawnEffort:   spawnEffort,
		ModelOptions:  spawnModels,
		EffortOptions: spawnEfforts,
	}
	if r.URL.Query().Get("notice") == "no-ready" {
		data.Notice = noReadyNotice
	}
	if data.LoggedIn {
		return data, nil
	}
	active, err := s.sessions.IsRunning(loginSession)
	if err != nil {
		return pageData{}, fmt.Errorf("login session check: %w", err)
	}
	data.LoginActive = active
	if active {
		url := s.getLoginURL()
		if url == "" {
			// lab restarted mid-login: the in-memory URL is gone but the tmux
			// session survives, so recover the link straight from the pane.
			if u := s.sessions.ScrapeOAuthURL(loginSession); u != "" {
				s.setLoginURL(u)
				url = u
			}
		}
		data.LoginURL = url
	}
	return data, nil
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	// Guard: a remote-control session spawned while logged out hits the login
	// wall immediately. Refuse it (the UI already blocks the button) and send
	// the user back to the banner instead of leaving a doomed session behind.
	// forceAuthRefresh (not the cached loggedIn) because the 30s render cache can
	// hide a silent token expiry — the Start button is rare and user-initiated,
	// so the ~1s status check is invisible here, and the refreshed state lands in
	// the fragment we render back so the banner appears in the same response.
	if !s.forceAuthRefresh() {
		s.ok(w, r)
		return
	}
	project := strings.TrimPrefix(r.URL.Path, "/start/")
	// dir is the project's reference repo — the worktree parent + fetch/branch
	// host, never an instance's cwd (ADR-0017).
	dir, err := s.projectDir(project)
	if err != nil {
		s.fail(w, r, err, http.StatusNotFound)
		return
	}
	// Worktrees are mandatory now (ADR-0017): every instance runs in its own,
	// forked from origin. main always wires git; guard defensively so a misconfig
	// fails loudly rather than nil-panicking.
	if s.git == nil {
		s.fail(w, r, errors.New("worktrees are not configured"), http.StatusInternalServerError)
		return
	}
	// One tmux listing serves both the cap check and the label-collision check.
	live, err := s.sessions.List()
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	// Server-side cap guard. The UI already disables the button at the cap, but a
	// direct POST must not be able to exceed it — bounce back to the index.
	if s.liveInstanceCount(live) >= s.maxInstances {
		s.ok(w, r)
		return
	}
	// Identity: <project>~<label>, label = <user>-<timestamp> (labelled) or bare
	// <timestamp> (unlabelled), the timestamp bumped a minute on a same-minute
	// collision so the session name, lab/<label> branch, and <project>-<label>
	// worktree it seeds all stay unique. The name is the tmux identity, the
	// deep-link key, and the claude --remote-control argument at once.
	taken := make(map[string]bool, len(live))
	for _, n := range live {
		taken[n] = true
	}
	label := uniqueManualLabel(project, sanitizeLabel(r.FormValue("label")), s.now(), taken)
	id := instanceID{Project: project, Label: label}
	name := composeSessionName(id)

	// Synchronous, fail-loud worktree creation off origin/<default>, with rollback
	// (ADR-0017): a repo with no usable origin, a failing fetch, or any spawn
	// failure aborts Start with the git cause and leaves nothing behind.
	if err := s.startInstance(name, dir, s.worktreePath(id), instanceBranch(id)); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	// Recency is keyed by project, not instance, so starting any instance floats
	// the whole project to the top. Stamp before the URL scrape so the sort is
	// correct even if the URL never appears (claude crashed, scrape window missed).
	if err := s.store.StampOpened(project, s.now()); err != nil {
		log.Printf("stamp opened %q: %v", project, err)
	}
	// Capture the deep link in the background so Start returns immediately
	// instead of blocking the request up to bridgeTimeout. Until it lands, the
	// index renders this instance as "connecting…"; see startCapture.
	s.startCapture(name)
	s.ok(w, r)
}

// startInstance is the synchronous, fail-loud Start core (ADR-0017): create the
// instance's worktree off freshly-fetched origin/<default>, seed trust into it,
// and spawn the session there — in the worktree, never the reference repo. It
// mirrors launchAFKRun's teardownClaim: any failure once the worktree exists
// rolls back BOTH the worktree and its branch, restoring the pre-Start state so
// nothing strands, and returns the git cause for the banner. A failed AddWorktree
// created nothing, so there is nothing to roll back.
func (s *Server) startInstance(name, repoDir, wt, branch string) error {
	// Protect this instance from the runtime sweep for the whole window between its
	// worktree being created (below) and its session going live — otherwise the
	// sweep could read the not-yet-live worktree as a merged, clean orphan and
	// remove it mid-Start (see Server.starting / reconcile.go).
	s.markStarting(name)
	defer s.clearStarting(name)
	teardown := func() {
		if err := s.git.RemoveWorktree(repoDir, wt); err != nil {
			log.Printf("start %s: rollback remove worktree: %v", name, err)
		}
		if err := s.git.DeleteBranch(repoDir, branch); err != nil {
			log.Printf("start %s: rollback delete branch: %v", name, err)
		}
	}
	if err := s.git.AddWorktree(repoDir, wt, branch); err != nil {
		return err
	}
	if err := SeedTrust(s.claudeCfg, wt); err != nil {
		teardown()
		return err
	}
	if err := s.sessions.Start(name, wt); err != nil {
		teardown()
		return err
	}
	return nil
}

// markStarting records a session as mid-Start — its worktree + branch exist but its
// tmux session is not live yet — so the runtime sweep treats its branch as owned and
// can't GC it mid-Start (see startingSnapshot and reconcile.go's gatherRefs).
// Cleared by clearStarting once the session is live or the Start has rolled back.
func (s *Server) markStarting(name string) {
	s.startingMu.Lock()
	defer s.startingMu.Unlock()
	s.starting[name] = true
}

// clearStarting drops a session from the starting set — once it is live (so the
// live listing now covers it) or its Start rolled back (so there is nothing left to
// protect). Safe to call for an unmarked name.
func (s *Server) clearStarting(name string) {
	s.startingMu.Lock()
	defer s.startingMu.Unlock()
	delete(s.starting, name)
}

// startingSnapshot is the current starting set as a slice, for the sweep to union
// with the live sessions when deciding which branches are off-limits.
func (s *Server) startingSnapshot() []string {
	s.startingMu.Lock()
	defer s.startingMu.Unlock()
	names := make([]string, 0, len(s.starting))
	for n := range s.starting {
		names = append(names, n)
	}
	return names
}

// handleStop stops a single instance, addressed by its full tmux session name
// (the path after /stop/). The deep link is keyed by that same name, so only
// this instance's link is forgotten; a stopped lone instance keeps its project's
// recency timestamp, so the project row stays put in the sort.
//
// A user-initiated Stop is neutral for an AFK run (ADR-0007), unchanged this
// slice: it keeps the worktree and its afk/<N> branch, so the issue stays
// claimed/parked for manual requeue (ADR-0013), and stopInstance forgets the run
// from the watcher's budget clock so the now-dead session is not later reaped as
// a death-failure. A MANUAL instance's Stop instead applies the guarded teardown
// (ADR-0017): a dirty worktree is kept whole, a clean one is removed and its
// lab/<label> branch deleted only if already merged into origin/<default>.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/stop/")
	if err := s.stopInstance(name); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	if err := s.store.ForgetURL(name); err != nil {
		log.Printf("forget url %q: %v", name, err)
	}
	s.ok(w, r)
}

// stopInstance stops one session and tears down its workspace per kind. An AFK
// run is killed AND forgotten from the watcher's budget clock as one atomic step
// under afkRunsMu (unchanged this slice): that atomicity is what makes its Stop
// neutral — the watcher's reap decision (classifyAndClaimAFKRun) reads tracking
// and liveness under the same lock, so it only ever observes this whole step done
// or not-yet-started, never a killed-but-still-tracked run it would mistake for a
// death-failure — and the worktree + afk/<N> branch are kept (parked). A manual
// instance instead gets the guarded teardown (ADR-0017) after it is stopped.
func (s *Server) stopInstance(name string) error {
	if _, ok := parseAFKRun(name); ok {
		s.afkRunsMu.Lock()
		defer s.afkRunsMu.Unlock()
		if err := s.sessions.Stop(name); err != nil {
			return err
		}
		delete(s.afkStarts, name)
		return nil
	}
	if err := s.sessions.Stop(name); err != nil {
		return err
	}
	s.teardownInstance(name)
	return nil
}

// teardownInstance resolves an instance's reference repo, worktree, and branch
// from its session name and applies the one guarded teardown (ADR-0017). It is
// kind-agnostic — instanceBranch / worktreePath derive afk/<N> + <project>-<N> for
// an AFK run and lab/<label> + <project>-<label> for a manual one — so the manual
// Stop path and the AFK reaper share this single teardown (ADR-0017 slice 2).
// Best-effort and never fails its caller: the session is already gone, so a
// teardown hiccup only leaves a reclaimable worktree behind (startup reconciliation
// or the runtime sweep collects it). A nil git seam is a no-op.
func (s *Server) teardownInstance(name string) {
	if s.git == nil {
		return
	}
	id := parseSessionName(name)
	dir, err := s.projectDir(id.Project)
	if err != nil {
		log.Printf("teardown %s: project dir: %v", name, err)
		return
	}
	s.teardownGuarded(dir, s.worktreePath(id), instanceBranch(id))
}

// teardownGuarded applies lab's single guarded-teardown rule to a worktree +
// branch (ADR-0017): a dirty worktree is kept whole (unsaved work survives); a
// clean one has its worktree removed and its branch deleted only when already
// merged into origin/<default> (else the branch is kept so unmerged commits
// aren't lost). The decision is the pure decideTeardown; this wires it to the git
// seam, skipping the merged check on a dirty tree (where it doesn't matter).
// Conservative on uncertainty — an unreadable status keeps everything, since a
// clean worktree is reproducible from its branch but destroyed work is not.
// Best-effort and logged: a Stop must not fail on a teardown hiccup.
func (s *Server) teardownGuarded(repoDir, wt, branch string) {
	dirty, err := s.git.WorktreeDirty(wt)
	if err != nil {
		log.Printf("teardown %s: worktree status: %v (keeping worktree + branch)", wt, err)
		return
	}
	merged := false
	if !dirty {
		if merged, err = s.git.BranchMerged(repoDir, branch); err != nil {
			log.Printf("teardown %s: merged check: %v (keeping worktree + branch)", branch, err)
			return
		}
	}
	act := decideTeardown(dirty, merged)
	if act.RemoveWorktree {
		if err := s.git.RemoveWorktree(repoDir, wt); err != nil {
			log.Printf("teardown %s: remove worktree: %v", wt, err)
			return
		}
	}
	if act.DeleteBranch {
		if err := s.git.DeleteBranch(repoDir, branch); err != nil {
			log.Printf("teardown %s: delete branch: %v", branch, err)
		}
	}
}

// teardownAction is what the guarded teardown does with an instance's worktree +
// branch: whether to remove the worktree and whether to delete the branch.
type teardownAction struct {
	RemoveWorktree bool
	DeleteBranch   bool
}

// decideTeardown is the guarded-teardown rule as a pure function of two facts —
// is the worktree dirty, is the branch merged into origin/<default> — so it is
// table-testable in isolation from git (mirroring classifyAFKRun). A dirty
// worktree keeps everything; a clean one removes the worktree and deletes the
// branch only if merged. merged is meaningful only when not dirty (the caller
// skips the check on a dirty tree, which keeps the branch regardless).
func decideTeardown(dirty, merged bool) teardownAction {
	if dirty {
		return teardownAction{}
	}
	return teardownAction{RemoveWorktree: true, DeleteBranch: merged}
}

// handleStopAll stops every live instance of one project (the path after
// /stop-all/). belongsTo confines the kill to that exact project, so an instance
// of a similarly-named project (foo vs foobar) is never caught by mistake.
func (s *Server) handleStopAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	project := strings.TrimPrefix(r.URL.Path, "/stop-all/")
	live, err := s.sessions.List()
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	for _, name := range live {
		if name == loginSession || !belongsTo(name, project) {
			continue
		}
		if err := s.sessions.Stop(name); err != nil {
			s.fail(w, r, err, http.StatusInternalServerError)
			return
		}
		if err := s.store.ForgetURL(name); err != nil {
			log.Printf("forget url %q: %v", name, err)
		}
	}
	s.ok(w, r)
}

// handleSpawnConfig persists the global model + effort that govern every new
// spawn (#156). It is the single writer of that setting: it hands both values to
// the store's validated setter, which rejects anything outside the closed
// allowlists WITHOUT persisting — so a bad value can never break every future
// spawn. On success the standard #live fragment is returned (the selector itself
// lives outside #live and is client-owned, so the morph leaves the user's choice
// alone); on rejection the error is surfaced in the UI's banner via fail(). The
// change takes effect on the next spawn — running sessions are untouched.
func (s *Server) handleSpawnConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := s.store.SetSpawnConfig(r.FormValue("model"), r.FormValue("effort")); err != nil {
		s.fail(w, r, err, http.StatusBadRequest)
		return
	}
	s.ok(w, r)
}

// handleLoginStart (re)starts the global login flow. Killing any prior attempt
// first means a click always doubles as cancel/retry and always yields a fresh,
// non-expired OAuth URL.
func (s *Server) handleLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if err := s.sessions.Stop(loginSession); err != nil {
		s.failLogin(w, r, err, http.StatusInternalServerError)
		return
	}
	s.setLoginURL("")
	if err := s.sessions.StartCommand(loginSession, s.loginDir, s.loginArgv); err != nil {
		s.failLogin(w, r, err, http.StatusInternalServerError)
		return
	}
	if url := s.sessions.CaptureOAuthURL(loginSession, s.captureTimeout); url != "" {
		s.setLoginURL(url)
	}
	s.ok(w, r)
}

// handleLoginCode delivers the pasted OAuth code to the waiting login session,
// then polls auth status until login lands or we give up.
func (s *Server) handleLoginCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	code, err := validateLoginCode(r.FormValue("code"))
	if err != nil {
		// Bad input — the login session is still waiting, so bounce back to the
		// in-progress banner with the hint rather than tearing anything down.
		s.failLogin(w, r, errors.New("paste the code from the authorize page"), http.StatusBadRequest)
		return
	}
	if err := s.sessions.SendKeys(loginSession, code); err != nil {
		s.failLogin(w, r, err, http.StatusInternalServerError)
		return
	}
	// Poll auth status (force-refreshing past the cache) until login lands or
	// the deadline passes. On success `claude auth login` has already exited, so
	// its session is gone — nothing to clean up.
	deadline := time.Now().Add(s.loginTimeout)
	for {
		if s.forceAuthRefresh() {
			s.ok(w, r)
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(s.loginPoll)
	}
	// Timed out: tear down the stuck attempt and hint the user to retry.
	if err := s.sessions.Stop(loginSession); err != nil {
		log.Printf("stop %s after login timeout: %v", loginSession, err)
	}
	s.setLoginURL("")
	s.failLogin(w, r, errors.New("login did not complete in time — try again"), http.StatusGatewayTimeout)
}

// validateLoginCode trims and sanity-checks a pasted OAuth code before it is
// handed to tmux send-keys. The real value is a URL-safe code#state string, so
// "#", "=", and base64url characters must pass; only empty input, control
// characters (which could inject a premature Enter), and absurd lengths are
// rejected.
func validateLoginCode(raw string) (string, error) {
	code := strings.TrimSpace(raw)
	if code == "" {
		return "", errors.New("empty code")
	}
	if len(code) > maxLoginCodeLen {
		return "", errors.New("code too long")
	}
	for _, r := range code {
		if unicode.IsControl(r) {
			return "", errors.New("code contains a control character")
		}
	}
	return code, nil
}

// loggedIn returns the cached login state, refreshing it only when older than
// authTTL — so a render costs nothing most of the time.
func (s *Server) loggedIn() bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if time.Since(s.authChecked) > s.authTTL {
		s.refreshAuthLocked()
	}
	return s.authState
}

// forceAuthRefresh re-runs the status check unconditionally and returns the
// fresh result, used during/after a login attempt to bypass the cache.
func (s *Server) forceAuthRefresh() bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	s.refreshAuthLocked()
	return s.authState
}

func (s *Server) refreshAuthLocked() {
	ok, err := s.auth.LoggedIn()
	if err != nil {
		// Treat an unreadable status as logged-out: better to show the login
		// banner than to spawn doomed remote-control sessions.
		log.Printf("auth status: %v", err)
	}
	s.authState = ok
	s.authChecked = time.Now()
}

func (s *Server) getLoginURL() string {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	return s.loginURL
}

func (s *Server) setLoginURL(url string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.loginURL = url
}

// startCapture marks name as having an in-flight deep-link capture and kicks
// off a background goroutine to do it, so the caller (Start, the AFK claim, or
// the startup heal) returns without blocking on the up-to-bridgeTimeout poll.
// The link is read from claude's session registry, keyed by the instance's
// worktree — the one cwd unique to this session (ADR-0017) — because claude no
// longer prints it into the pane (see registry.go). The goroutine writes the
// URL to the store only if it finds one; a miss writes nothing, so the render
// falls back to the generic link rather than ever overwriting a real one.
// Idempotent: a second call while a capture is already in flight is a no-op,
// so it can't stack goroutines on the same session.
func (s *Server) startCapture(name string) {
	s.captureMu.Lock()
	if s.capturing[name] {
		s.captureMu.Unlock()
		return
	}
	s.capturing[name] = true
	s.captureMu.Unlock()

	dir := s.worktreePath(parseSessionName(name))
	go func() {
		defer func() {
			s.captureMu.Lock()
			delete(s.capturing, name)
			s.captureMu.Unlock()
		}()
		if url := captureBridgeURL(s.registryDir, dir, s.bridgeTimeout); url != "" {
			if err := s.store.SetURL(name, url); err != nil {
				log.Printf("set url %q: %v", name, err)
			}
		}
	}()
}

// isCapturing reports whether name has a scrape in flight — the "connecting…"
// state the index shows before a link is known.
func (s *Server) isCapturing(name string) bool {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	return s.capturing[name]
}

// snapshot assembles the grouped project view and the global at-cap flag. Each
// project row carries its live instances (in slot order); idle projects still
// appear so they can be started again. The cap counts every live instance except
// the login session, which must never consume a slot.
func (s *Server) snapshot() ([]projectGroup, bool, error) {
	projects, err := Scan(s.root)
	if err != nil {
		return nil, false, fmt.Errorf("scan: %w", err)
	}
	live, err := s.sessions.List()
	if err != nil {
		return nil, false, fmt.Errorf("tmux: %w", err)
	}
	atCap := s.liveInstanceCount(live) >= s.maxInstances

	groups := make([]projectGroup, len(projects))
	for i, p := range projects {
		// One Forgejo lookup feeds both the AFK gate (IsForgejo) and the repo link
		// (RepoURL), so the cached detection is read once per project per poll.
		fj := s.forgejoFor(p.Path)
		g := projectGroup{Name: p.Name, Path: p.Path, Forgejo: fj.IsForgejo, RepoURL: fj.RepoURL()}
		// The auto toggle and the pause state only render for Forgejo projects, so
		// only read them there (cheap store lookups either way; the gate keeps
		// non-Forgejo cards clean).
		if g.Forgejo {
			g.AutoEnabled = s.store.AutoEnabled(p.Name)
			g.ConsecutiveFailures = s.store.ConsecutiveFailures(p.Name)
			g.Paused = g.ConsecutiveFailures >= afkPauseThreshold
			// "Start AFK run (N ready)" hint: read the scheduler's cached count
			// (in-memory, no tea). afkStartHint gates the actual display on the auto
			// toggle, so reading here for every Forgejo project is harmless — a stale
			// entry left by a since-disabled toggle is simply not shown.
			g.ReadyCount, g.ReadyKnown = s.readyCount(p.Name)
		}
		// Collapsed Parked count: a local-only git read (for-each-ref + worktree
		// list), no tea/network, so it is cheap enough for every poll. Reuses the
		// shared `live` listing for the ownership exclusion. Independent of Forgejo —
		// a manual lab/<label> branch parks on any project — so it is gated only on
		// the git seam. A read error just yields no strip (0); the lazy endpoint is
		// where a real failure surfaces loud on expand.
		if s.git != nil {
			if parked, err := s.gatherParked(p.Path, p.Name, live); err == nil {
				g.ParkedCount = len(parked)
			}
		}
		for _, name := range live {
			if name == loginSession || !belongsTo(name, p.Name) {
				continue
			}
			id := parseSessionName(name)
			url := s.store.URL(name)
			issue, _, isAFK := parseAFKLabel(id.Label)
			iv := instanceView{
				Name: name,
				URL:  url,
				// "connecting…" only while the scrape is in flight and no link is
				// known yet; a known URL always wins, a finished-but-empty scrape
				// falls through to the render-time generic link.
				Connecting: url == "" && s.isCapturing(name),
				AFK:        isAFK,
				Issue:      issue,
			}
			// A manual instance renders "<label> · 15:30" / "15:30" from its label's
			// timestamp; an AFK run renders AFK #Issue and carries neither.
			if !isAFK {
				iv.Label, iv.Time = parseManualLabel(id.Label)
			}
			g.Instances = append(g.Instances, iv)
		}
		// Slots are gone (ADR-0017); order instances by session name for a stable,
		// deterministic render (timestamped manual labels sort chronologically).
		sort.Slice(g.Instances, func(a, b int) bool {
			return g.Instances[a].Name < g.Instances[b].Name
		})
		if t, ok := s.store.LastOpenedAt(p.Name); ok {
			g.openedAt = t
		}
		groups[i] = g
	}
	sortGroups(groups)
	return groups, atCap, nil
}

// liveInstanceCount counts live sessions that are project instances — every
// session except the global login one, which is not a project instance and must
// never count against the cap.
func (s *Server) liveInstanceCount(live []string) int {
	n := 0
	for _, name := range live {
		if name != loginSession {
			n++
		}
	}
	return n
}

// sortGroups orders groups by lastOpenedAt desc; groups with no timestamp fall
// to the bottom, sorted alphabetically by Name. Scan already returns projects
// alphabetical-by-Name, so the stable sort preserves that order within the
// unstamped tail without an explicit secondary key.
func sortGroups(groups []projectGroup) {
	sort.SliceStable(groups, func(i, j int) bool {
		ai, aj := groups[i].openedAt, groups[j].openedAt
		switch {
		case !ai.IsZero() && aj.IsZero():
			return true
		case ai.IsZero() && !aj.IsZero():
			return false
		case ai.IsZero() && aj.IsZero():
			return false // both unstamped: keep Scan's alphabetical order
		default:
			return ai.After(aj)
		}
	})
}

func (s *Server) projectDir(name string) (string, error) {
	projects, err := Scan(s.root)
	if err != nil {
		return "", err
	}
	for _, p := range projects {
		if p.Name == name {
			return p.Path, nil
		}
	}
	return "", errors.New("unknown project: " + name)
}
