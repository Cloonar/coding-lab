package readiness

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/podmanx"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// The decision tables below are the specification of the readiness report
// (issue #61): for each of the six checks, when it passes, fails, is pending
// and — just as much part of the contract — when it is LEFT OUT because lab
// cannot evaluate it from what it already knows. Evaluate is pure, so every
// row is a hand-built Input and nothing here touches a store, git or a
// network.

func ptr[T any](v T) *T { return &v }

// readyRepo is a clone-ready, builtin-bound repo on the host Runner — the
// baseline every row varies one thing from.
func readyRepo() store.Repo {
	return store.Repo{
		ID: "repo_1", Name: "widget", RemoteURL: "https://forge.example.com/acme/widget.git",
		TrackerBinding: store.TrackerBindingBuiltin, CloneStatus: store.CloneStatusReady,
	}
}

// baseInput is readyRepo evaluated with nothing recorded and no imports.
func baseInput() Input {
	return Input{
		Repo:               readyRepo(),
		GitCredentialStamp: store.NoCredentialStamp,
		Runner:             store.RunnerHost,
		ImportsKnown:       true,
	}
}

// find returns the check with the given id, or nil when the report left it
// out.
func find(r Report, id string) *Check {
	for i := range r.Checks {
		if r.Checks[i].ID == id {
			return &r.Checks[i]
		}
	}
	return nil
}

// want is one expected check: absent, or state + detail + its one remedy.
type want struct {
	absent bool
	state  State
	detail string // exact sentence
	action string
	fix    *Fix
}

func assertCheck(t *testing.T, r Report, id string, w want) {
	t.Helper()
	c := find(r, id)
	if w.absent {
		if c != nil {
			t.Fatalf("%s check = %+v, want it LEFT OUT", id, *c)
		}
		return
	}
	if c == nil {
		t.Fatalf("%s check is missing; report = %+v", id, r)
	}
	if c.State != w.state {
		t.Errorf("%s state = %q, want %q (detail %q)", id, c.State, w.state, c.Detail)
	}
	if c.Detail != w.detail {
		t.Errorf("%s detail =\n  %q\nwant\n  %q", id, c.Detail, w.detail)
	}
	if c.Action != w.action {
		t.Errorf("%s action = %q, want %q", id, c.Action, w.action)
	}
	switch {
	case w.fix == nil && c.Fix != nil:
		t.Errorf("%s fix = %+v, want none", id, *c.Fix)
	case w.fix != nil && c.Fix == nil:
		t.Errorf("%s fix missing, want %+v", id, *w.fix)
	case w.fix != nil && *c.Fix != *w.fix:
		t.Errorf("%s fix = %+v, want %+v", id, *c.Fix, *w.fix)
	}
	// The wire contract: a failing check offers exactly one remedy and a
	// non-failing one offers none (ReadinessCheck in web/src/api/repos.ts).
	remedies := 0
	if c.Action != "" {
		remedies++
	}
	if c.Fix != nil {
		remedies++
	}
	if c.State == Failing && remedies != 1 {
		t.Errorf("%s is failing with %d remedies, want exactly one", id, remedies)
	}
	if c.State != Failing && remedies != 0 {
		t.Errorf("%s is %s yet carries a remedy", id, c.State)
	}
	assertOneSentence(t, id, c.Detail)
}

// assertOneSentence pins the shape of every detail: one line, bounded, closed
// by terminal punctuation — never a stack trace or raw multi-line stderr.
func assertOneSentence(t *testing.T, id, detail string) {
	t.Helper()
	if detail == "" {
		t.Errorf("%s detail is empty", id)
		return
	}
	if strings.ContainsAny(detail, "\n\r\t") {
		t.Errorf("%s detail spans lines: %q", id, detail)
	}
	if n := utf8.RuneCountInString(detail); n > 400 {
		t.Errorf("%s detail is %d runes long: %q", id, n, detail)
	}
	if last, _ := utf8.DecodeLastRuneInString(detail); !strings.ContainsRune(".!?…", last) {
		t.Errorf("%s detail does not end a sentence: %q", id, detail)
	}
}

func repoFix(section, field string) *Fix {
	return &Fix{Scope: ScopeRepo, Section: section, Field: field}
}

func TestCloneCheck(t *testing.T) {
	cloneStderr := "git clone --bare --progress --config remote.origin.fetch=+refs/heads/*:refs/remotes/origin/* https://forge.example.com/acme/widget.git /state/repos/repo_1.git: exit status 128: Cloning into bare repository '/state/repos/repo_1.git'...\n" +
		"remote: Repository not found.\n" +
		"fatal: repository 'https://forge.example.com/acme/widget.git/' not found"
	tests := []struct {
		name   string
		status string
		cerr   *string
		want   want
	}{
		{"cloning is pending and says when runs can start", store.CloneStatusCloning, nil,
			want{state: Pending, detail: "Lab is cloning this repository — runs can start when the clone finishes."}},
		{"a failed clone names git's verdict and offers the retry", store.CloneStatusError, &cloneStderr,
			want{state: Failing, detail: "The clone failed: repository 'https://forge.example.com/acme/widget.git/' not found.", action: ActionRetryClone}},
		{"a clone cut short by a restart says so", store.CloneStatusError, ptr(store.CloneErrorInterrupted),
			want{state: Failing, detail: "The clone was interrupted by a restart of lab.", action: ActionRetryClone}},
		{"a failed clone without a recorded error still fails", store.CloneStatusError, nil,
			want{state: Failing, detail: "The clone failed.", action: ActionRetryClone}},
		{"a ready clone passes", store.CloneStatusReady, nil,
			want{state: Passing, detail: "The repository is cloned."}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			in.Repo.CloneStatus, in.Repo.CloneError = tc.status, tc.cerr
			assertCheck(t, Evaluate(in), CheckClone, tc.want)
		})
	}
}

func TestGitCredentialCheck(t *testing.T) {
	sshKey := &store.CredentialMeta{ID: "cred_ssh", Name: "deploy key", Kind: store.CredentialKindSSHKey}
	const stamp = "cred_ssh@2026-07-01T12:00:00.000Z"
	fix := repoFix("integrations", "credential_id")
	fetchErr := "git fetch origin: exit status 128: remote: Invalid username or password.\nfatal: Authentication failed for 'https://forge.example.com/acme/widget.git/'"

	tests := []struct {
		name string
		mod  func(*Input)
		want want
	}{
		{"left out while the clone is still running", func(in *Input) {
			in.Repo.CloneStatus = store.CloneStatusCloning
			in.Fetch = &FetchRecord{OK: true, Credential: store.NoCredentialStamp}
		}, want{absent: true}},
		{"left out while the clone has failed", func(in *Input) {
			in.Repo.CloneStatus = store.CloneStatusError
			in.Fetch = &FetchRecord{OK: false, Error: fetchErr, Credential: store.NoCredentialStamp}
		}, want{absent: true}},
		{"left out when no fetch has been recorded", func(*Input) {}, want{absent: true}},
		{"a credential of the wrong kind fails on the local fact alone", func(in *Input) {
			in.GitCredential = &store.CredentialMeta{ID: "cred_f", Name: "forge token", Kind: store.CredentialKindForgeToken}
			in.GitCredentialStamp = "cred_f@x"
		}, want{state: Failing, fix: fix,
			detail: `The git credential "forge token" cannot authenticate git — pick an SSH key or an HTTPS token.`}},
		{"a failed fetch fails with git's own verdict", func(in *Input) {
			in.GitCredential, in.GitCredentialStamp = sshKey, stamp
			in.Fetch = &FetchRecord{OK: false, Error: fetchErr, Credential: stamp}
		}, want{state: Failing, fix: fix,
			detail: "The last fetch from the remote failed: Authentication failed for 'https://forge.example.com/acme/widget.git/'."}},
		{"a failed fetch without any credential points at the credential field", func(in *Input) {
			in.Fetch = &FetchRecord{OK: false, Credential: store.NoCredentialStamp,
				Error: "git fetch origin: exit status 128: fatal: could not read Username for 'https://forge.example.com': terminal prompts disabled"}
		}, want{state: Failing, fix: fix,
			detail: "The last fetch from the remote failed: could not read Username for 'https://forge.example.com': terminal prompts disabled."}},
		{"a successful fetch passes naming the credential", func(in *Input) {
			in.GitCredential, in.GitCredentialStamp = sshKey, stamp
			in.Fetch = &FetchRecord{OK: true, Credential: stamp}
		}, want{state: Passing, detail: `The last fetch from the remote succeeded with the git credential "deploy key".`}},
		{"a successful fetch of a public remote passes", func(in *Input) {
			in.Fetch = &FetchRecord{OK: true, Credential: store.NoCredentialStamp}
		}, want{state: Passing, detail: "The last fetch from the remote succeeded without a git credential."}},
		{"STALE: the credential was rotated since the failed fetch", func(in *Input) {
			in.GitCredential, in.GitCredentialStamp = sshKey, "cred_ssh@2026-07-02T09:00:00.000Z"
			in.Fetch = &FetchRecord{OK: false, Error: fetchErr, Credential: stamp}
		}, want{absent: true}},
		{"STALE: the repo now names another credential", func(in *Input) {
			in.GitCredential = &store.CredentialMeta{ID: "cred_new", Name: "new key", Kind: store.CredentialKindSSHKey}
			in.GitCredentialStamp = "cred_new@2026-07-01T12:00:00.000Z"
			in.Fetch = &FetchRecord{OK: false, Error: fetchErr, Credential: stamp}
		}, want{absent: true}},
		{"STALE: a success without a credential says nothing once one is set", func(in *Input) {
			in.GitCredential, in.GitCredentialStamp = sshKey, stamp
			in.Fetch = &FetchRecord{OK: true, Credential: store.NoCredentialStamp}
		}, want{absent: true}},
		{"STALE: a success with a credential says nothing once it is removed", func(in *Input) {
			in.Fetch = &FetchRecord{OK: true, Credential: stamp}
		}, want{absent: true}},
		{"left out when the current credential could not be read", func(in *Input) {
			in.GitCredentialStamp = ""
			in.Fetch = &FetchRecord{OK: true, Credential: ""}
		}, want{absent: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			tc.mod(&in)
			assertCheck(t, Evaluate(in), CheckGitCredential, tc.want)
		})
	}
}

func TestTrackerCheck(t *testing.T) {
	const stamp = "cred_forge@2026-07-01T12:00:00.000Z"
	credFix := repoFix("integrations", "forge_credential_id")
	bindingFix := repoFix("integrations", "tracker_binding")
	forge := func(in *Input) {
		in.Repo.TrackerBinding = store.TrackerBindingForge
		in.TrackerChecked = true
		in.ForgeCredentialStamp = stamp
	}
	wrap := func(err error) error { return fmt.Errorf("tracker for repo %q: %w (detail)", "repo_1", err) }
	at := func(minute int) time.Time { return time.Date(2026, 7, 1, 12, minute, 0, 0, time.UTC) }

	tests := []struct {
		name string
		mod  func(*Input)
		want want
	}{
		{"the built-in tracker always passes", func(*Input) {},
			want{state: Passing, detail: "This repository uses the built-in tracker."}},
		{"the built-in tracker passes whatever was recorded", func(in *Input) {
			in.TrackerReads = []TrackerRecord{{OK: false, Error: "boom", Credential: stamp}}
		}, want{state: Passing, detail: "This repository uses the built-in tracker."}},
		{"forge: left out when there is nothing to validate the binding with", func(in *Input) {
			in.Repo.TrackerBinding = store.TrackerBindingForge
			in.TrackerReads = []TrackerRecord{{OK: true}}
		}, want{absent: true}},
		{"forge: left out when no list read has been recorded", forge, want{absent: true}},
		{"forge: a successful read passes", func(in *Input) {
			forge(in)
			in.TrackerReads = []TrackerRecord{{OK: true, Op: tracker.OpReadyIssues, Credential: stamp}}
		}, want{state: Passing, detail: "The last read of the forge tracker succeeded."}},
		{"forge: a failed read fails with the forge's words on one line", func(in *Input) {
			forge(in)
			in.TrackerReads = []TrackerRecord{{OK: false, Credential: stamp,
				Error: `forgejo GET /repos/acme/widget/issues: unexpected status 401: {"message":"invalid username, password or token"}`}}
		}, want{state: Failing, fix: credFix,
			detail: `The last read of the forge tracker failed: forgejo GET /repos/acme/widget/issues: unexpected status 401: {"message":"invalid username, password or token"}.`}},
		{"forge: a 404 on a list read is a repository the token cannot see", func(in *Input) {
			forge(in)
			in.TrackerReads = []TrackerRecord{{OK: false, NotFound: true, Credential: stamp,
				Error: "forgejo GET /repos/acme/widget/issues: unexpected status 404: (empty body)"}}
		}, want{state: Failing, fix: credFix,
			detail: "The forge does not know this repository, or the forge credential's token cannot see it."}},
		{"forge: one kind of read failing fails the check though another succeeds", func(in *Input) {
			forge(in)
			in.TrackerReads = []TrackerRecord{
				{OK: true, Op: tracker.OpReadyIssues, Credential: stamp, At: at(2)},
				{OK: false, Op: tracker.OpPullsForHead, Credential: stamp, At: at(1),
					Error: "forgejo GET /repos/acme/widget/pulls: unexpected status 500: boom"},
			}
		}, want{state: Failing, fix: credFix,
			detail: "The last read of the forge tracker failed: forgejo GET /repos/acme/widget/pulls: unexpected status 500: boom."}},
		{"forge: a 404 on ONE listing while another answers is that listing's, not the repository's", func(in *Input) {
			forge(in)
			in.TrackerReads = []TrackerRecord{
				{OK: true, Op: tracker.OpReadyIssues, Credential: stamp, At: at(1)},
				{OK: false, NotFound: true, Op: tracker.OpPullsForHead, Credential: stamp, At: at(2),
					Error: "forgejo GET /repos/acme/widget/pulls: unexpected status 404: (empty body)"},
			}
		}, want{state: Failing, fix: credFix,
			detail: "The last read of the forge tracker failed: forgejo GET /repos/acme/widget/pulls: unexpected status 404: (empty body)."}},
		{"forge: of two failed kinds the most recent one is quoted", func(in *Input) {
			forge(in)
			in.TrackerReads = []TrackerRecord{
				{OK: false, Op: tracker.OpIssues, Credential: stamp, At: at(1), Error: "older failure"},
				{OK: false, Op: tracker.OpReadyIssues, Credential: stamp, At: at(3), Error: "newest failure"},
				{OK: false, Op: tracker.OpPulls, Credential: stamp, At: at(2), Error: "middle failure"},
			}
		}, want{state: Failing, fix: credFix, detail: "The last read of the forge tracker failed: newest failure."}},
		{"forge: a stale failure of one kind does not outweigh a fresh success of another", func(in *Input) {
			forge(in)
			in.TrackerReads = []TrackerRecord{
				{OK: false, Op: tracker.OpPulls, Credential: "cred_forge@old", At: at(1), Error: "401"},
				{OK: true, Op: tracker.OpReadyIssues, Credential: stamp, At: at(2)},
			}
		}, want{state: Passing, detail: "The last read of the forge tracker succeeded."}},
		{"forge STALE: the forge credential was rotated since the failed read", func(in *Input) {
			forge(in)
			in.ForgeCredentialStamp = "cred_forge@2026-07-02T09:00:00.000Z"
			in.TrackerReads = []TrackerRecord{{OK: false, Error: "401", Credential: stamp}}
		}, want{absent: true}},
		{"forge STALE: the repo now names another forge credential", func(in *Input) {
			forge(in)
			in.ForgeCredentialStamp = "cred_other@2026-07-01T12:00:00.000Z"
			in.TrackerReads = []TrackerRecord{{OK: true, Credential: stamp}}
		}, want{absent: true}},
		{"forge: left out when the current forge credential could not be read", func(in *Input) {
			forge(in)
			in.ForgeCredentialStamp = ""
			in.TrackerReads = []TrackerRecord{{OK: true, Credential: ""}}
		}, want{absent: true}},
		{"forge: a missing forge credential fails locally, before any record", func(in *Input) {
			forge(in)
			in.TrackerConfigErr = wrap(tracker.ErrForgeCredentialMissing)
			in.TrackerReads = []TrackerRecord{{OK: true, Credential: stamp}}
		}, want{state: Failing, fix: credFix, detail: "The forge tracker binding needs a forge credential, and none is set."}},
		{"forge: a credential of the wrong kind", func(in *Input) {
			forge(in)
			in.TrackerConfigErr = wrap(tracker.ErrForgeCredentialKind)
		}, want{state: Failing, fix: credFix, detail: "The forge credential is not a forge token."}},
		{"forge: a flavor that contradicts the remote's host", func(in *Input) {
			forge(in)
			in.TrackerConfigErr = wrap(tracker.ErrForgeFlavorMismatch)
		}, want{state: Failing, fix: credFix,
			detail: "The forge credential is for a different kind of forge than the one this repository's remote is on."}},
		{"forge: an unsupported flavor", func(in *Input) {
			forge(in)
			in.TrackerConfigErr = wrap(tracker.ErrForgeUnsupported)
		}, want{state: Failing, fix: credFix, detail: "The forge credential names a kind of forge lab has no client for."}},
		{"forge: an invalid host in the credential", func(in *Input) {
			forge(in)
			in.TrackerConfigErr = wrap(tracker.ErrForgeHost)
		}, want{state: Failing, fix: credFix, detail: "The forge credential's host is not a valid forge address."}},
		{"forge: a credential that cannot be loaded or decrypted", func(in *Input) {
			forge(in)
			in.TrackerConfigErr = errors.New(`tracker for repo "repo_1": decrypt forge credential: cipher: message authentication failed`)
		}, want{state: Failing, fix: credFix, detail: "The forge credential could not be read."}},
		{"forge: a remote no forge tracker could address points at the binding", func(in *Input) {
			forge(in)
			in.TrackerConfigErr = wrap(tracker.ErrRemotePath)
		}, want{state: Failing, fix: bindingFix,
			detail: "The remote URL has no owner/repository path a forge tracker could address — use the built-in tracker."}},
		{"an unknown binding points at the binding", func(in *Input) {
			in.Repo.TrackerBinding = "gitlab"
			in.TrackerChecked = true
			in.TrackerConfigErr = wrap(tracker.ErrUnknownBinding)
		}, want{state: Failing, fix: bindingFix, detail: "The tracker binding is neither forge nor builtin."}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			tc.mod(&in)
			assertCheck(t, Evaluate(in), CheckTracker, tc.want)
		})
	}
}

func TestAgentLoginCheck(t *testing.T) {
	credentials := &Fix{Scope: ScopeCredentials}
	tests := []struct {
		name   string
		logins []Login
		want   want
	}{
		{"left out when no provider resolves", nil, want{absent: true}},
		{"left out when the login was never checked", []Login{{Provider: "Agent One"}}, want{absent: true}},
		{"a logged-in agent passes by its display name", []Login{{Provider: "Agent One", Known: true, LoggedIn: true}},
			want{state: Passing, detail: "Agent One is logged in."}},
		{"a logged-out agent fails and points at the Credentials page", []Login{{Provider: "Agent One", Known: true}},
			want{state: Failing, fix: credentials, detail: "Agent One is logged out on this server."}},
		{"two agents, both logged in", []Login{
			{Provider: "Agent One", Known: true, LoggedIn: true}, {Provider: "Agent Two", Known: true, LoggedIn: true}},
			want{state: Passing, detail: "Agent One and Agent Two are logged in."}},
		{"two agents: failing wins over passing", []Login{
			{Provider: "Agent One", Known: true, LoggedIn: true}, {Provider: "Agent Two", Known: true}},
			want{state: Failing, fix: credentials, detail: "Agent Two is logged out on this server."}},
		{"two agents: failing wins over never checked", []Login{
			{Provider: "Agent One"}, {Provider: "Agent Two", Known: true}},
			want{state: Failing, fix: credentials, detail: "Agent Two is logged out on this server."}},
		{"two agents: one logged in and one never checked is not a pass", []Login{
			{Provider: "Agent One", Known: true, LoggedIn: true}, {Provider: "Agent Two"}},
			want{absent: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			in.Logins = tc.logins
			assertCheck(t, Evaluate(in), CheckAgentLogin, tc.want)
		})
	}
}

func TestDevImageCheck(t *testing.T) {
	runnerFix := repoFix("runner", "runner")
	imageFix := repoFix("runner", "image_ref")
	const image = "ghcr.io/acme/dev:1@sha256:0123"
	container := func(g instance.ContainerGate, rec *ImageRecord) func(*Input) {
		return func(in *Input) {
			in.Runner = store.RunnerContainer
			in.Container = &ContainerInput{Gate: g, Provider: "Agent One", Image: rec}
		}
	}
	open := instance.ContainerGate{Stage: instance.ContainerGateOpen, Image: image}

	tests := []struct {
		name string
		mod  func(*Input)
		want want
	}{
		{"absent on the host Runner", func(in *Input) {
			// Even with a container verdict at hand: host runs need no image.
			in.Container = &ContainerInput{Gate: open, Image: &ImageRecord{OK: false, Error: "boom"}}
		}, want{absent: true}},
		{"absent when the gate could not be asked", func(in *Input) { in.Runner = store.RunnerContainer }, want{absent: true}},
		{"an inherited Runner that cannot be resolved fails", func(in *Input) {
			in.Runner, in.RunnerErr = "", errors.New("the runner_default setting holds \"podman\"")
		}, want{state: Failing, fix: runnerFix,
			detail: "This repository inherits the global default Runner, which is not set to host or container."}},
		{"a pinned Runner outside the enum fails naming it", func(in *Input) {
			in.Repo.Runner = ptr("vm")
			in.Runner, in.RunnerErr = "", errors.New("pinned runner \"vm\"")
		}, want{state: Failing, fix: runnerFix,
			detail: `This repository's Runner is set to "vm", which is neither host nor container.`}},
		{"no container wiring on this server", container(instance.ContainerGate{
			Stage: instance.ContainerGateNotConfigured, Err: errors.New("container runner not configured")}, nil),
			want{state: Failing, fix: runnerFix, detail: "The container Runner is not configured on this server."}},
		{"the preflight has not finished: pending, nothing to fix", container(instance.ContainerGate{
			Stage: instance.ContainerGatePreflightPending, Err: errors.New("retry in a moment")}, nil),
			want{state: Pending, detail: "The container preflight has not finished yet."}},
		{"a failed preflight names its first actionable failure", container(instance.ContainerGate{
			Stage: instance.ContainerGatePreflightFailed,
			Preflight: podmanx.Result{Failures: []podmanx.Failure{
				{Check: podmanx.CheckPodman, Detail: "podman not found on PATH", Hint: "install podman >= 4"},
				{Check: podmanx.CheckPasta, Detail: "pasta not found on PATH", Hint: "install passt (provides pasta)"},
			}}}, nil),
			want{state: Failing, fix: runnerFix,
				detail: "The container preflight failed: podman not found on PATH (install podman >= 4) — and 1 more."}},
		{"no agent-tools image for the provider, named by its display name", container(instance.ContainerGate{
			Stage: instance.ContainerGateNoToolsImage, Err: errors.New("no agent-tools image configured for provider x")}, nil),
			want{state: Failing, fix: runnerFix, detail: "No agent-tools image is configured for Agent One on this server."}},
		{"no dev image at any layer points at the image field", container(instance.ContainerGate{
			Stage: instance.ContainerGateNoDevImage, Err: fmt.Errorf("refused: %w", instance.ErrNoDevImage)}, nil),
			want{state: Failing, fix: imageFix, detail: "No dev image is configured for this repository."}},
		{"an unreadable default dev image setting", container(instance.ContainerGate{
			Stage: instance.ContainerGateNoDevImage, Err: errors.New("the dev_image_default setting could not be read")}, nil),
			want{state: Failing, fix: imageFix, detail: "The default dev image setting could not be read."}},
		{"an open gate with the image never ensured is left out", container(open, nil), want{absent: true}},
		{"an open gate with the image present passes", container(open, &ImageRecord{OK: true}),
			want{state: Passing, detail: "The dev image is present on this server."}},
		{"an open gate whose image failed to pull fails with podman's last line", container(open, &ImageRecord{OK: false,
			Error: "pulling dev image " + image + ": exit status 125: Trying to pull " + image + "...\nError: initializing source: reading manifest: manifest unknown (check the ref and registry access from this host)"}),
			want{state: Failing, fix: imageFix,
				detail: "The dev image could not be pulled: initializing source: reading manifest: manifest unknown (check the ref and registry access from this host)."}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			tc.mod(&in)
			assertCheck(t, Evaluate(in), CheckDevImage, tc.want)
		})
	}
}

func TestImportsCheck(t *testing.T) {
	fix := &Fix{Scope: ScopeRepo, Section: "imports"}
	target := func(name, status string) store.Repo {
		return store.Repo{ID: "repo_" + name, Name: name, CloneStatus: status}
	}
	okFetch := func(stamp string) *FetchRecord { return &FetchRecord{OK: true, Credential: stamp} }
	failed := func(stamp string) *FetchRecord {
		return &FetchRecord{OK: false, Credential: stamp,
			Error: "git fetch origin: exit status 128: git@forge.example.com: Permission denied (publickey).\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists."}
	}
	const none = store.NoCredentialStamp

	tests := []struct {
		name    string
		unknown bool
		imports []ImportInput
		want    want
	}{
		{"left out when the imports could not be read", true, nil, want{absent: true}},
		{"no imports declared passes", false, nil, want{state: Passing, detail: "No read-only imports declared."}},
		// Still cloning clears by itself, and removing the import is not its
		// remedy: pending, with no fix.
		{"a target still cloning is pending naming it, with no fix", false, []ImportInput{
			{Target: target("lib", store.CloneStatusReady), Stamp: none, Fetch: okFetch(none)},
			{Target: target("proto", store.CloneStatusCloning), Stamp: none},
		}, want{state: Pending, detail: `The read-only import "proto" is still being cloned.`}},
		{"a target whose clone failed fails naming it", false, []ImportInput{
			{Target: target("proto", store.CloneStatusError), Stamp: none},
		}, want{state: Failing, fix: fix, detail: `The clone of the read-only import "proto" failed.`}},
		{"a target whose last fetch failed fails with git's reason", false, []ImportInput{
			{Target: target("lib", store.CloneStatusReady), Stamp: none, Fetch: okFetch(none)},
			{Target: target("proto", store.CloneStatusReady), Stamp: "cred_p@1", Fetch: failed("cred_p@1")},
		}, want{state: Failing, fix: fix,
			detail: `The last fetch of the read-only import "proto" failed: git@forge.example.com: Permission denied (publickey).`}},
		// Something to fix outranks something that settles by itself.
		{"a failed fetch outranks a target still cloning", false, []ImportInput{
			{Target: target("lib", store.CloneStatusReady), Stamp: none, Fetch: failed(none)},
			{Target: target("proto", store.CloneStatusCloning), Stamp: none},
		}, want{state: Failing, fix: fix,
			detail: `The last fetch of the read-only import "lib" failed: git@forge.example.com: Permission denied (publickey).`}},
		{"a failed clone outranks a target still cloning", false, []ImportInput{
			{Target: target("lib", store.CloneStatusCloning), Stamp: none},
			{Target: target("proto", store.CloneStatusError), Stamp: none},
		}, want{state: Failing, fix: fix, detail: `The clone of the read-only import "proto" failed.`}},
		{"a target still cloning is pending even beside unfetched ones", false, []ImportInput{
			{Target: target("lib", store.CloneStatusReady), Stamp: none},
			{Target: target("proto", store.CloneStatusCloning), Stamp: none, Fetch: failed(none)},
		}, want{state: Pending, detail: `The read-only import "proto" is still being cloned.`}},
		{"STALE: a failed fetch with a credential since changed is not held against the target", false, []ImportInput{
			{Target: target("proto", store.CloneStatusReady), Stamp: "cred_p@2", Fetch: failed("cred_p@1")},
		}, want{absent: true}},
		{"one target, fetched successfully", false, []ImportInput{
			{Target: target("proto", store.CloneStatusReady), Stamp: none, Fetch: okFetch(none)},
		}, want{state: Passing, detail: `The read-only import "proto" was fetched successfully.`}},
		{"every target fetched successfully", false, []ImportInput{
			{Target: target("lib", store.CloneStatusReady), Stamp: none, Fetch: okFetch(none)},
			{Target: target("proto", store.CloneStatusReady), Stamp: "cred_p@1", Fetch: okFetch("cred_p@1")},
		}, want{state: Passing, detail: "All 2 read-only imports were fetched successfully."}},
		{"left out while some target has never been fetched", false, []ImportInput{
			{Target: target("lib", store.CloneStatusReady), Stamp: none, Fetch: okFetch(none)},
			{Target: target("proto", store.CloneStatusReady), Stamp: none},
		}, want{absent: true}},
		{"left out while some target's success is stale", false, []ImportInput{
			{Target: target("lib", store.CloneStatusReady), Stamp: none, Fetch: okFetch(none)},
			{Target: target("proto", store.CloneStatusReady), Stamp: "cred_p@2", Fetch: okFetch("cred_p@1")},
		}, want{absent: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput()
			in.ImportsKnown, in.Imports = !tc.unknown, tc.imports
			assertCheck(t, Evaluate(in), CheckImports, tc.want)
		})
	}
}

// fullInput is a container-Runner, forge-bound repo with one import and a
// record for every check — the report with all six.
func fullInput() Input {
	const gitStamp, forgeStamp = "cred_git@1", "cred_forge@1"
	in := baseInput()
	in.Repo.TrackerBinding = store.TrackerBindingForge
	in.GitCredential = &store.CredentialMeta{ID: "cred_git", Name: "deploy key", Kind: store.CredentialKindSSHKey}
	in.GitCredentialStamp = gitStamp
	in.Fetch = &FetchRecord{OK: true, Credential: gitStamp}
	in.TrackerChecked, in.ForgeCredentialStamp = true, forgeStamp
	in.TrackerReads = []TrackerRecord{{OK: true, Credential: forgeStamp}}
	in.Logins = []Login{{Provider: "Agent One", Known: true, LoggedIn: true}}
	in.Runner = store.RunnerContainer
	in.Container = &ContainerInput{
		Gate:     instance.ContainerGate{Stage: instance.ContainerGateOpen, Image: "img"},
		Provider: "Agent One", Image: &ImageRecord{OK: true},
	}
	in.Imports = []ImportInput{{
		Target: store.Repo{ID: "repo_lib", Name: "lib", CloneStatus: store.CloneStatusReady},
		Stamp:  store.NoCredentialStamp, Fetch: &FetchRecord{OK: true, Credential: store.NoCredentialStamp},
	}}
	return in
}

func checkIDs(r Report) []string {
	ids := make([]string, len(r.Checks))
	for i, c := range r.Checks {
		ids[i] = c.ID
	}
	return ids
}

// The six checks come out in canonical order, and a report holds only the
// checks lab could evaluate — it gets shorter, it never gets a guess.
func TestCanonicalOrderAndOmission(t *testing.T) {
	all := []string{CheckClone, CheckGitCredential, CheckTracker, CheckAgentLogin, CheckDevImage, CheckImports}
	got := checkIDs(Evaluate(fullInput()))
	if strings.Join(got, ",") != strings.Join(all, ",") {
		t.Fatalf("checks = %v, want the canonical order %v", got, all)
	}

	// Nothing recorded, no provider, host Runner, forge-bound with nothing to
	// validate against, imports unreadable: only the clone is known.
	in := Input{Repo: readyRepo(), Runner: store.RunnerHost}
	in.Repo.TrackerBinding = store.TrackerBindingForge
	if got := checkIDs(Evaluate(in)); strings.Join(got, ",") != CheckClone {
		t.Fatalf("checks = %v, want only the clone check", got)
	}

	// A subset keeps the canonical order: drop the middle ones.
	in = fullInput()
	in.Fetch, in.Logins = nil, nil
	want := []string{CheckClone, CheckTracker, CheckDevImage, CheckImports}
	if got := checkIDs(Evaluate(in)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("checks = %v, want %v", got, want)
	}
}

// dev_image exists only while the effective Runner is container.
func TestDevImageAbsentOnHostRunner(t *testing.T) {
	in := fullInput()
	in.Runner = store.RunnerHost
	r := Evaluate(in)
	if c := find(r, CheckDevImage); c != nil {
		t.Fatalf("dev_image = %+v on the host Runner, want it absent", *c)
	}
	if len(r.Checks) != 5 {
		t.Fatalf("got %d checks, want the other five: %v", len(r.Checks), checkIDs(r))
	}
}

// The roll-up: failing if any check fails, else pending if any is pending,
// else passing.
func TestRollUp(t *testing.T) {
	t.Run("all passing", func(t *testing.T) {
		if r := Evaluate(fullInput()); r.State != Passing {
			t.Fatalf("state = %q, want passing: %+v", r.State, r.Checks)
		}
	})
	t.Run("a pending check makes the report pending", func(t *testing.T) {
		in := fullInput()
		in.Container.Gate = instance.ContainerGate{Stage: instance.ContainerGatePreflightPending}
		if r := Evaluate(in); r.State != Pending {
			t.Fatalf("state = %q, want pending: %+v", r.State, r.Checks)
		}
	})
	t.Run("failing outranks pending", func(t *testing.T) {
		in := fullInput()
		in.Container.Gate = instance.ContainerGate{Stage: instance.ContainerGatePreflightPending}
		in.Logins[0].LoggedIn = false
		if r := Evaluate(in); r.State != Failing {
			t.Fatalf("state = %q, want failing: %+v", r.State, r.Checks)
		}
	})
	t.Run("a cloning repo is pending, not failing", func(t *testing.T) {
		in := baseInput()
		in.Repo.CloneStatus = store.CloneStatusCloning
		r := Evaluate(in)
		if r.State != Pending {
			t.Fatalf("state = %q, want pending: %+v", r.State, r.Checks)
		}
	})
	t.Run("a failed clone is failing", func(t *testing.T) {
		in := baseInput()
		in.Repo.CloneStatus = store.CloneStatusError
		if r := Evaluate(in); r.State != Failing {
			t.Fatalf("state = %q, want failing", r.State)
		}
	})
}

// The JSON is the wire contract of web/src/api/repos.ts: `checks` is an
// array even when it would be empty, `action` and `fix` exist only on the
// check that has them, and a fix omits the section/field it does not name.
func TestReportJSONShape(t *testing.T) {
	in := baseInput()
	in.Repo.CloneStatus = store.CloneStatusError
	in.Logins = []Login{{Provider: "Agent One", Known: true}}
	in.Imports = []ImportInput{{Target: store.Repo{Name: "lib", CloneStatus: store.CloneStatusError}}}
	raw, err := json.Marshal(Summary{Readiness: Evaluate(in)})
	if err != nil {
		t.Fatal(err)
	}
	const wantJSON = `{"claimable":null,"open_issues":null,"readiness":{"state":"failing","checks":[` +
		`{"id":"clone","state":"failing","detail":"The clone failed.","action":"retry_clone"},` +
		`{"id":"tracker","state":"passing","detail":"This repository uses the built-in tracker."},` +
		`{"id":"agent_login","state":"failing","detail":"Agent One is logged out on this server.","fix":{"scope":"credentials"}},` +
		`{"id":"imports","state":"failing","detail":"The clone of the read-only import \"lib\" failed.","fix":{"scope":"repo","section":"imports"}}` +
		`]}}`
	if string(raw) != wantJSON {
		t.Fatalf("summary JSON =\n%s\nwant\n%s", raw, wantJSON)
	}

	// No evaluable check at all still marshals `[]`, never null.
	empty, err := json.Marshal(Evaluate(Input{Repo: store.Repo{CloneStatus: "?"}}))
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != `{"state":"passing","checks":[]}` {
		t.Fatalf("empty report JSON = %s", empty)
	}
	n := 3
	counted, _ := json.Marshal(Summary{Claimable: &n, OpenIssues: new(int), Readiness: Report{State: Passing, Checks: []Check{}}})
	if !strings.HasPrefix(string(counted), `{"claimable":3,"open_issues":0,`) {
		t.Fatalf("counted summary JSON = %s", counted)
	}
}

// gitReason picks the one line of a git error an operator needs.
func TestGitReason(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"empty", "", ""},
		{"the last fatal line, without its prefix",
			"git fetch origin: exit status 128: remote: Invalid username or password.\nfatal: Authentication failed for 'https://x/y.git/'",
			"Authentication failed for 'https://x/y.git/'"},
		{"a single fatal line behind the engine's framing",
			"git fetch origin: exit status 128: fatal: unable to access 'https://x/y.git/': Could not resolve host: x",
			"unable to access 'https://x/y.git/': Could not resolve host: x"},
		{"ssh's reason above git's catch-all closing line",
			"git fetch origin: exit status 128: git@x: Permission denied (publickey).\nfatal: Could not read from remote repository.\n\nPlease make sure you have the correct access rights\nand the repository exists.",
			"git@x: Permission denied (publickey)."},
		{"the catch-all line alone is still the answer",
			"git fetch origin: exit status 128: fatal: Could not read from remote repository.",
			"Could not read from remote repository."},
		{"an error line when there is no fatal one",
			"git fetch origin: exit status 1: error: cannot lock ref 'refs/remotes/origin/main'\nsome trailing noise",
			"cannot lock ref 'refs/remotes/origin/main'"},
		{"the engine's own timeout", "git fetch origin: timed out after 1m0s", "the remote did not answer within 1m0s"},
		{"a non-git error is quoted as it is", "prepare git credential: credential kind forge_token cannot authenticate git",
			"prepare git credential: credential kind forge_token cannot authenticate git"},
		{"clone progress noise is skipped",
			"…ository '/s/r.git'...\nremote: Counting objects: 100% (3/3), done.\nfatal: early EOF\nfatal: fetch-pack: invalid index-pack output",
			"fetch-pack: invalid index-pack output"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitReason(tc.in); got != tc.want {
				t.Fatalf("gitReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// sentence keeps a quoted reason on one bounded line and closes it.
func TestSentence(t *testing.T) {
	if got := sentence("It failed: ", "  several\n\tlines   here "); got != "It failed: several lines here." {
		t.Errorf("collapsed = %q", got)
	}
	if got := sentence("It failed: ", "already closed."); got != "It failed: already closed." {
		t.Errorf("closed = %q", got)
	}
	if got := sentence("It failed: ", ""); got != "It failed." {
		t.Errorf("empty reason = %q", got)
	}
	long := sentence("It failed: ", strings.Repeat("x", 5000))
	if n := utf8.RuneCountInString(long); n != len("It failed: ")+maxReasonRunes+1 || !strings.HasSuffix(long, "…") {
		t.Errorf("long reason is %d runes, suffix %q", n, long[len(long)-4:])
	}
	// Multi-byte text is cut on a rune boundary.
	if got := sentence("", strings.Repeat("é", maxReasonRunes+10)); !utf8.ValidString(got) {
		t.Errorf("cut inside a rune: %q", got)
	}
}
