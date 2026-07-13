package instance

import (
	"context"
	"encoding/json"
	"fmt"

	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/vault"
)

// credentialEnv materializes the repo's GIT credential (design §6) keyed by
// opID (the run id) and returns the exact git env entries plus a cleanup that
// removes exactly this op's files. Unlike the clone path, the materialized
// files must survive for the whole session (the spawned agent's git ops use
// the same GIT_SSH_COMMAND/GIT_ASKPASS), so the caller does NOT run cleanup on
// the success path — it runs it only on Start rollback, and Stop removes the
// files explicitly at session end (mat.Cleanup(credID, runID)).
//
// A nil credential yields no env and a no-op cleanup. Error strings never carry
// payload bytes (the vault sanitizes decrypt errors). Mirrors
// reposvc.credentialEnv — the same design §6 wiring, kept per-service to avoid
// coupling the two lifecycles.
func (s *Service) credentialEnv(ctx context.Context, repo store.Repo, opID string) (env []string, cleanup func(), err error) {
	noop := func() {}
	if repo.CredentialID == nil {
		return nil, noop, nil
	}
	cred, err := s.store.CredentialByID(ctx, *repo.CredentialID)
	if err != nil {
		return nil, noop, err
	}
	remove := func() {
		if err := s.mat.Cleanup(cred.ID, opID); err != nil {
			s.log.Warn("cleaning materialized credential", "component", "instance", "err", err)
		}
	}
	switch cred.Kind {
	case store.CredentialKindSSHKey:
		var p vault.SSHKeyPayload
		if err := s.vault.DecryptPayload(cred.EncryptedPayload, &p); err != nil {
			return nil, noop, err
		}
		keyPath, sshAskpass, err := s.mat.MaterializeSSHKey(cred.ID, opID, p)
		if err != nil {
			remove()
			return nil, noop, err
		}
		if sshAskpass != "" {
			return vault.SSHEnvWithPassphrase(keyPath, s.mat.KnownHostsPath(), sshAskpass), remove, nil
		}
		return vault.SSHEnv(keyPath, s.mat.KnownHostsPath()), remove, nil
	case store.CredentialKindHTTPSToken:
		var p vault.HTTPSTokenPayload
		if err := s.vault.DecryptPayload(cred.EncryptedPayload, &p); err != nil {
			return nil, noop, err
		}
		askpass, err := s.mat.MaterializeAskpass(cred.ID, opID, p)
		if err != nil {
			remove()
			return nil, noop, err
		}
		return vault.HTTPSEnv(askpass), remove, nil
	default:
		// forge_token never authenticates git (design §3a).
		return nil, noop, fmt.Errorf("credential kind %s cannot authenticate git", cred.Kind)
	}
}

// cleanupCredential removes a run's materialized credential files at session
// end (opID = run id). A nil credential or a missing file is not an error.
func (s *Service) cleanupCredential(repo store.Repo, runID string) {
	if repo.CredentialID == nil {
		return
	}
	if err := s.mat.Cleanup(*repo.CredentialID, runID); err != nil {
		s.log.Warn("cleaning materialized credential at stop", "component", "instance", "run", runID, "err", err)
	}
}

// isAFKKind reports whether a run kind resolves through the AFK-override layer
// (issue #19): both AFK kinds share the same override-then-base resolution;
// manual does not.
func isAFKKind(kind string) bool {
	return kind == store.RunKindAFKManual || kind == store.RunKindAFKAuto
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
