package store

// Run-token accessors. Lookup (RunTokenByHash) backs the agent-API auth
// middleware; CreateRunToken mints a token at spawn time (§3a: AFK →
// budget_deadline+30min, manual → NULL expiry) and DeleteRunTokens is the
// terminal-outcome reap chokepoint (a stopped/reaped run's tokens must 401
// immediately, so the token delete rides the same step as the outcome write).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/ids"
)

// CreateRunToken inserts a run token: the caller supplies the run id, the
// token's sha256 hex (the plaintext is shown once, never stored), and the
// expiry (nil → NULL, the manual-run rule; AFK runs pass budget_deadline+30min).
func (s *Store) CreateRunToken(ctx context.Context, runID, tokenHash string, expiresAt *time.Time, createdAt time.Time) error {
	_, err := s.db.ExecContext(ctx, s.rebind(
		`INSERT INTO run_tokens (id, run_id, token_hash, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?)`),
		ids.NewID("rtok"), runID, tokenHash, fmtNullTime(expiresAt), fmtTime(createdAt))
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("create run token: %w", ErrNotFound)
		}
		return fmt.Errorf("create run token: %w", err)
	}
	return nil
}

// DeleteRunTokens removes every token of a run — the reap/stop chokepoint that
// makes a terminated run's tokens 401 at once (§3a auth rule). Deleting none
// (a run that minted no token, or was already reaped) is not an error.
func (s *Store) DeleteRunTokens(ctx context.Context, runID string) error {
	if _, err := s.db.ExecContext(ctx, s.rebind(
		`DELETE FROM run_tokens WHERE run_id = ?`), runID); err != nil {
		return fmt.Errorf("delete run tokens for %q: %w", runID, err)
	}
	return nil
}

// RunTokenInfo is a run token joined with the columns of its run that the
// §3a validity rule and run-scoped authorization need. Kind and Branch feed
// the agent API's PR-create contract: an AFK run's PR body must carry
// `Closes #<N>` and its head is always the run's own branch.
type RunTokenInfo struct {
	TokenID     string
	RunID       string
	RepoID      string
	IssueNumber *int
	Kind        string // manual|afk_manual|afk_auto
	Branch      string
	Outcome     string
	ExpiresAt   *time.Time
}

// RunTokenByHash returns the run token with the given sha256 hex joined to
// its run, or ErrNotFound. It does NOT decide validity: the §3a rule
// (outcome='active' AND unexpired) is the caller's, with an injected clock.
func (s *Store) RunTokenByHash(ctx context.Context, tokenHash string) (RunTokenInfo, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT rt.id, r.id, r.repo_id, r.issue_number, r.kind, r.branch, r.outcome, rt.expires_at
		 FROM run_tokens rt JOIN runs r ON r.id = rt.run_id
		 WHERE rt.token_hash = ?`), tokenHash)
	var (
		info    RunTokenInfo
		issueN  sql.NullInt64
		expires sql.NullString
	)
	if err := row.Scan(&info.TokenID, &info.RunID, &info.RepoID, &issueN, &info.Kind, &info.Branch, &info.Outcome, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunTokenInfo{}, fmt.Errorf("run token by hash: %w", ErrNotFound)
		}
		return RunTokenInfo{}, fmt.Errorf("run token by hash: %w", err)
	}
	if issueN.Valid {
		n := int(issueN.Int64)
		info.IssueNumber = &n
	}
	var err error
	if info.ExpiresAt, err = parseNullTime(expires); err != nil {
		return RunTokenInfo{}, fmt.Errorf("run token by hash: %w", err)
	}
	return info, nil
}
