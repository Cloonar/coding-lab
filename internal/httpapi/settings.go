package httpapi

// Settings surface (pinned M5 contract): GET returns every settings row with
// typed values (the integer knobs as JSON numbers, the boolean knobs as JSON
// bools), plus the computed read-only fields (afk_prompt_default,
// dev_image_fallback — see injectReadonlySettings); PATCH validates the whole
// body first — unknown keys (the read-only fields included), non-integers,
// non-booleans, out-of-range intervals (transcript_retention_days, issue #81,
// is bounded both ways: 0..365), spawn defaults outside the provider
// catalogs, and a runner_default outside host/container are 400s that write
// NOTHING — then upserts and returns the updated map. The one validation that
// touches the network, pinning a non-blank dev_image_default (issue #55 /
// ADR-0071, through the same pinner a repo's image_ref uses), runs LAST,
// after every cheap check of the body has passed, and a failed pin writes
// nothing either. No event is published and no restart is needed: the
// runtime loops re-read settings every tick (D12c), and spawn paths read them
// per call. A refusal about ONE key answers {"error","field":<that key>} (issue
// #85, the writeFieldError shape) so a form can pin the message under its
// input; refusals with no key to blame — an undecodable body, an unknown or
// read-only key — keep the plain {"error"}.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/afk"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/reposvc"
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

// devImageFallbackKey is the read-only settings field carrying the deployed
// fallback dev image — the --container-image value, "" when the flag is unset
// (issue #55 / ADR-0071). Like afk_prompt_default it is NOT a stored settings
// row and PATCH rejects it as an unknown key; it is injected into every
// settings response so the UI can say what a blank dev_image_default falls
// through to.
const devImageFallbackKey = "dev_image_fallback"

// settingsIntMin is the closed set of integer settings keys with each key's
// minimum: a zero cap or budget would deadlock every spawn, and sub-5s ticks
// would hammer tmux and the tracker (pinned: budget > 0, ticks >= 5s).
// dialog_timeout_minutes (issue #124) is the one key with floor 0: 0/absent
// means "never auto-dismiss", not a deadlock.
// container_pids/container_nofile (issue #205) floor at 1: podman's
// --pids-limit and --ulimit nofile both need at least one to run anything.
// transcript_retention_days (issue #81) floors at 0 too (retain nothing) and
// is the one key with a ceiling as well (settingsIntMax).
var settingsIntMin = map[string]int{
	store.SettingMaxInstances:         1,
	store.SettingAFKBudgetMinutes:     1,
	store.SettingAFKTickSeconds:       5,
	store.SettingAFKScheduleSeconds:   5,
	store.SettingSweepIntervalMinutes: 1,
	store.SettingDialogTimeoutMinutes: 0,
	store.SettingContainerPids:        1,
	store.SettingContainerNofile:      1,

	store.SettingTranscriptRetentionDays: 0,
}

// settingsIntMax is the subset of settingsIntMin's keys that also carry a
// ceiling, checked right after the floor (same all-or-nothing 400).
// transcript_retention_days (issue #81) floors at 0 — 0 is the off switch,
// not a deadlock — and caps at store.MaxTranscriptRetentionDays: there is no
// "forever", and the cap is the one TranscriptRetentionDays falls back from.
var settingsIntMax = map[string]int{
	store.SettingTranscriptRetentionDays: store.MaxTranscriptRetentionDays,
}

// settingsBoolNullable is the closed set of BOOLEAN settings keys (issue #163 —
// the first non-string, non-integer knobs on this surface), each mapped to
// whether JSON null is a legal value for it. The store speaks key → string, so
// these are persisted as "true"/"false" (and "" for the AFK inherit state) and
// typed back to real JSON bools on the way out.
//
// The base key is a plain bool: false is a VALUE (never remote), not an absence.
// The AFK override is tri-state — null (stored "") means inherit the base chain,
// mirroring how the string _afk keys use "" — because `false`-means-inherit would
// be a lie for a boolean knob (the trap ResolveRemote's doc calls out).
var settingsBoolNullable = map[string]bool{
	store.SettingSpawnRemoteDefault:    false,
	store.SettingSpawnRemoteDefaultAFK: true,

	// merge_delete_head (issue #90, ADR-0081): a plain non-nullable bool —
	// false is a VALUE (keep the head branch on origin), and the seeded
	// "true" row is always present, so there is no inherit state.
	store.SettingMergeDeleteHead: false,
}

// typedSettings renders a raw settings map with the integer keys as JSON
// numbers and the boolean keys as JSON bools (a nullable boolean key stored ""
// renders as JSON null — inherit). A garbled stored value (an unparseable int,
// a boolean row holding neither "true" nor "false") is passed through as the
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
		if nullable, isBool := settingsBoolNullable[k]; isBool {
			switch strings.TrimSpace(v) {
			case "true":
				out[k] = true
				continue
			case "false":
				out[k] = false
				continue
			case "":
				if nullable {
					out[k] = nil
					continue
				}
			}
		}
		out[k] = v
	}
	return out
}

// injectReadonlySettings adds the computed read-only settings fields to a typed
// settings map before it is returned, so GET and PATCH carry an identical
// surface (both handlers route through here). Neither is a stored row, and
// PATCH rejects both as unknown keys. afk_prompt_default (issue #52 /
// ADR-0027) is the BASE seed-prompt template — non-incogni, tokens
// un-interpolated — the exact text the built-in SeedPrompt renders from and
// that an afk_prompt override replaces. dev_image_fallback (issue #55 /
// ADR-0071) is the --container-image value (Options.DevImageFallback, "" when
// unset), the last layer a blank dev_image_default falls through to.
func (s *Server) injectReadonlySettings(m map[string]any) map[string]any {
	m[afkPromptDefaultKey] = afk.SeedPromptTemplate(false)
	m[devImageFallbackKey] = s.devImageFallback
	return m
}

// handleSettingsGet is GET /api/v1/settings: every key+value, typed.
func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.AllSettings(r.Context())
	if err != nil {
		s.internalError(w, "loading settings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": s.injectReadonlySettings(typedSettings(all))})
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
	var devImageRef *string // a non-blank dev_image_default awaiting its pin
	for key, raw := range body {
		if floor, isInt := settingsIntMin[key]; isInt {
			n, err := parseSettingInt(raw)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be an integer", key))
				return
			}
			if n < floor {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be at least %d", key, floor))
				return
			}
			if ceil, capped := settingsIntMax[key]; capped && n > ceil {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be at most %d", key, ceil))
				return
			}
			updates[key] = strconv.Itoa(n)
			continue
		}
		if nullable, isBool := settingsBoolNullable[key]; isBool {
			v, err := parseSettingBool(raw, key, nullable)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, err.Error())
				return
			}
			updates[key] = v
			continue
		}
		switch key {
		case store.SettingSpawnModelDefault:
			v, err := parseSettingString(raw)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be a string", key))
				return
			}
			if !s.spawnDefaultAllowed(v, true) {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("unknown model %q", v))
				return
			}
			updates[key] = v
		case store.SettingSpawnEffortDefault:
			v, err := parseSettingString(raw)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be a string", key))
				return
			}
			if !s.spawnDefaultAllowed(v, false) {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("unknown effort %q", v))
				return
			}
			updates[key] = v
		case store.SettingSpawnModelDefaultAFK, store.SettingSpawnModelDefaultLander:
			v, err := parseSettingString(raw)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be a string", key))
				return
			}
			// Empty = inherit the base default (issue #19), so unlike the base
			// key an empty AFK override is explicitly allowed. The lander
			// override follows the identical rule.
			if v != "" && !s.spawnDefaultAllowed(v, true) {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("unknown model %q", v))
				return
			}
			updates[key] = v
		case store.SettingSpawnEffortDefaultAFK, store.SettingSpawnEffortDefaultLander:
			v, err := parseSettingString(raw)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be a string", key))
				return
			}
			if v != "" && !s.spawnDefaultAllowed(v, false) {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("unknown effort %q", v))
				return
			}
			updates[key] = v
		case store.SettingProviderDefault:
			v, err := parseSettingString(raw)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be a string", key))
				return
			}
			// The base provider default (issue #66) must always name a real
			// provider: unlike the AFK override below there is no lower layer
			// an empty value could inherit from (the first-registered fallback
			// is a resolution rule, not an operator setting).
			if v == "" || !s.providerRegistered(v) {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("unknown provider %q", v))
				return
			}
			updates[key] = v
		case store.SettingSpawnProviderDefaultAFK:
			v, err := parseSettingString(raw)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be a string", key))
				return
			}
			// Empty = inherit the base provider chain (issue #66), mirroring
			// the spawn_*_default_afk keys.
			if v != "" && !s.providerRegistered(v) {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("unknown provider %q", v))
				return
			}
			updates[key] = v
		case store.SettingSpawnOptionsAFK:
			v, err := s.parseSpawnOptionsBag(raw)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, err.Error())
				return
			}
			updates[key] = v
		case store.SettingAFKPrompt:
			v, err := parseSettingString(raw)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be a string", key))
				return
			}
			// Whitespace-only normalizes to "" = inherit the built-in (issue #52
			// / ADR-0027); a real override is stored AS-IS — no trimming, since a
			// prompt may legitimately carry leading/trailing structure. The cap
			// guards the spawn argv (afkPromptMaxBytes).
			if strings.TrimSpace(v) == "" {
				v = ""
			} else if len(v) > afkPromptMaxBytes {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("afk_prompt must be at most %d bytes", afkPromptMaxBytes))
				return
			}
			updates[key] = v
		case store.SettingGitAuthorName, store.SettingGitAuthorEmail:
			v, err := parseSettingString(raw)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be a string", key))
				return
			}
			updates[key] = v
		case store.SettingContainerMemory:
			// The global floor under repos.container_memory (issue #205): must
			// match podman's --memory grammar (store.ValidContainerMemory, shared
			// with the repo-level override's own validation in reposvc).
			v, err := parseSettingString(raw)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be a string", key))
				return
			}
			if !store.ValidContainerMemory(v) {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must look like a podman --memory value, e.g. %q", key, "8g"))
				return
			}
			updates[key] = v
		case store.SettingRunnerDefault:
			// The global runner default (issue #55 / ADR-0071): exactly host or
			// container, the value every inheriting repo's next spawn resolves
			// to. Strict like the boolean keys — null, "", a case variant, a
			// number are all 400s — because the spawn path refuses anything
			// else, and a value it would refuse must never be stored.
			v, err := parseSettingString(raw)
			if err != nil || (v != store.RunnerHost && v != store.RunnerContainer) {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be %q or %q", key, store.RunnerHost, store.RunnerContainer))
				return
			}
			updates[key] = v
		case store.SettingDevImageDefault:
			// The global default dev image (issue #55 / ADR-0071). Blank or
			// whitespace clears it (stored "", which falls through to
			// --container-image); a non-blank ref is only recorded here and
			// pinned after the loop, once every cheap check has passed.
			v, err := parseSettingString(raw)
			if err != nil {
				writeFieldError(w, http.StatusBadRequest, key, fmt.Sprintf("%s must be a string", key))
				return
			}
			if strings.TrimSpace(v) == "" {
				updates[key] = ""
			} else {
				devImageRef = &v
			}
		default:
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown setting %q", key))
			return
		}
	}

	// dev_image_default is pinned LAST (issue #55 / ADR-0071), mirroring
	// reposvc's image_ref rule: it is the only key that touches the network, so
	// every cheap check above gates the registry round-trip (a request invalid
	// for another reason never pays for a pin), and running it after the loop
	// keeps that true whatever order the map yields the keys in. It pins
	// through reposvc.Service.PinImageRef — the very path a repo's image_ref
	// takes, not a second implementation — and its errors map exactly as the
	// repo field's do (writeRepoError): the pinner's rejection is a 400
	// carrying its message verbatim, an unavailable pinner a 500. Either way
	// the PATCH writes nothing, the body's other keys included. The RETURNED
	// pinned string is stored, never the operator's tag.
	if devImageRef != nil {
		pinned, err := s.pinDevImageDefault(r.Context(), *devImageRef)
		if err != nil {
			// The pinner's bad-ref rejection names no field of its own (it is
			// shared with the repo image_ref path), so it is pinned to this key
			// here; every other error maps as writeRepoError does.
			var bad *reposvc.BadRequestError
			if errors.As(err, &bad) {
				writeFieldError(w, http.StatusBadRequest, store.SettingDevImageDefault, bad.Error())
				return
			}
			s.writeRepoError(w, "pinning "+store.SettingDevImageDefault, err)
			return
		}
		updates[store.SettingDevImageDefault] = pinned
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
	writeJSON(w, http.StatusOK, map[string]any{"settings": s.injectReadonlySettings(typedSettings(all))})
}

// pinDevImageDefault digest-pins a non-blank dev_image_default through the
// repo service's PinImageRef (issue #55 / ADR-0071) and returns the pinned
// ref to store. A server built without the repo service has no pinner at all,
// so it fails exactly like reposvc's nil-pinner boot does — a plain error,
// never an unpinned ref stored as if it were pinned.
func (s *Server) pinDevImageDefault(ctx context.Context, ref string) (string, error) {
	if s.repos == nil {
		return "", errors.New("image ref pinning unavailable")
	}
	return s.repos.PinImageRef(ctx, ref)
}

// parseSettingInt accepts a JSON integer or a string holding one (the SPA
// sends numbers; curl users often send strings). Fractional numbers fail, and
// so does JSON null (issue #81): encoding/json leaves an int untouched on null,
// which would otherwise read as 0 — for transcript_retention_days the off
// switch, for dialog_timeout_minutes "never" — so an absent value could never
// be told apart from a deliberate zero.
func parseSettingInt(raw json.RawMessage) (int, error) {
	if strings.TrimSpace(string(raw)) == "null" {
		return 0, fmt.Errorf("not an integer")
	}
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

// parseSettingBool reads a BOOLEAN settings value (issue #163) and returns the
// canonical string the store holds ("true"/"false", or "" for the accepted null).
// STRICT by design, unlike parseSettingInt's string tolerance: only a JSON bool
// (and, for a nullable key, JSON null) is accepted — "true", "yes" and 1 are all
// 400s. A boolean knob whose wire type is loose invites exactly the confusion the
// tri-state exists to prevent (is "" off, or unset?), so the wire type is pinned.
func parseSettingBool(raw json.RawMessage, key string, nullable bool) (string, error) {
	must := fmt.Errorf("%s must be a boolean", key)
	if nullable {
		must = fmt.Errorf("%s must be a boolean or null", key)
	}
	var v *bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", must
	}
	if v == nil {
		if !nullable {
			return "", must
		}
		return "", nil // null = inherit the base layer (stored as the blank row)
	}
	return strconv.FormatBool(*v), nil
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
// the value must exist in at least one registered provider's catalog. Efforts
// deliberately check the model-independent UNION Efforts() — write-time
// settings validation stays model-independent (issue #156 pins this; the
// spawn path enforces the per-model list). With no provider registry (an
// instance-less lab) there is no catalog to check against, so the value
// passes — the spawn path re-validates on every start.
func (s *Server) spawnDefaultAllowed(value string, model bool) bool {
	if s.providers == nil {
		return true
	}
	for _, p := range s.providers.List() {
		if model {
			if provider.HasModelOption(p.Models(), value) {
				return true
			}
			continue
		}
		if provider.HasOption(p.Efforts(), value) {
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
