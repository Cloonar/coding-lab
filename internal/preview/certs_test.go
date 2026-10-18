package preview

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testClock is an adjustable clock for CertOptions.Now, safe to move while
// Run reads it.
type testClock struct{ t atomic.Pointer[time.Time] }

func newTestClock(t time.Time) *testClock {
	c := &testClock{}
	c.set(t)
	return c
}

func (c *testClock) set(t time.Time)         { c.t.Store(&t) }
func (c *testClock) now() time.Time          { return *c.t.Load() }
func (c *testClock) advance(d time.Duration) { c.set(c.now().Add(d)) }

// quietLogger discards everything, for tests that do not assert on logs.
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// readFile reads path or fails the test.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	return b
}

// must fails the test on a non-nil err.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// newLabCA builds a lab-CA manager on dir for domain at the clock's time.
func newLabCA(t *testing.T, dir, domain string, clock *testClock) *CertManager {
	t.Helper()
	m, err := NewCertManager(CertOptions{Domain: domain, Dir: dir, Now: clock.now, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("NewCertManager: %v", err)
	}
	return m
}

// served returns the leaf the manager currently hands to a handshake.
func served(t *testing.T, m *CertManager, sni string) *x509.Certificate {
	t.Helper()
	c, err := m.GetCertificate(&tls.ClientHelloInfo{ServerName: sni})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatalf("parse served leaf: %v", err)
	}
	return leaf
}

// caOf parses the manager's CA.
func caOf(t *testing.T, m *CertManager) *x509.Certificate {
	t.Helper()
	pemBytes, ok := m.CACertPEM()
	if !ok {
		t.Fatalf("CACertPEM: no CA for source %s", m.Status().Source)
	}
	ca, err := parseCertPEM(pemBytes)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	return ca
}

// snapshot reads the four lab-CA files.
func snapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, f := range []string{caCertFile, caKeyFile, leafCertFile, leafKeyFile} {
		out[f] = readFile(t, filepath.Join(dir, f))
	}
	return out
}

func TestLabCAMint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "preview") // missing: created
	start := time.Now()
	clock := newTestClock(start)
	m := newLabCA(t, dir, "lab.internal", clock)

	st := m.Status()
	if st.Source != SourceLabCA || st.LastError != "" {
		t.Errorf("Status = %+v, want lab-ca with no error", st)
	}
	if want := []string{"*.lab.internal", "lab.internal"}; !slices.Equal(st.DNSNames, want) {
		t.Errorf("Status.DNSNames = %v, want %v", st.DNSNames, want)
	}

	// Permissions: dir 0700, keys 0600.
	fi, err := os.Stat(dir)
	must(t, err)
	if mode := fi.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode = %o, want 700", mode)
	}
	for _, f := range []string{caKeyFile, leafKeyFile} {
		fi, err := os.Stat(filepath.Join(dir, f))
		must(t, err)
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s mode = %o, want 600", f, mode)
		}
		if blk, _ := pem.Decode(readFile(t, filepath.Join(dir, f))); blk == nil || blk.Type != "PRIVATE KEY" {
			t.Errorf("%s is not a PKCS#8 PRIVATE KEY PEM block", f)
		}
	}
	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	must(t, err)
	if len(entries) != 4 {
		t.Errorf("dir holds %d entries, want exactly the four files", len(entries))
	}

	ca := caOf(t, m)
	if !ca.IsCA || !ca.BasicConstraintsValid || ca.MaxPathLen != 0 || !ca.MaxPathLenZero {
		t.Errorf("CA constraints: IsCA=%v valid=%v maxPathLen=%d zero=%v", ca.IsCA, ca.BasicConstraintsValid, ca.MaxPathLen, ca.MaxPathLenZero)
	}
	if ca.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign {
		t.Errorf("CA key usage = %v", ca.KeyUsage)
	}
	if len(ca.SubjectKeyId) == 0 {
		t.Errorf("CA has no SubjectKeyId")
	}
	if !strings.HasPrefix(ca.Subject.CommonName, "lab preview CA (") {
		t.Errorf("CA CN = %q", ca.Subject.CommonName)
	}
	if d := ca.NotAfter.Sub(ca.NotBefore); d > 10*365*24*time.Hour+2*time.Hour {
		t.Errorf("CA validity = %v, want ≤ 10 years", d)
	}
	if _, ok := ca.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Errorf("CA key is %T, want ECDSA", ca.PublicKey)
	}

	leaf := served(t, m, "web.lab.internal")
	if want := []string{"*.lab.internal", "lab.internal"}; !slices.Equal(leaf.DNSNames, want) {
		t.Errorf("leaf SANs = %v, want %v", leaf.DNSNames, want)
	}
	if leaf.Subject.CommonName != "*.lab.internal" {
		t.Errorf("leaf CN = %q", leaf.Subject.CommonName)
	}
	if d := leaf.NotAfter.Sub(leaf.NotBefore); d > 365*24*time.Hour+2*time.Hour {
		t.Errorf("leaf validity = %v, want ≤ 1 year", d)
	}
	if leaf.NotAfter.Sub(start) > 398*24*time.Hour {
		t.Errorf("leaf NotAfter %v is over 398 days out", leaf.NotAfter)
	}
	if leaf.IsCA {
		t.Errorf("leaf is a CA")
	}
	if leaf.KeyUsage != x509.KeyUsageDigitalSignature || !slices.Equal(leaf.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
		t.Errorf("leaf usage = %v / %v", leaf.KeyUsage, leaf.ExtKeyUsage)
	}
	if !bytes.Equal(leaf.AuthorityKeyId, ca.SubjectKeyId) {
		t.Errorf("leaf AuthorityKeyId does not name the CA")
	}
	if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Errorf("leaf key is %T, want ECDSA", leaf.PublicKey)
	}
	if !st.NotAfter.Equal(leaf.NotAfter) {
		t.Errorf("Status.NotAfter = %v, want the leaf's %v", st.NotAfter, leaf.NotAfter)
	}

	// The leaf verifies against the CA alone, for a preview host and the
	// bare domain, but not for a host outside it.
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for _, name := range []string{"web.lab.internal", "lab.internal"} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: pool}); err != nil {
			t.Errorf("Verify(%s): %v", name, err)
		}
	}
	for _, name := range []string{"a.web.lab.internal", "web.example.com"} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: pool}); err == nil {
			t.Errorf("Verify(%s) succeeded, want a name mismatch", name)
		}
	}

	// No SNI gets the same pair.
	if got := served(t, m, ""); !bytes.Equal(got.Raw, leaf.Raw) {
		t.Errorf("empty SNI served a different certificate")
	}
}

func TestLabCATightensExistingDir(t *testing.T) {
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0o755))
	newLabCA(t, dir, "lab.internal", newTestClock(time.Now()))
	fi, err := os.Stat(dir)
	must(t, err)
	if mode := fi.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode = %o, want 700", mode)
	}
}

func TestLabCARestartReusesFiles(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock(time.Now())
	m1 := newLabCA(t, dir, "lab.internal", clock)
	before := snapshot(t, dir)
	stats := map[string]time.Time{}
	for f := range before {
		fi, err := os.Stat(filepath.Join(dir, f))
		must(t, err)
		stats[f] = fi.ModTime()
	}

	clock.advance(48 * time.Hour)
	m2 := newLabCA(t, dir, "lab.internal", clock)
	for f, b := range snapshot(t, dir) {
		if !bytes.Equal(b, before[f]) {
			t.Errorf("%s rewritten on restart", f)
		}
		fi, err := os.Stat(filepath.Join(dir, f))
		must(t, err)
		if !fi.ModTime().Equal(stats[f]) {
			t.Errorf("%s touched on restart", f)
		}
	}
	if !bytes.Equal(served(t, m1, "web.lab.internal").Raw, served(t, m2, "web.lab.internal").Raw) {
		t.Errorf("restart serves a different leaf")
	}
}

func TestLabCADomainChangeRemintsOnlyLeaf(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock(time.Now())
	newLabCA(t, dir, "lab.internal", clock)
	before := snapshot(t, dir)

	m := newLabCA(t, dir, "previews.example.org", clock)
	after := snapshot(t, dir)
	for _, f := range []string{caCertFile, caKeyFile} {
		if !bytes.Equal(after[f], before[f]) {
			t.Errorf("%s changed on a domain change; the CA must be reused", f)
		}
	}
	for _, f := range []string{leafCertFile, leafKeyFile} {
		if bytes.Equal(after[f], before[f]) {
			t.Errorf("%s unchanged on a domain change; the leaf must be re-minted", f)
		}
	}
	leaf := served(t, m, "web.previews.example.org")
	if want := []string{"*.previews.example.org", "previews.example.org"}; !slices.Equal(leaf.DNSNames, want) {
		t.Errorf("re-minted SANs = %v, want %v", leaf.DNSNames, want)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t, m))
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "web.previews.example.org", Roots: pool, CurrentTime: clock.now()}); err != nil {
		t.Errorf("re-minted leaf does not verify under the reused CA: %v", err)
	}
}

func TestLabCAExpiringLeafRemintedAtStartup(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock(time.Now())
	m1 := newLabCA(t, dir, "lab.internal", clock)
	before := snapshot(t, dir)
	oldLeaf := served(t, m1, "web.lab.internal")

	// 31 days left: kept.
	clock.set(oldLeaf.NotAfter.Add(-RenewBefore - 24*time.Hour))
	newLabCA(t, dir, "lab.internal", clock)
	if !bytes.Equal(snapshot(t, dir)[leafCertFile], before[leafCertFile]) {
		t.Fatalf("leaf with 31 days left was re-minted")
	}

	// 29 days left: re-minted, CA reused.
	clock.set(oldLeaf.NotAfter.Add(-RenewBefore + 24*time.Hour))
	m2 := newLabCA(t, dir, "lab.internal", clock)
	after := snapshot(t, dir)
	if bytes.Equal(after[leafCertFile], before[leafCertFile]) {
		t.Fatalf("leaf with 29 days left was not re-minted")
	}
	if !bytes.Equal(after[caCertFile], before[caCertFile]) || !bytes.Equal(after[caKeyFile], before[caKeyFile]) {
		t.Errorf("CA changed on a leaf renewal")
	}
	newLeaf := served(t, m2, "web.lab.internal")
	if !newLeaf.NotAfter.After(oldLeaf.NotAfter) {
		t.Errorf("renewed leaf NotAfter %v not after the old %v", newLeaf.NotAfter, oldLeaf.NotAfter)
	}
}

func TestLabCARunRenews(t *testing.T) {
	dir := t.TempDir()
	clock := newTestClock(time.Now())
	m, err := NewCertManager(CertOptions{
		Domain: "lab.internal", Dir: dir, Now: clock.now, Logger: quietLogger(),
		RenewInterval: 5 * time.Millisecond,
	})
	must(t, err)
	oldLeaf := served(t, m, "web.lab.internal")
	oldFile := readFile(t, filepath.Join(dir, leafCertFile))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// Several ticks with plenty of validity left change nothing.
	time.Sleep(30 * time.Millisecond)
	if !bytes.Equal(served(t, m, "web.lab.internal").Raw, oldLeaf.Raw) {
		t.Fatalf("Run renewed a leaf with a year left")
	}

	clock.set(oldLeaf.NotAfter.Add(-RenewBefore + time.Hour))
	waitFor(t, "the renewal tick", func() bool {
		return !bytes.Equal(served(t, m, "web.lab.internal").Raw, oldLeaf.Raw)
	})
	newLeaf := served(t, m, "web.lab.internal")
	if bytes.Equal(readFile(t, filepath.Join(dir, leafCertFile)), oldFile) {
		t.Errorf("the renewed leaf was not persisted")
	}
	if st := m.Status(); !st.NotAfter.Equal(newLeaf.NotAfter) || st.LastError != "" {
		t.Errorf("Status after renewal = %+v", st)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caOf(t, m))
	if _, err := newLeaf.Verify(x509.VerifyOptions{DNSName: "web.lab.internal", Roots: pool, CurrentTime: clock.now()}); err != nil {
		t.Errorf("renewed leaf does not verify: %v", err)
	}
}

func TestLabCARunRenewalFailureKeepsOldPair(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	clock := newTestClock(time.Now())
	m, err := NewCertManager(CertOptions{
		Domain: "lab.internal", Dir: dir, Now: clock.now, Logger: quietLogger(),
		RenewInterval: 5 * time.Millisecond,
	})
	must(t, err)
	oldLeaf := served(t, m, "web.lab.internal")
	// An unwritable directory makes the persist step fail.
	must(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	clock.set(oldLeaf.NotAfter.Add(-RenewBefore + time.Hour))
	waitFor(t, "a failed renewal", func() bool { return m.Status().LastError != "" })
	if !bytes.Equal(served(t, m, "web.lab.internal").Raw, oldLeaf.Raw) {
		t.Errorf("a failed renewal replaced the served leaf")
	}

	// Writable again: the next tick renews and clears the error.
	must(t, os.Chmod(dir, 0o700))
	waitFor(t, "the retried renewal", func() bool { return m.Status().LastError == "" })
	if bytes.Equal(served(t, m, "web.lab.internal").Raw, oldLeaf.Raw) {
		t.Errorf("the retried renewal did not swap the leaf")
	}
}

func TestLabCALeafProblemsRemintOnlyLeaf(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(t *testing.T, dir string)
	}{
		{"missing", func(t *testing.T, dir string) { must(t, os.Remove(filepath.Join(dir, leafCertFile))) }},
		{"unparsable", func(t *testing.T, dir string) {
			must(t, os.WriteFile(filepath.Join(dir, leafCertFile), []byte("garbage"), 0o644))
		}},
		{"key mismatch", func(t *testing.T, dir string) {
			_, keyPEM := selfSigned(t, "x", false)
			must(t, os.WriteFile(filepath.Join(dir, leafKeyFile), keyPEM, 0o600))
		}},
		{"foreign signer", func(t *testing.T, dir string) {
			certPEM, keyPEM := selfSigned(t, "*.lab.internal", false, "*.lab.internal", "lab.internal")
			must(t, os.WriteFile(filepath.Join(dir, leafCertFile), certPEM, 0o644))
			must(t, os.WriteFile(filepath.Join(dir, leafKeyFile), keyPEM, 0o600))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			clock := newTestClock(time.Now())
			newLabCA(t, dir, "lab.internal", clock)
			before := snapshot(t, dir)
			tc.corrupt(t, dir)

			m := newLabCA(t, dir, "lab.internal", clock)
			after := snapshot(t, dir)
			if !bytes.Equal(after[caCertFile], before[caCertFile]) || !bytes.Equal(after[caKeyFile], before[caKeyFile]) {
				t.Errorf("CA changed")
			}
			pool := x509.NewCertPool()
			pool.AddCert(caOf(t, m))
			if _, err := served(t, m, "web.lab.internal").Verify(x509.VerifyOptions{DNSName: "web.lab.internal", Roots: pool}); err != nil {
				t.Errorf("re-minted leaf does not verify: %v", err)
			}
			if _, err := tls.X509KeyPair(after[leafCertFile], after[leafKeyFile]); err != nil {
				t.Errorf("persisted leaf pair unusable: %v", err)
			}
		})
	}
}

func TestLabCABadCARefusesStartup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(t *testing.T, dir string)
		want    error
	}{
		{"corrupt certificate", func(t *testing.T, dir string) {
			must(t, os.WriteFile(filepath.Join(dir, caCertFile), []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), 0o644))
		}, ErrCAInvalid},
		{"corrupt key", func(t *testing.T, dir string) {
			must(t, os.WriteFile(filepath.Join(dir, caKeyFile), []byte("not a key"), 0o600))
		}, ErrCAInvalid},
		{"mismatched key", func(t *testing.T, dir string) {
			_, keyPEM := selfSigned(t, "x", true)
			must(t, os.WriteFile(filepath.Join(dir, caKeyFile), keyPEM, 0o600))
		}, ErrCAInvalid},
		{"not a CA", func(t *testing.T, dir string) {
			certPEM, keyPEM := selfSigned(t, "x", false)
			must(t, os.WriteFile(filepath.Join(dir, caCertFile), certPEM, 0o644))
			must(t, os.WriteFile(filepath.Join(dir, caKeyFile), keyPEM, 0o600))
		}, ErrCAInvalid},
		{"only the key", func(t *testing.T, dir string) { must(t, os.Remove(filepath.Join(dir, caCertFile))) }, ErrCAIncomplete},
		{"only the certificate", func(t *testing.T, dir string) { must(t, os.Remove(filepath.Join(dir, caKeyFile))) }, ErrCAIncomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			clock := newTestClock(time.Now())
			newLabCA(t, dir, "lab.internal", clock)
			tc.corrupt(t, dir)
			left := map[string][]byte{}
			for _, f := range []string{caCertFile, caKeyFile} {
				if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
					left[f] = b
				}
			}

			_, err := NewCertManager(CertOptions{Domain: "lab.internal", Dir: dir, Now: clock.now, Logger: quietLogger()})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			// Nothing was minted over the broken trust anchor.
			for f, b := range left {
				if got, _ := os.ReadFile(filepath.Join(dir, f)); !bytes.Equal(got, b) {
					t.Errorf("%s was replaced", f)
				}
			}
		})
	}
}

func TestCertOptionsPairing(t *testing.T) {
	for _, o := range []CertOptions{
		{Domain: "lab.internal", CertFile: "/x/cert.pem"},
		{Domain: "lab.internal", KeyFile: "/x/key.pem"},
	} {
		_, err := NewCertManager(o)
		if !errors.Is(err, ErrCertPairIncomplete) {
			t.Errorf("%+v: err = %v, want ErrCertPairIncomplete", o, err)
			continue
		}
		for _, flag := range []string{"--preview-cert-file", "--preview-key-file"} {
			if !strings.Contains(err.Error(), flag) {
				t.Errorf("error %q does not name %s", err, flag)
			}
		}
	}
	if _, err := NewCertManager(CertOptions{Domain: "lab.internal"}); err == nil {
		t.Errorf("lab-CA source without a Dir: no error")
	}
	if _, err := NewCertManager(CertOptions{Dir: t.TempDir()}); err == nil {
		t.Errorf("no domain: no error")
	}
}

// selfSigned makes a throwaway ECDSA certificate and key (PEM), a CA or not.
func selfSigned(t *testing.T, cn string, isCA bool, dnsNames ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		DNSNames:              dnsNames,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(90 * 24 * time.Hour),
		IsCA:                  isCA,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(t, err)
	certPEM, keyPEM, err = encodePair(der, key)
	must(t, err)
	return certPEM, keyPEM
}

// writeOperatorPair writes a fresh self-signed pair for *.lab.internal into
// certPath/keyPath and returns the certificate PEM.
func writeOperatorPair(t *testing.T, certPath, keyPath string) []byte {
	t.Helper()
	certPEM, keyPEM := selfSigned(t, "*.lab.internal", false, "*.lab.internal", "lab.internal")
	// Each file replaced atomically, as ACME clients do, so a reload tick
	// sees either file whole — though possibly one new beside one old.
	must(t, writeFileAtomic(certPath, certPEM, 0o644))
	must(t, writeFileAtomic(keyPath, keyPEM, 0o600))
	return certPEM
}

// servedPEMIs reports whether m currently serves the certificate in certPEM.
func servedPEMIs(t *testing.T, m *CertManager, certPEM []byte) bool {
	t.Helper()
	want, err := parseCertPEM(certPEM)
	must(t, err)
	return bytes.Equal(served(t, m, "web.lab.internal").Raw, want.Raw)
}

// capturingHandler is a slog.Handler that keeps every record's message and
// level, for the tests that assert a warning was logged.
type capturingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r)
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) warnings() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.recs {
		if r.Level == slog.LevelWarn {
			n++
		}
	}
	return n
}

func TestOperatorSourceServesAndReloads(t *testing.T) {
	tmp := t.TempDir()
	certPath, keyPath := filepath.Join(tmp, "cert.pem"), filepath.Join(tmp, "key.pem")
	first := writeOperatorPair(t, certPath, keyPath)
	logs := &capturingHandler{}
	stateDir := filepath.Join(tmp, "state-preview")

	m, err := NewCertManager(CertOptions{
		Domain: "lab.internal", Dir: stateDir, CertFile: certPath, KeyFile: keyPath,
		Logger: slog.New(logs), ReloadInterval: 5 * time.Millisecond,
	})
	must(t, err)
	if !servedPEMIs(t, m, first) {
		t.Fatalf("operator pair not served")
	}
	st := m.Status()
	if st.Source != SourceOperator || !slices.Equal(st.DNSNames, []string{"*.lab.internal", "lab.internal"}) || st.NotAfter.IsZero() {
		t.Errorf("Status = %+v", st)
	}
	if _, ok := m.CACertPEM(); ok {
		t.Errorf("CACertPEM reported a CA for the operator source")
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the operator source touched Dir (stat err %v)", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// A rewrite is picked up without a restart.
	second := writeOperatorPair(t, certPath, keyPath)
	waitFor(t, "the rewritten pair", func() bool { return servedPEMIs(t, m, second) })

	// Half a renewal: a new certificate beside the old key. The previous
	// pair keeps serving and the failure is reported, once.
	warnsBefore := logs.warnings()
	half, _ := selfSigned(t, "*.lab.internal", false, "*.lab.internal", "lab.internal")
	must(t, writeFileAtomic(certPath, half, 0o644))
	waitFor(t, "the failed reload", func() bool { return m.Status().LastError != "" })
	time.Sleep(30 * time.Millisecond) // several more failing ticks
	if !servedPEMIs(t, m, second) {
		t.Errorf("a mismatched pair replaced the served certificate")
	}
	if n := logs.warnings() - warnsBefore; n != 1 {
		t.Errorf("warnings for one broken pair = %d, want 1", n)
	}

	// Garbage instead of a key: still the old pair.
	must(t, writeFileAtomic(keyPath, []byte("garbage"), 0o600))
	time.Sleep(30 * time.Millisecond)
	if !servedPEMIs(t, m, second) || m.Status().LastError == "" {
		t.Errorf("a garbage key replaced the pair or cleared the error")
	}

	// The renewal completes: the next good write is picked up, error cleared.
	third := writeOperatorPair(t, certPath, keyPath)
	waitFor(t, "the repaired pair", func() bool { return servedPEMIs(t, m, third) })
	if st := m.Status(); st.LastError != "" {
		t.Errorf("LastError after a good reload = %q", st.LastError)
	}
}

func TestOperatorSourceStartupErrorNamesFiles(t *testing.T) {
	tmp := t.TempDir()
	certPath, keyPath := filepath.Join(tmp, "cert.pem"), filepath.Join(tmp, "key.pem")
	for _, setup := range []func(){
		func() {}, // both missing
		func() { // mismatched pair
			writeOperatorPair(t, certPath, keyPath)
			_, other := selfSigned(t, "x", false)
			must(t, os.WriteFile(keyPath, other, 0o600))
		},
	} {
		setup()
		_, err := NewCertManager(CertOptions{Domain: "lab.internal", CertFile: certPath, KeyFile: keyPath, Logger: quietLogger()})
		if err == nil {
			t.Fatalf("bad operator pair: no error")
		}
		if !strings.Contains(err.Error(), certPath) || !strings.Contains(err.Error(), keyPath) {
			t.Errorf("error %q does not name both files", err)
		}
	}
}

func TestTLSConfig(t *testing.T) {
	t.Setenv("GODEBUG", "") // whatever the environment says, start from Go's default
	m := newLabCA(t, t.TempDir(), "lab.internal", newTestClock(time.Now()))
	cfg := m.TLSConfig()
	if cfg.MinVersion != tls.VersionTLS12 || cfg.GetCertificate == nil {
		t.Errorf("TLSConfig = MinVersion %x, GetCertificate set %v", cfg.MinVersion, cfg.GetCertificate != nil)
	}
	if !slices.Equal(cfg.NextProtos, []string{"h2", "http/1.1"}) {
		t.Errorf("NextProtos = %v, want h2 then http/1.1", cfg.NextProtos)
	}

	// With Go's extended-CONNECT switch on, HTTP/2 is off.
	t.Setenv("GODEBUG", "http2xconnect=1")
	if got := m.TLSConfig().NextProtos; !slices.Equal(got, []string{"http/1.1"}) {
		t.Errorf("NextProtos with http2xconnect=1 = %v, want http/1.1 only", got)
	}
}
