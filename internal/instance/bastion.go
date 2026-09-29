package instance

// bastion.go is the PURE core of the Warpgate SSH-bastion run wiring (issue
// #39 / ADR-0068) — gateway.go's sibling, split off from the launch sequence
// for the same reason: everything here is a function over strings, paths and
// a handful of file writes, and none of it reads a Service field, dials
// Warpgate, or logs. The launch path (prepareBastion / wireBastion in
// launch.go) and the revocation path (bastion_revoke.go) call these; the
// parts whose EXACT output is the contract — the ssh_config text, the three
// wrapper scripts, the alias filter, the PATH values — live where a table
// test can pin them byte for byte.
//
// The property the whole file serves is ADR-0068's first pin: a run reaches
// an operator-defined SSH target without ever holding that target's
// credential. What it does hold — the run key generated here — opens nothing
// but Warpgate, only the targets its repo's role carries, and only until the
// run is wiped. So the private key is the one secret this file handles, and
// it is treated the way gateway.go treats the proxy token: it is written to
// exactly one 0600 file and never enters an error string, a log line, or a
// return value other than the bytes handed to that write.
//
// # Why the files look the way they do (ADR-0068 "Addressing", verified on OpenSSH 10.5p1)
//
// OpenSSH locates the per-user config through the PASSWD entry's home
// directory, not $HOME — a host run reads the lab service user's own
// ~/.ssh/config and a --userns=keep-id container reads none (its generated
// passwd home is "/") — and `ssh -o Include=…` is refused outright. A config
// in the instance HOME is therefore invisible to ssh. So the run's config
// lives in its runtime dir and is handed to ssh with -F by per-run
// ssh/scp/sftp wrappers PREPENDED to the run's PATH; because -F replaces
// BOTH the per-user and the system config, the config ends by including them
// again under `Match all`, so every host that is not an alias — the forge
// first among them — resolves exactly as it did without the wrapper. The
// instance HOME gets a ~/.ssh/config symlink to the same file, for
// discoverability and for libraries that read $HOME/.ssh/config themselves.
//
// Every path baked into the config and the wrappers is an absolute
// RUNTIME-dir path: the runtime dir is bind-mounted into a container run at
// its host-identical path, so the same string resolves to the same file on
// both sides of the container boundary (the instance HOME does not — inside a
// container it is /home/agent, ADR-0052).

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"

	"git.cloonar.com/Cloonar/coding-lab/internal/warpgate"
)

// The basenames a wired run gets (ADR-0068 "The files a wired run gets").
// Fixed rather than run-derived, like trustBundleName: every one of them
// already lives in a per-run directory, so a per-run basename would only make
// the paths harder to recognize in an argv, a strace or an operator's `ls`.
// The "warpgate-" prefix keeps them apart from the vault's own files in the
// same runtime dir (known_hosts, cred.* — ADR-0006), which a run's git uses
// and which this wiring must never shadow.
const (
	// bastionKeyName is the run key's PRIVATE half, OpenSSH PEM, 0600 —
	// OpenSSH refuses an identity file others can read.
	bastionKeyName = "warpgate-run-key"
	// bastionKnownHostsName pins Warpgate's SSH host key(s), rendered from
	// lab's stored pin (never from a fresh scan), 0600.
	bastionKnownHostsName = "warpgate-known-hosts"
	// bastionConfigName is THE per-run OpenSSH client config, 0600: the file
	// the wrappers pass with -F and ~/.ssh/config links to.
	bastionConfigName = "warpgate-ssh-config"
	// bastionBinDirName holds the ssh/scp/sftp wrappers, 0700, and is the
	// directory a wired run's PATH is prefixed with.
	bastionBinDirName = "warpgate-bin"
	// bastionMarkerName is the revocation marker, 0600: which Warpgate user
	// holds this run's key and under which credential id. No secret in it —
	// it is written IMMEDIATELY after the key is registered so that every
	// path that wipes the run's tree can revoke the key first
	// (RevokeBastionKey, through instancehome's pre-wipe hook).
	bastionMarkerName = "warpgate-run-key.json"
)

// bastionSystemConfig is the system-wide client config the run's config
// re-includes: -F replaced OpenSSH's own read of it. The path is the one
// every OpenSSH build this lab meets was compiled with (NixOS, Debian, Fedora
// and Alpine images alike); an Include of a file that does not exist is a
// silent no-op, so an image without one loses nothing.
const bastionSystemConfig = "/etc/ssh/ssh_config"

// bastionWrappedTools are the binaries a wired run gets a wrapper for, in the
// order they are written. ssh because everything that runs "ssh" by name —
// rsync -e ssh, git's GIT_SSH_COMMAND, ansible — resolves it through PATH;
// scp and sftp because they launch ssh by a compiled-in ABSOLUTE path, never
// through PATH, so only a wrapper of their own can hand them -F.
var bastionWrappedTools = []string{"ssh", "scp", "sftp"}

// bastionAliasPattern is the shape a Warpgate target name must have to become
// a `Host` alias (ADR-0068): an alphanumeric first character, then
// alphanumerics, dots, underscores and hyphens. Everything it keeps out is a
// hazard somewhere on the path: whitespace and glob characters (`*`, `?`,
// `!`, `,`) would turn one `Host` line into a pattern matching hosts nobody
// named; `%` would meet ssh_config's own token expansion in the `User` line;
// `#` would become Warpgate's selector separator instead of `:` and route the
// session to the wrong target; `:` would do the same today.
var bastionAliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// bastionPaths is every absolute path the wiring touches for one run,
// derived once from the run's runtime dir and instance HOME so the renderers
// and the writer can never disagree about where a file lives.
type bastionPaths struct {
	runtimeDir string
	key        string // <runtime>/warpgate-run-key
	knownHosts string // <runtime>/warpgate-known-hosts
	config     string // <runtime>/warpgate-ssh-config
	binDir     string // <runtime>/warpgate-bin
	marker     string // <runtime>/warpgate-run-key.json
	homeSSHDir string // <home>/.ssh
	homeConfig string // <home>/.ssh/config → symlink to config
}

// newBastionPaths derives a run's bastionPaths from its runtime dir
// (instancehome.RuntimePath) and instance HOME (instancehome.HomePath).
func newBastionPaths(runtimeDir, home string) bastionPaths {
	return bastionPaths{
		runtimeDir: runtimeDir,
		key:        filepath.Join(runtimeDir, bastionKeyName),
		knownHosts: filepath.Join(runtimeDir, bastionKnownHostsName),
		config:     filepath.Join(runtimeDir, bastionConfigName),
		binDir:     filepath.Join(runtimeDir, bastionBinDirName),
		marker:     filepath.Join(runtimeDir, bastionMarkerName),
		homeSSHDir: filepath.Join(home, ".ssh"),
		homeConfig: filepath.Join(home, ".ssh", "config"),
	}
}

// bastionAliases filters a repo's SSH targets (warpgate.RoleSSHTargets — the
// FRESH read, never the lab-side cache) down to the names that may become
// `Host` aliases, returning the kept aliases sorted and deduplicated and the
// skipped target names in input order. A target is skipped when:
//
//   - its name does not match bastionAliasPattern (see there for why each
//     excluded character is a hazard);
//   - its name equals the repo's forge host, case-insensitively. This is
//     ADR-0068's git rule, and it is load-bearing rather than tidy: on a wired
//     run GIT_SSH_COMMAND's bare `ssh` resolves to lab's wrapper and so reads
//     the run's config, and a block named like the forge would reroute
//     `git push` through the bastion as a Warpgate user with no such target —
//     breaking the run's own git. Case-insensitively, because OpenSSH matches
//     `Host` patterns case-insensitively: a target named "GitHub.com" shadows
//     github.com exactly as "github.com" would. forge is forgeHost(remote),
//     and "" (a repo cloned from a local path) excludes nothing;
//   - its name duplicates, case-insensitively, one already kept: OpenSSH
//     would apply the FIRST matching block and the second would be dead text
//     in the file, naming a target the run can never reach.
//
// Skipping is never an error — the caller logs one warning naming the
// skipped targets, and a repo whose targets are all skipped spawns unwired.
// Renaming the target in Warpgate is the fix.
func bastionAliases(targets []warpgate.Target, forge string) (aliases, skipped []string) {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.Name)
	}
	// Sort first so the case-insensitive dedupe keeps the same one of two
	// colliding names on every spawn, whatever order Warpgate listed them in.
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	seen := make(map[string]bool, len(sorted))
	keep := make(map[string]bool, len(sorted))
	for _, name := range sorted {
		lower := strings.ToLower(name)
		if !bastionAliasPattern.MatchString(name) || (forge != "" && strings.EqualFold(name, forge)) || seen[lower] {
			continue
		}
		seen[lower] = true
		keep[name] = true
		aliases = append(aliases, name)
	}
	for _, name := range names {
		if !keep[name] {
			skipped = append(skipped, name)
			continue
		}
		// A byte-identical duplicate is kept once and skipped after that, so
		// the warning still names it.
		delete(keep, name)
	}
	return aliases, skipped
}

// bastionConfigSpec is everything renderBastionSSHConfig needs. Every field is
// a name, an address, or a path — the private key never reaches a renderer.
type bastionConfigSpec struct {
	runID    string
	username string   // the repo's Warpgate username (warpgate.RepoSlug) — the left half of every selector
	addr     string   // --warpgate-ssh-addr, host:port as a RUN dials it
	aliases  []string // bastionAliases' output: validated, sorted, deduplicated
	paths    bastionPaths
	// userConfig is the per-user config to re-include, or "" to omit the line
	// — bastionUserInclude's answer for a host run, always "" for a container
	// run (ADR-0068: inside a keep-id container the passwd home is "/", so ssh
	// never read a per-user config there).
	userConfig string
}

// renderBastionSSHConfig renders a wired run's OpenSSH client config in
// ADR-0068's exact shape: a header, one `Host` block per alias in the given
// (sorted) order, then the restoration of OpenSSH's default lookup.
//
// Each block's last five directives each close one door: GlobalKnownHostsFile
// /dev/null — no system file can vouch for the bastion; StrictHostKeyChecking
// yes — nothing outside the pin is ever accepted (accept-new is never
// written: inside a run every connection would be a first use); UpdateHostKeys
// no — the server cannot push keys into the pinned file; and the two
// authentication lines — a failed key fails fast instead of prompting in a
// pane nobody watches.
//
// The tail is `Match all` plus the Includes and NOTHING ELSE: a directive of
// lab's that applied to every host would apply the bastion's settings to the
// forge too, which is why there is no `Host *` block, ever. `Match all` itself
// is load-bearing — an Include placed after a `Host` block would otherwise be
// scoped to that block. The per-user Include (quoted, it is a derived path)
// comes first because that is the order OpenSSH reads the two files in
// without -F; the system one is a fixed path and is left unquoted, exactly as
// ADR-0068 prints it.
//
// Refuses — rather than renders a file ssh would read differently — an
// address that is not host:port, a username or alias that could not appear on
// a directive line verbatim, and any path checkSSHConfigPath rejects.
func renderBastionSSHConfig(spec bastionConfigSpec) (string, error) {
	host, port, err := splitBastionAddr(spec.addr)
	if err != nil {
		return "", err
	}
	if spec.username == "" || !bastionAliasPattern.MatchString(spec.username) {
		return "", fmt.Errorf("warpgate: the repo's Warpgate username %q cannot be written into an ssh_config User line", spec.username)
	}
	if len(spec.aliases) == 0 {
		return "", errors.New("warpgate: no SSH target alias to write — a run with no alias is not wired")
	}
	for _, p := range []string{spec.paths.key, spec.paths.knownHosts} {
		if err := checkSSHConfigPath(p); err != nil {
			return "", err
		}
	}
	if spec.userConfig != "" {
		if err := checkSSHConfigPath(spec.userConfig); err != nil {
			return "", err
		}
		if strings.ContainsAny(spec.userConfig, "*?[") {
			return "", fmt.Errorf("warpgate: the per-user ssh config path %q contains a glob character, which an ssh_config Include would expand", spec.userConfig)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Generated by lab for run %s: SSH targets reached through the Warpgate bastion (ADR-0068).\n", spec.runID)
	b.WriteString("# Rewritten at every spawn; edits are lost.\n")
	for _, alias := range spec.aliases {
		if !bastionAliasPattern.MatchString(alias) {
			return "", fmt.Errorf("warpgate: SSH target %q is not a valid ssh_config alias", alias)
		}
		fmt.Fprintf(&b, "\nHost %s\n", alias)
		fmt.Fprintf(&b, "  HostName %s\n", host)
		fmt.Fprintf(&b, "  Port %s\n", port)
		fmt.Fprintf(&b, "  User %s:%s\n", spec.username, alias)
		fmt.Fprintf(&b, "  IdentityFile \"%s\"\n", spec.paths.key)
		b.WriteString("  IdentitiesOnly yes\n")
		fmt.Fprintf(&b, "  UserKnownHostsFile \"%s\"\n", spec.paths.knownHosts)
		b.WriteString("  GlobalKnownHostsFile /dev/null\n")
		b.WriteString("  StrictHostKeyChecking yes\n")
		b.WriteString("  UpdateHostKeys no\n")
		b.WriteString("  PasswordAuthentication no\n")
		b.WriteString("  KbdInteractiveAuthentication no\n")
	}
	b.WriteString("\n# -F (passed by this run's ssh/scp/sftp wrappers) replaces OpenSSH's default per-user and system\n")
	b.WriteString("# config lookup; restore it for every other host, the forge included.\n")
	b.WriteString("Match all\n")
	if spec.userConfig != "" {
		fmt.Fprintf(&b, "Include \"%s\"\n", spec.userConfig)
	}
	b.WriteString("Include " + bastionSystemConfig + "\n")
	return b.String(), nil
}

// splitBastionAddr splits --warpgate-ssh-addr into the HostName and Port an
// ssh_config block carries. The config layer validated the shape already
// (validateSSHAddr); this re-check exists because the renderer must never
// write a half-formed block, and a bracketed IPv6 literal comes back bare,
// which is what `HostName` takes.
func splitBastionAddr(addr string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", "", fmt.Errorf("warpgate: --warpgate-ssh-addr %q is not host:port: %w", addr, err)
	}
	n, perr := strconv.Atoi(port)
	if host == "" || perr != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("warpgate: --warpgate-ssh-addr %q is not host:port with a port in 1-65535", addr)
	}
	if strings.ContainsAny(host, " \t\"'%#") {
		return "", "", fmt.Errorf("warpgate: --warpgate-ssh-addr %q has a host that cannot be written into an ssh_config HostName line", addr)
	}
	return host, strconv.Itoa(n), nil
}

// checkSSHConfigPath refuses a path that cannot be written into an ssh_config
// directive and read back as the same path. Every path lab writes lives under
// <state>/instances/run_<hex>/runtime, so none of these fire unless the
// operator's --state-dir (or the service user's home) carries one of the
// characters — and then a loud refusal naming the path beats a config that
// silently points ssh somewhere else:
//
//   - not absolute: ssh resolves a relative IdentityFile against ~/.ssh, not
//     against the runtime dir;
//   - `"` or `\`: the quoting and escaping ssh_config's own argument splitter
//     applies;
//   - `%` or `$`: IdentityFile and UserKnownHostsFile expand %-tokens and
//     ${ENV} references;
//   - control characters: a newline would end the directive mid-path.
func checkSSHConfigPath(p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("warpgate: %q must be an absolute path to be written into the run's ssh_config", p)
	}
	if strings.ContainsAny(p, "\"\\%$") || strings.ContainsFunc(p, isControl) {
		return fmt.Errorf("warpgate: the path %q contains a character (one of \" \\ %% $ or a control character) that ssh_config would quote, escape or expand — choose a --state-dir (or a service-user home) without it", p)
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// bastionUserInclude decides the per-user Include of a HOST run's config: the
// lab service user's own ~/.ssh/config (userConfig — its passwd home plus
// /.ssh/config, the very file a host run's ssh read before it was wired), or
// "" to omit the line. Omitted when userConfig is "" (the home is unknown) or
// when it resolves — symlinks followed — to the run's own config: a
// self-Include is a hard OpenSSH error ("Too many recursive configuration
// includes"), while an Include of a missing file is a silent no-op. Both the
// runtime config and the ~/.ssh/config link that will point at it count as
// "the run's own", and the check runs BEFORE either exists, which is why it
// resolves paths with canonicalPath rather than filepath.EvalSymlinks alone
// (EvalSymlinks fails on anything missing).
//
// Container runs never call this: inside a keep-id container ssh never read
// a per-user config, so there is nothing to restore.
func bastionUserInclude(userConfig string, p bastionPaths) string {
	if userConfig == "" {
		return ""
	}
	got := canonicalPath(userConfig)
	if got == canonicalPath(p.config) || got == canonicalPath(p.homeConfig) {
		return ""
	}
	return userConfig
}

// canonicalPath resolves every symlink along p that exists — the longest
// existing ancestor directory through filepath.EvalSymlinks, a final-component
// symlink by reading it even when its target does not exist yet — bounded at
// 40 hops like the kernel's own ELOOP limit. Components that do not exist yet
// are kept as written (a missing entry is not a symlink), so two paths compare
// equal exactly when they would name the same file once the missing parts are
// created.
func canonicalPath(p string) string {
	p = filepath.Clean(p)
	for range 40 {
		p = resolveParentDir(p)
		target, err := os.Readlink(p)
		if err != nil {
			return p // not a symlink, or missing: nothing further to follow
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(p), target)
		}
		p = filepath.Clean(target)
	}
	return p
}

// resolveParentDir resolves the symlinks in p's longest existing ancestor
// directory and re-appends the rest verbatim. filepath.EvalSymlinks alone
// fails on any path with a missing component — a run's ~/.ssh does not exist
// yet when bastionUserInclude asks — which would leave a symlinked parent
// above it unresolved.
func resolveParentDir(p string) string {
	dir, rest := filepath.Dir(p), filepath.Base(p)
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return p
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// renderBastionWrapper renders one of a wired run's PATH wrappers (tool is
// "ssh", "scp" or "sftp"; ADR-0068 decision 7b): POSIX sh that finds the REAL
// binary by scanning PATH — skipping the wrapper's own dir, so it works with
// the host's ssh and with whatever ssh the dev image ships — and execs it
// with `-F <config>` ahead of the caller's arguments. No real binary is exit
// 127 with a message, the shell's own "command not found" status.
//
// The two paths are baked in single-quoted, which makes every character but
// `'` literal; a path containing `'` is refused rather than escaped, because
// the realistic source of one is an unusual --state-dir and a refusal naming
// it is clearer than a script nobody can read. `set -f` keeps the unquoted
// $PATH split from globbing, and `${d%/}` also skips the wrapper dir when a
// tool re-exported PATH with a trailing slash on it — without it the wrapper
// would find itself and exec itself forever.
func renderBastionWrapper(tool, runID string, p bastionPaths) (string, error) {
	for _, path := range []string{p.binDir, p.config} {
		if strings.Contains(path, "'") {
			return "", fmt.Errorf("warpgate: the path %q contains a single quote, which the run's %s wrapper cannot quote — choose a --state-dir without it", path, tool)
		}
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "# Generated by lab for run %s (ADR-0068): runs the real %s with this run's Warpgate\n", runID, tool)
	b.WriteString("# bastion config. OpenSSH ignores $HOME when locating ~/.ssh/config, so the config is passed with -F.\n")
	b.WriteString("set -f\n")
	fmt.Fprintf(&b, "self='%s'\n", p.binDir)
	b.WriteString("IFS=:\n")
	b.WriteString("for d in $PATH; do\n")
	b.WriteString("  [ -n \"$d\" ] && [ \"${d%/}\" != \"$self\" ] || continue\n")
	fmt.Fprintf(&b, "  if [ -f \"$d/%s\" ] && [ -x \"$d/%s\" ]; then\n", tool, tool)
	b.WriteString("    unset IFS\n")
	fmt.Fprintf(&b, "    exec \"$d/%s\" -F '%s' \"$@\"\n", tool, p.config)
	b.WriteString("  fi\n")
	b.WriteString("done\n")
	fmt.Fprintf(&b, "echo \"%s: no %s binary on PATH besides lab's bastion wrapper\" >&2\n", tool, tool)
	b.WriteString("exit 127\n")
	return b.String(), nil
}

// bastionRunKey is a freshly generated run key: the private half as OpenSSH
// PEM (SECRET — it goes to exactly one 0600 file, never anywhere else) and
// the public half as one authorized_keys line.
type bastionRunKey struct {
	privatePEM    []byte
	authorizedKey string
	fingerprint   string // SHA256:… of the public half — public, loggable
}

// generateBastionRunKey mints a run's ed25519 key pair (ADR-0068: access
// attaches per repo, authentication is per run). Both halves carry the
// comment warpgate.RunKeyLabel(runID): Warpgate strips the comment on
// registration, so the credential's LABEL is what actually carries the run id
// there, but the comment is harmless, survives in the private key file, and
// keeps working if a future Warpgate keeps it.
func generateBastionRunKey(runID string) (bastionRunKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return bastionRunKey{}, fmt.Errorf("warpgate: generating the run key: %w", err)
	}
	label := warpgate.RunKeyLabel(runID)
	block, err := ssh.MarshalPrivateKey(priv, label)
	if err != nil {
		return bastionRunKey{}, fmt.Errorf("warpgate: encoding the run key: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return bastionRunKey{}, fmt.Errorf("warpgate: encoding the run key's public half: %w", err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + label
	return bastionRunKey{
		privatePEM:    pem.EncodeToMemory(block),
		authorizedKey: line,
		fingerprint:   ssh.FingerprintSHA256(sshPub),
	}, nil
}

// bastionMarker is the revocation marker's JSON: where the run's key is
// registered. Two opaque Warpgate ids and nothing else — no key material, no
// label, no address.
type bastionMarker struct {
	UserID string `json:"user_id"`
	KeyID  string `json:"key_id"`
}

// writeBastionMarker writes the marker into the run's runtime dir, 0600. The
// launch path calls it the moment AddPublicKey succeeds and BEFORE any other
// file, so that from then on every path that wipes the run's tree can find
// and revoke the key (RevokeBastionKey).
func writeBastionMarker(runtimeDir string, m bastionMarker) error {
	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("warpgate: encoding the run key marker: %w", err)
	}
	return writeNewFile(filepath.Join(runtimeDir, bastionMarkerName), append(body, '\n'), 0o600)
}

// readBastionMarker reads a run's marker. A missing marker is an error that
// wraps fs.ErrNotExist — the ordinary answer for every unwired run — and a
// marker missing either id is an error too: revoking with half an address
// would 404 at best.
func readBastionMarker(runtimeDir string) (bastionMarker, error) {
	body, err := os.ReadFile(filepath.Join(runtimeDir, bastionMarkerName))
	if err != nil {
		return bastionMarker{}, err
	}
	var m bastionMarker
	if err := json.Unmarshal(body, &m); err != nil {
		return bastionMarker{}, fmt.Errorf("warpgate: the run key marker is not valid JSON: %w", err)
	}
	if m.UserID == "" || m.KeyID == "" {
		return bastionMarker{}, errors.New("warpgate: the run key marker names no user or no key")
	}
	return m, nil
}

// bastionFiles is everything writeBastionFiles puts on disk after the key is
// registered and the marker written: the three rendered texts and the key.
type bastionFiles struct {
	privateKey []byte // SECRET
	knownHosts string
	config     string
	wrappers   map[string]string // tool → script, one per bastionWrappedTools entry
}

// writeBastionFiles writes a wired run's files with ADR-0068's exact modes:
// the private key, the known_hosts and the config (0600) into the runtime dir,
// the wrapper dir (0700) with its three wrappers (0700), then ~/.ssh (0700) in
// the instance HOME with ~/.ssh/config a symlink to the runtime config by
// absolute host path (it resolves inside a container too, because the
// runtime dir is mounted host-identically).
//
// The runtime dir already exists 0700 (instancehome.Materialize) and is not
// created here — as with writeTrustBundle, creating it would let this succeed
// against a path nobody mounts. Every file is created O_EXCL: a fresh run's
// tree holds none of them, so an existing entry is a lab bug (or a planted
// symlink) to fail on, never a file to write through. A failure leaves
// whatever was written behind; the caller wipes the whole tree.
func writeBastionFiles(p bastionPaths, f bastionFiles) error {
	if err := writeNewFile(p.key, f.privateKey, 0o600); err != nil {
		return err
	}
	if err := writeNewFile(p.knownHosts, []byte(f.knownHosts), 0o600); err != nil {
		return err
	}
	if err := writeNewFile(p.config, []byte(f.config), 0o600); err != nil {
		return err
	}
	if err := os.Mkdir(p.binDir, 0o700); err != nil {
		return fmt.Errorf("warpgate: creating the run's wrapper dir: %w", err)
	}
	if err := os.Chmod(p.binDir, 0o700); err != nil {
		return fmt.Errorf("warpgate: creating the run's wrapper dir: %w", err)
	}
	for _, tool := range bastionWrappedTools {
		script, ok := f.wrappers[tool]
		if !ok {
			return fmt.Errorf("warpgate: no %s wrapper was rendered", tool)
		}
		if err := writeNewFile(filepath.Join(p.binDir, tool), []byte(script), 0o700); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(p.homeSSHDir, 0o700); err != nil {
		return fmt.Errorf("warpgate: creating the run's ~/.ssh: %w", err)
	}
	if err := os.Chmod(p.homeSSHDir, 0o700); err != nil {
		return fmt.Errorf("warpgate: creating the run's ~/.ssh: %w", err)
	}
	if err := os.Symlink(p.config, p.homeConfig); err != nil {
		return fmt.Errorf("warpgate: linking the run's ~/.ssh/config: %w", err)
	}
	return nil
}

// writeNewFile creates path exclusively with exactly perm — chmod'ed after
// the write, so the process umask cannot leave a wrapper unexecutable — and
// never follows or overwrites an existing entry (O_EXCL). Errors name the
// path and never the contents.
func writeNewFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("warpgate: writing %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("warpgate: writing %s: %w", path, err)
	}
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return fmt.Errorf("warpgate: writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("warpgate: writing %s: %w", path, err)
	}
	return nil
}

// bastionHostPATH is a wired HOST run's PATH value: the wrapper dir, then
// base — lab's own PATH, the value an unwired pane inherits through tmux's
// baseline allow-list (tmuxx) — so the only names that resolve differently
// are ssh, scp and sftp. An empty base yields the wrapper dir alone rather
// than "<dir>:": a trailing empty PATH entry means the current directory.
func bastionHostPATH(binDir, base string) string {
	if base == "" {
		return binDir
	}
	return binDir + ":" + base
}

// bastionContainerPATHPrefix is what a wired CONTAINER run's PATH gains in
// front of podmanx.PATH (containerEnv appends the one PATH entry, and the
// host's PATH is never forwarded inward). Kept as a
// prefix rather than a whole value so containerEnv stays the single place a
// container PATH is composed, and an unwired run's prefix is simply "".
func bastionContainerPATHPrefix(binDir string) string {
	return binDir + ":"
}

// renderBastionFileSet renders the three texts a wired run gets — config and
// wrappers — for the paths and wiring given. Pulled out of the launch path so
// every refusal it can produce happens BEFORE the key is registered, when
// there is nothing in Warpgate to undo.
func renderBastionFileSet(spec bastionConfigSpec) (config string, wrappers map[string]string, err error) {
	config, err = renderBastionSSHConfig(spec)
	if err != nil {
		return "", nil, err
	}
	wrappers = make(map[string]string, len(bastionWrappedTools))
	for _, tool := range bastionWrappedTools {
		script, err := renderBastionWrapper(tool, spec.runID, spec.paths)
		if err != nil {
			return "", nil, err
		}
		wrappers[tool] = script
	}
	return config, wrappers, nil
}
