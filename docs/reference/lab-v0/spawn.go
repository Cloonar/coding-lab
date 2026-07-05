package main

import "fmt"

// The global, persisted Claude model + effort that govern every newly-spawned
// lab session — manual Start, New instance, and AFK runs alike (#156). A change
// takes effect on the *next* spawn with no lab restart; already-running sessions
// keep the model fixed in their process at spawn. This is lab's own --model /
// --effort spawn flag only — the machine-wide interactive claude default
// (home-manager settings.json) is intentionally left alone.
//
// This file is the single server-side source of truth for the two closed
// allowlists: it backs BOTH the dropdown rendering (pageData.ModelOptions /
// EffortOptions) and the endpoint's validation (validateSpawnConfig), so the two
// can never drift.

// modelOption is one entry in the model dropdown: Value is the Claude Code alias
// passed to `claude --model`, Label is the human text shown in the UI.
type modelOption struct {
	Value string
	Label string
}

// spawnModels is the closed allowlist of model aliases lab offers, in dropdown
// order. Deliberately Claude Code *family aliases* (not pinned ids like
// claude-opus-4-8[1m]) so the list tracks the latest of each family and never
// goes stale (#156) — the reverse of lab's original "pin the exact id" choice,
// now that the model is a visible, user-chosen setting. The [1m] suffix is
// carried only where a 1M-context variant is included on the plan: Opus maps to
// opus[1m]; Sonnet stays plain because its 1M would bill extra usage credits;
// Fable is inherently 1M; Haiku has no 1M variant. The best / opusplan aliases
// and a paid sonnet[1m] option are intentionally excluded.
var spawnModels = []modelOption{
	{Value: "opus[1m]", Label: "Opus (1M)"},
	{Value: "sonnet", Label: "Sonnet"},
	{Value: "fable", Label: "Fable"},
	{Value: "haiku", Label: "Haiku"},
}

// spawnEfforts is the closed allowlist of effort levels, in dropdown order,
// passed through to `claude --effort` verbatim. lab does NO per-model coupling:
// Claude Code itself clamps an unsupported model+effort combination per its own
// documented rule, so every level is offered for every model.
var spawnEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// defaultSpawnModel / defaultSpawnEffort are the documented defaults in force
// when the setting has never been set (fresh state): opus[1m] + max, preserving
// lab's original hardcoded spawn behavior (#156). Both are members of the
// allowlists above (TestSpawnDefaults_areValid guards that), so the unset →
// defaults path always yields a spawn lab would itself accept.
const (
	defaultSpawnModel  = "opus[1m]"
	defaultSpawnEffort = "max"
)

func validSpawnModel(v string) bool {
	for _, m := range spawnModels {
		if m.Value == v {
			return true
		}
	}
	return false
}

func validSpawnEffort(v string) bool {
	for _, e := range spawnEfforts {
		if e == v {
			return true
		}
	}
	return false
}

// validateSpawnConfig is the one gate every persisted model+effort passes
// through (called by Store.SetSpawnConfig). It rejects anything outside the two
// closed allowlists with a UI-surfacable message. The value is global and
// persisted, so a bad one would otherwise break every future spawn — hence it is
// rejected loudly and never stored.
func validateSpawnConfig(model, effort string) error {
	if !validSpawnModel(model) {
		return fmt.Errorf("unknown model %q", model)
	}
	if !validSpawnEffort(effort) {
		return fmt.Errorf("unknown effort %q", effort)
	}
	return nil
}
