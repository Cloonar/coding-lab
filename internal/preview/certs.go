package preview

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The preview listener's TLS front door (issue #106). The listener serves
// https://<name>.<preview-domain> with one of two certificate sources, and
// the CertManager below owns whichever is active: loading, minting,
// persistence, renewal and reload, behind a tls.Config whose GetCertificate
// always hands out the current pair — so a renewed or reloaded certificate
// takes effect on the next handshake without rebuilding the http.Server.

// CertSource names where the preview listener's certificate comes from. The
// value is what the health output and the Settings card report.
type CertSource string

const (
	// SourceLabCA: lab's own private CA plus a wildcard leaf it mints and
	// renews underneath it, persisted under <state>/preview. The default
	// when no operator pair is configured.
	SourceLabCA CertSource = "lab-ca"
	// SourceOperator: the PEM pair named by --preview-cert-file and
	// --preview-key-file, re-read whenever either file changes on disk.
	SourceOperator CertSource = "operator"
)

// Lifetimes and thresholds of the lab-CA source.
const (
	// RenewBefore: a leaf with less than this left is re-minted, at startup
	// and by the daily check in Run. Thirty days leaves a month of daily
	// retries if renewal keeps failing, long before a browser sees an
	// expired certificate.
	RenewBefore = 30 * 24 * time.Hour

	// caValidity is the CA's lifetime. The CA is the trust anchor the
	// operator installs once per device (on an iPhone: a profile plus "full
	// trust"), so it is long-lived on purpose; the short-lived leaf below it
	// is what rotates.
	caValidity = 10 * 365 * 24 * time.Hour
	// leafValidity is the wildcard leaf's lifetime: one year. Apple rejects
	// a TLS server certificate valid for more than 825 days even under a
	// user-trusted root, and browsers cap publicly trusted ones at 398 days;
	// a year is comfortably under both by construction, so a leaf lab mints
	// can never be the reason a device refuses a preview.
	leafValidity = 365 * 24 * time.Hour
	// backdate is how far before "now" a minted certificate's NotBefore
	// sits, so a device whose clock runs a little behind the lab host's
	// does not see a not-yet-valid certificate.
	backdate = time.Hour

	// defaultRenewInterval is how often Run re-checks the leaf's expiry.
	defaultRenewInterval = 24 * time.Hour
	// defaultReloadInterval is how often Run polls the operator pair's
	// files. A minute keeps an ACME renewal's pickup prompt while costing
	// two stat calls.
	defaultReloadInterval = time.Minute
)

// Files of the lab-CA source inside CertOptions.Dir.
const (
	caCertFile   = "ca.crt"
	caKeyFile    = "ca.key"
	leafCertFile = "leaf.crt"
	leafKeyFile  = "leaf.key"
)

// File modes of the lab-CA source. Keys are 0600; certificates are 0644
// because they are public material (the CA certificate is exactly what the
// operator downloads onto every device). The directory is 0700, so in
// practice only lab's own user can reach either.
const (
	dirMode  fs.FileMode = 0o700
	keyMode  fs.FileMode = 0o600
	certMode fs.FileMode = 0o644
)

// The certificate manager's error vocabulary. Every one of them is a fatal
// startup error: the preview listener refuses to come up rather than serve
// a certificate nobody asked for.
var (
	// ErrCertPairIncomplete: exactly one of CertFile and KeyFile is set.
	// The pairing rule of the Warpgate url/token flags — half a
	// configuration is a typo, not a request for the default.
	ErrCertPairIncomplete = errors.New("preview: --preview-cert-file and --preview-key-file must be set together")
	// ErrCAIncomplete: exactly one of ca.crt and ca.key exists. Lab will not
	// mint over the survivor: if a device already trusts that CA, replacing
	// it would silently break every preview on that device.
	ErrCAIncomplete = errors.New("preview: lab CA is incomplete (exactly one of ca.crt and ca.key exists)")
	// ErrCAInvalid: ca.crt / ca.key exist but cannot be used — unreadable,
	// unparsable, a key that does not match the certificate, a certificate
	// that is not a CA, or a CA that has expired. Same reasoning as
	// ErrCAIncomplete: a trust anchor is never replaced behind the
	// operator's back; they delete the files to have lab mint a new one.
	ErrCAInvalid = errors.New("preview: lab CA is unusable")
)

// CertOptions configures NewCertManager.
type CertOptions struct {
	// Domain is the preview domain (e.g. "lab.internal"), already validated
	// as a lowercase hostname by the caller. The lab-CA leaf covers exactly
	// "*.<Domain>" and "<Domain>".
	Domain string
	// Dir is where the lab-CA source keeps its files (<state>/preview). It
	// is created 0700 when missing. Unused, and never touched, by the
	// operator source.
	Dir string
	// CertFile and KeyFile name the operator's PEM pair. Both set selects
	// SourceOperator, both empty SourceLabCA, one alone is
	// ErrCertPairIncomplete.
	CertFile, KeyFile string

	// Now is the clock; nil means time.Now. Tests inject one to age a leaf.
	Now func() time.Time
	// Logger receives mint/renew/reload events; nil means slog.Default().
	Logger *slog.Logger
	// RenewInterval is how often Run re-checks the lab-CA leaf (0 = 24h).
	RenewInterval time.Duration
	// ReloadInterval is how often Run polls the operator pair's files
	// (0 = 1 minute).
	ReloadInterval time.Duration
}

// CertStatus is what the health output and the Settings card report about
// the served certificate.
type CertStatus struct {
	Source CertSource
	// NotAfter is the served leaf's expiry.
	NotAfter time.Time
	// DNSNames are the served leaf's SANs.
	DNSNames []string
	// LastError is the last failed renewal or reload, "" once one succeeds
	// (and before any fails). The served pair is always the last good one.
	LastError string
}

// CertManager owns the preview listener's certificate. Build it with
// NewCertManager (which does every startup load and mint), hand
// ConfigureServer the preview http.Server, and run Run for the process
// lifetime so the leaf renews (lab-CA) or the operator pair reloads. Safe
// for concurrent use.
type CertManager struct {
	source         CertSource
	domain         string
	dir            string
	certFile       string
	keyFile        string
	now            func() time.Time
	log            *slog.Logger
	renewInterval  time.Duration
	reloadInterval time.Duration

	// The lab CA, lab-CA source only. Immutable after NewCertManager: the
	// CA is never re-minted while lab runs.
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte

	// cur is the pair every handshake gets. An atomic pointer rather than a
	// lock because GetCertificate runs on every TLS handshake and a swap is
	// a single store.
	cur atomic.Pointer[servedCert]

	mu sync.Mutex
	// lastErr backs CertStatus.LastError.
	lastErr string
	// loaded is the operator pair's file stamp as of the last successful
	// load. A failed reload deliberately leaves it alone so the next tick
	// retries (see reloadOperator).
	loaded pairStamp
}

// servedCert is one certificate as served, with the facts Status reports
// pulled out of its leaf once.
type servedCert struct {
	cert     *tls.Certificate
	leaf     *x509.Certificate
	notAfter time.Time
	dnsNames []string
}

func newServedCert(c *tls.Certificate, leaf *x509.Certificate) *servedCert {
	return &servedCert{cert: c, leaf: leaf, notAfter: leaf.NotAfter, dnsNames: slices.Clone(leaf.DNSNames)}
}

// NewCertManager validates o and loads (or mints) the certificate the
// preview listener will serve. Any error is a fatal startup error.
//
// The lab-CA source, at startup:
//   - ca.crt and ca.key both missing: mint a CA, and therefore a leaf.
//   - exactly one present: ErrCAIncomplete. Present but unusable:
//     ErrCAInvalid. Neither is ever replaced silently — devices may trust it.
//   - the leaf is missing, unparsable, does not match its key, is not
//     signed by the current CA, does not cover exactly {*.<domain>,
//     <domain>} (a changed --preview-domain), is not yet valid, or has
//     under RenewBefore left: re-mint only the leaf, reusing the CA.
//   - otherwise all four files are reused untouched.
//
// The operator source loads the pair with tls.LoadX509KeyPair; a failure
// names both files.
func NewCertManager(o CertOptions) (*CertManager, error) {
	if o.Domain == "" {
		return nil, errors.New("preview: certificate manager needs a preview domain")
	}
	if (o.CertFile == "") != (o.KeyFile == "") {
		return nil, ErrCertPairIncomplete
	}
	m := &CertManager{
		domain:         o.Domain,
		dir:            o.Dir,
		certFile:       o.CertFile,
		keyFile:        o.KeyFile,
		now:            o.Now,
		log:            o.Logger,
		renewInterval:  o.RenewInterval,
		reloadInterval: o.ReloadInterval,
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	m.log = m.log.With("component", "preview")
	if m.renewInterval <= 0 {
		m.renewInterval = defaultRenewInterval
	}
	if m.reloadInterval <= 0 {
		m.reloadInterval = defaultReloadInterval
	}

	if o.CertFile != "" {
		m.source = SourceOperator
		stamp := m.stampPair()
		sc, err := m.loadOperator()
		if err != nil {
			return nil, err
		}
		m.loaded = stamp
		m.cur.Store(sc)
		m.log.Info("preview certificate loaded", "source", m.source, "cert_file", m.certFile,
			"key_file", m.keyFile, "not_after", sc.notAfter)
		m.warnIfNotCovering(sc.leaf)
		return m, nil
	}

	m.source = SourceLabCA
	if o.Dir == "" {
		return nil, errors.New("preview: certificate manager needs a state directory for the lab CA")
	}
	if err := m.startLabCA(); err != nil {
		return nil, err
	}
	return m, nil
}

// GetCertificate is the tls.Config hook: the current pair, whatever the
// ClientHello asks for — including no SNI at all (a client dialling the
// listener by IP), which gets the same certificate rather than a handshake
// failure, so the error it sees is a name mismatch it can explain.
func (m *CertManager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return m.cur.Load().cert, nil
}

// TLSConfig returns a fresh tls.Config for the preview listener: TLS 1.2 at
// least (every browser that matters speaks 1.3; 1.2 keeps older iOS and
// curl builds working), GetCertificate for hot swap, and the ALPN list from
// nextProtos.
//
// Prefer ConfigureServer: http.Server.ServeTLS rewrites NextProtos from the
// server's own Protocols set, so the ALPN decision below only holds when the
// server's Protocols agree with it.
func (m *CertManager) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: m.GetCertificate,
		NextProtos:     nextProtos(),
	}
}

// ConfigureServer points srv at this manager: TLSConfig, plus a Protocols
// set matching its ALPN list, which is what actually decides ALPN once srv
// runs ServeTLS (adjustNextProtos in net/http re-adds "h2" to any NextProtos
// while the server's Protocols include HTTP/2). Call srv.ServeTLS(ln, "", "")
// afterwards — the certificate comes from GetCertificate, not from files.
func (m *CertManager) ConfigureServer(srv *http.Server) {
	srv.TLSConfig = m.TLSConfig()
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetHTTP2(slices.Contains(srv.TLSConfig.NextProtos, "h2"))
	srv.Protocols = &p
}

// nextProtos decides the preview listener's ALPN list.
//
// The concern (issue #106's "assumptions to check"): a preview's WebSocket
// (Vite HMR, Next's dev overlay) reaches the run through
// httputil.ReverseProxy, which can only forward an HTTP/1.1 `Upgrade` — it
// hijacks the inbound connection, and an HTTP/2 stream cannot be hijacked.
// Over HTTP/2 a WebSocket would instead be an RFC 8441 extended CONNECT
// (`:method CONNECT`, `:protocol websocket`), which the proxy does not
// translate. A browser only attempts that when the server's SETTINGS frame
// carries SETTINGS_ENABLE_CONNECT_PROTOCOL=1; without it, Chrome and Firefox
// open a separate HTTP/1.1 connection for the WebSocket (their WebSocket
// handshakes offer only "http/1.1" in ALPN) and the Upgrade path works.
//
// Go 1.26's bundled HTTP/2 server does NOT advertise the setting: extended
// CONNECT is disabled by default (golang/go#71128, precisely because
// browsers would otherwise send WebSockets the handler cannot serve), and a
// request carrying :protocol is reset with PROTOCOL_ERROR. The only switch
// is GODEBUG=http2xconnect=1, read from the environment once at init. So
// HTTP/2 stays on — a Vite dev server serves hundreds of unbundled modules,
// and multiplexing them beats six HTTP/1.1 connections — unless that
// GODEBUG is set, in which case the listener pins ALPN to HTTP/1.1, the one
// protocol whose WebSocket path is known to work. tls_test.go reads the
// server's SETTINGS frame off the wire, so a Go upgrade that flips the
// default fails a test instead of breaking HMR.
func nextProtos() []string {
	if extendedConnectEnabled() {
		return []string{"http/1.1"}
	}
	return []string{"h2", "http/1.1"}
}

// extendedConnectEnabled mirrors net/http's own test for its extended
// CONNECT switch — the same substring check on the same variable.
func extendedConnectEnabled() bool {
	return strings.Contains(os.Getenv("GODEBUG"), "http2xconnect=1")
}

// Status reports the served certificate and the last renewal or reload
// failure.
func (m *CertManager) Status() CertStatus {
	sc := m.cur.Load()
	m.mu.Lock()
	defer m.mu.Unlock()
	return CertStatus{
		Source:    m.source,
		NotAfter:  sc.notAfter,
		DNSNames:  slices.Clone(sc.dnsNames),
		LastError: m.lastErr,
	}
}

// CACertPEM returns lab's CA certificate as PEM, for the authenticated CA
// download; false for the operator source, which has no lab CA to trust.
func (m *CertManager) CACertPEM() ([]byte, bool) {
	if m.source != SourceLabCA {
		return nil, false
	}
	return bytes.Clone(m.caPEM), true
}

// Run blocks until ctx is done, keeping the served certificate current: the
// lab-CA source re-checks the leaf every RenewInterval and re-mints it under
// RenewBefore; the operator source polls its two files every ReloadInterval
// and reloads them on change. A failure logs, sets LastError and keeps the
// previous pair serving.
func (m *CertManager) Run(ctx context.Context) {
	interval, tick := m.renewInterval, m.renewIfNeeded
	if m.source == SourceOperator {
		interval, tick = m.reloadInterval, m.reloadOperator
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// setErr records the outcome of a renewal or reload for Status.
func (m *CertManager) setErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.lastErr = ""
	} else {
		m.lastErr = err.Error()
	}
}

// ---- lab-CA source ----

// startLabCA is NewCertManager's lab-CA half.
func (m *CertManager) startLabCA() error {
	if err := os.MkdirAll(m.dir, dirMode); err != nil {
		return fmt.Errorf("preview: creating %s: %w", m.dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone; the directory
	// holds two private keys and is lab's own, so tighten it regardless.
	if err := os.Chmod(m.dir, dirMode); err != nil {
		return fmt.Errorf("preview: securing %s: %w", m.dir, err)
	}

	minted, err := m.loadOrMintCA()
	if err != nil {
		return err
	}
	reason := "new CA"
	if !minted {
		sc, why := m.loadLeaf()
		if why == "" {
			m.cur.Store(sc)
			m.log.Info("preview certificate loaded", "source", m.source, "not_after", sc.notAfter)
			return nil
		}
		reason = why
	}
	sc, err := m.mintLeaf()
	if err != nil {
		return err
	}
	m.cur.Store(sc)
	m.log.Info("preview leaf certificate minted", "source", m.source, "reason", reason, "not_after", sc.notAfter)
	return nil
}

// loadOrMintCA loads the CA from Dir, or mints and persists one when both
// files are missing (minted = true). Every other state is an error.
func (m *CertManager) loadOrMintCA() (minted bool, err error) {
	certPath, keyPath := filepath.Join(m.dir, caCertFile), filepath.Join(m.dir, caKeyFile)
	certOK, err := exists(certPath)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrCAInvalid, err)
	}
	keyOK, err := exists(keyPath)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrCAInvalid, err)
	}
	switch {
	case certOK && keyOK:
		return false, m.loadCA(certPath, keyPath)
	case certOK || keyOK:
		return false, fmt.Errorf("%w in %s; remove the leftover file to mint a new CA (devices that trusted the old one must trust the new one)",
			ErrCAIncomplete, m.dir)
	}
	return true, m.mintCA(certPath, keyPath)
}

// loadCA reads and checks the persisted CA.
func (m *CertManager) loadCA(certPath, keyPath string) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%w (%s, %s): %s; remove both files to mint a new CA (devices that trusted the old one must trust the new one)",
			ErrCAInvalid, certPath, keyPath, fmt.Sprintf(format, a...))
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return fail("%v", err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return fail("%v", err)
	}
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return fail("certificate: %v", err)
	}
	key, err := parseKeyPEM(keyPEM)
	if err != nil {
		return fail("key: %v", err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return fail("the key does not match the certificate")
	}
	if !cert.BasicConstraintsValid || !cert.IsCA {
		return fail("the certificate is not a CA")
	}
	if !m.now().Before(cert.NotAfter) {
		return fail("the CA expired %s", cert.NotAfter.Format(time.RFC3339))
	}
	m.caCert, m.caKey, m.caPEM = cert, key, certPEM
	return nil
}

// mintCA creates the CA, persists it and keeps it in m.
//
// ECDSA P-256, not Ed25519, for both CA and leaf: browsers (Chrome, Safari,
// Firefox) do not accept Ed25519 in TLS server certificate chains, and P-256
// is the one key type every TLS client verifies.
func (m *CertManager) mintCA(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("preview: generating the CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	ski, err := subjectKeyID(&key.PublicKey)
	if err != nil {
		return err
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "lab"
	}
	now := m.now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "lab preview CA (" + host + ")", Organization: []string{"lab"}},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(caValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		// A path length of zero: this CA signs leaves only, never another
		// CA, so a stolen key cannot mint an intermediate.
		MaxPathLen:     0,
		MaxPathLenZero: true,
		KeyUsage:       x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SubjectKeyId:   ski,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("preview: creating the CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("preview: parsing the minted CA: %w", err)
	}
	certPEM, keyPEM, err := encodePair(der, key)
	if err != nil {
		return err
	}
	// Key first, certificate last: a crash between the two renames leaves
	// only ca.key, which the next start reports (ErrCAIncomplete) instead
	// of minting over — a window of one rename.
	if err := writeFileAtomic(keyPath, keyPEM, keyMode); err != nil {
		return err
	}
	if err := writeFileAtomic(certPath, certPEM, certMode); err != nil {
		return err
	}
	m.caCert, m.caKey, m.caPEM = cert, key, certPEM
	m.log.Info("preview CA minted", "source", m.source, "subject", cert.Subject.CommonName, "not_after", cert.NotAfter)
	return nil
}

// loadLeaf loads the persisted leaf. A non-empty reason means it must be
// re-minted, and says why (for the log); it is never an error, since the
// leaf is lab's to replace at will.
func (m *CertManager) loadLeaf() (*servedCert, string) {
	certPEM, err := os.ReadFile(filepath.Join(m.dir, leafCertFile))
	if err != nil {
		return nil, "leaf certificate unreadable: " + err.Error()
	}
	keyPEM, err := os.ReadFile(filepath.Join(m.dir, leafKeyFile))
	if err != nil {
		return nil, "leaf key unreadable: " + err.Error()
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, "leaf pair unusable: " + err.Error()
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, "leaf certificate unparsable: " + err.Error()
	}
	if err := leaf.CheckSignatureFrom(m.caCert); err != nil {
		return nil, "leaf not signed by the current CA"
	}
	if !sameNames(leaf.DNSNames, m.leafNames()) {
		return nil, "leaf names " + strings.Join(leaf.DNSNames, ",") + " do not match the preview domain"
	}
	if why := m.leafTimeProblem(leaf); why != "" {
		return nil, why
	}
	pair.Leaf = leaf
	return newServedCert(&pair, leaf), ""
}

// leafTimeProblem says why leaf's validity window calls for a re-mint, or "".
func (m *CertManager) leafTimeProblem(leaf *x509.Certificate) string {
	now := m.now()
	if now.Before(leaf.NotBefore) {
		return "leaf not yet valid"
	}
	if leaf.NotAfter.Sub(now) < RenewBefore {
		return "leaf expires " + leaf.NotAfter.Format(time.RFC3339)
	}
	return ""
}

// leafNames is what the leaf must cover: every preview host and the bare
// domain (so a browser pointed at the domain itself gets a 404 page, not a
// certificate warning).
func (m *CertManager) leafNames() []string {
	return []string{"*." + m.domain, m.domain}
}

// mintLeaf creates a leaf under the CA, persists it and returns it. It does
// not install it; the caller swaps it in.
func (m *CertManager) mintLeaf() (*servedCert, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("preview: generating the leaf key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := m.now()
	notAfter := now.Add(leafValidity)
	if notAfter.After(m.caCert.NotAfter) {
		// A leaf outliving its CA would fail verification for its last
		// stretch anyway; ending with the CA makes that visible as expiry.
		notAfter = m.caCert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber:   serial,
		Subject:        pkix.Name{CommonName: "*." + m.domain, Organization: []string{"lab"}},
		DNSNames:       m.leafNames(),
		NotBefore:      now.Add(-backdate),
		NotAfter:       notAfter,
		KeyUsage:       x509.KeyUsageDigitalSignature,
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		AuthorityKeyId: m.caCert.SubjectKeyId,
		// BasicConstraintsValid with IsCA false: an explicit "not a CA".
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, m.caCert, &key.PublicKey, m.caKey)
	if err != nil {
		return nil, fmt.Errorf("preview: creating the leaf certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("preview: parsing the minted leaf: %w", err)
	}
	certPEM, keyPEM, err := encodePair(der, key)
	if err != nil {
		return nil, err
	}
	// Key first: a crash between the two renames leaves a new key beside
	// the old certificate, which the next start sees as a mismatched pair
	// and simply re-mints.
	if err := writeFileAtomic(filepath.Join(m.dir, leafKeyFile), keyPEM, keyMode); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(m.dir, leafCertFile), certPEM, certMode); err != nil {
		return nil, err
	}
	return newServedCert(&tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf), nil
}

// renewIfNeeded is Run's lab-CA tick.
func (m *CertManager) renewIfNeeded() {
	why := m.leafTimeProblem(m.cur.Load().leaf)
	if why == "" {
		return
	}
	sc, err := m.mintLeaf()
	if err != nil {
		m.setErr(err)
		m.log.Warn("renewing the preview leaf certificate; still serving the previous one",
			"source", m.source, "reason", why, "err", err)
		return
	}
	m.cur.Store(sc)
	m.setErr(nil)
	m.log.Info("preview leaf certificate renewed", "source", m.source, "reason", why, "not_after", sc.notAfter)
}

// ---- operator source ----

// pairStamp is what reloadOperator compares to notice a rewrite: each
// file's mtime and size. ok is false when either file could not be stat'd.
type pairStamp struct {
	certMod, keyMod   time.Time
	certSize, keySize int64
	ok                bool
}

// stampPair stats the operator pair.
func (m *CertManager) stampPair() pairStamp {
	ci, err := os.Stat(m.certFile)
	if err != nil {
		return pairStamp{}
	}
	ki, err := os.Stat(m.keyFile)
	if err != nil {
		return pairStamp{}
	}
	return pairStamp{certMod: ci.ModTime(), keyMod: ki.ModTime(), certSize: ci.Size(), keySize: ki.Size(), ok: true}
}

// loadOperator loads the operator pair. The error names both files.
func (m *CertManager) loadOperator() (*servedCert, error) {
	pair, err := tls.LoadX509KeyPair(m.certFile, m.keyFile)
	if err != nil {
		return nil, fmt.Errorf("preview: loading the certificate pair %s / %s: %w", m.certFile, m.keyFile, err)
	}
	leaf := pair.Leaf
	if leaf == nil {
		if leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return nil, fmt.Errorf("preview: parsing the certificate in %s: %w", m.certFile, err)
		}
		pair.Leaf = leaf
	}
	return newServedCert(&pair, leaf), nil
}

// reloadOperator is Run's operator tick. The stamp is taken BEFORE the load
// and recorded only when the load succeeds: an ACME client rewrites the
// certificate and the key as two separate writes, so a tick landing between
// them sees a mismatched pair. That must simply fail — keep serving the
// previous pair, report it — and since the half-written stamp is not
// recorded, every later tick retries until the pair is whole again. A write
// racing the load itself shows up as a newer stamp on the next tick.
func (m *CertManager) reloadOperator() {
	stamp := m.stampPair()
	m.mu.Lock()
	unchanged := stamp.ok && stamp == m.loaded
	prevErr := m.lastErr
	m.mu.Unlock()
	if unchanged {
		return
	}
	sc, err := m.loadOperator()
	if err != nil {
		m.setErr(err)
		// A broken pair is retried every tick; warn once per distinct
		// failure rather than once a minute for as long as it stays broken.
		level := slog.LevelWarn
		if err.Error() == prevErr {
			level = slog.LevelDebug
		}
		m.log.Log(context.Background(), level, "reloading the preview certificate; still serving the previous one",
			"source", m.source, "err", err)
		return
	}
	m.cur.Store(sc)
	m.mu.Lock()
	m.loaded = stamp
	m.lastErr = ""
	m.mu.Unlock()
	m.log.Info("preview certificate reloaded", "source", m.source, "not_after", sc.notAfter)
	m.warnIfNotCovering(sc.leaf)
}

// warnIfNotCovering logs when the operator's certificate does not cover
// preview hosts under the configured domain. A warning, not an error: the
// operator may be mid-migration, and lab cannot know better than them.
func (m *CertManager) warnIfNotCovering(leaf *x509.Certificate) {
	if err := leaf.VerifyHostname("preview." + m.domain); err != nil {
		m.log.Warn("the preview certificate does not cover *."+m.domain+"; browsers will reject previews",
			"source", m.source, "dns_names", leaf.DNSNames)
	}
}

// ---- helpers ----

// randomSerial is a random 128-bit serial number, as CAs are expected to use.
func randomSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("preview: generating a serial number: %w", err)
	}
	return n, nil
}

// subjectKeyID derives a key identifier from the public key: the first 20
// bytes of SHA-256 over its SubjectPublicKeyInfo (RFC 7093, method 1's
// shape). The leaf's AuthorityKeyId repeats it, which is how a verifier
// picks this CA out of a pool.
func subjectKeyID(pub *ecdsa.PublicKey) ([]byte, error) {
	spki, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, fmt.Errorf("preview: encoding the CA public key: %w", err)
	}
	sum := sha256.Sum256(spki)
	return sum[:20], nil
}

// encodePair PEM-encodes a certificate and its key (PKCS#8 "PRIVATE KEY").
func encodePair(der []byte, key *ecdsa.PrivateKey) (certPEM, keyPEM []byte, err error) {
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("preview: encoding a private key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})
	return certPEM, keyPEM, nil
}

// parseCertPEM parses the first CERTIFICATE block of b.
func parseCertPEM(b []byte) (*x509.Certificate, error) {
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil, errors.New("no CERTIFICATE PEM block")
		}
		if blk.Type == "CERTIFICATE" {
			return x509.ParseCertificate(blk.Bytes)
		}
	}
}

// parseKeyPEM parses an ECDSA key from a PKCS#8 "PRIVATE KEY" (what lab
// writes) or SEC 1 "EC PRIVATE KEY" block.
func parseKeyPEM(b []byte) (*ecdsa.PrivateKey, error) {
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil, errors.New("no PRIVATE KEY PEM block")
		}
		switch blk.Type {
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
			if err != nil {
				return nil, err
			}
			ek, ok := k.(*ecdsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("key is %T, want ECDSA", k)
			}
			return ek, nil
		case "EC PRIVATE KEY":
			return x509.ParseECPrivateKey(blk.Bytes)
		}
	}
}

// sameNames reports whether a and b hold the same names, in any order.
func sameNames(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// exists reports whether path exists; an error other than "not there" is
// returned as is, so an unreadable directory is not mistaken for an empty one.
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// writeFileAtomic writes data to path so that a crash leaves either the old
// file or the new one, never half of a key: a temp file in the same
// directory, fsynced, renamed over path, and the directory fsynced so the
// rename itself is durable.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("preview: writing %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	// CreateTemp makes the file 0600; Chmod (not the umask) sets the final
	// mode, so a certificate ends up 0644 and a key never anything but 0600.
	if err = f.Chmod(perm); err != nil {
		return fmt.Errorf("preview: writing %s: %w", path, err)
	}
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("preview: writing %s: %w", path, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("preview: writing %s: %w", path, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("preview: writing %s: %w", path, err)
	}
	if err = os.Rename(tmp, path); err != nil {
		return fmt.Errorf("preview: writing %s: %w", path, err)
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
