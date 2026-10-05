package readiness

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// State is one check's verdict, and the report's roll-up.
type State string

// The three verdicts. There is deliberately no "unknown": a check lab cannot
// evaluate from what it already knows is left out of the report, never shown
// as passing (issue #61).
const (
	Passing State = "passing"
	Failing State = "failing"
	// Pending is "still settling, nothing to fix": a clone in flight (the
	// repo's own, or a read-only import's), a container preflight that has
	// not published its verdict yet.
	Pending State = "pending"
)

// The six checks, in the report's canonical order.
const (
	CheckClone         = "clone"
	CheckGitCredential = "git_credential"
	CheckTracker       = "tracker"
	CheckAgentLogin    = "agent_login"
	CheckDevImage      = "dev_image"
	CheckImports       = "imports"
)

// ActionRetryClone is the one action that is not a settings field: the
// failing clone check is fixed by POST /repos/{id}/clone/retry.
const ActionRetryClone = "retry_clone"

// Fix scopes: where the setting that fixes a failing check lives.
const (
	// ScopeRepo: this repo's settings page — Section is a repo-settings
	// section slug, Field the repo PATCH key of the offending field.
	ScopeRepo = "repo"
	// ScopeGlobal: the global Settings area — Section is its slug.
	ScopeGlobal = "global"
	// ScopeCredentials: the Credentials page, where the agent login cards
	// live.
	ScopeCredentials = "credentials"
)

// Repo-settings section slugs a fix points into (the SPA's
// REPO_SETTINGS_CATEGORIES; slugs are URL surface and do not change).
const (
	sectionIntegrations = "integrations"
	sectionRunner       = "runner"
	sectionImports      = "imports"
)

// Fix points at the settings field that fixes a failing check.
type Fix struct {
	Scope   string `json:"scope"`
	Section string `json:"section,omitempty"`
	Field   string `json:"field,omitempty"`
}

// Check is one line of the report: what was verified, or what is wrong, in
// one operator-facing sentence. A failing check carries exactly one remedy —
// Action (the clone check only) or Fix, never both.
type Check struct {
	ID     string `json:"id"`
	State  State  `json:"state"`
	Detail string `json:"detail"`
	Action string `json:"action,omitempty"`
	Fix    *Fix   `json:"fix,omitempty"`
}

// Report is whether a run can start in one repo right now. Checks is in
// canonical order and holds only the checks lab could evaluate; State is the
// roll-up — failing if any check fails, else pending if any is pending, else
// passing.
type Report struct {
	State  State   `json:"state"`
	Checks []Check `json:"checks"`
}

// Summary is what a repo response carries about the repo beyond its row
// (issue #61): the claimable and open-issue counts as last known — nil is
// "not known yet", rendered as JSON null — and the readiness report.
type Summary struct {
	Claimable  *int   `json:"claimable"`
	OpenIssues *int   `json:"open_issues"`
	Readiness  Report `json:"readiness"`
}

// Input is everything one repo's report is computed from, already gathered:
// Evaluate reads nothing else and has no side effect, so every rule of the
// report — including which record is stale and which check is left out — is
// a pure function of this value.
type Input struct {
	Repo store.Repo

	// GitCredential is the credential row repo.credential_id names; nil when
	// the repo has none.
	GitCredential *store.CredentialMeta
	// GitCredentialStamp is the CURRENT stamp of the repo's git credential
	// (store.NoCredentialStamp without one; "" when it could not be read).
	GitCredentialStamp string
	// Fetch is the recorded outcome of the last credentialed fetch; nil =
	// none recorded.
	Fetch *FetchRecord

	// TrackerChecked says the tracker binding's local validation ran
	// (TrackerConfigErr is its result). False = there is nothing to validate
	// with, and a forge-bound repo's tracker check is left out.
	TrackerChecked   bool
	TrackerConfigErr error
	// ForgeCredentialStamp is the CURRENT stamp of the repo's forge
	// credential.
	ForgeCredentialStamp string
	// TrackerReads are the recorded outcomes of the repo's list reads, the
	// most recent of each kind; empty = none recorded.
	TrackerReads []TrackerRecord

	// Logins is the login state of each agent a run in this repo would use:
	// the effective provider of runs the operator starts, then — while Auto
	// is on and it differs — the effective provider of AFK runs. Empty =
	// no provider could be resolved.
	Logins []Login

	// Runner is the effective Runner (instance.EffectiveRunner), RunnerErr
	// its failure.
	Runner    string
	RunnerErr error
	// Container is the container gate's verdict for this repo; nil when the
	// effective Runner is not container, or the gate could not be asked.
	Container *ContainerInput

	// ImportsKnown says Imports was read (an empty list then means "none
	// declared"); false leaves the imports check out.
	ImportsKnown bool
	Imports      []ImportInput
}

// Login is one provider's last known login state. Known is false when the
// state has never been checked since lab started (or the provider cannot be
// asked without checking).
type Login struct {
	// Provider is the provider's display name — the only way a report names
	// an agent (the provider-neutral rule).
	Provider string
	Known    bool
	LoggedIn bool
}

// ContainerInput is the container side of the dev image check.
type ContainerInput struct {
	// Gate is the spawn gate's verdict (instance.Service.ContainerGate) — the
	// same one a container spawn of this repo would be refused by.
	Gate instance.ContainerGate
	// Provider is the display name of the provider the gate was asked for.
	Provider string
	// Image is the recorded pull-if-missing outcome of Gate.Image; nil =
	// never ensured since lab started.
	Image *ImageRecord
}

// ImportInput is one read-only import target of the repo.
type ImportInput struct {
	Target store.Repo
	// Stamp is the CURRENT stamp of the target's git credential.
	Stamp string
	// Fetch is the recorded outcome of the last credentialed fetch of the
	// target's reference repo; nil = none.
	Fetch *FetchRecord
}

// Evaluate builds the report for one repo. A check function returning nil
// leaves its check out.
func Evaluate(in Input) Report {
	checks := make([]Check, 0, 6)
	for _, c := range []*Check{
		cloneCheck(in),
		gitCredentialCheck(in),
		trackerCheck(in),
		agentLoginCheck(in),
		devImageCheck(in),
		importsCheck(in),
	} {
		if c != nil {
			checks = append(checks, *c)
		}
	}
	return Report{State: rollUp(checks), Checks: checks}
}

// rollUp is the report's state: failing if any check fails, else pending if
// any is pending, else passing.
func rollUp(checks []Check) State {
	state := Passing
	for _, c := range checks {
		switch c.State {
		case Failing:
			return Failing
		case Pending:
			state = Pending
		}
	}
	return state
}

func passing(id, detail string) *Check { return &Check{ID: id, State: Passing, Detail: detail} }
func pending(id, detail string) *Check { return &Check{ID: id, State: Pending, Detail: detail} }

// failing is a failing check fixed by the repo-settings field section/field.
func failing(id, detail, section, field string) *Check {
	return &Check{ID: id, State: Failing, Detail: detail, Fix: &Fix{Scope: ScopeRepo, Section: section, Field: field}}
}

// cloneCheck: the clone is always evaluable — it is the repo row.
func cloneCheck(in Input) *Check {
	switch in.Repo.CloneStatus {
	case store.CloneStatusCloning:
		return pending(CheckClone, "Lab is cloning this repository — runs can start when the clone finishes.")
	case store.CloneStatusError:
		detail := "The clone failed."
		if msg := in.Repo.CloneError; msg != nil && strings.TrimSpace(*msg) != "" {
			if *msg == store.CloneErrorInterrupted {
				detail = "The clone was interrupted by a restart of lab."
			} else {
				detail = sentence("The clone failed: ", gitReason(*msg))
			}
		}
		return &Check{ID: CheckClone, State: Failing, Detail: detail, Action: ActionRetryClone}
	case store.CloneStatusReady:
		return passing(CheckClone, "The repository is cloned.")
	}
	return nil
}

// gitCredentialCheck rests on the last recorded fetch: whether a remote needs
// a credential at all is not decidable from stored state, a referenced
// credential cannot be deleted, and none carries an expired flag — so the
// only local fact is a credential of the wrong kind, and everything else is
// what the last fetch that ran with THIS credential version said.
func gitCredentialCheck(in Input) *Check {
	if in.Repo.CloneStatus != store.CloneStatusReady {
		return nil // nothing to fetch into yet; the clone check speaks
	}
	if c := in.GitCredential; c != nil && c.Kind != store.CredentialKindSSHKey && c.Kind != store.CredentialKindHTTPSToken {
		return failing(CheckGitCredential,
			fmt.Sprintf("The git credential %q cannot authenticate git — pick an SSH key or an HTTPS token.", c.Name),
			sectionIntegrations, "credential_id")
	}
	rec := in.Fetch
	if rec == nil || in.GitCredentialStamp == "" || rec.Credential != in.GitCredentialStamp {
		return nil // never fetched, or fetched with a credential since changed
	}
	if !rec.OK {
		return failing(CheckGitCredential,
			sentence("The last fetch from the remote failed: ", gitReason(rec.Error)),
			sectionIntegrations, "credential_id")
	}
	if c := in.GitCredential; c != nil {
		return passing(CheckGitCredential, fmt.Sprintf("The last fetch from the remote succeeded with the git credential %q.", c.Name))
	}
	return passing(CheckGitCredential, "The last fetch from the remote succeeded without a git credential.")
}

// trackerCheck: the built-in tracker is lab's own store and always answers.
// A forge binding is first validated locally (the registry's own resolution,
// no request), then judged by the last list read made with this forge
// credential version.
func trackerCheck(in Input) *Check {
	if in.Repo.TrackerBinding == store.TrackerBindingBuiltin {
		return passing(CheckTracker, "This repository uses the built-in tracker.")
	}
	if !in.TrackerChecked {
		return nil
	}
	if err := in.TrackerConfigErr; err != nil {
		detail, field := trackerConfigProblem(err)
		return failing(CheckTracker, detail, sectionIntegrations, field)
	}
	failed, ok := JudgeTrackerReads(in.TrackerReads, in.ForgeCredentialStamp)
	switch {
	case failed != nil:
		detail := sentence("The last read of the forge tracker failed: ", lastLine(failed.Error))
		if failed.NotFound && !ok {
			// A 404 with no other read succeeding: the repository itself is
			// what the forge does not show this token. (With another read
			// answering, the 404 is about that one listing, and the forge's
			// own words above say which.)
			detail = "The forge does not know this repository, or the forge credential's token cannot see it."
		}
		return failing(CheckTracker, detail, sectionIntegrations, "forge_credential_id")
	case ok:
		return passing(CheckTracker, "The last read of the forge tracker succeeded.")
	}
	return nil // never read, or read only with a forge credential since changed
}

// trackerConfigProblem words a tracker-resolution failure and names the
// repo-settings field that fixes it: the forge credential for everything
// about the credential, the tracker binding where no credential could help.
func trackerConfigProblem(err error) (detail, field string) {
	switch {
	case errors.Is(err, tracker.ErrForgeCredentialMissing):
		return "The forge tracker binding needs a forge credential, and none is set.", "forge_credential_id"
	case errors.Is(err, tracker.ErrForgeCredentialKind):
		return "The forge credential is not a forge token.", "forge_credential_id"
	case errors.Is(err, tracker.ErrForgeFlavorMismatch):
		return "The forge credential is for a different kind of forge than the one this repository's remote is on.", "forge_credential_id"
	case errors.Is(err, tracker.ErrForgeUnsupported):
		return "The forge credential names a kind of forge lab has no client for.", "forge_credential_id"
	case errors.Is(err, tracker.ErrForgeHost):
		return "The forge credential's host is not a valid forge address.", "forge_credential_id"
	case errors.Is(err, tracker.ErrRemotePath):
		return "The remote URL has no owner/repository path a forge tracker could address — use the built-in tracker.", "tracker_binding"
	case errors.Is(err, tracker.ErrUnknownBinding):
		return "The tracker binding is neither forge nor builtin.", "tracker_binding"
	}
	// What is left is the credential row itself: it could not be loaded or
	// did not decrypt.
	return "The forge credential could not be read.", "forge_credential_id"
}

// agentLoginCheck reads the last KNOWN login state of each agent a run here
// would use — never a fresh check. One logged-out agent fails the check; it
// passes only when every one of them is known to be logged in.
func agentLoginCheck(in Input) *Check {
	if len(in.Logins) == 0 {
		return nil
	}
	for _, l := range in.Logins {
		if l.Known && !l.LoggedIn {
			return &Check{ID: CheckAgentLogin, State: Failing,
				Detail: fmt.Sprintf("%s is logged out on this server.", l.Provider),
				Fix:    &Fix{Scope: ScopeCredentials}}
		}
	}
	names := make([]string, 0, len(in.Logins))
	for _, l := range in.Logins {
		if !l.Known {
			return nil // never checked: nothing to say yet
		}
		names = append(names, l.Provider)
	}
	if len(names) == 1 {
		return passing(CheckAgentLogin, names[0]+" is logged in.")
	}
	return passing(CheckAgentLogin, strings.Join(names[:len(names)-1], ", ")+" and "+names[len(names)-1]+" are logged in.")
}

// devImageCheck exists only while the effective Runner is container. It
// follows the container spawn gate stage by stage (instance.Service.
// ContainerGate — the very verdict a spawn is refused by), and past an open
// gate it reports the last pull-if-missing of the resolved dev image. Every
// host-side refusal points at the Runner field, because switching the Runner
// is the one remedy this app offers for a host that cannot run containers.
func devImageCheck(in Input) *Check {
	if in.RunnerErr != nil {
		detail := "This repository inherits the global default Runner, which is not set to host or container."
		if in.Repo.Runner != nil {
			detail = fmt.Sprintf("This repository's Runner is set to %q, which is neither host nor container.", *in.Repo.Runner)
		}
		return failing(CheckDevImage, detail, sectionRunner, "runner")
	}
	if in.Runner != store.RunnerContainer || in.Container == nil {
		return nil
	}
	c := in.Container
	switch c.Gate.Stage {
	case instance.ContainerGateNotConfigured:
		return failing(CheckDevImage, "The container Runner is not configured on this server.", sectionRunner, "runner")
	case instance.ContainerGatePreflightPending:
		return pending(CheckDevImage, "The container preflight has not finished yet.")
	case instance.ContainerGatePreflightFailed:
		return failing(CheckDevImage, preflightProblem(c.Gate), sectionRunner, "runner")
	case instance.ContainerGateNoToolsImage:
		return failing(CheckDevImage,
			fmt.Sprintf("No agent-tools image is configured for %s on this server.", c.Provider),
			sectionRunner, "runner")
	case instance.ContainerGateNoDevImage:
		detail := "The default dev image setting could not be read."
		if errors.Is(c.Gate.Err, instance.ErrNoDevImage) {
			detail = "No dev image is configured for this repository."
		}
		return failing(CheckDevImage, detail, sectionRunner, "image_ref")
	case instance.ContainerGateOpen:
		switch rec := c.Image; {
		case rec == nil:
			return nil // not ensured since lab started
		case !rec.OK:
			return failing(CheckDevImage,
				sentence("The dev image could not be pulled: ", strings.TrimPrefix(lastLine(rec.Error), "Error: ")),
				sectionRunner, "image_ref")
		}
		return passing(CheckDevImage, "The dev image is present on this server.")
	}
	return nil
}

// preflightProblem words a failed container preflight by its first failure —
// what was observed and the operator action that fixes it — and says how
// many more there are.
func preflightProblem(g instance.ContainerGate) string {
	fs := g.Preflight.Failures
	if len(fs) == 0 {
		return "The container preflight failed."
	}
	reason := fs[0].Detail
	if fs[0].Hint != "" {
		reason += " (" + fs[0].Hint + ")"
	}
	if more := len(fs) - 1; more > 0 {
		reason += fmt.Sprintf(" — and %d more", more)
	}
	return sentence("The container preflight failed: ", reason)
}

// importsCheck mirrors what a spawn does with the repo's read-only imports:
// it is refused while any target's clone is not ready, and when any target's
// snapshot fetch fails — a failure only the TARGET's fetch record shows.
//
// A target whose clone is still in flight is pending, not failing: that
// state clears by itself, and removing the import is not its remedy. It
// carries no fix, and anything failing — another target's failed clone, a
// ready target's failed fetch — outranks it.
func importsCheck(in Input) *Check {
	if !in.ImportsKnown {
		return nil
	}
	if len(in.Imports) == 0 {
		return passing(CheckImports, "No read-only imports declared.")
	}
	var cloning *store.Repo
	for i, imp := range in.Imports {
		switch imp.Target.CloneStatus {
		case store.CloneStatusReady:
		case store.CloneStatusCloning:
			if cloning == nil {
				cloning = &in.Imports[i].Target
			}
		default:
			return failing(CheckImports,
				fmt.Sprintf("The clone of the read-only import %q failed.", imp.Target.Name), sectionImports, "")
		}
	}
	verified := 0
	for _, imp := range in.Imports {
		rec := imp.Fetch
		if imp.Target.CloneStatus != store.CloneStatusReady || rec == nil || imp.Stamp == "" || rec.Credential != imp.Stamp {
			continue
		}
		if !rec.OK {
			return failing(CheckImports,
				sentence(fmt.Sprintf("The last fetch of the read-only import %q failed: ", imp.Target.Name), gitReason(rec.Error)),
				sectionImports, "")
		}
		verified++
	}
	if cloning != nil {
		return pending(CheckImports, fmt.Sprintf("The read-only import %q is still being cloned.", cloning.Name))
	}
	if verified < len(in.Imports) {
		return nil // some target has no usable fetch record yet
	}
	if len(in.Imports) == 1 {
		return passing(CheckImports, fmt.Sprintf("The read-only import %q was fetched successfully.", in.Imports[0].Target.Name))
	}
	return passing(CheckImports, fmt.Sprintf("All %d read-only imports were fetched successfully.", len(in.Imports)))
}

// maxReasonRunes bounds the foreign text (git's, a forge's, podman's) a
// detail sentence quotes.
const maxReasonRunes = 240

// sentence joins a fixed lead-in and a quoted reason into one sentence: the
// reason on one line, whitespace collapsed, bounded, and closed with a full
// stop when it brings no terminal punctuation of its own.
func sentence(lead, reason string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		return strings.TrimRight(strings.TrimSpace(lead), ":") + "."
	}
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		reason = strings.TrimSpace(string([]rune(reason)[:maxReasonRunes])) + "…"
	}
	if last, _ := utf8.DecodeLastRuneInString(reason); !strings.ContainsRune(".!?…", last) {
		reason += "."
	}
	return lead + reason
}

// lastLine is the last non-blank line of a possibly multi-line tool error —
// where the tool's own verdict sits.
func lastLine(text string) string {
	lines := nonBlankLines(text)
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

func nonBlankLines(text string) []string {
	var lines []string
	for line := range strings.Lines(text) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// gitExitPrefix is the engine's own framing in front of git's stderr:
// "git <args>: exit status <n>: " (gitx.Engine.run and CloneBare).
var gitExitPrefix = regexp.MustCompile(`^.*?: exit status \d+: `)

// gitTimedOut is the engine's timeout error, "git <args>: timed out after
// <duration>" — no stderr follows it.
var gitTimedOut = regexp.MustCompile(`: timed out after (\S+)$`)

// gitGenericFatal is the line git closes EVERY failed transport with; the
// line above it is the one that says why.
const gitGenericFatal = "Could not read from remote repository."

// gitReason picks the one line of a git error an operator needs. git's
// stderr is a transcript — progress, remote chatter, then the verdict — so
// the last "fatal:" line is the reason, an "error:" line the next best, the
// last line the fallback. The engine's framing in front of the first stderr
// line is dropped, and so is git's catch-all closing line when a more
// specific one precedes it (ssh's "Permission denied (publickey)").
func gitReason(text string) string {
	lines := nonBlankLines(text)
	if len(lines) == 0 {
		return ""
	}
	if m := gitTimedOut.FindStringSubmatch(lines[len(lines)-1]); m != nil {
		return "the remote did not answer within " + m[1]
	}
	for i := range lines {
		lines[i] = gitExitPrefix.ReplaceAllString(lines[i], "")
	}
	pick := len(lines) - 1
	for _, prefix := range []string{"fatal: ", "error: "} {
		found := -1
		for i, line := range lines {
			if strings.HasPrefix(line, prefix) {
				found = i
			}
		}
		if found >= 0 {
			pick = found
			lines[pick] = strings.TrimPrefix(lines[pick], prefix)
			break
		}
	}
	if lines[pick] == gitGenericFatal && pick > 0 {
		pick--
	}
	return lines[pick]
}
