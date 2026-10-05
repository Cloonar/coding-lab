package httpapi

// httptest suite for the readiness report and the repo summaries (issue #61).
// It runs the production wiring in miniature — ONE readiness recorder fed by
// the real git engine, the real tracker registry with the real Forgejo and
// GitHub REST clients, the real instance service and the real AFK engine —
// against a local forge stub, real bare repos, a fake tmux and a fake
// provider. Around that it mounts four tripwires, one per thing a page view
// must never do: every forge request goes through a counting transport, every
// git subprocess through a logging wrapper binary, every podman invocation
// through a recording exec seam, and the provider counts its status checks.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/events"
	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/instancehome"
	"git.cloonar.com/Cloonar/coding-lab/internal/logx"
	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider/providertest"
	"git.cloonar.com/Cloonar/coding-lab/internal/readiness"
	"git.cloonar.com/Cloonar/coding-lab/internal/reposvc"
	"git.cloonar.com/Cloonar/coding-lab/internal/startguard"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/builtin"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/forgejo"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/github"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

const (
	rdyDevImage   = "registry.test/acme/dev:1@sha256:0123"
	rdyToolsImage = "registry.test/lab/agent-tools@sha256:beef"
	rdyPodmanBin  = "podman-tripwire"
)

// forgeStub is the local forge: it answers the issue listings both REST
// clients read (the same JSON shape serves Forgejo and GitHub) from a script.
// It counts every request it receives.
type forgeStub struct {
	mu       sync.Mutex
	status   int              // non-200 → answered for every request
	body     string           // body of a non-200 answer
	issues   []map[string]any // the open issue set served on page 1
	requests []string
}

func (f *forgeStub) set(status int, body string, issues ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body, f.issues = status, body, issues
}

func (f *forgeStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if f.status != http.StatusOK {
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if !strings.HasSuffix(r.URL.Path, "/issues") || (r.URL.Query().Get("page") != "1" && r.URL.Query().Get("page") != "") {
		_, _ = io.WriteString(w, "[]")
		return
	}
	issues := f.issues
	if issues == nil {
		issues = []map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(issues)
}

// received is how many requests reached the stub.
func (f *forgeStub) received() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func forgeIssue(n int, labels ...string) map[string]any {
	ls := make([]map[string]any, 0, len(labels))
	for _, l := range labels {
		ls = append(ls, map[string]any{"name": l})
	}
	return map[string]any{
		"number": n, "title": fmt.Sprintf("issue %d", n), "state": "open", "labels": ls, "comments": 0,
		"created_at": "2026-07-01T12:00:00Z", "updated_at": "2026-07-01T12:00:00Z",
	}
}

// forgeTripwire is the http.RoundTripper under every forge REST client: it
// counts each request, whatever host it is for, and — once armed — fails the
// test for any request at all. A request that is refused here never reaches
// the stub, so an armed tripwire cannot be satisfied by accident.
type forgeTripwire struct {
	t     *testing.T
	mu    sync.Mutex
	armed bool
	fail  error // non-nil: every request fails with it, like a network fault
	seen  []string
}

func (f *forgeTripwire) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.String())
	armed, fail := f.armed, f.fail
	f.mu.Unlock()
	if armed {
		f.t.Errorf("a forge request was made while none is allowed: %s %s", r.Method, r.URL)
		return nil, fmt.Errorf("forge request refused by the tripwire: %s %s", r.Method, r.URL)
	}
	if fail != nil {
		return nil, fail
	}
	return http.DefaultTransport.RoundTrip(r)
}

// failWith makes every request fail at the transport with err (nil heals):
// the forge unreachable, or a request that timed out.
func (f *forgeTripwire) failWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = err
}

func (f *forgeTripwire) arm(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = on
}

func (f *forgeTripwire) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

// since returns the requests seen after the first n.
func (f *forgeTripwire) since(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seen[n:])
}

// podmanTripwire is the podmanx.CmdRunner of the instance service and the
// AFK engine: it records every invocation and answers success, so a container
// spawn works in the test and every podman call is visible afterwards.
type podmanTripwire struct {
	mu    sync.Mutex
	calls []string
}

func (p *podmanTripwire) run(_ context.Context, name string, args ...string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, name+" "+strings.Join(args, " "))
	return nil, nil
}

func (p *podmanTripwire) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// since returns the invocations recorded after the first n.
func (p *podmanTripwire) since(n int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls[n:])
}

// flakyReadinessStore is the readiness evaluator's Store with reads that fail
// on demand — a dependency failure injected through the evaluator's own seam
// (installed before the listener starts, so toggling it is race-free).
type flakyReadinessStore struct {
	readiness.Store
	failCredentials, failImports atomic.Bool
}

var errFlakyRead = errors.New("database is locked")

func (f *flakyReadinessStore) Credentials(ctx context.Context) ([]store.CredentialMeta, error) {
	if f.failCredentials.Load() {
		return nil, errFlakyRead
	}
	return f.Store.Credentials(ctx)
}

func (f *flakyReadinessStore) AllRepoImports(ctx context.Context) (map[string][]string, error) {
	if f.failImports.Load() {
		return nil, errFlakyRead
	}
	return f.Store.AllRepoImports(ctx)
}

// readinessServer is the production readiness wiring in miniature.
type readinessServer struct {
	*testServer
	flaky    *flakyReadinessStore // the evaluator's store, failing on demand
	rec      *readiness.Recorder
	vlt      *vault.Vault
	git      *gitx.Engine
	gitLog   string // every git subprocess the engine ran, one argv per line
	stub     *forgeStub
	forge    *forgeTripwire
	podman   *podmanTripwire
	prov     *providertest.Fake
	runner   *tmuxx.Fake
	home     string
	env      []string
	reposDir string
	events   <-chan events.Event
}

// loggingGit writes a wrapper script that appends its argv to logPath and
// then runs the real git — the engine's binary in this suite, so EVERY git
// subprocess lab starts is on record.
func loggingGit(t *testing.T, logPath string) string {
	t.Helper()
	testutil.RequireTool(t, "sh")
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found: %v", err)
	}
	path := filepath.Join(t.TempDir(), "git-logged")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newReadinessServer(t *testing.T) *readinessServer {
	t.Helper()
	testutil.RequireTool(t, "git")
	home := t.TempDir()
	env := testutil.HermeticGitEnv(home)
	stateDir := t.TempDir()
	reposDir := filepath.Join(stateDir, "repos")
	worktreeRoot := filepath.Join(stateDir, "worktrees")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}

	rs := &readinessServer{
		home: home, env: env, reposDir: reposDir,
		gitLog: filepath.Join(t.TempDir(), "git.log"),
		stub:   &forgeStub{status: http.StatusOK},
		forge:  &forgeTripwire{t: t},
		podman: &podmanTripwire{},
		prov:   providertest.New(),
		runner: tmuxx.NewFake(),
	}
	rs.prov.SetDisplayName("Agent One")
	rs.git = gitx.New(loggingGit(t, rs.gitLog))
	// A host-Runner spawn write-protects its read-only import snapshots
	// (ADR-0063); production removes them through instancehome.Wipe, which
	// restores the bits first. Runs left live by a test need the same before
	// t.TempDir's plain RemoveAll.
	t.Cleanup(func() {
		_ = filepath.WalkDir(filepath.Join(stateDir, "instances"), func(path string, d os.DirEntry, err error) error {
			if err == nil && d.Type()&os.ModeSymlink == 0 {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	forgeTS := httptest.NewServer(rs.stub)
	t.Cleanup(forgeTS.Close)

	var svc *reposvc.Service
	rs.testServer = newTestServerHooked(t, func(o *Options) {
		st := o.Store
		if err := st.SeedDefaultSettings(context.Background(), 6, "claude-code"); err != nil {
			t.Fatal(err)
		}
		vlt, err := vault.New(make([]byte, vault.KeySize))
		if err != nil {
			t.Fatal(err)
		}
		rs.vlt = vlt
		mat, err := vault.NewMaterializer(filepath.Join(stateDir, "runtime"))
		if err != nil {
			t.Fatal(err)
		}
		reg, err := provider.NewRegistry(rs.prov)
		if err != nil {
			t.Fatal(err)
		}

		// The recorder, its fanout and its four feeds — cmd/lab's wiring
		// (readinessFanout there), line for line.
		rs.rec = readiness.NewRecorder(o.Bus, nil)
		rs.rec.SetFanout(readiness.Fanout{
			Importers: func(ctx context.Context, repoID string) ([]string, error) {
				importers, err := st.RepoImporters(ctx, repoID)
				out := make([]string, 0, len(importers))
				for _, r := range importers {
					out = append(out, r.ID)
				}
				return out, err
			},
			ImageRepos: func(ctx context.Context) ([]string, error) {
				repos, err := st.Repos(ctx)
				var out []string
				for _, r := range repos {
					if runner, err := instance.EffectiveRunner(ctx, st, r); err != nil || runner == store.RunnerContainer {
						out = append(out, r.ID)
					}
				}
				return out, err
			},
		})
		rs.git.SetFetchObserver(rs.rec.ObserveFetch)
		// The REAL registry and the REAL REST clients: credential decrypt,
		// flavor routing, RepoPath resolution — only the unreachable https
		// BaseURL is swapped for the stub's, and every request crosses the
		// tripwire transport.
		httpClient := &http.Client{Transport: rs.forge, Timeout: 10 * time.Second}
		trackerReg := tracker.NewRegistry(st, vlt, httpClient, builtin.New,
			func(c tracker.ForgejoConfig) tracker.Tracker {
				return forgejo.New(c.HTTPClient, forgeTS.URL+"/api/v1", c.Token, c.Owner, c.Repo)
			},
			func(c tracker.GitHubConfig) tracker.Tracker {
				return github.New(c.HTTPClient, forgeTS.URL, c.Token, c.Owner, c.Repo)
			})
		trackerReg.SetReadObserver(rs.rec.ObserveTrackerRead)

		gate := &podmanx.Gate{}
		gate.Set(podmanx.Result{Version: "5.0.0"})
		guard := startguard.New()
		homes := instancehome.New(filepath.Join(stateDir, "instances"))
		inst, err := instance.New(instance.Options{
			Store: st, Git: rs.git, Runner: rs.runner, Providers: reg, Vault: vlt, Materializer: mat,
			Homes: homes, Guard: guard, Bus: o.Bus, Logger: logx.New(io.Discard), ReposDir: reposDir,
			WorktreeRoot: worktreeRoot, LabURL: "http://127.0.0.1:8080", GitEnv: env,
			CaptureCtx: context.Background(),
			PodmanBin:  rdyPodmanBin, PodmanRun: rs.podman.run, ContainerPreflight: gate.Result,
			ContainerImage:       rdyDevImage,
			ContainerToolsImages: map[string]string{rs.prov.ID(): rdyToolsImage},
			AgentSockDir:         filepath.Join(stateDir, "agent"),
			ImageEnsured:         rs.rec.ObserveImage,
		})
		if err != nil {
			t.Fatal(err)
		}
		afkSvc, err := afk.New(afk.Options{
			Store: st, Git: rs.git, Runner: rs.runner, Trackers: trackerReg,
			Instances: inst, Homes: homes, Bus: o.Bus, Logger: logx.New(io.Discard),
			Guard: guard, ReposDir: reposDir, WorktreeRoot: worktreeRoot, GitEnv: env,
			PodmanBin: rdyPodmanBin, PodmanRun: rs.podman.run, ContainerPreflight: gate.Result,
			OnClaimable: rs.rec.ObserveClaimable,
		})
		if err != nil {
			t.Fatal(err)
		}
		inst.SetAFKStopper(afkSvc)
		svc, err = reposvc.New(reposvc.Options{
			Store: st, Vault: vlt, Materializer: mat, Git: rs.git, Bus: o.Bus,
			Logger: logx.New(io.Discard), ReposDir: reposDir, GitEnv: env, Providers: reg,
			LiveInstances: inst.LiveInstances, StopInstances: inst.StopAll,
		})
		if err != nil {
			t.Fatal(err)
		}
		o.Vault = vlt
		o.Repos = svc
		o.Instances = inst
		o.Providers = reg
		o.Homes = homes
		o.Tracker = trackerReg
		o.AFK = afkSvc
		o.Readiness = rs.rec
	}, func(s *Server) {
		rs.flaky = &flakyReadinessStore{Store: s.readiness.Store}
		s.readiness.Store = rs.flaky
	})
	t.Cleanup(svc.Close)
	rs.setup("op", "password123")

	ch, cancel := rs.bus.Subscribe(context.Background())
	t.Cleanup(cancel)
	rs.events = ch
	return rs
}

// repoChanged drains the bus and returns how many repo.changed events named
// repoID since the last drain. Every publish this suite counts happens inside
// a request handler, before the response is written — Bus.Publish delivers
// into the subscriber's buffer synchronously — so the count is exact the
// moment the request returns.
func (rs *readinessServer) repoChanged(repoID string) int {
	rs.t.Helper()
	n := 0
	for {
		select {
		case e := <-rs.events:
			if e.Type != "repo.changed" {
				continue
			}
			raw, err := json.Marshal(e.Payload)
			if err != nil {
				rs.t.Fatalf("marshal event payload: %v", err)
			}
			var p struct {
				Type   string `json:"type"`
				RepoID string `json:"repoID"`
			}
			if err := json.Unmarshal(raw, &p); err != nil || p.Type != "repo.changed" {
				rs.t.Fatalf("repo.changed payload = %s (%v), want {type, repoID}", raw, err)
			}
			if p.RepoID == repoID {
				n++
			}
		default:
			return n
		}
	}
}

// liveSessions counts the fake tmux server's live sessions.
func (rs *readinessServer) liveSessions() int {
	rs.t.Helper()
	live, err := rs.runner.List(context.Background())
	if err != nil {
		rs.t.Fatalf("List: %v", err)
	}
	return len(live)
}

// gitCalls returns every git argv the engine ran since the log was last
// cleared.
func (rs *readinessServer) gitCalls() []string {
	rs.t.Helper()
	b, err := os.ReadFile(rs.gitLog)
	if err != nil && !os.IsNotExist(err) {
		rs.t.Fatal(err)
	}
	var calls []string
	for line := range strings.Lines(string(b)) {
		if line = strings.TrimSpace(line); line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}

func (rs *readinessServer) clearGitLog() {
	rs.t.Helper()
	if err := os.WriteFile(rs.gitLog, nil, 0o644); err != nil {
		rs.t.Fatal(err)
	}
}

// credential stores a vault-sealed credential, created an hour ago so a
// later rotation through the API always lands on a different updated_at.
func (rs *readinessServer) credential(name, kind string, payload any) string {
	rs.t.Helper()
	blob, err := rs.vlt.EncryptPayload(payload)
	if err != nil {
		rs.t.Fatalf("EncryptPayload: %v", err)
	}
	id := ids.NewID("cred")
	if _, err := rs.st.CreateCredential(context.Background(), id, name, kind, blob, time.Now().Add(-time.Hour)); err != nil {
		rs.t.Fatalf("CreateCredential: %v", err)
	}
	return id
}

// repo registers a clone-ready repo over a REAL bare reference repo whose
// origin is a local fixture — so its fetches run for real and stay on this
// machine — and returns the row and the origin directory.
func (rs *readinessServer) repo(name string, mod func(*store.Repo)) (store.Repo, string) {
	rs.t.Helper()
	origin := makeRepoOrigin(rs.t, rs.home, "main", 2)
	repoID := ids.NewID("repo")
	// Cloned with a plain engine: fixture setup is not lab's doing, and must
	// not show up as a recorded fetch.
	if err := gitx.New("git").CloneBare(context.Background(), "file://"+origin, rs.bare(repoID), rs.env, nil); err != nil {
		rs.t.Fatalf("CloneBare: %v", err)
	}
	r := store.Repo{
		ID: repoID, Name: name, RemoteURL: "file://" + origin,
		TrackerBinding: store.TrackerBindingBuiltin, ForgeKind: "none", DefaultBranch: "main",
		AFKBranchPattern: "afk/<N>", ManualBranchPrefix: "lab/",
		CloneStatus: store.CloneStatusReady, CreatedAt: time.Now(),
	}
	if mod != nil {
		mod(&r)
	}
	created, err := rs.st.CreateRepo(context.Background(), r)
	if err != nil {
		rs.t.Fatalf("CreateRepo %s: %v", name, err)
	}
	return created, origin
}

func (rs *readinessServer) bare(repoID string) string {
	return filepath.Join(rs.reposDir, repoID+".git")
}

// forgeRepo is repo bound to a forge of the given flavor: a forge credential
// of that flavor, and a remote URL with the owner/repo path the REST client
// addresses (the bare repo's own origin stays the local fixture).
func (rs *readinessServer) forgeRepo(name, flavor string, mod func(*store.Repo)) (store.Repo, string) {
	rs.t.Helper()
	host, kind := "forge.test", "forgejo"
	if flavor == vault.ForgeGitHub {
		host, kind = "api.github.test", "github"
	}
	credID := rs.credential(name+" forge token", store.CredentialKindForgeToken,
		vault.ForgeTokenPayload{Host: host, Token: "forge-secret-token", Forge: flavor})
	return rs.repo(name, func(r *store.Repo) {
		r.RemoteURL = "https://" + host + "/acme/" + name + ".git"
		r.TrackerBinding, r.ForgeKind, r.ForgeCredentialID = store.TrackerBindingForge, kind, &credID
		if mod != nil {
			mod(r)
		}
	})
}

func (rs *readinessServer) getJSON(path string) map[string]any {
	rs.t.Helper()
	resp := rs.do("GET", path, nil, nil)
	wantStatus(rs.t, resp, http.StatusOK)
	return decodeBody(rs.t, resp)
}

func (rs *readinessServer) readiness(repoID string) map[string]any {
	rs.t.Helper()
	return rs.getJSON("/api/v1/repos/" + repoID + "/readiness")
}

func (rs *readinessServer) startInstance(repoID, label string, want int) {
	rs.t.Helper()
	resp := rs.do("POST", "/api/v1/repos/"+repoID+"/instances", map[string]any{"label": label}, csrfHeaders(rs.ts.URL))
	wantStatus(rs.t, resp, want)
	_ = resp.Body.Close()
}

// rdyChecks indexes a readiness report's checks by id and returns their ids
// in report order.
func rdyChecks(t *testing.T, report map[string]any) (map[string]map[string]any, []string) {
	t.Helper()
	raw, ok := report["checks"].([]any)
	if !ok {
		t.Fatalf("readiness report has no checks array: %v", report)
	}
	byID := make(map[string]map[string]any, len(raw))
	order := make([]string, 0, len(raw))
	for _, c := range raw {
		check := c.(map[string]any)
		id, _ := check["id"].(string)
		byID[id] = check
		order = append(order, id)
	}
	return byID, order
}

func wantCheck(t *testing.T, report map[string]any, id, state string) map[string]any {
	t.Helper()
	checks, order := rdyChecks(t, report)
	c, ok := checks[id]
	if !ok {
		t.Fatalf("%s check is missing (have %v), want %s", id, order, state)
	}
	if c["state"] != state {
		t.Fatalf("%s = %v (%v), want %s", id, c["state"], c["detail"], state)
	}
	return c
}

func wantNoCheck(t *testing.T, report map[string]any, id string) {
	t.Helper()
	if checks, _ := rdyChecks(t, report); checks[id] != nil {
		t.Fatalf("%s = %v, want it left out", id, checks[id])
	}
}

func wantFix(t *testing.T, check map[string]any, scope, section, field string) {
	t.Helper()
	fix, _ := check["fix"].(map[string]any)
	want := map[string]any{"scope": scope}
	if section != "" {
		want["section"] = section
	}
	if field != "" {
		want["field"] = field
	}
	if fmt.Sprint(fix) != fmt.Sprint(want) {
		t.Fatalf("%v fix = %v, want %v", check["id"], fix, want)
	}
	if _, has := check["action"]; has {
		t.Fatalf("%v carries both a fix and an action", check["id"])
	}
}

// summaryOf pulls a repo response's summary apart.
func summaryOf(t *testing.T, repo map[string]any) (claimable, openIssues any, report map[string]any) {
	t.Helper()
	s, ok := repo["summary"].(map[string]any)
	if !ok {
		t.Fatalf("repo response has no summary object: %v", repo)
	}
	for _, key := range []string{"claimable", "open_issues", "readiness"} {
		if _, present := s[key]; !present {
			t.Fatalf("summary lacks the %q key: %v", key, s)
		}
	}
	report, ok = s["readiness"].(map[string]any)
	if !ok {
		t.Fatalf("summary.readiness is not an object: %v", s)
	}
	if _, ok := report["state"].(string); !ok {
		t.Fatalf("summary.readiness has no state: %v", report)
	}
	if _, ok := report["checks"].([]any); !ok {
		t.Fatalf("summary.readiness.checks is not an array: %v", report)
	}
	return s["claimable"], s["open_issues"], report
}

// The recorders are fed at the seams where lab's own operations run, and the
// report follows them: a failing tracker list read marks the repo, a failed
// credentialed fetch through the real git engine marks it, each verdict flip
// publishes exactly one repo.changed, and a changed credential makes the
// record stale instead of wrong.
func TestReadiness_RecordersFeedTheReport(t *testing.T) {
	rs := newReadinessServer(t)
	gitCred := rs.credential("deploy token", store.CredentialKindHTTPSToken, vault.HTTPSTokenPayload{Username: "op", Token: "git-secret-token"})
	repo, _ := rs.forgeRepo("widget", "", func(r *store.Repo) { r.CredentialID = &gitCred })
	base := "/api/v1/repos/" + repo.ID
	rs.repoChanged(repo.ID)

	// Nothing has been read or fetched since this lab started: the report
	// holds what stored state alone decides, and guesses nothing else.
	report := rs.readiness(repo.ID)
	if _, order := rdyChecks(t, report); fmt.Sprint(order) != fmt.Sprint([]string{"clone", "imports"}) {
		t.Fatalf("checks before any record = %v, want only clone and imports", order)
	}
	if report["state"] != "passing" {
		t.Fatalf("state = %v, want passing", report["state"])
	}

	// --- tracker: a failing list read marks the repo ---
	rs.stub.set(http.StatusUnauthorized, `{"message":"invalid username, password or token"}`)
	resp := rs.do("GET", base+"/issues", nil, nil)
	wantStatus(t, resp, http.StatusBadGateway)
	_ = resp.Body.Close()
	if n := rs.repoChanged(repo.ID); n != 1 {
		t.Fatalf("the first failed tracker read published %d repo.changed, want exactly 1", n)
	}
	report = rs.readiness(repo.ID)
	trk := wantCheck(t, report, "tracker", "failing")
	wantFix(t, trk, "repo", "integrations", "forge_credential_id")
	detail, _ := trk["detail"].(string)
	if !strings.HasPrefix(detail, "The last read of the forge tracker failed: ") || !strings.Contains(detail, "401") {
		t.Fatalf("tracker detail = %q", detail)
	}
	if strings.Contains(detail, "forge-secret-token") {
		t.Fatalf("tracker detail leaks the forge token: %q", detail)
	}
	if report["state"] != "failing" {
		t.Fatalf("state = %v, want failing", report["state"])
	}

	// The same failure again, and again: no further event.
	for range 3 {
		resp = rs.do("GET", base+"/issues", nil, nil)
		wantStatus(t, resp, http.StatusBadGateway)
		_ = resp.Body.Close()
	}
	if n := rs.repoChanged(repo.ID); n != 0 {
		t.Fatalf("repeated failing reads published %d repo.changed, want none", n)
	}

	// The forge answers again: the verdict flips, once, and the open issue
	// count that read carried is remembered.
	rs.stub.set(http.StatusOK, "", forgeIssue(1), forgeIssue(2, tracker.ReadyLabel), forgeIssue(3))
	rs.getJSON(base + "/issues")
	if n := rs.repoChanged(repo.ID); n != 1 {
		t.Fatalf("failed → succeeded published %d repo.changed, want exactly 1", n)
	}
	wantCheck(t, rs.readiness(repo.ID), "tracker", "passing")
	_, openIssues, _ := summaryOf(t, rs.getJSON(base))
	if openIssues != float64(3) {
		t.Fatalf("summary.open_issues = %v, want the 3 the last read returned", openIssues)
	}
	rs.getJSON(base + "/issues")
	if n := rs.repoChanged(repo.ID); n != 0 {
		t.Fatalf("an unchanged read published %d repo.changed, want none", n)
	}

	// The AFK strip reads the ready queue: the claimable count is remembered
	// (and announced, being new).
	ready := rs.getJSON(base + "/ready?claimable=1")
	if ready["claimable_count"] != float64(1) {
		t.Fatalf("claimable_count = %v, want 1", ready["claimable_count"])
	}
	if n := rs.repoChanged(repo.ID); n != 1 {
		t.Fatalf("the first claimable count published %d repo.changed, want exactly 1", n)
	}
	claimable, _, _ := summaryOf(t, rs.getJSON(base))
	if claimable != float64(1) {
		t.Fatalf("summary.claimable = %v, want the 1 just computed", claimable)
	}

	// --- git credential: a spawn's credentialed fetch is the evidence ---
	rs.startInstance(repo.ID, "one", http.StatusCreated)
	if n := rs.repoChanged(repo.ID); n != 1 {
		t.Fatalf("the first recorded fetch published %d repo.changed, want exactly 1", n)
	}
	report = rs.readiness(repo.ID)
	gc := wantCheck(t, report, "git_credential", "passing")
	if gc["detail"] != `The last fetch from the remote succeeded with the git credential "deploy token".` {
		t.Fatalf("git_credential detail = %q", gc["detail"])
	}
	// The spawn force-checked the login, so the report now knows it.
	al := wantCheck(t, report, "agent_login", "passing")
	if al["detail"] != "Agent One is logged in." {
		t.Fatalf("agent_login detail = %q", al["detail"])
	}
	if report["state"] != "passing" {
		t.Fatalf("state = %v, want passing", report["state"])
	}

	// The remote stops answering: the next spawn's fetch fails through the
	// real engine on the real bare repo — and the repo is marked.
	repoGitCmd(t, rs.home, rs.bare(repo.ID), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	rs.startInstance(repo.ID, "two", http.StatusInternalServerError)
	if n := rs.repoChanged(repo.ID); n != 1 {
		t.Fatalf("succeeded → failed fetch published %d repo.changed, want exactly 1", n)
	}
	report = rs.readiness(repo.ID)
	gc = wantCheck(t, report, "git_credential", "failing")
	wantFix(t, gc, "repo", "integrations", "credential_id")
	detail, _ = gc["detail"].(string)
	if !strings.HasPrefix(detail, "The last fetch from the remote failed: ") ||
		!strings.Contains(detail, "does not appear to be a git repository") || strings.ContainsAny(detail, "\n\r") {
		t.Fatalf("git_credential detail = %q", detail)
	}
	if strings.Contains(detail, "git-secret-token") {
		t.Fatalf("git_credential detail leaks the credential: %q", detail)
	}
	// The list carries the same verdict — what its "Needs you" block reads.
	var listed map[string]any
	for _, r := range rs.getJSON("/api/v1/repos")["repos"].([]any) {
		if r.(map[string]any)["id"] == repo.ID {
			listed = r.(map[string]any)
		}
	}
	_, _, listReport := summaryOf(t, listed)
	if listReport["state"] != "failing" {
		t.Fatalf("list summary state = %v, want failing", listReport["state"])
	}
	wantCheck(t, listReport, "git_credential", "failing")

	// A third failing spawn is the same verdict: silence.
	rs.startInstance(repo.ID, "three", http.StatusInternalServerError)
	if n := rs.repoChanged(repo.ID); n != 0 {
		t.Fatalf("a repeated failing fetch published %d repo.changed, want none", n)
	}

	// The operator rotates the credential. The recorded failure was the old
	// version's: the check disappears instead of nagging about it.
	resp = rs.do("PATCH", "/api/v1/credentials/"+gitCred,
		map[string]any{"payload": map[string]any{"username": "op", "token": "rotated-secret"}}, csrfHeaders(rs.ts.URL))
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
	report = rs.readiness(repo.ID)
	wantNoCheck(t, report, "git_credential")
	if report["state"] != "passing" {
		t.Fatalf("state after the rotation = %v, want passing", report["state"])
	}
	// So does a rotated forge credential's tracker verdict.
	resp = rs.do("PATCH", "/api/v1/credentials/"+*repo.ForgeCredentialID,
		map[string]any{"payload": map[string]any{"host": "forge.test", "token": "rotated-forge"}}, csrfHeaders(rs.ts.URL))
	wantStatus(t, resp, http.StatusOK)
	_ = resp.Body.Close()
	wantNoCheck(t, rs.readiness(repo.ID), "tracker")

	// Deleting the repo drops what was remembered about it.
	resp = rs.do("DELETE", base+"?force=true", nil, csrfHeaders(rs.ts.URL))
	wantStatus(t, resp, http.StatusNoContent)
	_ = resp.Body.Close()
	if _, ok := rs.rec.Fetch(repo.ID); ok {
		t.Error("the deleted repo's fetch record survived")
	}
	if recs := rs.rec.TrackerReads(repo.ID); recs != nil {
		t.Error("the deleted repo's tracker records survived")
	}
	if _, ok := rs.rec.OpenIssues(repo.ID); ok {
		t.Error("the deleted repo's open issue count survived")
	}
}

// THE acceptance criterion (issue #61): "Loading the Repositories list or a
// repo home causes no request to a forge and no git network operation." —
// and, by the same rule, no provider CLI process and no podman process.
//
// The fleet below covers every branch of the evaluation: forge-bound repos of
// both flavors with real recorded outcomes of both verdicts, a container-
// Runner repo with a recorded image, a repo importing others, an Auto-on
// repo, and builtin-bound ones with and without a ready queue. Then the
// tripwires are armed and the three page-view endpoints are loaded, for every
// repo, repeatedly. Any forge request fails the test at the transport; the
// git log, the podman log and the provider's check counter are compared
// before and after.
func TestReadiness_PageViewsCauseNoNetworkAndNoProcess(t *testing.T) {
	rs := newReadinessServer(t)
	h := csrfHeaders(rs.ts.URL)
	gitCred := rs.credential("deploy token", store.CredentialKindHTTPSToken, vault.HTTPSTokenPayload{Username: "op", Token: "git-secret-token"})

	forgejoRepo, _ := rs.forgeRepo("fj-app", "", func(r *store.Repo) { r.CredentialID = &gitCred })
	githubRepo, _ := rs.forgeRepo("gh-app", vault.ForgeGitHub, nil)
	broken, _ := rs.forgeRepo("fj-broken", "", nil)
	containerRepo, _ := rs.forgeRepo("fj-container", "", func(r *store.Repo) { r.Runner = new(store.RunnerContainer) })
	autoRepo, _ := rs.forgeRepo("fj-auto", "", func(r *store.Repo) { r.AFKAutoEnabled = true })
	untouched, _ := rs.forgeRepo("fj-untouched", "", nil)
	lib, _ := rs.repo("lib", nil)
	builtinBusy, _ := rs.repo("builtin-busy", nil)
	cloning, _ := rs.repo("cloning", func(r *store.Repo) { r.CloneStatus = store.CloneStatusCloning })
	failed, _ := rs.repo("clone-failed", func(r *store.Repo) {
		r.CloneStatus = store.CloneStatusError
		r.CloneError = new("git clone --bare: exit status 128: fatal: repository 'https://x/y.git/' not found")
	})
	for _, pair := range [][2]string{{forgejoRepo.ID, lib.ID}, {builtinBusy.ID, forgejoRepo.ID}, {githubRepo.ID, cloning.ID}} {
		if err := rs.st.AddRepoImport(context.Background(), pair[0], pair[1]); err != nil {
			t.Fatalf("AddRepoImport: %v", err)
		}
	}
	// builtin-busy has a ready queue, so its claimable count needs the claim
	// branches: one issue ready, one ready and already claimed.
	labels, err := rs.st.LabelsByRepo(context.Background(), builtinBusy.ID)
	if err != nil {
		t.Fatal(err)
	}
	var readyLabel string
	for _, l := range labels {
		if l.Name == tracker.ReadyLabel {
			readyLabel = l.ID
		}
	}
	for range 2 {
		if _, err := rs.st.CreateIssueWithLabels(context.Background(), builtinBusy.ID, "ready", "", []string{readyLabel}, store.CommentAuthorOperator, nil, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	repoGitCmd(t, rs.home, rs.bare(builtinBusy.ID), "branch", "afk/1", "main")

	// --- Let lab do its own work, so real outcomes are on record. ---
	// Tracker reads: the issue list and the ready queue of the forge repos.
	rs.stub.set(http.StatusOK, "", forgeIssue(1, tracker.ReadyLabel), forgeIssue(2))
	for _, r := range []store.Repo{forgejoRepo, githubRepo, containerRepo, autoRepo} {
		rs.getJSON("/api/v1/repos/" + r.ID + "/issues")
		rs.getJSON("/api/v1/repos/" + r.ID + "/ready?claimable=1")
	}
	rs.stub.set(http.StatusNotFound, `{"message":"not found"}`)
	resp := rs.do("GET", "/api/v1/repos/"+broken.ID+"/ready", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()
	rs.stub.set(http.StatusOK, "")
	// Spawns: credentialed fetches of the repos and of the import target, a
	// forced login check, and the container repo's pull-if-missing.
	rs.startInstance(forgejoRepo.ID, "a", http.StatusCreated) // also fetches its import, lib
	rs.startInstance(githubRepo.ID, "a", http.StatusInternalServerError)
	rs.startInstance(containerRepo.ID, "a", http.StatusCreated)
	rs.startInstance(builtinBusy.ID, "a", http.StatusCreated)
	repoGitCmd(t, rs.home, rs.bare(autoRepo.ID), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
	rs.startInstance(autoRepo.ID, "a", http.StatusInternalServerError)

	// Preconditions: the tripwires see real traffic when lab does real work —
	// otherwise a silent tripwire would prove nothing.
	if rs.forge.count() == 0 || rs.stub.received() == 0 {
		t.Fatal("precondition: the setup made no forge request through the tripwire transport")
	}
	if !slices.ContainsFunc(rs.gitCalls(), func(c string) bool { return strings.HasPrefix(c, "fetch ") }) {
		t.Fatalf("precondition: the setup ran no git fetch through the logging wrapper: %v", rs.gitCalls())
	}
	if rs.podman.count() == 0 {
		t.Fatal("precondition: the container spawn ran no podman command through the recording seam")
	}
	if rs.prov.AuthChecks() == 0 {
		t.Fatal("precondition: no spawn checked the provider login")
	}

	// --- Arm everything, then view the pages. ---
	rs.forge.arm(true)
	defer rs.forge.arm(false)
	rs.clearGitLog()
	forgeBefore, stubBefore := rs.forge.count(), rs.stub.received()
	podmanBefore, authBefore := rs.podman.count(), rs.prov.AuthChecks()
	sessionsBefore := rs.liveSessions()

	all := []store.Repo{forgejoRepo, githubRepo, broken, containerRepo, autoRepo, untouched, lib, builtinBusy, cloning, failed}
	var list map[string]any
	reports := map[string]map[string]any{}
	summaries := map[string]map[string]any{}
	for range 3 { // a page is reloaded; the answer must stay free every time
		list = rs.getJSON("/api/v1/repos")
		for _, r := range all {
			summaries[r.ID] = rs.getJSON("/api/v1/repos/" + r.ID)
			reports[r.ID] = rs.readiness(r.ID)
		}
	}

	// No request to a forge.
	if n := rs.forge.count() - forgeBefore; n != 0 {
		t.Errorf("the page views made %d forge request(s): %v", n, rs.forge.since(forgeBefore))
	}
	if n := rs.stub.received() - stubBefore; n != 0 {
		t.Errorf("the forge stub received %d request(s) during the page views", n)
	}
	// No git network operation — and for the forge-bound repos no git at all.
	// The one thing the page views may run is the builtin repo's local claim
	// listing.
	for _, call := range rs.gitCalls() {
		if !strings.HasPrefix(call, "for-each-ref ") {
			t.Errorf("the page views ran git %q; only a local for-each-ref is allowed", call)
		}
	}
	if n := len(rs.gitCalls()); n != 3*2 {
		// One claim listing per list and per single view of builtin-busy —
		// the only repo with a ready queue — and none for anything else.
		t.Errorf("the page views ran %d git commands, want %d (builtin-busy's claim listing only): %v", n, 3*2, rs.gitCalls())
	}
	// No podman process, no provider status check, no session.
	if n := rs.podman.count() - podmanBefore; n != 0 {
		t.Errorf("the page views ran %d podman command(s): %v", n, rs.podman.since(podmanBefore))
	}
	if n := rs.prov.AuthChecks() - authBefore; n != 0 {
		t.Errorf("the page views ran %d provider login check(s)", n)
	}
	if n := rs.liveSessions(); n != sessionsBefore {
		t.Errorf("the page views changed the session count: %d → %d", sessionsBefore, n)
	}

	// --- And the answers are the real ones, not an empty shell. ---
	wantCheck(t, reports[forgejoRepo.ID], "git_credential", "passing")
	wantCheck(t, reports[forgejoRepo.ID], "tracker", "passing")
	wantCheck(t, reports[forgejoRepo.ID], "agent_login", "passing")
	imp := wantCheck(t, reports[forgejoRepo.ID], "imports", "passing")
	if imp["detail"] != `The read-only import "lib" was fetched successfully.` {
		t.Errorf("fj-app imports detail = %q", imp["detail"])
	}
	wantNoCheck(t, reports[forgejoRepo.ID], "dev_image") // host Runner
	if reports[forgejoRepo.ID]["state"] != "passing" {
		t.Errorf("fj-app state = %v", reports[forgejoRepo.ID]["state"])
	}

	wantCheck(t, reports[githubRepo.ID], "tracker", "passing")
	// An import still cloning settles by itself: pending, nothing to fix.
	ghImports := wantCheck(t, reports[githubRepo.ID], "imports", "pending")
	if _, has := ghImports["fix"]; has {
		t.Errorf("gh-app imports check carries a fix while its target is still cloning: %v", ghImports)
	}
	if ghImports["detail"] != `The read-only import "cloning" is still being cloned.` {
		t.Errorf("gh-app imports detail = %q", ghImports["detail"])
	}

	notFound := wantCheck(t, reports[broken.ID], "tracker", "failing")
	wantFix(t, notFound, "repo", "integrations", "forge_credential_id")
	if notFound["detail"] != "The forge does not know this repository, or the forge credential's token cannot see it." {
		t.Errorf("fj-broken tracker detail = %q", notFound["detail"])
	}

	dev := wantCheck(t, reports[containerRepo.ID], "dev_image", "passing")
	if dev["detail"] != "The dev image is present on this server." {
		t.Errorf("fj-container dev_image detail = %q", dev["detail"])
	}
	if _, order := rdyChecks(t, reports[containerRepo.ID]); fmt.Sprint(order) !=
		fmt.Sprint([]string{"clone", "git_credential", "tracker", "agent_login", "dev_image", "imports"}) {
		t.Errorf("fj-container checks = %v, want all six in canonical order", order)
	}

	autoGit := wantCheck(t, reports[autoRepo.ID], "git_credential", "failing")
	wantFix(t, autoGit, "repo", "integrations", "credential_id")

	// Never read, never fetched: only what stored state decides.
	if _, order := rdyChecks(t, reports[untouched.ID]); fmt.Sprint(order) != fmt.Sprint([]string{"clone", "agent_login", "imports"}) {
		t.Errorf("fj-untouched checks = %v, want clone, agent_login, imports", order)
	}

	pendingClone := wantCheck(t, reports[cloning.ID], "clone", "pending")
	if _, has := pendingClone["action"]; has || reports[cloning.ID]["state"] != "pending" {
		t.Errorf("cloning repo report = %v", reports[cloning.ID])
	}
	failedClone := wantCheck(t, reports[failed.ID], "clone", "failing")
	if failedClone["action"] != "retry_clone" || failedClone["fix"] != nil ||
		failedClone["detail"] != "The clone failed: repository 'https://x/y.git/' not found." {
		t.Errorf("failed clone check = %v", failedClone)
	}

	// Counts: last known for the forge, fresh for the built-in tracker, null
	// where nothing is known.
	wantCounts := func(name string, id string, claimable, openIssues any) {
		t.Helper()
		gotClaimable, gotOpen, _ := summaryOf(t, summaries[id])
		if gotClaimable != claimable || gotOpen != openIssues {
			t.Errorf("%s summary counts = claimable %v, open_issues %v; want %v, %v", name, gotClaimable, gotOpen, claimable, openIssues)
		}
	}
	wantCounts("fj-app", forgejoRepo.ID, float64(1), float64(2))
	wantCounts("gh-app", githubRepo.ID, float64(1), float64(2))
	wantCounts("fj-untouched", untouched.ID, nil, nil)
	wantCounts("fj-broken", broken.ID, nil, nil)
	wantCounts("builtin-busy", builtinBusy.ID, float64(1), float64(2)) // two ready, #1 claimed
	wantCounts("lib", lib.ID, float64(0), float64(0))
	wantCounts("cloning", cloning.ID, float64(0), float64(0))

	// The list carries the very same summary as each single view, and the
	// summary's report is the readiness endpoint's.
	listed := map[string]map[string]any{}
	for _, r := range list["repos"].([]any) {
		listed[r.(map[string]any)["id"].(string)] = r.(map[string]any)
	}
	if len(listed) != len(all) {
		t.Fatalf("the list holds %d repos, want %d", len(listed), len(all))
	}
	for _, r := range all {
		single, _ := json.Marshal(summaries[r.ID]["summary"])
		inList, _ := json.Marshal(listed[r.ID]["summary"])
		if string(single) != string(inList) {
			t.Errorf("%s: list summary differs from the single view\n list:   %s\n single: %s", r.Name, inList, single)
		}
		report, _ := json.Marshal(reports[r.ID])
		embedded, _ := json.Marshal(summaries[r.ID]["summary"].(map[string]any)["readiness"])
		if string(report) != string(embedded) {
			t.Errorf("%s: summary.readiness differs from GET /readiness\n summary:  %s\n endpoint: %s", r.Name, embedded, report)
		}
	}

	// The mutations that answer with a repo carry the summary too, still
	// without a forge request: the settings PATCH, the Auto toggle (off — on
	// would rightly kick the engine's own spawn pass), the three-strikes
	// Reset.
	for _, tc := range []struct {
		method, path string
		body         map[string]any
	}{
		{"PATCH", "/api/v1/repos/" + untouched.ID, map[string]any{"max_fix_attempts": 3}},
		{"PUT", "/api/v1/repos/" + autoRepo.ID + "/afk/auto", map[string]any{"enabled": false}},
		{"POST", "/api/v1/repos/" + forgejoRepo.ID + "/afk/reset", map[string]any{}},
	} {
		resp := rs.do(tc.method, tc.path, tc.body, h)
		wantStatus(t, resp, http.StatusOK)
		summaryOf(t, decodeBody(t, resp))
	}
	if n := rs.forge.count() - forgeBefore; n != 0 {
		t.Errorf("the repo mutations made %d forge request(s)", n)
	}
}

// A repo is registered through the API and cloned for real, asynchronously: the
// 201 already carries a summary (pending — the clone is in flight), and the
// completed clone is the first evidence that the remote answers.
func TestReadiness_CloneLifecycle(t *testing.T) {
	rs := newReadinessServer(t)
	h := csrfHeaders(rs.ts.URL)
	origin := makeRepoOrigin(t, rs.home, "main", 2)

	resp := rs.do("POST", "/api/v1/repos", map[string]any{"remote_url": "file://" + origin, "name": "cloned"}, h)
	wantStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	claimable, openIssues, report := summaryOf(t, created)
	if created["clone_status"] == store.CloneStatusCloning {
		// (The clone of a tiny local fixture may already be done.)
		clone := wantCheck(t, report, "clone", "pending")
		if clone["detail"] != "Lab is cloning this repository — runs can start when the clone finishes." || report["state"] != "pending" {
			t.Fatalf("cloning report = %v", report)
		}
		wantNoCheck(t, report, "git_credential")
	}
	if claimable != float64(0) || openIssues != float64(0) {
		t.Fatalf("new builtin repo counts = %v / %v, want 0 / 0", claimable, openIssues)
	}
	id := created["id"].(string)

	deadline := time.Now().Add(repoWaitTimeout)
	for rs.getJSON("/api/v1/repos/" + id)["clone_status"] != store.CloneStatusReady {
		if time.Now().After(deadline) {
			t.Fatal("the clone did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	report = rs.readiness(id)
	wantCheck(t, report, "clone", "passing")
	gc := wantCheck(t, report, "git_credential", "passing")
	if gc["detail"] != "The last fetch from the remote succeeded without a git credential." {
		t.Fatalf("git_credential after the clone = %q", gc["detail"])
	}

	// A clone that cannot succeed: failing, with the retry as its one action.
	resp = rs.do("POST", "/api/v1/repos", map[string]any{"remote_url": "file://" + filepath.Join(rs.home, "no-such-origin"), "name": "doomed"}, h)
	wantStatus(t, resp, http.StatusCreated)
	doomed := decodeBody(t, resp)["id"].(string)
	deadline = time.Now().Add(repoWaitTimeout)
	for rs.getJSON("/api/v1/repos/" + doomed)["clone_status"] != store.CloneStatusError {
		if time.Now().After(deadline) {
			t.Fatal("the doomed clone did not fail")
		}
		time.Sleep(20 * time.Millisecond)
	}
	report = rs.readiness(doomed)
	clone := wantCheck(t, report, "clone", "failing")
	if clone["action"] != "retry_clone" || clone["fix"] != nil {
		t.Fatalf("failed clone check = %v", clone)
	}
	if d, _ := clone["detail"].(string); !strings.HasPrefix(d, "The clone failed: ") || strings.ContainsAny(d, "\n\r") {
		t.Fatalf("failed clone detail = %q", d)
	}
	wantNoCheck(t, report, "git_credential")
	if report["state"] != "failing" {
		t.Fatalf("state = %v, want failing", report["state"])
	}
	if _, ok := rs.rec.Fetch(doomed); ok {
		t.Error("a failed clone was recorded as a fetch outcome")
	}
}

// Every server answers a repo with a summary — also one built without the
// instance stack, the tracker registry or a wired recorder. And the readiness
// endpoint is an authenticated repo route like the others.
func TestReadiness_MinimalServerAndRouteGuards(t *testing.T) {
	x := newRepoTestServerWith(t, func(o *Options) {
		// Seeded as every real start seeds them: without a runner_default
		// row an inheriting repo's Runner cannot be resolved, and that is a
		// failing check of its own (covered in internal/readiness).
		if err := o.Store.SeedDefaultSettings(context.Background(), 6, "claude-code"); err != nil {
			t.Fatal(err)
		}
	})
	h := csrfHeaders(x.ts.URL)
	origin := makeRepoOrigin(t, x.home, "main", 1)

	resp := x.do("POST", "/api/v1/repos", map[string]any{"remote_url": "file://" + origin, "name": "plain"}, h)
	wantStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	summaryOf(t, created)
	id := created["id"].(string)
	repo := x.waitCloneStatus(t, id, store.CloneStatusReady)

	claimable, openIssues, report := summaryOf(t, repo)
	// No AFK engine: the claimable count is unknown. The open count is a
	// store read and known.
	if claimable != nil || openIssues != float64(0) {
		t.Fatalf("counts = %v / %v, want null / 0", claimable, openIssues)
	}
	if _, order := rdyChecks(t, report); fmt.Sprint(order) != fmt.Sprint([]string{"clone", "tracker", "imports"}) {
		t.Fatalf("checks = %v, want clone, tracker, imports", order)
	}
	if report["state"] != "passing" {
		t.Fatalf("state = %v", report["state"])
	}
	for _, r := range func() []any {
		resp := x.do("GET", "/api/v1/repos", nil, nil)
		wantStatus(t, resp, http.StatusOK)
		return decodeBody(t, resp)["repos"].([]any)
	}() {
		summaryOf(t, r.(map[string]any))
	}
	resp = x.do("PATCH", "/api/v1/repos/"+id, map[string]any{"name": "renamed"}, h)
	wantStatus(t, resp, http.StatusOK)
	summaryOf(t, decodeBody(t, resp))

	resp = x.do("GET", "/api/v1/repos/"+id+"/readiness", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	if got := decodeBody(t, resp); got["state"] != "passing" {
		t.Fatalf("readiness = %v", got)
	}
	resp = x.do("GET", "/api/v1/repos/repo_missing/readiness", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	_ = resp.Body.Close()

	// Unauthenticated: 401, like every repo route.
	anon, err := http.Get(x.ts.URL + "/api/v1/repos/" + id + "/readiness")
	if err != nil {
		t.Fatal(err)
	}
	_ = anon.Body.Close()
	if anon.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated readiness = %d, want 401", anon.StatusCode)
	}
}

// A summary is derived; a write is not. When the readiness evaluation cannot
// run (a store read fails), every handler that answers with a repo still
// answers — the settings PATCH, the Auto toggle, the three-strikes Reset and
// a create all committed their write already — with the summary its row
// alone decides: counts null, the clone check only. The list degrades per
// read: an unreadable import list leaves only the imports checks out. The
// readiness endpoint itself may say the evaluation failed.
func TestReadiness_FailedSummaryDegrades(t *testing.T) {
	rs := newReadinessServer(t)
	h := csrfHeaders(rs.ts.URL)
	app, _ := rs.repo("app", nil)
	lib, _ := rs.repo("lib", nil)
	if err := rs.st.AddRepoImport(context.Background(), app.ID, lib.ID); err != nil {
		t.Fatal(err)
	}
	rs.rec.ObserveFetch(gitx.FetchAttribution{RepoID: lib.ID, Credential: store.NoCredentialStamp}, nil)
	base := "/api/v1/repos/" + app.ID

	wantRowOnly := func(what string, repo map[string]any) {
		t.Helper()
		claimable, openIssues, report := summaryOf(t, repo)
		if claimable != nil || openIssues != nil {
			t.Fatalf("%s: counts = %v / %v, want null while the evaluation fails", what, claimable, openIssues)
		}
		if _, order := rdyChecks(t, report); fmt.Sprint(order) != "[clone]" {
			t.Fatalf("%s: checks = %v, want the clone check alone", what, order)
		}
	}

	// Healthy first: the full report.
	_, _, report := summaryOf(t, rs.getJSON(base))
	wantCheck(t, report, "tracker", "passing")
	wantCheck(t, report, "imports", "passing")

	rs.flaky.failCredentials.Store(true)

	resp := rs.do("PATCH", base, map[string]any{"max_fix_attempts": 3}, h)
	wantStatus(t, resp, http.StatusOK)
	patched := decodeBody(t, resp)
	if patched["max_fix_attempts"] != float64(3) {
		t.Fatalf("PATCH answered max_fix_attempts = %v, want the saved 3", patched["max_fix_attempts"])
	}
	wantRowOnly("PATCH", patched)
	if row, err := rs.st.RepoByID(context.Background(), app.ID); err != nil || row.MaxFixAttempts != 3 {
		t.Fatalf("stored max_fix_attempts = %d (%v), want the committed 3", row.MaxFixAttempts, err)
	}

	resp = rs.do("PUT", base+"/afk/auto", map[string]any{"enabled": false}, h)
	wantStatus(t, resp, http.StatusOK)
	wantRowOnly("Auto toggle", decodeBody(t, resp))
	resp = rs.do("POST", base+"/afk/reset", map[string]any{}, h)
	wantStatus(t, resp, http.StatusOK)
	wantRowOnly("Reset", decodeBody(t, resp))

	origin := makeRepoOrigin(t, rs.home, "main", 1)
	resp = rs.do("POST", "/api/v1/repos", map[string]any{"remote_url": "file://" + origin, "name": "fresh"}, h)
	wantStatus(t, resp, http.StatusCreated)
	created := decodeBody(t, resp)
	wantRowOnly("create", created)
	if _, _, rep := summaryOf(t, created); rep["checks"].([]any)[0].(map[string]any)["state"] == "failing" {
		t.Fatalf("a fresh clone's check reads failing: %v", rep)
	}

	wantRowOnly("GET repo", rs.getJSON(base))
	list := rs.getJSON("/api/v1/repos")["repos"].([]any)
	if len(list) != 3 {
		t.Fatalf("the list holds %d repos while the evaluation fails, want all 3", len(list))
	}
	for _, r := range list {
		wantRowOnly("list "+r.(map[string]any)["name"].(string), r.(map[string]any))
	}
	resp = rs.do("GET", base+"/readiness", nil, nil)
	wantStatus(t, resp, http.StatusInternalServerError)
	_ = resp.Body.Close()

	// Only the import list is unreadable: the list leaves the imports checks
	// out — never failing — and everything else stands.
	rs.flaky.failCredentials.Store(false)
	rs.flaky.failImports.Store(true)
	for _, r := range rs.getJSON("/api/v1/repos")["repos"].([]any) {
		repo := r.(map[string]any)
		claimable, openIssues, report := summaryOf(t, repo)
		if claimable == nil || openIssues == nil {
			t.Fatalf("%v: counts = %v / %v, want known", repo["name"], claimable, openIssues)
		}
		wantNoCheck(t, report, "imports")
		wantCheck(t, report, "tracker", "passing")
	}
}

// Only a DEFINITIVE forge answer is evidence (issue #61), through the real
// instrumented tracker and the real REST client of each flavor: a 5xx, an
// unreachable forge and a request that timed out are not recorded — before
// any record, after a success, after a refusal — and never announced. 401,
// 403 and 404 are, and fail the check.
func TestReadiness_TrackerOnlyDefinitiveAnswersAreEvidence(t *testing.T) {
	for _, tc := range []struct{ name, flavor string }{{"forgejo", ""}, {"github", vault.ForgeGitHub}} {
		t.Run(tc.name, func(t *testing.T) {
			rs := newReadinessServer(t)
			repo, _ := rs.forgeRepo("widget", tc.flavor, nil)
			base := "/api/v1/repos/" + repo.ID
			rs.repoChanged(repo.ID)
			read := func() {
				t.Helper()
				resp := rs.do("GET", base+"/issues", nil, nil)
				_ = resp.Body.Close()
			}
			noise := func(what string) {
				t.Helper()
				for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
					rs.stub.set(status, "upstream is having a moment")
					read()
				}
				rs.forge.failWith(errors.New("dial tcp 10.0.0.1:443: connect: connection refused"))
				read()
				rs.forge.failWith(context.DeadlineExceeded)
				read()
				rs.forge.failWith(nil)
				if n := rs.repoChanged(repo.ID); n != 0 {
					t.Fatalf("%s: forge noise published %d repo.changed, want none", what, n)
				}
			}
			trackerDetail := func(state string) string {
				t.Helper()
				c := wantCheck(t, rs.readiness(repo.ID), "tracker", state)
				if state == "failing" {
					wantFix(t, c, "repo", "integrations", "forge_credential_id")
				}
				d, _ := c["detail"].(string)
				return d
			}

			noise("nothing known")
			wantNoCheck(t, rs.readiness(repo.ID), "tracker")
			if recs := rs.rec.TrackerReads(repo.ID); recs != nil {
				t.Fatalf("forge noise left records: %+v", recs)
			}

			rs.stub.set(http.StatusOK, "", forgeIssue(1), forgeIssue(2))
			read()
			if n := rs.repoChanged(repo.ID); n != 1 {
				t.Fatalf("the first success published %d repo.changed, want 1", n)
			}
			noise("after a success")
			trackerDetail("passing")
			if _, openIssues, _ := summaryOf(t, rs.getJSON(base)); openIssues != float64(2) {
				t.Fatalf("open_issues = %v after the noise, want the last read's 2", openIssues)
			}

			for _, refusal := range []struct {
				status int
				body   string
			}{
				{http.StatusUnauthorized, `{"message":"Bad credentials"}`},
				{http.StatusForbidden, `{"message":"Resource not accessible by personal access token"}`},
			} {
				rs.stub.set(refusal.status, refusal.body)
				read()
				if n := rs.repoChanged(repo.ID); n != 1 {
					t.Fatalf("a %d published %d repo.changed, want 1", refusal.status, n)
				}
				want := fmt.Sprint(refusal.status)
				if d := trackerDetail("failing"); !strings.Contains(d, want) {
					t.Fatalf("tracker detail = %q, want the forge's %s", d, want)
				}
				noise(fmt.Sprintf("after a %d", refusal.status))
				if d := trackerDetail("failing"); !strings.Contains(d, want) {
					t.Fatalf("tracker detail after noise = %q, want the %s still", d, want)
				}
				rs.stub.set(http.StatusOK, "", forgeIssue(1), forgeIssue(2))
				read()
				trackerDetail("passing")
				rs.repoChanged(repo.ID)
			}

			rs.stub.set(http.StatusNotFound, `{"message":"Not Found"}`)
			read()
			if d := trackerDetail("failing"); d != "The forge does not know this repository, or the forge credential's token cannot see it." {
				t.Fatalf("tracker detail after a 404 = %q", d)
			}
		})
	}
}
