package main

import "testing"

// The model + effort dropdowns are a CLOSED allowlist (#156): these pin the exact
// option sets the acceptance criteria require, so adding/removing/renaming one is
// a deliberate, test-visible change. Order matters — it is the dropdown order.
func TestSpawnAllowlists_exactContents(t *testing.T) {
	wantModels := []modelOption{
		{Value: "opus[1m]", Label: "Opus (1M)"},
		{Value: "sonnet", Label: "Sonnet"},
		{Value: "fable", Label: "Fable"},
		{Value: "haiku", Label: "Haiku"},
	}
	if len(spawnModels) != len(wantModels) {
		t.Fatalf("spawnModels = %+v; want %+v", spawnModels, wantModels)
	}
	for i, w := range wantModels {
		if spawnModels[i] != w {
			t.Errorf("spawnModels[%d] = %+v; want %+v", i, spawnModels[i], w)
		}
	}

	wantEfforts := []string{"low", "medium", "high", "xhigh", "max"}
	if len(spawnEfforts) != len(wantEfforts) {
		t.Fatalf("spawnEfforts = %v; want %v", spawnEfforts, wantEfforts)
	}
	for i, w := range wantEfforts {
		if spawnEfforts[i] != w {
			t.Errorf("spawnEfforts[%d] = %q; want %q", i, spawnEfforts[i], w)
		}
	}
}

// The unset → defaults path (fresh state) must yield a spawn lab would itself
// accept, or it would spawn an invalid --model/--effort. opus[1m] + max preserves
// today's behavior (#156).
func TestSpawnDefaults_areValid(t *testing.T) {
	if defaultSpawnModel != "opus[1m]" {
		t.Errorf("defaultSpawnModel = %q; want opus[1m]", defaultSpawnModel)
	}
	if defaultSpawnEffort != "max" {
		t.Errorf("defaultSpawnEffort = %q; want max", defaultSpawnEffort)
	}
	if err := validateSpawnConfig(defaultSpawnModel, defaultSpawnEffort); err != nil {
		t.Errorf("documented defaults rejected by validateSpawnConfig: %v", err)
	}
}

func TestValidateSpawnConfig(t *testing.T) {
	for _, tc := range []struct {
		name          string
		model, effort string
		wantErr       bool
	}{
		{"defaults", "opus[1m]", "max", false},
		{"every model with low", "sonnet", "low", false},
		{"fable medium", "fable", "medium", false},
		{"haiku xhigh", "haiku", "xhigh", false},
		{"high", "opus[1m]", "high", false},
		{"unknown model", "gpt-4", "max", true},
		{"excluded best alias", "best", "max", true},
		{"excluded opusplan alias", "opusplan", "max", true},
		{"excluded paid sonnet[1m]", "sonnet[1m]", "max", true},
		{"pinned id is no longer accepted", "claude-opus-4-8[1m]", "max", true},
		{"unknown effort", "opus[1m]", "extreme", true},
		{"empty model", "", "max", true},
		{"empty effort", "opus[1m]", "", true},
		{"both empty", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpawnConfig(tc.model, tc.effort)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateSpawnConfig(%q,%q) err = %v; wantErr %v", tc.model, tc.effort, err, tc.wantErr)
			}
		})
	}
}
