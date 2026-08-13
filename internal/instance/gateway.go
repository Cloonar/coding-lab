package instance

// gateway.go is the PURE core of the OneCLI run wiring (issue #24 /
// ADR-0067): everything a run needs to speak to the credential gateway,
// expressed as functions over strings and one file write. The launch path
// calls these; none of them reads a Service field, dials anything, or logs.
// That split is deliberate — the launch sequence is hard to drive from a test
// and these are the parts whose EXACT output is the contract (an env bundle
// compared as a slice, a URL compared byte for byte, a PEM file compared to a
// concatenation), so they live where a table test can pin them.
//
// The property the whole file serves: an instance never holds a secret VALUE.
// It holds a proxy URL carrying its repo's agent-identity token, and the
// gateway injects the real credential on the way out (ADR-0067's first pin).
// So the token in the proxy URL is the one credential this file handles, and
// every function here is written as if it will one day be read by someone
// looking for where lab leaked it. It does not: no token ever enters an error
// string, and the assembled proxy URL is a secret-bearing string that callers
// must treat like LAB_TOKEN.

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// trustBundleName is the basename writeTrustBundle writes inside the run's
// runtime dir. Fixed, not derived from the run id: the file already lives in a
// per-run directory, so a per-run basename would only make the path harder to
// recognize in an argv or a strace.
const trustBundleName = "onecli-ca-bundle.pem"

// pemCertHeader is the marker writeTrustBundle checks the configured gateway
// CA for. Checking the HEADER rather than parsing with encoding/pem is the
// point: lab is not validating the operator's CA, it is refusing to build a
// trust bundle out of a file that is obviously not one (a DER blob, a
// downloaded HTML error page, an empty file left by a failed provisioning
// step).
const pemCertHeader = "-----BEGIN CERTIFICATE-----"

// proxyBundleEnv renders the run's proxy env bundle: the entries that point a
// run's outbound HTTPS at the credential gateway and tell every HTTPS client
// in the run to trust the gateway's interception CA (issue #24 / ADR-0067).
// Order is fixed and load-bearing — the container argv is compared as a slice
// in tests and rendered into a visible command line in production, so a
// map-ordered bundle would make both nondeterministic.
//
// Why each variable, and why the case duplication is NOT redundant: real
// tools split on case and lab does not get to pick which one an agent reaches
// for. curl reads lowercase `https_proxy` only (it ignores `HTTPS_PROXY` for
// http_proxy-parity reasons of its own); Go's net/http and python-requests
// read either; some Node HTTP stacks read only the uppercase spelling. A
// gateway a tool fails to notice is worse than no gateway at all — the tool
// makes the request directly, gets a 401 from a service it was "granted", and
// the run spends its budget clock on a permissions bug that does not exist.
// Do not "clean this up" to one spelling.
//
// The four CA variables all carry the SAME path, for the same reason in four
// dialects: SSL_CERT_FILE (OpenSSL, curl, and Go's own x509 loader),
// NODE_EXTRA_CA_CERTS (the provider CLIs are Node programs),
// REQUESTS_CA_BUNDLE (python-requests, which every scripting agent reaches
// for), GIT_SSL_CAINFO (git's https transport, which must keep working
// through a proxy that terminates TLS).
//
// A blank noProxy OMITS the NO_PROXY/no_proxy pair entirely rather than
// emitting it empty. The two are not the same thing: several clients treat a
// PRESENT NO_PROXY as an explicit, authoritative exemption list and skip any
// other source they would otherwise consult, so `NO_PROXY=` can read as
// "exempt nothing, and stop looking" — a strictly worse state than never
// having set it. In practice noProxyValue yields at least the run's forge host
// and its provider's direct API hosts, so the empty case is the defensive
// branch, not the common one.
//
// Deliberately absent: HTTP_PROXY/http_proxy. The gateway fronts outbound
// HTTPS (ADR-0067); routing a run's plaintext http through it is a separate
// decision nobody has made, and adding the pair here would make it silently.
//
// The proxyURL argument carries the agent identity's token in userinfo (see
// gatewayProxyURL). The returned slice is therefore SECRET-BEARING as a
// whole: the entries isProxySecretEnv names must never reach an argv or a log.
func proxyBundleEnv(proxyURL, trustBundlePath, noProxy string) []string {
	env := []string{
		"HTTPS_PROXY=" + proxyURL,
		"https_proxy=" + proxyURL,
	}
	if noProxy != "" {
		env = append(env,
			"NO_PROXY="+noProxy,
			"no_proxy="+noProxy,
		)
	}
	return append(env,
		"SSL_CERT_FILE="+trustBundlePath,
		"NODE_EXTRA_CA_CERTS="+trustBundlePath,
		"REQUESTS_CA_BUNDLE="+trustBundlePath,
		"GIT_SSL_CAINFO="+trustBundlePath,
	)
}

// isProxySecretEnv reports whether an env NAME from proxyBundleEnv's bundle
// carries a credential. This is the ONE place that classification lives —
// container.go's containerEnv asks this question of the bundle the same way it
// answers it for LAB_TOKEN, and a second copy of the answer somewhere else is
// how the two drift apart.
//
// Exactly HTTPS_PROXY and https_proxy are secret: both carry the repo's agent
// identity token as userinfo in the proxy URL, so a container run must forward
// them BY NAME through tmux `-e` (podman copies the value out of the pane
// environment) and never as `--env K=V` in the podman argv, where the value
// would be visible in `ps`, in podman's own logs, and in every argv comparison
// a test prints. Everything else in the bundle is a filesystem path or a
// hostname list — public by construction, argv-safe, and deliberately visible
// so an operator can read a run's command line and see what it trusts.
func isProxySecretEnv(name string) bool {
	return name == "HTTPS_PROXY" || name == "https_proxy"
}

// gatewayProxyURL folds an agent identity's proxy token into the configured
// gateway URL as userinfo, yielding the value HTTPS_PROXY carries (issue #24 /
// ADR-0067). Any userinfo already present in the configured URL is REPLACED —
// the operator's --onecli-gateway-url names an address, and a credential
// someone parked in it is not the run's credential.
//
// # This function is the single point of correction
//
// Read this before changing anything else about how a run authenticates to
// the gateway — the register here is internal/onecli/wire.go's, and for the
// same reason. OneCLI authenticates a client with a per-agent token in the
// `Proxy-Authorization` header (ADR-0067). Standard HTTP clients derive that
// header from proxy-URL userinfo as `Proxy-Authorization: Basic
// base64(user:pass)`, so putting the token in the USERNAME slot with no
// password is lab's documented assumption about how OneCLI reads that header:
// it expects the token to arrive as the basic-auth username. That assumption
// was made without a live sidecar to check it against.
//
// If a real OneCLI build disagrees — it wants the token as the PASSWORD
// (url.UserPassword("", token)), under a fixed username, or as a raw
// `Proxy-Authorization: Bearer …` that no proxy-URL form can express — this
// function is the only edit. Every caller consumes an opaque URL string, so
// nothing downstream knows or cares which slot the token sits in. Do not
// spread that knowledge outward by, say, having the launch path build the
// header itself.
//
// The returned string CONTAINS THE TOKEN. Never log it, never fold it into an
// error, never put it in an argv (isProxySecretEnv is the enforcement).
// Errors here name the failing shape and the flag that fixes it and never
// quote the token or the assembled URL; a url.Parse failure is unwrapped to
// its reason so the *url.Error's echo of the input cannot smuggle a
// credential the operator parked in the configured URL into a log line.
func gatewayProxyURL(gatewayURL, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", errors.New("onecli gateway: the agent identity's proxy token is empty — a run cannot authenticate to the gateway without one")
	}
	raw := strings.TrimSpace(gatewayURL)
	if raw == "" {
		return "", errors.New("onecli gateway: the gateway URL is empty — set --onecli-gateway-url (e.g. http://10.0.0.1:10255)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("onecli gateway: the gateway URL is not a valid http(s) URL (e.g. http://10.0.0.1:10255): %w", urlErrReason(err))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("onecli gateway: the gateway URL must be an http(s) URL, got scheme %q — set --onecli-gateway-url to the sidecar's proxy address (e.g. http://10.0.0.1:10255)", u.Scheme)
	}
	// Hostname(), not Host: "http://:10255" has a non-empty Host and no host at
	// all, and a run handed that would fail at connect time instead of here.
	if u.Hostname() == "" {
		return "", errors.New("onecli gateway: the gateway URL must include a host (e.g. http://10.0.0.1:10255) — a containerized run cannot reach a loopback-only address (ADR-0052's host.containers.internal pin)")
	}
	// url.User escapes whatever the token contains on render (@ / ? : and
	// non-ASCII all survive a Parse round trip), so no token shape needs
	// pre-encoding here — and pre-encoding one would double-escape it.
	u.User = url.User(token)
	return u.String(), nil
}

// urlErrReason unwraps a *url.Error to its underlying reason, discarding the
// URL the error would otherwise quote back — the same discipline (and the same
// name) as internal/onecli's helper, for the same reason: a configured URL can
// legitimately carry userinfo, so its raw text is treated as potentially
// secret-bearing wherever it appears in an error.
func urlErrReason(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// noProxyValue composes a run's NO_PROXY value: the hosts whose traffic must
// stay DIRECT even though every other outbound request goes through the
// credential gateway (issue #24 / ADR-0067).
//
// The rule this encodes: a gateway outage must break exactly one thing —
// secret-backed API calls — and nothing else. Everything on this list is
// traffic a run cannot lose without losing the ability to report what went
// wrong:
//
//   - The lab host, so labctl and the agent API keep working. A run that
//     cannot reach lab cannot post a comment, open a PR, or finish its claim;
//     it just dies silently while the operator watches an idle pane. (For a
//     container run LAB_URL is rewritten to the mounted unix socket by
//     containerEnv, which no proxy would see anyway — the entry costs nothing
//     there and is load-bearing for a host run's TCP agent URL.)
//   - The repo's forge host, so `git push`, `git fetch`, and the tracker keep
//     working. Git auth is ADR-0006's vault credential materialized per
//     operation, NOT a gateway grant (ADR-0067 scopes the gateway to repo
//     secrets and explicitly leaves git auth untouched) — so forge traffic has
//     nothing to gain from the proxy and everything to lose.
//   - directHosts, the RESOLVING provider's own declared API hosts
//     (provider.SeedMeta().DirectAPIHosts), so the agent keeps streaming.
//     ADR-0067 rules LLM traffic out of the gateway's scope; an unreachable
//     gateway must not be able to take the model connection down with it. The
//     list arrives from the provider and is NOT a constant here, deliberately:
//     core naming one provider's API host is precisely what ADR-0033's
//     neutrality guard forbids (internal/provider's
//     TestCoreAttributionNeutrality fails the build on it), and it would be
//     wrong as well — a second provider's runs would exempt the first
//     provider's host and proxy their own model traffic, silently. Do not
//     "simplify" this parameter back into a constant; the adapter is the only
//     layer that knows which endpoint its CLI dials. An empty list is legal
//     and simply contributes nothing.
//
// Entries are BARE HOSTNAMES, deduped, comma-joined, no spaces, in the order
// above (the caller's order within directHosts). The lab and forge entries get
// their port stripped on the way out of the URL they are read from; the
// provider's hosts are declared bare already. The port strip is deliberate: a
// bare host in NO_PROXY matches every port on that
// host in Go, curl, and requests, and the thing being exempted is the HOST —
// lab on 8080 and lab's socket, the forge on 443 and on its ssh port, are one
// destination as far as this list is concerned. Adding the port would narrow
// the exemption to whichever port the URL happened to name and quietly proxy
// the rest. The spacing matters too: some clients split NO_PROXY on ","
// without trimming, so " git.example.com" is a different, never-matching
// entry.
func noProxyValue(labURL, repoRemoteURL string, directHosts []string) string {
	var hosts []string
	seen := make(map[string]bool, 2+len(directHosts))
	add := func(host string) {
		if host == "" || seen[host] {
			return
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	add(labProxyHost(labURL))
	add(forgeHost(repoRemoteURL))
	// The provider's hosts are taken as declared — no port strip and no
	// parsing: SeedMeta().DirectAPIHosts is documented as bare hostnames, and
	// re-deriving a host from one here would invent a second, weaker contract
	// beside the declared one. Blank entries are dropped by add.
	for _, h := range directHosts {
		add(h)
	}
	return strings.Join(hosts, ",")
}

// labProxyHost is the lab host to exempt for a run, read off LAB_URL. A
// `unix://` LAB_URL — the default (issue #201: the agent socket unless an
// explicit --agent-url is set) — contributes NOTHING, and that is the correct
// answer rather than a gap: a unix socket has no host, is never proxied by any
// client, and inventing a placeholder entry for it would put a meaningless
// token in NO_PROXY that an operator reading the run's env would have to
// puzzle over.
func labProxyHost(labURL string) string {
	u, err := url.Parse(strings.TrimSpace(labURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Hostname()
}

// forgeHost extracts the bare host from a git remote URL (store.Repo.RemoteURL)
// for noProxyValue's exemption list. Port and userinfo are stripped; anything
// unreadable yields "" and simply contributes no entry — a repo cloned from a
// local path has no forge to exempt, and refusing a spawn over it would be
// absurd.
//
// The three shapes that actually appear in this repo's rows:
//
//	https://host/owner/repo.git       — scheme'd, the http(s) clone URL
//	ssh://git@host:22/owner/repo.git  — scheme'd, explicit port
//	git@host:owner/repo.git           — scp-like, NO scheme
//
// The scp-like form is why this is not a one-line url.Parse. Its colon
// separates host from PATH and is not a port, and url.Parse rejects or
// mis-reads it outright ("first path segment in URL cannot contain colon").
// The presence of "://" is the discriminator, checked before parsing rather
// than after a failed parse: url.Parse ACCEPTS plenty of junk (it reads
// "gateway.example.com:10255" as scheme "gateway.example.com"), so "did it
// error" is not a usable test for "was this a URL".
func forgeHost(remoteURL string) string {
	raw := strings.TrimSpace(remoteURL)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		// Hostname() drops the port and the userinfo, and returns "" for the
		// host-less shapes (file:///srv/repo.git) that are local paths wearing
		// a scheme.
		return u.Hostname()
	}
	rest := raw
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:] // strip git@ / forgejo@ / anything@
	}
	// A bracketed IPv6 literal is legal in the scp-like form (git@[2001:db8::1]:o/r.git)
	// and must be read whole — splitting on the first colon would yield "[2001",
	// and junk in NO_PROXY is worse than no entry, since it silently matches
	// nothing.
	if strings.HasPrefix(rest, "[") {
		if end := strings.Index(rest, "]:"); end > 1 {
			return rest[1:end]
		}
		return ""
	}
	host, path, ok := strings.Cut(rest, ":")
	// No colon at all is a local path (/srv/git/repo.git, ../sibling); a colon
	// with nothing on one side of it is not a remote either. A slash BEFORE the
	// colon means the colon is inside a path, not after a host.
	if !ok || host == "" || path == "" || strings.Contains(host, "/") {
		return ""
	}
	return host
}

// systemCACandidates is the search list writeTrustBundle reads the host's
// system CA roots from, first readable file wins. It MIRRORS crypto/x509's own
// certFiles list (src/crypto/x509/root_linux.go), in its order, deliberately:
// the run's HTTPS clients — the Go binaries among them literally — resolve
// their roots from exactly this list, so composing the bundle from a different
// file than the one the host would have used is how a run ends up trusting a
// different world than lab thinks it does. Keep it in sync with upstream's
// list rather than curating a better one.
//
// A package var, not a const array, so tests can point it at a temp file
// without touching /etc.
var systemCACandidates = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian/Ubuntu/Gentoo/NixOS
	"/etc/pki/tls/certs/ca-bundle.crt",                  // Fedora/RHEL 6
	"/etc/ssl/ca-bundle.pem",                            // OpenSUSE
	"/etc/pki/tls/cacert.pem",                           // OpenELEC
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // CentOS/RHEL 7
	"/etc/ssl/cert.pem",                                 // Alpine
}

// writeTrustBundle composes the trust bundle a gateway-wired run runs with —
// the host's system CA roots FOLLOWED BY the gateway's interception CA — into
// dir, and returns the written path (issue #24 / ADR-0067). It is what the
// four CA variables in proxyBundleEnv point at.
//
// Both halves are required, and the ORDER of the argument list is the order of
// the reasoning:
//
//   - The system roots, because a run does not stop making direct HTTPS calls
//     when it gains a proxy. Everything noProxyValue exempts — lab, the forge,
//     the provider's own declared API hosts — is verified against these roots,
//     by clients whose SSL_CERT_FILE this bundle has just replaced.
//   - The gateway CA, because the proxy terminates TLS to inject credentials.
//     Without it every proxied request fails certificate verification.
//
// # Why no bare-CA fallback
//
// If no system bundle is found this returns an ERROR naming the candidates. It
// does NOT fall back to writing the gateway CA alone, and the temptation to
// "make it work anyway" is exactly what must not be indulged: a run whose
// SSL_CERT_FILE holds only the interception CA can verify nothing it reaches
// directly. The agent's model connection, `git push`, and labctl all break —
// the three things NO_PROXY exists to protect — while proxied calls keep
// working, so the run half-works in the most confusing possible way: some
// HTTPS fine, some HTTPS failing with x509 errors, and no single symptom
// pointing at the cause. A loud refusal at spawn names the host misconfiguration
// once. ADR-0067's fail-closed pin is the same argument one layer up.
//
// The caller writes this into the run's PER-RUN runtime dir
// (instancehome.RuntimePath). That directory is already bind-mounted at its
// HOST-IDENTICAL path into a container run (podmanx.RunSpec.RuntimeDir, rw,
// `-v <dir>:<dir>`), which is the whole reason this needs no new mount and no
// path translation: the string this function returns is valid inside the
// container and outside it, unchanged, exactly as the git credential files
// beside it already are. dir must therefore be ABSOLUTE — a relative dir would
// resolve against lab's cwd on the host and against nothing at all in the
// container.
//
// Mode 0644, and that is not an oversight: a CA bundle is public material (the
// system half is literally shipped by the distro), and the container run's
// user-namespace mapping means a 0600 file owned by the lab user is a file the
// agent process may not be able to read.
func writeTrustBundle(dir, gatewayCAFile string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("onecli gateway: the trust-bundle directory %q must be an absolute path — a run references the bundle by absolute path on both sides of the container boundary", dir)
	}
	if strings.TrimSpace(gatewayCAFile) == "" {
		return "", errors.New("onecli gateway: no gateway CA file is configured — a run cannot verify the gateway's intercepted TLS without it")
	}
	gatewayPEM, err := os.ReadFile(gatewayCAFile)
	if err != nil {
		return "", fmt.Errorf("onecli gateway: reading the gateway CA file %s: %w", gatewayCAFile, err)
	}
	if !bytes.Contains(gatewayPEM, []byte(pemCertHeader)) {
		return "", fmt.Errorf("onecli gateway: the gateway CA file %s carries no %q block — it must be the sidecar's CA certificate in PEM form, not DER and not a key", gatewayCAFile, pemCertHeader)
	}
	systemPEM, err := readSystemCARoots()
	if err != nil {
		return "", err
	}

	// Exactly one newline between the halves. A system bundle that does not end
	// in one would otherwise glue its last "-----END CERTIFICATE-----" onto the
	// gateway's "-----BEGIN CERTIFICATE-----", and a PEM parser reading that
	// line finds neither marker: the system roots' LAST certificate and the
	// gateway CA would both vanish from the bundle, silently.
	var buf bytes.Buffer
	buf.Write(bytes.TrimRight(systemPEM, "\n"))
	buf.WriteByte('\n')
	buf.Write(gatewayPEM)

	path := filepath.Join(dir, trustBundleName)
	// No MkdirAll: dir is the run's runtime dir, which the launch path has
	// already created (vault.NewMaterializer). Creating it here would mean this
	// function could succeed against a typo'd path nobody mounts.
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return "", fmt.Errorf("onecli gateway: writing the run's trust bundle %s: %w", path, err)
	}
	return path, nil
}

// readSystemCARoots returns the contents of the first USABLE entry in
// systemCACandidates, or an error naming every candidate it tried.
//
// "Usable" is readable AND non-empty. A zero-byte ca-certificates.crt is a
// broken trust store, not an empty one — it is what a failed provisioning step
// or a half-populated container image leaves behind — and accepting it would
// produce precisely the bare-gateway-CA bundle writeTrustBundle's doc explains
// must never be built, only reached through a side door instead of the front
// one this function guards.
func readSystemCARoots() ([]byte, error) {
	for _, candidate := range systemCACandidates {
		pem, err := os.ReadFile(candidate)
		if err != nil || len(bytes.TrimSpace(pem)) == 0 {
			continue
		}
		return pem, nil
	}
	return nil, fmt.Errorf("onecli gateway: no system CA bundle found on this host — tried %s; a run's trust bundle must carry the system roots as well as the gateway CA, and lab refuses to build one without them", strings.Join(systemCACandidates, ", "))
}
