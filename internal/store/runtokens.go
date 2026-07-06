package store

// Minimal run-token accessors needed by the agent-API auth middleware (M1).
// Token creation at spawn time and the terminal-outcome reap chokepoint land
// with the M5 engine work; only lookup lives here for now.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RunTokenInfo is a run token joined with the columns of its run that the
// §3a validity rule and run-scoped authorization need.
type RunTokenInfo struct {
	TokenID     string
	RunID       string
	RepoID      string
	IssueNumber *int
	Outcome     string
	ExpiresAt   *time.Time
}

// RunTokenByHash returns the run token with the given sha256 hex joined to
// its run, or ErrNotFound. It does NOT decide validity: the §3a rule
// (outcome='active' AND unexpired) is the caller's, with an injected clock.
func (s *Store) RunTokenByHash(ctx context.Context, tokenHash string) (RunTokenInfo, error) {
	row := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT rt.id, r.id, r.repo_id, r.issue_number, r.outcome, rt.expires_at
		 FROM run_tokens rt JOIN runs r ON r.id = rt.run_id
		 WHERE rt.token_hash = ?`), tokenHash)
	var (
		info    RunTokenInfo
		issueN  sql.NullInt64
		expires sql.NullString
	)
	if err := row.Scan(&info.TokenID, &info.RunID, &info.RepoID, &issueN, &info.Outcome, &expires); err != nil {
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
