package httpapi

// Settings surface (pinned M5 contract): GET returns every settings row with
// typed values (the integer knobs as JSON numbers); PATCH validates the whole
// body first — unknown keys, non-integers, out-of-range intervals, and spawn
// defaults outside the provider catalogs are 400s that write NOTHING — then
// upserts and returns the updated map. No event is published and no restart
// is needed: the runtime loops re-read settings every tick (D12c), and spawn
// paths read them per call.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// settingsIntMin is the closed set of integer settings keys with each key's
// minimum: a zero cap or budget would deadlock every spawn, and sub-5s ticks
// would hammer tmux and the tracker (pinned: budget > 0, ticks >= 5s).
var settingsIntMin = map[string]int{
	store.SettingMaxInstances:         1,
	store.SettingAFKBudgetMinutes:     1,
	store.SettingAFKTickSeconds:       5,
	store.SettingAFKScheduleSeconds:   5,
	store.SettingSweepIntervalMinutes: 1,
}

// typedSettings renders a raw settings map with the integer keys as JSON
// numbers. A garbled stored value (unparseable int) is passed through as the
// raw string — visible to the operator rather than silently rewritten.
func typedSettings(all map[string]string) map[string]any {
	out := make(map[string]any, len(all))
	for k, v := range all {
		if _, isInt := settingsIntMin[k]; isInt {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				out[k] = n
				continue
			}
		}
		out[k] = v
	}
	return out
}

// handleSettingsGet is GET /api/v1/settings: every key+value, typed.
func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.AllSettings(r.Context())
	if err != nil {
		s.internalError(w, "loading settings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": typedSettings(all)})
}

// handleSettingsPatch is PATCH /api/v1/settings {key: value, …}: validate
// everything, then write everything, then answer 200 with the full updated
// typed map. Integer values arrive as JSON numbers or numeric strings and are
// stored canonically.
func (s *Server) handleSettingsPatch(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if decodeJSON(w, r, &body) != nil {
		return
	}

	updates := make(map[string]string, len(body))
	for key, raw := range body {
		if floor, isInt := settingsIntMin[key]; isInt {
			n, err := parseSettingInt(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be an integer", key))
				return
			}
			if n < floor {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be at least %d", key, floor))
				return
			}
			updates[key] = strconv.Itoa(n)
			continue
		}
		switch key {
		case store.SettingSpawnModelDefault:
			v, err := parseSettingString(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a string", key))
				return
			}
			if !s.spawnDefaultAllowed(v, true) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown model %q", v))
				return
			}
			updates[key] = v
		case store.SettingSpawnEffortDefault:
			v, err := parseSettingString(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a string", key))
				return
			}
			if !s.spawnDefaultAllowed(v, false) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown effort %q", v))
				return
			}
			updates[key] = v
		case store.SettingGitAuthorName, store.SettingGitAuthorEmail:
			v, err := parseSettingString(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a string", key))
				return
			}
			updates[key] = v
		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown setting %q", key))
			return
		}
	}

	// All valid — write. (Individual upserts: a store failure mid-way is a
	// 500; validation failures above never reach here, so a 400 writes
	// nothing.)
	for key, value := range updates {
		if err := s.store.SetSetting(r.Context(), key, value); err != nil {
			s.internalError(w, "saving settings", err)
			return
		}
	}
	all, err := s.store.AllSettings(r.Context())
	if err != nil {
		s.internalError(w, "loading settings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": typedSettings(all)})
}

// parseSettingInt accepts a JSON integer or a string holding one (the SPA
// sends numbers; curl users often send strings). Fractional numbers fail.
func parseSettingInt(raw json.RawMessage) (int, error) {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return strconv.Atoi(strings.TrimSpace(str))
	}
	return 0, fmt.Errorf("not an integer")
}

func parseSettingString(raw json.RawMessage) (string, error) {
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	return v, nil
}

// spawnDefaultAllowed validates a spawn default against the provider-owned
// catalogs (D14: nothing outside a provider hardcodes model/effort values):
// the value must exist in at least one registered provider's catalog. With no
// provider registry (an instance-less lab) there is no catalog to check
// against, so the value passes — the spawn path re-validates on every start.
func (s *Server) spawnDefaultAllowed(value string, model bool) bool {
	if s.providers == nil {
		return true
	}
	for _, p := range s.providers.List() {
		opts := p.Efforts()
		if model {
			opts = p.Models()
		}
		if provider.HasOption(opts, value) {
			return true
		}
	}
	return false
}
