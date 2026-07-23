package instance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// credentialEnv materializes the repo's GIT credential (design §6) keyed by
// opID (the run id) into runMat — the run's PRIVATE per-run runtime
// materializer (<state>/instances/<runID>/runtime, issue #205) — and returns
// the exact git env entries. Key, askpass helper, and known_hosts all
// reference per-run paths, so the container runner can bind-mount exactly
// this run's credential surface and no other's. Unlike the clone path, the
// materialized files must survive for the whole session (the spawned agent's
// git ops use the same GIT_SSH_COMMAND/GIT_ASKPASS); there is no per-file
// cleanup anymore — the files live inside the per-run tree, which
// instancehome.Wipe removes wholesale at Stop and on Start rollback.
//
// A nil credential yields no env. Error strings never carry payload bytes
// (the vault sanitizes decrypt errors). Mirrors reposvc.credentialEnv — the
// same design §6 wiring, kept per-service to avoid coupling the two
// lifecycles (the clone path stays on the GLOBAL runtime materializer).
func (s *Service) credentialEnv(ctx context.Context, repo store.Repo, opID string, runMat *vault.Materializer) ([]string, error) {
	if repo.CredentialID == nil {
		return nil, nil
	}
	cred, err := s.store.CredentialByID(ctx, *repo.CredentialID)
	if err != nil {
		return nil, err
	}
	switch cred.Kind {
	case store.CredentialKindSSHKey:
		var p vault.SSHKeyPayload
		if err := s.vault.DecryptPayload(cred.EncryptedPayload, &p); err != nil {
			return nil, err
		}
		keyPath, sshAskpass, err := runMat.MaterializeSSHKey(cred.ID, opID, p)
		if err != nil {
			return nil, err
		}
		if sshAskpass != "" {
			return vault.SSHEnvWithPassphrase(keyPath, runMat.KnownHostsPath(), sshAskpass), nil
		}
		return vault.SSHEnv(keyPath, runMat.KnownHostsPath()), nil
	case store.CredentialKindHTTPSToken:
		var p vault.HTTPSTokenPayload
		if err := s.vault.DecryptPayload(cred.EncryptedPayload, &p); err != nil {
			return nil, err
		}
		askpass, err := runMat.MaterializeAskpass(cred.ID, opID, p)
		if err != nil {
			return nil, err
		}
		return vault.HTTPSEnv(askpass), nil
	default:
		// forge_token never authenticates git (design §3a).
		return nil, fmt.Errorf("credential kind %s cannot authenticate git", cred.Kind)
	}
}

// isAFKKind reports whether a run kind resolves through the AFK-override layer
// (issue #19): the two AFK kinds share the override-then-base resolution, and
// so does a fix run (issue #182; #189) — unattended AFK-class work continuing
// an AFK claim, so the AFK resolver layers (the options bag, the remote
// default, the model/effort override-then-base chain) apply to it. A fix run
// resolves provider/model/effort through this chain like any AFK-kind run
// (#189 reversed the ADR-0048 authoring-run inheritance — there is no
// per-spawn request to fall back from). Manual stays base-chain; lander AND
// escalate stay non-AFK — validation-class runs resolved on the lander chain.
func isAFKKind(kind string) bool {
	return kind == store.RunKindAFKManual || kind == store.RunKindAFKAuto || kind == store.RunKindFix
}

// ResolveProvider resolves the EFFECTIVE agent provider for a spawn of the
// given kind (issue #66) — the same three-level, skip-layer layering
// model/effort use. A MANUAL run: explicit per-spawn request → repo.provider →
// global provider_default → first registered. An AFK run has no per-spawn
// request; it consults the AFK-override layer FIRST — repo.afk_provider_default
// → global spawn_provider_default_afk — then the same base chain. Only the
// explicit request is STRICT (an unknown non-empty id is a 400); every default
// layer skips a value the registry does not carry, treating it as unset, so a
// stored default naming an absent provider can never wedge a spawn.
func (s *Service) ResolveProvider(ctx context.Context, repo store.Repo, kind, reqProvider string) (provider.AgentProvider, error) {
	if reqProvider != "" {
		p, ok := s.providers.Get(reqProvider)
		if !ok {
			return nil, badRequestf("unknown provider %q", reqProvider)
		}
		return p, nil
	}
	var candidates []string
	if isAFKKind(kind) {
		candidates = append(candidates, strOrEmpty(repo.AFKProviderDefault))
		v, err := s.store.GetString(ctx, store.SettingSpawnProviderDefaultAFK, "")
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, v)
	}
	candidates = append(candidates, strOrEmpty(repo.Provider))
	v, err := s.store.GetString(ctx, store.SettingProviderDefault, "")
	if err != nil {
		return nil, err
	}
	candidates = append(candidates, v)
	p, ok := s.providers.DefaultFor(candidates...)
	if !ok {
		return nil, fmt.Errorf("no agent providers registered")
	}
	return p, nil
}

// strOrEmpty flattens a nullable column for the candidate chains (nil = "").
func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ResolveModelEffort layers the spawn model/effort, run-kind-aware (D12d +
// issue #19 / ADR-0021) and skip-layer against the EFFECTIVE provider's
// catalogs (issue #66). A MANUAL run: explicit per-spawn value → repo base
// default → global base default. An AFK run has no per-spawn request; it
// consults the AFK-override layer FIRST — repo.afk_* → global spawn_*_default_afk
// — then falls back to the same base (repo base → global base). A DEFAULT-layer
// value the provider's catalog does not carry is treated as unset and falls
// through (a claude-shaped global default must not 400 a spawn on another
// provider); only the explicit request stays strict (unknown → 400).
//
// The effort pass runs against the RESOLVED MODEL's own effort list (issue
// #156): an explicit effort the resolved model does not support is the
// unsupported-combo 400 even when the union Efforts() carries it, and a
// stored effort default outside the model's list skip-layers like any other
// foreign value (a model-independent stored default is fine — it just falls
// through when the model doesn't support it). With every layer unset or
// foreign the model's reported DefaultEffort wins when it declares one, else
// the catalog's first entry; an EMPTY catalog (a provider without that knob)
// resolves to "" — the CLI flag is omitted. Exported for the M5 AFK engine,
// whose spawns resolve through the same rule.
func (s *Service) ResolveModelEffort(ctx context.Context, prov provider.AgentProvider, repo store.Repo, kind, reqModel, reqEffort string) (model, effort string, err error) {
	afk := isAFKKind(kind)
	models := prov.Models()
	if model, err = s.layerSpawnDefault(ctx, modelOptions(models), "", afk, reqModel, repo.AFKModelDefault, repo.ModelDefault, store.SettingSpawnModelDefaultAFK, store.SettingSpawnModelDefault, "model"); err != nil {
		return "", "", err
	}
	// The effort catalog is the resolved model's OWN list (issue #156). A ""
	// model is only possible when the model catalog itself is empty (the model
	// pass returns members or ""), so the miss branch falls back to the union
	// Efforts() with no reported default — a model-less provider keeps the
	// pre-#156 union semantics.
	effortCatalog, reportedDefault := prov.Efforts(), ""
	if m, ok := provider.FindModelOption(models, model); ok {
		effortCatalog, reportedDefault = m.Efforts, m.DefaultEffort
	}
	if effort, err = s.layerSpawnDefault(ctx, effortCatalog, reportedDefault, afk, reqEffort, repo.AFKEffortDefault, repo.EffortDefault, store.SettingSpawnEffortDefaultAFK, store.SettingSpawnEffortDefault, "effort"); err != nil {
		return "", "", err
	}
	return model, effort, nil
}

// modelOptions projects the enriched model catalog onto its embedded Options
// — the model pass of layerSpawnDefault stays on the plain Option catalog
// (issue #156 enriched only the effort side of the resolution).
func modelOptions(models []provider.ModelOption) []provider.Option {
	opts := make([]provider.Option, len(models))
	for i, m := range models {
		opts[i] = m.Option
	}
	return opts
}

// layerSpawnDefault resolves one spawn default (model or effort) through the
// D12d + issue-#19 layering with issue-#66 skip-layer semantics: the explicit
// per-spawn request is STRICT (a non-empty value outside catalog — including
// any non-empty value against an empty catalog — is a 400; for efforts the
// catalog is the resolved model's own list, so this is the issue-#156
// unsupported-combo rejection); every default layer counts only when its
// value is non-empty AND present in the catalog, else it is treated as unset
// and falls through. For an AFK run the AFK-override layer (repo override
// column, then the global AFK-override setting) is consulted first; then
// every run type falls back to the base layer (repo base column, then the
// global base setting), and finally reportedDefault — the provider's reported
// per-model default effort (issue #156), honored when non-empty AND a catalog
// member (defensive; conformance pins membership), "" for the model pass and
// for models reporting none — else the catalog's first entry, or "" for a
// provider whose catalog is empty (the flag is omitted at spawn). req is
// always "" for AFK (the start is bodyless). knob names the field in the 400
// message ("model"/"effort").
func (s *Service) layerSpawnDefault(ctx context.Context, catalog []provider.Option, reportedDefault string, afk bool, req string, repoAFK, repoBase *string, afkKey, baseKey, knob string) (string, error) {
	if req != "" {
		if !provider.HasOption(catalog, req) {
			return "", badRequestf("unknown %s %q", knob, req)
		}
		return req, nil
	}
	inCatalog := func(v string) bool { return v != "" && provider.HasOption(catalog, v) }
	if afk {
		if repoAFK != nil && inCatalog(*repoAFK) {
			return *repoAFK, nil
		}
		v, err := s.store.GetString(ctx, afkKey, "")
		if err != nil {
			return "", err
		}
		if inCatalog(v) {
			return v, nil
		}
	}
	if repoBase != nil && inCatalog(*repoBase) {
		return *repoBase, nil
	}
	v, err := s.store.GetString(ctx, baseKey, "")
	if err != nil {
		return "", err
	}
	if inCatalog(v) {
		return v, nil
	}
	if len(catalog) == 0 {
		return "", nil
	}
	if reportedDefault != "" && provider.HasOption(catalog, reportedDefault) {
		return reportedDefault, nil
	}
	return catalog[0].Value, nil
}

// ResolveRemote resolves the EFFECTIVE remote-control knob for a spawn of the
// given kind (issue #163) — the fourth typed spawn knob, layered on the same
// ADR-0030 skip-layer model as provider/model/effort, with ONE difference that
// governs the whole implementation: this knob is a BOOLEAN, so `false` is a
// legal VALUE and can never double as the "unset" sentinel the string knobs get
// for free ("" is not a legal model or effort id, so `""`-means-inherit is safe
// there; `false`-means-inherit would be a lie here). Every layer is therefore
// tri-state — *bool for the request and for the repo columns, an ABSENT-or-blank
// row for the settings layer (GetString(key,"") == "" is the inherit test;
// GetBool only speaks once the row is known non-blank) — and a layer that is
// present-and-false WINS over any lower layer that is true. Without that, "remote
// explicitly OFF at repo scope" would silently inherit a global true.
//
// MANUAL: explicit per-spawn request → repo.remote_default → spawn_remote_default
// → false. AFK has no per-spawn request (the start is bodyless); it consults its
// OVERRIDE layer first — repo.afk_remote_default → spawn_remote_default_afk —
// then falls through to the same base chain, exactly as layerSpawnDefault does
// with afk=true. spawn_remote_default is seeded ("false"); the _afk key is not,
// because absent = inherit.
//
// The layered answer is then CLAMPED by the provider's capability: a provider
// that does not implement provider.RemoteCapable — or that reports
// SupportsRemoteControl() == false — resolves to false no matter what every
// layer says. That clamp is what makes prov load-bearing here: the run row must
// TRUTHFULLY record whether a session is remote-controlled, because lab itself
// reads that stored bool — ArmCapture gates deep-link capture on it, and the SPA
// gates its Open affordance on it. A codex run is never remote, so it must never
// be recorded as one. The check is the same structural type assertion (ADR-0017)
// the deep-linker call sites use — lab asks WHETHER, never HOW.
func (s *Service) ResolveRemote(ctx context.Context, prov provider.AgentProvider, repo store.Repo, kind string, req *bool) (bool, error) {
	rc, ok := prov.(provider.RemoteCapable)
	if !ok || !rc.SupportsRemoteControl() {
		return false, nil // capability clamp — no layer can make this run remote
	}
	if req != nil {
		return *req, nil // an explicit per-spawn pick (true OR false) beats every layer
	}
	if isAFKKind(kind) {
		if repo.AFKRemoteDefault != nil {
			return *repo.AFKRemoteDefault, nil
		}
		if v, set, err := s.settingBool(ctx, store.SettingSpawnRemoteDefaultAFK); err != nil || set {
			return v, err
		}
	}
	if repo.RemoteDefault != nil {
		return *repo.RemoteDefault, nil
	}
	// The base setting is the last layer: absent or blank means the seeded row is
	// missing, and the floor for this knob is false (never remote by accident).
	v, _, err := s.settingBool(ctx, store.SettingSpawnRemoteDefault)
	return v, err
}

// settingBool reads a BOOLEAN settings row as a tri-state: a present, non-blank
// row yields its parsed value with set=true; an absent or blank row yields
// set=false — inherit. The two-step read is the whole point: GetBool's own def
// parameter flattens "absent" into a value, which is exactly what a boolean knob
// must not do (false is an answer, not a hole). Probing blankness with
// GetString(key, "") FIRST keeps the sentinel out of the value space; a present
// non-boolean value still fails loud through GetBool (never coerced to false).
func (s *Service) settingBool(ctx context.Context, key string) (val, set bool, err error) {
	raw, err := s.store.GetString(ctx, key, "")
	if err != nil {
		return false, false, err
	}
	if strings.TrimSpace(raw) == "" {
		return false, false, nil
	}
	v, err := s.store.GetBool(ctx, key, false)
	if err != nil {
		return false, false, err
	}
	return v, true, nil
}

// ResolveSpawnOptions resolves the provider-owned spawn-options bag for a launch
// of the given kind (issue #19 / ADR-0021). A MANUAL run carries no options —
// the operator types provider keywords (ultracode) into the Start prompt
// themselves. An AFK run resolves repo.afk_options ?? global spawn_options_afk,
// then FILTERS the bag to the provider's declared schema (a global bag may span
// providers once more than one exists) and VALIDATES the remainder (a bad value
// is a badRequest, mirroring an unknown model/effort). Always returns a non-nil
// map.
func (s *Service) ResolveSpawnOptions(ctx context.Context, prov provider.AgentProvider, repo store.Repo, kind string) (map[string]string, error) {
	if !isAFKKind(kind) {
		return map[string]string{}, nil
	}
	bag := repo.AFKOptions // a present map (even empty) wins over the global bag
	if bag == nil {
		raw, err := s.store.GetString(ctx, store.SettingSpawnOptionsAFK, "")
		if err != nil {
			return nil, err
		}
		if bag, err = decodeOptionsBag(raw); err != nil {
			return nil, err
		}
	}
	specs := prov.SpawnOptions()
	filtered := provider.FilterSpawnOptions(specs, bag)
	if err := provider.ValidateSpawnOptions(specs, filtered); err != nil {
		return nil, badRequestf("%s", err)
	}
	return filtered, nil
}

// ResolveAFKPrompt resolves the AFK seed-prompt override for a launch (issue
// #52 / ADR-0027), the sibling of ResolveSpawnOptions on the AFK layer: the
// repo's afk_prompt (non-nil and non-empty) wins, else the global afk_prompt
// setting, else "" — which the afk package reads as "use the built-in template"
// (afk.SeedPromptTemplate). Whitespace-only overrides normalize to inherit at
// the API boundary, so a stored value that reaches here is already the operator's
// verbatim prompt. There is no kind parameter: unlike ResolveModelEffort /
// ResolveSpawnOptions there is no manual analogue — only the AFK launch path
// (internal/afk/launch.go, both AFK kinds) resolves a seed prompt, so the caller
// gates on kind and this method assumes an AFK run.
func (s *Service) ResolveAFKPrompt(ctx context.Context, repo store.Repo) (string, error) {
	if repo.AFKPrompt != nil && *repo.AFKPrompt != "" {
		return *repo.AFKPrompt, nil
	}
	return s.store.GetString(ctx, store.SettingAFKPrompt, "")
}

// decodeOptionsBag parses the global spawn_options_afk JSON value: an empty
// string is an empty bag (nothing configured); non-empty JSON is decoded, and
// malformed JSON is a loud error (never a silent empty bag). Always returns a
// non-nil map on success.
func decodeOptionsBag(raw string) (map[string]string, error) {
	if raw == "" {
		return map[string]string{}, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, badRequestf("spawn_options_afk: invalid JSON: %v", err)
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

// authorEnv resolves the git author/committer identity for a spawned session:
// the repo's git_author_name/email override the global settings, and only a
// complete name+email pair is applied (GIT_AUTHOR_* and GIT_COMMITTER_* both).
// An unconfigured author yields no entries (the agent's own git identity, if
// any, applies).
func (s *Service) authorEnv(ctx context.Context, repo store.Repo) ([]string, error) {
	name, email := "", ""
	if repo.GitAuthorName != nil {
		name = *repo.GitAuthorName
	}
	if repo.GitAuthorEmail != nil {
		email = *repo.GitAuthorEmail
	}
	if name == "" {
		var err error
		if name, err = s.store.GetString(ctx, store.SettingGitAuthorName, ""); err != nil {
			return nil, err
		}
	}
	if email == "" {
		var err error
		if email, err = s.store.GetString(ctx, store.SettingGitAuthorEmail, ""); err != nil {
			return nil, err
		}
	}
	if name == "" || email == "" {
		return nil, nil
	}
	return []string{
		"GIT_AUTHOR_NAME=" + name,
		"GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name,
		"GIT_COMMITTER_EMAIL=" + email,
	}, nil
}
