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

// checkCgroupLimitsAt is cgroup-limits: the run must not be able to raise
// its own caps. memory.max and pids.max are named explicitly, and every
// other regular file directly in the cgroup directory is tried too, because
// the limit files may not exist at all in the namespaced root view (the
// caps can live on the parent, issue #114) — then the assertion is that no
// file there is writable. The three files the setup chowned to the session
// (cgroupChownPaths) are the only exceptions.
//
// Each file is opened O_WRONLY — no O_CREATE, no O_TRUNC — and closed at
// once, never written: opening a cgroup file has no effect, writing would.
// A missing file, or any refused open, is "cannot open for writing", which
// is what is asserted.
func checkCgroupLimitsAt(root string) error {
	dir := filepath.Join(root, cgroupDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("list %s: %v", cgroupDir, pathReason(err))
	}
	skip := map[string]bool{"memory.max": true, "pids.max": true}
	for _, p := range cgroupChownPaths[1:] {
		skip[path.Base(p)] = true
	}
	names := []string{"memory.max", "pids.max"}
	for _, e := range entries { // sorted by name
		if e.Type().IsRegular() && !skip[e.Name()] {
			names = append(names, e.Name())
		}
	}
	var writable []string
	for _, n := range names {
		f, err := os.OpenFile(filepath.Join(dir, n), os.O_WRONLY, 0)
		if err != nil {
			continue
		}
		_ = f.Close()
		writable = append(writable, cgroupDir+"/"+n)
	}
	switch len(writable) {
	case 0:
		return nil
	case 1:
		return fmt.Errorf("%s is writable", writable[0])
	default:
		return fmt.Errorf("%s are writable", strings.Join(writable, ", "))
	}
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
