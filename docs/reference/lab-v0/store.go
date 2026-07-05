package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store is the on-disk persistence for two per-project facts: the claude.ai
// deep link captured for a running session, and the timestamp of the last
// Start click (used to order the index by recency). It serialises to a single
// JSON file rewritten atomically on each mutation. URLs come and go with the
// session lifecycle; lastOpenedAt sticks around so a Stop-ed project still
// retains its position in the recency sort.
type Store struct {
	path string

	mu   sync.Mutex
	data storeData
}

type storeData struct {
	Projects map[string]*projectState `json:"projects"`
	// Spawn is the single GLOBAL (not per-project) model + effort that governs
	// every newly-spawned session (#156). A pointer with omitempty so a store that
	// has never had it set serialises without the key and reads back as the
	// documented defaults via SpawnConfig.
	Spawn *spawnSettings `json:"spawn,omitempty"`
}

// spawnSettings is the persisted global spawn setting: the Claude Code model
// alias and effort level lab passes to every new `claude` spawn. Both are
// validated against the closed allowlists before they are ever written (see
// SetSpawnConfig / validateSpawnConfig), so a value read back from disk is always
// one lab offered — except a hand-edited file, which SpawnConfig falls back to
// the defaults for field-by-field when the field is blank.
type spawnSettings struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

type projectState struct {
	URL          string `json:"url,omitempty"`
	LastOpenedAt string `json:"lastOpenedAt,omitempty"` // RFC3339 UTC
	// AutoEnabled is the per-project "automatic AFK runs" toggle. Keyed by
	// project name (like LastOpenedAt, not the session name URLs use), so it is
	// one fact per project regardless of how many instances run. omitempty keeps
	// the common (off) case out of the file; a missing field reads back as false.
	AutoEnabled bool `json:"autoEnabled,omitempty"`
	// ConsecutiveFailures counts AFK runs that ended in a failure (death or
	// timeout) back-to-back for this project; a successful run or a UI Reset
	// zeroes it. At afkPauseThreshold the scheduler stops launching automatic runs
	// for the project (manual Start is never gated). Project-name-keyed and
	// persisted like AutoEnabled — omitempty keeps the common (zero) case out of
	// the file, and a missing field reads back as 0.
	ConsecutiveFailures int `json:"consecutiveFailures,omitempty"`
}

func NewStore(path string) *Store {
	return &Store{path: path, data: storeData{Projects: map[string]*projectState{}}}
}

// Load reads the state file if present. A missing file is not an error — the
// store starts empty. A malformed file IS returned as an error so the caller
// (main.go) can decide whether to log and continue or abort.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("read %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var d storeData
	if err := json.Unmarshal(data, &d); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	if d.Projects == nil {
		d.Projects = map[string]*projectState{}
	}
	s.data = d
	return nil
}

// URL returns the stored URL for name, or "" if none.
func (s *Store) URL(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.data.Projects[name]; p != nil {
		return p.URL
	}
	return ""
}

// LastOpenedAt returns the stored timestamp and true, or zero+false if
// unstamped. Parses on read; bad strings count as unstamped (the file was
// probably hand-edited or written by a future version).
func (s *Store) LastOpenedAt(name string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.data.Projects[name]
	if p == nil || p.LastOpenedAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, p.LastOpenedAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// SetURL records the URL for name and persists.
func (s *Store) SetURL(name, url string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.entryLocked(name)
	p.URL = url
	return s.saveLocked()
}

// ForgetURL clears the URL for name (preserving lastOpenedAt) and persists.
// No-op if name has no entry.
func (s *Store) ForgetURL(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.data.Projects[name]
	if p == nil || p.URL == "" {
		return nil
	}
	p.URL = ""
	return s.saveLocked()
}

// StampOpened sets lastOpenedAt for name to t (UTC, RFC3339) and persists.
func (s *Store) StampOpened(name string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.entryLocked(name)
	p.LastOpenedAt = t.UTC().Format(time.RFC3339)
	return s.saveLocked()
}

// AutoEnabled reports whether project (by name) has automatic AFK runs turned
// on. An unknown or never-toggled project defaults to off.
func (s *Store) AutoEnabled(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.data.Projects[name]; p != nil {
		return p.AutoEnabled
	}
	return false
}

// SetAutoEnabled records project's automatic-AFK toggle and persists, so the
// flag survives a lab restart and the scheduler re-reads it next tick.
func (s *Store) SetAutoEnabled(name string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.entryLocked(name)
	p.AutoEnabled = enabled
	return s.saveLocked()
}

// ConsecutiveFailures returns the persisted count of back-to-back failed AFK
// runs for project (by name). An unknown or never-failed project reads back as 0.
func (s *Store) ConsecutiveFailures(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p := s.data.Projects[name]; p != nil {
		return p.ConsecutiveFailures
	}
	return 0
}

// IncrementFailures bumps project's consecutive-failure count by one, persists,
// and returns the new value. The read-modify-write happens entirely under the
// store lock so the reaper goroutine and the Reset handler — which both touch
// this counter concurrently — never race; callers must NOT do their own
// get-then-set (ADR-0007).
func (s *Store) IncrementFailures(name string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.entryLocked(name)
	p.ConsecutiveFailures++
	return p.ConsecutiveFailures, s.saveLocked()
}

// ResetFailures zeroes project's consecutive-failure count and persists, the
// atomic re-arm shared by the success reap and the UI Reset action. A no-op
// (no write) when the counter is already zero, so an unbroken run of successes
// doesn't rewrite the file on every reap.
func (s *Store) ResetFailures(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.data.Projects[name]
	if p == nil || p.ConsecutiveFailures == 0 {
		return nil
	}
	p.ConsecutiveFailures = 0
	return s.saveLocked()
}

// SpawnConfig returns the global model + effort that govern new spawns, falling
// back to the documented defaults for any field never set (fresh state) or left
// blank by a hand-edited file (#156). This is the accessor every spawn path reads
// at spawn time (main wires it into Sessions.spawnConfig), so a change takes
// effect on the next spawn with no lab restart; running sessions keep the model
// fixed in their own process. A present-but-out-of-allowlist value (only reachable
// by hand-editing — the setter validates) is passed through and fails loud at the
// spawn, not silently rewritten here.
func (s *Store) SpawnConfig() (model, effort string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	model, effort = defaultSpawnModel, defaultSpawnEffort
	if sp := s.data.Spawn; sp != nil {
		if sp.Model != "" {
			model = sp.Model
		}
		if sp.Effort != "" {
			effort = sp.Effort
		}
	}
	return model, effort
}

// SetSpawnConfig validates model + effort against the closed allowlists and, only
// if BOTH pass, persists them as the global spawn setting. An out-of-allowlist
// value is rejected (the error is surfaced in the UI banner) and NOTHING is
// written — the setting is global, so persisting a bad one would break every
// future spawn (#156). Validation happens before the lock; the write is then a
// single atomic replace of the whole pair, so a concurrent SpawnConfig reader
// never observes a half-applied model/effort.
func (s *Store) SetSpawnConfig(model, effort string) error {
	if err := validateSpawnConfig(model, effort); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Spawn = &spawnSettings{Model: model, Effort: effort}
	return s.saveLocked()
}

// PruneDeadURLs drops the URL on every entry whose name is not in live. Used
// on startup so a URL recorded for a session that is no longer running (lab
// crashed, claude exited, microvm rebooted) doesn't linger as a broken link.
// Timestamps are untouched.
func (s *Store) PruneDeadURLs(live map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for name, p := range s.data.Projects {
		if p.URL != "" && !live[name] {
			p.URL = ""
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.saveLocked()
}

func (s *Store) entryLocked(name string) *projectState {
	p := s.data.Projects[name]
	if p == nil {
		p = &projectState{}
		s.data.Projects[name] = p
	}
	return p
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(s.path), err)
	}
	out, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return writeAtomic(s.path, out)
}
