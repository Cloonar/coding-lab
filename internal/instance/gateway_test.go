package instance

import (
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The two fake PEMs the trust-bundle tests concatenate. They are not real
// certificates and do not need to be — writeTrustBundle checks for the PEM
// header and never parses, precisely so lab is not in the business of
// validating the operator's CA.
const (
	testSystemPEM  = "-----BEGIN CERTIFICATE-----\nc3lzdGVtLXJvb3RzCg==\n-----END CERTIFICATE-----\n"
	testGatewayPEM = "-----BEGIN CERTIFICATE-----\nZ2F0ZXdheS1jYQo=\n-----END CERTIFICATE-----\n"
)

// writeFile drops contents at path with mode 0644 and fails the test on error.
func writeFile(t *testing.T, path, contents string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// useSystemCARoots points systemCACandidates at a temp file holding contents,
// behind one deliberately ABSENT candidate so the "first READABLE wins" search
// is exercised rather than just the first entry. Restored on cleanup.
func useSystemCARoots(t *testing.T, contents string) {
	t.Helper()
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "system-roots.pem"), contents)
	old := systemCACandidates
	t.Cleanup(func() { systemCACandidates = old })
	systemCACandidates = []string{filepath.Join(dir, "absent-ca-bundle.crt"), path}
}

// proxyBundleEnv's exact slice and order — the contract the container argv and
// the launch wiring both compare against (see the function doc for why the
// case duplication is not redundant).
func TestProxyBundleEnv(t *testing.T) {
	const (
		proxyURL = "http://tok@10.0.0.1:10255"
		bundle   = "/state/instances/run_1/runtime/onecli-ca-bundle.pem"
		// An already-composed noProxyValue output — opaque to proxyBundleEnv,
		// and provider-neutral here because core never names a real API host
		// (the third entry stands in for the resolving provider's declared
		// DirectAPIHosts).
		noProxy = "127.0.0.1,git.example.com,api.provider.example.com"
	)

	t.Run("full bundle", func(t *testing.T) {
		got := proxyBundleEnv(proxyURL, bundle, noProxy)
		want := []string{
			"HTTPS_PROXY=" + proxyURL,
			"https_proxy=" + proxyURL,
			"NO_PROXY=" + noProxy,
			"no_proxy=" + noProxy,
			"SSL_CERT_FILE=" + bundle,
			"NODE_EXTRA_CA_CERTS=" + bundle,
			"REQUESTS_CA_BUNDLE=" + bundle,
			"GIT_SSL_CAINFO=" + bundle,
		}
		if !slices.Equal(got, want) {
			t.Errorf("proxyBundleEnv =\n  %q\nwant\n  %q", got, want)
		}
	})

	// An empty noProxy omits the pair ENTIRELY rather than emitting it empty:
	// a present-but-empty NO_PROXY reads as an authoritative "exempt nothing"
	// to some clients, which is strictly worse than never setting it.
	t.Run("blank NO_PROXY omits the pair", func(t *testing.T) {
		got := proxyBundleEnv(proxyURL, bundle, "")
		want := []string{
			"HTTPS_PROXY=" + proxyURL,
			"https_proxy=" + proxyURL,
			"SSL_CERT_FILE=" + bundle,
			"NODE_EXTRA_CA_CERTS=" + bundle,
			"REQUESTS_CA_BUNDLE=" + bundle,
			"GIT_SSL_CAINFO=" + bundle,
		}
		if !slices.Equal(got, want) {
			t.Errorf("proxyBundleEnv with no NO_PROXY =\n  %q\nwant\n  %q", got, want)
		}
		for _, kv := range got {
			if strings.HasPrefix(kv, "NO_PROXY=") || strings.HasPrefix(kv, "no_proxy=") {
				t.Errorf("blank noProxy still emitted %q", kv)
			}
		}
	})

	// Exactly the two proxy entries are classified secret; every other entry is
	// a path or a hostname list and rides the visible argv.
	t.Run("only the proxy entries are secret", func(t *testing.T) {
		var secret []string
		for _, kv := range proxyBundleEnv(proxyURL, bundle, noProxy) {
			name, _, _ := strings.Cut(kv, "=")
			if isProxySecretEnv(name) {
				secret = append(secret, name)
			}
		}
		if want := []string{"HTTPS_PROXY", "https_proxy"}; !slices.Equal(secret, want) {
			t.Errorf("secret bundle entries = %q, want %q", secret, want)
		}
	})
}

// isProxySecretEnv is the single place the secret/non-secret split of the proxy
// bundle lives (issue #24): the two proxy variables carry the agent identity's
// token in userinfo, nothing else does.
func TestIsProxySecretEnv(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"HTTPS_PROXY", true},
		{"https_proxy", true},
		{"NO_PROXY", false},
		{"no_proxy", false},
		{"SSL_CERT_FILE", false},
		{"NODE_EXTRA_CA_CERTS", false},
		{"REQUESTS_CA_BUNDLE", false},
		{"GIT_SSL_CAINFO", false},
		{"LAB_TOKEN", false}, // container.go's own case, not this one's
		{"HTTP_PROXY", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isProxySecretEnv(tc.name); got != tc.want {
			t.Errorf("isProxySecretEnv(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// gatewayProxyURL folds the agent identity's token into the configured gateway
// URL as the userinfo USERNAME (see the function doc: lab's documented
// assumption about OneCLI's Proxy-Authorization reading, and the single point
// of correction if a real build disagrees).
func TestGatewayProxyURL(t *testing.T) {
	t.Run("token folded in", func(t *testing.T) {
		got, err := gatewayProxyURL("http://10.0.0.1:10255", "agent-token")
		if err != nil {
			t.Fatalf("gatewayProxyURL: %v", err)
		}
		if want := "http://agent-token@10.0.0.1:10255"; got != want {
			t.Errorf("gatewayProxyURL = %q, want %q", got, want)
		}
	})

	// A credential parked in the configured URL is not the run's credential:
	// whatever userinfo the operator set is REPLACED, password included.
	t.Run("existing userinfo replaced", func(t *testing.T) {
		got, err := gatewayProxyURL("https://stale-user:stale-pass@gw.example.com:10255/", "agent-token")
		if err != nil {
			t.Fatalf("gatewayProxyURL: %v", err)
		}
		if want := "https://agent-token@gw.example.com:10255/"; got != want {
			t.Errorf("gatewayProxyURL = %q, want %q", got, want)
		}
		for _, leaked := range []string{"stale-user", "stale-pass"} {
			if strings.Contains(got, leaked) {
				t.Errorf("gatewayProxyURL kept the configured userinfo %q: %q", leaked, got)
			}
		}
	})

	// A token full of characters that mean something in a URL must survive the
	// round trip a real HTTP client makes: render, then Parse, then read the
	// username back. Percent-encoding is url.User's job — this pins that lab
	// neither pre-escapes (double-encoding it) nor skips it (truncating at the
	// first '@' or ':').
	t.Run("token needing escapes round-trips", func(t *testing.T) {
		const token = "p@ss:w/rd+ä?#&=;$, %20"
		got, err := gatewayProxyURL("http://10.0.0.1:10255", token)
		if err != nil {
			t.Fatalf("gatewayProxyURL: %v", err)
		}
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("the assembled proxy URL does not parse: %v", err)
		}
		if u.User.Username() != token {
			t.Errorf("round-tripped token = %q, want %q", u.User.Username(), token)
		}
		if _, set := u.User.Password(); set {
			t.Error("the token was rendered with a password component; it must occupy the USERNAME slot alone")
		}
		if u.Host != "10.0.0.1:10255" {
			t.Errorf("round-tripped host = %q, want the configured 10.0.0.1:10255", u.Host)
		}
	})

	t.Run("errors", func(t *testing.T) {
		const token = "super-secret-token"
		cases := []struct {
			name    string
			url     string
			token   string
			wantMsg string
		}{
			{name: "empty token", url: "http://10.0.0.1:10255", token: "", wantMsg: "proxy token is empty"},
			{name: "blank token", url: "http://10.0.0.1:10255", token: "   ", wantMsg: "proxy token is empty"},
			{name: "empty URL", url: "", token: token, wantMsg: "gateway URL is empty"},
			{name: "blank URL", url: "  ", token: token, wantMsg: "gateway URL is empty"},
			{name: "unparseable URL", url: "://10.0.0.1:10255", token: token, wantMsg: "not a valid http(s) URL"},
			{name: "bad escape", url: "http://10.0.0.1:10255/%zz", token: token, wantMsg: "not a valid http(s) URL"},
			{name: "non-http scheme", url: "socks5://10.0.0.1:1080", token: token, wantMsg: `must be an http(s) URL, got scheme "socks5"`},
			{name: "host-less scheme", url: "unix:///var/run/onecli.sock", token: token, wantMsg: "must be an http(s) URL"},
			// Two flavours of "the operator forgot the scheme": a numeric host
			// cannot be a scheme, so url.Parse rejects it outright; a NAMED host
			// parses as its own scheme and is caught one check later. Both must
			// be actionable, which is why the parse branch names the example.
			{name: "no scheme, numeric host", url: "10.0.0.1:10255", token: token, wantMsg: "not a valid http(s) URL"},
			{name: "no scheme, named host", url: "gateway.example.com:10255", token: token, wantMsg: `must be an http(s) URL, got scheme "gateway.example.com"`},
			{name: "missing host", url: "http://:10255", token: token, wantMsg: "must include a host"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got, err := gatewayProxyURL(tc.url, tc.token)
				if err == nil {
					t.Fatalf("gatewayProxyURL(%q) = %q, want an error", tc.url, got)
				}
				if got != "" {
					t.Errorf("gatewayProxyURL returned %q alongside its error, want \"\"", got)
				}
				if !strings.Contains(err.Error(), tc.wantMsg) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantMsg)
				}
				// The token NEVER enters an error string — the whole file is
				// written on that promise. (A blank token has no text to look
				// for; Contains against "" is trivially true.)
				if trimmed := strings.TrimSpace(tc.token); trimmed != "" && strings.Contains(err.Error(), trimmed) {
					t.Errorf("error %q leaked the proxy token", err)
				}
			})
		}
	})

	// A url.Parse failure is unwrapped to its reason so the *url.Error's echo of
	// the input cannot carry userinfo an operator parked in the configured URL
	// into a log line.
	t.Run("parse errors do not echo the configured URL", func(t *testing.T) {
		_, err := gatewayProxyURL("http://parked-user:parked-pass@10.0.0.1:10255/%zz", "agent-token")
		if err == nil {
			t.Fatal("gatewayProxyURL succeeded on a URL with a bad escape")
		}
		for _, leaked := range []string{"parked-user", "parked-pass"} {
			if strings.Contains(err.Error(), leaked) {
				t.Errorf("error %q echoed the configured URL's userinfo %q", err, leaked)
			}
		}
	})
}

// noProxyValue's composition: lab host, forge host, then the RESOLVING
// provider's declared direct API hosts in the caller's order — deduped, bare
// hostnames, comma-joined without spaces. The direct hosts arrive as a
// parameter (provider.SeedMeta().DirectAPIHosts) rather than a constant in
// this package, which is why the fixtures below name a synthetic provider host
// instead of any real one: core must not know which endpoint a given agent CLI
// dials (issue #24 / ADR-0033's neutrality rule).
func TestNoProxyValue(t *testing.T) {
	const (
		apiHost    = "api.provider.example.com"
		streamHost = "stream.provider.example.com"
	)
	cases := []struct {
		name      string
		labURL    string
		remoteURL string
		direct    []string
		want      string
	}{
		{
			name:      "http LAB_URL and https forge, both ports stripped",
			labURL:    "http://lab.example.com:8080",
			remoteURL: "https://git.example.com:8443/Cloonar/coding-lab.git",
			direct:    []string{apiHost},
			want:      "lab.example.com,git.example.com," + apiHost,
		},
		{
			// The default LAB_URL (issue #201): a unix socket has no host to
			// exempt and contributes nothing.
			name:      "unix LAB_URL contributes nothing",
			labURL:    "unix:///var/lib/lab/agent/agent.sock",
			remoteURL: "forgejo@git.cloonar.com:Cloonar/coding-lab.git",
			direct:    []string{apiHost},
			want:      "git.cloonar.com," + apiHost,
		},
		{
			name:      "lab host equal to forge host is emitted once",
			labURL:    "https://git.example.com:8080/",
			remoteURL: "ssh://git@git.example.com:22/Cloonar/coding-lab.git",
			direct:    []string{apiHost},
			want:      "git.example.com," + apiHost,
		},
		{
			name:      "no forge host",
			labURL:    "http://127.0.0.1:8080",
			remoteURL: "/srv/git/coding-lab.git",
			direct:    []string{apiHost},
			want:      "127.0.0.1," + apiHost,
		},
		{
			name:   "nothing configured still exempts the provider's API",
			labURL: "", remoteURL: "",
			direct: []string{apiHost},
			want:   apiHost,
		},
		{
			// A provider may declare several endpoints; they keep the order the
			// adapter declared them in, after lab and the forge.
			name:      "several direct hosts keep the provider's declared order",
			labURL:    "http://lab.example.com:8080",
			remoteURL: "https://git.example.com/Cloonar/coding-lab.git",
			direct:    []string{streamHost, apiHost},
			want:      "lab.example.com,git.example.com," + streamHost + "," + apiHost,
		},
		{
			// The empty declaration must be HARMLESS (provider.SeedMeta's rule):
			// no entry, never a broken one — the codex adapter ships this shape
			// today rather than guessing its CLI's API host.
			name:      "a provider that declares no direct host contributes nothing",
			labURL:    "http://lab.example.com:8080",
			remoteURL: "https://git.example.com/Cloonar/coding-lab.git",
			direct:    nil,
			want:      "lab.example.com,git.example.com",
		},
		{
			// A self-hosted forge that IS the provider's API host (or a lab
			// deployed behind it) must still produce one entry: NO_PROXY is a
			// set, and a duplicate is noise an operator has to decode.
			name:      "a direct host equal to the lab or forge host is emitted once",
			labURL:    "http://lab.example.com:8080",
			remoteURL: "https://git.example.com/Cloonar/coding-lab.git",
			direct:    []string{"git.example.com", apiHost, "lab.example.com", apiHost},
			want:      "lab.example.com,git.example.com," + apiHost,
		},
		{
			// A declaration with a blank entry (a stray "" in an adapter's
			// slice) contributes nothing rather than an empty NO_PROXY item,
			// which some clients read as a never-matching entry.
			name:      "a blank direct host is skipped",
			labURL:    "",
			remoteURL: "https://git.example.com/Cloonar/coding-lab.git",
			direct:    []string{"", apiHost},
			want:      "git.example.com," + apiHost,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := noProxyValue(tc.labURL, tc.remoteURL, tc.direct)
			if got != tc.want {
				t.Errorf("noProxyValue(%q, %q, %q) = %q, want %q", tc.labURL, tc.remoteURL, tc.direct, got, tc.want)
			}
			if strings.Contains(got, " ") {
				t.Errorf("noProxyValue = %q, want no spaces (some clients split on \",\" without trimming)", got)
			}
			// Every declared direct host appears EXACTLY once: the model
			// connection is the one thing a gateway outage must never touch
			// (ADR-0067 rules LLM traffic out of the gateway's scope), and a
			// duplicate would mean the dedupe stopped covering the list.
			entries := strings.Split(got, ",")
			for _, h := range tc.direct {
				if h == "" {
					continue
				}
				n := 0
				for _, e := range entries {
					if e == h {
						n++
					}
				}
				if n != 1 {
					t.Errorf("noProxyValue = %q contains the declared direct host %q %d time(s), want exactly 1", got, h, n)
				}
			}
		})
	}
}

// forgeHost reads the bare host out of every git remote shape this repo's rows
// actually carry, and refuses to guess at anything else.
func TestForgeHost(t *testing.T) {
	cases := []struct {
		name      string
		remoteURL string
		want      string
	}{
		{name: "https clone URL", remoteURL: "https://git.example.com/owner/repo.git", want: "git.example.com"},
		{name: "https with port", remoteURL: "https://git.example.com:8443/owner/repo.git", want: "git.example.com"},
		{name: "https with userinfo", remoteURL: "https://user:pw@git.example.com/owner/repo.git", want: "git.example.com"},
		{name: "ssh URL with explicit port", remoteURL: "ssh://git@git.example.com:22/owner/repo.git", want: "git.example.com"},
		{name: "ssh URL without port", remoteURL: "ssh://git@git.example.com/owner/repo.git", want: "git.example.com"},
		{name: "git protocol", remoteURL: "git://git.example.com/owner/repo.git", want: "git.example.com"},
		// The scp-like shape: the colon separates host from PATH and is NOT a
		// port, which is exactly why this cannot be a bare url.Parse.
		{name: "scp-like", remoteURL: "git@git.example.com:owner/repo.git", want: "git.example.com"},
		{name: "scp-like, non-git user", remoteURL: "forgejo@git.cloonar.com:Cloonar/coding-lab.git", want: "git.cloonar.com"},
		{name: "scp-like without a user", remoteURL: "git.example.com:owner/repo.git", want: "git.example.com"},
		{name: "scp-like IPv6 literal", remoteURL: "git@[2001:db8::1]:owner/repo.git", want: "2001:db8::1"},
		{name: "surrounding whitespace", remoteURL: "  https://git.example.com/owner/repo.git\n", want: "git.example.com"},
		// Everything unreadable contributes no entry rather than a guess.
		{name: "empty", remoteURL: "", want: ""},
		{name: "whitespace only", remoteURL: "   ", want: ""},
		{name: "absolute local path", remoteURL: "/srv/git/repo.git", want: ""},
		{name: "relative local path", remoteURL: "../sibling.git", want: ""},
		{name: "file URL", remoteURL: "file:///srv/git/repo.git", want: ""},
		{name: "scheme with no host", remoteURL: "https://", want: ""},
		{name: "colon inside a path", remoteURL: "/srv/git/weird:name.git", want: ""},
		{name: "trailing colon, no path", remoteURL: "git@git.example.com:", want: ""},
		{name: "leading colon, no host", remoteURL: ":owner/repo.git", want: ""},
		{name: "unterminated IPv6 literal", remoteURL: "git@[2001:db8::1", want: ""},
		{name: "junk", remoteURL: "not a remote at all", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forgeHost(tc.remoteURL); got != tc.want {
				t.Errorf("forgeHost(%q) = %q, want %q", tc.remoteURL, got, tc.want)
			}
		})
	}
}

// writeTrustBundle composes the system roots + the gateway CA into the run's
// runtime dir, and refuses — never falls back — when either half is missing
// (see the function doc for why a bare-CA bundle is the wrong answer).
func TestWriteTrustBundle(t *testing.T) {
	t.Run("happy path", func(t *testing.T) {
		useSystemCARoots(t, testSystemPEM)
		dir := t.TempDir()
		caFile := writeFile(t, filepath.Join(t.TempDir(), "gateway-ca.pem"), testGatewayPEM)

		path, err := writeTrustBundle(dir, caFile)
		if err != nil {
			t.Fatalf("writeTrustBundle: %v", err)
		}
		if want := filepath.Join(dir, "onecli-ca-bundle.pem"); path != want {
			t.Errorf("bundle path = %q, want %q", path, want)
		}
		if !filepath.IsAbs(path) {
			t.Errorf("bundle path %q is not absolute — the container references it at its host-identical path", path)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the bundle: %v", err)
		}
		if want := testSystemPEM + testGatewayPEM; string(got) != want {
			t.Errorf("bundle =\n%q\nwant\n%q", got, want)
		}
		// Both halves are present as themselves, and nothing glued.
		for _, half := range []string{"c3lzdGVtLXJvb3RzCg==", "Z2F0ZXdheS1jYQo="} {
			if !strings.Contains(string(got), half) {
				t.Errorf("bundle is missing the payload %q:\n%s", half, got)
			}
		}
		if strings.Contains(string(got), "-----END CERTIFICATE----------BEGIN CERTIFICATE-----") {
			t.Errorf("the two PEMs were glued together without a newline:\n%s", got)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != 0o644 {
			t.Errorf("bundle mode = %04o, want 0644 (public CA material a userns-mapped run must be able to read)", perm)
		}
	})

	// A system bundle that does not end in a newline must not glue its last
	// END marker onto the gateway's BEGIN marker — that would silently drop
	// BOTH certificates from the parsed bundle.
	t.Run("system roots without a trailing newline", func(t *testing.T) {
		useSystemCARoots(t, strings.TrimSuffix(testSystemPEM, "\n"))
		caFile := writeFile(t, filepath.Join(t.TempDir(), "gateway-ca.pem"), testGatewayPEM)

		path, err := writeTrustBundle(t.TempDir(), caFile)
		if err != nil {
			t.Fatalf("writeTrustBundle: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the bundle: %v", err)
		}
		if want := testSystemPEM + testGatewayPEM; string(got) != want {
			t.Errorf("bundle =\n%q\nwant exactly one newline between the halves\n%q", got, want)
		}
	})

	// And a system bundle ending in SEVERAL newlines still joins with exactly
	// one, so the output is a function of the certificates, not of the host's
	// file formatting.
	t.Run("system roots with extra trailing newlines", func(t *testing.T) {
		useSystemCARoots(t, testSystemPEM+"\n\n")
		caFile := writeFile(t, filepath.Join(t.TempDir(), "gateway-ca.pem"), testGatewayPEM)

		path, err := writeTrustBundle(t.TempDir(), caFile)
		if err != nil {
			t.Fatalf("writeTrustBundle: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the bundle: %v", err)
		}
		if want := testSystemPEM + testGatewayPEM; string(got) != want {
			t.Errorf("bundle =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("gateway CA refusals", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "absent-ca.pem")
		notPEM := writeFile(t, filepath.Join(dir, "not-a-pem.bin"), "\x30\x82\x01\x0a not DER either, but certainly not PEM\n")
		empty := writeFile(t, filepath.Join(dir, "empty.pem"), "")
		// A DIRECTORY reads back as an error for every uid, unlike a chmod-000
		// file (which root would happily read) — the portable "unreadable".
		unreadableDir := filepath.Join(dir, "ca-dir")
		if err := os.Mkdir(unreadableDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		cases := []struct {
			name    string
			caFile  string
			wantMsg []string
		}{
			{name: "unset", caFile: "", wantMsg: []string{"no gateway CA file is configured"}},
			{name: "blank", caFile: "   ", wantMsg: []string{"no gateway CA file is configured"}},
			{name: "missing", caFile: missing, wantMsg: []string{"reading the gateway CA file", missing}},
			{name: "unreadable", caFile: unreadableDir, wantMsg: []string{"reading the gateway CA file", unreadableDir}},
			{name: "not PEM", caFile: notPEM, wantMsg: []string{notPEM, "BEGIN CERTIFICATE"}},
			{name: "empty file", caFile: empty, wantMsg: []string{empty, "BEGIN CERTIFICATE"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				useSystemCARoots(t, testSystemPEM)
				out := t.TempDir()
				path, err := writeTrustBundle(out, tc.caFile)
				if err == nil {
					t.Fatalf("writeTrustBundle(%q) = %q, want an error", tc.caFile, path)
				}
				if path != "" {
					t.Errorf("writeTrustBundle returned %q alongside its error, want \"\"", path)
				}
				for _, msg := range tc.wantMsg {
					if !strings.Contains(err.Error(), msg) {
						t.Errorf("error = %q, want it to contain %q", err, msg)
					}
				}
				// A refusal writes nothing — a half-built bundle is worse than none.
				if _, statErr := os.Stat(filepath.Join(out, trustBundleName)); statErr == nil {
					t.Error("a refused writeTrustBundle still wrote a bundle")
				}
			})
		}
	})

	// No system bundle on the host is an ERROR naming the candidates, never a
	// bare-gateway-CA fallback: a run whose SSL_CERT_FILE holds only the
	// interception CA can verify nothing it reaches directly.
	t.Run("no system bundle found", func(t *testing.T) {
		dir := t.TempDir()
		old := systemCACandidates
		t.Cleanup(func() { systemCACandidates = old })
		absent := []string{filepath.Join(dir, "no-such-a.crt"), filepath.Join(dir, "no-such-b.pem")}
		systemCACandidates = absent
		caFile := writeFile(t, filepath.Join(dir, "gateway-ca.pem"), testGatewayPEM)

		out := t.TempDir()
		path, err := writeTrustBundle(out, caFile)
		if err == nil {
			t.Fatalf("writeTrustBundle = %q, want a refusal with no system CA bundle", path)
		}
		if !strings.Contains(err.Error(), "no system CA bundle found") {
			t.Errorf("error = %q, want it to name the missing system bundle", err)
		}
		for _, candidate := range absent {
			if !strings.Contains(err.Error(), candidate) {
				t.Errorf("error = %q, want it to name the candidate %q it tried", err, candidate)
			}
		}
		if _, statErr := os.Stat(filepath.Join(out, trustBundleName)); statErr == nil {
			t.Error("the refusal still wrote a bundle — a bare gateway CA is exactly what must not be produced")
		}
	})

	// A zero-byte candidate is a BROKEN trust store, not an empty one: the
	// search steps over it and takes the next usable file, and refuses if that
	// was the only candidate.
	t.Run("empty system candidate is skipped", func(t *testing.T) {
		dir := t.TempDir()
		old := systemCACandidates
		t.Cleanup(func() { systemCACandidates = old })
		blank := writeFile(t, filepath.Join(dir, "blank-roots.crt"), "\n\n")
		real := writeFile(t, filepath.Join(dir, "real-roots.crt"), testSystemPEM)
		caFile := writeFile(t, filepath.Join(dir, "gateway-ca.pem"), testGatewayPEM)

		systemCACandidates = []string{blank, real}
		path, err := writeTrustBundle(t.TempDir(), caFile)
		if err != nil {
			t.Fatalf("writeTrustBundle: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the bundle: %v", err)
		}
		if want := testSystemPEM + testGatewayPEM; string(got) != want {
			t.Errorf("bundle =\n%q\nwant the SECOND candidate's roots\n%q", got, want)
		}

		systemCACandidates = []string{blank}
		if path, err := writeTrustBundle(t.TempDir(), caFile); err == nil {
			t.Errorf("writeTrustBundle = %q, want a refusal when the only candidate is empty", path)
		}
	})

	// A relative directory would resolve against lab's cwd on the host and
	// against nothing at all inside the container, where the bundle is
	// referenced at its host-identical path.
	t.Run("relative directory refused", func(t *testing.T) {
		useSystemCARoots(t, testSystemPEM)
		caFile := writeFile(t, filepath.Join(t.TempDir(), "gateway-ca.pem"), testGatewayPEM)

		path, err := writeTrustBundle("runtime", caFile)
		if err == nil {
			t.Fatalf("writeTrustBundle = %q, want a refusal for a relative directory", path)
		}
		if !strings.Contains(err.Error(), "must be an absolute path") {
			t.Errorf("error = %q, want it to name the absolute-path requirement", err)
		}
	})
}
