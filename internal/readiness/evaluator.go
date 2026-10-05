package readiness

import (
	"context"
	"errors"

	"git.cloonar.com/Cloonar/coding-lab/internal/instance"
	"git.cloonar.com/Cloonar/coding-lab/internal/provider"
	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// Store is the slice of the store an evaluation reads. *store.Store
// satisfies it. Every method is a local query; the batch forms
// (Credentials, AllRepoImports, OpenIssueCounts) exist so a repo list of N
// rows costs a handful of queries, not N of each.
type Store interface {
	// GetSetting backs instance.EffectiveRunner (the runner_default row).
	GetSetting(ctx context.Context, key string) (string, error)
	Credentials(ctx context.Context) ([]store.CredentialMeta, error)
	RepoImports(ctx context.Context, repoID string) ([]store.Repo, error)
	AllRepoImports(ctx context.Context) (map[string][]string, error)
	OpenIssueCounts(ctx context.Context, label string) (map[string]int, error)
}

// Spawner is the slice of the instance service an evaluation asks — the
// spawn path's own resolvers, so the report cannot disagree with a spawn
// about which agent runs or whether a container could start.
// *instance.Service satisfies it. Both methods read the store, the provider
// registry and the in-memory preflight verdict; neither starts a process.
type Spawner interface {
	ResolveProvider(ctx context.Context, repo store.Repo, kind, reqProvider string) (provider.AgentProvider, error)
	ContainerGate(ctx context.Context, providerID string, repo store.Repo) instance.ContainerGate
}

// TrackerConfig validates a repo's tracker binding locally — no request to a
// forge. *tracker.Registry satisfies it (CheckConfig).
type TrackerConfig interface {
	CheckConfig(ctx context.Context, repo store.Repo) error
}

// ClaimableCounter computes a builtin-bound repo's claimable count without
// leaving the machine. *afk.Service satisfies it (LocalClaimableCount, which
// refuses a forge-bound repo outright).
type ClaimableCounter interface {
	LocalClaimableCount(ctx context.Context, repo store.Repo) (int, error)
}

// Evaluator builds readiness reports and repo summaries from the store, the
// Recorder and the spawn path's resolvers. It performs no network operation
// and starts no provider CLI and no podman process, whatever it is asked:
// every forge-, git-remote-, provider-CLI- or podman-shaped fact comes from
// the Recorder or from a read-only peek. That property is the feature (issue
// #61 — "no new network call per page view"), and the seams are cut to keep
// it: the evaluator holds no tracker (only the registry's local validation),
// no git engine, and reaches the instance service and the AFK engine through
// interfaces that expose their pure resolvers alone. The one thing it asks a
// provider is LastAuthStatus — the last known login state, never a check.
//
// The single subprocess an evaluation can cause is local: the `git
// for-each-ref` listing a BUILTIN-bound repo's claim branches, and only when
// that repo has a ready queue to weigh them against (ClaimableCounter). A
// forge-bound repo never causes one.
//
// Store and Recorder are required. A nil Spawner leaves the agent login and
// dev image checks out; a nil Tracker leaves a forge-bound repo's tracker
// check out; a nil Claimable leaves a builtin-bound repo's claimable count
// unknown.
type Evaluator struct {
	Store     Store
	Recorder  *Recorder
	Spawner   Spawner
	Tracker   TrackerConfig
	Claimable ClaimableCounter
}

// Summaries evaluates every repo of a full repo listing: one Summary per
// repo, in order. repos must be the WHOLE list — import targets are looked
// up in it, which is what keeps the batch free of a query per repo.
func (e *Evaluator) Summaries(ctx context.Context, repos []store.Repo) ([]Summary, error) {
	w, err := e.load(ctx, repos, true)
	if err != nil {
		return nil, err
	}
	all, err := e.Store.AllRepoImports(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]store.Repo, len(repos))
	for _, r := range repos {
		byID[r.ID] = r
	}
	w.imports = func(_ context.Context, repo store.Repo) ([]store.Repo, error) {
		targets := make([]store.Repo, 0, len(all[repo.ID]))
		for _, id := range all[repo.ID] {
			t, ok := byID[id]
			if !ok {
				// Declared between the two reads: the row is not in this
				// listing, so its state is unknown here.
				return nil, errors.New("import target outside the listing")
			}
			targets = append(targets, t)
		}
		return targets, nil
	}
	out := make([]Summary, len(repos))
	for i, repo := range repos {
		out[i] = e.summary(ctx, w, repo)
	}
	return out, nil
}

// Summary evaluates one repo: its counts and its report.
func (e *Evaluator) Summary(ctx context.Context, repo store.Repo) (Summary, error) {
	w, err := e.load(ctx, []store.Repo{repo}, true)
	if err != nil {
		return Summary{}, err
	}
	return e.summary(ctx, w, repo), nil
}

// Report evaluates one repo's readiness report alone.
func (e *Evaluator) Report(ctx context.Context, repo store.Repo) (Report, error) {
	w, err := e.load(ctx, []store.Repo{repo}, false)
	if err != nil {
		return Report{}, err
	}
	return Evaluate(e.input(ctx, w, repo)), nil
}

// world is the store state the evaluations of one request share.
type world struct {
	creds    map[string]store.CredentialMeta
	settings *settingsMemo
	// imports returns a repo's read-only import targets; an error leaves the
	// imports check out for that repo.
	imports func(ctx context.Context, repo store.Repo) ([]store.Repo, error)
	// builtinOpen and builtinReady are the open-issue and ready-queue sizes
	// of builtin-bound repos by repo id (absent = zero); nil when not
	// loaded.
	builtinOpen, builtinReady map[string]int
}

// load reads what every evaluation of the request needs, once. counts also
// loads the built-in tracker's per-repo issue counts, when any repo is
// builtin-bound.
func (e *Evaluator) load(ctx context.Context, repos []store.Repo, counts bool) (*world, error) {
	metas, err := e.Store.Credentials(ctx)
	if err != nil {
		return nil, err
	}
	w := &world{
		creds:    make(map[string]store.CredentialMeta, len(metas)),
		settings: &settingsMemo{store: e.Store, values: map[string]settingRead{}},
		imports: func(ctx context.Context, repo store.Repo) ([]store.Repo, error) {
			return e.Store.RepoImports(ctx, repo.ID)
		},
	}
	for _, m := range metas {
		w.creds[m.ID] = m
	}
	if !counts {
		return w, nil
	}
	for _, r := range repos {
		if r.TrackerBinding != store.TrackerBindingBuiltin {
			continue
		}
		if w.builtinOpen, err = e.Store.OpenIssueCounts(ctx, ""); err != nil {
			return nil, err
		}
		if w.builtinReady, err = e.Store.OpenIssueCounts(ctx, tracker.ReadyLabel); err != nil {
			return nil, err
		}
		break
	}
	return w, nil
}

// summary is one repo's Summary under w.
func (e *Evaluator) summary(ctx context.Context, w *world, repo store.Repo) Summary {
	return Summary{
		Claimable:  e.claimable(ctx, w, repo),
		OpenIssues: e.openIssues(w, repo),
		Readiness:  Evaluate(e.input(ctx, w, repo)),
	}
}

// claimable is the repo's claimable count, nil when not known. A
// builtin-bound repo's is computed fresh — its ready queue is a store query
// and its claim branches a local ref listing — and an empty ready queue
// answers zero without even that. A forge-bound repo's is whatever the AFK
// engine or an operator view last computed: reading it again would be a
// forge request per repo per page view.
func (e *Evaluator) claimable(ctx context.Context, w *world, repo store.Repo) *int {
	if repo.TrackerBinding != store.TrackerBindingBuiltin {
		if n, ok := e.Recorder.Claimable(repo.ID, repo.TrackerBinding); ok {
			return &n
		}
		return nil
	}
	if e.Claimable == nil || w.builtinReady == nil {
		return nil
	}
	if w.builtinReady[repo.ID] == 0 {
		return new(int)
	}
	n, err := e.Claimable.LocalClaimableCount(ctx, repo)
	if err != nil {
		return nil // e.g. the clone is not ready: no claim refs to read
	}
	return &n
}

// openIssues is the repo's open issue count, nil when not known: a store
// count for the built-in tracker, the size of the last open-set read for a
// forge.
func (e *Evaluator) openIssues(w *world, repo store.Repo) *int {
	if repo.TrackerBinding == store.TrackerBindingBuiltin {
		if w.builtinOpen == nil {
			return nil
		}
		n := w.builtinOpen[repo.ID]
		return &n
	}
	if n, ok := e.Recorder.OpenIssues(repo.ID); ok {
		return &n
	}
	return nil
}

// input gathers one repo's Input. Nothing here can fail the evaluation: a
// fact that cannot be read is simply absent, and Evaluate leaves the check
// that needed it out.
func (e *Evaluator) input(ctx context.Context, w *world, repo store.Repo) Input {
	in := Input{Repo: repo}

	// Git credential: the row, its current stamp, the last fetch.
	in.GitCredential, in.GitCredentialStamp = w.credential(repo.CredentialID)
	if rec, ok := e.Recorder.Fetch(repo.ID); ok {
		in.Fetch = &rec
	}

	// Tracker: the binding's local validation, then the last list read.
	if repo.TrackerBinding != store.TrackerBindingBuiltin && e.Tracker != nil {
		in.TrackerChecked = true
		in.TrackerConfigErr = e.Tracker.CheckConfig(ctx, repo)
		_, in.ForgeCredentialStamp = w.credential(repo.ForgeCredentialID)
		in.TrackerReads = e.Recorder.TrackerReads(repo.ID)
	}

	// Agents: who would run here, and what is known of their login.
	provs := e.providers(ctx, repo)
	for _, p := range provs {
		l := Login{Provider: p.DisplayName()}
		if peek, ok := p.(provider.AuthPeeker); ok {
			var st provider.AuthStatus
			st, l.Known = peek.LastAuthStatus()
			l.LoggedIn = st.LoggedIn
		}
		in.Logins = append(in.Logins, l)
	}

	// Runner and, for a container repo, the spawn gate plus the last
	// pull-if-missing of the image it resolves.
	in.Runner, in.RunnerErr = instance.EffectiveRunner(ctx, w.settings, repo)
	if in.RunnerErr == nil && in.Runner == store.RunnerContainer && e.Spawner != nil && len(provs) > 0 {
		c := &ContainerInput{}
		for _, p := range provs {
			c.Gate, c.Provider = e.Spawner.ContainerGate(ctx, p.ID(), repo), p.DisplayName()
			if c.Gate.Stage != instance.ContainerGateOpen {
				break // the first closed gate is the report
			}
		}
		if c.Gate.Stage == instance.ContainerGateOpen {
			if rec, ok := e.Recorder.Image(c.Gate.Image); ok {
				c.Image = &rec
			}
		}
		in.Container = c
	}

	// Read-only imports: each target's clone state and last fetch.
	if targets, err := w.imports(ctx, repo); err == nil {
		in.ImportsKnown = true
		for _, t := range targets {
			imp := ImportInput{Target: t}
			_, imp.Stamp = w.credential(t.CredentialID)
			if rec, ok := e.Recorder.Fetch(t.ID); ok {
				imp.Fetch = &rec
			}
			in.Imports = append(in.Imports, imp)
		}
	}
	return in
}

// providers resolves the agents a run in repo would use, in report order:
// the effective provider of runs the operator starts (ResolveProvider,
// manual kind), then — only while Auto is on, and only when it is a
// different one — the effective provider of AFK runs. Nil when there is no
// spawner or nothing resolves.
func (e *Evaluator) providers(ctx context.Context, repo store.Repo) []provider.AgentProvider {
	if e.Spawner == nil {
		return nil
	}
	manual, err := e.Spawner.ResolveProvider(ctx, repo, store.RunKindManual, "")
	if err != nil {
		return nil
	}
	provs := []provider.AgentProvider{manual}
	if repo.AFKAutoEnabled {
		if auto, err := e.Spawner.ResolveProvider(ctx, repo, store.RunKindAFKAuto, ""); err == nil && auto.ID() != manual.ID() {
			provs = append(provs, auto)
		}
	}
	return provs
}

// credential returns the credential row id names and its current stamp:
// (nil, store.NoCredentialStamp) for a nil id, and (nil, "") for an id with
// no row — a stamp nothing recorded can match.
func (w *world) credential(id *string) (*store.CredentialMeta, string) {
	if id == nil {
		return nil, store.NoCredentialStamp
	}
	m, ok := w.creds[*id]
	if !ok {
		return nil, ""
	}
	return &m, store.CredentialStamp(m.ID, m.UpdatedAt)
}

// settingsMemo reads each settings row at most once per request. It is the
// instance.RunnerSettings the evaluations hand to EffectiveRunner, so a repo
// list resolves every inheriting repo's Runner from ONE runner_default read
// — errors memoized too, since the resolver distinguishes an absent row from
// an unreadable one.
type settingsMemo struct {
	store  Store
	values map[string]settingRead
}

type settingRead struct {
	value string
	err   error
}

func (m *settingsMemo) GetSetting(ctx context.Context, key string) (string, error) {
	r, ok := m.values[key]
	if !ok {
		r.value, r.err = m.store.GetSetting(ctx, key)
		m.values[key] = r
	}
	return r.value, r.err
}
