package codex

import (
	"path/filepath"
	"testing"
)

// MasterStoreSpec (issue #206) must resolve through credentialsPath's own
// directory (== codexHomeDir(p.loginDir)) — the same chain logout's
// rm-escalation and the refresh probe's CODEX_HOME pin use — so the
// declaration can never disagree with them. Override-awareness (a set
// CODEX_HOME outranks the <loginDir>/.codex fallback, re-resolved on every
// call) is the issue #211 criterion; the conformance suite pins it
// generically, this test pins codex's exact declared values.
func TestMasterStoreSpec(t *testing.T) {
	p, _ := testProvider(t, newFakeRunner())

	t.Setenv("CODEX_HOME", "")
	spec := p.MasterStoreSpec()
	if spec.EnvVar != "CODEX_HOME" {
		t.Errorf("EnvVar = %q; want %q", spec.EnvVar, "CODEX_HOME")
	}
	if spec.HomeSubdir != ".codex" {
		t.Errorf("HomeSubdir = %q; want %q", spec.HomeSubdir, ".codex")
	}
	if want := filepath.Join(p.loginDir, ".codex"); spec.HostDir != want {
		t.Errorf("HostDir with no override = %q; want the loginDir fallback %q", spec.HostDir, want)
	}

	// The override must move the resolved dir on the NEXT call — a spec
	// cached at construction (or first call) would keep pointing at the old
	// store after the operator repointed the env (issue #211).
	override := t.TempDir()
	t.Setenv("CODEX_HOME", override)
	if got := p.MasterStoreSpec().HostDir; got != override {
		t.Errorf("HostDir after setting CODEX_HOME = %q; want the override %q (resolved freshly per call)", got, override)
	}

	// The package-level MasterStore (the wiring layer's pre-construction Spec
	// closure) and the method must stay ONE resolver — a divergence would let
	// the containerized login mount a different store than the adapter's own
	// master-store ops target.
	if fn, m := MasterStore(p.loginDir), p.MasterStoreSpec(); fn != m {
		t.Errorf("MasterStore(loginDir) = %+v; MasterStoreSpec() = %+v — function and method must agree", fn, m)
	}
}
