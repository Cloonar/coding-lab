package claudecode

import (
	"path/filepath"
	"testing"
)

// MasterStoreSpec (issue #206) must resolve through masterConfigDir — the ONE
// master resolver logout.go owns — so the declaration, the logout
// rm-escalation, the InjectCredentials source, and the refresh poke's env pin
// can never disagree. Override-awareness (a set CLAUDE_CONFIG_DIR outranks
// the HOME fallback, re-resolved on every call) is the issue #211 criterion;
// the conformance suite pins it generically, this test pins claude's exact
// declared values.
func TestMasterStoreSpec(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	spec := p.MasterStoreSpec()
	if spec.EnvVar != "CLAUDE_CONFIG_DIR" {
		t.Errorf("EnvVar = %q; want %q", spec.EnvVar, "CLAUDE_CONFIG_DIR")
	}
	if spec.HomeSubdir != ".claude" {
		t.Errorf("HomeSubdir = %q; want %q", spec.HomeSubdir, ".claude")
	}
	if want := filepath.Join(home, ".claude"); spec.HostDir != want {
		t.Errorf("HostDir with no override = %q; want the HOME fallback %q", spec.HostDir, want)
	}

	// The override must move the resolved dir on the NEXT call — a spec
	// cached at construction (or first call) would keep pointing at the old
	// store after the operator repointed the env (issue #211).
	override := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", override)
	if got := p.MasterStoreSpec().HostDir; got != override {
		t.Errorf("HostDir after setting CLAUDE_CONFIG_DIR = %q; want the override %q (resolved freshly per call)", got, override)
	}

	// The package-level MasterStore (the wiring layer's pre-construction Spec
	// closure) and the method must stay ONE resolver — a divergence would let
	// the containerized login mount a different store than the adapter's own
	// master-store ops target.
	if fn, m := MasterStore(), p.MasterStoreSpec(); fn != m {
		t.Errorf("MasterStore() = %+v; MasterStoreSpec() = %+v — function and method must agree", fn, m)
	}
}
