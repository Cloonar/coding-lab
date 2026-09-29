package warpgate

// Host-key scanning against an in-process x/crypto/ssh server, and the pin's
// state machine against a fake settings store with an injected scan.

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

const pinKey = "warpgate_ssh_host_key"

// fakeStore is a SettingsStore with failure injection and a write counter.
type fakeStore struct {
	mu       sync.Mutex
	m        map[string]string
	getErr   error
	setErr   error
	setCalls int
}

func (s *fakeStore) GetString(_ context.Context, key, def string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return "", s.getErr
	}
	if v := s.m[key]; v != "" {
		return v, nil
	}
	return def, nil
}

func (s *fakeStore) SetSetting(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setCalls++
	if s.setErr != nil {
		return s.setErr
	}
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[key] = value
	return nil
}

func (s *fakeStore) get() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[pinKey], s.setCalls
}

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

func TestHostKeyPinLifecycle(t *testing.T) {
	k1, k2, k3 := ed25519Signer(t).PublicKey(), rsaSigner(t).PublicKey(), ed25519Signer(t).PublicKey()
	store := &fakeStore{}
	scan := &fakeScan{keys: []ssh.PublicKey{k1, k2}}
	pin := NewHostKeyPin(store, pinKey, pinAddr, scan.scan)
	ctx := context.Background()
	original := Fingerprints([]ssh.PublicKey{k1, k2})

	// 1. Trust on first use: nothing stored, a clean scan → pinned.
	st := pin.Check(ctx)
	if st.State != HostKeyPinned || !reflect.DeepEqual(st.Pinned, original) || !reflect.DeepEqual(st.Observed, original) || st.Error != "" {
		t.Fatalf("TOFU status = %+v", st)
	}
	stored, sets := store.get()
	if stored != FormatAuthorizedKeys([]ssh.PublicKey{k1, k2}) || sets != 1 {
		t.Fatalf("TOFU stored %q with %d writes", stored, sets)
	}

	// 2. Stable: the same keys (in another order) → pinned, no write.
	scan.set([]ssh.PublicKey{k2, k1}, nil)
	if st := pin.Check(ctx); st.State != HostKeyPinned {
		t.Errorf("stable status = %+v", st)
	}
	kh, err := pin.KnownHosts(ctx)
	if err != nil {
		t.Fatalf("KnownHosts: %v", err)
	}
	pinned, _ := ParseAuthorizedKeys(stored)
	if kh != KnownHostsLines(pinAddr, pinned) || !strings.HasPrefix(kh, "[10.88.0.1]:2222 ") {
		t.Errorf("KnownHosts = %q, want the pin rendered for %s", kh, pinAddr)
	}
	if _, sets := store.get(); sets != 1 {
		t.Errorf("a stable check wrote the pin (%d writes)", sets)
	}

	// 3. Mismatch: a different key → mismatch, nothing stored, KnownHosts refuses
	//    with both fingerprint sets and the accept endpoint.
	scan.set([]ssh.PublicKey{k3}, nil)
	st = pin.Check(ctx)
	if st.State != HostKeyMismatch || !reflect.DeepEqual(st.Pinned, original) || !reflect.DeepEqual(st.Observed, Fingerprints([]ssh.PublicKey{k3})) {
		t.Errorf("mismatch status = %+v", st)
	}
	if got, sets := store.get(); got != stored || sets != 1 {
		t.Errorf("a mismatch changed the pin (%d writes)", sets)
	}
	_, err = pin.KnownHosts(ctx)
	if err == nil {
		t.Fatal("KnownHosts succeeded on a mismatch")
	}
	for _, want := range append([]string{ssh.FingerprintSHA256(k3), "/api/v1/warpgate/host-key/accept", pinAddr}, original...) {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("mismatch error %q does not mention %q", err, want)
		}
	}

	// 4. A subset is not the pin: set equality, not containment.
	scan.set([]ssh.PublicKey{k1}, nil)
	if st := pin.Check(ctx); st.State != HostKeyMismatch {
		t.Errorf("subset status = %+v, want mismatch", st)
	}

	// 5. Accept with a fingerprint that is not observed: refused, pin unchanged.
	scan.set([]ssh.PublicKey{k3}, nil)
	st, err = pin.Accept(ctx, ssh.FingerprintSHA256(k1))
	if !errors.Is(err, ErrFingerprintNotObserved) || st.State != HostKeyMismatch {
		t.Errorf("Accept(unobserved) = %+v, %v; want ErrFingerprintNotObserved with the mismatch status", st, err)
	}
	if got, sets := store.get(); got != stored || sets != 1 {
		t.Errorf("a refused accept changed the pin (%d writes)", sets)
	}
	if _, err := pin.Accept(ctx, "  "); err == nil {
		t.Error("Accept with an empty fingerprint succeeded")
	}

	// 6. Accept with an observed fingerprint: the pin becomes the observed set.
	st, err = pin.Accept(ctx, " "+ssh.FingerprintSHA256(k3)+" ")
	if err != nil {
		t.Fatalf("Accept(observed): %v", err)
	}
	newFps := Fingerprints([]ssh.PublicKey{k3})
	if st.State != HostKeyPinned || !reflect.DeepEqual(st.Pinned, newFps) || !reflect.DeepEqual(st.Observed, newFps) {
		t.Errorf("accepted status = %+v", st)
	}
	if got, _ := store.get(); got != FormatAuthorizedKeys([]ssh.PublicKey{k3}) {
		t.Errorf("stored pin after accept = %q", got)
	}
	if st := pin.Check(ctx); st.State != HostKeyPinned {
		t.Errorf("status after accept = %+v", st)
	}

	// 7. Unreachable: the scan fails → unreachable with the pin still reported;
	//    KnownHosts and Accept refuse; nothing is written.
	_, setsBefore := store.get()
	scan.set(nil, errors.New("dial tcp 10.88.0.1:2222: connect: connection refused"))
	st = pin.Check(ctx)
	if st.State != HostKeyUnreachable || !reflect.DeepEqual(st.Pinned, newFps) || len(st.Observed) != 0 || !strings.Contains(st.Error, "connection refused") {
		t.Errorf("unreachable status = %+v", st)
	}
	if _, err := pin.KnownHosts(ctx); err == nil || !strings.Contains(err.Error(), pinAddr) || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("KnownHosts while unreachable = %v", err)
	}
	st, err = pin.Accept(ctx, ssh.FingerprintSHA256(k3))
	if err == nil || st.State != HostKeyUnreachable || errors.Is(err, ErrFingerprintNotObserved) {
		t.Errorf("Accept while unreachable = %+v, %v", st, err)
	}
	if _, sets := store.get(); sets != setsBefore {
		t.Error("an unreachable check or accept wrote the pin")
	}
}

// TestHostKeyPinUnreachableWhileUnpinnedDoesNotPin: TOFU needs a successful
// scan; a failed first scan pins nothing.
func TestHostKeyPinUnreachableWhileUnpinnedDoesNotPin(t *testing.T) {
	store := &fakeStore{}
	scan := &fakeScan{err: errors.New("no route to host")}
	pin := NewHostKeyPin(store, pinKey, pinAddr, scan.scan)
	st := pin.Check(context.Background())
	if st.State != HostKeyUnreachable || len(st.Pinned) != 0 || st.Pinned == nil {
		t.Errorf("status = %+v", st)
	}
	if _, sets := store.get(); sets != 0 {
		t.Errorf("a failed first scan wrote %d times", sets)
	}
}

func TestHostKeyPinStoreFailures(t *testing.T) {
	k := ed25519Signer(t).PublicKey()
	ctx := context.Background()

	t.Run("read failure is unreachable, never TOFU", func(t *testing.T) {
		store := &fakeStore{getErr: errors.New("database is locked")}
		pin := NewHostKeyPin(store, pinKey, pinAddr, (&fakeScan{keys: []ssh.PublicKey{k}}).scan)
		st := pin.Check(ctx)
		if st.State != HostKeyUnreachable || !strings.Contains(st.Error, "database is locked") || len(st.Observed) != 0 {
			t.Errorf("status = %+v", st)
		}
		if _, sets := store.get(); sets != 0 {
			t.Error("a failed read led to a pin write")
		}
		if _, err := pin.KnownHosts(ctx); err == nil {
			t.Error("KnownHosts succeeded with an unreadable store")
		}
	})

	t.Run("TOFU write failure stays unpinned", func(t *testing.T) {
		store := &fakeStore{setErr: errors.New("disk full")}
		pin := NewHostKeyPin(store, pinKey, pinAddr, (&fakeScan{keys: []ssh.PublicKey{k}}).scan)
		st := pin.Check(ctx)
		if st.State != HostKeyUnpinned || !strings.Contains(st.Error, "disk full") || len(st.Observed) != 1 {
			t.Errorf("status = %+v", st)
		}
		if _, err := pin.KnownHosts(ctx); err == nil || !strings.Contains(err.Error(), "no SSH host key is pinned") {
			t.Errorf("KnownHosts = %v", err)
		}
	})

	t.Run("corrupt pin is a mismatch that accept repairs", func(t *testing.T) {
		store := &fakeStore{m: map[string]string{pinKey: "ssh-ed25519 not-a-key\n"}}
		pin := NewHostKeyPin(store, pinKey, pinAddr, (&fakeScan{keys: []ssh.PublicKey{k}}).scan)
		st := pin.Check(ctx)
		if st.State != HostKeyMismatch || !strings.Contains(st.Error, "unreadable") {
			t.Errorf("status = %+v", st)
		}
		if _, sets := store.get(); sets != 0 {
			t.Error("a corrupt pin was overwritten without an accept")
		}
		if _, err := pin.Accept(ctx, ssh.FingerprintSHA256(k)); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		if st := pin.Check(ctx); st.State != HostKeyPinned {
			t.Errorf("status after accept = %+v", st)
		}
	})

	t.Run("accept write failure", func(t *testing.T) {
		store := &fakeStore{m: map[string]string{pinKey: FormatAuthorizedKeys([]ssh.PublicKey{ed25519Signer(t).PublicKey()})}, setErr: errors.New("disk full")}
		pin := NewHostKeyPin(store, pinKey, pinAddr, (&fakeScan{keys: []ssh.PublicKey{k}}).scan)
		st, err := pin.Accept(ctx, ssh.FingerprintSHA256(k))
		if err == nil || errors.Is(err, ErrFingerprintNotObserved) || st.State != HostKeyMismatch {
			t.Errorf("Accept = %+v, %v; want the write error with the mismatch status", st, err)
		}
	})
}

// TestHostKeyPinConcurrentTOFUPinsOnce: many spawns checking an unpinned
// listener at once must agree on one pin and write it once.
func TestHostKeyPinConcurrentTOFUPinsOnce(t *testing.T) {
	k := ed25519Signer(t).PublicKey()
	store := &fakeStore{}
	pin := NewHostKeyPin(store, pinKey, pinAddr, (&fakeScan{keys: []ssh.PublicKey{k}}).scan)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if kh, err := pin.KnownHosts(context.Background()); err != nil || !strings.Contains(kh, "ssh-ed25519") {
				t.Errorf("KnownHosts = %q, %v", kh, err)
			}
		}()
	}
	wg.Wait()
	if _, sets := store.get(); sets != 1 {
		t.Errorf("concurrent TOFU wrote the pin %d times, want 1", sets)
	}
}

// TestHostKeyPinAgainstRealListener wires the default scan (nil) end to end.
func TestHostKeyPinAgainstRealListener(t *testing.T) {
	ed, rs := ed25519Signer(t), rsaSigner(t)
	addr := startSSHServer(t, ed, rs)
	store := &fakeStore{}
	pin := NewHostKeyPin(store, pinKey, addr, nil)
	kh, err := pin.KnownHosts(context.Background())
	if err != nil {
		t.Fatalf("KnownHosts: %v", err)
	}
	if strings.Count(kh, "\n") != 2 || !strings.Contains(kh, "ssh-ed25519") || !strings.Contains(kh, "ssh-rsa") {
		t.Errorf("KnownHosts = %q", kh)
	}
}
