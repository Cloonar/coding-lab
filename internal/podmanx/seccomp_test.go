package podmanx

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// seccompRule is the test's typed view of one generated rule.
type seccompRule struct {
	Names    []string `json:"names"`
	Action   string   `json:"action"`
	Args     []any    `json:"args"`
	Includes struct {
		Caps   []string `json:"caps"`
		Arches []string `json:"arches"`
	} `json:"includes"`
	Excludes struct {
		Caps   []string `json:"caps"`
		Arches []string `json:"arches"`
	} `json:"excludes"`
}

type seccompProfileView struct {
	DefaultAction   string        `json:"defaultAction"`
	DefaultErrnoRet int           `json:"defaultErrnoRet"`
	Syscalls        []seccompRule `json:"syscalls"`
}

func decodeProfile(t *testing.T, b []byte) seccompProfileView {
	t.Helper()
	var p seccompProfileView
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("generated profile is not valid JSON: %v", err)
	}
	return p
}

// TestNestedSeccompProfile pins the ADR-0086 delta on the profile lab
// actually generates from the embedded upstream copy — so a `make
// seccomp-refresh` that moves upstream's shape fails here, not on a host.
func TestNestedSeccompProfile(t *testing.T) {
	b, err := NestedSeccompProfile()
	if err != nil {
		t.Fatalf("NestedSeccompProfile: %v", err)
	}
	p := decodeProfile(t, b)

	// ENOSYS as the default is what makes drift against a newer podman
	// benign (ADR-0086): an unknown syscall reads as "not implemented".
	if p.DefaultAction != "SCMP_ACT_ERRNO" || p.DefaultErrnoRet != 38 {
		t.Errorf("default = %s/%d, want SCMP_ACT_ERRNO/38 (ENOSYS)", p.DefaultAction, p.DefaultErrnoRet)
	}

	// The upstream layout the delta depends on: rule 0 is NOT the allow
	// list — it is an ERRNO list — which is why the rule is found by shape.
	if p.Syscalls[0].Action != "SCMP_ACT_ERRNO" {
		t.Errorf("rule 0 action = %s; the test documents upstream's layout (rule 0 an ERRNO list)", p.Syscalls[0].Action)
	}

	var allow []seccompRule
	for _, r := range p.Syscalls {
		if r.Action == "SCMP_ACT_ALLOW" && len(r.Args) == 0 && len(r.Includes.Caps) == 0 &&
			len(r.Includes.Arches) == 0 && len(r.Excludes.Caps) == 0 && len(r.Excludes.Arches) == 0 {
			allow = append(allow, r)
		}
	}
	if len(allow) != 1 {
		t.Fatalf("found %d unconditional allow rules, want exactly 1", len(allow))
	}
	for _, n := range nestedSeccompAllow {
		if c := countOf(allow[0].Names, n); c != 1 {
			t.Errorf("%s appears %d times in the unconditional allow rule, want once", n, c)
		}
	}
	// The big list survived: the delta added to it, it did not replace it.
	if len(allow[0].Names) < 300 || !slices.Contains(allow[0].Names, "read") {
		t.Errorf("unconditional allow rule has %d names; the upstream list was lost", len(allow[0].Names))
	}

	sawSysAdminErrno := false
	for i, r := range p.Syscalls {
		if r.Action == "SCMP_ACT_ALLOW" {
			continue
		}
		for _, n := range nestedSeccompAllow {
			if slices.Contains(r.Names, n) {
				// Stricter than the ADR's "not in the CAP_SYS_ADMIN-excluded
				// ERRNO rule": no non-allow rule may name them at all.
				t.Errorf("rule %d (%s, excludes %v) still names %s", i, r.Action, r.Excludes.Caps, n)
			}
		}
		if r.Action == "SCMP_ACT_ERRNO" && slices.Contains(r.Excludes.Caps, "CAP_SYS_ADMIN") {
			sawSysAdminErrno = true
		}
	}
	if !sawSysAdminErrno {
		t.Errorf("no ERRNO rule excluding CAP_SYS_ADMIN left; the delta must strip names, not rules")
	}

	// Unknown upstream fields survive the generic round trip.
	if !bytes.Contains(b, []byte(`"archMap"`)) || !bytes.Contains(b, []byte(`"errnoRet"`)) || !bytes.Contains(b, []byte(`"comment"`)) {
		t.Errorf("generated profile lost upstream fields")
	}
	// Deterministic and idempotent: same bytes twice, and the delta
	// applied to its own output changes nothing.
	again, err := NestedSeccompProfile()
	if err != nil || !bytes.Equal(again, b) {
		t.Errorf("NestedSeccompProfile not deterministic (err %v)", err)
	}
	twice, err := patchNestedSeccomp(b)
	if err != nil {
		t.Fatalf("patching the generated profile again: %v", err)
	}
	if !bytes.Equal(twice, b) {
		t.Errorf("the delta is not idempotent: re-patching changed the profile")
	}
}

func countOf(names []string, n string) int {
	c := 0
	for _, x := range names {
		if x == n {
			c++
		}
	}
	return c
}

// TestPatchNestedSeccompShapes: the delta locates both rules by shape, and
// an upstream that no longer has either shape fails loudly instead of
// writing a profile that silently lacks the delta.
func TestPatchNestedSeccompShapes(t *testing.T) {
	const errnoRule = `{"names":["quotactl","sethostname","setdomainname"],"action":"SCMP_ACT_ERRNO","args":[],"includes":{},"excludes":{"caps":["CAP_SYS_ADMIN"]},"errnoRet":1}`
	const allowRule = `{"names":["read","write"],"action":"SCMP_ACT_ALLOW","args":[],"includes":{},"excludes":{}}`
	const rule0 = `{"names":["bdflush"],"action":"SCMP_ACT_ERRNO","args":[],"includes":{},"excludes":{},"errnoRet":1}`
	const argAllow = `{"names":["personality"],"action":"SCMP_ACT_ALLOW","args":[{"index":0,"value":0,"op":"SCMP_CMP_EQ"}],"includes":{},"excludes":{}}`
	const capAllow = `{"names":["sethostname"],"action":"SCMP_ACT_ALLOW","args":[],"includes":{"caps":["CAP_SYS_ADMIN"]},"excludes":{}}`
	profile := func(rules ...string) []byte {
		return []byte(`{"defaultAction":"SCMP_ACT_ERRNO","defaultErrnoRet":38,"futureField":{"x":1},"syscalls":[` + strings.Join(rules, ",") + `]}`)
	}

	t.Run("finds the rules past rule 0 and among look-alikes", func(t *testing.T) {
		out, err := patchNestedSeccomp(profile(rule0, argAllow, allowRule, capAllow, errnoRule))
		if err != nil {
			t.Fatalf("patch: %v", err)
		}
		p := decodeProfile(t, out)
		if got := p.Syscalls[2].Names; !slices.Equal(got, []string{"read", "write", "sethostname", "setdomainname"}) {
			t.Errorf("allow rule names = %q", got)
		}
		if got := p.Syscalls[4].Names; !slices.Equal(got, []string{"quotactl"}) {
			t.Errorf("errno rule names = %q", got)
		}
		// Untouched: rule 0, the arg-conditioned allow, the cap-gated allow.
		if got := p.Syscalls[0].Names; !slices.Equal(got, []string{"bdflush"}) {
			t.Errorf("rule 0 names = %q", got)
		}
		if got := p.Syscalls[1].Names; !slices.Equal(got, []string{"personality"}) {
			t.Errorf("arg-conditioned allow names = %q", got)
		}
		if got := p.Syscalls[3].Names; !slices.Equal(got, []string{"sethostname"}) {
			t.Errorf("cap-gated allow names = %q", got)
		}
		if !bytes.Contains(out, []byte(`"futureField"`)) {
			t.Errorf("unknown top-level field dropped: %s", out)
		}
	})

	t.Run("names already allowed are not duplicated", func(t *testing.T) {
		pre := `{"names":["read","sethostname"],"action":"SCMP_ACT_ALLOW","args":[],"includes":{},"excludes":{}}`
		out, err := patchNestedSeccomp(profile(rule0, pre, errnoRule))
		if err != nil {
			t.Fatalf("patch: %v", err)
		}
		if got := decodeProfile(t, out).Syscalls[1].Names; !slices.Equal(got, []string{"read", "sethostname", "setdomainname"}) {
			t.Errorf("allow rule names = %q", got)
		}
	})

	for _, tc := range []struct {
		name, want string
		in         []byte
	}{
		{"no unconditional allow rule", "no unconditional", profile(rule0, argAllow, capAllow, errnoRule)},
		{"two unconditional allow rules", "two unconditional", profile(allowRule, allowRule, errnoRule)},
		{"no ERRNO rule excluding CAP_SYS_ADMIN", "CAP_SYS_ADMIN", profile(rule0, allowRule)},
		{"not JSON", "decoding", []byte("nope")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := patchNestedSeccomp(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("patch error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// TestWriteNestedSeccomp: the profile lands at NestedSeccompPath as valid
// JSON with the delta, the containers dir is created private, and a
// rewrite (the next start) replaces the file in place.
func TestWriteNestedSeccomp(t *testing.T) {
	state := t.TempDir()
	path, err := WriteNestedSeccomp(state)
	if err != nil {
		t.Fatalf("WriteNestedSeccomp: %v", err)
	}
	if want := filepath.Join(state, "containers", "nested-seccomp.json"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the profile: %v", err)
	}
	want, _ := NestedSeccompProfile()
	if !bytes.Equal(b, want) {
		t.Errorf("written profile differs from NestedSeccompProfile")
	}
	decodeProfile(t, b)
	if st, err := os.Stat(filepath.Dir(path)); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("containers dir mode = %v (err %v), want 0700", st.Mode().Perm(), err)
	}

	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteNestedSeccomp(state); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, want) {
		t.Errorf("rewrite did not replace the stale profile")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("containers dir holds %d entries, want only the profile (no temp leftovers)", len(entries))
	}
}
