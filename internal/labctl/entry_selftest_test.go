//go:build linux

package labctl

// Tests for `labctl container-entry --self-test` (entry_selftest.go,
// entry_selftest_linux.go). The stdout format and the exit codes are pinned
// with fake checks over the fake setup of entry_test.go; the file-based
// checks run against a scratch root; the namespace probes run for real, as
// this (non-root) user, through a re-exec'd copy of the test binary
// (TestSelfTestProbeHelper) — skipped, with the host's reason, where the
// environment refuses the namespaces.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// noSelfTestChecks is the selfTestChecksFunc of tests that must never reach
// the checks: building them fails the test.
func noSelfTestChecks(t *testing.T) selfTestChecksFunc {
	return func(entrySys, int, int) selfTestChecks {
		t.Helper()
		t.Error("self-test checks built")
		return selfTestChecks{}
	}
}

// fakeChecks is a selfTestChecksFunc whose checks return the given errors
// (nil = ok) by check name, and which records the ids it was built for and
// which checks ran.
type fakeChecks struct {
	errs     map[string]error
	builtFor []int
	ran      []string
}

func (f *fakeChecks) fn() selfTestChecksFunc {
	return func(_ entrySys, uid, gid int) selfTestChecks {
		f.builtFor = []int{uid, gid}
		c := func(name string) func() error {
			return func() error {
				f.ran = append(f.ran, name)
				return f.errs[name]
			}
		}
		return selfTestChecks{
			CgroupChild:  c(checkCgroupChild),
			CgroupLimits: c(checkCgroupLimits),
			ProcfsMount:  c(checkProcfsMount),
			UTSHostname:  c(checkUTSHostname),
			DevNetTun:    c(checkDevNetTun),
			SysRW:        c(checkSysRW),
			Newuidmap:    c(checkNewuidmap),
		}
	}
}

func runSelfTestEntry(t *testing.T, sys entrySys, checks selfTestChecksFunc, args ...string) (code int, stdout, stderr string, prov *providerRecorder) {
	t.Helper()
	var out, errw strings.Builder
	prov = &providerRecorder{}
	code = containerEntry(args, Env{Stdout: &out, Stderr: &errw}, sys, prov.fn(), checks)
	return code, out.String(), errw.String(), prov
}

var allChecks = []string{
	checkCgroupChild, checkCgroupLimits, checkProcfsMount, checkUTSHostname,
	checkDevNetTun, checkSysRW, checkNewuidmap,
}

// TestSelfTestOutputFormat PINS the stdout contract the preflight probe
// parses: one `ok: <check>` / `fail: <check>: <why>` line per check, all
// seven, in this order, nothing else; reasons flattened to one line; exit 0
// unless a check other than newuidmap failed.
func TestSelfTestOutputFormat(t *testing.T) {
	tests := []struct {
		name     string
		errs     map[string]error
		wantOut  string
		wantCode int
	}{
		{"all ok", nil, `ok: cgroup-child
ok: cgroup-limits
ok: procfs-mount
ok: uts-hostname
ok: dev-net-tun
ok: sys-rw
ok: newuidmap
`, 0},
		{"only newuidmap fails: reported, not fatal", map[string]error{
			checkNewuidmap: errors.New(`exec: "newuidmap": executable file not found in $PATH`),
		}, `ok: cgroup-child
ok: cgroup-limits
ok: procfs-mount
ok: uts-hostname
ok: dev-net-tun
ok: sys-rw
fail: newuidmap: exec: "newuidmap": executable file not found in $PATH
`, 0},
		{"several fail, reasons flattened", map[string]error{
			checkCgroupLimits: errors.New("write of the current value to /sys/fs/cgroup/memory.max was accepted"),
			checkProcfsMount:  errors.New("mount proc on /proc:\n\toperation not permitted\n"),
			checkSysRW:        errors.New("/sys is mounted read-only"),
			checkNewuidmap:    errors.New("/usr/bin/newuidmap: no security.capability xattr"),
		}, `ok: cgroup-child
fail: cgroup-limits: write of the current value to /sys/fs/cgroup/memory.max was accepted
fail: procfs-mount: mount proc on /proc: operation not permitted
ok: uts-hostname
ok: dev-net-tun
fail: sys-rw: /sys is mounted read-only
fail: newuidmap: /usr/bin/newuidmap: no security.capability xattr
`, 1},
		{"empty reason", map[string]error{checkDevNetTun: errors.New(" \n")}, `ok: cgroup-child
ok: cgroup-limits
ok: procfs-mount
ok: uts-hostname
fail: dev-net-tun: no reason given
ok: sys-rw
ok: newuidmap
`, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeEntry()
			checks := &fakeChecks{errs: tc.errs}
			code, stdout, stderr, prov := runSelfTestEntry(t, f.sys(scratchRoot(t, labPasswd)), checks.fn(), "--self-test", "--uid", "1000", "--gid", "1001")
			if stdout != tc.wantOut {
				t.Errorf("stdout:\n%s\nwant:\n%s", stdout, tc.wantOut)
			}
			if code != tc.wantCode || stderr != "" {
				t.Errorf("code %d stderr %q, want %d and no stderr", code, stderr, tc.wantCode)
			}
			if prov.started {
				t.Error("--self-test started a provider")
			}
			if len(checks.builtFor) != 2 || checks.builtFor[0] != 1000 || checks.builtFor[1] != 1001 {
				t.Errorf("checks built for %v, want [1000 1001]", checks.builtFor)
			}
			if strings.Join(checks.ran, " ") != strings.Join(allChecks, " ") {
				t.Errorf("checks ran %q, want all, in order", checks.ran)
			}
			// The setup ran, and ran before the checks: the drop is done.
			if f.uid != 1000 || f.gid != 1001 {
				t.Errorf("credentials %d:%d, want the drop to 1000:1001", f.uid, f.gid)
			}
		})
	}

	// Each fatal check failing alone fails the self-test.
	for _, name := range allChecks[:6] {
		f := newFakeEntry()
		checks := &fakeChecks{errs: map[string]error{name: errors.New("nope")}}
		code, stdout, _, _ := runSelfTestEntry(t, f.sys(scratchRoot(t, labPasswd)), checks.fn(), "--self-test", "--uid", "1000", "--gid", "1000")
		if code != 1 || !strings.Contains(stdout, "\nfail: "+name+": nope\n") && !strings.HasPrefix(stdout, "fail: "+name+": nope\n") {
			t.Errorf("%s failing: code %d stdout %q, want 1 and its fail line", name, code, stdout)
		}
	}

	// A missing check is a failure line, never a missing line.
	var out bytes.Buffer
	if code := runSelfTest(selfTestChecks{}, &out); code != 1 || strings.Count(out.String(), ": check unavailable\n") != 7 {
		t.Errorf("no checks: code %d stdout %q", code, out.String())
	}
}

// TestSelfTestSetupFailure: a failed setup is the single stdout line
// `fail: setup: <why>` (and the usual stderr line), exit 1, no check run.
func TestSelfTestSetupFailure(t *testing.T) {
	t.Run("not root", func(t *testing.T) {
		f := newFakeEntry()
		f.uid, f.euid = 994, 994
		code, stdout, stderr, _ := runSelfTestEntry(t, f.sys(scratchRoot(t, labPasswd)), noSelfTestChecks(t), "--self-test", "--uid", "1000", "--gid", "1000")
		if want := "fail: setup: must run as uid 0 (container root), running as uid 994 (euid 994)\n"; stdout != want {
			t.Errorf("stdout %q, want %q", stdout, want)
		}
		if want := "labctl container-entry: must run as uid 0 (container root), running as uid 994 (euid 994)\n"; stderr != want {
			t.Errorf("stderr %q, want %q", stderr, want)
		}
		if code != 1 || len(f.calls) != 0 {
			t.Errorf("code %d privileged calls %q, want 1 and none", code, f.calls)
		}
	})
	t.Run("read-only cgroup", func(t *testing.T) {
		f := newFakeEntry()
		f.chownErr = map[string]error{"/sys/fs/cgroup": syscall.EROFS}
		code, stdout, _, _ := runSelfTestEntry(t, f.sys(scratchRoot(t, labPasswd)), noSelfTestChecks(t), "--self-test", "--uid", "1000", "--gid", "1000")
		if want := "fail: setup: chown cgroup: /sys/fs/cgroup: read-only file system\n"; code != 1 || stdout != want {
			t.Errorf("code %d stdout %q, want 1 and %q", code, stdout, want)
		}
	})
}

// TestSelfTestUsage: --self-test takes no `--`/argv and still needs --uid
// and --gid (exit 2, nothing on stdout); --help works with it. Through Run
// with the REAL entrySys: each returns before the first privileged step.
func TestSelfTestUsage(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{"with -- argv", []string{"--self-test", "--uid", "1000", "--gid", "1000", "--", "true"}, "--self-test runs no command"},
		{"with a bare --", []string{"--self-test", "--uid", "1000", "--gid", "1000", "--"}, "--self-test runs no command"},
		{"with positional args", []string{"--self-test", "--uid", "1000", "--gid", "1000", "true"}, `--self-test takes no arguments, got ["true"]`},
		{"without --uid", []string{"--self-test", "--gid", "1000"}, "--uid is required"},
		{"without --gid", []string{"--self-test", "--uid", "1000"}, "--gid is required"},
		{"uid 0", []string{"--self-test", "--uid", "0", "--gid", "1000"}, "--uid must be 1–65534, got 0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := run(t, append([]string{"container-entry"}, tc.args...), nil)
			if code != 2 || stdout != "" {
				t.Errorf("code %d stdout %q, want 2 and nothing on stdout", code, stdout)
			}
			if !strings.Contains(stderr, tc.wantStderr) || !strings.Contains(stderr, "Usage: labctl container-entry") {
				t.Errorf("stderr %q, want it to contain %q and the usage", stderr, tc.wantStderr)
			}
		})
	}

	code, stdout, stderr := run(t, []string{"container-entry", "--self-test", "--help"}, nil)
	if code != 0 || stdout != entryUsage || stderr != "" {
		t.Errorf("--self-test --help: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	for _, want := range append([]string{
		"labctl container-entry --self-test --uid N --gid M", "ok: <check>", "fail: <check>: <why>",
		"fail: setup: <why>", "(reported, NOT",
	}, allChecks...) {
		if !strings.Contains(entryUsage, want) {
			t.Errorf("container-entry usage does not mention %q", want)
		}
	}
	_, top, _ := run(t, []string{"--help"}, nil)
	if !strings.Contains(top, "labctl container-entry --self-test --uid N --gid M") {
		t.Error("top-level usage does not list container-entry --self-test")
	}
	if strings.Contains(top+entryUsage, selfTestProbeArg) {
		t.Errorf("the hidden %s argument shows up in a usage text", selfTestProbeArg)
	}
}

// TestSelfTestRealNonRoot runs the real verb as this (non-root) test
// process: the setup refuses, which is one `fail: setup:` line and exit 1.
func TestSelfTestRealNonRoot(t *testing.T) {
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		t.Skip("running as root: the real verb would modify this host")
	}
	code, stdout, _ := run(t, []string{"container-entry", "--self-test", "--uid", "1000", "--gid", "1000"}, nil)
	if code != 1 || !regexp.MustCompile(`^fail: setup: must run as uid 0 \(container root\), running as uid \d+ \(euid \d+\)\n$`).MatchString(stdout) {
		t.Errorf("code %d stdout %q", code, stdout)
	}
}

// cgroupRoot is a scratch root with a /sys/fs/cgroup holding files of the
// given modes, each with its own distinct content (cgroupFileContent), owned
// by the test user: 0644 is a file whose write-back the scratch root
// accepts, 0444 stands in for one whose open is refused. A mode with
// os.ModeDir makes a directory of that name instead.
func cgroupRoot(t *testing.T, files map[string]os.FileMode) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, cgroupDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, mode := range files {
		p := filepath.Join(dir, name)
		if mode.IsDir() {
			if err := os.Mkdir(p, mode.Perm()); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(p, []byte(cgroupFileContent(name)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// cgroupFileContent is a scratch cgroup file's content: realistic for the
// files the check reads, distinct for every file, so a write of one file's
// content to another would show.
func cgroupFileContent(name string) string {
	switch name {
	case "memory.max":
		return "max\n"
	case "pids.max":
		return "4096\n"
	case "memory.high":
		return "1073741824\n"
	case "cpu.max":
		return "max 100000\n"
	case "cgroup.freeze":
		return "0\n"
	}
	return "content of " + name + "\n"
}

// procMissing, as a procSelf argument, leaves that file out.
const procMissing = "\x00missing"

// procSelf writes the scratch root's /proc/self/mountinfo and
// /proc/self/cgroup.
func procSelf(t *testing.T, root, mountinfo, cgroup string) {
	t.Helper()
	dir := filepath.Join(root, "proc", "self")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"mountinfo": mountinfo, "cgroup": cgroup} {
		if content == procMissing {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// mountinfo is a container's /proc/self/mountinfo with the given lines for
// /sys/fs/cgroup, stacked in order, among the usual others.
func mountinfo(cgroupLines ...string) string {
	return "1600 1580 0:65 / / rw,relatime - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w\n" +
		"1601 1600 0:68 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw\n" +
		"1608 1600 0:70 / /sys rw,nosuid,nodev,noexec,relatime - sysfs sysfs rw\n" +
		strings.Join(cgroupLines, "") +
		"1620 1600 0:72 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755,uid=100000,gid=100000\n"
}

// The /sys/fs/cgroup lines of the mountinfo fixtures.
const (
	miNSDelegate   = "1614 1608 0:21 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime,nosymfollow - cgroup2 cgroup2 rw,nsdelegate,memory_recursiveprot,memory_hugetlb_accounting\n"
	miNoNSDelegate = "1614 1608 0:21 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime,nosymfollow - cgroup2 cgroup2 rw,memory_recursiveprot,memory_hugetlb_accounting\n"
	// The nested shape: a read-write cgroup2 over the runtime's one, with
	// an optional field before the separator.
	miStackedNSDelegate   = "1702 1614 0:21 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime master:9 - cgroup2 cgroup2 rw,nsdelegate,memory_recursiveprot\n"
	miStackedNoNSDelegate = "1702 1614 0:21 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime master:9 - cgroup2 cgroup2 rw,memory_recursiveprot\n"
	miCgroupV1            = "1614 1608 0:21 / /sys/fs/cgroup ro,nosuid,nodev,noexec - tmpfs tmpfs ro,mode=755\n"
	miUnified             = "1615 1614 0:22 / /sys/fs/cgroup/unified rw,nosuid,nodev,noexec - cgroup2 cgroup2 rw,nsdelegate\n"
)

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root opens a 0444 file for writing: the scratch root cannot model a root-owned file")
	}
}

// writeBackCall is one call of a recorded write-back.
type writeBackCall struct{ path, data string }

// TestCheckCgroupLimits: the check fails, with every reason in a stable
// order, when the cgroup2 mount at /sys/fs/cgroup lacks nsdelegate, when the
// process is not at its cgroup namespace root, or when a write-back of
// memory.max's or pids.max's current value is accepted; a refused open or
// write, or an absent cap file, passes. The write-back is the real one or a
// fake that refuses (the kernel's nsdelegate rule on a file the session
// owns) unless told to accept, and is recorded either way: it is called for
// exactly the present caps, with exactly their content, and every file in
// the cgroup directory is byte-identical after the check.
func TestCheckCgroupLimits(t *testing.T) {
	// The files beside the caps in every case: the three chowned ones and
	// the ones with side effects, all writable to the test user.
	extras := map[string]os.FileMode{
		"cgroup.procs": 0o644, "cgroup.threads": 0o644, "cgroup.subtree_control": 0o644,
		"cgroup.kill": 0o644, "cgroup.freeze": 0o644, "cpu.max": 0o644, "memory.high": 0o644,
		"child": os.ModeDir | 0o755, // a sub-cgroup is not a file of this one
	}
	both := map[string]os.FileMode{"memory.max": 0o644, "pids.max": 0o644}
	const (
		memAccepted  = "write of the current value to /sys/fs/cgroup/memory.max was accepted"
		pidsAccepted = "write of the current value to /sys/fs/cgroup/pids.max was accepted"
	)
	tests := []struct {
		name      string
		caps      map[string]os.FileMode
		mountinfo string // "" = mountinfo(miNSDelegate)
		cgroup    string // "" = "0::/\n"
		real      bool   // the real write-back instead of the fake
		accept    map[string]bool
		nonRoot   bool // a 0444 file must refuse the open
		want      string
	}{
		{name: "preconditions hold, both write-backs refused", caps: both},
		{name: "both write-backs accepted", caps: both, real: true, want: memAccepted + "; " + pidsAccepted},
		{name: "both write-backs accepted (fake)", caps: both, accept: map[string]bool{"memory.max": true, "pids.max": true}, want: memAccepted + "; " + pidsAccepted},
		{name: "only memory.max accepted", caps: both, accept: map[string]bool{"memory.max": true}, want: memAccepted},
		{name: "only pids.max accepted", caps: both, accept: map[string]bool{"pids.max": true}, want: pidsAccepted},
		{name: "only pids.max accepted, memory.max open refused", caps: map[string]os.FileMode{"memory.max": 0o444, "pids.max": 0o644}, real: true, nonRoot: true, want: pidsAccepted},
		{name: "both opens refused", caps: map[string]os.FileMode{"memory.max": 0o444, "pids.max": 0o444}, real: true, nonRoot: true},
		{name: "pids.max absent", caps: map[string]os.FileMode{"memory.max": 0o644}},
		{name: "memory.max absent, pids.max accepted", caps: map[string]os.FileMode{"pids.max": 0o644}, real: true, want: pidsAccepted},
		{name: "both caps absent from the root view", caps: nil, real: true},
		{name: "cap unreadable", caps: map[string]os.FileMode{"memory.max": os.ModeDir | 0o755, "pids.max": 0o644},
			want: "read /sys/fs/cgroup/memory.max: is a directory"},
		{name: "mount lacks nsdelegate", caps: both, mountinfo: mountinfo(miNoNSDelegate), want: "cgroup2 mount lacks nsdelegate"},
		{name: "stacked, the visible one lacks nsdelegate", caps: both, mountinfo: mountinfo(miNSDelegate, miStackedNoNSDelegate), want: "cgroup2 mount lacks nsdelegate"},
		{name: "stacked, the visible one has nsdelegate", caps: both, mountinfo: mountinfo(miNoNSDelegate, miStackedNSDelegate)},
		{name: "no mount at /sys/fs/cgroup", caps: both, mountinfo: mountinfo(), want: "no cgroup2 mount at /sys/fs/cgroup"},
		{name: "cgroup2 only below /sys/fs/cgroup", caps: both, mountinfo: mountinfo(miCgroupV1, miUnified), want: "no cgroup2 mount at /sys/fs/cgroup (tmpfs is mounted there)"},
		{name: "not at the namespace root", caps: both, cgroup: "0::/foo\n", want: `not at the cgroup namespace root (/proc/self/cgroup is "0::/foo")`},
		{name: "hybrid, at the namespace root", caps: both, cgroup: "12:pids:/\n1:name=systemd:/\n0::/\n"},
		{name: "no cgroup v2 line", caps: both, cgroup: "12:pids:/\n1:name=systemd:/\n",
			want: `not at the cgroup namespace root (/proc/self/cgroup is "12:pids:/\n1:name=systemd:/")`},
		{name: "proc files missing", caps: both, mountinfo: procMissing, cgroup: procMissing,
			want: "read /proc/self/mountinfo: no such file or directory; read /proc/self/cgroup: no such file or directory"},
		{name: "everything fails, in order", caps: both, mountinfo: mountinfo(miNoNSDelegate), cgroup: "0::/foo\n", real: true,
			want: `cgroup2 mount lacks nsdelegate; not at the cgroup namespace root (/proc/self/cgroup is "0::/foo"); ` + memAccepted + "; " + pidsAccepted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.nonRoot {
				skipIfRoot(t)
			}
			files := map[string]os.FileMode{}
			for _, m := range []map[string]os.FileMode{extras, tc.caps} {
				for k, v := range m {
					files[k] = v
				}
			}
			root := cgroupRoot(t, files)
			mi, cg := tc.mountinfo, tc.cgroup
			if mi == "" {
				mi = mountinfo(miNSDelegate)
			}
			if cg == "" {
				cg = "0::/\n"
			}
			procSelf(t, root, mi, cg)
			dir := filepath.Join(root, cgroupDir)
			before := cgroupFiles(t, dir)

			var calls []writeBackCall
			write := func(p string, b []byte) error {
				calls = append(calls, writeBackCall{p, string(b)})
				if tc.real {
					return writeBack(p, b)
				}
				if tc.accept[filepath.Base(p)] {
					return nil
				}
				return syscall.EPERM
			}
			if got := errString(checkCgroupLimitsWith(root, write)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}

			var wantCalls []writeBackCall
			for _, n := range cgroupCapFiles {
				if c, ok := before[n]; ok {
					wantCalls = append(wantCalls, writeBackCall{filepath.Join(dir, n), c})
				}
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Errorf("write-backs %q, want %q", calls, wantCalls)
			}
			if after := cgroupFiles(t, dir); !reflect.DeepEqual(after, before) {
				t.Errorf("cgroup files changed by the check:\nbefore %q\nafter  %q", before, after)
			}
		})
	}

	// checkCgroupLimitsAt is the check with the real write-back.
	root := cgroupRoot(t, both)
	procSelf(t, root, mountinfo(miNSDelegate), "0::/\n")
	if got := errString(checkCgroupLimitsAt(root)); got != memAccepted+"; "+pidsAccepted {
		t.Errorf("checkCgroupLimitsAt: %q", got)
	}

	// The real write-back opens without O_CREATE: an absent file stays
	// absent.
	p := filepath.Join(t.TempDir(), "memory.max")
	if err := writeBack(p, []byte("max\n")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("write-back of an absent file: %v, want not-exist", err)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("write-back created the file (stat err %v)", err)
	}
}

// cgroupFiles is the content of every regular file directly in dir, by
// name.
func cgroupFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		m[e.Name()] = string(b)
	}
	return m
}

// TestCheckCgroupChild: a child cgroup is created and removed again; a
// missing or unwritable cgroup directory, or a name already taken, fails
// with the step.
func TestCheckCgroupChild(t *testing.T) {
	root := cgroupRoot(t, nil)
	if err := checkCgroupChildAt(root, 42); err != nil {
		t.Fatalf("writable cgroup dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, cgroupDir, "lab-selftest-42")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("child cgroup left behind (stat err %v)", err)
	}

	if got := errString(checkCgroupChildAt(t.TempDir(), 42)); got != "mkdir /sys/fs/cgroup/lab-selftest-42: no such file or directory" {
		t.Errorf("missing cgroup dir: %q", got)
	}

	if err := os.Mkdir(filepath.Join(root, cgroupDir, "lab-selftest-7"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := errString(checkCgroupChildAt(root, 7)); got != "mkdir /sys/fs/cgroup/lab-selftest-7: file exists" {
		t.Errorf("name taken: %q", got)
	}

	if os.Geteuid() != 0 {
		ro := cgroupRoot(t, nil)
		if err := os.Chmod(filepath.Join(ro, cgroupDir), 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(ro, cgroupDir), 0o755) })
		if got := errString(checkCgroupChildAt(ro, 42)); got != "mkdir /sys/fs/cgroup/lab-selftest-42: permission denied" {
			t.Errorf("unwritable cgroup dir: %q", got)
		}
	}
}

// TestCheckDevNetTun: the device opens read-write, or the reason is named.
func TestCheckDevNetTun(t *testing.T) {
	root := t.TempDir()
	if got := errString(checkDevNetTunAt(root)); got != "open /dev/net/tun: no such file or directory" {
		t.Errorf("missing: %q", got)
	}
	p := filepath.Join(root, devNetTunPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkDevNetTunAt(root); err != nil {
		t.Errorf("read-write file: %v", err)
	}
	if os.Geteuid() != 0 {
		if err := os.Chmod(p, 0o400); err != nil {
			t.Fatal(err)
		}
		if got := errString(checkDevNetTunAt(root)); got != "open /dev/net/tun: permission denied" {
			t.Errorf("read-only: %q", got)
		}
	}
}

// TestCheckSysRW: a writable mount passes, a missing /sys fails, and on the
// real root the statfs verdict agrees with /proc/self/mountinfo's.
func TestCheckSysRW(t *testing.T) {
	root := t.TempDir()
	if got := errString(checkSysRWAt(root)); got != "statfs /sys: no such file or directory" {
		t.Errorf("missing: %q", got)
	}
	if err := os.Mkdir(filepath.Join(root, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkSysRWAt(root); err != nil {
		t.Errorf("scratch /sys on a writable mount: %v", err)
	}

	mi, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Skipf("no mountinfo: %v", err)
	}
	// Fields: id parent major:minor root mountpoint options ...; the last
	// mount at /sys is the visible one.
	ro, found := false, false
	for _, line := range strings.Split(string(mi), "\n") {
		if f := strings.Fields(line); len(f) > 5 && f[4] == "/sys" {
			found = true
			ro = strings.HasPrefix(f[5], "ro,") || f[5] == "ro"
		}
	}
	if !found {
		t.Skip("no /sys mount in mountinfo")
	}
	err = checkSysRWAt("/")
	if ro != (errString(err) == "/sys is mounted read-only") || (!ro && err != nil) {
		t.Errorf("real /sys: mountinfo says ro=%v, check says %v", ro, err)
	}
	t.Logf("this host: sys-rw %v", err)
}

// capBlob builds a security.capability value: rev 1, 2 or 3 with the given
// low/high permitted and inheritable words.
func capBlob(rev int, perm0, inh0, perm1, inh1 uint32) []byte {
	words := []uint32{uint32(rev)<<24 | 1, perm0, inh0} // 1: the effective flag
	if rev >= 2 {
		words = append(words, perm1, inh1)
	}
	if rev == 3 {
		words = append(words, 100000) // rootid
	}
	b := make([]byte, 4*len(words))
	for i, w := range words {
		binary.LittleEndian.PutUint32(b[4*i:], w)
	}
	return b
}

// TestCheckNewuidmap pins the vfs_cap_data parsing and the reasons: only
// cap_setuid in the PERMITTED set passes, any revision; malformed values,
// a missing xattr or a missing binary fail.
func TestCheckNewuidmap(t *testing.T) {
	const bin = "/usr/bin/newuidmap"
	setuid := uint32(1) << capSetuid
	tests := []struct {
		name string
		blob []byte
		err  error
		want string // "" = ok
	}{
		{"v2 cap_setuid+ep", capBlob(2, setuid, 0, 0, 0), nil, ""},
		{"v2 with other caps too", capBlob(2, setuid|1<<6, 0, 1<<5, 0), nil, ""},
		{"v3 cap_setuid", capBlob(3, setuid, 0, 0, 0), nil, ""},
		{"v1 cap_setuid", capBlob(1, setuid, 0, 0, 0), nil, ""},
		{"v2 cap_setgid only", capBlob(2, 1<<6, 0, 0, 0), nil, bin + ": cap_setuid is not in its permitted file capabilities"},
		{"v2 cap_setuid inheritable only", capBlob(2, 0, setuid, 0, 0), nil, bin + ": cap_setuid is not in its permitted file capabilities"},
		{"v2 bit 7 of the high word is not cap_setuid", capBlob(2, 0, 0, setuid, 0), nil, bin + ": cap_setuid is not in its permitted file capabilities"},
		{"short", []byte{1, 2, 3}, nil, bin + ": security.capability is 3 bytes, too short"},
		{"empty", nil, nil, bin + ": security.capability is 0 bytes, too short"},
		{"unknown revision", append([]byte{0, 0, 0, 9}, make([]byte, 16)...), nil, bin + ": security.capability has unknown revision 0x9000000"},
		{"v2 truncated", capBlob(2, setuid, 0, 0, 0)[:19], nil, bin + ": security.capability is 19 bytes, want 20 for revision 2"},
		{"no xattr", nil, errNoFileCaps, bin + ": no security.capability xattr"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkNewuidmapWith(
				func(string) (string, error) { return bin, nil },
				func(p string) ([]byte, error) {
					if p != bin {
						t.Errorf("getxattr on %q, want the resolved %q", p, bin)
					}
					return tc.blob, tc.err
				})
			if got := errString(err); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	notFound := &exec.Error{Name: "newuidmap", Err: exec.ErrNotFound}
	err := checkNewuidmapWith(func(string) (string, error) { return "", notFound }, nil)
	if got := errString(err); got != `exec: "newuidmap": executable file not found in $PATH` {
		t.Errorf("not on PATH: %q", got)
	}

	// The real getxattr on a file without file capabilities.
	plain := filepath.Join(t.TempDir(), "newuidmap")
	if err := os.WriteFile(plain, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := capXattr(plain); !errors.Is(err, errNoFileCaps) {
		if errors.Is(err, syscall.ENOTSUP) {
			t.Skipf("no xattrs on the temp dir's file system: %v", err)
		}
		t.Errorf("capXattr on a plain file: %v, want errNoFileCaps", err)
	}
	t.Logf("this host: newuidmap %v", checkNewuidmapWith(exec.LookPath, capXattr))
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

const probeHelperEnv = "LABCTL_SELFTEST_PROBE_HELPER"

// TestSelfTestProbeHelper is not a test: re-exec'd with probeHelperEnv set
// (already inside the probe's namespaces), it runs the probe verb on the
// argument after `--` and exits with its code — what /proc/self/exe
// `container-entry --self-test-probe <kind>` does in the real binary.
func TestSelfTestProbeHelper(t *testing.T) {
	if os.Getenv(probeHelperEnv) != "1" {
		return
	}
	args := os.Args
	for i, a := range os.Args {
		if a == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	os.Exit(runSelfTestProbe(args, os.Stderr))
}

// testProbeRunner runs the real probes through the re-exec'd test binary.
func testProbeRunner() nsProbeRunner {
	return nsProbeRunner{
		Exe:     os.Args[0],
		Args:    func(kind string) []string { return []string{"-test.run=^TestSelfTestProbeHelper$", "--", kind} },
		Env:     append(os.Environ(), probeHelperEnv+"=1"),
		Timeout: probeTimeout,
	}
}

// TestSelfTestProbesReal runs both namespace probes for real, as this user
// mapped to root in the probe's user namespace. The result is environment
// dependent (a container with a masked /proc or the default seccomp profile
// refuses them, which is exactly what the self-test is for), so a refusal
// skips with the host's reason — but the line it would print must still be
// one well-formed line.
func TestSelfTestProbesReal(t *testing.T) {
	probe := testProbeRunner()
	for _, tc := range []struct{ kind, check string }{{probeProcfs, checkProcfsMount}, {probeUTS, checkUTSHostname}} {
		t.Run(tc.check, func(t *testing.T) {
			err := probe.Run(tc.kind, os.Getuid(), os.Getgid())
			checks := selfTestChecks{}
			if tc.kind == probeProcfs {
				checks.ProcfsMount = func() error { return err }
			} else {
				checks.UTSHostname = func() error { return err }
			}
			var out bytes.Buffer
			runSelfTest(checks, &out)
			var line string
			for _, l := range strings.SplitAfter(out.String(), "\n") {
				if strings.HasPrefix(l, "ok: "+tc.check+"\n") || strings.HasPrefix(l, "fail: "+tc.check+": ") {
					line = l
				}
			}
			re := regexp.MustCompile(`^(ok: ` + tc.check + `|fail: ` + tc.check + `: \S[^\n]*)\n$`)
			if !re.MatchString(line) {
				t.Fatalf("malformed line %q (probe err %v)", line, err)
			}
			if err != nil {
				t.Skipf("this host refuses the %s probe: %s", tc.kind, strings.TrimSpace(line))
			}
		})
	}
}

// TestNSProbeRunner pins the runner's reasons with stand-in probes run in
// real user+UTS namespaces: a failing probe's stderr (bounded) is the
// reason, a silent failure names the exit, an unknown kind and a hung probe
// fail. Skipped where the namespaces themselves are refused.
func TestNSProbeRunner(t *testing.T) {
	sh := func(script string) nsProbeRunner {
		return nsProbeRunner{
			Exe:     "/bin/sh",
			Args:    func(string) []string { return []string{"-c", script} },
			Timeout: 5 * time.Second,
		}
	}
	uid, gid := os.Getuid(), os.Getgid()
	if err := sh("exit 0").Run(probeUTS, uid, gid); err != nil {
		t.Skipf("this host refuses a user+UTS namespace: %v", err)
	}

	// Inside, the probe is root of its user namespace.
	if err := sh(`[ "$(id -u)" = 0 ] || { echo "uid $(id -u)" >&2; exit 1; }`).Run(probeUTS, uid, gid); err != nil {
		t.Errorf("probe not root in its namespace: %v", err)
	}
	if got := errString(sh("echo 'line one' >&2; echo '  line two' >&2; exit 3").Run(probeUTS, uid, gid)); got != "line one\n  line two" {
		t.Errorf("stderr reason: %q", got)
	}
	if got := errString(sh("exit 4").Run(probeUTS, uid, gid)); got != "probe exit status 4" {
		t.Errorf("silent failure: %q", got)
	}
	if got := errString(sh("echo out; exit 0").Run(probeUTS, uid, gid)); got != "" {
		t.Errorf("stdout from the probe must not matter: %q", got)
	}
	if got := errString(sh(`i=0; while [ $i -lt 400 ]; do echo xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx >&2; i=$((i+1)); done; exit 1`).Run(probeUTS, uid, gid)); len(got) == 0 || len(got) > probeStderrMax {
		t.Errorf("chatty probe: reason of %d bytes, want 1..%d", len(got), probeStderrMax)
	}
	if got := errString(sh("exit 0").Run("bogus", uid, gid)); got != `unknown probe "bogus"` {
		t.Errorf("unknown kind: %q", got)
	}

	hung := sh("exec sleep 30")
	hung.Timeout = 200 * time.Millisecond
	start := time.Now()
	if got := errString(hung.Run(probeUTS, uid, gid)); got != "probe timed out after 200ms" {
		t.Errorf("hung probe: %q", got)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("hung probe took %v to fail", d)
	}

	// The real probe verb rejects a malformed call.
	var errw bytes.Buffer
	if code := runSelfTestProbe(nil, &errw); code != 2 {
		t.Errorf("no kind: code %d stderr %q", code, errw.String())
	}
	// Outside a probe's user namespace — here, the test process's own — the
	// probe verb refuses before touching anything.
	errw.Reset()
	if code := runSelfTestProbe([]string{probeUTS}, &errw); code != 1 || !strings.HasPrefix(errw.String(), "probe: not in a probe's user namespace (uid_map ") {
		t.Errorf("uts probe outside its namespace: code %d stderr %q", code, errw.String())
	}
	// In a stand-in probe namespace the guard passes and an unknown kind
	// is the probe's own failure.
	bogus := testProbeRunner()
	bogus.Args = func(string) []string { return []string{"-test.run=^TestSelfTestProbeHelper$", "--", "bogus"} }
	if got := errString(bogus.Run(probeUTS, uid, gid)); got != `unknown probe "bogus"` {
		t.Errorf("bogus kind in the probe: %q", got)
	}
}
