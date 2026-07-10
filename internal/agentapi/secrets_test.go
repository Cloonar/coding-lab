package agentapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

// seedSecret encrypts value with the fixture's vault and stores it as a repo
// secret — the exact path the operator's `secret set` will take: the store
// only ever sees ciphertext.
func (f *testFixture) seedSecret(t *testing.T, repoID, name, description, value string) {
	t.Helper()
	blob, err := f.vlt.Encrypt([]byte(value))
	if err != nil {
		t.Fatalf("encrypt %s: %v", name, err)
	}
	if _, err := f.st.CreateRepoSecret(context.Background(), ids.NewID("sec"), repoID, name, description, blob, f.now); err != nil {
		t.Fatalf("CreateRepoSecret %s: %v", name, err)
	}
}

// TestSecretListMetadataOnly pins that GET /secrets returns name + description
// only — never an id, a timestamp, or (above all) the value.
func TestSecretListMetadataOnly(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "prod api key", "sup3r-s3cr3t-value")
	f.seedSecret(t, "repo_a", "DB_URL", "", "postgres://who:cares@host/db")

	rr := doJSON(t, f.server().Handler(), "GET", "/agent/v1/secrets", token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Names + descriptions present, ordered by name (store order).
	var resp struct {
		Secrets []secretMeta `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(resp.Secrets) != 2 {
		t.Fatalf("secrets = %+v, want 2", resp.Secrets)
	}
	if resp.Secrets[0].Name != "API_KEY" || resp.Secrets[0].Description != "prod api key" {
		t.Errorf("first = %+v, want API_KEY/prod api key", resp.Secrets[0])
	}
	if resp.Secrets[1].Name != "DB_URL" {
		t.Errorf("second = %+v, want DB_URL", resp.Secrets[1])
	}

	// The raw body must carry no value and no leaked metadata field.
	for _, forbidden := range []string{"sup3r-s3cr3t-value", "postgres://", "\"id\"", "created_at", "updated_at", "encrypted"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("list body leaked %q: %s", forbidden, body)
		}
	}
}

// TestSecretValuesRoundTrip pins the decrypt path: an encrypted secret comes
// back as its plaintext, keyed by name.
func TestSecretValuesRoundTrip(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "", "plaintext-A")
	f.seedSecret(t, "repo_a", "B_KEY", "", "plaintext-B")

	rr := doJSON(t, f.server().Handler(), "POST", "/agent/v1/secrets/values", token, `{"names":["API_KEY","B_KEY"]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Values map[string]string `json:"values"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Values["API_KEY"] != "plaintext-A" || resp.Values["B_KEY"] != "plaintext-B" {
		t.Fatalf("values = %v, want the two plaintexts", resp.Values)
	}
}

// TestSecretValuesPartialMiss pins the all-or-nothing rule: one unknown name
// among knowns is a 404 naming ONLY the missing (sorted), with NO values.
func TestSecretValuesPartialMiss(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "", "plaintext-A")

	rr := doJSON(t, f.server().Handler(), "POST", "/agent/v1/secrets/values", token,
		`{"names":["API_KEY","ZEBRA","ALPHA"]}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s, want 404", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Missing named, sorted; the known name is not the subject of the error and
	// no value (or values key) is present.
	if !strings.Contains(body, "unknown secret(s): ALPHA, ZEBRA") {
		t.Errorf("body = %q, want sorted missing names", body)
	}
	for _, forbidden := range []string{"plaintext-A", "\"values\""} {
		if strings.Contains(body, forbidden) {
			t.Errorf("partial-miss body leaked %q: %s", forbidden, body)
		}
	}
}

// TestSecretValuesEmptyNames pins that an empty (or missing) names list is a
// 400 — a values fetch must name at least one secret.
func TestSecretValuesEmptyNames(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)

	for _, body := range []string{`{"names":[]}`, `{}`} {
		rr := doJSON(t, f.server().Handler(), "POST", "/agent/v1/secrets/values", token, body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rr.Code)
		}
	}
}

// TestSecretScopingCrossRepo pins the run-token scope: a run in repo A cannot
// see or fetch repo B's secret — B's name simply does not exist in A's scope,
// so a list is empty and a values fetch is the same 404 as any unknown name.
func TestSecretScopingCrossRepo(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRepo(t, "repo_b")
	f.seedRun(t, "run_a", "repo_a", "active")
	f.seedRun(t, "run_b", "repo_b", "active")
	tokenA := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_b", "SHARED", "b's secret", "b-plaintext")

	handler := f.server().Handler()

	// A's list does not contain B's secret.
	rr := doJSON(t, handler, "GET", "/agent/v1/secrets", tokenA, "")
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), "SHARED") {
		t.Fatalf("A list = %d %s, want 200 without SHARED", rr.Code, rr.Body.String())
	}

	// A fetching B's name by value: 404, no plaintext.
	rr = doJSON(t, handler, "POST", "/agent/v1/secrets/values", tokenA, `{"names":["SHARED"]}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("A fetch B: status = %d, want 404", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "b-plaintext") {
		t.Errorf("cross-repo fetch leaked B's plaintext: %s", rr.Body.String())
	}
}

// TestSecretRoutesRequireAuth pins that every secret route sits behind the
// run token: no header and a garbage token are the same opaque 401.
func TestSecretRoutesRequireAuth(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	handler := f.server().Handler()

	routes := []struct{ method, path, body string }{
		{"GET", "/agent/v1/secrets", ""},
		{"POST", "/agent/v1/secrets/values", `{"names":["API_KEY"]}`},
		{"POST", "/agent/v1/secrets/scan", "+++ b/a.txt\n+leaky\n"},
	}
	for _, rt := range routes {
		for _, authz := range []string{"", "Bearer lab_run_bogus"} {
			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
			if authz != "" {
				req.Header.Set("Authorization", authz)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("%s %s auth=%q: status = %d, want 401", rt.method, rt.path, authz, rr.Code)
			}
		}
	}
}

// --- POST /secrets/scan (issue #106, the pre-push leak guard) -----------------

// patchFor builds a minimal one-file patch whose hunk body is the given raw
// lines, each already carrying its unified-diff marker (+, -, space).
func patchFor(file string, lines ...string) string {
	var b strings.Builder
	b.WriteString("diff --git a/" + file + " b/" + file + "\n")
	b.WriteString("--- a/" + file + "\n")
	b.WriteString("+++ b/" + file + "\n")
	b.WriteString("@@ -1,2 +1,2 @@\n")
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String()
}

// scanFindings posts a raw diff body to /secrets/scan and decodes the
// findings, failing on any non-200.
func scanFindings(t *testing.T, h http.Handler, token, diff string) []scanFinding {
	t.Helper()
	rr := doJSON(t, h, "POST", "/agent/v1/secrets/scan", token, diff)
	if rr.Code != http.StatusOK {
		t.Fatalf("scan status = %d, body %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Findings []scanFinding `json:"findings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode scan response: %v (%s)", err, rr.Body.String())
	}
	return resp.Findings
}

// TestSecretScanExactHit pins the core loop: a secret value in an added line
// is one finding naming secret + file + form — and the response body never
// carries the value itself.
func TestSecretScanExactHit(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "", "sup3r-s3cr3t-value")

	rr := doJSON(t, f.server().Handler(), "POST", "/agent/v1/secrets/scan", token,
		patchFor("cfg/app.env", "+API_KEY=sup3r-s3cr3t-value"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "sup3r-s3cr3t-value") {
		t.Fatalf("scan response leaked the value: %s", body)
	}
	var resp struct {
		Findings []scanFinding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	want := scanFinding{Secret: "API_KEY", File: "cfg/app.env", Form: "exact"}
	if len(resp.Findings) != 1 || resp.Findings[0] != want {
		t.Fatalf("findings = %+v, want [%+v]", resp.Findings, want)
	}
}

// TestSecretScanBase64Hit pins the encoded-form path: an added line carrying
// only the base64 of the value is a finding with form "base64".
func TestSecretScanBase64Hit(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "", "sup3r-s3cr3t-value")

	encoded := base64.StdEncoding.EncodeToString([]byte("sup3r-s3cr3t-value"))
	got := scanFindings(t, f.server().Handler(), token,
		patchFor("cfg/app.env", "+token: "+encoded))
	want := scanFinding{Secret: "API_KEY", File: "cfg/app.env", Form: "base64"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("findings = %+v, want [%+v]", got, want)
	}
}

// TestSecretScanCleanDiff pins the all-clear answer: 200 with a findings
// array that is [] on the wire — never null (the guard's client indexes it).
func TestSecretScanCleanDiff(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "", "sup3r-s3cr3t-value")

	rr := doJSON(t, f.server().Handler(), "POST", "/agent/v1/secrets/scan", token,
		patchFor("main.go", "+func main() {}", " unchanged context"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"findings":[]`) {
		t.Fatalf("body = %s, want a non-null empty findings array", rr.Body.String())
	}
}

// TestSecretScanZeroSecrets pins the cheap pass: a repo without secrets
// answers empty findings even for a non-trivial diff (no decrypt, no match).
func TestSecretScanZeroSecrets(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)

	diff := patchFor("a.txt", "+some added line", "+another added line") +
		patchFor("b.txt", "+more content here")
	rr := doJSON(t, f.server().Handler(), "POST", "/agent/v1/secrets/scan", token, diff)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"findings":[]`) {
		t.Fatalf("body = %s, want a non-null empty findings array", rr.Body.String())
	}
}

// TestSecretScanAddedLinesOnly pins the policy: the value in a removed line
// or a context line is NOT a finding — blocking a push that removes an
// already-leaked value would strand the fix behind the fail-closed guard.
func TestSecretScanAddedLinesOnly(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "", "sup3r-s3cr3t-value")

	got := scanFindings(t, f.server().Handler(), token, patchFor("cfg/app.env",
		"-API_KEY=sup3r-s3cr3t-value",
		" context: sup3r-s3cr3t-value",
		"+API_KEY=redacted"))
	if len(got) != 0 {
		t.Fatalf("findings = %+v, want none for removed/context occurrences", got)
	}
}

// TestSecretScanMultiLineValue pins the '\n' feeding: a value spanning three
// lines reconstructs across three ADJACENT added lines, but not when an
// unfed context line sits in between (no false concatenation).
func TestSecretScanMultiLineValue(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "PEM_KEY", "", "PEMLINE1\nPEMLINE2\nPEMLINE3")
	handler := f.server().Handler()

	got := scanFindings(t, handler, token,
		patchFor("key.pem", "+PEMLINE1", "+PEMLINE2", "+PEMLINE3"))
	want := scanFinding{Secret: "PEM_KEY", File: "key.pem", Form: "exact"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("adjacent added lines: findings = %+v, want [%+v]", got, want)
	}

	// A context line between the added lines is not fed, so the value must
	// NOT reconstruct from non-adjacent additions.
	got = scanFindings(t, handler, token,
		patchFor("key.pem", "+PEMLINE1", " PEMLINE2", "+PEMLINE3"))
	if len(got) != 0 {
		t.Fatalf("interleaved context: findings = %+v, want none", got)
	}
}

// TestSecretScanDedupeAndSort pins the finding set semantics: the same value
// in two files is two findings sorted by file; twice in one file is one.
func TestSecretScanDedupeAndSort(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "", "sup3r-s3cr3t-value")
	handler := f.server().Handler()

	// Patch order z-then-a; findings must come back sorted by file.
	diff := patchFor("z/first.env", "+k=sup3r-s3cr3t-value") +
		patchFor("a/second.env", "+k=sup3r-s3cr3t-value")
	got := scanFindings(t, handler, token, diff)
	if len(got) != 2 || got[0].File != "a/second.env" || got[1].File != "z/first.env" {
		t.Fatalf("two files: findings = %+v, want a/second.env then z/first.env", got)
	}

	got = scanFindings(t, handler, token, patchFor("one.env",
		"+first=sup3r-s3cr3t-value",
		"+second=sup3r-s3cr3t-value"))
	if len(got) != 1 {
		t.Fatalf("same file twice: findings = %+v, want exactly one", got)
	}
}

// TestSecretScanLongLine pins the ReadLine/isPrefix path: an added line far
// beyond bufio's buffer (a minified-JS shape) with the value near its end is
// still found.
func TestSecretScanLongLine(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "", "sup3r-s3cr3t-value")

	long := "+" + strings.Repeat("x", 100<<10) + "sup3r-s3cr3t-value;"
	got := scanFindings(t, f.server().Handler(), token, patchFor("bundle.min.js", long))
	want := scanFinding{Secret: "API_KEY", File: "bundle.min.js", Form: "exact"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("findings = %+v, want [%+v]", got, want)
	}
}

// TestSecretScanCrossRepoScope pins the run-token scope: repo B's value in a
// diff scanned under repo A's token is invisible — A's matcher is built from
// A's secrets only.
func TestSecretScanCrossRepoScope(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRepo(t, "repo_b")
	f.seedRun(t, "run_a", "repo_a", "active")
	tokenA := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "A_KEY", "", "a-plaintext-value")
	f.seedSecret(t, "repo_b", "B_KEY", "", "b-plaintext-value")

	got := scanFindings(t, f.server().Handler(), tokenA,
		patchFor("cfg.env", "+leak=b-plaintext-value"))
	if len(got) != 0 {
		t.Fatalf("findings = %+v, want none for another repo's value", got)
	}
}

// TestSecretScanFileAttribution pins per-file attribution: with two files in
// one patch and the value only in the second, the finding names the second.
func TestSecretScanFileAttribution(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "", "sup3r-s3cr3t-value")

	diff := patchFor("clean/readme.md", "+nothing to see") +
		patchFor("dirty/config.yaml", "+key: sup3r-s3cr3t-value")
	got := scanFindings(t, f.server().Handler(), token, diff)
	want := scanFinding{Secret: "API_KEY", File: "dirty/config.yaml", Form: "exact"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("findings = %+v, want [%+v]", got, want)
	}
}

// TestSecretScanPlusPlusContentLine pins the header/content disambiguation:
// an ADDED line whose content starts with "++ " renders as "+++ ..." in the
// patch. It must be scanned as content — a leak on it found and attributed to
// the current file — not skipped as a `+++ ` file header.
func TestSecretScanPlusPlusContentLine(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedRun(t, "run_a", "repo_a", "active")
	token := f.seedToken(t, "run_a", nil)
	f.seedSecret(t, "repo_a", "API_KEY", "", "sup3r-s3cr3t-value")

	got := scanFindings(t, f.server().Handler(), token,
		patchFor("notes.md", "+++ token sup3r-s3cr3t-value trailing"))
	want := scanFinding{Secret: "API_KEY", File: "notes.md", Form: "exact"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("findings = %+v, want [%+v]", got, want)
	}
}
