package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_loadMissingFileIsEmpty(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err := s.Load(); err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if got := s.URL("anything"); got != "" {
		t.Errorf("URL on empty store = %q; want empty", got)
	}
	if _, ok := s.LastOpenedAt("anything"); ok {
		t.Errorf("LastOpenedAt on empty store returned ok=true")
	}
}

func TestStore_corruptFileIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	if err := s.Load(); err == nil {
		t.Errorf("expected Load to return error on corrupt JSON")
	}
}

func TestStore_setAndForgetURLPreservesTimestamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path)

	stamp := time.Date(2026, 5, 22, 18, 0, 0, 0, time.UTC)
	if err := s.StampOpened("proj", stamp); err != nil {
		t.Fatal(err)
	}
	if err := s.SetURL("proj", "https://claude.ai/code/abc"); err != nil {
		t.Fatal(err)
	}
	if got := s.URL("proj"); got != "https://claude.ai/code/abc" {
		t.Errorf("URL after set = %q; want %q", got, "https://claude.ai/code/abc")
	}

	if err := s.ForgetURL("proj"); err != nil {
		t.Fatal(err)
	}
	if got := s.URL("proj"); got != "" {
		t.Errorf("URL after forget = %q; want empty", got)
	}
	got, ok := s.LastOpenedAt("proj")
	if !ok || !got.Equal(stamp) {
		t.Errorf("LastOpenedAt after forget = (%v, %v); want (%v, true)", got, ok, stamp)
	}
}

func TestStore_persistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	stamp := time.Date(2026, 5, 22, 18, 0, 0, 0, time.UTC)

	s1 := NewStore(path)
	if err := s1.StampOpened("proj", stamp); err != nil {
		t.Fatal(err)
	}
	if err := s1.SetURL("proj", "https://claude.ai/code/abc"); err != nil {
		t.Fatal(err)
	}

	s2 := NewStore(path)
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s2.URL("proj"); got != "https://claude.ai/code/abc" {
		t.Errorf("URL after reload = %q; want %q", got, "https://claude.ai/code/abc")
	}
	got, ok := s2.LastOpenedAt("proj")
	if !ok || !got.Equal(stamp) {
		t.Errorf("LastOpenedAt after reload = (%v, %v); want (%v, true)", got, ok, stamp)
	}
}

func TestStore_autoEnabledRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	s1 := NewStore(path)
	if s1.AutoEnabled("proj") {
		t.Error("AutoEnabled on a never-toggled project = true; want false (default off)")
	}
	if err := s1.SetAutoEnabled("proj", true); err != nil {
		t.Fatal(err)
	}
	// A stamp on the same project must coexist with the toggle (both are
	// project-name-keyed facts on the one entry).
	stamp := time.Date(2026, 5, 22, 18, 0, 0, 0, time.UTC)
	if err := s1.StampOpened("proj", stamp); err != nil {
		t.Fatal(err)
	}

	// Survives a restart: a fresh Store reading the same file sees the flag on.
	s2 := NewStore(path)
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if !s2.AutoEnabled("proj") {
		t.Error("AutoEnabled after reload = false; want true (must persist across restart)")
	}
	if got, ok := s2.LastOpenedAt("proj"); !ok || !got.Equal(stamp) {
		t.Errorf("LastOpenedAt after reload = (%v, %v); want (%v, true) — toggle must not clobber the stamp", got, ok, stamp)
	}

	// Toggling back off persists too, and reads back as the default.
	if err := s2.SetAutoEnabled("proj", false); err != nil {
		t.Fatal(err)
	}
	s3 := NewStore(path)
	if err := s3.Load(); err != nil {
		t.Fatal(err)
	}
	if s3.AutoEnabled("proj") {
		t.Error("AutoEnabled after toggling off + reload = true; want false")
	}
}

func TestStore_consecutiveFailuresRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	s1 := NewStore(path)
	if got := s1.ConsecutiveFailures("proj"); got != 0 {
		t.Errorf("ConsecutiveFailures on a never-failed project = %d; want 0 (default)", got)
	}
	// Increment is an atomic read-modify-write returning the new value.
	for want := 1; want <= 3; want++ {
		got, err := s1.IncrementFailures("proj")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("IncrementFailures #%d returned %d; want %d", want, got, want)
		}
	}
	// The counter coexists with the other project-name-keyed facts on the one entry.
	if err := s1.SetAutoEnabled("proj", true); err != nil {
		t.Fatal(err)
	}

	// Survives a restart: a fresh Store reading the same file sees the count.
	s2 := NewStore(path)
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s2.ConsecutiveFailures("proj"); got != 3 {
		t.Errorf("ConsecutiveFailures after reload = %d; want 3 (must persist across restart)", got)
	}
	if !s2.AutoEnabled("proj") {
		t.Error("AutoEnabled after reload = false; want true — the counter must not clobber the toggle")
	}

	// Reset zeroes it and persists as the default; the toggle is left untouched
	// (Reset re-arms by clearing only the counter, never by flipping auto).
	if err := s2.ResetFailures("proj"); err != nil {
		t.Fatal(err)
	}
	if got := s2.ConsecutiveFailures("proj"); got != 0 {
		t.Errorf("ConsecutiveFailures after reset = %d; want 0", got)
	}
	s3 := NewStore(path)
	if err := s3.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s3.ConsecutiveFailures("proj"); got != 0 {
		t.Errorf("ConsecutiveFailures after reset + reload = %d; want 0", got)
	}
	if !s3.AutoEnabled("proj") {
		t.Error("Reset cleared the auto toggle; want it untouched (Reset only zeroes the counter)")
	}
}

func TestStore_spawnConfigDefaultsAndRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	s1 := NewStore(path)
	// Fresh state: the documented defaults (opus[1m] + max), preserving today's
	// behavior.
	if m, e := s1.SpawnConfig(); m != defaultSpawnModel || e != defaultSpawnEffort {
		t.Errorf("SpawnConfig on a fresh store = (%q,%q); want (%q,%q)", m, e, defaultSpawnModel, defaultSpawnEffort)
	}

	// A valid set persists; it is GLOBAL, so it coexists with a per-project fact on
	// the same store without either clobbering the other.
	if err := s1.SetSpawnConfig("sonnet", "high"); err != nil {
		t.Fatalf("SetSpawnConfig(valid): %v", err)
	}
	if err := s1.SetAutoEnabled("proj", true); err != nil {
		t.Fatal(err)
	}

	// Survives a restart: a fresh Store reading the same file sees the chosen pair.
	s2 := NewStore(path)
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if m, e := s2.SpawnConfig(); m != "sonnet" || e != "high" {
		t.Errorf("SpawnConfig after reload = (%q,%q); want (sonnet,high) — must persist across restart", m, e)
	}
	if !s2.AutoEnabled("proj") {
		t.Error("the global spawn setting clobbered a per-project fact (or vice versa)")
	}

	// An out-of-allowlist value is rejected AND nothing is written — the prior good
	// value survives, so one bad POST can't break every future spawn (#156).
	if err := s2.SetSpawnConfig("gpt-4", "max"); err == nil {
		t.Error("SetSpawnConfig(invalid model) returned nil; want a rejection")
	}
	if err := s2.SetSpawnConfig("sonnet", "extreme"); err == nil {
		t.Error("SetSpawnConfig(invalid effort) returned nil; want a rejection")
	}
	if m, e := s2.SpawnConfig(); m != "sonnet" || e != "high" {
		t.Errorf("SpawnConfig after a rejected set = (%q,%q); want the prior (sonnet,high) — a bad value must not persist", m, e)
	}
	// And the rejection is durable: nothing leaked to disk.
	s3 := NewStore(path)
	if err := s3.Load(); err != nil {
		t.Fatal(err)
	}
	if m, e := s3.SpawnConfig(); m != "sonnet" || e != "high" {
		t.Errorf("SpawnConfig after rejected set + reload = (%q,%q); want (sonnet,high)", m, e)
	}
}

// A hand-edited / partial file with a blank field reads that field back as the
// default rather than spawning a `--effort ""` — the setter can never write a
// blank (it validates), so this only guards the off-by-hand case.
func TestStore_spawnConfigBlankFieldFallsBackToDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"projects":{},"spawn":{"model":"fable","effort":""}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if m, e := s.SpawnConfig(); m != "fable" || e != defaultSpawnEffort {
		t.Errorf("SpawnConfig with a blank effort = (%q,%q); want (fable,%q)", m, e, defaultSpawnEffort)
	}
}

func TestStore_pruneDeadURLsKeepsLiveAndTimestamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := NewStore(path)

	stampA := time.Date(2026, 5, 22, 12, 0, 0, 0, time.UTC)
	stampB := time.Date(2026, 5, 22, 13, 0, 0, 0, time.UTC)
	if err := s.StampOpened("alive", stampA); err != nil {
		t.Fatal(err)
	}
	if err := s.SetURL("alive", "https://claude.ai/code/alive"); err != nil {
		t.Fatal(err)
	}
	if err := s.StampOpened("dead", stampB); err != nil {
		t.Fatal(err)
	}
	if err := s.SetURL("dead", "https://claude.ai/code/dead"); err != nil {
		t.Fatal(err)
	}

	if err := s.PruneDeadURLs(map[string]bool{"alive": true}); err != nil {
		t.Fatal(err)
	}

	if got := s.URL("alive"); got != "https://claude.ai/code/alive" {
		t.Errorf("alive URL pruned; got %q", got)
	}
	if got := s.URL("dead"); got != "" {
		t.Errorf("dead URL not pruned; got %q", got)
	}
	if got, ok := s.LastOpenedAt("dead"); !ok || !got.Equal(stampB) {
		t.Errorf("dead timestamp lost; got (%v, %v)", got, ok)
	}
}

func TestStore_atomicWriteSurvivesReload(t *testing.T) {
	// Roundtrip every field through a fresh Store to catch silent serialisation
	// bugs (omitempty too aggressive, field name typo, etc.).
	path := filepath.Join(t.TempDir(), "state.json")
	stamp := time.Date(2026, 5, 22, 18, 0, 0, 0, time.UTC)

	s := NewStore(path)
	if err := s.StampOpened("proj", stamp); err != nil {
		t.Fatal(err)
	}
	if err := s.SetURL("proj", "https://claude.ai/code/abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetURL("proj"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatalf("state file empty after writes")
	}

	s2 := NewStore(path)
	if err := s2.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s2.URL("proj"); got != "" {
		t.Errorf("URL reloaded = %q; want empty (was forgotten)", got)
	}
	if got, ok := s2.LastOpenedAt("proj"); !ok || !got.Equal(stamp) {
		t.Errorf("LastOpenedAt reloaded = (%v, %v); want (%v, true)", got, ok, stamp)
	}
}
