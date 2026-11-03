package labctl

// `labctl container-entry --self-test` (issue #114, ADR-0086): the same setup
// as the provider path (entrySetup), then, as the dropped user, one check per
// thing a nested podman needs from the host and from the outer argv. The
// preflight probe (internal/podmanx, issue 3) runs it in a nested-shaped
// container and parses its stdout, so that stdout is a contract:
//
//	ok: <check>
//	fail: <check>: <why>
//
// one line per check, always all seven, always in the order of
// selfTestChecks.ordered, and nothing else — no banner, no summary;
// diagnostics go to stderr. <why> is one line (oneLine flattens it). A failed
// setup is the single line `fail: setup: <why>` and no check runs, so the
// parser always has a reason. Renaming, reordering or adding a check is a
// contract change for that parser.
//
// Exit 0 when every fatal check passed, 1 otherwise (or when the setup
// failed). newuidmap is the one check that is reported but not fatal: the
// image may legitimately be one without the nested stack (ADR-0086), and the
// self-test is about the host and the argv, not the image.
//
// The checks are injectable (selfTestChecksFunc) so the format and the exit
// code are pinned with fakes; the file-based ones below take the scratch-able
// root, and the real wiring, including the namespace probes, is Linux-only
// (entry_selftest_linux.go).

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// The stable check identifiers, in output order.
const (
	checkCgroupChild  = "cgroup-child"
	checkCgroupLimits = "cgroup-limits"
	checkProcfsMount  = "procfs-mount"
	checkUTSHostname  = "uts-hostname"
	checkDevNetTun    = "dev-net-tun"
	checkSysRW        = "sys-rw"
	checkNewuidmap    = "newuidmap"
)

// devNetTunPath is pasta's device, the nested containers' network; the outer
// argv passes it with --device.
const devNetTunPath = "/dev/net/tun"

// selfTestChecks holds one function per check, each run as the dropped user;
// a nil error is `ok`, an error's text is the `fail` reason. A nil function
// is reported as a failure rather than skipped, so the output always has all
// seven lines.
type selfTestChecks struct {
	CgroupChild  func() error
	CgroupLimits func() error
	ProcfsMount  func() error
	UTSHostname  func() error
	DevNetTun    func() error
	SysRW        func() error
	Newuidmap    func() error
}

// selfTestChecksFunc builds the checks for the dropped uid:gid, after the
// setup through sys succeeded; realSelfTestChecks (Linux) is the real one.
type selfTestChecksFunc func(sys entrySys, uid, gid int) selfTestChecks

// selfTestCheck is one line of the output: its identifier, whether a failure
// fails the self-test, and the check itself.
type selfTestCheck struct {
	name  string
	fatal bool
	run   func() error
}

// ordered is the ONE place the output order and the fatal flags live.
func (c selfTestChecks) ordered() []selfTestCheck {
	return []selfTestCheck{
		{checkCgroupChild, true, c.CgroupChild},
		{checkCgroupLimits, true, c.CgroupLimits},
		{checkProcfsMount, true, c.ProcfsMount},
		{checkUTSHostname, true, c.UTSHostname},
		{checkDevNetTun, true, c.DevNetTun},
		{checkSysRW, true, c.SysRW},
		{checkNewuidmap, false, c.Newuidmap},
	}
}

// containerEntrySelfTest is `--self-test` after the arguments parsed: the
// setup, then every check, as described at the top of this file.
func containerEntrySelfTest(env Env, sys entrySys, newChecks selfTestChecksFunc, uid, gid int) int {
	if err := entrySetup(sys, uid, gid); err != nil {
		// stderr as on the provider path, plus the one stdout line the
		// parser needs.
		_, _ = fmt.Fprintf(env.Stderr, "labctl container-entry: %v\n", err)
		_, _ = fmt.Fprintf(env.Stdout, "fail: setup: %s\n", oneLine(err.Error()))
		return 1
	}
	return runSelfTest(newChecks(sys, uid, gid), env.Stdout)
}

// runSelfTest runs every check in order — a failure never skips the ones
// after it, the preflight wants the whole picture — writes one line each,
// and returns 0 unless a fatal check failed.
func runSelfTest(checks selfTestChecks, stdout io.Writer) int {
	code := 0
	for _, c := range checks.ordered() {
		err := errors.New("check unavailable")
		if c.run != nil {
			err = c.run()
		}
		if err == nil {
			_, _ = fmt.Fprintf(stdout, "ok: %s\n", c.name)
			continue
		}
		_, _ = fmt.Fprintf(stdout, "fail: %s: %s\n", c.name, oneLine(err.Error()))
		if c.fatal {
			code = 1
		}
	}
	return code
}

// oneLine flattens a reason to a single line: every run of whitespace,
// newlines included, becomes one space. An empty reason would leave a line
// the parser cannot tell from a truncated one, so it gets a placeholder.
func oneLine(s string) string {
	if s = strings.Join(strings.Fields(s), " "); s != "" {
		return s
	}
	return "no reason given"
}

// checkCgroupChildAt is cgroup-child: nested podman creates its containers'
// cgroups under the run's own, which is exactly what the chown of the
// directory is for. The pid in the name keeps a manual self-test beside the
// preflight's from colliding; the directory is removed again at once.
func checkCgroupChildAt(root string, pid int) error {
	p := path.Join(cgroupDir, fmt.Sprintf("lab-selftest-%d", pid))
	full := filepath.Join(root, p)
	if err := os.Mkdir(full, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %v", p, pathReason(err))
	}
	// syscall.Rmdir, not os.Remove: os.Remove tries unlink first, and its
	// error is then the less telling of the two.
	if err := syscall.Rmdir(full); err != nil {
		return fmt.Errorf("rmdir %s: %v", p, err)
	}
	return nil
}

// checkCgroupLimitsAt is cgroup-limits: the run must not be able to change
// its own caps. File mode cannot show that: crun chowns only the delegatable
// files to container root, so under keep-id the session uid owns every other
// file of the namespace root, memory.max and pids.max included, and opens
// them for writing (issue #121). What protects them is the kernel's nsdelegate
// rule: cgroup_file_write (kernel/cgroup/cgroup.c) refuses with EPERM, on a
// cgroup2 mount with nsdelegate, any write to a file not marked delegatable
// in the root cgroup of a non-init cgroup namespace, from inside that
// namespace, whatever the file's mode.
//
// So the check asserts what the kernel enforces, in three parts, all run and
// all reported, in this order:
//
//   - the cgroup2 mount at /sys/fs/cgroup carries nsdelegate (mountinfo's
//     super options of the visible mount, the last one stacked there);
//   - this process is at its cgroup namespace root (/proc/self/cgroup is
//     0::/, which podman's default cgroupns=private gives on cgroup v2);
//   - memory.max and pids.max are written back with exactly their current
//     content, and the kernel must refuse the write: a refused open or a
//     refused write both mean the session cannot change the cap. A cap file
//     absent from the namespaced root view is no failure (the caps can sit
//     on the scope, issue #114).
//
// Writing the current value is a no-op even where it is accepted, so the
// check never changes a run's caps. No other file is opened for writing:
// cgroup.kill and cgroup.freeze act on any accepted write, so they are
// covered by the two preconditions only.
func checkCgroupLimitsAt(root string) error {
	return checkCgroupLimitsWith(root, writeBack)
}

// cgroupCapFiles are the caps cgroup-limits test-writes, in reporting order.
var cgroupCapFiles = []string{"memory.max", "pids.max"}

// checkCgroupLimitsWith is checkCgroupLimitsAt with the write-back
// injectable, so tests can model the kernel refusing a write the scratch
// root would accept.
func checkCgroupLimitsWith(root string, write func(path string, b []byte) error) error {
	var reasons []string
	if r := checkNSDelegate(root); r != "" {
		reasons = append(reasons, r)
	}
	if r := checkCgroupNSRoot(root); r != "" {
		reasons = append(reasons, r)
	}
	for _, n := range cgroupCapFiles {
		p := cgroupDir + "/" + n
		full := filepath.Join(root, p)
		b, err := os.ReadFile(full)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("read %s: %v", p, pathReason(err)))
			continue
		}
		if write(full, b) == nil {
			reasons = append(reasons, fmt.Sprintf("write of the current value to %s was accepted", p))
		}
	}
	if len(reasons) == 0 {
		return nil
	}
	return errors.New(strings.Join(reasons, "; "))
}

// checkNSDelegate returns why the mount visible at /sys/fs/cgroup is not a
// cgroup2 mount with the nsdelegate super option, or "". A mountinfo line is
// `id parent major:minor root mountpoint options [optional...] - fstype
// source superoptions`; mounts stack, so the last line for the mount point
// is the one the path resolves to.
func checkNSDelegate(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "/proc/self/mountinfo"))
	if err != nil {
		return fmt.Sprintf("read /proc/self/mountinfo: %v", pathReason(err))
	}
	var visible []string // the fields after the separator, or none
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[4] != cgroupDir {
			continue
		}
		found, visible = true, nil
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				visible = f[i+1:]
				break
			}
		}
	}
	switch {
	case !found:
		return "no cgroup2 mount at " + cgroupDir
	case len(visible) < 3 || visible[0] != "cgroup2":
		fstype := "an unparsable mount"
		if len(visible) > 0 {
			fstype = visible[0]
		}
		return fmt.Sprintf("no cgroup2 mount at %s (%s is mounted there)", cgroupDir, fstype)
	}
	for _, o := range strings.Split(visible[2], ",") {
		if o == "nsdelegate" {
			return ""
		}
	}
	return "cgroup2 mount lacks nsdelegate"
}

// checkCgroupNSRoot returns why this process is not at its cgroup namespace
// root, or "": the cgroup v2 line of /proc/self/cgroup must be exactly 0::/.
func checkCgroupNSRoot(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "/proc/self/cgroup"))
	if err != nil {
		return fmt.Sprintf("read /proc/self/cgroup: %v", pathReason(err))
	}
	shown := strings.TrimSpace(string(b))
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "0::") {
			if line == "0::/" {
				return ""
			}
			shown = line
			break
		}
	}
	return fmt.Sprintf("not at the cgroup namespace root (/proc/self/cgroup is %q)", shown)
}

// writeBack is the real write-back: open O_WRONLY — no O_CREATE, no O_TRUNC
// — write b in one write, close. An error from the open or the write is the
// kernel (or the mode) refusing it; nil is an accepted write. A cgroup file
// applies a write in write(2), so the close's result says nothing about it.
func writeBack(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	_ = f.Close()
	return err
}

// checkDevNetTunAt is dev-net-tun: open read-write and close at once. An
// open without TUNSETIFF creates no interface.
func checkDevNetTunAt(root string) error {
	f, err := os.OpenFile(filepath.Join(root, devNetTunPath), os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %v", devNetTunPath, pathReason(err))
	}
	return f.Close()
}

// errNoFileCaps is a binary without a security.capability xattr.
var errNoFileCaps = errors.New("no security.capability xattr")

// checkNewuidmapWith is newuidmap: it resolves on PATH (as the dropped user,
// so the user's PATH decides) and its file capabilities grant cap_setuid —
// the layout the image ships instead of Debian's setuid bit (ADR-0086).
// getxattr returns the raw security.capability value, or errNoFileCaps.
func checkNewuidmapWith(lookPath func(string) (string, error), getxattr func(string) ([]byte, error)) error {
	p, err := lookPath("newuidmap")
	if err != nil {
		return err
	}
	blob, err := getxattr(p)
	if err != nil {
		return fmt.Errorf("%s: %v", p, err)
	}
	permitted, err := vfsCapPermitted(blob)
	if err != nil {
		return fmt.Errorf("%s: %v", p, err)
	}
	if permitted&(1<<capSetuid) == 0 {
		return fmt.Errorf("%s: cap_setuid is not in its permitted file capabilities", p)
	}
	return nil
}

// The parts of linux/capability.h's struct vfs_cap_data (v1, v2) and struct
// vfs_ns_cap_data (v3) the newuidmap check reads. The layout is
// little-endian: magic_etc, then {permitted, inheritable} once (v1, 32
// capabilities) or twice (v2, v3: the low then the high 32), then for v3 the
// rootid. The kernel accepts each revision only at its exact size.
const (
	vfsCapRevisionMask = 0xFF000000
	vfsCapRevision1    = 0x01000000
	vfsCapRevision2    = 0x02000000
	vfsCapRevision3    = 0x03000000
	capSetuid          = 7
)

// vfsCapPermitted returns the permitted capability set of a
// security.capability xattr value.
func vfsCapPermitted(blob []byte) (uint64, error) {
	if len(blob) < 4 {
		return 0, fmt.Errorf("security.capability is %d bytes, too short", len(blob))
	}
	rev := binary.LittleEndian.Uint32(blob) & vfsCapRevisionMask
	var size int
	switch rev {
	case vfsCapRevision1:
		size = 12
	case vfsCapRevision2:
		size = 20
	case vfsCapRevision3:
		size = 24
	default:
		return 0, fmt.Errorf("security.capability has unknown revision %#x", rev)
	}
	if len(blob) != size {
		return 0, fmt.Errorf("security.capability is %d bytes, want %d for revision %d", len(blob), size, rev>>24)
	}
	permitted := uint64(binary.LittleEndian.Uint32(blob[4:]))
	if rev != vfsCapRevision1 {
		permitted |= uint64(binary.LittleEndian.Uint32(blob[12:])) << 32
	}
	return permitted, nil
}
