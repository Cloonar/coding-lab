package vault

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/fsx"
)

// Materialized-file suffixes under the runtime dir, keyed per
// (credential, op): <credID>.<opID>.key for SSH private keys,
// <credID>.<opID>.askpass for HTTPS askpass helpers, and
// <credID>.<opID>.sshpass for SSH key-passphrase helpers. Filenames are
// per-op so concurrent ops sharing one credential can never unlink each
// other's files (each op cleans up exactly its own). CleanupAll touches
// only these plus stale atomic-write temp files; anything else in
// runtime/ (e.g. known_hosts) is never removed by this package.
const (
	sshKeySuffix  = ".key"
	askpassSuffix = ".askpass"
	sshPassSuffix = ".sshpass"
)

// staleTempMaxAge is how old an atomic-write temp file must be before
// CleanupAll reaps it as a crash orphan. Atomic writes complete in
// milliseconds, so anything older was orphaned by a kill between
// CreateTemp and rename; the age guard keeps a sweep from racing an
// in-flight write.
const staleTempMaxAge = 5 * time.Minute

// credFileMinAge is how old a materialized credential file must be before
// CleanupAll's keep-set is allowed to reap it. It guards the mid-op window
// a keep-set cannot see: a credential is materialized at the start of a
// clone or instance Start (opID = repo/run id) but its run only enters the
// live keep-set once the tmux session is up — after the AddWorktree fetch
// (up to gitTimeout, 60s) plus seeding and spawn. Without this guard a
// throttled sweep firing in that window would unlink the key out from
// under an in-flight fetch or a just-spawned live agent, stranding it with
// a dangling GIT_SSH_COMMAND/GIT_ASKPASS. 5 minutes is comfortably above
// that window; a genuine orphan (its op ended without calling Cleanup) is
// still reaped on the next sweep once it ages out. Normal op-end cleanup
// is immediate via Cleanup(credID, opID) — this only bounds the belt-and-
// suspenders GC.
const credFileMinAge = 5 * time.Minute

// Materializer writes decrypted GIT credentials under the runtime dir
// (<state>/runtime, 0700 — design §6/§7) so git subprocesses can
// authenticate without secrets in argv, and removes them again per the
// cleanup rules. Forge tokens are never materialized (design §3a).
type Materializer struct {
	dir string
}

// NewMaterializer ensures dir exists with mode 0700 (tightening an
// existing looser dir) and returns a Materializer over it.
func NewMaterializer(dir string) (*Materializer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("runtime dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("runtime dir: %w", err)
	}
	return &Materializer{dir: dir}, nil
}

// Dir returns the runtime directory.
func (m *Materializer) Dir() string { return m.dir }

// KnownHostsPath returns <dir>/known_hosts — the pinned
// UserKnownHostsFile of design §6's GIT_SSH_COMMAND. The file is owned
// by ssh (populated via StrictHostKeyChecking=accept-new); CleanupAll
// never touches it.
func (m *Materializer) KnownHostsPath() string { return filepath.Join(m.dir, "known_hosts") }

// SeedKnownHosts copies the known_hosts file at src into this Materializer's
// dir (mode 0600). It exists for PER-RUN runtime dirs (issue #205): each run
// gets a fresh runtime dir, which would otherwise reset ssh's accept-new
// TOFU state on every launch — every run would silently re-accept every host
// key from scratch. Seeding from the global runtime dir's known_hosts (which
// clone/fetch ops keep accumulating) preserves previously pinned host keys,
// while pins accepted DURING a run stay confined to that run's file and die
// with its tree — deliberately, so one run's accepted key never widens
// another's trust. A missing src is a no-op (nil): a fresh install has
// nothing to seed. A plain (non-atomic) write suffices — the file is written
// once at launch, before any ssh of this run could read it.
func (m *Materializer) SeedKnownHosts(src string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("seed known_hosts: %w", err)
	}
	if err := os.WriteFile(m.KnownHostsPath(), b, 0o600); err != nil {
		return fmt.Errorf("seed known_hosts: %w", err)
	}
	return nil
}

func (m *Materializer) sshKeyPath(credID, opID string) string {
	return filepath.Join(m.dir, credID+"."+opID+sshKeySuffix)
}

func (m *Materializer) askpassPath(credID, opID string) string {
	return filepath.Join(m.dir, credID+"."+opID+askpassSuffix)
}

func (m *Materializer) sshPassPath(credID, opID string) string {
	return filepath.Join(m.dir, credID+"."+opID+sshPassSuffix)
}

// checkNameParts validates the components of the <credID>.<opID>.<suffix>
// filename grammar: both must be non-empty and free of the separator dot,
// path separators, and NUL. IDs from internal/ids always satisfy this;
// the check keeps hand-fed values from escaping the runtime dir or
// corrupting the grammar.
func checkNameParts(credID, opID string) error {
	if credID == "" || strings.ContainsAny(credID, "./\\\x00") {
		return errors.New("materialize: invalid credential ID for runtime filename")
	}
	if opID == "" || strings.ContainsAny(opID, "./\\\x00") {
		return errors.New("materialize: invalid op ID for runtime filename")
	}
	return nil
}

// MaterializeSSHKey writes the credential's private key to
// <dir>/<credID>.<opID>.key, mode 0600, normalizing CRLF/CR line endings
// to LF (private keys are line-oriented ASCII armor; a Windows-pasted
// key's \r bytes make OpenSSH reject it) and guaranteeing a single
// trailing newline (OpenSSH rejects PEM keys without one). When the
// payload carries a passphrase it also writes an SSH askpass helper to
// <dir>/<credID>.<opID>.sshpass, mode 0700, echoing the passphrase — the
// design D9 wiring that lets a TTY-less ssh load an encrypted key; wire
// it via SSHEnvWithPassphrase. It returns the key path and the helper
// path ("" when the key has no passphrase).
//
// opID keys the files per-op (callers pass the repo ID for clone ops) so
// concurrent ops sharing a credential cannot unlink each other's files;
// Cleanup(credID, opID) removes exactly this op's files.
// Re-materializing the same (credID, opID) atomically replaces them.
func (m *Materializer) MaterializeSSHKey(credID, opID string, p SSHKeyPayload) (keyPath, sshAskpassPath string, err error) {
	if err := checkNameParts(credID, opID); err != nil {
		return "", "", err
	}
	key := strings.ReplaceAll(p.PrivateKey, "\r\n", "\n")
	key = strings.ReplaceAll(key, "\r", "\n")
	if !strings.HasSuffix(key, "\n") {
		key += "\n"
	}
	keyPath = m.sshKeyPath(credID, opID)
	if err := fsx.WriteFileAtomic(keyPath, []byte(key), 0o600); err != nil {
		return "", "", fmt.Errorf("materialize ssh key for %s: %w", credID, err)
	}
	if p.Passphrase == "" {
		return keyPath, "", nil
	}
	script := "#!/bin/sh\n" +
		"# Generated by lab (vault); answers ssh passphrase prompts. Do not edit.\n" +
		"printf '%s\\n' " + shellQuote(p.Passphrase) + "\n"
	sshAskpassPath = m.sshPassPath(credID, opID)
	if err := fsx.WriteFileAtomic(sshAskpassPath, []byte(script), 0o700); err != nil {
		_ = os.Remove(keyPath) // no partial op state on error
		return "", "", fmt.Errorf("materialize ssh passphrase helper for %s: %w", credID, err)
	}
	return keyPath, sshAskpassPath, nil
}

// MaterializeAskpass writes a GIT_ASKPASS helper script to
// <dir>/<credID>.<opID>.askpass, mode 0700, and returns the path. The
// script answers git's "Username for …" / "Password for …" prompts on
// stdout. opID keys the file per-op — see MaterializeSSHKey.
//
// Chosen variant (design §6 allows either): the username and token are
// embedded in the script body rather than an adjacent 0600 file — one
// file per credential means no partial-cleanup states, and the 0700
// script body is owner-only, which the design explicitly accepts. The
// token therefore never appears in any process's argv: git invokes the
// script with only the prompt text as its argument.
func (m *Materializer) MaterializeAskpass(credID, opID string, p HTTPSTokenPayload) (string, error) {
	if err := checkNameParts(credID, opID); err != nil {
		return "", err
	}
	script := "#!/bin/sh\n" +
		"# Generated by lab (vault); answers git credential prompts. Do not edit.\n" +
		"case \"$1\" in\n" +
		"Username*) printf '%s\\n' " + shellQuote(p.Username) + " ;;\n" +
		"Password*) printf '%s\\n' " + shellQuote(p.Token) + " ;;\n" +
		"*) exit 1 ;;\n" +
		"esac\n"
	path := m.askpassPath(credID, opID)
	if err := fsx.WriteFileAtomic(path, []byte(script), 0o700); err != nil {
		return "", fmt.Errorf("materialize askpass for %s: %w", credID, err)
	}
	return path, nil
}

// shellQuote single-quotes s for /bin/sh, escaping any embedded single
// quote as quote-backslash-quote-quote so the value reaches printf
// verbatim regardless of spaces, $, backticks, or quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Cleanup removes one op's materialized files for the credential (key,
// askpass, and ssh-passphrase helper); missing files are not an error.
// Only the named op's files are touched — concurrent ops sharing the
// credential keep theirs. Callers invoke it at the design §6 lifecycle
// points: op end for per-op files, session teardown for per-session
// files.
func (m *Materializer) Cleanup(credID, opID string) error {
	if err := checkNameParts(credID, opID); err != nil {
		return err
	}
	var errs []error
	for _, path := range []string{
		m.sshKeyPath(credID, opID),
		m.askpassPath(credID, opID),
		m.sshPassPath(credID, opID),
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CleanupAll enumerates the materialized credential files (*.key,
// *.askpass, *.sshpass) and removes every one for which keep(filename)
// is false — the design §6 restart rule: keep exactly the files
// referenced by re-adopted live runs, unlink the rest; the throttled
// sweep repeats this. keep receives the base filename (e.g.
// "cred_ab12….op_cd34….key"); the keep-set is per (credID, opID) — use
// CredIDFromFile and OpIDFromFile to recover the parts. A nil keep
// removes all materialized credential files.
//
// A not-kept credential file younger than credFileMinAge is left in
// place: it may belong to an in-flight clone/Start whose run has not yet
// entered the keep-set (its opID cannot be a live run id until the
// session is up). It is reaped by a later sweep once it ages out; normal
// op-end removal is immediate via Cleanup(credID, opID).
//
// It also reaps stale atomic-write temp files (".<name>.tmp-…" older
// than staleTempMaxAge): a kill between temp write and rename orphans a
// temp file holding secret bytes that no Cleanup path would otherwise
// ever see. Non-credential files (e.g. known_hosts) are never touched.
func (m *Materializer) CleanupAll(keep func(filename string) bool) error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return fmt.Errorf("runtime dir: %w", err)
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if isMaterializeTempName(name) {
			info, ierr := e.Info()
			if ierr != nil || time.Since(info.ModTime()) < staleTempMaxAge {
				continue // possibly an in-flight atomic write
			}
			if err := os.Remove(filepath.Join(m.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if _, ok := CredIDFromFile(name); !ok {
			continue
		}
		if keep != nil && keep(name) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || time.Since(info.ModTime()) < credFileMinAge {
			continue // in-flight op: materialized, but its run isn't in the keep-set yet
		}
		if err := os.Remove(filepath.Join(m.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// isMaterializeTempName reports whether name matches fsx.WriteFileAtomic's
// temp pattern for a materialized credential file: ".<base>.tmp-<rand>"
// where base carries one of the credential suffixes. Kept next to the
// suffix constants so the two stay in sync.
func isMaterializeTempName(name string) bool {
	if !strings.HasPrefix(name, ".") {
		return false
	}
	for _, suffix := range []string{sshKeySuffix, askpassSuffix, sshPassSuffix} {
		if strings.Contains(name, suffix+".tmp-") {
			return true
		}
	}
	return false
}

// splitMaterializedName parses a materialized credential filename
// (<credID>.<opID>.<suffix>) into its parts. Temp files and other
// runtime files do not parse.
func splitMaterializedName(filename string) (credID, opID string, ok bool) {
	if strings.HasPrefix(filename, ".") {
		return "", "", false // fsx.WriteFileAtomic temp file
	}
	for _, suffix := range []string{sshKeySuffix, askpassSuffix, sshPassSuffix} {
		rest, found := strings.CutSuffix(filename, suffix)
		if !found {
			continue
		}
		credID, opID, found = strings.Cut(rest, ".")
		if !found || credID == "" || opID == "" || strings.Contains(opID, ".") {
			return "", "", false
		}
		return credID, opID, true
	}
	return "", "", false
}

// CredIDFromFile reports the credential ID a materialized filename
// belongs to ("cred_ab12….op_cd34….key" → "cred_ab12…") and whether the
// name is a materialized credential file at all (temp files and other
// runtime files are not).
func CredIDFromFile(filename string) (string, bool) {
	credID, _, ok := splitMaterializedName(filename)
	return credID, ok
}

// OpIDFromFile reports the op ID a materialized filename belongs to
// ("cred_ab12….op_cd34….key" → "op_cd34…") and whether the name is a
// materialized credential file at all.
func OpIDFromFile(filename string) (string, bool) {
	_, opID, ok := splitMaterializedName(filename)
	return opID, ok
}

// SSHEnv returns the exact design §6 git environment for an SSH
// credential: GIT_SSH_COMMAND pinning the materialized key,
// IdentitiesOnly, the runtime known_hosts file, and accept-new host-key
// checking. The same env goes to spawned sessions (the GIT credential
// only — never forge_token, design §3a). For passphrase-protected keys
// use SSHEnvWithPassphrase.
func SSHEnv(keyPath, knownHostsPath string) []string {
	return []string{
		"GIT_SSH_COMMAND=ssh -i " + keyPath +
			" -o IdentitiesOnly=yes" +
			" -o UserKnownHostsFile=" + knownHostsPath +
			" -o StrictHostKeyChecking=accept-new",
	}
}

// SSHEnvWithPassphrase returns SSHEnv plus the passphrase wiring for an
// encrypted private key (design D9): SSH_ASKPASS pointing at the
// materialized passphrase helper, SSH_ASKPASS_REQUIRE=force so OpenSSH
// (>= 8.4) consults it even with a controlling terminal, and DISPLAY=:0
// — a harmless dummy that lets older OpenSSH (< 8.4, which only uses
// SSH_ASKPASS when DISPLAY is set and no TTY is present) fall back to
// the helper too.
func SSHEnvWithPassphrase(keyPath, knownHostsPath, sshAskpassPath string) []string {
	return append(SSHEnv(keyPath, knownHostsPath),
		"SSH_ASKPASS="+sshAskpassPath,
		"SSH_ASKPASS_REQUIRE=force",
		"DISPLAY=:0",
	)
}

// HTTPSEnv returns the exact design §6 git environment for an HTTPS
// token credential: GIT_ASKPASS pointing at the materialized helper.
// Username and token flow via the helper's stdout — never argv, URLs,
// repo config, or logs.
func HTTPSEnv(askpassPath string) []string {
	return []string{"GIT_ASKPASS=" + askpassPath}
}
