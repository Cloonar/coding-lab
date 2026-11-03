package podmanx

// The nested-containers seccomp profile (ADR-0086, issue #115).
//
// A nested run starts its own rootless podman, and every nested `crun`
// calls sethostname(2) for the nested container's UTS namespace. The
// default seccomp profile podman applies to the OUTER container allows
// sethostname and setdomainname only to a container holding CAP_SYS_ADMIN
// (a conditional allow rule, plus an ERRNO rule that excludes the cap), and
// a lab run holds no CAP_SYS_ADMIN — so every nested container died with
// `sethostname: Operation not permitted` in the 2026-11-02 probe. The fix
// is a lab-generated profile: upstream's default, with the two syscalls
// moved into the unconditional allow list.
//
// Why that grants nothing new: seccomp is only the first gate. The kernel
// still demands CAP_SYS_ADMIN over the UTS namespace the caller lives in
// (ns_capable(uts_ns->user_ns, CAP_SYS_ADMIN)), and the run holds that only
// over namespaces it created itself — its nested containers'. The host's
// UTS namespace, and the outer run's own (owned by the user namespace
// podman created, in which the session has no capabilities after
// container-entry's drop), stay out of reach. The profile change therefore
// widens nothing outside namespaces the run created.
//
// Why drift is benign: the profile's defaultAction is SCMP_ACT_ERRNO with
// defaultErrnoRet ENOSYS (38). A syscall a newer podman or kernel knows and
// this embedded copy does not list fails as "not implemented", which libc
// and Go fall back from, rather than EPERM, which they treat as a policy
// denial. An embedded copy a few releases behind the host's podman is
// therefore no hazard; the test pins defaultErrnoRet so a refresh that
// changes it fails loudly.
//
// Why embedded data and a delta in Go: importing containers/common for its
// profile would drag the libseccomp cgo bindings into lab, against
// ADR-0065's static binaries; shipping the file from the Nix module would
// leave non-NixOS hosts without one and let it drift from the lab release
// that renders the argv (both rejected in ADR-0086). The embedded upstream
// file is data, the delta is code with a test, and lab writes the result at
// startup (WriteNestedSeccomp) to the path RunArgv's nested shape passes
// with --security-opt seccomp=.
//
// The embedded copy: seccomp/upstream.json is containers/common's
// pkg/seccomp/seccomp.json, which moved with containers/common into the
// container-libs monorepo (github.com/podman-container-tools/container-libs,
// formerly github.com/containers/container-libs; the old containers/common
// repository is no longer updated). Pinned to tag common/v0.70.0
// (common/pkg/seccomp/seccomp.json, identical to main at commit
// 3beb7f638967ef1a6b94c0388682ca3129d9a0c3, the last change to the file
// when it was embedded). `make seccomp-refresh` re-downloads it at the tag
// the Makefile's SECCOMP_REF names; bump that and this comment together,
// and run the tests, which fail if a refresh breaks the shape the delta
// relies on.

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"git.cloonar.com/Cloonar/coding-lab/internal/fsx"
)

//go:embed seccomp/upstream.json
var upstreamSeccomp []byte

// nestedSeccompAllow are the syscalls the delta moves from the
// CAP_SYS_ADMIN-gated rules into the unconditional allow list.
var nestedSeccompAllow = []string{"sethostname", "setdomainname"}

// The seccomp vocabulary the delta matches rules by.
const (
	seccompActAllow = "SCMP_ACT_ALLOW"
	seccompActErrno = "SCMP_ACT_ERRNO"
	capSysAdmin     = "CAP_SYS_ADMIN"
)

// NestedSeccompPath is where WriteNestedSeccomp writes the profile and the
// nested run shape reads it from: <state>/containers/nested-seccomp.json.
func NestedSeccompPath(stateDir string) string {
	return filepath.Join(stateDir, "containers", "nested-seccomp.json")
}

// SharedStorePath is the shared read-only image store of nested runs
// (ADR-0086): <state>/containers/shared-store, beside the seccomp profile.
// Lab populates it with podman --root; RunArgv mounts it read-only at this
// same host path and names it in LAB_SHARED_STORE.
func SharedStorePath(stateDir string) string {
	return filepath.Join(stateDir, "containers", "shared-store")
}

// NestedSeccompProfile returns the nested-containers seccomp profile: the
// embedded upstream profile with the ADR-0086 delta applied, re-marshalled
// as tab-indented JSON. Deterministic — the same lab binary always yields
// the same bytes. See patchNestedSeccomp for the delta and its failure
// modes.
func NestedSeccompProfile() ([]byte, error) {
	return patchNestedSeccomp(upstreamSeccomp)
}

// WriteNestedSeccomp generates the profile and publishes it at
// NestedSeccompPath(stateDir), returning that path. Called at startup when
// nested containers may be used; rewritten every time, so a lab upgrade
// that carries a refreshed upstream copy reaches the file at its next
// start.
//
// <state>/containers is created 0700: everything in it — this profile and
// the shared image store — is read only by the service user, as lab itself
// and as the rootless podman it runs (the same uid), and cmd/lab already
// creates the state dir itself 0700, so a group or world bit here would
// grant nothing anyone uses. MkdirAll leaves an existing directory's mode
// alone. The file is 0644: it is upstream data plus a public delta, nothing
// secret; the directory is the access boundary. The write is atomic
// (fsx.WriteFileAtomic: same-directory temp file, fsync, rename), so a
// podman starting a nested run concurrently with a lab restart reads either
// the old profile or the new one, never a torn file.
func WriteNestedSeccomp(stateDir string) (string, error) {
	profile, err := NestedSeccompProfile()
	if err != nil {
		return "", err
	}
	path := NestedSeccompPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("nested seccomp profile: %w", err)
	}
	if err := fsx.WriteFileAtomic(path, profile, 0o644); err != nil {
		return "", fmt.Errorf("nested seccomp profile: %w", err)
	}
	return path, nil
}

// seccompRuleShape is the part of one syscalls[] rule the delta inspects.
// Everything else in a rule (comments, errno strings, args' operands, any
// field a future upstream adds) is never decoded into a typed value, so it
// survives the round trip byte-for-byte modulo JSON formatting.
type seccompRuleShape struct {
	Names    []string        `json:"names"`
	Action   string          `json:"action"`
	Args     json.RawMessage `json:"args"`
	Includes json.RawMessage `json:"includes"`
	Excludes json.RawMessage `json:"excludes"`
}

// patchNestedSeccomp applies the ADR-0086 delta to a containers/common
// profile:
//
//   - add sethostname and setdomainname to THE unconditional allow rule —
//     action SCMP_ACT_ALLOW, no args, and no includes or excludes
//     conditions (no caps, arches or minKernel). That is the big allow
//     list, and it is NOT rule 0: upstream's rule 0 is an SCMP_ACT_ERRNO
//     list of always-denied syscalls. Exactly one such rule must exist;
//     none, or several (which one would be "the" list?), is an error.
//   - remove both from every SCMP_ACT_ERRNO rule whose excludes.caps names
//     CAP_SYS_ADMIN — the rule that denies them to a container without the
//     cap. Upstream has more than one rule of that shape (bpf and
//     perf_event_open have their own, excluding CAP_SYS_ADMIN together with
//     CAP_BPF/CAP_PERFMON), and stripping the two names from all of them is
//     exactly "no CAP_SYS_ADMIN-gated denial of them remains". At least one
//     such rule must exist, else the file is not the shape this delta was
//     written against.
//
// The conditional allow rule that includes CAP_SYS_ADMIN is left alone: it
// applies only to a container holding the cap, where it duplicates the
// unconditional allow with the same action, which is harmless.
//
// Rules are located by shape, never by index, and an upstream refresh that
// breaks either shape fails here — loudly, with the test catching it before
// a release does. The JSON is decoded generically (top level and each rule
// as raw fields), so fields this code does not know survive the round
// trip. Idempotent: a name already in the allow rule is not added twice,
// and applying the delta to its own output yields the same bytes.
func patchNestedSeccomp(upstream []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(upstream, &top); err != nil {
		return nil, fmt.Errorf("nested seccomp profile: decoding upstream profile: %w", err)
	}
	var rules []map[string]json.RawMessage
	if err := json.Unmarshal(top["syscalls"], &rules); err != nil {
		return nil, fmt.Errorf("nested seccomp profile: decoding syscalls: %w", err)
	}

	allowIdx := -1
	var errnoIdx []int
	shapes := make([]seccompRuleShape, len(rules))
	for i, r := range rules {
		raw, err := json.Marshal(r)
		if err != nil {
			return nil, fmt.Errorf("nested seccomp profile: rule %d: %w", i, err)
		}
		if err := json.Unmarshal(raw, &shapes[i]); err != nil {
			return nil, fmt.Errorf("nested seccomp profile: rule %d: %w", i, err)
		}
		s := shapes[i]
		switch {
		case s.Action == seccompActAllow && emptyJSON(s.Args) && emptyJSON(s.Includes) && emptyJSON(s.Excludes):
			if allowIdx >= 0 {
				return nil, fmt.Errorf("nested seccomp profile: two unconditional %s rules (%d and %d); the upstream profile changed shape", seccompActAllow, allowIdx, i)
			}
			allowIdx = i
		case s.Action == seccompActErrno && excludesCap(s.Excludes, capSysAdmin):
			errnoIdx = append(errnoIdx, i)
		}
	}
	if allowIdx < 0 {
		return nil, fmt.Errorf("nested seccomp profile: no unconditional %s rule (no args, includes or excludes) in the upstream profile", seccompActAllow)
	}
	if len(errnoIdx) == 0 {
		return nil, fmt.Errorf("nested seccomp profile: no %s rule excluding %s in the upstream profile", seccompActErrno, capSysAdmin)
	}

	allow := shapes[allowIdx].Names
	for _, n := range nestedSeccompAllow {
		if !slices.Contains(allow, n) {
			allow = append(allow, n)
		}
	}
	if err := setNames(rules[allowIdx], allow); err != nil {
		return nil, err
	}
	for _, i := range errnoIdx {
		kept := slices.DeleteFunc(slices.Clone(shapes[i].Names), func(n string) bool {
			return slices.Contains(nestedSeccompAllow, n)
		})
		if err := setNames(rules[i], kept); err != nil {
			return nil, err
		}
	}

	syscalls, err := json.Marshal(rules)
	if err != nil {
		return nil, fmt.Errorf("nested seccomp profile: encoding syscalls: %w", err)
	}
	top["syscalls"] = syscalls

	// An Encoder, not MarshalIndent, so HTML escaping stays off: upstream
	// comments carry '&' and '>' that need no \u escapes in a file only
	// podman reads. Map keys come out sorted, which is what makes the
	// output deterministic.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "\t")
	if err := enc.Encode(top); err != nil {
		return nil, fmt.Errorf("nested seccomp profile: encoding: %w", err)
	}
	return buf.Bytes(), nil
}

// setNames replaces one rule's names field. A rule left with no names keeps
// an empty list ([]), never null — containers/common reads either, but []
// is what upstream itself writes.
func setNames(rule map[string]json.RawMessage, names []string) error {
	if names == nil {
		names = []string{}
	}
	raw, err := json.Marshal(names)
	if err != nil {
		return fmt.Errorf("nested seccomp profile: encoding names: %w", err)
	}
	rule["names"] = raw
	return nil
}

// emptyJSON reports whether a rule field carries no condition: absent,
// null, an empty array or object, or an object whose every member is
// itself empty ("" / 0 / [] / {} / null). Upstream writes "includes": {}
// and "args": [] for an unconditional rule; the recursive object case keeps
// a refresh that spells it {"caps": []} from losing the allow rule.
func emptyJSON(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	return emptyValue(v)
}

func emptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case float64:
		return x == 0
	case bool:
		return !x
	case []any:
		return len(x) == 0
	case map[string]any:
		for _, m := range x {
			if !emptyValue(m) {
				return false
			}
		}
		return true
	}
	return false
}

// excludesCap reports whether a rule's excludes object lists capability in
// its caps. A malformed excludes is "no" — the rule then simply does not
// match the shape, and the at-least-one check reports the profile.
func excludesCap(raw json.RawMessage, capability string) bool {
	if emptyJSON(raw) {
		return false
	}
	var ex struct {
		Caps []string `json:"caps"`
	}
	if err := json.Unmarshal(raw, &ex); err != nil {
		return false
	}
	return slices.Contains(ex.Caps, capability)
}
