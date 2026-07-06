package instance

import (
	"context"
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

// ResolveModelEffort layers the spawn model/effort per D12d: an explicit
// per-spawn value wins, then the repo default, then the global settings
// default; the resolved pair is validated against the provider's catalogs
// (closed allowlists — an unknown value is a 400, never spawned). Exported
// for the M5 AFK engine, whose spawns resolve through the same rule.
func (s *Service) ResolveModelEffort(ctx context.Context, prov provider.AgentProvider, repo store.Repo, reqModel, reqEffort string) (model, effort string, err error) {
	model = reqModel
	if model == "" && repo.ModelDefault != nil {
		model = *repo.ModelDefault
	}
	if model == "" {
		if model, err = s.store.GetString(ctx, store.SettingSpawnModelDefault, ""); err != nil {
			return "", "", err
		}
	}
	effort = reqEffort
	if effort == "" && repo.EffortDefault != nil {
		effort = *repo.EffortDefault
	}
	if effort == "" {
		if effort, err = s.store.GetString(ctx, store.SettingSpawnEffortDefault, ""); err != nil {
			return "", "", err
		}
	}
	if !provider.HasOption(prov.Models(), model) {
		return "", "", badRequestf("unknown model %q", model)
	}
	if !provider.HasOption(prov.Efforts(), effort) {
		return "", "", badRequestf("unknown effort %q", effort)
	}
	return model, effort, nil
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
