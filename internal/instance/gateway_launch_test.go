package instance

// The credential gateway's INTEGRATION coverage (issue #24 / ADR-0067):
// gateway.go's pure core is pinned next door in gateway_test.go, and this file
// drives the wiring the other way round — through real Launch/Start calls on
// the package's existing fixture, so every assertion is about what a spawn
// actually produced: the session env, the trust bundle on disk, the seeded
// context file, the podman argv, and what a refusal left behind.
//
// # The two OneCLI stubs, and which is used where
//
// The REST API is stubbed TWICE on purpose, because the two shapes prove
// different things:
//
//   - onecliREST — an httptest.Server driving a REAL *onecli.Client. Highest
//     fidelity: it exercises wire.go's paths and JSON shapes, EnsureAgent's
//     list → create → token-resolving re-list, the access-token read (the
//     listing carries it; nothing is ever regenerated), and decodeGrants.
//     Used for the success paths (the wired env bundle, the stable-token
//     property, the grant inventory), where "does lab talk to OneCLI
//     correctly" is part of the claim.
//   - gatewayAPIStub — a hand-written GatewayAPI struct stub. Used wherever a
//     test must FORCE a failure at an exact step or count calls precisely; a
//     scripted error is one field there, versus a second stub server teaching
//     an httptest handler to fail on the third request.
//
// The gateway PROXY port is not HTTP at all to this package — onecli.Probe
// Gateway is a bare TCP dial — so it is stubbed as a net.Listener on
// 127.0.0.1:0 for "reachable" and as a listener opened then closed for
// "unreachable". No fixed ports, no sleeps.
//
// The host's system CA roots are substituted through the systemCACandidates
// package var (useSystemCARoots, gateway_test.go) so these tests never read
// /etc and pass on a machine that has no /etc/ssl at all.
//
// # What stays a live check
//
// ADR-0067's register, verbatim: "Tests run against a stubbed HTTP API, never a
// live sidecar." Issue #24's last acceptance line — an HTTPS call from inside
// the container to a granted service returns authenticated — is therefore NOT
// asserted here and is not faked either: it needs a real OneCLI sidecar
// terminating TLS with a real interception CA and a real grant, which is a
// deployment check, not a unit under test. What these tests pin instead is the
// closest real proxy for it, end to end: the trust bundle exists on disk at a
// path the container runner binds HOST-IDENTICALLY, it carries the system roots
// AND the gateway CA, and the run's env points every TLS client lab knows about
// (OpenSSL/curl, Node, python-requests, git) at exactly that path while
// HTTPS_PROXY carries the repo's agent-identity token. Everything between that
// and an authenticated response belongs to OneCLI.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
	"git.cloonar.com/Cloonar/coding-lab/internal/onecli"
	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// testProxyToken is the agent-identity access token the hand-written seam
// stub's agent carries (the listing-borne credential EnsureAgent reads).
// Deliberately distinctive so a leak assertion — the token must appear in no
// error string and in no argv element — cannot pass by accident.
const testProxyToken = "onecli-proxy-token-do-not-leak"

// gatewayEnvNames is proxyBundleEnv's whole surface: every env name a
// gateway-wired run gains. The parity assertions scan for these by NAME rather
// than reading values, because envValue cannot tell "absent" from "present and
// empty" — and "present and empty" is a regression the parity criterion
// forbids just as much (a bare NO_PROXY= reads as an authoritative "exempt
// nothing" to several clients).
var gatewayEnvNames = []string{
	"HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy",
	"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO",
}

// --- the GatewayAPI seam stub ----------------------------------------------

// gatewayAPIStub is a hand-written GatewayAPI (instance.go's two-method
// seam): it records what a spawn asked for and answers with scripted values or
// scripted errors. Used where a test must force one step to fail or count
// calls exactly — the REST stub below is the fidelity one.
type gatewayAPIStub struct {
	mu sync.Mutex

	agent     onecli.Agent // the identity EnsureAgent answers with (Token included)
	ensureErr error
	grants    []onecli.Grant
	grantsErr error

	ensured    []string // the NAME each EnsureAgent call carried, in order
	grantCalls int
}

func (s *gatewayAPIStub) EnsureAgent(_ context.Context, name string) (onecli.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensured = append(s.ensured, name)
	if s.ensureErr != nil {
		return onecli.Agent{}, s.ensureErr
	}
	return s.agent, nil
}

func (s *gatewayAPIStub) ListGrants(_ context.Context, _ string) ([]onecli.Grant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grantCalls++
	if s.grantsErr != nil {
		return nil, s.grantsErr
	}
	return s.grants, nil
}

// counts returns the stub's call tallies: ensures and grant listings.
func (s *gatewayAPIStub) counts() (ensures, grants int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ensured), s.grantCalls
}

// ensuredNames returns the agent names EnsureAgent was called with, in order.
func (s *gatewayAPIStub) ensuredNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ensured...)
}

// newGatewayStub is the ordinary happy stub: one agent identity carrying its
// listing-borne access token, and whatever grants the case wants rendered
// into its context file.
func newGatewayStub(grants ...onecli.Grant) *gatewayAPIStub {
	return &gatewayAPIStub{
		agent:  onecli.Agent{ID: "ag_proj", Name: "ag_proj", Token: testProxyToken},
		grants: grants,
	}
}

// --- the REST stub (a real *onecli.Client on top) ---------------------------

// The identity and credential the REST stub hands out. restToken is the value
// a run's HTTPS_PROXY must end up carrying as userinfo.
const (
	restAgentID = "ag_rest"
	restToken   = "onecli-rest-token-do-not-leak"
)

// onecliREST is an httptest stand-in for OneCLI's REST API, answering exactly
// the requests one spawn makes — list agents (rows carry each agent's
// accessToken, the credential the run authenticates with), create agent (its
// answer carries NO token, forcing the client's token-resolving re-list), and
// list grants — in the wire shapes internal/onecli/wire.go verified against
// the 1.45.0 source. There is deliberately NO token endpoint: the client must
// never regenerate (that would invalidate concurrent runs' tokens), so a POST
// to any token path lands in the default branch and fails the test.
type onecliREST struct {
	*httptest.Server

	mu         sync.Mutex
	agents     []string // agent names the project holds, in creation order
	lists      int
	creates    int
	grantLists int
	grantsBody string
}

// newOneCLIREST starts the stub. grantsBody is the raw body GET
// /v1/agents/{id}/grants answers with ("" ⇒ an agent with no grants, the
// normal state of a freshly created per-repo identity).
func newOneCLIREST(t *testing.T, grantsBody string) *onecliREST {
	t.Helper()
	r := &onecliREST{grantsBody: grantsBody}
	if r.grantsBody == "" {
		r.grantsBody = `{"agentId":"` + restAgentID + `","mode":"grants","secrets":[],"connections":[]}`
	}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/v1/agents":
			r.lists++
			rows := make([]string, 0, len(r.agents))
			for _, name := range r.agents {
				rows = append(rows, fmt.Sprintf(`{"id":%q,"name":%q,"accessToken":%q}`, restAgentID, name, restToken))
			}
			_, _ = fmt.Fprintf(w, `[%s]`, strings.Join(rows, ","))

		case req.Method == http.MethodPost && req.URL.Path == "/v1/agents":
			r.creates++
			body, _ := io.ReadAll(req.Body)
			var create struct {
				Name       string `json:"name"`
				Identifier string `json:"identifier"`
			}
			if err := json.Unmarshal(body, &create); err != nil {
				t.Errorf("decoding POST /v1/agents body %q: %v", body, err)
			}
			if create.Identifier == "" {
				// The real build's Zod validation: identifier is required.
				t.Errorf("POST /v1/agents body %q carries no identifier — the real build 400s this", body)
			}
			r.agents = append(r.agents, create.Name)
			w.WriteHeader(http.StatusCreated)
			// The create answer carries no accessToken (wire.go point 5).
			_, _ = fmt.Fprintf(w, `{"id":%q,"name":%q,"identifier":%q}`, restAgentID, create.Name, create.Identifier)

		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/grants"):
			r.grantLists++
			_, _ = io.WriteString(w, r.grantsBody)

		default:
			t.Errorf("unexpected OneCLI request %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(r.Close)
	return r
}

// counts returns the stub's tallies: agent listings, creates, grant listings.
func (r *onecliREST) counts() (lists, creates, grants int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lists, r.creates, r.grantLists
}

// created returns the agent names the project ended up holding.
func (r *onecliREST) created() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.agents...)
}

// client is a real *onecli.Client bound to the stub.
func (r *onecliREST) client(t *testing.T) *onecli.Client {
	t.Helper()
	c, err := onecli.New(onecli.Options{
		BaseURL:    r.URL,
		APIKey:     "oc_proj_test_key",
		HTTPClient: r.Client(),
	})
	if err != nil {
		t.Fatalf("onecli.New: %v", err)
	}
	return c
}

// --- fixture wiring ---------------------------------------------------------

// liveGatewayURL is a reachable gateway proxy address: a loopback listener on
// an ephemeral port, closed on cleanup. ProbeGateway only completes a TCP
// handshake (it deliberately does not speak CONNECT and does not authenticate),
// so a listening socket with nothing accepting is a faithful stand-in for the
// sidecar's port 10255 — and the kernel-assigned port means no fixed port and
// no sleep anywhere in these tests.
func liveGatewayURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the gateway stub: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return "http://" + ln.Addr().String()
}

// deadGatewayURL is a well-formed gateway address nothing answers at: a
// listener is opened so the kernel hands out a port that is genuinely free,
// then closed. That is the "configured and unreachable" state ADR-0067's
// fail-closed pin is about — distinct from a typo, which gatewayProxyURL and
// ProbeGateway both refuse before any dial.
func deadGatewayURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening for the dead-gateway port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the dead-gateway listener: %v", err)
	}
	return "http://" + addr
}

// enableGateway wires the fixture service's OneCLI seam, mirroring
// enableContainer's shape (internal-package field pokes — the same values
// cmd/lab would pass through instance.Options): the REST seam, a REACHABLE
// gateway proxy address, the interception CA file, and substituted system CA
// roots so nothing reads the host's /etc. Returns the gateway URL.
func (f *fixture) enableGateway(t *testing.T, api GatewayAPI) string {
	t.Helper()
	useSystemCARoots(t, testSystemPEM)
	gatewayURL := liveGatewayURL(t)
	f.svc.onecli = api
	f.svc.oneCLIGatewayURL = gatewayURL
	f.svc.oneCLICAFile = writeFile(t, filepath.Join(t.TempDir(), "gateway-ca.pem"), testGatewayPEM)
	return gatewayURL
}

// proxyURLFor is the HTTPS_PROXY value a run wired to gatewayURL must carry:
// the token folded in as userinfo (gatewayProxyURL's contract, pinned exactly
// in gateway_test.go). Built by string surgery rather than by calling the
// function under test, so this file's expectations are independent of it.
func proxyURLFor(gatewayURL, token string) string {
	return strings.Replace(gatewayURL, "http://", "http://"+token+"@", 1)
}

// directAPIHosts is the RESOLVING provider's declared direct-traffic hosts,
// read off the fixture provider rather than written out here: core must not
// name any provider's API host (ADR-0033's neutrality rule, which is exactly
// why noProxyValue takes them as a parameter), and that applies to core's tests
// too. Fails the test if the fixture provider declares none, since every
// NO_PROXY assertion below would then be vacuous.
func (f *fixture) directAPIHosts(t *testing.T) []string {
	t.Helper()
	hosts := f.prov.SeedMeta().DirectAPIHosts
	if len(hosts) == 0 {
		t.Fatal("the fixture provider declares no DirectAPIHosts — the NO_PROXY assertions would be vacuous")
	}
	return hosts
}

// contextFile reads the run's seeded context file out of worktree wt.
func contextFile(t *testing.T, wt string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(wt, "CLAUDE.local.md"))
	if err != nil {
		t.Fatalf("CLAUDE.local.md missing after Launch: %v", err)
	}
	return string(b)
}

// --- 1. parity: an unconfigured lab spawns exactly as it did before #24 ------

// Issue #24's headline acceptance criterion, asserted rather than smoke-tested:
// with the wiring OFF a spawn is indistinguishable from a lab built before the
// gateway existed. Both OFF shapes are covered — Options.OneCLI nil, and a
// client configured with no --onecli-gateway-url (a legitimate deployment:
// issue #23's health surface is exactly that) — and for each one: no proxy, no
// NO_PROXY and no CA variable anywhere in the session env, no trust bundle in
// the run's runtime dir, and a seeded context file BYTE-IDENTICAL to the
// unwired baseline's. The repo carries a secret so the comparison has teeth in
// both directions: the legacy `labctl secret exec` section must still render,
// and the gateway section must not.
func TestLaunch_GatewayUnconfiguredParity(t *testing.T) {
	// spawn runs one manual Start on a secret-bearing repo and returns the
	// fixture, the run, and the seeded context file.
	spawn := func(t *testing.T, wire func(*fixture)) (*fixture, store.Run, string) {
		t.Helper()
		f := newFixture(t)
		if _, err := f.st.CreateRepoSecret(t.Context(), ids.NewID("sec"), f.repo.ID,
			"API_KEY", "Widget API token", []byte("sealed-blob"), f.clock.Now()); err != nil {
			t.Fatalf("CreateRepoSecret: %v", err)
		}
		wire(f)
		run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		return f, run, contextFile(t, filepath.Join(f.worktreeRoot, "proj-20260608-1530"))
	}

	// assertUnwired pins the on-disk and in-env half of the parity claim.
	assertUnwired := func(t *testing.T, f *fixture, run store.Run) {
		t.Helper()
		sess, live := f.runner.Session(run.SessionName)
		if !live {
			t.Fatal("session not live after an unwired Start")
		}
		for _, kv := range sess.ExtraEnv {
			name, _, _ := strings.Cut(kv, "=")
			if slices.Contains(gatewayEnvNames, name) {
				t.Errorf("unwired spawn env carries the gateway entry %q", kv)
			}
		}
		bundle := filepath.Join(f.homes.RuntimePath(run.ID), trustBundleName)
		if _, err := os.Stat(bundle); !os.IsNotExist(err) {
			t.Errorf("unwired spawn wrote a trust bundle at %s (stat err %v), want none", bundle, err)
		}
	}

	// The baseline IS the first case: a lab that never heard of OneCLI.
	base, baseRun, baseline := spawn(t, func(*fixture) {})
	assertUnwired(t, base, baseRun)
	if !strings.Contains(baseline, "labctl secret exec") {
		t.Fatalf("the unwired baseline lost the legacy Secrets section:\n%s", baseline)
	}
	if strings.Contains(baseline, "credential gateway") {
		t.Fatalf("the unwired baseline rendered the GATEWAY Secrets section:\n%s", baseline)
	}

	// The second OFF shape: a REST client is configured (issue #23's health
	// deployment) but no gateway URL, so gatewayActive stays false. Nothing may
	// differ — not one byte of the context file, and not one call on the seam.
	stub := newGatewayStub()
	f, run, got := spawn(t, func(f *fixture) {
		f.svc.onecli = stub
		f.svc.oneCLICAFile = writeFile(t, filepath.Join(t.TempDir(), "gateway-ca.pem"), testGatewayPEM)
		// oneCLIGatewayURL deliberately left empty.
	})
	assertUnwired(t, f, run)
	if got != baseline {
		t.Errorf("context file with a gateway-less OneCLI client differs from the unwired baseline:\ngot\n%s\nwant\n%s", got, baseline)
	}
	if ensures, grants := stub.counts(); ensures != 0 || grants != 0 {
		t.Errorf("the gateway seam was called (%d ensures, %d grant listings) on a lab with no --onecli-gateway-url; want none", ensures, grants)
	}
}

// --- 2. the wired happy path, host runner -----------------------------------

// The full env bundle a gateway-wired host run spawns with, driven end to end
// through a REAL *onecli.Client against the REST stub (so EnsureAgent's
// list→create→re-list, the listing-borne token read and the grants read are
// all genuinely exercised). Everything a run needs is asserted from the
// outside: the proxy URL carries the agent's access token as userinfo and
// points at the configured gateway, the four CA variables name one bundle
// inside the run's private runtime dir, that bundle exists and carries BOTH
// halves, and NO_PROXY exempts the lab host, the repo's forge host and the
// resolving provider's declared direct API hosts.
//
// Launch is called directly rather than through Start so the spec can carry a
// FORGE remote (the fixture repo's own remote is a file:// path, which
// correctly contributes no NO_PROXY entry — the git operations still run
// against the bare clone's own origin, which the spec never touches).
func TestLaunch_GatewayWiredHostRunner(t *testing.T) {
	f := newFixture(t)
	rest := newOneCLIREST(t, "")
	gatewayURL := f.enableGateway(t, rest.client(t))
	// A legacy secret row, present but irrelevant: a gateway-wired run's context
	// file must carry the gateway section INSTEAD of the legacy one, never both.
	if _, err := f.st.CreateRepoSecret(t.Context(), ids.NewID("sec"), f.repo.ID,
		"API_KEY", "Widget API token", []byte("sealed-blob"), f.clock.Now()); err != nil {
		t.Fatalf("CreateRepoSecret: %v", err)
	}

	repo := f.repo
	repo.RemoteURL = "https://git.example.com/Cloonar/coding-lab.git"
	wt := filepath.Join(f.worktreeRoot, "proj-gw")
	run, err := f.svc.Launch(t.Context(), LaunchSpec{
		Repo: repo, Provider: f.prov, Kind: store.RunKindManual,
		SessionName: "proj~gw", Branch: "lab/gw", WorktreePath: wt,
		Model: "opus[1m]", Effort: "max",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	sess, live := f.runner.Session("proj~gw")
	if !live {
		t.Fatal("session not live after a gateway-wired Launch")
	}

	// HTTPS_PROXY (both spellings) points at the CONFIGURED gateway with the
	// agent's listing-borne access token as userinfo.
	wantProxy := proxyURLFor(gatewayURL, restToken)
	for _, name := range []string{"HTTPS_PROXY", "https_proxy"} {
		if got := envValue(sess.ExtraEnv, name); got != wantProxy {
			t.Errorf("%s = %q, want %q", name, got, wantProxy)
		}
	}
	u, err := url.Parse(envValue(sess.ExtraEnv, "HTTPS_PROXY"))
	if err != nil {
		t.Fatalf("HTTPS_PROXY does not parse: %v", err)
	}
	if u.User.Username() != restToken {
		t.Errorf("HTTPS_PROXY userinfo username = %q, want the agent's access token", u.User.Username())
	}
	if want := strings.TrimPrefix(gatewayURL, "http://"); u.Host != want {
		t.Errorf("HTTPS_PROXY host = %q, want the configured gateway %q", u.Host, want)
	}

	// The four CA variables all name ONE bundle, inside this run's private
	// runtime dir (<state>/instances/<runID>/runtime) — the directory the
	// container runner already binds at its host-identical path.
	bundle := filepath.Join(f.homes.RuntimePath(run.ID), trustBundleName)
	if want := filepath.Join(f.instancesDir, run.ID, "runtime", trustBundleName); bundle != want {
		t.Fatalf("bundle path %q disagrees with the per-run runtime layout %q", bundle, want)
	}
	for _, name := range []string{"SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO"} {
		if got := envValue(sess.ExtraEnv, name); got != bundle {
			t.Errorf("%s = %q, want the run's trust bundle %q", name, got, bundle)
		}
	}
	got, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatalf("trust bundle missing after a wired Launch: %v", err)
	}
	if want := testSystemPEM + testGatewayPEM; string(got) != want {
		t.Errorf("trust bundle =\n%q\nwant the substituted system roots followed by the gateway CA\n%q", got, want)
	}

	// NO_PROXY: the lab host, the repo's forge host, then the resolving
	// provider's declared direct API hosts — the traffic a gateway outage must
	// never be able to take down with it.
	wantNoProxy := strings.Join(append([]string{"127.0.0.1", "git.example.com"}, f.directAPIHosts(t)...), ",")
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		if got := envValue(sess.ExtraEnv, name); got != wantNoProxy {
			t.Errorf("%s = %q, want %q", name, got, wantNoProxy)
		}
	}

	// The REST conversation was exactly a first spawn's worth: list (miss),
	// create — addressed by the repo's STORE ID — then the token-resolving
	// re-list, then grants. No token endpoint exists on the stub, so a
	// regenerate attempt would already have failed the test loudly.
	lists, creates, grants := rest.counts()
	if lists != 2 || creates != 1 || grants != 1 {
		t.Errorf("REST calls = %d lists, %d creates, %d grant listings; want 2/1/1", lists, creates, grants)
	}
	if names := rest.created(); !slices.Equal(names, []string{repo.ID}) {
		t.Errorf("created agents = %q, want exactly the repo's store id [%q]", names, repo.ID)
	}

	// The seeder was handed the gateway ref, so the context file teaches the
	// gateway norm — and ONLY that one, despite the repo's legacy secret row.
	local := contextFile(t, wt)
	if !strings.Contains(local, "credential gateway") {
		t.Errorf("context file carries no gateway Secrets section:\n%s", local)
	}
	if strings.Contains(local, "labctl secret exec") {
		t.Errorf("context file carries BOTH Secrets sections — two contradictory norms in one file:\n%s", local)
	}
}

// --- 3. the agent identity is the repo's store id ---------------------------

// ADR-0067 maps one OneCLI agent identity per lab repo, and the name that
// mapping keys on must survive a repo RENAME: grants live on the agent, so
// keying on repo.Name would silently create a second, grant-less identity the
// first time an operator renamed a repo — surfacing as "my secrets vanished"
// with nothing pointing at the rename. Two launches, the second under a
// different repo NAME, must ensure the same identity: the store id, both times.
func TestLaunch_GatewayAgentIdentityIsRepoStoreID(t *testing.T) {
	f := newFixture(t)
	stub := newGatewayStub()
	f.enableGateway(t, stub)

	launch := func(t *testing.T, repo store.Repo, label string) {
		t.Helper()
		if _, err := f.svc.Launch(t.Context(), LaunchSpec{
			Repo: repo, Provider: f.prov, Kind: store.RunKindManual,
			SessionName:  "proj~" + label,
			Branch:       "lab/" + label,
			WorktreePath: filepath.Join(f.worktreeRoot, "proj-"+label),
			Model:        "opus[1m]", Effort: "max",
		}); err != nil {
			t.Fatalf("Launch(%s): %v", label, err)
		}
	}

	launch(t, f.repo, "before")
	renamed := f.repo
	renamed.Name = "renamed-proj" // the operator renamed the repo between spawns
	launch(t, renamed, "after")

	names := stub.ensuredNames()
	if want := []string{f.repo.ID, f.repo.ID}; !slices.Equal(names, want) {
		t.Errorf("EnsureAgent names = %q, want the repo's STORE ID both times %q", names, want)
	}
	for _, n := range names {
		if n == f.repo.Name || n == renamed.Name {
			t.Errorf("EnsureAgent was called with the repo NAME %q — a rename would strand the repo's grants", n)
		}
	}
}

// --- 4. fail closed: the gateway is unreachable -----------------------------

// ADR-0067's fail-closed pin, landing at the spawn: a configured gateway that
// does not answer refuses the launch. The refusal is a *BadRequestError (the
// 400 mapping operator-fixable spawn refusals share) naming the repo and the
// unreachable address — and it lands BEFORE the claim, which is the whole
// reason prepareGateway sits where it does: an AFK spec refused here leaves its
// issue selectable instead of parked behind a host problem. Nothing may exist
// afterwards: no branch, no worktree, no run row, no session, no per-run tree,
// and no start-guard mark.
func TestLaunch_GatewayUnreachableRefusesBeforeTheClaim(t *testing.T) {
	f := newFixture(t)
	stub := newGatewayStub()
	f.enableGateway(t, stub)
	dead := deadGatewayURL(t)
	f.svc.oneCLIGatewayURL = dead

	issue := 42
	name, branch := "proj~afk-42", "afk/42"
	wt := filepath.Join(f.worktreeRoot, "proj-42")
	_, err := f.svc.Launch(t.Context(), LaunchSpec{
		Repo: f.repo, Provider: f.prov, Kind: store.RunKindAFKManual, IssueNumber: &issue,
		SessionName: name, Branch: branch, WorktreePath: wt,
		Model: "opus[1m]", Effort: "max", SeedPrompt: "resolve issue #42",
	})
	var bad *BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("Launch err = %T (%v), want *BadRequestError (the 400 mapping)", err, err)
	}
	for _, want := range []string{
		"refusing to spawn for repo proj without credential-gateway access",
		"dialing " + strings.TrimPrefix(dead, "http://"),
		"check that the OneCLI gateway is running",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to contain %q", err, want)
		}
	}

	// Nothing was claimed and nothing was created.
	if dirExists(wt) {
		t.Error("the refused spawn created a worktree")
	}
	if f.branchExists(branch) {
		t.Errorf("the refused spawn created %s — the AFK claim was parked behind an unreachable sidecar", branch)
	}
	if _, err := f.st.RunBySession(t.Context(), name); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RunBySession after the refusal: %v, want ErrNotFound (no run row)", err)
	}
	if _, live := f.runner.Session(name); live {
		t.Error("the refused spawn left a session")
	}
	entries, rerr := os.ReadDir(f.instancesDir)
	if rerr != nil && !os.IsNotExist(rerr) {
		t.Fatalf("ReadDir(instances): %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("the refused spawn left per-run trees: %v", entries)
	}
	if snap := f.guard.Snapshot(); len(snap) != 0 {
		t.Errorf("startguard marked despite a pre-guard refusal: %v", snap)
	}
	// The probe fails closed BEFORE any REST call: no identity (and with it no
	// credential) was resolved for a run that will not exist.
	if ensures, grants := stub.counts(); ensures != 0 || grants != 0 {
		t.Errorf("the REST seam was called (%d ensures, %d grant listings) after an unreachable probe; want none", ensures, grants)
	}
}

// --- 5 + 6. fail closed: configuration and REST failures --------------------

// Every other way the gateway precheck refuses, driven through Start so the
// refusal-before-claim assertion is the package's existing one
// (assertNothingClaimed). Each row is a *BadRequestError whose message is
// actionable — it names the repo and the flag or the upstream reason — and NO
// row's error may contain the access token, including the last row, where the
// agent identity (token and all) really was resolved before the refusal
// happened. That last row is the sharp version of the claim: a credential
// existed in the process and still reached no error string.
func TestLaunch_GatewayRefusals(t *testing.T) {
	cases := []struct {
		name    string
		wire    func(t *testing.T, f *fixture, api *gatewayAPIStub)
		wantMsg []string
	}{
		{
			// A run pointed at a TLS-terminating proxy whose CA it cannot verify
			// has broken HTTPS in a way nothing inside the run can diagnose.
			name: "gateway URL set without a CA file",
			wire: func(_ *testing.T, f *fixture, _ *gatewayAPIStub) { f.svc.oneCLICAFile = "" },
			wantMsg: []string{
				"--onecli-gateway-url is set but --onecli-ca-file is not",
				"a run for repo proj could not verify",
				"--onecli-ca-file",
			},
		},
		{
			name: "EnsureAgent fails",
			wire: func(_ *testing.T, _ *fixture, api *gatewayAPIStub) {
				api.ensureErr = errors.New("onecli GET /v1/agents: unexpected status 401: invalid api key")
			},
			wantMsg: []string{
				"resolving the agent identity for repo proj",
				"unexpected status 401: invalid api key",
			},
		},
		{
			// The identity resolved but carries no access token: a run cannot
			// authenticate to the gateway without one, so it does not start —
			// and the message names the wire seam, because an empty token in
			// the listing means the OneCLI build changed shape.
			name: "the agent identity carries no access token",
			wire: func(_ *testing.T, _ *fixture, api *gatewayAPIStub) { api.agent.Token = "" },
			wantMsg: []string{
				"carries no access token in OneCLI's agent listing",
				"internal/onecli/wire.go",
			},
		},
		{
			// The precheck passed and the identity (token included) is in hand;
			// the trust bundle is what refuses. Operator config, so still a 400,
			// and still no credential in the message.
			name: "the gateway CA file is not PEM",
			wire: func(t *testing.T, f *fixture, _ *gatewayAPIStub) {
				f.svc.oneCLICAFile = writeFile(t, filepath.Join(t.TempDir(), "ca.der"),
					"\x30\x82\x01\x0a certainly not PEM\n")
			},
			wantMsg: []string{
				"carries no \"-----BEGIN CERTIFICATE-----\" block",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			api := newGatewayStub()
			f.enableGateway(t, api)
			tc.wire(t, f, api)

			_, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
			if err == nil {
				t.Fatal("Start succeeded, want a gateway refusal")
			}
			var bad *BadRequestError
			if !errors.As(err, &bad) {
				t.Errorf("refusal type = %T (%v), want *BadRequestError (the 400 mapping)", err, err)
			}
			for _, want := range tc.wantMsg {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal = %q, want it to contain %q", err, want)
				}
			}
			// No credential, ever, in an error string — the promise gateway.go's
			// header is written on.
			if strings.Contains(err.Error(), testProxyToken) {
				t.Errorf("refusal %q leaked the agent identity's access token", err)
			}
			assertNothingClaimed(t, f)
		})
	}
}

// --- 7. the token is read, never regenerated ---------------------------------

// The agent's access token is STABLE: it rides in OneCLI's agent listing and
// lab only ever reads it — a regenerate at spawn would invalidate the token
// every already-running run of the same repo still holds, and (unlike the
// in-process mint cache this property once rested on) the read survives a lab
// restart with no state at all. Two successive spawns of the same repo must
// therefore touch no token endpoint whatsoever (the REST stub has none — a
// regenerate POST fails the test in its default branch) and both runs must
// carry the same proxy URL, which is the symptom the property protects.
func TestLaunch_GatewayTokenIsStableAcrossSpawns(t *testing.T) {
	f := newFixture(t)
	rest := newOneCLIREST(t, "")
	gatewayURL := f.enableGateway(t, rest.client(t))

	first, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "a"})
	if err != nil {
		t.Fatalf("Start(a): %v", err)
	}
	second, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID, Label: "b"})
	if err != nil {
		t.Fatalf("Start(b): %v", err)
	}

	// The identity mapping is re-established per spawn (idempotent by
	// construction) but creates exactly one agent: the first spawn lists,
	// creates and re-lists for the token; the second lists once and is done.
	lists, creates, _ := rest.counts()
	if lists != 3 || creates != 1 {
		t.Errorf("agent listings/creates = %d/%d, want 3/1 (list+create+re-list, then one warm list)", lists, creates)
	}

	want := proxyURLFor(gatewayURL, restToken)
	for _, run := range []store.Run{first, second} {
		sess, live := f.runner.Session(run.SessionName)
		if !live {
			t.Fatalf("session %s not live", run.SessionName)
		}
		if got := envValue(sess.ExtraEnv, "HTTPS_PROXY"); got != want {
			t.Errorf("%s HTTPS_PROXY = %q, want the one stable token %q", run.SessionName, got, want)
		}
	}
}

// --- 8. grants are best effort ----------------------------------------------

// The grant listing is the ONE step of the precheck that does not fail closed,
// because it is documentation: a run holding a valid token has exactly the
// access its grants describe whether or not lab could render the inventory. So
// a failed listing still spawns — with the gateway section rendered in its
// no-inventory shape — and a successful one puts the granted names into the
// context file SORTED, so the same repo's runs do not produce a reshuffled diff
// an operator has to read and discard.
func TestLaunch_GatewayGrantsBestEffort(t *testing.T) {
	t.Run("a failed listing still spawns, without the inventory", func(t *testing.T) {
		f := newFixture(t)
		api := newGatewayStub()
		api.grantsErr = errors.New("onecli GET /v1/agents/ag_proj/grants: unexpected status 503: (no message)")
		gatewayURL := f.enableGateway(t, api)

		run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		// Still fully wired — documentation never gates the spawn.
		sess, live := f.runner.Session(run.SessionName)
		if !live {
			t.Fatal("session not live")
		}
		if got, want := envValue(sess.ExtraEnv, "HTTPS_PROXY"), proxyURLFor(gatewayURL, testProxyToken); got != want {
			t.Errorf("HTTPS_PROXY = %q, want %q", got, want)
		}
		local := contextFile(t, filepath.Join(f.worktreeRoot, "proj-20260608-1530"))
		if !strings.Contains(local, "No services are granted to this repository yet") {
			t.Errorf("context file does not render the no-inventory shape:\n%s", local)
		}
		if strings.Contains(local, "Services granted to this repository:") {
			t.Errorf("context file rendered an inventory the listing never produced:\n%s", local)
		}
	})

	t.Run("granted names reach the context file, sorted", func(t *testing.T) {
		f := newFixture(t)
		// The REST stub answers the verified grants shape, unsorted, with one
		// connection row whose label AND provider upstream never filled in —
		// its id must stand in for the name rather than vanishing from the
		// inventory.
		rest := newOneCLIREST(t, `{"agentId":"`+restAgentID+`","mode":"grants",`+
			`"secrets":[{"secretId":"sec_2","name":"stripe","type":"generic","scope":"project"},{"secretId":"sec_1","name":"acme-registry","type":"generic","scope":"project"}],`+
			`"connections":[{"connectionId":"conn_9","provider":"","label":null,"scope":"project"}]}`)
		f.enableGateway(t, rest.client(t))

		if _, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		local := contextFile(t, filepath.Join(f.worktreeRoot, "proj-20260608-1530"))
		want := "Services granted to this repository:\n\n- `acme-registry`\n- `conn_9`\n- `stripe`\n"
		if !strings.Contains(local, want) {
			t.Errorf("context file does not carry the sorted grant inventory\nwant\n%s\ngot\n%s", want, local)
		}
	})
}

// --- 9. the container runner, end to end ------------------------------------

// A gateway-wired CONTAINER spawn, asserted against the same exact-argv
// comparison the other container tests use (podmanx.RunArgv over the expected
// RunSpec), plus the three properties that are specific to #24:
//
//   - the agent identity's token appears in NO argv element — the podman
//     command line is visible in `ps`, in podman's logs, and in every argv a
//     test prints, so the proxy pair travels by NAME through tmux -e and its
//     values exist only in the pane environment;
//   - the rest of the bundle rides `--env K=V` deliberately: NO_PROXY and the
//     four CA paths are public by construction and an operator reading a run's
//     command line should be able to see what it trusts and what it exempts;
//   - the trust bundle lands inside the run's runtime dir, which the runner
//     already binds at its HOST-IDENTICAL path — the reason the bundle needs no
//     new mount and no path translation.
//
// This is also as close as an integration test gets to issue #24's live
// acceptance line (an authenticated HTTPS call from inside the container to a
// granted service): the CA bundle is on disk at a bound host-identical path and
// the env points every TLS client at it. What remains unverifiable without a
// real sidecar — that OneCLI accepts the token in the Proxy-Authorization
// header this proxy URL renders, and injects the credential — is deliberately
// not faked here (ADR-0067: tests run against a stubbed HTTP API, never a live
// sidecar; gatewayProxyURL's doc is the single point of correction if a real
// build reads that header differently).
func TestStart_ContainerRunnerGatewayBundle(t *testing.T) {
	f := newFixture(t)
	f.enableContainer(t)
	api := newGatewayStub(onecli.Grant{Kind: onecli.GrantSecret, ID: "sec_1", Name: "stripe"})
	gatewayURL := f.enableGateway(t, api)

	run, err := f.svc.Start(t.Context(), StartParams{RepoID: f.repo.ID})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	name := "proj~20260608-1530"
	sess, live := f.runner.Session(name)
	if !live {
		t.Fatal("session not live after a gateway-wired container Start")
	}

	runtimeDir := f.homes.RuntimePath(run.ID)
	bundle := filepath.Join(runtimeDir, trustBundleName)
	proxyURL := proxyURLFor(gatewayURL, testProxyToken)
	// The fixture repo's remote is a file:// path (no forge host to exempt), so
	// the list is the lab host plus the provider's declared direct hosts.
	noProxy := strings.Join(append([]string{"127.0.0.1"}, f.directAPIHosts(t)...), ",")

	wt := filepath.Join(f.worktreeRoot, "proj-20260608-1530")
	wantArgv := podmanx.RunArgv(podmanx.RunSpec{
		Bin:         testPodmanBin,
		Name:        podmanx.ContainerName(name),
		Image:       testDevImage,
		ToolsImage:  testToolsImage,
		WorktreeDir: wt,
		BareDir:     f.bare(),
		AgentDir:    "/var/lib/lab-test/agent",
		HomeDir:     f.homes.HomePath(run.ID),
		RuntimeDir:  runtimeDir,
		Memory:      "8g",
		Pids:        4096,
		Nofile:      16384,
		// The visible half of the bundle: hostnames and paths as K=V.
		Env: []string{
			"LAB_URL=unix:///var/lib/lab-test/agent/agent.sock",
			"HOME=" + podmanx.Home,
			"NO_PROXY=" + noProxy,
			"no_proxy=" + noProxy,
			"SSL_CERT_FILE=" + bundle,
			"NODE_EXTRA_CA_CERTS=" + bundle,
			"REQUESTS_CA_BUNDLE=" + bundle,
			"GIT_SSL_CAINFO=" + bundle,
			"PATH=" + podmanx.PATH,
		},
		// The secret half: names only.
		ForwardEnv: []string{"LAB_TOKEN", "HTTPS_PROXY", "https_proxy", "TERM"},
		Argv:       f.wantSpawnArgv(name, run.Model, run.Effort, "", run.ID),
	})
	if !slices.Equal(sess.Argv, wantArgv) {
		t.Errorf("container pane argv =\n  %q\nwant\n  %q", sess.Argv, wantArgv)
	}

	// The token value appears NOWHERE in the argv — grep the whole command line,
	// not just the entries this test happens to predict.
	if joined := strings.Join(sess.Argv, " "); strings.Contains(joined, testProxyToken) {
		t.Errorf("the agent identity's proxy token leaked into the pane argv:\n  %s", joined)
	}
	// The proxy pair's VALUES exist only in the tmux -e payload, beside
	// LAB_TOKEN's.
	if len(sess.ExtraEnv) != 3 {
		t.Fatalf("container tmux env = %q, want exactly [LAB_TOKEN=…, HTTPS_PROXY=…, https_proxy=…]", sess.ExtraEnv)
	}
	if !strings.HasPrefix(sess.ExtraEnv[0], "LAB_TOKEN=lab_run_") {
		t.Errorf("container tmux env[0] = %q, want the run token", sess.ExtraEnv[0])
	}
	if got, want := sess.ExtraEnv[1:], []string{"HTTPS_PROXY=" + proxyURL, "https_proxy=" + proxyURL}; !slices.Equal(got, want) {
		t.Errorf("container tmux env proxy entries = %q, want %q", got, want)
	}

	// The runtime dir rides the argv as a host-identical bind, which is what
	// makes the bundle path in the env valid on both sides of the boundary.
	if bind := "-v " + runtimeDir + ":" + runtimeDir; !strings.Contains(strings.Join(sess.Argv, " "), bind) {
		t.Errorf("pane argv carries no host-identical runtime bind %q:\n  %q", bind, sess.Argv)
	}
	got, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatalf("trust bundle missing inside the bound runtime dir: %v", err)
	}
	if want := testSystemPEM + testGatewayPEM; string(got) != want {
		t.Errorf("trust bundle =\n%q\nwant\n%q", got, want)
	}
}
