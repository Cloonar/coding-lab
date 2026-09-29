package instance

// The Warpgate SSH bastion's PURE core (issue #39 / ADR-0068), pinned exactly:
// bastion.go's header explains why these outputs are the contract — the
// ssh_config OpenSSH reads, the wrappers a run's PATH resolves ssh through,
// the alias rule that keeps the forge out of the bastion, the key a run
// authenticates with — so they are compared byte for byte here, not by
// substring. The launch-level coverage (what a spawn actually produced, and
// what a refusal left behind) is next door in bastion_launch_test.go.
//
// The last two tests leave the table: they EXECUTE the generated wrapper
// against a fake ssh, and — when this machine has OpenSSH — ask the real ssh
// how it resolves the generated config (`ssh -G`), because "OpenSSH reads
// this file the way we think it does" is the one claim no string comparison
// can make.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/warpgate"
)

// The fixed identities the pure tests render with. testBastionRunID has the
// exact ids.NewID("run") shape, so warpgate.RunIDFromLabel accepts its label.
const (
	testBastionRunID = "run_0123456789abcdef0123456789abcdef"
	testBastionUser  = "repo-fedcba9876543210fedcba9876543210"
	testBastionAddr  = "10.88.0.1:2222"
)

// testBastionPaths is a run's path set under a fixed, realistic state layout
// — the paths are rendered, never touched, by the renderer tests.
var testBastionPaths = newBastionPaths("/var/lib/lab/instances/"+testBastionRunID+"/runtime", "/var/lib/lab/instances/"+testBastionRunID+"/home")

func targets(names ...string) []warpgate.Target {
	out := make([]warpgate.Target, 0, len(names))
	for i, n := range names {
		out = append(out, warpgate.Target{ID: "t" + string(rune('a'+i)), Name: n})
	}
	return out
}

// --- the alias rule ----------------------------------------------------------

// ADR-0068's alias rule: a target becomes a `Host` alias only when its name is
// alias-safe AND is not the repo's forge host, case-insensitively — the
// forge-host exclusion is what keeps `git push` (GIT_SSH_COMMAND's bare ssh,
// which resolves to the wrapper on a wired run) off the bastion. Kept aliases
// come back sorted and deduplicated; skipped names come back in input order
// so the warning reads like Warpgate's own listing.
func TestBastionAliases(t *testing.T) {
	cases := []struct {
		name        string
		targets     []warpgate.Target
		forge       string
		wantAliases []string
		wantSkipped []string
	}{
		{
			name:        "valid names are kept, sorted",
			targets:     targets("staging", "build-box", "db_1.internal", "A1"),
			forge:       "git.example.com",
			wantAliases: []string{"A1", "build-box", "db_1.internal", "staging"},
		},
		{
			// The load-bearing case: a target named like the forge host would
			// reroute the run's own git through the bastion. OpenSSH matches Host
			// patterns case-insensitively, so the exclusion must too.
			name:        "the forge host is excluded, case-insensitively",
			targets:     targets("github.com", "GitHub.com", "staging", "github"),
			forge:       "github.com",
			wantAliases: []string{"github", "staging"},
			wantSkipped: []string{"github.com", "GitHub.com"},
		},
		{
			name:        "no forge host (a local-path remote) excludes nothing",
			targets:     targets("github.com"),
			forge:       "",
			wantAliases: []string{"github.com"},
		},
		{
			// Each excluded character is a hazard: whitespace and globs turn a
			// Host line into a pattern, % meets ssh_config token expansion in
			// User, # becomes Warpgate's selector separator, : splits the
			// selector, and a leading dot/hyphen is not a hostname.
			name: "names that cannot be aliases are skipped",
			targets: targets("bad name", "tab\tname", "hash#name", "pct%name", "glob*", "q?",
				"!neg", "a,b", "colon:x", ".dot", "-dash", "", "ok"),
			forge:       "git.example.com",
			wantAliases: []string{"ok"},
			wantSkipped: []string{"bad name", "tab\tname", "hash#name", "pct%name", "glob*", "q?",
				"!neg", "a,b", "colon:x", ".dot", "-dash", ""},
		},
		{
			// Two blocks OpenSSH treats as the same host: the second would be
			// dead text. The byte-smaller name wins, on every spawn.
			name:        "case-insensitive and exact duplicates are kept once",
			targets:     targets("staging", "Staging", "staging"),
			wantAliases: []string{"Staging"},
			wantSkipped: []string{"staging", "staging"},
		},
		{
			name:        "every target skipped",
			targets:     targets("bad name", "GitHub.COM"),
			forge:       "github.com",
			wantSkipped: []string{"bad name", "GitHub.COM"},
		},
		{
			name: "no targets",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aliases, skipped := bastionAliases(tc.targets, tc.forge)
			if !slices.Equal(aliases, tc.wantAliases) {
				t.Errorf("aliases = %q, want %q", aliases, tc.wantAliases)
			}
			if !slices.Equal(skipped, tc.wantSkipped) {
				t.Errorf("skipped = %q, want %q", skipped, tc.wantSkipped)
			}
		})
	}
}

// --- the ssh_config ----------------------------------------------------------

// wantBastionBlock is one alias's Host block as ADR-0068 prints it, for the
// fixed test paths.
func wantBastionBlock(alias string) string {
	return "\nHost " + alias + "\n" +
		"  HostName 10.88.0.1\n" +
		"  Port 2222\n" +
		"  User " + testBastionUser + ":" + alias + "\n" +
		"  IdentityFile \"/var/lib/lab/instances/" + testBastionRunID + "/runtime/warpgate-run-key\"\n" +
		"  IdentitiesOnly yes\n" +
		"  UserKnownHostsFile \"/var/lib/lab/instances/" + testBastionRunID + "/runtime/warpgate-known-hosts\"\n" +
		"  GlobalKnownHostsFile /dev/null\n" +
		"  StrictHostKeyChecking yes\n" +
		"  UpdateHostKeys no\n" +
		"  PasswordAuthentication no\n" +
		"  KbdInteractiveAuthentication no\n"
}

const wantBastionHeader = "# Generated by lab for run " + testBastionRunID + ": SSH targets reached through the Warpgate bastion (ADR-0068).\n" +
	"# Rewritten at every spawn; edits are lost.\n"

const wantBastionTailComment = "\n# -F (passed by this run's ssh/scp/sftp wrappers) replaces OpenSSH's default per-user and system\n" +
	"# config lookup; restore it for every other host, the forge included.\n" +
	"Match all\n"

// The whole file, exactly, for both runners: the header, one block per alias
// in the given (sorted) order, then `Match all` carrying ONLY the Includes —
// the per-user one for a host run (the service user's config, quoted), and
// the system one always.
func TestRenderBastionSSHConfig_exactShape(t *testing.T) {
	aliases := []string{"build-box", "staging"}

	t.Run("host run re-includes the service user's config, then the system one", func(t *testing.T) {
		got, err := renderBastionSSHConfig(bastionConfigSpec{
			runID: testBastionRunID, username: testBastionUser, addr: testBastionAddr,
			aliases: aliases, paths: testBastionPaths, userConfig: "/var/lib/lab/.ssh/config",
		})
		if err != nil {
			t.Fatalf("renderBastionSSHConfig: %v", err)
		}
		want := wantBastionHeader + wantBastionBlock("build-box") + wantBastionBlock("staging") +
			wantBastionTailComment +
			"Include \"/var/lib/lab/.ssh/config\"\n" +
			"Include /etc/ssh/ssh_config\n"
		if got != want {
			t.Errorf("config =\n%s\nwant\n%s", got, want)
		}
	})

	t.Run("container run (no per-user config) includes only the system one", func(t *testing.T) {
		got, err := renderBastionSSHConfig(bastionConfigSpec{
			runID: testBastionRunID, username: testBastionUser, addr: testBastionAddr,
			aliases: aliases, paths: testBastionPaths,
		})
		if err != nil {
			t.Fatalf("renderBastionSSHConfig: %v", err)
		}
		want := wantBastionHeader + wantBastionBlock("build-box") + wantBastionBlock("staging") +
			wantBastionTailComment +
			"Include /etc/ssh/ssh_config\n"
		if got != want {
			t.Errorf("config =\n%s\nwant\n%s", got, want)
		}
	})
}

// The invariants ADR-0068 states as "never", checked structurally so they
// survive any rewording of the file: no directive of lab's applies to every
// host (no `Host *`, and `Match all` carries nothing but Includes), and
// accept-new is never written.
func TestRenderBastionSSHConfig_neverAppliesToEveryHost(t *testing.T) {
	got, err := renderBastionSSHConfig(bastionConfigSpec{
		runID: testBastionRunID, username: testBastionUser, addr: testBastionAddr,
		aliases: []string{"staging"}, paths: testBastionPaths, userConfig: "/var/lib/lab/.ssh/config",
	})
	if err != nil {
		t.Fatalf("renderBastionSSHConfig: %v", err)
	}
	if strings.Contains(got, "accept-new") {
		t.Error("the run's config writes accept-new")
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	inMatchAll := false
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if strings.EqualFold(fields[0], "Host") && slices.Contains(fields[1:], "*") {
			t.Errorf("wildcard Host block: %q", line)
		}
		if strings.EqualFold(fields[0], "Match") {
			inMatchAll = true
			continue
		}
		if inMatchAll && !strings.EqualFold(fields[0], "Include") {
			t.Errorf("directive %q applies to every host under Match all; only Includes may", line)
		}
	}
	if !inMatchAll {
		t.Error("no Match all block restores the default lookup")
	}
}

// A bracketed IPv6 --warpgate-ssh-addr comes back bare for HostName, which is
// the form ssh_config takes.
func TestRenderBastionSSHConfig_ipv6Addr(t *testing.T) {
	got, err := renderBastionSSHConfig(bastionConfigSpec{
		runID: testBastionRunID, username: testBastionUser, addr: "[fd00::1]:2222",
		aliases: []string{"staging"}, paths: testBastionPaths,
	})
	if err != nil {
		t.Fatalf("renderBastionSSHConfig: %v", err)
	}
	if !strings.Contains(got, "  HostName fd00::1\n  Port 2222\n") {
		t.Errorf("IPv6 address rendered wrong:\n%s", got)
	}
}

// Every refusal: the renderer never writes a block ssh would read differently
// from what lab meant, and never a half-formed one.
func TestRenderBastionSSHConfig_refusals(t *testing.T) {
	good := bastionConfigSpec{
		runID: testBastionRunID, username: testBastionUser, addr: testBastionAddr,
		aliases: []string{"staging"}, paths: testBastionPaths, userConfig: "/var/lib/lab/.ssh/config",
	}
	withPaths := func(runtime string) bastionPaths { return newBastionPaths(runtime, "/h") }
	cases := []struct {
		name string
		mut  func(*bastionConfigSpec)
		want string
	}{
		{"address without a port", func(s *bastionConfigSpec) { s.addr = "10.88.0.1" }, "not host:port"},
		{"address with a bad port", func(s *bastionConfigSpec) { s.addr = "10.88.0.1:0" }, "port in 1-65535"},
		{"address with an empty host", func(s *bastionConfigSpec) { s.addr = ":2222" }, "port in 1-65535"},
		{"empty username", func(s *bastionConfigSpec) { s.username = "" }, "User line"},
		{"username with a selector separator", func(s *bastionConfigSpec) { s.username = "repo#x" }, "User line"},
		{"no aliases", func(s *bastionConfigSpec) { s.aliases = nil }, "no SSH target alias"},
		{"an unsafe alias", func(s *bastionConfigSpec) { s.aliases = []string{"a b"} }, "not a valid ssh_config alias"},
		{"relative runtime dir", func(s *bastionConfigSpec) { s.paths = withPaths("state/runtime") }, "absolute path"},
		{"a double quote in the path", func(s *bastionConfigSpec) { s.paths = withPaths(`/st"ate/runtime`) }, "choose a --state-dir"},
		{"a backslash in the path", func(s *bastionConfigSpec) { s.paths = withPaths(`/st\ate/runtime`) }, "choose a --state-dir"},
		{"a percent token in the path", func(s *bastionConfigSpec) { s.paths = withPaths("/st%date/runtime") }, "choose a --state-dir"},
		{"an env reference in the path", func(s *bastionConfigSpec) { s.paths = withPaths("/st${HOME}/runtime") }, "choose a --state-dir"},
		{"a newline in the path", func(s *bastionConfigSpec) { s.paths = withPaths("/st\nate/runtime") }, "choose a --state-dir"},
		{"a glob in the per-user config", func(s *bastionConfigSpec) { s.userConfig = "/home/l*b/.ssh/config" }, "glob character"},
		{"a relative per-user config", func(s *bastionConfigSpec) { s.userConfig = ".ssh/config" }, "absolute path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := good
			tc.mut(&spec)
			got, err := renderBastionSSHConfig(spec)
			if err == nil {
				t.Fatalf("rendered\n%s\nwant a refusal", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
	// Spaces are legal: the quoting carries them.
	spec := good
	spec.paths = withPaths("/srv/lab state/runtime")
	got, err := renderBastionSSHConfig(spec)
	if err != nil {
		t.Fatalf("a path with a space was refused: %v", err)
	}
	if !strings.Contains(got, `IdentityFile "/srv/lab state/runtime/warpgate-run-key"`) {
		t.Errorf("space-bearing path not quoted:\n%s", got)
	}
}

// The per-user Include is omitted exactly when it would be a self-Include
// (a hard OpenSSH error) — the run's runtime config, or the ~/.ssh/config link
// that will point at it — including through symlinks, and including BEFORE
// either file exists (the decision is made before anything is written).
func TestBastionUserInclude(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "instances", "run_x", "runtime")
	home := filepath.Join(root, "instances", "run_x", "home")
	for _, d := range []string{runtimeDir, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p := newBastionPaths(runtimeDir, home)

	// A service-user .ssh whose config is a dangling symlink to the run's
	// runtime config (which does not exist yet).
	linked := filepath.Join(root, "svc-linked", ".ssh")
	if err := os.MkdirAll(linked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(p.config, filepath.Join(linked, "config")); err != nil {
		t.Fatal(err)
	}
	// A symlinked directory that IS the run's home, so <link>/.ssh/config is
	// the run's ~/.ssh/config even though .ssh does not exist yet.
	homeAlias := filepath.Join(root, "home-alias")
	if err := os.Symlink(home, homeAlias); err != nil {
		t.Fatal(err)
	}
	// An ordinary per-user config that exists.
	real := filepath.Join(root, "svc", ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, real, "Host forge\n  Port 2200\n")

	cases := []struct {
		name, userConfig, want string
	}{
		{"unknown home", "", ""},
		{"an ordinary existing config is included", real, real},
		{"a missing config is still included (ssh ignores it)", filepath.Join(root, "nobody", ".ssh", "config"), filepath.Join(root, "nobody", ".ssh", "config")},
		{"the run's own runtime config", p.config, ""},
		{"the run's ~/.ssh/config link path", p.homeConfig, ""},
		{"a dangling symlink to the run's config", filepath.Join(linked, "config"), ""},
		{"the run's ~/.ssh/config through a symlinked home", filepath.Join(homeAlias, ".ssh", "config"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bastionUserInclude(tc.userConfig, p); got != tc.want {
				t.Errorf("bastionUserInclude(%q) = %q, want %q", tc.userConfig, got, tc.want)
			}
		})
	}
}

// --- the wrappers -------------------------------------------------------------

// The wrapper, exactly (ADR-0068 decision 7b), for each of the three tools.
func TestRenderBastionWrapper(t *testing.T) {
	want := func(tool string) string {
		return "#!/bin/sh\n" +
			"# Generated by lab for run " + testBastionRunID + " (ADR-0068): runs the real " + tool + " with this run's Warpgate\n" +
			"# bastion config. OpenSSH ignores $HOME when locating ~/.ssh/config, so the config is passed with -F.\n" +
			"set -f\n" +
			"self='/var/lib/lab/instances/" + testBastionRunID + "/runtime/warpgate-bin'\n" +
			"IFS=:\n" +
			"for d in $PATH; do\n" +
			"  [ -n \"$d\" ] && [ \"${d%/}\" != \"$self\" ] || continue\n" +
			"  if [ -f \"$d/" + tool + "\" ] && [ -x \"$d/" + tool + "\" ]; then\n" +
			"    unset IFS\n" +
			"    exec \"$d/" + tool + "\" -F '/var/lib/lab/instances/" + testBastionRunID + "/runtime/warpgate-ssh-config' \"$@\"\n" +
			"  fi\n" +
			"done\n" +
			"echo \"" + tool + ": no " + tool + " binary on PATH besides lab's bastion wrapper\" >&2\n" +
			"exit 127\n"
	}
	for _, tool := range bastionWrappedTools {
		got, err := renderBastionWrapper(tool, testBastionRunID, testBastionPaths)
		if err != nil {
			t.Fatalf("renderBastionWrapper(%s): %v", tool, err)
		}
		if got != want(tool) {
			t.Errorf("%s wrapper =\n%s\nwant\n%s", tool, got, want(tool))
		}
	}
	if !slices.Equal(bastionWrappedTools, []string{"ssh", "scp", "sftp"}) {
		t.Errorf("wrapped tools = %q, want ssh, scp and sftp", bastionWrappedTools)
	}
}

// A single quote in a baked path cannot be single-quoted: refused, not
// mis-quoted.
func TestRenderBastionWrapper_refusesASingleQuote(t *testing.T) {
	p := newBastionPaths("/srv/lab's state/runtime", "/h")
	if got, err := renderBastionWrapper("ssh", testBastionRunID, p); err == nil {
		t.Fatalf("rendered\n%s\nwant a refusal", got)
	} else if !strings.Contains(err.Error(), "single quote") {
		t.Errorf("err = %q, want it to name the single quote", err)
	}
	if _, _, err := renderBastionFileSet(bastionConfigSpec{
		runID: testBastionRunID, username: testBastionUser, addr: testBastionAddr,
		aliases: []string{"staging"}, paths: p,
	}); err == nil {
		t.Error("renderBastionFileSet accepted a path its wrappers cannot quote")
	}
}

// --- the run key --------------------------------------------------------------

// The run key is an ed25519 pair whose private half parses back as OpenSSH
// PEM and matches the public half, and whose authorized_keys line carries the
// lab run label as its comment (Warpgate strips it; the credential label is
// what survives — but the comment is set to the same string by design).
func TestGenerateBastionRunKey(t *testing.T) {
	k, err := generateBastionRunKey(testBastionRunID)
	if err != nil {
		t.Fatalf("generateBastionRunKey: %v", err)
	}
	if !bytes.HasPrefix(k.privatePEM, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n")) {
		t.Errorf("private key is not OpenSSH PEM: %.40q", k.privatePEM)
	}
	signer, err := ssh.ParsePrivateKey(k.privatePEM)
	if err != nil {
		t.Fatalf("private key does not parse: %v", err)
	}
	pub, comment, _, rest, err := ssh.ParseAuthorizedKey([]byte(k.authorizedKey))
	if err != nil {
		t.Fatalf("public half %q does not parse: %v", k.authorizedKey, err)
	}
	if len(bytes.TrimSpace(rest)) != 0 || strings.Contains(k.authorizedKey, "\n") {
		t.Errorf("public half %q is not exactly one line", k.authorizedKey)
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		t.Errorf("key type = %s, want ed25519", pub.Type())
	}
	if want := warpgate.RunKeyLabel(testBastionRunID); comment != want {
		t.Errorf("public key comment = %q, want %q", comment, want)
	}
	if id, ok := warpgate.RunIDFromLabel(comment); !ok || id != testBastionRunID {
		t.Errorf("comment %q does not parse back to the run id (%q, %v)", comment, id, ok)
	}
	if !bytes.Equal(signer.PublicKey().Marshal(), pub.Marshal()) {
		t.Error("private and public halves do not match")
	}
	if k.fingerprint != ssh.FingerprintSHA256(pub) {
		t.Errorf("fingerprint = %q, want %q", k.fingerprint, ssh.FingerprintSHA256(pub))
	}
	// Fresh per run: two spawns never share a key.
	k2, err := generateBastionRunKey(testBastionRunID)
	if err != nil {
		t.Fatalf("second generateBastionRunKey: %v", err)
	}
	if k2.authorizedKey == k.authorizedKey || bytes.Equal(k2.privatePEM, k.privatePEM) {
		t.Error("two generated run keys are identical")
	}
}

// --- the files ----------------------------------------------------------------

// The marker round-trips, is 0600, carries exactly two ids, and every broken
// shape reads as an error (a missing one wrapping fs.ErrNotExist — the
// ordinary answer for an unwired run).
func TestBastionMarker(t *testing.T) {
	dir := t.TempDir()
	if _, err := readBastionMarker(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("reading a missing marker: err = %v, want fs.ErrNotExist", err)
	}
	want := bastionMarker{UserID: "wg-user-1", KeyID: "wg-key-1"}
	if err := writeBastionMarker(dir, want); err != nil {
		t.Fatalf("writeBastionMarker: %v", err)
	}
	assertMode(t, filepath.Join(dir, bastionMarkerName), 0o600)
	raw, err := os.ReadFile(filepath.Join(dir, bastionMarkerName))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("marker is not JSON: %v", err)
	}
	if len(fields) != 2 || fields["user_id"] != "wg-user-1" || fields["key_id"] != "wg-key-1" {
		t.Errorf("marker JSON = %s, want exactly {user_id, key_id}", raw)
	}
	got, err := readBastionMarker(dir)
	if err != nil || got != want {
		t.Errorf("readBastionMarker = %+v, %v; want %+v", got, err, want)
	}
	// Written once per run: a second write is refused (O_EXCL), never a
	// silent overwrite.
	if err := writeBastionMarker(dir, want); err == nil {
		t.Error("a second marker write succeeded; want O_EXCL to refuse it")
	}

	for name, body := range map[string]string{
		"not JSON":   "{",
		"no key id":  `{"user_id":"u"}`,
		"no user id": `{"key_id":"k"}`,
	} {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			writeFile(t, filepath.Join(d, bastionMarkerName), body)
			if m, err := readBastionMarker(d); err == nil {
				t.Errorf("readBastionMarker = %+v, want an error", m)
			}
		})
	}
}

// assertMode fails unless path's permission bits are exactly want.
func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Errorf("%s mode = %04o, want %04o", path, got, want)
	}
}

// materializedBastionPaths makes a run's runtime dir and HOME the way
// instancehome.Materialize does (0700) under a temp root.
func materializedBastionPaths(t *testing.T) bastionPaths {
	t.Helper()
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	home := filepath.Join(root, "home")
	for _, d := range []string{runtimeDir, home} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return newBastionPaths(runtimeDir, home)
}

// renderedBastionFiles renders a real config and wrapper set for p.
func renderedBastionFiles(t *testing.T, p bastionPaths, userConfig string, aliases ...string) bastionFiles {
	t.Helper()
	config, wrappers, err := renderBastionFileSet(bastionConfigSpec{
		runID: testBastionRunID, username: testBastionUser, addr: testBastionAddr,
		aliases: aliases, paths: p, userConfig: userConfig,
	})
	if err != nil {
		t.Fatalf("renderBastionFileSet: %v", err)
	}
	return bastionFiles{
		privateKey: []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-real-key\n-----END OPENSSH PRIVATE KEY-----\n"),
		knownHosts: "[10.88.0.1]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPinned\n",
		config:     config,
		wrappers:   wrappers,
	}
}

// ADR-0068's exact modes: 0600 for the key, known_hosts and config, 0700 for
// the wrapper dir and each wrapper, 0700 for ~/.ssh, and ~/.ssh/config a
// symlink to the runtime config by absolute path. Every file is created
// exclusively: a second write into the same tree fails rather than writing
// through whatever is there.
func TestWriteBastionFiles(t *testing.T) {
	p := materializedBastionPaths(t)
	files := renderedBastionFiles(t, p, "", "staging")
	if err := writeBastionFiles(p, files); err != nil {
		t.Fatalf("writeBastionFiles: %v", err)
	}
	for path, mode := range map[string]os.FileMode{
		p.key:                           0o600,
		p.knownHosts:                    0o600,
		p.config:                        0o600,
		p.binDir:                        0o700,
		filepath.Join(p.binDir, "ssh"):  0o700,
		filepath.Join(p.binDir, "scp"):  0o700,
		filepath.Join(p.binDir, "sftp"): 0o700,
		p.homeSSHDir:                    0o700,
	} {
		assertMode(t, path, mode)
	}
	for path, want := range map[string]string{
		p.key:        string(files.privateKey),
		p.knownHosts: files.knownHosts,
		p.config:     files.config,
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	target, err := os.Readlink(p.homeConfig)
	if err != nil {
		t.Fatalf("~/.ssh/config is not a symlink: %v", err)
	}
	if target != p.config || !filepath.IsAbs(target) {
		t.Errorf("~/.ssh/config -> %q, want the absolute runtime config %q", target, p.config)
	}
	if got, _ := os.ReadFile(p.homeConfig); string(got) != files.config {
		t.Error("~/.ssh/config does not resolve to the run's config")
	}
	if err := writeBastionFiles(p, files); err == nil {
		t.Error("a second writeBastionFiles into the same tree succeeded; want O_EXCL to refuse")
	}
}

// --- PATH -----------------------------------------------------------------------

// A host run's PATH is the wrapper dir then lab's own PATH — and never a
// trailing ":" (an empty entry means the current directory). A container's
// prefix goes in front of podmanx.PATH.
func TestBastionPATH(t *testing.T) {
	bin := testBastionPaths.binDir
	if got, want := bastionHostPATH(bin, "/run/current-system/sw/bin:/usr/bin"), bin+":/run/current-system/sw/bin:/usr/bin"; got != want {
		t.Errorf("bastionHostPATH = %q, want %q", got, want)
	}
	if got := bastionHostPATH(bin, ""); got != bin {
		t.Errorf("bastionHostPATH with no base = %q, want the wrapper dir alone %q", got, bin)
	}
	if got, want := bastionContainerPATHPrefix(bin)+podmanx.PATH, bin+":"+podmanx.PATH; got != want {
		t.Errorf("container PATH = %q, want %q", got, want)
	}
}

// containerEnv emits EXACTLY ONE PATH: a PATH arriving in the spawn env is
// dropped (a host-shaped value means nothing inside the image, and a second
// --env PATH would leave the winner to podman's ordering), and the one it
// appends is podmanx.PATH behind the wired run's prefix. With no prefix the
// output is the pre-#39 one, entry for entry.
func TestContainerEnv_bastionPATH(t *testing.T) {
	const hostHome = "/state/instances/run_1/home"
	const sockURL = "unix:///state/agent/agent.sock"
	bin := "/state/instances/run_1/runtime/warpgate-bin"
	spawnEnv := []string{
		"LAB_URL=http://127.0.0.1:8080",
		"LAB_TOKEN=lab_run_secret",
		"HOME=" + hostHome,
		"PATH=" + bin + ":/run/current-system/sw/bin",
	}

	env, forward := containerEnv(spawnEnv, hostHome, sockURL, bastionContainerPATHPrefix(bin))
	want := []string{"LAB_URL=" + sockURL, "HOME=" + podmanx.Home, "PATH=" + bin + ":" + podmanx.PATH}
	if !slices.Equal(env, want) {
		t.Errorf("wired env =\n  %q\nwant\n  %q", env, want)
	}
	if !slices.Equal(forward, []string{"LAB_TOKEN", "TERM"}) {
		t.Errorf("forward = %q, want [LAB_TOKEN TERM] — PATH is never forwarded", forward)
	}

	env, _ = containerEnv(spawnEnv, hostHome, sockURL, "")
	if want := []string{"LAB_URL=" + sockURL, "HOME=" + podmanx.Home, "PATH=" + podmanx.PATH}; !slices.Equal(env, want) {
		t.Errorf("unwired env =\n  %q\nwant\n  %q", env, want)
	}
}

// A typed nil in an interface-typed Options field is normalized back to the
// nil interface, so bastionActive reads "off" rather than "configured" — the
// nil-interface trap the Options doc warns about. Fakes pass through.
func TestNormalizeBastion(t *testing.T) {
	var nilClient *warpgate.Client
	var nilPin *warpgate.HostKeyPin
	api, keys := normalizeBastion(nilClient, nilPin)
	if api != nil || keys != nil {
		t.Errorf("typed nils survived normalization: api %v, keys %v", api, keys)
	}
	stub, pin := newBastionStub("repo_x"), &hostKeysStub{}
	api, keys = normalizeBastion(stub, pin)
	if api != BastionAPI(stub) || keys != BastionHostKeys(pin) {
		t.Error("normalization replaced a non-nil seam")
	}
	s := &Service{warpgate: api, warpgateSSHAddr: testBastionAddr, warpgateHostKeys: keys}
	if !s.bastionActive() {
		t.Error("bastionActive false with all three halves set")
	}
	for name, s := range map[string]*Service{
		"no client":   {warpgateSSHAddr: testBastionAddr, warpgateHostKeys: keys},
		"no address":  {warpgate: api, warpgateHostKeys: keys},
		"no host pin": {warpgate: api, warpgateSSHAddr: testBastionAddr},
	} {
		if s.bastionActive() {
			t.Errorf("bastionActive true with %s", name)
		}
	}
}

// The per-user config a host run re-includes is the PASSWD home's
// .ssh/config (or "" when unknown) — never derived from anything relative.
func TestServiceUserSSHConfig(t *testing.T) {
	got := serviceUserSSHConfig()
	if got != "" && (!filepath.IsAbs(got) || !strings.HasSuffix(got, string(filepath.Separator)+filepath.Join(".ssh", "config"))) {
		t.Errorf("serviceUserSSHConfig() = %q, want an absolute <home>/.ssh/config or \"\"", got)
	}
}

// --- end to end: the wrappers execute, and OpenSSH reads the config ---------

// writeFakeTool drops an executable that prints its own name and then each
// argument on its own line, standing in for the real ssh/scp/sftp.
func writeFakeTool(t *testing.T, dir, tool string) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' 'fake-" + tool + "'\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n"
	if err := os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The generated wrappers, EXECUTED: each finds the real binary on PATH past
// its own dir and execs it with -F <config> first and the caller's arguments
// after, verbatim (an argument with spaces stays one argument). A trailing
// slash on the wrapper dir's PATH entry does not make it find itself, and with
// no real binary on PATH it exits 127 with a message.
func TestBastionWrapper_executes(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh on this machine")
	}
	p := materializedBastionPaths(t)
	if err := writeBastionFiles(p, renderedBastionFiles(t, p, "", "staging")); err != nil {
		t.Fatalf("writeBastionFiles: %v", err)
	}
	fakeDir := t.TempDir()
	for _, tool := range bastionWrappedTools {
		writeFakeTool(t, fakeDir, tool)
	}

	run := func(tool, path string, args ...string) (string, string, error) {
		cmd := exec.Command(filepath.Join(p.binDir, tool), args...)
		cmd.Env = []string{"PATH=" + path}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}

	for _, tool := range bastionWrappedTools {
		for _, path := range []string{p.binDir + ":" + fakeDir, p.binDir + "/:" + fakeDir, "::" + p.binDir + ":" + fakeDir} {
			out, stderr, err := run(tool, path, "staging", "echo hello world")
			if err != nil {
				t.Fatalf("%s wrapper with PATH=%s: %v (stderr %q)", tool, path, err, stderr)
			}
			want := "fake-" + tool + "\n-F\n" + p.config + "\nstaging\necho hello world\n"
			if out != want {
				t.Errorf("%s wrapper with PATH=%s exec'd\n%q\nwant\n%q", tool, path, out, want)
			}
		}
	}

	_, stderr, err := run("ssh", p.binDir, "staging")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 127 {
		t.Fatalf("wrapper with no real ssh on PATH: err = %v, want exit 127", err)
	}
	if !strings.Contains(stderr, "no ssh binary on PATH besides lab's bastion wrapper") {
		t.Errorf("stderr = %q, want the no-binary message", stderr)
	}
}

// sshG runs the real `ssh -G -F config host` and returns its settings as a
// key → values map (ssh -G prints lowercase keys, one setting per line).
func sshG(t *testing.T, sshBin, config, host string) map[string][]string {
	t.Helper()
	cmd := exec.Command(sshBin, "-G", "-F", config, host)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "No user exists") {
			t.Skipf("ssh -G cannot run as this uid: %s", out)
		}
		t.Fatalf("ssh -G -F %s %s: %v\n%s", config, host, err, out)
	}
	settings := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, " ")
		if ok {
			settings[k] = append(settings[k], v)
		}
	}
	return settings
}

// The real OpenSSH reads the generated config the way ADR-0068 says it does:
// an alias resolves to the bastion with the target selector as the user, the
// run key, the pinned known_hosts and strict checking; the service user's own
// config is restored for every other host through the per-user Include under
// `Match all`; and the forge host resolves to itself, untouched. Skipped when
// this machine has no ssh (CI images may not).
func TestBastionSSHConfig_resolvesWithOpenSSH(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh on PATH")
	}
	p := materializedBastionPaths(t)
	// The service user's own config, which a host run's ssh read before it
	// was wired, and which must still apply to non-alias hosts.
	userConfig := filepath.Join(t.TempDir(), ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(userConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userConfig, []byte("Host forge.example.test\n  Port 2200\n  User forge-user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeBastionFiles(p, renderedBastionFiles(t, p, bastionUserInclude(userConfig, p), "build-box", "staging")); err != nil {
		t.Fatalf("writeBastionFiles: %v", err)
	}

	for _, alias := range []string{"staging", "build-box"} {
		got := sshG(t, sshBin, p.config, alias)
		for key, want := range map[string]string{
			"hostname":               "10.88.0.1",
			"port":                   "2222",
			"user":                   testBastionUser + ":" + alias,
			"identitiesonly":         "yes",
			"stricthostkeychecking":  "true",
			"passwordauthentication": "no",
		} {
			if len(got[key]) == 0 || got[key][0] != want {
				t.Errorf("ssh -G %s: %s = %q, want %q", alias, key, got[key], want)
			}
		}
		if !slices.Contains(got["identityfile"], p.key) {
			t.Errorf("ssh -G %s: identityfile = %q, want it to include the run key %q", alias, got["identityfile"], p.key)
		}
		if !slices.Contains(got["userknownhostsfile"], p.knownHosts) {
			t.Errorf("ssh -G %s: userknownhostsfile = %q, want the pinned %q", alias, got["userknownhostsfile"], p.knownHosts)
		}
	}

	// A non-alias host gets the service user's own config back.
	forge := sshG(t, sshBin, p.config, "forge.example.test")
	if forge["port"][0] != "2200" || forge["user"][0] != "forge-user" {
		t.Errorf("per-user config not restored for a non-alias host: port %q user %q", forge["port"], forge["user"])
	}
	// The forge host resolves to itself: none of the bastion's settings leak.
	gh := sshG(t, sshBin, p.config, "github.com")
	if gh["hostname"][0] != "github.com" {
		t.Errorf("ssh -G github.com: hostname = %q, want github.com", gh["hostname"])
	}
	if strings.Contains(gh["user"][0], testBastionUser) || slices.Contains(gh["identityfile"], p.key) ||
		slices.Contains(gh["userknownhostsfile"], p.knownHosts) {
		t.Errorf("the bastion's settings leaked onto github.com: user %q identityfile %q userknownhostsfile %q",
			gh["user"], gh["identityfile"], gh["userknownhostsfile"])
	}
	// Through the ~/.ssh/config link the same file is read.
	if got := sshG(t, sshBin, p.homeConfig, "staging"); got["hostname"][0] != "10.88.0.1" {
		t.Errorf("ssh -G through ~/.ssh/config: hostname = %q", got["hostname"])
	}
}
