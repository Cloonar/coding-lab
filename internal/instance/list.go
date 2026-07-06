package instance

import (
	"context"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
)

// InstanceView is one active run annotated for GET /api/v1/instances: the run
// row plus its repo name and the two derived flags — live (is the tmux session
// actually running, from tmuxx NOT the DB — the DB row is history) and
// connecting (the provider's in-flight deep-link capture state).
type InstanceView struct {
	Run        store.Run
	RepoName   string
	Live       bool
	Connecting bool
}

// List returns every active run joined with live tmux liveness and the
// provider's connecting state. Liveness is derived from tmux, never the DB: an
// active row whose session died (not yet reconciled) shows live=false. Ordering
// follows ActiveRuns (newest first).
func (s *Service) List(ctx context.Context) ([]InstanceView, error) {
	active, err := s.store.ActiveRuns(ctx)
	if err != nil {
		return nil, err
	}
	live, err := s.runner.List(ctx)
	if err != nil {
		return nil, err
	}
	liveSet := make(map[string]bool, len(live))
	for _, n := range live {
		liveSet[n] = true
	}
	repos, err := s.store.Repos(ctx)
	if err != nil {
		return nil, err
	}
	repoName := make(map[string]string, len(repos))
	for _, r := range repos {
		repoName[r.ID] = r.Name
	}

	views := make([]InstanceView, 0, len(active))
	for _, run := range active {
		views = append(views, InstanceView{
			Run:        run,
			RepoName:   repoName[run.RepoID],
			Live:       liveSet[run.SessionName],
			Connecting: s.connecting(run.Provider, run.SessionName),
		})
	}
	return views, nil
}
