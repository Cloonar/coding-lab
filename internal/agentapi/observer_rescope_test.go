package agentapi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/store"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
	"git.cloonar.com/Cloonar/coding-lab/internal/tracker/builtin"
)

// TestCommentAttributionThroughObservedRegistry reproduces the PRODUCTION
// tracker wiring — a real *tracker.Registry with the metrics observer set,
// which wraps every resolved tracker in the observer decorator — and pins
// that the run-identity rescope survives the wrapping. Regression: the
// handler used to assert the concrete *builtin.Tracker, which the decorator
// masks, so with metrics wired every agent comment silently fell back to
// the operator identity (author_kind=operator, run_id NULL) while every
// test — fixtures resolve bare builtin trackers — stayed green.
func TestCommentAttributionThroughObservedRegistry(t *testing.T) {
	f := newFixture(t)
	f.seedRepo(t, "repo_a")
	f.seedIssue(t, "iss7", "repo_a", 7, "Fix it", "")
	f.seedRun(t, "run_afk", "repo_a", "active")
	token := f.seedToken(t, "run_afk", nil)

	reg := tracker.NewRegistry(f.st, nil, nil, builtin.New,
		func(tracker.ForgejoConfig) tracker.Tracker { return nil },
		func(tracker.GitHubConfig) tracker.Tracker { return nil })
	observed := 0
	reg.SetObserver(func(string, string, bool) { observed++ })

	srv := New(f.st, reg, nil, discard(), func() time.Time { return f.now }, nil)
	rr := doJSON(t, srv.Handler(), "POST", "/agent/v1/issues/7/comments", token, `{"body":"working on it"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rr.Code, rr.Body.String())
	}

	is, err := f.st.IssueByRepoNumber(context.Background(), "repo_a", 7)
	if err != nil {
		t.Fatalf("IssueByRepoNumber: %v", err)
	}
	if len(is.Comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(is.Comments))
	}
	c := is.Comments[0]
	if c.AuthorKind != store.CommentAuthorRun {
		t.Errorf("author kind = %q, want run (the rescope was skipped behind the observer wrapper)", c.AuthorKind)
	}
	if c.RunID == nil || *c.RunID != "run_afk" {
		t.Errorf("run id = %v, want run_afk", c.RunID)
	}
	if observed == 0 {
		t.Errorf("observer saw no tracker calls — the rescoped tracker dropped the metrics seam")
	}
}
