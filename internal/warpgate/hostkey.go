package warpgate

// Warpgate's SSH host key, as a run trusts it (ADR-0068 decision 4). A run
// reaches its targets through the bastion with StrictHostKeyChecking on and
// a known_hosts file lab writes — never accept-new inside a run — so lab has
// to know the bastion's host key before the run needs it. Warpgate exposes no
// endpoint for it (its host keys live in its database), so lab reads it the
// way ssh-keyscan does: one SSH handshake per host-key algorithm, aborted the
// moment the server has proven possession of its key and before any
// authentication.
//
// What a run then trusts is the operator's choice. With
// --warpgate-ssh-host-key set, that key (or keys) is the pin: every run's
// known_hosts is rendered from it verbatim, and a listener presenting none of
// the configured keys refuses target-bearing spawns — a man in the middle
// between lab and the bastion can at most block spawns, never get a run to
// trust its key. With the setting unset, lab accepts every key the listener
// presents: each spawn's scan is what its run trusts. Health reports the
// observed fingerprints either way, so the setting can be filled in from
// them.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
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

// HostKeyState is the outcome of comparing the listener's keys to what lab
// trusts.
type HostKeyState string

const (
	// HostKeyUnpinned: no trusted key is configured (--warpgate-ssh-host-key
	// unset), so a run trusts whatever the listener presented to the scan
	// that wired it. Observed lists those keys; Pinned is empty.
	HostKeyUnpinned HostKeyState = "unpinned"
	// HostKeyPinned: a trusted key is configured and the listener presents
	// at least one of the configured keys.
	HostKeyPinned HostKeyState = "pinned"
	// HostKeyMismatch: a trusted key is configured and the listener presents
	// none of the configured keys. Target-bearing spawns are refused until
	// --warpgate-ssh-host-key names a key the listener presents.
	HostKeyMismatch HostKeyState = "mismatch"
	// HostKeyUnreachable: the listener could not be scanned. Error says why.
	HostKeyUnreachable HostKeyState = "unreachable"
)

// HostKeyStatus is one check's result, shaped for the health endpoint.
type HostKeyStatus struct {
	State    HostKeyState
	Pinned   []string // fingerprints of the configured trusted keys (empty when none is configured)
	Observed []string // fingerprints of the last scan (empty when unreachable)
	Error    string   // why the state is not simply pinned/unpinned, when there is a reason
}

// HostKeyPin decides which host keys a run trusts for Warpgate's SSH
// listener at addr:
//
//   - With trusted keys configured (--warpgate-ssh-host-key), the run's
//     known_hosts is rendered from THOSE keys, verbatim, and a scan that
//     presents none of them refuses to wire a run. Nothing the listener says
//     can widen what a run trusts.
//   - With none configured, the run's known_hosts is rendered from the keys
//     the listener presents to the scan at spawn: every key it offers is
//     accepted. The operator chose that by leaving the setting unset; health
//     reports the observed fingerprints so the setting can be filled in.
//
// It holds no state and is safe for concurrent use: every call scans.
type HostKeyPin struct {
	addr    string
	trusted []ssh.PublicKey
	scan    func(ctx context.Context, addr string) ([]ssh.PublicKey, error)
}

// NewHostKeyPin returns the pin for the SSH listener at addr. trusted is the
// parsed --warpgate-ssh-host-key (nil or empty when unset). scan is
// ScanHostKeys when nil; tests inject their own.
func NewHostKeyPin(addr string, trusted []ssh.PublicKey, scan func(ctx context.Context, addr string) ([]ssh.PublicKey, error)) *HostKeyPin {
	if scan == nil {
		scan = ScanHostKeys
	}
	return &HostKeyPin{addr: addr, trusted: slices.Clone(trusted), scan: scan}
}

// Pinned reports whether a trusted key is configured.
func (p *HostKeyPin) Pinned() bool {
	return len(p.trusted) > 0
}

// Check scans the listener and compares its keys to the trusted set. It
// always scans; there is no cached answer, because the scan is also the SSH
// listener's reachability probe.
func (p *HostKeyPin) Check(ctx context.Context) HostKeyStatus {
	st, _ := p.check(ctx)
	return st
}

// check is Check that also returns the keys a run's known_hosts is rendered
// from: the trusted keys when configured, else the observed ones.
func (p *HostKeyPin) check(ctx context.Context) (HostKeyStatus, []ssh.PublicKey) {
	observed, scanErr := p.scan(ctx, p.addr)
	st := evaluate(p.trusted, observed, scanErr)
	switch st.State {
	case HostKeyPinned:
		return st, p.trusted
	case HostKeyUnpinned:
		return st, observed
	default:
		return st, nil
	}
}

// evaluate is the pure comparison: no I/O.
func evaluate(trusted, observed []ssh.PublicKey, scanErr error) HostKeyStatus {
	st := HostKeyStatus{Pinned: Fingerprints(trusted), Observed: []string{}}
	if scanErr != nil {
		st.State, st.Error = HostKeyUnreachable, scanErr.Error()
		return st
	}
	st.Observed = Fingerprints(observed)
	switch {
	case len(trusted) == 0:
		st.State = HostKeyUnpinned
	case anyKeyIn(trusted, observed):
		st.State = HostKeyPinned
	default:
		st.State = HostKeyMismatch
	}
	return st
}

// anyKeyIn reports whether at least one of want is among have, compared by
// wire encoding.
func anyKeyIn(want, have []ssh.PublicKey) bool {
	set := make(map[string]bool, len(have))
	for _, k := range have {
		set[string(k.Marshal())] = true
	}
	for _, k := range want {
		if set[string(k.Marshal())] {
			return true
		}
	}
	return false
}

// KnownHosts checks the listener and returns the known_hosts text a run gets
// under the configured address's host pattern: the trusted keys when
// --warpgate-ssh-host-key is set and the listener presents one of them, else
// — with no trusted key configured — every key the listener presented. Every
// other outcome is an error that tells the operator what to do; a
// target-bearing spawn is refused on it.
func (p *HostKeyPin) KnownHosts(ctx context.Context) (string, error) {
	st, keys := p.check(ctx)
	switch st.State {
	case HostKeyPinned, HostKeyUnpinned:
		return KnownHostsLines(p.addr, keys), nil
	case HostKeyMismatch:
		return "", fmt.Errorf("warpgate: the SSH host key(s) presented at %s [%s] are not among the trusted keys configured with --warpgate-ssh-host-key [%s]; refusing to wire SSH targets. If Warpgate's host key was changed on purpose, put the new key in --warpgate-ssh-host-key (ssh-keyscan -p <port> <addr> run on the Warpgate host itself prints it) and restart lab",
			p.addr, strings.Join(st.Observed, ", "), strings.Join(st.Pinned, ", "))
	default:
		return "", fmt.Errorf("warpgate: cannot verify the SSH host key at %s (is --warpgate-ssh-addr right and Warpgate's SSH listener enabled?): %s", p.addr, st.Error)
	}
}
