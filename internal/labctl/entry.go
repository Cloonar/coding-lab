package labctl

// `labctl container-entry` (issue #114, ADR-0086): the PID 1 of a
// nested-containers run. A nested run is started as container root under
// podman's systemd mode (the only route to a writable, namespaced cgroup), and
// podman refuses `--init` there, so this program takes over two jobs at once:
// the few privileged setup steps that only container root can do, and the
// orphan reaping ADR-0074 gave to catatonit.
//
// The setup (entrySetup) is, in order:
//
//  1. Refuse to run unless uid 0 — the writes and the chown need container
//     root, and a non-root caller would only fail halfway.
//  2. Write /etc/subuid and /etc/subgid with exactly ONE line each for the
//     session user. Podman sums every line matching the user by name or by
//     numeric uid, so a second line overlaps the first and newuidmap fails
//     with `write to uid_map failed: Invalid argument`; the range must lie
//     inside what keep-id maps, uid+1 through 65535.
//  3. Chown the run's cgroup directory and exactly its cgroup.procs,
//     cgroup.threads and cgroup.subtree_control to the session user — never
//     recursively: memory.max and pids.max must stay root-owned so the run
//     cannot raise its own caps. A read-only cgroup mount (the non-nested
//     shape) is a hard error naming the file.
//  4. Drop to the session uid/gid irreversibly and prove it by failing to get
//     uid 0 back.
//
// Then the provider runs as a CHILD (not an exec: PID 1 has to stay to reap),
// with the environment untouched (secrets travel by name into the container
// env, ADR-0052) and the process's own stdio, so tmux's pty stays the
// provider's. Signals are forwarded, every child is reaped, and the
// provider's exit status is relayed (128+signal when it was killed).
//
// Exit codes deviate from the binary-wide convention like `secret exec`: the
// provider's status passes through verbatim. Only a failure before the
// provider runs is labctl's own — 2 for a usage error, 1 for a failed step
// (not root, a write, the chown, the drop, or a provider that cannot start),
// each logged on stderr with the step that failed. The provider is never
// started after a failed step.
//
// `--self-test` runs the same setup and then, instead of a provider, checks
// as the dropped user that nested containers can work on this host; the
// preflight probe (issue 3) runs it and parses its stdout, whose contract is
// in entry_selftest.go.
//
// The privileged operations sit behind entrySys so the steps are testable
// without privilege (a scratch root and fakes); the real implementation and
// the init loop are Linux-only (entry_linux.go).

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

// maxSubID is the highest id keep-id maps into a run's user namespace; the
// subordinate range handed to the session user is uid+1 through maxSubID.
const maxSubID = 65535

// The files container-entry touches, as paths inside the container. entrySys
// .Root prefixes them (a scratch directory in tests, "/" for real).
const (
	subuidPath = "/etc/subuid"
	subgidPath = "/etc/subgid"
	passwdPath = "/etc/passwd"
	cgroupDir  = "/sys/fs/cgroup"
)

// cgroupChownPaths are exactly the four cgroup paths the session user must own
// for nested podman to create and populate child cgroups: the directory, and
// the files that move processes and enable controllers (moving a process into
// a child cgroup needs write access to the common ancestor's cgroup.procs).
// Nothing else is chowned here, but that is not what protects the limit
// files: under crun and keep-id the session uid owns them already. They are
// protected by the kernel's nsdelegate rule, which refuses writes to every
// non-delegatable file at the run's cgroup namespace root whatever its owner
// or mode (checkCgroupLimitsAt, issue #121).
var cgroupChownPaths = []string{
	cgroupDir,
	cgroupDir + "/cgroup.procs",
	cgroupDir + "/cgroup.threads",
	cgroupDir + "/cgroup.subtree_control",
}

// entrySys is the set of privileged operations the setup performs, injectable
// so tests run them against a scratch root and fakes. realEntrySys (Linux)
// wires the real syscalls with Root "/".
type entrySys struct {
	// Root prefixes every file path the setup touches.
	Root string

	Getuid, Geteuid, Getgid, Getegid func() int
	Getgroups                        func() ([]int, error)

	ReadFile func(path string) ([]byte, error)
	// WriteFile replaces path's content with data, mode 0644.
	WriteFile func(path string, data []byte) error
	Lchown    func(path string, uid, gid int) error

	Setgroups func(gids []int) error
	Setresgid func(rgid, egid, sgid int) error
	Setresuid func(ruid, euid, suid int) error
}

// superviseFunc runs the provider argv as a child of an init loop and returns
// the exit code to relay; superviseProvider (Linux) is the real one.
type superviseFunc func(argv []string, stderr io.Writer) int

// entryUsage is the subcommand's own help, printed on --help (stdout, exit 0)
// and after a usage error (stderr, exit 2).
const entryUsage = `Usage: labctl container-entry --uid N --gid M -- <cmd> [args...]
       labctl container-entry --self-test --uid N --gid M

PID 1 of a nested-containers run, started as container root: writes the one
/etc/subuid and /etc/subgid line for uid N, chowns the run's cgroup to N:M,
drops to N:M irreversibly, then runs cmd as a child, forwards SIGTERM, SIGINT,
SIGHUP, SIGQUIT and SIGWINCH to it, reaps orphans, and exits with cmd's status
(128+signal when it was killed). N and M must be 1–65534. Exit 2 is a usage
error, 1 a failed step before cmd ran (named on stderr).

--self-test does the same setup, runs no cmd (it takes no --), and as N:M
checks that nested containers can work on this host. Stdout carries only one
line per check, in this order, nothing else:

  ok: <check>
  fail: <check>: <why>

  cgroup-child   a child cgroup can be created under /sys/fs/cgroup and removed
  cgroup-limits  no limit file in /sys/fs/cgroup (memory.max, pids.max, ...)
                 opens for writing; only cgroup.procs, cgroup.threads and
                 cgroup.subtree_control are the session's
  procfs-mount   a new user+pid+mount namespace can mount a fresh procfs
  uts-hostname   a new user+UTS namespace can set its hostname
  dev-net-tun    /dev/net/tun opens read-write
  sys-rw         /sys is mounted read-write
  newuidmap      newuidmap is on PATH and carries cap_setuid (reported, NOT
                 fatal: the image may lack the nested stack; the self-test is
                 about the host and the argv)

A failed setup prints the single line "fail: setup: <why>" and runs no check.
Exit 0 when every check but newuidmap passed, 1 when one failed or the setup
did, 2 on a usage error (nothing on stdout).
`

// containerEntry parses the arguments, runs the setup through sys and, only
// when every step succeeded, hands the provider argv to supervise — or, with
// --self-test, runs the checks newChecks builds for the dropped user.
func containerEntry(args []string, env Env, sys entrySys, supervise superviseFunc, newChecks selfTestChecksFunc) int {
	// Split on the first `--` by hand: flag.Parse would swallow the
	// separator, and then `--uid 1 --gid 1 cmd` (no `--`) would be
	// indistinguishable from the real shape.
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	flagArgs := args
	if sep >= 0 {
		flagArgs = args[:sep]
	}

	fs := flag.NewFlagSet("labctl container-entry", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {}
	uid := fs.Int("uid", 0, "session uid to drop to")
	gid := fs.Int("gid", 0, "session gid to drop to")
	selfTest := fs.Bool("self-test", false, "check the host for nested containers instead of running a command")
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = fmt.Fprint(env.Stdout, entryUsage)
			return 0
		}
		return entryUsageError(env, "")
	}
	if fs.NArg() > 0 {
		if *selfTest {
			return entryUsageError(env, fmt.Sprintf("--self-test takes no arguments, got %q", fs.Args()))
		}
		return entryUsageError(env, fmt.Sprintf("unexpected arguments before --: %q", fs.Args()))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["uid"] {
		return entryUsageError(env, "--uid is required")
	}
	if !set["gid"] {
		return entryUsageError(env, "--gid is required")
	}
	// 0 is container root itself, and at maxSubID or above the subordinate
	// range uid+1..65535 is empty.
	if *uid < 1 || *uid >= maxSubID {
		return entryUsageError(env, fmt.Sprintf("--uid must be 1–%d, got %d", maxSubID-1, *uid))
	}
	if *gid < 1 || *gid >= maxSubID {
		return entryUsageError(env, fmt.Sprintf("--gid must be 1–%d, got %d", maxSubID-1, *gid))
	}
	if *selfTest {
		if sep >= 0 {
			return entryUsageError(env, "--self-test runs no command: drop the -- and what follows")
		}
		return containerEntrySelfTest(env, sys, newChecks, *uid, *gid)
	}
	if sep < 0 {
		return entryUsageError(env, "want -- <cmd> [args...] (the -- separator is required)")
	}
	argv := args[sep+1:]
	if len(argv) == 0 {
		return entryUsageError(env, "want a command after --")
	}

	if err := entrySetup(sys, *uid, *gid); err != nil {
		_, _ = fmt.Fprintf(env.Stderr, "labctl container-entry: %v\n", err)
		return 1
	}
	return supervise(argv, env.Stderr)
}

// entryUsageError reports a usage error: the message (if any) and the
// subcommand usage on stderr, exit 2.
func entryUsageError(env Env, msg string) int {
	if msg != "" {
		_, _ = fmt.Fprintf(env.Stderr, "labctl container-entry: %s\n\n", msg)
	}
	_, _ = fmt.Fprint(env.Stderr, entryUsage)
	return 2
}

// entrySetup performs the privileged part of container-entry, in order: the
// uid 0 check, the subuid/subgid lines, the cgroup chown and the irreversible
// drop to uid:gid. It stops at the first failure and returns an error whose
// text starts with the step that failed (e.g. "chown cgroup: /sys/fs/cgroup/
// cgroup.procs: read-only file system"). On success the process runs as
// uid:gid with no supplementary groups and no way back to uid 0.
//
// It is the shared front half of the provider path and of `--self-test`.
func entrySetup(sys entrySys, uid, gid int) error {
	if u, eu := sys.Getuid(), sys.Geteuid(); u != 0 || eu != 0 {
		return fmt.Errorf("must run as uid 0 (container root), running as uid %d (euid %d)", u, eu)
	}
	if err := writeSubIDs(sys, uid, gid); err != nil {
		return err
	}
	if err := chownCgroup(sys, uid, gid); err != nil {
		return err
	}
	return dropPrivileges(sys, uid, gid)
}

// writeSubIDs writes the one subuid and the one subgid line.
//
// Both files name the user with the SAME identifier: the passwd name of uid
// when there is one, else the decimal uid — never both forms, since every
// matching line counts. The subgid line deliberately does not fall back to
// the gid: podman (containers/storage) looks the user up in BOTH files by the
// user name, or by the numeric UID, and shadow's newgidmap matches the
// subgid owner against the calling user's name or uid as well; neither ever
// looks for the gid. Only the ranges differ: uid+1.. in subuid, gid+1.. in
// subgid.
func writeSubIDs(sys entrySys, uid, gid int) error {
	owner, err := subIDOwner(sys, uid)
	if err != nil {
		return err
	}
	if err := sys.WriteFile(filepath.Join(sys.Root, subuidPath), []byte(subIDLine(owner, uid))); err != nil {
		return fmt.Errorf("write %s: %v", subuidPath, pathReason(err))
	}
	if err := sys.WriteFile(filepath.Join(sys.Root, subgidPath), []byte(subIDLine(owner, gid))); err != nil {
		return fmt.Errorf("write %s: %v", subgidPath, pathReason(err))
	}
	return nil
}

// subIDLine is the single `<owner>:<id+1>:<65535-id>` line: the range is
// every id keep-id maps above the session's own.
func subIDLine(owner string, id int) string {
	return fmt.Sprintf("%s:%d:%d\n", owner, id+1, maxSubID-id)
}

// subIDOwner is the passwd name of uid in Root's /etc/passwd (keep-id adds an
// entry for the session user), or the decimal uid when there is none — a
// missing passwd file included. Any other read error fails the step: guessing
// the numeric form while a name exists would be the wrong line.
func subIDOwner(sys entrySys, uid int) (string, error) {
	data, err := sys.ReadFile(filepath.Join(sys.Root, passwdPath))
	if errors.Is(err, fs.ErrNotExist) {
		return strconv.Itoa(uid), nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %v", passwdPath, pathReason(err))
	}
	if name := passwdName(data, uid); name != "" {
		return name, nil
	}
	return strconv.Itoa(uid), nil
}

// passwdName returns the name of the first passwd(5) entry with uid, or "".
func passwdName(data []byte, uid int) string {
	want := strconv.Itoa(uid)
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 3 || fields[2] != want {
			continue
		}
		if name := strings.TrimSpace(fields[0]); name != "" {
			return name
		}
	}
	return ""
}

// chownCgroup hands exactly cgroupChownPaths to uid:gid, the directory first.
// Lchown, so nothing is followed; nothing is recursive.
func chownCgroup(sys entrySys, uid, gid int) error {
	for _, p := range cgroupChownPaths {
		if err := sys.Lchown(filepath.Join(sys.Root, p), uid, gid); err != nil {
			return fmt.Errorf("chown cgroup: %s: %v", p, pathReason(err))
		}
	}
	return nil
}

// dropPrivileges clears the supplementary groups, then sets all three gids,
// then all three uids — gid before uid, because after the uid change the
// process no longer has the privilege to change its gid. Go applies each of
// these to every thread of the process on Linux.
//
// It deliberately does NOT set no_new_privs: that would disable the
// file-capability newuidmap/newgidmap the image ships (ADR-0086), and nested
// podman would fail with the same uid_map error the probe hit with setuid
// binaries.
//
// The drop is then verified: an attempt to get uid 0 back must fail, and the
// real and effective ids must be exactly uid:gid with no supplementary
// groups. A process that could regain root is a fatal error, never a
// provider start.
func dropPrivileges(sys entrySys, uid, gid int) error {
	if err := sys.Setgroups([]int{}); err != nil {
		return fmt.Errorf("drop privileges: setgroups: %v", err)
	}
	if err := sys.Setresgid(gid, gid, gid); err != nil {
		return fmt.Errorf("drop privileges: setresgid %d: %v", gid, err)
	}
	if err := sys.Setresuid(uid, uid, uid); err != nil {
		return fmt.Errorf("drop privileges: setresuid %d: %v", uid, err)
	}
	if err := sys.Setresuid(0, 0, 0); err == nil {
		return errors.New("drop privileges: uid 0 could be regained after the drop")
	}
	if u, eu := sys.Getuid(), sys.Geteuid(); u != uid || eu != uid {
		return fmt.Errorf("drop privileges: uid %d euid %d after the drop, want %d", u, eu, uid)
	}
	if g, eg := sys.Getgid(), sys.Getegid(); g != gid || eg != gid {
		return fmt.Errorf("drop privileges: gid %d egid %d after the drop, want %d", g, eg, gid)
	}
	groups, err := sys.Getgroups()
	if err != nil {
		return fmt.Errorf("drop privileges: getgroups: %v", err)
	}
	if len(groups) != 0 {
		return fmt.Errorf("drop privileges: supplementary groups %v remain after the drop", groups)
	}
	return nil
}

// pathReason strips the operation and path an *fs.PathError carries, so a
// step's message names its file once: "chown cgroup: <path>: <reason>".
func pathReason(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
