//go:build linux

package labctl

// The Linux half of `labctl container-entry --self-test` (see
// entry_selftest.go): the real checks, and the namespace probes.
//
// procfs-mount and uts-hostname need a process in fresh namespaces, and Go
// cannot fork without exec, so each probe re-executes this binary with the
// hidden argument selfTestProbeArg in new namespaces (clone flags and id
// maps through SysProcAttr). The probe does its one privileged-in-namespace
// operation and exits; the self-test waits for it with exec.Cmd.Wait, which
// is safe here because the self-test path is no subreaper and runs no init
// loop (superviseProvider's wait4(-1) never runs in this process).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// selfTestProbeArg is the hidden first argument of `labctl container-entry`
// that makes it a namespace probe (selfTestProbe) instead of the entry
// program. It is in no usage text: it exists only so the self-test can run
// code in fresh namespaces (see the top of this file). Run by hand outside
// such a namespace it refuses (inProbeUserNS), so not even root can make it
// remount the caller's /proc or rename the caller's host.
const selfTestProbeArg = "--self-test-probe"

// The probe kinds, the argument after selfTestProbeArg.
const (
	probeProcfs = "procfs"
	probeUTS    = "uts"
)

// probeCloneflags are the namespaces each probe runs in. procfs needs its
// own pid namespace (a fresh procfs shows the mounting process's) and a
// mount namespace that dies with it; uts needs only its own hostname. Both
// get a user namespace, owned by the dropped user, in which they are root.
var probeCloneflags = map[string]uintptr{
	probeProcfs: syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
	probeUTS:    syscall.CLONE_NEWUSER | syscall.CLONE_NEWUTS,
}

// probeHostname is what the uts probe sets, in its own UTS namespace only.
const probeHostname = "lab-selftest"

// probeTimeout bounds one probe: a hung probe is a failed check, not a hung
// preflight.
const probeTimeout = 10 * time.Second

// probeStderrMax bounds how much of a failing probe's stderr becomes the
// fail reason.
const probeStderrMax = 1024

// realSelfTestChecks wires the real checks: the file-based ones under
// sys.Root, the probes through this very binary.
func realSelfTestChecks(sys entrySys, uid, gid int) selfTestChecks {
	probe := nsProbeRunner{
		Exe:     "/proc/self/exe",
		Args:    func(kind string) []string { return []string{"container-entry", selfTestProbeArg, kind} },
		Timeout: probeTimeout,
	}
	return selfTestChecks{
		CgroupChild:  func() error { return checkCgroupChildAt(sys.Root, os.Getpid()) },
		CgroupLimits: func() error { return checkCgroupLimitsAt(sys.Root) },
		ProcfsMount:  func() error { return probe.Run(probeProcfs, uid, gid) },
		UTSHostname:  func() error { return probe.Run(probeUTS, uid, gid) },
		DevNetTun:    func() error { return checkDevNetTunAt(sys.Root) },
		SysRW:        func() error { return checkSysRWAt(sys.Root) },
		Newuidmap:    func() error { return checkNewuidmapWith(exec.LookPath, capXattr) },
	}
}

// nsProbeRunner starts a namespace probe: Exe with Args(kind), in the
// namespaces of probeCloneflags[kind]. The real one re-executes
// /proc/self/exe as the labctl verb; tests point it at the re-exec'd test
// binary. Env nil inherits this process's environment.
type nsProbeRunner struct {
	Exe     string
	Args    func(kind string) []string
	Env     []string
	Timeout time.Duration
}

// Run runs one probe to completion and returns nil when it succeeded, or a
// reason: the probe's own stderr, the start failure (a host or seccomp
// profile that refuses the namespaces themselves) or the timeout.
//
// The probe is root in a user namespace that maps exactly the dropped
// uid:gid to 0 — the one mapping an unprivileged process may write itself,
// with setgroups denied as the kernel requires for it. Its stdout goes to
// /dev/null: the self-test's stdout carries the check lines only.
func (r nsProbeRunner) Run(kind string, uid, gid int) error {
	flags, ok := probeCloneflags[kind]
	if !ok {
		return fmt.Errorf("unknown probe %q", kind)
	}
	// The privilege drop left this process non-dumpable (the kernel resets
	// dumpable on every uid change), and a forked child inherits that until
	// it execs: its /proc/<pid>/uid_map is then owned by container root, and
	// the id-map write below — done by this process, as the dropped user —
	// fails with EACCES. Nothing privileged is left to protect after the
	// verified drop (an exec would reset the flag the same way), so restore
	// it.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set dumpable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Exe, r.Args(kind)...)
	cmd.Env = r.Env
	stderr := &boundedBuffer{max: probeStderrMax}
	cmd.Stderr = stderr
	// A probe that left something holding its stderr open must not hold
	// Wait up past the kill.
	cmd.WaitDelay = time.Second
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 flags,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: uid, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: 0, HostID: gid, Size: 1}},
		GidMappingsEnableSetgroups: false,
		Pdeathsig:                  syscall.SIGKILL,
	}
	err := cmd.Run()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("probe timed out after %v", r.Timeout)
	case err == nil:
		return nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return fmt.Errorf("start probe: %v", err)
	}
	if msg := bytes.TrimSpace(stderr.Bytes()); len(msg) > 0 {
		return errors.New(string(msg))
	}
	return fmt.Errorf("probe %v", ee.ProcessState)
}

// boundedBuffer keeps the first max bytes written to it and silently drops
// the rest, so a chatty probe cannot grow the reason (or memory) unbounded.
type boundedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }

// runSelfTestProbe is the hidden probe verb: `labctl container-entry
// --self-test-probe <kind>`, already inside its namespaces. Exit 0 on
// success, 1 with the reason on stderr, 2 on a malformed call.
func runSelfTestProbe(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintf(stderr, "probe: want one kind, got %q\n", args)
		return 2
	}
	if err := inProbeUserNS(); err != nil {
		_, _ = fmt.Fprintf(stderr, "probe: %v\n", err)
		return 1
	}
	if err := selfTestProbe(args[0]); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// inProbeUserNS refuses unless this process is in a user namespace of the
// shape nsProbeRunner creates: one mapping, of exactly one id, to 0. The
// initial namespace maps the whole id range and a container's maps more
// than one id, so the probe's operations can only ever hit namespaces that
// a probe start created for them.
func inProbeUserNS() error {
	b, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return fmt.Errorf("read uid_map: %v", err)
	}
	if f := strings.Fields(string(b)); len(f) != 3 || f[0] != "0" || f[2] != "1" {
		return fmt.Errorf("not in a probe's user namespace (uid_map %q)", oneLine(string(b)))
	}
	return nil
}

// selfTestProbe is the probe's one operation.
//
// procfs: the mount tree goes private first, so nothing propagates out of
// the probe's mount namespace, and then a fresh procfs is mounted over
// /proc — no scratch directory is needed, since the namespace dies with the
// probe. This is what the masked paths of the default argv make the kernel
// refuse (mount_too_revealing; unmask=ALL lifts it). nosuid, nodev and
// noexec are what every runtime mounts procfs with, and asking for the
// flags the visible procfs has locked is what keeps the kernel from
// refusing the mount for flags rather than for masks.
//
// uts: sethostname in the probe's own UTS namespace — what the default
// seccomp profile denies without CAP_SYS_ADMIN, and lab's patched profile
// (issue 3) allows.
func selfTestProbe(kind string) error {
	switch kind {
	case probeProcfs:
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			return fmt.Errorf("make / private: %v", err)
		}
		if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return fmt.Errorf("mount proc on /proc: %v", err)
		}
		return nil
	case probeUTS:
		if err := unix.Sethostname([]byte(probeHostname)); err != nil {
			return fmt.Errorf("sethostname: %v", err)
		}
		return nil
	default:
		return fmt.Errorf("unknown probe %q", kind)
	}
}

// checkSysRWAt is sys-rw: buildx's privileged buildkit container mounts
// sysfs read-write, which the kernel allows only when the outer /sys is a
// read-write mount (the -v /sys:/sys bind). statfs(2) of the path reports
// ST_RDONLY for exactly the mount /sys resolves to (mount flag or read-only
// superblock), with no mountinfo parsing and its longest-prefix and
// over-mount pitfalls.
func checkSysRWAt(root string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(filepath.Join(root, "/sys"), &st); err != nil {
		return fmt.Errorf("statfs /sys: %v", err)
	}
	if st.Flags&unix.ST_RDONLY != 0 {
		return errors.New("/sys is mounted read-only")
	}
	return nil
}

// capXattr returns path's raw security.capability value (getxattr follows a
// symlink to the binary), or errNoFileCaps. Every vfs_cap_data revision
// fits the buffer; a larger value fails with ERANGE, which is malformed
// anyway.
func capXattr(path string) ([]byte, error) {
	buf := make([]byte, 64)
	n, err := unix.Getxattr(path, "security.capability", buf)
	if errors.Is(err, unix.ENODATA) {
		return nil, errNoFileCaps
	}
	if err != nil {
		return nil, fmt.Errorf("getxattr security.capability: %v", err)
	}
	return buf[:n], nil
}
