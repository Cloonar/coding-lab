package warpgate

// Host-key scanning against an in-process x/crypto/ssh server, and the pin's
// trusted-key comparison with an injected scan.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startSSHServer runs an SSH server on 127.0.0.1:0 presenting signers as its
// host keys and returns its address. Connections are handshaken and dropped;
// the scan aborts during key exchange, so nothing past it matters.
func startSSHServer(t *testing.T, signers ...ssh.Signer) string {
	t.Helper()
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	for _, s := range signers {
		cfg.AddHostKey(s)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "test server")
				}
				_ = sc.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

func ed25519Signer(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rsaSigner(t *testing.T) ssh.Signer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func marshalSet(keys []ssh.PublicKey) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, string(k.Marshal()))
	}
	return out
}

// TestScanHostKeysReadsEveryKeyType: a listener with an ed25519 and an RSA
// host key (Warpgate's pair) yields both — and the ECDSA algorithms it does
// not offer are skipped rather than failing the scan.
func TestScanHostKeysReadsEveryKeyType(t *testing.T) {
	ed, rs := ed25519Signer(t), rsaSigner(t)
	addr := startSSHServer(t, ed, rs)

	keys, err := ScanHostKeys(context.Background(), addr)
	if err != nil {
		t.Fatalf("ScanHostKeys: %v", err)
	}
	want := []string{string(ed.PublicKey().Marshal()), string(rs.PublicKey().Marshal())}
	if got := marshalSet(keys); !reflect.DeepEqual(got, want) {
		t.Errorf("scanned %d keys %v, want the ed25519 then the RSA key", len(keys), Fingerprints(keys))
	}
	if keys[0].Type() != ssh.KeyAlgoED25519 || keys[1].Type() != ssh.KeyAlgoRSA {
		t.Errorf("key types = %s, %s", keys[0].Type(), keys[1].Type())
	}
}

func TestScanHostKeysSingleKey(t *testing.T) {
	ed := ed25519Signer(t)
	keys, err := ScanHostKeys(context.Background(), startSSHServer(t, ed))
	if err != nil {
		t.Fatalf("ScanHostKeys: %v", err)
	}
	if len(keys) != 1 || string(keys[0].Marshal()) != string(ed.PublicKey().Marshal()) {
		t.Errorf("keys = %v", Fingerprints(keys))
	}
}

// TestScanHostKeysUnreachable: nothing listening is an error naming the
// address, and it comes back fast (no per-algorithm timeout stacking).
func TestScanHostKeysUnreachable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	start := time.Now()
	keys, err := ScanHostKeys(context.Background(), addr)
	if err == nil {
		t.Fatalf("ScanHostKeys succeeded with %v", Fingerprints(keys))
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("error %q does not name %s", err, addr)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("an unreachable scan took %v", d)
	}
	if _, err := ScanHostKeys(context.Background(), ""); err == nil {
		t.Error("ScanHostKeys with an empty address succeeded")
	}
}

// TestScanHostKeysHonoursContext: a listener that accepts but never speaks
// must not hold the caller past its context.
func TestScanHostKeysHonoursContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = ScanHostKeys(ctx, ln.Addr().String())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the context deadline", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a cancelled scan took %v", d)
	}
}

func TestKnownHostsLines(t *testing.T) {
	ed, rs := ed25519Signer(t), rsaSigner(t)
	addr := startSSHServer(t, ed, rs)
	keys, err := ScanHostKeys(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(addr)

	got := KnownHostsLines(addr, keys)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if !strings.HasSuffix(got, "\n") || len(lines) != 2 {
		t.Fatalf("KnownHostsLines = %q, want two newline-terminated lines", got)
	}
	edLine := "[127.0.0.1]:" + port + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(ed.PublicKey())))
	if lines[0] != edLine {
		t.Errorf("line 0 = %q, want %q", lines[0], edLine)
	}
	if !strings.HasPrefix(lines[1], "[127.0.0.1]:"+port+" ssh-rsa ") {
		t.Errorf("line 1 = %q", lines[1])
	}

	// Port 22 drops the brackets, the form OpenSSH looks up.
	if got := KnownHostsLines("warpgate.local:22", keys[:1]); !strings.HasPrefix(got, "warpgate.local ssh-ed25519 ") {
		t.Errorf("port-22 line = %q", got)
	}
	if got := KnownHostsLines(addr, nil); got != "" {
		t.Errorf("no keys = %q, want empty", got)
	}
}

func TestAuthorizedKeysRoundTrip(t *testing.T) {
	a, b := ed25519Signer(t).PublicKey(), rsaSigner(t).PublicKey()
	text := FormatAuthorizedKeys([]ssh.PublicKey{b, a, a})
	if strings.Count(text, "\n") != 2 || !strings.HasSuffix(text, "\n") {
		t.Errorf("FormatAuthorizedKeys = %q, want two canonical lines", text)
	}
	if again := FormatAuthorizedKeys([]ssh.PublicKey{a, b}); again != text {
		t.Errorf("FormatAuthorizedKeys is order-dependent:\n%q\n%q", text, again)
	}
	keys, err := ParseAuthorizedKeys("# pinned by lab\n\n" + text)
	if err != nil {
		t.Fatalf("ParseAuthorizedKeys: %v", err)
	}
	if !sameKeySet(keys, []ssh.PublicKey{a, b}) || len(keys) != 2 {
		t.Errorf("round trip = %v", Fingerprints(keys))
	}
	if keys, err := ParseAuthorizedKeys(""); err != nil || len(keys) != 0 {
		t.Errorf("empty text = %v, %v", keys, err)
	}
	if FormatAuthorizedKeys(nil) != "" {
		t.Error("FormatAuthorizedKeys(nil) is not empty")
	}
	if _, err := ParseAuthorizedKeys(text + "ssh-ed25519 !!!garbage\n"); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("garbage line: error = %v, want it named by line number", err)
	}
}

func TestFingerprints(t *testing.T) {
	a, b := ed25519Signer(t).PublicKey(), ed25519Signer(t).PublicKey()
	got := Fingerprints([]ssh.PublicKey{b, a, b})
	want := []string{ssh.FingerprintSHA256(a), ssh.FingerprintSHA256(b)}
	if want[0] > want[1] {
		want[0], want[1] = want[1], want[0]
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Fingerprints = %v, want sorted, de-duplicated %v", got, want)
	}
	if got := Fingerprints(nil); got == nil || len(got) != 0 {
		t.Errorf("Fingerprints(nil) = %#v, want an empty non-nil slice", got)
	}
}

// --- HostKeyPin ------------------------------------------------------------

// fakeScan is an injectable scan whose answer a test changes between calls.
type fakeScan struct {
	mu   sync.Mutex
	keys []ssh.PublicKey
	err  error
	n    int
}

func (f *fakeScan) set(keys []ssh.PublicKey, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys, f.err = keys, err
}

func (f *fakeScan) scan(_ context.Context, _ string) ([]ssh.PublicKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return f.keys, f.err
}

const pinAddr = "10.88.0.1:2222"

// TestHostKeyPinTrusted: with --warpgate-ssh-host-key set, the trusted key is
// what a run gets — verbatim, never widened by what the listener presents —
// and a listener presenting none of the trusted keys refuses.
func TestHostKeyPinTrusted(t *testing.T) {
	k1, k2, k3 := ed25519Signer(t).PublicKey(), rsaSigner(t).PublicKey(), ed25519Signer(t).PublicKey()
	scan := &fakeScan{keys: []ssh.PublicKey{k1, k2}}
	pin := NewHostKeyPin(pinAddr, []ssh.PublicKey{k1}, scan.scan)
	ctx := context.Background()
	trusted := Fingerprints([]ssh.PublicKey{k1})
	if !pin.Pinned() {
		t.Fatal("Pinned() = false with a trusted key configured")
	}

	// 1. The listener presents the trusted key among others → pinned, and
	//    the run's known_hosts holds the TRUSTED key only.
	st := pin.Check(ctx)
	if st.State != HostKeyPinned || !reflect.DeepEqual(st.Pinned, trusted) || !reflect.DeepEqual(st.Observed, Fingerprints([]ssh.PublicKey{k1, k2})) || st.Error != "" {
		t.Fatalf("status = %+v", st)
	}
	kh, err := pin.KnownHosts(ctx)
	if err != nil {
		t.Fatalf("KnownHosts: %v", err)
	}
	if kh != KnownHostsLines(pinAddr, []ssh.PublicKey{k1}) || !strings.HasPrefix(kh, "[10.88.0.1]:2222 ") || strings.Contains(kh, "ssh-rsa") {
		t.Errorf("KnownHosts = %q, want the trusted key alone rendered for %s", kh, pinAddr)
	}

	// 2. Order and repeats are irrelevant.
	scan.set([]ssh.PublicKey{k2, k1, k1}, nil)
	if st := pin.Check(ctx); st.State != HostKeyPinned {
		t.Errorf("reordered status = %+v", st)
	}

	// 3. Mismatch: the listener presents none of the trusted keys → refused,
	//    naming both sides, the address, and the setting to change.
	scan.set([]ssh.PublicKey{k3}, nil)
	st = pin.Check(ctx)
	if st.State != HostKeyMismatch || !reflect.DeepEqual(st.Pinned, trusted) || !reflect.DeepEqual(st.Observed, Fingerprints([]ssh.PublicKey{k3})) {
		t.Errorf("mismatch status = %+v", st)
	}
	_, err = pin.KnownHosts(ctx)
	if err == nil {
		t.Fatal("KnownHosts succeeded on a mismatch")
	}
	for _, want := range []string{ssh.FingerprintSHA256(k3), ssh.FingerprintSHA256(k1), pinAddr, "--warpgate-ssh-host-key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("mismatch error %q does not mention %q", err, want)
		}
	}

	// 4. Unreachable: the trusted set is still reported; KnownHosts refuses.
	scan.set(nil, errors.New("dial tcp 10.88.0.1:2222: connect: connection refused"))
	st = pin.Check(ctx)
	if st.State != HostKeyUnreachable || !reflect.DeepEqual(st.Pinned, trusted) || len(st.Observed) != 0 || st.Observed == nil || !strings.Contains(st.Error, "connection refused") {
		t.Errorf("unreachable status = %+v", st)
	}
	if _, err := pin.KnownHosts(ctx); err == nil || !strings.Contains(err.Error(), pinAddr) || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("KnownHosts while unreachable = %v", err)
	}
}

// TestHostKeyPinSeveralTrustedKeys: one presented trusted key is enough, and
// the run gets every trusted key (ssh picks the one the server negotiates).
func TestHostKeyPinSeveralTrustedKeys(t *testing.T) {
	k1, k2 := ed25519Signer(t).PublicKey(), rsaSigner(t).PublicKey()
	pin := NewHostKeyPin(pinAddr, []ssh.PublicKey{k1, k2}, (&fakeScan{keys: []ssh.PublicKey{k2}}).scan)
	st := pin.Check(context.Background())
	if st.State != HostKeyPinned || !reflect.DeepEqual(st.Pinned, Fingerprints([]ssh.PublicKey{k1, k2})) {
		t.Fatalf("status = %+v", st)
	}
	kh, err := pin.KnownHosts(context.Background())
	if err != nil || strings.Count(kh, "\n") != 2 || !strings.Contains(kh, "ssh-ed25519") || !strings.Contains(kh, "ssh-rsa") {
		t.Errorf("KnownHosts = %q, %v; want both trusted keys", kh, err)
	}
}

// TestHostKeyPinUntrusted: with no trusted key configured, a run trusts
// whatever the listener presents to the scan that wires it — every key,
// every time — and only an unreachable listener refuses.
func TestHostKeyPinUntrusted(t *testing.T) {
	k1, k2, k3 := ed25519Signer(t).PublicKey(), rsaSigner(t).PublicKey(), ed25519Signer(t).PublicKey()
	scan := &fakeScan{keys: []ssh.PublicKey{k1, k2}}
	pin := NewHostKeyPin(pinAddr, nil, scan.scan)
	ctx := context.Background()
	if pin.Pinned() {
		t.Fatal("Pinned() = true with no trusted key configured")
	}

	st := pin.Check(ctx)
	if st.State != HostKeyUnpinned || len(st.Pinned) != 0 || st.Pinned == nil || !reflect.DeepEqual(st.Observed, Fingerprints([]ssh.PublicKey{k1, k2})) || st.Error != "" {
		t.Fatalf("status = %+v", st)
	}
	kh, err := pin.KnownHosts(ctx)
	if err != nil || kh != KnownHostsLines(pinAddr, []ssh.PublicKey{k1, k2}) {
		t.Errorf("KnownHosts = %q, %v; want every observed key", kh, err)
	}

	// The listener's key changes: accepted, nothing to compare against.
	scan.set([]ssh.PublicKey{k3}, nil)
	if st := pin.Check(ctx); st.State != HostKeyUnpinned || !reflect.DeepEqual(st.Observed, Fingerprints([]ssh.PublicKey{k3})) {
		t.Errorf("status after a key change = %+v", st)
	}
	if kh, err := pin.KnownHosts(ctx); err != nil || kh != KnownHostsLines(pinAddr, []ssh.PublicKey{k3}) {
		t.Errorf("KnownHosts after a key change = %q, %v", kh, err)
	}

	scan.set(nil, errors.New("no route to host"))
	if st := pin.Check(ctx); st.State != HostKeyUnreachable || !strings.Contains(st.Error, "no route to host") {
		t.Errorf("unreachable status = %+v", st)
	}
	if _, err := pin.KnownHosts(ctx); err == nil {
		t.Error("KnownHosts succeeded with an unreachable listener")
	}
}

// TestHostKeyPinAgainstRealListener wires the default scan (nil) end to end.
func TestHostKeyPinAgainstRealListener(t *testing.T) {
	ed, rs := ed25519Signer(t), rsaSigner(t)
	addr := startSSHServer(t, ed, rs)
	ctx := context.Background()

	kh, err := NewHostKeyPin(addr, nil, nil).KnownHosts(ctx)
	if err != nil {
		t.Fatalf("untrusted KnownHosts: %v", err)
	}
	if strings.Count(kh, "\n") != 2 || !strings.Contains(kh, "ssh-ed25519") || !strings.Contains(kh, "ssh-rsa") {
		t.Errorf("untrusted KnownHosts = %q", kh)
	}

	kh, err = NewHostKeyPin(addr, []ssh.PublicKey{ed.PublicKey()}, nil).KnownHosts(ctx)
	if err != nil {
		t.Fatalf("trusted KnownHosts: %v", err)
	}
	if strings.Count(kh, "\n") != 1 || !strings.Contains(kh, "ssh-ed25519") {
		t.Errorf("trusted KnownHosts = %q, want the trusted key alone", kh)
	}

	if _, err := NewHostKeyPin(addr, []ssh.PublicKey{ed25519Signer(t).PublicKey()}, nil).KnownHosts(ctx); err == nil || !strings.Contains(err.Error(), "--warpgate-ssh-host-key") {
		t.Errorf("KnownHosts with an unpresented trusted key = %v, want a mismatch", err)
	}
}
