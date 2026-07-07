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

// ResolveModelEffort layers the spawn model/effort, run-kind-aware (D12d +
// issue #19 / ADR-0021). A MANUAL run: explicit per-spawn value → repo base
// default → global base default. An AFK run has no per-spawn request; it
// consults the AFK-override layer FIRST — repo.afk_* → global spawn_*_default_afk
// — then falls back to the same base (repo base → global base). Empty AFK
// overrides mean inherit, so the layering degrades cleanly. The resolved pair
// is validated against the provider's catalogs (closed allowlists — an unknown
// value is a 400, never spawned). Exported for the M5 AFK engine, whose spawns
// resolve through the same rule.
func (s *Service) ResolveModelEffort(ctx context.Context, prov provider.AgentProvider, repo store.Repo, kind, reqModel, reqEffort string) (model, effort string, err error) {
	afk := isAFKKind(kind)
	if model, err = s.layerSpawnDefault(ctx, afk, reqModel, repo.AFKModelDefault, repo.ModelDefault, store.SettingSpawnModelDefaultAFK, store.SettingSpawnModelDefault); err != nil {
		return "", "", err
	}
	if effort, err = s.layerSpawnDefault(ctx, afk, reqEffort, repo.AFKEffortDefault, repo.EffortDefault, store.SettingSpawnEffortDefaultAFK, store.SettingSpawnEffortDefault); err != nil {
		return "", "", err
	}
	if !provider.HasOption(prov.Models(), model) {
		return "", "", badRequestf("unknown model %q", model)
	}
	if !provider.HasOption(prov.Efforts(), effort) {
		return "", "", badRequestf("unknown effort %q", effort)
	}
	return model, effort, nil
}

// layerSpawnDefault resolves one spawn default (model or effort) through the
// D12d + issue-#19 layering. For an AFK run it consults the AFK-override layer
// first (repo override column, then the global AFK-override setting), each
// empty = inherit; then every run type falls back to the base layer (the
// per-spawn request for manual, then the repo base column, then the global base
// setting). req is always "" for AFK (the start is bodyless).
func (s *Service) layerSpawnDefault(ctx context.Context, afk bool, req string, repoAFK, repoBase *string, afkKey, baseKey string) (string, error) {
	if afk {
		if repoAFK != nil && *repoAFK != "" {
			return *repoAFK, nil
		}
		v, err := s.store.GetString(ctx, afkKey, "")
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
	}
	if req != "" {
		return req, nil
	}
	if repoBase != nil && *repoBase != "" {
		return *repoBase, nil
	}
	return s.store.GetString(ctx, baseKey, "")
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
