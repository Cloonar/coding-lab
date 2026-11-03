//go:build linux

package labctl

// Tests for `labctl container-entry`'s argument handling and setup steps
// (entry.go). The privileged operations are fakes over a scratch root, so
// nothing here needs or uses real privilege; the init loop's tests, with real
// processes, are in entry_supervise_test.go.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// fakeEntry models the process credentials and records every privileged
// call, in order. Its zero value is a process that is NOT root; newFakeEntry
// starts as container root.
type fakeEntry struct {
	uid, euid, gid, egid int
	groups               []int

	calls []string

	chownErr       map[string]error // by in-container path
	setgroupsErr   error
	regainSucceeds bool // a broken drop: setresuid(0,0,0) works afterwards
	ignoreSetresid bool // a broken drop: setresuid "succeeds" without effect
}

func newFakeEntry() *fakeEntry {
	return &fakeEntry{groups: []int{0, 10}}
}

// sys returns an entrySys over root whose file reads and writes are real
// (the scratch root) and whose credential and chown operations are fakes.
func (f *fakeEntry) sys(root string) entrySys {
	s := realEntrySys()
	s.Root = root
	s.Getuid = func() int { return f.uid }
	s.Geteuid = func() int { return f.euid }
	s.Getgid = func() int { return f.gid }
	s.Getegid = func() int { return f.egid }
	s.Getgroups = func() ([]int, error) { return f.groups, nil }
	s.Lchown = func(path string, uid, gid int) error {
		p := strings.TrimPrefix(path, root)
		f.calls = append(f.calls, fmt.Sprintf("lchown %s %d:%d", p, uid, gid))
		if err := f.chownErr[p]; err != nil {
			return &fs.PathError{Op: "lchown", Path: path, Err: err}
		}
		return nil
	}
	s.Setgroups = func(gids []int) error {
		f.calls = append(f.calls, fmt.Sprintf("setgroups %v", gids))
		if f.setgroupsErr != nil {
			return f.setgroupsErr
		}
		f.groups = gids
		return nil
	}
	s.Setresgid = func(r, e, sg int) error {
		f.calls = append(f.calls, fmt.Sprintf("setresgid %d %d %d", r, e, sg))
		if f.euid != 0 {
			return syscall.EPERM
		}
		f.gid, f.egid = r, e
		return nil
	}
	s.Setresuid = func(r, e, su int) error {
		f.calls = append(f.calls, fmt.Sprintf("setresuid %d %d %d", r, e, su))
		if f.euid != 0 && !f.regainSucceeds {
			return syscall.EPERM
		}
		if !f.ignoreSetresid {
			f.uid, f.euid = r, e
		}
		return nil
	}
	return s
}

// scratchRoot is a container root with /etc (and, if passwd != "", an
// /etc/passwd holding it).
func scratchRoot(t *testing.T, passwd string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if passwd != "" {
		if err := os.WriteFile(filepath.Join(root, "etc", "passwd"), []byte(passwd), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// providerRecorder is a superviseFunc that records whether the provider was
// started (and with what) instead of starting it.
type providerRecorder struct {
	started bool
	argv    []string
}

func (p *providerRecorder) fn() superviseFunc {
	return func(argv []string, _ io.Writer) int {
		p.started, p.argv = true, argv
		return 17 // a distinctive "provider's own" code, relayed verbatim
	}
}

func runEntry(t *testing.T, sys entrySys, args ...string) (code int, stdout, stderr string, prov *providerRecorder) {
	t.Helper()
	var out, errw strings.Builder
	prov = &providerRecorder{}
	code = containerEntry(args, Env{Stdout: &out, Stderr: &errw}, sys, prov.fn(), noSelfTestChecks(t))
	return code, out.String(), errw.String(), prov
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

const labPasswd = `root:x:0:0:root:/root:/bin/bash
nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin
labx:x:10000:10000::/home/labx:/bin/sh
lab:x:1000:1000::/home/lab:/bin/bash
`

// TestContainerEntrySuccess pins the whole happy path: the subuid/subgid
// lines, exactly the four chowns, the drop in order with its verification,
// and the provider started last with the argv after `--`, its code relayed.
func TestContainerEntrySuccess(t *testing.T) {
	f := newFakeEntry()
	root := scratchRoot(t, labPasswd)
	code, _, stderr, prov := runEntry(t, f.sys(root), "--uid", "1000", "--gid", "1001", "--", "claude", "--resume", "x")
	if code != 17 || stderr != "" {
		t.Fatalf("code %d stderr %q, want 17 (the provider's) and no stderr", code, stderr)
	}
	if !prov.started || !reflect.DeepEqual(prov.argv, []string{"claude", "--resume", "x"}) {
		t.Fatalf("provider started=%v argv=%q", prov.started, prov.argv)
	}
	if got := readFile(t, filepath.Join(root, "etc/subuid")); got != "lab:1001:64535\n" {
		t.Errorf("subuid = %q", got)
	}
	if got := readFile(t, filepath.Join(root, "etc/subgid")); got != "lab:1002:64534\n" {
		t.Errorf("subgid = %q", got)
	}
	want := []string{
		"lchown /sys/fs/cgroup 1000:1001",
		"lchown /sys/fs/cgroup/cgroup.procs 1000:1001",
		"lchown /sys/fs/cgroup/cgroup.threads 1000:1001",
		"lchown /sys/fs/cgroup/cgroup.subtree_control 1000:1001",
		"setgroups []",
		"setresgid 1001 1001 1001",
		"setresuid 1000 1000 1000",
		"setresuid 0 0 0", // the verification attempt, which must fail
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Errorf("privileged calls:\n got %q\nwant %q", f.calls, want)
	}
	if f.uid != 1000 || f.euid != 1000 || f.gid != 1001 || f.egid != 1001 || len(f.groups) != 0 {
		t.Errorf("credentials after setup: %+v", f)
	}
}

// TestSubIDLines pins the one-line subuid/subgid content: the passwd name of
// the uid, else the numeric uid in BOTH files (never the gid, never both
// forms), ranges from N and M up to 65535, and an overwrite of whatever was
// there, mode 0644.
func TestSubIDLines(t *testing.T) {
	tests := []struct {
		name       string
		passwd     string // "" = no /etc/passwd at all
		uid, gid   int
		wantSubuid string
		wantSubgid string
	}{
		{"passwd name", labPasswd, 1000, 1000, "lab:1001:64535\n", "lab:1001:64535\n"},
		{"passwd name, gid differs", labPasswd, 1000, 2000, "lab:1001:64535\n", "lab:2001:63535\n"},
		{"no passwd entry: numeric uid in both", labPasswd, 1234, 991, "1234:1235:64301\n", "1234:992:64544\n"},
		{"no passwd file: numeric uid", "", 994, 991, "994:995:64541\n", "994:992:64544\n"},
		{"comments and blank lines skipped", "# x\n\nfoo:x:7:7::/:/bin/sh\n", 7, 7, "foo:8:65528\n", "foo:8:65528\n"},
		{"largest uid", labPasswd, 65534, 65534, "nobody:65535:1\n", "nobody:65535:1\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := scratchRoot(t, tc.passwd)
			// A pre-existing, multi-line, private file must end up as
			// exactly the one line, world-readable.
			for _, p := range []string{"etc/subuid", "etc/subgid"} {
				old := "lab:100000:65536\n994:200000:65536\nother:300000:65536\n"
				if err := os.WriteFile(filepath.Join(root, p), []byte(old), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			f := newFakeEntry()
			if err := entrySetup(f.sys(root), tc.uid, tc.gid); err != nil {
				t.Fatalf("entrySetup: %v", err)
			}
			for p, want := range map[string]string{"etc/subuid": tc.wantSubuid, "etc/subgid": tc.wantSubgid} {
				path := filepath.Join(root, p)
				if got := readFile(t, path); got != want {
					t.Errorf("%s = %q, want %q", p, got, want)
				}
				st, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if st.Mode().Perm() != 0o644 {
					t.Errorf("%s mode %v, want 0644", p, st.Mode().Perm())
				}
			}
		})
	}
}

// TestContainerEntryRefusesNonRoot: a caller that is not uid 0 is refused
// before anything is read, written, chowned or dropped, and the provider
// never starts.
func TestContainerEntryRefusesNonRoot(t *testing.T) {
	for _, tc := range []struct{ uid, euid int }{{994, 994}, {0, 994}, {994, 0}} {
		f := newFakeEntry()
		f.uid, f.euid = tc.uid, tc.euid
		root := scratchRoot(t, labPasswd)
		code, _, stderr, prov := runEntry(t, f.sys(root), "--uid", "1000", "--gid", "1000", "--", "true")
		if code != 1 {
			t.Errorf("uid %d euid %d: code %d, want 1", tc.uid, tc.euid, code)
		}
		wantErr := fmt.Sprintf("labctl container-entry: must run as uid 0 (container root), running as uid %d (euid %d)\n", tc.uid, tc.euid)
		if stderr != wantErr {
			t.Errorf("stderr %q, want %q", stderr, wantErr)
		}
		if prov.started || len(f.calls) != 0 {
			t.Errorf("provider started=%v, privileged calls %q; want none", prov.started, f.calls)
		}
		for _, p := range []string{"etc/subuid", "etc/subgid"} {
			if _, err := os.Stat(filepath.Join(root, p)); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s written by a non-root caller (stat err %v)", p, err)
			}
		}
	}
}

// TestContainerEntryReadOnlyCgroup: a chown failure (a read-only cgroup
// mount) aborts with the file and the reason, before the drop and before the
// provider.
func TestContainerEntryReadOnlyCgroup(t *testing.T) {
	f := newFakeEntry()
	f.chownErr = map[string]error{"/sys/fs/cgroup/cgroup.procs": syscall.EROFS}
	code, _, stderr, prov := runEntry(t, f.sys(scratchRoot(t, labPasswd)), "--uid", "1000", "--gid", "1000", "--", "true")
	if code != 1 {
		t.Errorf("code %d, want 1", code)
	}
	want := "labctl container-entry: chown cgroup: /sys/fs/cgroup/cgroup.procs: read-only file system\n"
	if stderr != want {
		t.Errorf("stderr %q, want %q", stderr, want)
	}
	if prov.started {
		t.Error("provider started after a failed chown")
	}
	wantCalls := []string{
		"lchown /sys/fs/cgroup 1000:1000",
		"lchown /sys/fs/cgroup/cgroup.procs 1000:1000",
	}
	if !reflect.DeepEqual(f.calls, wantCalls) {
		t.Errorf("calls %q, want %q (stop at the failure, no drop)", f.calls, wantCalls)
	}
}

// TestContainerEntryWriteFailure: a subuid write that fails names the file
// and stops before any chown.
func TestContainerEntryWriteFailure(t *testing.T) {
	f := newFakeEntry()
	root := t.TempDir() // no /etc: the write fails
	code, _, stderr, prov := runEntry(t, f.sys(root), "--uid", "1000", "--gid", "1000", "--", "true")
	if code != 1 || stderr != "labctl container-entry: write /etc/subuid: no such file or directory\n" {
		t.Errorf("code %d stderr %q", code, stderr)
	}
	if prov.started || len(f.calls) != 0 {
		t.Errorf("provider started=%v calls %q after a failed write", prov.started, f.calls)
	}
}

// TestContainerEntryDropFailures: a drop that fails, or that leaves a way
// back to uid 0 or the wrong ids, is fatal and the provider never starts.
func TestContainerEntryDropFailures(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*fakeEntry)
		wantErr string
	}{
		{"setgroups refused", func(f *fakeEntry) { f.setgroupsErr = syscall.EPERM },
			"labctl container-entry: drop privileges: setgroups: operation not permitted\n"},
		{"uid 0 regained", func(f *fakeEntry) { f.regainSucceeds = true },
			"labctl container-entry: drop privileges: uid 0 could be regained after the drop\n"},
		{"setresuid without effect", func(f *fakeEntry) { f.ignoreSetresid = true },
			"labctl container-entry: drop privileges: uid 0 could be regained after the drop\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeEntry()
			tc.mutate(f)
			code, _, stderr, prov := runEntry(t, f.sys(scratchRoot(t, labPasswd)), "--uid", "1000", "--gid", "1000", "--", "true")
			if code != 1 || stderr != tc.wantErr {
				t.Errorf("code %d stderr %q, want 1 and %q", code, stderr, tc.wantErr)
			}
			if prov.started {
				t.Error("provider started after a failed drop")
			}
		})
	}
}

// TestDropPrivilegesVerifiesIDs: a drop whose calls all succeed but leave
// other ids, or supplementary groups, behind is still refused.
func TestDropPrivilegesVerifiesIDs(t *testing.T) {
	f := newFakeEntry()
	s := f.sys(t.TempDir())
	s.Getgid = func() int { return 0 }
	if err := dropPrivileges(s, 1000, 1000); err == nil || err.Error() != "drop privileges: gid 0 egid 1000 after the drop, want 1000" {
		t.Errorf("wrong gid: err %v", err)
	}

	f = newFakeEntry()
	s = f.sys(t.TempDir())
	s.Getgroups = func() ([]int, error) { return []int{10}, nil }
	if err := dropPrivileges(s, 1000, 1000); err == nil || err.Error() != "drop privileges: supplementary groups [10] remain after the drop" {
		t.Errorf("groups left: err %v", err)
	}
}

// TestContainerEntryUsage pins the usage errors (exit 2, nothing done) and
// --help (exit 0, stdout). They go through Run with the REAL entrySys: every
// one of them returns before the first privileged step.
func TestContainerEntryUsage(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{"no args", nil, "--uid is required"},
		{"missing --uid", []string{"--gid", "1000", "--", "true"}, "--uid is required"},
		{"missing --gid", []string{"--uid", "1000", "--", "true"}, "--gid is required"},
		{"uid 0", []string{"--uid", "0", "--gid", "1000", "--", "true"}, "--uid must be 1–65534, got 0"},
		{"uid 65535", []string{"--uid", "65535", "--gid", "1000", "--", "true"}, "--uid must be 1–65534, got 65535"},
		{"negative uid", []string{"--uid", "-5", "--gid", "1000", "--", "true"}, "--uid must be 1–65534, got -5"},
		{"gid 0", []string{"--uid", "1000", "--gid", "0", "--", "true"}, "--gid must be 1–65534, got 0"},
		{"gid too large", []string{"--uid", "1000", "--gid", "70000", "--", "true"}, "--gid must be 1–65534, got 70000"},
		{"uid not a number", []string{"--uid", "lab", "--gid", "1000", "--", "true"}, `invalid value "lab" for flag -uid`},
		{"unknown flag", []string{"--bogus", "--uid", "1", "--gid", "1", "--", "true"}, "flag provided but not defined: -bogus"},
		{"missing --", []string{"--uid", "1000", "--gid", "1000", "true"}, "unexpected arguments before --"},
		{"no -- at all", []string{"--uid", "1000", "--gid", "1000"}, "the -- separator is required"},
		{"empty argv", []string{"--uid", "1000", "--gid", "1000", "--"}, "want a command after --"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := run(t, append([]string{"container-entry"}, tc.args...), nil)
			if code != 2 {
				t.Errorf("code %d, want 2", code)
			}
			if !strings.Contains(stderr, tc.wantStderr) || !strings.Contains(stderr, "Usage: labctl container-entry") {
				t.Errorf("stderr %q, want it to contain %q and the usage", stderr, tc.wantStderr)
			}
			if stdout != "" {
				t.Errorf("stdout %q, want empty", stdout)
			}
		})
	}

	code, stdout, stderr := run(t, []string{"container-entry", "--help"}, nil)
	if code != 0 || !strings.HasPrefix(stdout, "Usage: labctl container-entry --uid N --gid M -- <cmd> [args...]") || stderr != "" {
		t.Errorf("--help: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, top, _ := run(t, []string{"--help"}, nil); !strings.Contains(top, "labctl container-entry --uid N --gid M -- <cmd> [args...]") {
		t.Error("top-level usage does not list container-entry")
	}
}

// TestContainerEntryRealNonRoot runs the real verb as this (non-root) test
// process: it must refuse at the uid check, before touching /etc. Skipped as
// root, where it would write the host's /etc/subuid.
func TestContainerEntryRealNonRoot(t *testing.T) {
	if os.Getuid() == 0 || os.Geteuid() == 0 {
		t.Skip("running as root: the real verb would modify this host")
	}
	code, _, stderr := run(t, []string{"container-entry", "--uid", "1000", "--gid", "1000", "--", "true"}, nil)
	if code != 1 || !strings.HasPrefix(stderr, "labctl container-entry: must run as uid 0 (container root), running as uid ") {
		t.Errorf("code %d stderr %q", code, stderr)
	}
}
