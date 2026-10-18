package httpapi

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/preview"
)

// fakePreviewCerts is a canned certificate manager: what Status reports and
// what CACertPEM hands out (nil ca = the operator source, no lab CA).
type fakePreviewCerts struct {
	status preview.CertStatus
	ca     []byte
}

func (f *fakePreviewCerts) Status() preview.CertStatus { return f.status }

func (f *fakePreviewCerts) CACertPEM() ([]byte, bool) {
	if f.ca == nil {
		return nil, false
	}
	return f.ca, true
}

// previewWiring is what a preview test configures; the zero value is a lab
// without previews. certs is set on Options only when non-nil, so no test can
// smuggle a typed nil in by accident (TestPreviewTypedNilIsOff does it on
// purpose).
type previewWiring struct {
	certs    PreviewCerts
	domain   string
	port     int
	redirect bool
}

func newPreviewServer(t *testing.T, wiring previewWiring) *testServer {
	t.Helper()
	x := newTestServer(t, func(o *Options) {
		if wiring.certs != nil {
			o.PreviewCerts = wiring.certs
		}
		o.PreviewDomain = wiring.domain
		o.PreviewPort = wiring.port
		o.PreviewRedirect = wiring.redirect
	})
	x.setup("op", "password123")
	return x
}

// previewStatus fetches GET /api/v1/preview as the authenticated operator,
// asserting the pinned 200 (the state is the payload), and returns the
// decoded and the raw body.
func (x *testServer) previewStatus() (map[string]any, string) {
	x.t.Helper()
	resp := x.do("GET", "/api/v1/preview", nil, nil)
	wantStatus(x.t, resp, http.StatusOK)
	raw := rawBody(x.t, resp)
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		x.t.Fatalf("decode preview body %q: %v", raw, err)
	}
	return body, raw
}

// labCAManager is a real lab-CA certificate manager over a temp state dir, so
// the lab-ca cases exercise the production type end to end (its CA PEM, its
// leaf's names and expiry) rather than a fake's idea of them.
func labCAManager(t *testing.T) *preview.CertManager {
	t.Helper()
	m, err := preview.NewCertManager(preview.CertOptions{Domain: "lab.internal", Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewCertManager: %v", err)
	}
	return m
}

// TestPreviewOff pins a lab without --preview-addr: the status route is
// mounted (never a 404) and answers exactly {"enabled": false} — no domain,
// port or cert echoed even when the Options carry them — and the CA download
// is the canonical 404 envelope.
func TestPreviewOff(t *testing.T) {
	x := newPreviewServer(t, previewWiring{domain: "lab.internal", port: 443, redirect: true})

	_, raw := x.previewStatus()
	if strings.TrimSpace(raw) != `{"enabled":false}` {
		t.Fatalf("off body = %s, want exactly {\"enabled\":false}", raw)
	}

	resp := x.do("GET", "/api/v1/preview/ca", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	if body := decodeBody(t, resp); body["error"] == "" || body["error"] == nil {
		t.Fatalf("off /ca body = %v, want the error envelope", body)
	}
}

// TestPreviewTypedNilIsOff: a nil *preview.CertManager in the interface field
// is folded back to nil by New, so it reads "off" instead of panicking.
func TestPreviewTypedNilIsOff(t *testing.T) {
	x := newTestServer(t, func(o *Options) {
		var m *preview.CertManager
		o.PreviewCerts = m
		o.PreviewDomain = "lab.internal"
	})
	x.setup("op", "password123")
	body, raw := x.previewStatus()
	if body["enabled"] != false {
		t.Fatalf("typed-nil body = %s, want enabled false", raw)
	}
}

// TestPreviewLabCA is the default certificate source: every field the
// Settings card reads, the CA downloadable, the leaf's real names and expiry.
func TestPreviewLabCA(t *testing.T) {
	m := labCAManager(t)
	x := newPreviewServer(t, previewWiring{certs: m, domain: "lab.internal", port: 8443, redirect: true})

	body, raw := x.previewStatus()
	if body["enabled"] != true || body["domain"] != "lab.internal" || body["port"] != float64(8443) ||
		body["redirect"] != true || body["ca_download"] != true {
		t.Fatalf("lab-ca body = %s", raw)
	}
	cert, ok := body["cert"].(map[string]any)
	if !ok {
		t.Fatalf("lab-ca body has no cert object: %s", raw)
	}
	if cert["source"] != "lab-ca" {
		t.Errorf("cert.source = %v, want lab-ca", cert["source"])
	}
	if _, ok := cert["error"]; ok {
		t.Errorf("a healthy manager reports an error field: %s", raw)
	}
	names, _ := cert["dns_names"].([]any)
	if len(names) != 2 || names[0] != "*.lab.internal" || names[1] != "lab.internal" {
		t.Errorf("cert.dns_names = %v, want [*.lab.internal lab.internal]", cert["dns_names"])
	}
	notAfter, err := time.Parse(time.RFC3339, cert["not_after"].(string))
	if err != nil {
		t.Fatalf("cert.not_after %v is not RFC 3339: %v", cert["not_after"], err)
	}
	if !strings.HasSuffix(cert["not_after"].(string), "Z") {
		t.Errorf("cert.not_after = %v, want UTC", cert["not_after"])
	}
	if want := m.Status().NotAfter; !notAfter.Equal(want.Truncate(time.Second)) {
		t.Errorf("cert.not_after = %v, want the leaf's %v", notAfter, want)
	}
}

// TestPreviewCADownload pins the download: the manager's CA PEM verbatim, the
// install-friendly content type, an attachment name, nosniff — and a body
// that really is the CA (CA:true) the leaf chains to.
func TestPreviewCADownload(t *testing.T) {
	m := labCAManager(t)
	x := newPreviewServer(t, previewWiring{certs: m, domain: "lab.internal", port: 443})

	resp := x.do("GET", "/api/v1/preview/ca", nil, nil)
	wantStatus(t, resp, http.StatusOK)
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for h, want := range map[string]string{
		"Content-Type":           "application/x-x509-ca-cert",
		"Content-Disposition":    `attachment; filename="lab-preview-ca.crt"`,
		"X-Content-Type-Options": "nosniff",
	} {
		if v := resp.Header.Get(h); v != want {
			t.Errorf("%s = %q, want %q", h, v, want)
		}
	}
	want, _ := m.CACertPEM()
	if string(got) != string(want) {
		t.Fatalf("body is not the manager's CA PEM:\n%s", got)
	}
	blk, _ := pem.Decode(got)
	if blk == nil || blk.Type != "CERTIFICATE" {
		t.Fatalf("body is not a PEM certificate: %q", got)
	}
	ca, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA {
		t.Error("downloaded certificate is not a CA")
	}
}

// TestPreviewOperator is the operator source: reported as such, no CA to
// download (ca_download false, /ca 404), and its own names passed through.
func TestPreviewOperator(t *testing.T) {
	exp := time.Date(2027, 1, 2, 3, 4, 5, 0, time.FixedZone("CET", 3600))
	certs := &fakePreviewCerts{status: preview.CertStatus{
		Source:   preview.SourceOperator,
		NotAfter: exp,
		DNSNames: []string{"*.lab.example.com"},
	}}
	x := newPreviewServer(t, previewWiring{certs: certs, domain: "lab.example.com", port: 443, redirect: false})

	body, raw := x.previewStatus()
	if body["enabled"] != true || body["port"] != float64(443) || body["redirect"] != false || body["ca_download"] != false {
		t.Fatalf("operator body = %s", raw)
	}
	cert := body["cert"].(map[string]any)
	if cert["source"] != "operator" || cert["not_after"] != "2027-01-02T02:04:05Z" {
		t.Errorf("operator cert = %v, want source operator, not_after in UTC", cert)
	}
	if names, _ := cert["dns_names"].([]any); len(names) != 1 || names[0] != "*.lab.example.com" {
		t.Errorf("operator dns_names = %v", cert["dns_names"])
	}

	resp := x.do("GET", "/api/v1/preview/ca", nil, nil)
	wantStatus(t, resp, http.StatusNotFound)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("operator /ca Content-Type = %q, want the JSON error envelope", ct)
	}
	if body := decodeBody(t, resp); body["error"] == nil {
		t.Fatalf("operator /ca body = %v, want the error envelope", body)
	}
}

// TestPreviewCertError: a failing renewal/reload is reported in cert.error
// while the last good pair keeps serving; nil DNS names still render [].
func TestPreviewCertError(t *testing.T) {
	certs := &fakePreviewCerts{status: preview.CertStatus{
		Source:    preview.SourceOperator,
		NotAfter:  time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		LastError: "preview: loading the certificate pair a / b: tls: private key does not match public key",
	}}
	x := newPreviewServer(t, previewWiring{certs: certs, domain: "lab.internal", port: 443})

	body, raw := x.previewStatus()
	cert := body["cert"].(map[string]any)
	if cert["error"] != certs.status.LastError {
		t.Errorf("cert.error = %v, want the manager's last error", cert["error"])
	}
	if !strings.Contains(raw, `"dns_names":[]`) {
		t.Errorf("nil DNS names not rendered as []: %s", raw)
	}
}

// TestPreviewRequiresAuth: both routes are behind requireAuth — the CA in
// particular is never handed to an unauthenticated caller.
func TestPreviewRequiresAuth(t *testing.T) {
	x := newPreviewServer(t, previewWiring{certs: labCAManager(t), domain: "lab.internal", port: 443})
	for _, path := range []string{"/api/v1/preview", "/api/v1/preview/ca"} {
		resp := doWith(t, http.DefaultClient, x.ts.URL, "GET", path, nil, nil)
		wantStatus(t, resp, http.StatusUnauthorized)
		_ = resp.Body.Close()
	}
}
