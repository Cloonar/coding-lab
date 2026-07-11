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

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// afkPromptMaxBytes caps a stored afk_prompt override (issue #52 / ADR-0027),
// enforced identically by the global settings PATCH here and the repo-level
// afk_prompt PATCH in repos.go (same package). The seed prompt travels as the
// spawn's trailing argv positional, so an unbounded value would risk the OS
// ARG_MAX ceiling at spawn; 16 KiB sits far under any platform limit yet is
// ample for a full custom playbook.
const afkPromptMaxBytes = 16 << 10

// afkPromptDefaultKey is the read-only settings field carrying the built-in
// seed-prompt template (issue #52 / ADR-0027). It is NOT a stored settings row:
// SeedDefaultSettings never writes it and PATCH rejects it as an unknown key.
// It is injected into every settings response so the UI can show the factory
// prompt as the placeholder an operator's afk_prompt override would replace.
const afkPromptDefaultKey = "afk_prompt_default"

// settingsIntMin is the closed set of integer settings keys with each key's
// minimum: a zero cap or budget would deadlock every spawn, and sub-5s ticks
// would hammer tmux and the tracker (pinned: budget > 0, ticks >= 5s).
// dialog_timeout_minutes (issue #124) is the one key with floor 0: 0/absent
// means "never auto-dismiss", not a deadlock.
var settingsIntMin = map[string]int{
	store.SettingMaxInstances:         1,
	store.SettingAFKBudgetMinutes:     1,
	store.SettingAFKTickSeconds:       5,
	store.SettingAFKScheduleSeconds:   5,
	store.SettingSweepIntervalMinutes: 1,
	store.SettingDialogTimeoutMinutes: 0,
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

// injectReadonlySettings adds the computed read-only settings fields (issue #52
// / ADR-0027) to a typed settings map before it is returned, so GET and PATCH
// carry an identical surface (both handlers route through here). afk_prompt_default
// is the BASE seed-prompt template — non-incogni, tokens un-interpolated — the
// exact text the built-in SeedPrompt renders from and that an afk_prompt override
// replaces.
func injectReadonlySettings(m map[string]any) map[string]any {
	m[afkPromptDefaultKey] = afk.SeedPromptTemplate(false)
	return m
}

// handleSettingsGet is GET /api/v1/settings: every key+value, typed.
func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.AllSettings(r.Context())
	if err != nil {
		s.internalError(w, "loading settings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": injectReadonlySettings(typedSettings(all))})
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
		case store.SettingSpawnModelDefaultAFK:
			v, err := parseSettingString(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a string", key))
				return
			}
			// Empty = inherit the base default (issue #19), so unlike the base
			// key an empty AFK override is explicitly allowed.
			if v != "" && !s.spawnDefaultAllowed(v, true) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown model %q", v))
				return
			}
			updates[key] = v
		case store.SettingSpawnEffortDefaultAFK:
			v, err := parseSettingString(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a string", key))
				return
			}
			if v != "" && !s.spawnDefaultAllowed(v, false) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown effort %q", v))
				return
			}
			updates[key] = v
		case store.SettingProviderDefault:
			v, err := parseSettingString(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a string", key))
				return
			}
			// The base provider default (issue #66) must always name a real
			// provider: unlike the AFK override below there is no lower layer
			// an empty value could inherit from (the first-registered fallback
			// is a resolution rule, not an operator setting).
			if v == "" || !s.providerRegistered(v) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown provider %q", v))
				return
			}
			updates[key] = v
		case store.SettingSpawnProviderDefaultAFK:
			v, err := parseSettingString(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a string", key))
				return
			}
			// Empty = inherit the base provider chain (issue #66), mirroring
			// the spawn_*_default_afk keys.
			if v != "" && !s.providerRegistered(v) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown provider %q", v))
				return
			}
			updates[key] = v
		case store.SettingSpawnOptionsAFK:
			v, err := s.parseSpawnOptionsBag(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			updates[key] = v
		case store.SettingAFKPrompt:
			v, err := parseSettingString(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be a string", key))
				return
			}
			// Whitespace-only normalizes to "" = inherit the built-in (issue #52
			// / ADR-0027); a real override is stored AS-IS — no trimming, since a
			// prompt may legitimately carry leading/trailing structure. The cap
			// guards the spawn argv (afkPromptMaxBytes).
			if strings.TrimSpace(v) == "" {
				v = ""
			} else if len(v) > afkPromptMaxBytes {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("afk_prompt must be at most %d bytes", afkPromptMaxBytes))
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
	writeJSON(w, http.StatusOK, map[string]any{"settings": injectReadonlySettings(typedSettings(all))})
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

// providerRegistered reports whether id names a registered provider (issue
// #66). With no provider registry (an instance-less lab) there is nothing to
// check against, so the value passes — the spawn path skip-layers over an
// unresolvable default anyway.
func (s *Server) providerRegistered(id string) bool {
	if s.providers == nil {
		return true
	}
	_, ok := s.providers.Get(id)
	return ok
}

// parseSpawnOptionsBag validates the global spawn_options_afk value (issue #19 /
// ADR-0021) and returns the canonical JSON string to store. The value must be a
// JSON object of string values; each key must be declared by some registered
// provider and its value legal for that option's type (unknown key / bad value
// → the returned error becomes a 400, mirroring an unknown model/effort). An
// empty object is allowed. With no provider registry the bag passes unchecked
// (spawn re-validates). The error messages are operator-facing.
func (s *Server) parseSpawnOptionsBag(raw json.RawMessage) (string, error) {
	var bag map[string]string
	if err := json.Unmarshal(raw, &bag); err != nil {
		return "", fmt.Errorf("%s must be a JSON object of string values", store.SettingSpawnOptionsAFK)
	}
	if bag == nil {
		bag = map[string]string{}
	}
	if s.providers != nil {
		for key, val := range bag {
			spec, ok := s.findSpawnOption(key)
			if !ok {
				return "", fmt.Errorf("unknown spawn option %q", key)
			}
			if !provider.ValidOptionValue(spec, val) {
				return "", fmt.Errorf("invalid value %q for spawn option %q", val, key)
			}
		}
	}
	// Canonicalize (strip whitespace, drop non-object noise) before storing so
	// the GET value is a clean bag.
	out, err := json.Marshal(bag)
	if err != nil {
		return "", fmt.Errorf("%s: %v", store.SettingSpawnOptionsAFK, err)
	}
	return string(out), nil
}

// findSpawnOption returns the OptionSpec declaring key across all registered
// providers (a global bag may span providers once more than one exists), or
// false. The first provider that declares the key wins — providers that share a
// key must agree on its type.
func (s *Server) findSpawnOption(key string) (provider.OptionSpec, bool) {
	for _, p := range s.providers.List() {
		if spec, ok := provider.FindSpawnOption(p.SpawnOptions(), key); ok {
			return spec, true
		}
	}
	return provider.OptionSpec{}, false
}
