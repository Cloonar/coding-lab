package warpgate

// Warpgate's SSH host key, pinned by lab (ADR-0068 decision 4). A run reaches
// its targets through the bastion with StrictHostKeyChecking on and a
// known_hosts file lab writes — never accept-new inside a run — so lab has to
// know the bastion's host key before the first run needs it. Warpgate exposes
// no endpoint for it (its host keys live in its database), so lab reads it the
// way ssh-keyscan does: one SSH handshake per host-key algorithm, aborted the
// moment the server has proven possession of its key and before any
// authentication.
//
// The pin is trust-on-first-use exactly ONCE, on the operator's host, by lab
// itself: the first successful scan with nothing stored becomes the pin.
// After that a scan that differs is a mismatch — nothing is stored, and
// target-bearing spawns are refused — until the operator verifies a fingerprint
// out of band and accepts it. known_hosts for runs is always rendered from the
// PIN, never from a fresh scan, so a man in the middle between lab and the
// bastion can at most block spawns, never get a run to trust its key.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// hostKeyAlgorithms are the host-key algorithms scanned, one handshake each.
// A server with an RSA key presents the same key for rsa-sha2-512 as for any
// other RSA signature algorithm, so one RSA entry covers it. Warpgate 0.29.1
// offers exactly ed25519 and RSA (warpgate-protocol-ssh/src/server/mod.rs:
// 148-159, keys from the ssh_host_key_ed25519/_rsa parameters); the ECDSA
// entries cost three fast "not offered" handshakes there and cover a future
// listener that has one.
var hostKeyAlgorithms = []string{
	ssh.KeyAlgoED25519,
	ssh.KeyAlgoRSASHA512,
	ssh.KeyAlgoECDSA256,
	ssh.KeyAlgoECDSA384,
	ssh.KeyAlgoECDSA521,
}

// scanTimeout bounds one scan handshake, dial included. The listener is a
// same-host sidecar: a handshake that has not reached the host key in this
// long is not coming.
const scanTimeout = 5 * time.Second

// scanUser is the SSH username the scan's client announces. It never reaches
// authentication — the scan aborts in key exchange — but a fixed, obviously
// lab-owned name keeps any server-side log line attributable.
const scanUser = "lab-host-key-scan"

// errHostKeyCaptured is what the scan's HostKeyCallback returns once it has
// the key: the handshake aborts with it, before any authentication.
var errHostKeyCaptured = errors.New("warpgate: host key captured; aborting the handshake")

// ScanHostKeys returns the distinct host keys the SSH listener at addr
// presents, one handshake per algorithm in hostKeyAlgorithms. The key a
// handshake reports has been proven by the server's signature over the key
// exchange (x/crypto verifies it before the callback runs), so a key in this
// list is one the listener actually holds.
//
// An algorithm the server does not offer is skipped. Every OTHER failure —
// unreachable, reset mid-handshake, timeout — fails the whole scan rather
// than returning what the earlier handshakes found: the pin is compared as a
// SET, and a partial set from a flaky scan would read as a host-key change
// (a scary "mismatch") instead of the transient failure it is. Zero keys is
// an error naming addr and the first failure.
//
// ctx cancels the scan, including a handshake in progress.
func ScanHostKeys(ctx context.Context, addr string) ([]ssh.PublicKey, error) {
	if addr == "" {
		return nil, errors.New("warpgate: SSH address must not be empty")
	}
	var (
		keys     []ssh.PublicKey
		seen     = make(map[string]bool)
		firstErr error
	)
	for _, alg := range hostKeyAlgorithms {
		key, err := scanOne(ctx, addr, alg)
		switch {
		case err == nil:
			if k := string(key.Marshal()); !seen[k] {
				seen[k] = true
				keys = append(keys, key)
			}
		case isHostKeyAlgUnsupported(err):
			if firstErr == nil {
				firstErr = err
			}
		default:
			return nil, fmt.Errorf("warpgate: scanning the SSH host keys at %s (%s): %w", addr, alg, err)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("warpgate: the SSH listener at %s presented no host key for any of %s: %w", addr, strings.Join(hostKeyAlgorithms, ", "), firstErr)
	}
	return keys, nil
}

// scanOne performs one handshake restricted to alg and returns the host key
// the server presented for it.
func scanOne(ctx context.Context, addr, alg string) (ssh.PublicKey, error) {
	dctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	// NewClientConn takes no context; closing the connection is what unblocks
	// a handshake when ctx is cancelled or the per-handshake timeout fires.
	stop := context.AfterFunc(dctx, func() { _ = conn.Close() })
	defer stop()

	// The callback runs on the handshake goroutine; a buffered channel hands
	// the key back without a data race.
	captured := make(chan ssh.PublicKey, 1)
	cfg := &ssh.ClientConfig{
		User:              scanUser,
		HostKeyAlgorithms: []string{alg},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			select {
			case captured <- key:
			default:
			}
			return errHostKeyCaptured
		},
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err == nil {
		// Unreachable — the callback fails every handshake — but a client
		// that did come up must not leak.
		go ssh.DiscardRequests(reqs)
		go func() {
			for ch := range chans {
				_ = ch.Reject(ssh.Prohibited, "host key scan")
			}
		}()
		_ = sshConn.Close()
	}
	select {
	case key := <-captured:
		return key, nil
	default:
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if dctx.Err() != nil {
		return nil, fmt.Errorf("no host key within %s: %w", scanTimeout, dctx.Err())
	}
	if err == nil {
		return nil, errors.New("the handshake completed without presenting a host key")
	}
	return nil, err
}

// isHostKeyAlgUnsupported recognizes x/crypto's key-exchange negotiation
// failure for the host-key algorithm — the server does not offer the one
// algorithm this handshake allowed. x/crypto's error type for it is
// unexported, so the match is on its message; the in-process server test
// pins it (a server without ECDSA keys must scan cleanly), so an x/crypto
// change of wording fails a test rather than every scan.
//
// The client sees this error — rather than a bare EOF from a server that gave
// up first — because the server's KEXINIT reaches it before any disconnect:
// Warpgate's russh (0.63.3) sends KEXINIT eagerly right after the version
// exchange (russh server/mod.rs:1091) and flushes it before reading anything
// (server/session.rs:640-642), exactly as x/crypto's own server does.
func isHostKeyAlgUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no common algorithm for host key")
}

// FormatAuthorizedKeys renders keys as authorized_keys lines ("<type>
// <base64>\n"), sorted and de-duplicated so that the same key set always
// renders to the same text — the stored pin is canonical.
func FormatAuthorizedKeys(keys []ssh.PublicKey) string {
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))))
	}
	slices.Sort(lines)
	lines = slices.Compact(lines)
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// ParseAuthorizedKeys parses authorized_keys text (FormatAuthorizedKeys'
// output, or anything in that format) into keys. Blank and "#" lines are
// skipped; any other line that does not parse is an error naming its line
// number. Empty text is zero keys and no error.
func ParseAuthorizedKeys(s string) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("warpgate: authorized_keys line %d: %w", i+1, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// KnownHostsLines renders keys as known_hosts lines for addr, one per key
// with a trailing newline. The host pattern is knownhosts.Normalize(addr):
// "host" for port 22, "[host]:port" otherwise — the form OpenSSH looks up.
func KnownHostsLines(addr string, keys []ssh.PublicKey) string {
	host := knownhosts.Normalize(addr)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(knownhosts.Line([]string{host}, k))
		b.WriteByte('\n')
	}
	return b.String()
}

// Fingerprints returns the keys' SHA256 fingerprints ("SHA256:…", the form
// ssh-keygen -lf prints), sorted and de-duplicated. Never nil, so a status
// built from it serializes as [] rather than null.
func Fingerprints(keys []ssh.PublicKey) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, ssh.FingerprintSHA256(k))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// sameKeySet reports whether a and b hold the same keys, compared by their
// wire encoding and ignoring order and repeats.
func sameKeySet(a, b []ssh.PublicKey) bool {
	set := func(keys []ssh.PublicKey) map[string]bool {
		m := make(map[string]bool, len(keys))
		for _, k := range keys {
			m[string(k.Marshal())] = true
		}
		return m
	}
	sa, sb := set(a), set(b)
	if len(sa) != len(sb) {
		return false
	}
	for k := range sa {
		if !sb[k] {
			return false
		}
	}
	return true
}

// SettingsStore is where the pin lives: one settings row holding
// FormatAuthorizedKeys text. *store.Store satisfies it; GetString returns def
// for a missing or blank row, which is what "unpinned" is.
type SettingsStore interface {
	GetString(ctx context.Context, key, def string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// HostKeyState is the outcome of comparing the listener's keys to the pin.
type HostKeyState string

const (
	// HostKeyUnpinned: nothing is pinned. Check pins on the first successful
	// scan, so this persists only when storing the pin failed (Error says so).
	HostKeyUnpinned HostKeyState = "unpinned"
	// HostKeyPinned: the listener presents exactly the pinned key set.
	HostKeyPinned HostKeyState = "pinned"
	// HostKeyMismatch: the listener presents a different key set than the pin
	// (or the stored pin is unreadable — Error says so). Nothing is stored;
	// the operator must accept an observed fingerprint.
	HostKeyMismatch HostKeyState = "mismatch"
	// HostKeyUnreachable: the comparison could not be made — the listener
	// could not be scanned, or the stored pin could not be read from lab's
	// settings. Error says which.
	HostKeyUnreachable HostKeyState = "unreachable"
)

// HostKeyStatus is one check's result, shaped for the health endpoint.
type HostKeyStatus struct {
	State    HostKeyState
	Pinned   []string // fingerprints of the stored pin (empty when unpinned)
	Observed []string // fingerprints of the last scan (empty when unreachable)
	Error    string   // why the state is not simply pinned/mismatch, when there is a reason
}

// ErrFingerprintNotObserved is returned (wrapped) by Accept when the
// fingerprint is not one the listener presents right now — the operator
// verified a key that is not the one lab sees, or the key changed again since
// they looked. The HTTP layer maps it to 409; a scan failure is a separate
// error with State unreachable.
var ErrFingerprintNotObserved = errors.New("fingerprint is not among the host keys the Warpgate SSH listener presents")

// HostKeyPin holds the pin of Warpgate's SSH host key(s) in one settings row
// and compares the listener's current keys against it. It is safe for
// concurrent use: scans run in parallel (every target-bearing spawn scans),
// while the read-compare-write of the stored pin is serialized, so a TOFU pin
// and an Accept cannot interleave.
type HostKeyPin struct {
	st         SettingsStore
	settingKey string
	addr       string
	scan       func(ctx context.Context, addr string) ([]ssh.PublicKey, error)

	mu sync.Mutex // guards the stored pin's read-compare-write
}

// NewHostKeyPin returns a pin stored under settingKey (store.SettingWarpgateSSHHostKey)
// for the SSH listener at addr. scan is ScanHostKeys when nil; tests inject
// their own.
func NewHostKeyPin(st SettingsStore, settingKey, addr string, scan func(ctx context.Context, addr string) ([]ssh.PublicKey, error)) *HostKeyPin {
	if scan == nil {
		scan = ScanHostKeys
	}
	return &HostKeyPin{st: st, settingKey: settingKey, addr: addr, scan: scan}
}

// Check scans the listener and compares its keys to the pin, pinning them if
// nothing is pinned yet (trust on first use — the only write Check ever
// makes). It always scans; there is no cached answer, because the scan is
// also the SSH listener's reachability probe.
func (p *HostKeyPin) Check(ctx context.Context) HostKeyStatus {
	st, _ := p.check(ctx)
	return st
}

// check is Check that also returns the pinned keys, which KnownHosts renders.
func (p *HostKeyPin) check(ctx context.Context) (HostKeyStatus, []ssh.PublicKey) {
	observed, scanErr := p.scan(ctx, p.addr)

	p.mu.Lock()
	defer p.mu.Unlock()
	pinned, stored, readErr := p.readPin(ctx)
	st := evaluate(pinned, stored, readErr, observed, scanErr)
	if st.State != HostKeyUnpinned {
		return st, pinned
	}
	// Trust on first use: nothing stored and a successful scan.
	if err := p.st.SetSetting(ctx, p.settingKey, FormatAuthorizedKeys(observed)); err != nil {
		st.Error = fmt.Sprintf("pinning the observed host key failed: %v", err)
		return st, nil
	}
	fps := Fingerprints(observed)
	return HostKeyStatus{State: HostKeyPinned, Pinned: fps, Observed: fps}, observed
}

// readPin loads and parses the stored pin. stored reports whether a
// non-blank row exists, which is what separates "unpinned" from "pinned but
// unreadable".
func (p *HostKeyPin) readPin(ctx context.Context) (keys []ssh.PublicKey, stored bool, err error) {
	text, err := p.st.GetString(ctx, p.settingKey, "")
	if err != nil {
		return nil, false, fmt.Errorf("reading the pinned host key from lab's settings: %w", err)
	}
	if strings.TrimSpace(text) == "" {
		return nil, false, nil
	}
	keys, err = ParseAuthorizedKeys(text)
	if err == nil && len(keys) == 0 {
		err = errors.New("the stored pin holds no key")
	}
	return keys, true, err
}

// evaluate is the pure comparison: no I/O, no writes.
func evaluate(pinned []ssh.PublicKey, stored bool, readErr error, observed []ssh.PublicKey, scanErr error) HostKeyStatus {
	st := HostKeyStatus{Pinned: Fingerprints(pinned), Observed: []string{}}
	switch {
	case scanErr != nil:
		st.State, st.Error = HostKeyUnreachable, scanErr.Error()
		return st
	case readErr != nil && !stored:
		// The settings read itself failed: whether a pin exists is unknown,
		// so neither TOFU nor a comparison is possible. Observed stays empty,
		// as it does for every unreachable status.
		st.State, st.Error = HostKeyUnreachable, readErr.Error()
		return st
	case readErr != nil:
		st.State = HostKeyMismatch
		st.Error = fmt.Sprintf("the stored host key pin is unreadable (%v); accept one of the observed fingerprints to replace it", readErr)
	case !stored:
		st.State = HostKeyUnpinned
	case sameKeySet(pinned, observed):
		st.State = HostKeyPinned
	default:
		st.State = HostKeyMismatch
	}
	st.Observed = Fingerprints(observed)
	return st
}

// KnownHosts checks the listener and, when it presents exactly the pinned
// keys, returns the known_hosts text a run gets: the PINNED keys under the
// configured address's host pattern. Every other outcome is an error that
// tells the operator what to do; a target-bearing spawn is refused on it.
func (p *HostKeyPin) KnownHosts(ctx context.Context) (string, error) {
	st, pinned := p.check(ctx)
	switch st.State {
	case HostKeyPinned:
		return KnownHostsLines(p.addr, pinned), nil
	case HostKeyMismatch:
		detail := ""
		if st.Error != "" {
			detail = " (" + st.Error + ")"
		}
		return "", fmt.Errorf("warpgate: the SSH host key(s) presented at %s [%s] do not match lab's pin [%s]%s; refusing to wire SSH targets. If Warpgate's host key was changed on purpose, verify one of the presented fingerprints out of band (e.g. ssh-keyscan -p <port> <addr> | ssh-keygen -lf - run on the Warpgate host itself) and accept it with POST /api/v1/warpgate/host-key/accept {\"fingerprint\":\"SHA256:…\"}",
			p.addr, strings.Join(st.Observed, ", "), strings.Join(st.Pinned, ", "), detail)
	case HostKeyUnreachable:
		return "", fmt.Errorf("warpgate: cannot verify the SSH host key at %s (is --warpgate-ssh-addr right and Warpgate's SSH listener enabled?): %s", p.addr, st.Error)
	default:
		return "", fmt.Errorf("warpgate: no SSH host key is pinned for %s: %s", p.addr, st.Error)
	}
}

// Accept replaces the pin with the listener's CURRENT key set, provided
// fingerprint (an operator-verified "SHA256:…") is one of the keys it
// presents right now. The whole observed set is pinned — the fingerprint is
// the operator's proof that the listener is the real one, and the pin holds
// every key it offers (see hostKeyAlgorithms).
//
// Nothing is stored on any failure: an empty fingerprint, a failed scan
// (State unreachable), a fingerprint not observed (ErrFingerprintNotObserved,
// with the current comparison as the status), or a failed write.
func (p *HostKeyPin) Accept(ctx context.Context, fingerprint string) (HostKeyStatus, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return HostKeyStatus{}, errors.New("warpgate: a host key fingerprint (SHA256:…) is required")
	}
	observed, scanErr := p.scan(ctx, p.addr)

	p.mu.Lock()
	defer p.mu.Unlock()
	pinned, stored, readErr := p.readPin(ctx)
	current := evaluate(pinned, stored, readErr, observed, scanErr)
	if scanErr != nil {
		return current, fmt.Errorf("warpgate: cannot scan the SSH listener at %s to accept its host key: %w", p.addr, scanErr)
	}
	if !slices.Contains(current.Observed, fingerprint) {
		return current, fmt.Errorf("warpgate: %w: %s is not among [%s] presented at %s right now; re-check the Warpgate health status and verify again", ErrFingerprintNotObserved, fingerprint, strings.Join(current.Observed, ", "), p.addr)
	}
	if err := p.st.SetSetting(ctx, p.settingKey, FormatAuthorizedKeys(observed)); err != nil {
		return current, fmt.Errorf("warpgate: storing the accepted host key pin: %w", err)
	}
	return HostKeyStatus{State: HostKeyPinned, Pinned: current.Observed, Observed: current.Observed}, nil
}
