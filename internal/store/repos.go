package store

// Repo accessors (design §3a). The store persists what it is given: defaults
// (name sanitization, tracker binding resolution, branch patterns, credential
// kind checks, pattern grammar validation) are applied by the caller before
// CreateRepo/UpdateRepoSettings. Clone execution lives in gitx; only the
// clone_status column and its startup healing live here.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

// Tracker bindings (design §3a; CONTEXT.md "tracker binding").
const (
	TrackerBindingForge   = "forge"
	TrackerBindingBuiltin = "builtin"
)

// Clone statuses (design §3a).
const (
	CloneStatusCloning = "cloning"
	CloneStatusReady   = "ready"
	CloneStatusError   = "error"
)

// CloneErrorInterrupted is the clone_error written by HealInterruptedClones
// for repos found mid-clone at startup; the operator retries from the UI.
const CloneErrorInterrupted = "interrupted by restart"

// Repo is one row of repos — every §3a column. Nil pointers are NULL.
type Repo struct {
	ID                string
	Name              string // sanitized, UNIQUE
	RemoteURL         string
	CredentialID      *string // GIT credential: kind ssh_key|https_token only (app-enforced)
	ForgeCredentialID *string // kind forge_token only; required when tracker_binding='forge'
	TrackerBinding    string  // forge|builtin
	ForgeKind         string  // forgejo|github|none (tracker.Detect at add time)
	DefaultBranch     string
	// Provider is the repo's agent-CLI override (issue #66). Nil / NULL means
	// inherit the next layer (the global provider_default setting, then the
	// first registered provider) — the same skip-layer chain model/effort use.
	Provider      *string
	ModelDefault  *string
	EffortDefault *string
	// AFK-override spawn defaults (issue #19 / ADR-0021). Nil / NULL means
	// inherit the base ModelDefault/EffortDefault above (and the globals behind
	// them). AFKOptions is the provider-owned options bag, nil when unset (a
	// present-but-empty map means "explicitly no options", distinct from NULL).
	AFKModelDefault  *string
	AFKEffortDefault *string
	AFKOptions       map[string]string
	// AFKProviderDefault is the AFK-override sibling of Provider (issue #66):
	// the provider an unattended run resolves FIRST, before the global
	// spawn_provider_default_afk and the base Provider chain. Nil = inherit.
	AFKProviderDefault *string
	// RemoteDefault / AFKRemoteDefault are the repo layer of the remote-control
	// knob (issue #163), the base and AFK-override pair that sit under the
	// spawn_remote_default / spawn_remote_default_afk globals. They are *bool,
	// not bool, and nil / NULL means inherit: false is a LEGAL value here, so
	// unlike the string knobs above, "unset" has no in-band spelling of its own.
	// The names carry the "Default" suffix on purpose — RemoteURL right at the
	// top of this struct is the git clone URL, and a bare Remote field would sit
	// beside it as a trap.
	RemoteDefault    *bool
	AFKRemoteDefault *bool
	// AFKPrompt is the repo-level AFK seed-prompt override (issue #52 /
	// ADR-0027). Nil / NULL means inherit the next layer (the global afk_prompt
	// setting, then the built-in template). A non-empty value REPLACES the whole
	// built-in prompt at spawn, with <N>/<BRANCH> tokens interpolated.
	AFKPrompt            *string
	Incogni              bool
	GitAuthorName        *string
	GitAuthorEmail       *string
	AFKBranchPattern     string // contains <N> exactly once; grammar §4a
	ManualBranchPrefix   string
	AFKAutoEnabled       bool
	ConsecutiveFailures  int
	BudgetMinutes        *int   // override; nil → settings
	MaxInstancesOverride *int   // per-repo cap override; nil → settings
	CloneStatus          string // cloning|ready|error
	CloneError           *string
	NextIssueNumber      int
	NextCRNumber         int
	CreatedAt            time.Time
	LastOpenedAt         *time.Time
	// Autoland (issue #181 / ADR-0048), per-repo and default OFF. AutolandEnabled
	// opts the repo in; MaxFixAttempts bounds fix-run SPAWNS (a dead fix run posts
	// no verdict, so counting rejections instead would let it respawn forever);
	// AutoMerge gates a clean PASS (on merges directly, off downgrades to pr
	// approve for a human to merge). LanderProvider is the lander run's provider
	// override — nil means inherit this repo's own Provider chain, the same
	// skip-layer model every other provider knob uses.
	AutolandEnabled bool
	MaxFixAttempts  int
	AutoMerge       bool
	LanderProvider  *string
}

// repoColumns is the one column list every repo SELECT/INSERT uses, in the
// order scanRepo reads.
const repoColumns = `id, name, remote_url, credential_id, forge_credential_id,
	tracker_binding, forge_kind, default_branch, provider, model_default,
	effort_default, incogni, git_author_name, git_author_email,
	afk_branch_pattern, manual_branch_prefix, afk_auto_enabled,
	consecutive_failures, budget_minutes, max_instances_override,
	clone_status, clone_error, next_issue_number, next_cr_number,
	created_at, last_opened_at,
	afk_model_default, afk_effort_default, afk_options, afk_prompt,
	afk_provider_default, remote_default, afk_remote_default,
	autoland_enabled, max_fix_attempts, auto_merge, lander_provider`

// triageLabels are the five canonical triage labels seeded per repo at
// creation (design §3a colors; docs/agents/triage-labels.md meanings).
var triageLabels = [...]struct{ Name, Color string }{
	{"needs-triage", "#fbca04"},
	{"needs-info", "#d4c5f9"},
	{"ready-for-agent", "#0e8a16"},
	{"ready-for-human", "#1d76db"},
	{"wontfix", "#cccccc"},
}

// CreateRepo inserts r exactly as given (the caller has applied every
// default, including clone_status "cloning") and seeds the five triage
// labels in the same transaction. Timestamps are normalized to stored
// precision; the returned Repo is what the database now holds. A UNIQUE name
// violation maps to ErrNameTaken.
func (s *Store) CreateRepo(ctx context.Context, r Repo) (Repo, error) {
	r.CreatedAt = storedTime(r.CreatedAt)
	if r.LastOpenedAt != nil {
		t := storedTime(*r.LastOpenedAt)
		r.LastOpenedAt = &t
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Repo{}, fmt.Errorf("create repo %q: %w", r.Name, err)
	}
	defer func() { _ = tx.Rollback() }()

	afkOptions, err := marshalOptions(r.AFKOptions)
	if err != nil {
		return Repo{}, fmt.Errorf("create repo %q: %w", r.Name, err)
	}
	_, err = tx.ExecContext(ctx, s.rebind(
		`INSERT INTO repos (`+repoColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		r.ID, r.Name, r.RemoteURL, r.CredentialID, r.ForgeCredentialID,
		r.TrackerBinding, r.ForgeKind, r.DefaultBranch, r.Provider, r.ModelDefault,
		r.EffortDefault, r.Incogni, r.GitAuthorName, r.GitAuthorEmail,
		r.AFKBranchPattern, r.ManualBranchPrefix, r.AFKAutoEnabled,
		r.ConsecutiveFailures, r.BudgetMinutes, r.MaxInstancesOverride,
		r.CloneStatus, r.CloneError, r.NextIssueNumber, r.NextCRNumber,
		fmtTime(r.CreatedAt), fmtNullTime(r.LastOpenedAt),
		r.AFKModelDefault, r.AFKEffortDefault, afkOptions, r.AFKPrompt,
		r.AFKProviderDefault, r.RemoteDefault, r.AFKRemoteDefault,
		r.AutolandEnabled, r.MaxFixAttempts, r.AutoMerge, r.LanderProvider)
	if err != nil {
		if isUniqueViolation(err) {
			return Repo{}, fmt.Errorf("create repo %q: %w", r.Name, ErrNameTaken)
		}
		if isForeignKeyViolation(err) {
			// The only FKs an insert can trip are the two credential columns.
			return Repo{}, fmt.Errorf("create repo %q: %w", r.Name, ErrCredentialGone)
		}
		return Repo{}, fmt.Errorf("create repo %q: %w", r.Name, err)
	}
	if err := s.seedRepoLabels(ctx, tx, r.ID); err != nil {
		return Repo{}, fmt.Errorf("create repo %q: %w", r.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return Repo{}, fmt.Errorf("create repo %q: %w", r.Name, err)
	}
	return r, nil
}

// SeedRepoLabels ensures the five triage labels exist for repoID. CreateRepo
// already runs it inside its transaction; the exported form is idempotent
// (existing names, colors included, are left untouched) for reseeding.
func (s *Store) SeedRepoLabels(ctx context.Context, repoID string) error {
	return s.seedRepoLabels(ctx, s.db, repoID)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *Store) seedRepoLabels(ctx context.Context, ex execer, repoID string) error {
	for _, l := range triageLabels {
		_, err := ex.ExecContext(ctx, s.rebind(
			`INSERT INTO labels (id, repo_id, name, color)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT (repo_id, name) DO NOTHING`),
			ids.NewID("lbl"), repoID, l.Name, l.Color)
		if err != nil {
			return fmt.Errorf("seed label %q: %w", l.Name, err)
		}
	}
	return nil
}

// Repos lists every repo ordered by name.
func (s *Store) Repos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+repoColumns+` FROM repos ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	defer func() { _ = rows.Close() }()

	repos := make([]Repo, 0)
	for rows.Next() {
		r, err := scanRepo(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list repos: %w", err)
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	return repos, nil
}

// RepoByID returns the repo with the given id, or ErrNotFound.
func (s *Store) RepoByID(ctx context.Context, id string) (Repo, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT `+repoColumns+` FROM repos WHERE id = ?`), id)
	r, err := scanRepo(row.Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Repo{}, fmt.Errorf("repo by id %q: %w", id, ErrNotFound)
		}
		return Repo{}, fmt.Errorf("repo by id %q: %w", id, err)
	}
	return r, nil
}

// Opt is one optional field of a partial update: the zero value leaves the
// column untouched; Set true writes Value, which may itself be a nil pointer
// for nullable columns.
type Opt[T any] struct {
	Set   bool
	Value T
}

// Set returns an Opt that writes v.
func Set[T any](v T) Opt[T] { return Opt[T]{Set: true, Value: v} }

// RepoSettingsUpdate names every repo column PATCH may touch (pinned M2
// contract). Validation — pattern grammar §4a, credential kinds, enum values
// beyond the DB CHECKs — is the caller's; the store only writes.
type RepoSettingsUpdate struct {
	Name                 Opt[string]
	CredentialID         Opt[*string]
	ForgeCredentialID    Opt[*string]
	TrackerBinding       Opt[string]
	DefaultBranch        Opt[string]
	Provider             Opt[*string] // agent-CLI override (issue #66); nil clears (NULL = inherit)
	ModelDefault         Opt[*string]
	EffortDefault        Opt[*string]
	AFKModelDefault      Opt[*string]
	AFKEffortDefault     Opt[*string]
	AFKProviderDefault   Opt[*string]           // AFK-override provider (issue #66); nil clears (NULL = inherit)
	RemoteDefault        Opt[*bool]             // remote-control override (issue #163); nil clears (NULL = inherit), false is a value
	AFKRemoteDefault     Opt[*bool]             // AFK-override remote-control (issue #163); same tri-state
	AFKOptions           Opt[map[string]string] // nil map clears (NULL); a present map (even empty) is stored as JSON
	AFKPrompt            Opt[*string]           // AFK seed-prompt override (issue #52 / ADR-0027); nil clears (NULL = inherit)
	Incogni              Opt[bool]
	GitAuthorName        Opt[*string]
	GitAuthorEmail       Opt[*string]
	AFKBranchPattern     Opt[string]
	ManualBranchPrefix   Opt[string]
	AFKAutoEnabled       Opt[bool]
	BudgetMinutes        Opt[*int]
	MaxInstancesOverride Opt[*int]
	AutolandEnabled      Opt[bool]
	MaxFixAttempts       Opt[int]
	AutoMerge            Opt[bool]
	LanderProvider       Opt[*string] // lander run's provider override (issue #181); nil clears (NULL = inherit repo's Provider)
}

// UpdateRepoSettings applies the set fields of u to one repo and returns the
// updated row. An empty update is a no-op that still verifies existence.
// ErrNotFound for a missing id; ErrNameTaken on a rename collision.
func (s *Store) UpdateRepoSettings(ctx context.Context, id string, u RepoSettingsUpdate) (Repo, error) {
	var (
		sets []string
		args []any
	)
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if u.Name.Set {
		add("name", u.Name.Value)
	}
	if u.CredentialID.Set {
		add("credential_id", u.CredentialID.Value)
	}
	if u.ForgeCredentialID.Set {
		add("forge_credential_id", u.ForgeCredentialID.Value)
	}
	if u.TrackerBinding.Set {
		add("tracker_binding", u.TrackerBinding.Value)
	}
	if u.DefaultBranch.Set {
		add("default_branch", u.DefaultBranch.Value)
	}
	if u.Provider.Set {
		add("provider", u.Provider.Value)
	}
	if u.ModelDefault.Set {
		add("model_default", u.ModelDefault.Value)
	}
	if u.EffortDefault.Set {
		add("effort_default", u.EffortDefault.Value)
	}
	if u.AFKModelDefault.Set {
		add("afk_model_default", u.AFKModelDefault.Value)
	}
	if u.AFKEffortDefault.Set {
		add("afk_effort_default", u.AFKEffortDefault.Value)
	}
	if u.AFKProviderDefault.Set {
		add("afk_provider_default", u.AFKProviderDefault.Value)
	}
	if u.RemoteDefault.Set {
		add("remote_default", u.RemoteDefault.Value)
	}
	if u.AFKRemoteDefault.Set {
		add("afk_remote_default", u.AFKRemoteDefault.Value)
	}
	if u.AFKOptions.Set {
		v, err := marshalOptions(u.AFKOptions.Value)
		if err != nil {
			return Repo{}, fmt.Errorf("update repo %q: %w", id, err)
		}
		add("afk_options", v)
	}
	if u.AFKPrompt.Set {
		add("afk_prompt", u.AFKPrompt.Value)
	}
	if u.Incogni.Set {
		add("incogni", u.Incogni.Value)
	}
	if u.GitAuthorName.Set {
		add("git_author_name", u.GitAuthorName.Value)
	}
	if u.GitAuthorEmail.Set {
		add("git_author_email", u.GitAuthorEmail.Value)
	}
	if u.AFKBranchPattern.Set {
		add("afk_branch_pattern", u.AFKBranchPattern.Value)
	}
	if u.ManualBranchPrefix.Set {
		add("manual_branch_prefix", u.ManualBranchPrefix.Value)
	}
	if u.AFKAutoEnabled.Set {
		add("afk_auto_enabled", u.AFKAutoEnabled.Value)
	}
	if u.BudgetMinutes.Set {
		add("budget_minutes", u.BudgetMinutes.Value)
	}
	if u.MaxInstancesOverride.Set {
		add("max_instances_override", u.MaxInstancesOverride.Value)
	}
	if u.AutolandEnabled.Set {
		add("autoland_enabled", u.AutolandEnabled.Value)
	}
	if u.MaxFixAttempts.Set {
		add("max_fix_attempts", u.MaxFixAttempts.Value)
	}
	if u.AutoMerge.Set {
		add("auto_merge", u.AutoMerge.Value)
	}
	if u.LanderProvider.Set {
		add("lander_provider", u.LanderProvider.Value)
	}
	if len(sets) == 0 {
		return s.RepoByID(ctx, id)
	}
	args = append(args, id)

	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE repos SET `+strings.Join(sets, ", ")+` WHERE id = ?`), args...)
	if err != nil {
		if isUniqueViolation(err) {
			return Repo{}, fmt.Errorf("update repo %q: %w", id, ErrNameTaken)
		}
		if isForeignKeyViolation(err) {
			// The only FKs an update can trip are the two credential columns.
			return Repo{}, fmt.Errorf("update repo %q: %w", id, ErrCredentialGone)
		}
		return Repo{}, fmt.Errorf("update repo %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Repo{}, fmt.Errorf("update repo %q: %w", id, err)
	}
	if n == 0 {
		return Repo{}, fmt.Errorf("update repo %q: %w", id, ErrNotFound)
	}
	return s.RepoByID(ctx, id)
}

// UpdateRepoCloneStatus moves a repo's clone state machine
// (cloning|ready|error). An empty errMsg clears clone_error to NULL.
func (s *Store) UpdateRepoCloneStatus(ctx context.Context, id, status, errMsg string) error {
	var cloneErr any
	if errMsg != "" {
		cloneErr = errMsg
	}
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE repos SET clone_status = ?, clone_error = ? WHERE id = ?`),
		status, cloneErr, id)
	if err != nil {
		return fmt.Errorf("update repo %q clone status: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update repo %q clone status: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("update repo %q clone status: %w", id, ErrNotFound)
	}
	return nil
}

// TouchRepoOpened stamps last_opened_at.
func (s *Store) TouchRepoOpened(ctx context.Context, id string, openedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, s.rebind(
		`UPDATE repos SET last_opened_at = ? WHERE id = ?`), fmtTime(openedAt), id)
	if err != nil {
		return fmt.Errorf("touch repo %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("touch repo %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("touch repo %q: %w", id, ErrNotFound)
	}
	return nil
}

// DeleteRepo removes the repo row; runs, issues, labels, and change requests
// cascade (FK ON DELETE CASCADE — the sqlite open recipe enables enforcement).
// The M3 guard (refuse while worktrees/instances exist unless forced) is the
// API layer's seam; the store delete is unconditional.
func (s *Store) DeleteRepo(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.rebind(`DELETE FROM repos WHERE id = ?`), id)
	if err != nil {
		return fmt.Errorf("delete repo %q: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete repo %q: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("delete repo %q: %w", id, ErrNotFound)
	}
	return nil
}

// HealInterruptedClones implements the design §3a startup healing: every
// repo still in clone_status='cloning' is probed — probe(id) reports whether
// the bare clone at repos/<id>.git exists and answers `git rev-parse
// --git-dir` — and moved to 'ready' or to 'error' with
// clone_error='interrupted by restart' (the operator retries from the UI).
// It returns the repo IDs healed to ready and those marked error, so the
// caller can publish repo.changed events.
func (s *Store) HealInterruptedClones(ctx context.Context, probe func(id string) bool) (ready, failed []string, err error) {
	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT id FROM repos WHERE clone_status = ?`), CloneStatusCloning)
	if err != nil {
		return nil, nil, fmt.Errorf("heal interrupted clones: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var interrupted []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, nil, fmt.Errorf("heal interrupted clones: %w", err)
		}
		interrupted = append(interrupted, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("heal interrupted clones: %w", err)
	}

	for _, id := range interrupted {
		if probe(id) {
			if err := s.UpdateRepoCloneStatus(ctx, id, CloneStatusReady, ""); err != nil {
				return ready, failed, fmt.Errorf("heal interrupted clones: %w", err)
			}
			ready = append(ready, id)
			continue
		}
		if err := s.UpdateRepoCloneStatus(ctx, id, CloneStatusError, CloneErrorInterrupted); err != nil {
			return ready, failed, fmt.Errorf("heal interrupted clones: %w", err)
		}
		failed = append(failed, id)
	}
	return ready, failed, nil
}

// scanRepo reads one row in repoColumns order.
func scanRepo(scan func(dest ...any) error) (Repo, error) {
	var (
		r                                 Repo
		credID, forgeCredID               sql.NullString
		providerCol                       sql.NullString
		modelDef, effortDef               sql.NullString
		authorName, authorEmail, cloneErr sql.NullString
		budget, maxInst                   sql.NullInt64
		created                           string
		lastOpened                        sql.NullString
		afkModelDef, afkEffortDef         sql.NullString
		afkOptions, afkPrompt             sql.NullString
		afkProviderDef                    sql.NullString
		remoteDef, afkRemoteDef           sql.NullBool
		landerProvider                    sql.NullString
	)
	if err := scan(&r.ID, &r.Name, &r.RemoteURL, &credID, &forgeCredID,
		&r.TrackerBinding, &r.ForgeKind, &r.DefaultBranch, &providerCol, &modelDef,
		&effortDef, &r.Incogni, &authorName, &authorEmail,
		&r.AFKBranchPattern, &r.ManualBranchPrefix, &r.AFKAutoEnabled,
		&r.ConsecutiveFailures, &budget, &maxInst,
		&r.CloneStatus, &cloneErr, &r.NextIssueNumber, &r.NextCRNumber,
		&created, &lastOpened,
		&afkModelDef, &afkEffortDef, &afkOptions, &afkPrompt,
		&afkProviderDef, &remoteDef, &afkRemoteDef,
		&r.AutolandEnabled, &r.MaxFixAttempts, &r.AutoMerge, &landerProvider); err != nil {
		return Repo{}, err
	}
	r.CredentialID = nullStr(credID)
	r.ForgeCredentialID = nullStr(forgeCredID)
	r.Provider = nullStr(providerCol)
	r.ModelDefault = nullStr(modelDef)
	r.EffortDefault = nullStr(effortDef)
	r.GitAuthorName = nullStr(authorName)
	r.GitAuthorEmail = nullStr(authorEmail)
	r.CloneError = nullStr(cloneErr)
	r.BudgetMinutes = nullInt(budget)
	r.MaxInstancesOverride = nullInt(maxInst)
	r.AFKModelDefault = nullStr(afkModelDef)
	r.AFKEffortDefault = nullStr(afkEffortDef)
	r.AFKPrompt = nullStr(afkPrompt)
	r.AFKProviderDefault = nullStr(afkProviderDef)
	r.RemoteDefault = nullBool(remoteDef)
	r.AFKRemoteDefault = nullBool(afkRemoteDef)
	r.LanderProvider = nullStr(landerProvider)

	var err error
	if r.AFKOptions, err = unmarshalOptions(afkOptions); err != nil {
		return Repo{}, err
	}
	if r.CreatedAt, err = parseTime(created); err != nil {
		return Repo{}, err
	}
	if r.LastOpenedAt, err = parseNullTime(lastOpened); err != nil {
		return Repo{}, err
	}
	return r, nil
}

// marshalOptions renders a spawn-options bag for the afk_options TEXT column
// (issue #19): a nil map is NULL (unset → inherit the global bag); a present
// map — even empty — is stored as canonical JSON, so "explicitly no options"
// stays distinct from "inherit". Returns any (nil or string) for the SQL arg.
func marshalOptions(m map[string]string) (any, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal afk_options: %w", err)
	}
	return string(b), nil
}

// unmarshalOptions parses the afk_options TEXT column: NULL or "" → nil map
// (unset). Non-empty JSON is decoded into the bag; malformed JSON is a loud
// error (never a silent empty bag).
func unmarshalOptions(ns sql.NullString) (map[string]string, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(ns.String), &m); err != nil {
		return nil, fmt.Errorf("parse afk_options %q: %w", ns.String, err)
	}
	return m, nil
}

func nullStr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

func nullInt(ni sql.NullInt64) *int {
	if !ni.Valid {
		return nil
	}
	v := int(ni.Int64)
	return &v
}

// nullBool converts a scanned nullable BOOLEAN column (issue #163): NULL → nil
// (inherit), and a present false stays a real false — never folded into nil.
func nullBool(nb sql.NullBool) *bool {
	if !nb.Valid {
		return nil
	}
	v := nb.Bool
	return &v
}
