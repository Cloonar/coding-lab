package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// readyLabel marks an issue as fully specified and waiting for an AFK agent —
// the queue lab claims from (ReadyIssues filters on it). lab writes no claim
// label: an AFK run's claim is its local afk/<N> branch, not a tracker label
// (ADR-0013), so triage and humans can (re)apply readyLabel freely without ever
// clobbering a claim or making the issue flap.
const readyLabel = "ready-for-agent"

// Issue is the subset of a tracker issue the AFK-run claim needs.
type Issue struct {
	Index int
	Title string
}

// PullRequest is the subset of a tracker pull request the AFK-run reaper needs:
// its head branch and state, so the watcher can match an afk/<N> PR client-side
// and decide whether the run is done.
type PullRequest struct {
	Head  string
	State string
}

// The PR states tea emits for the --fields state column. A merged PR renders as
// pullMerged (distinct from pullClosed, which is closed-and-unmerged): the
// reaper treats open and merged as "done", but a closed-unmerged afk/<N> PR as
// no PR — so the run fails on death/timeout rather than being falsely reaped as
// a success.
const (
	pullOpen   = "open"
	pullMerged = "merged"
	pullClosed = "closed"
)

// Tracker is the seam over the `tea` CLI for AFK runs: list the ready queue an
// AFK run claims from, and list the project's pull requests so the reaper can
// match a run's afk/<N> PR. It is read-only — lab claims via an afk/<N> branch,
// not a tracker label (ADR-0013), so there are no label-mutation methods. Every
// call is scoped to a project directory (tea resolves the active repo from its
// working dir). The real implementation shells out; tests substitute a fake so
// the selection/claim decision logic is exercised without a live tracker —
// mirroring how Sessions wraps tmux and Auth wraps claude auth.
type Tracker interface {
	ReadyIssues(dir string) ([]Issue, error)
	ListPulls(dir string) ([]PullRequest, error)
}

// pickLowestIssue returns the lowest-numbered issue from a ready queue, or
// ok=false when the queue is empty. Choosing the minimum here — rather than
// trusting tea's list order — keeps the claim deterministic and is the
// unit-tested core of the selection.
func pickLowestIssue(issues []Issue) (Issue, bool) {
	if len(issues) == 0 {
		return Issue{}, false
	}
	low := issues[0]
	for _, is := range issues[1:] {
		if is.Index < low.Index {
			low = is
		}
	}
	return low, true
}

// teaTracker is the production Tracker: it runs `tea` with the project directory
// as its working dir, which is how tea resolves the active repo.
type teaTracker struct{ bin string }

func NewTeaTracker(bin string) *teaTracker { return &teaTracker{bin: bin} }

func (t *teaTracker) run(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command(t.bin, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out, fmt.Errorf("tea %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return out, fmt.Errorf("tea %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

func (t *teaTracker) ReadyIssues(dir string) ([]Issue, error) {
	out, err := t.run(dir, "issues", "list", "--state", "open", "--labels", readyLabel, "--output", "json")
	if err != nil {
		return nil, err
	}
	return parseIssues(out)
}

// parseIssues decodes `tea issues list --output json`. tea renders the issue
// number as a quoted string ("index":"62"), so it is read as text and
// converted; an entry whose index isn't a number is skipped rather than failing
// the whole list.
func parseIssues(data []byte) ([]Issue, error) {
	var raw []struct {
		Index string `json:"index"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse tea issues: %w", err)
	}
	out := make([]Issue, 0, len(raw))
	for _, r := range raw {
		n, err := strconv.Atoi(r.Index)
		if err != nil {
			continue
		}
		out = append(out, Issue{Index: n, Title: r.Title})
	}
	return out, nil
}

// ListPulls returns every pull request on the project's repo with its head
// branch and state, so the reaper can match an afk/<N> PR client-side. --state
// all is required: a merged afk/<N> PR (the success signal) is no longer "open",
// and the reaper still needs to see it. The index field is requested to match
// ADR-0007's documented query shape even though matching is by head + state.
func (t *teaTracker) ListPulls(dir string) ([]PullRequest, error) {
	out, err := t.run(dir, "pulls", "list", "--state", "all", "--fields", "index,head,state", "--output", "json")
	if err != nil {
		return nil, err
	}
	return parsePulls(out)
}

// parsePulls decodes `tea pulls list --output json`. Only head and state are
// read (matching is client-side on those two); a PR missing either field still
// decodes, with the empty value simply never matching an afk/<N> branch.
func parsePulls(data []byte) ([]PullRequest, error) {
	var raw []struct {
		Head  string `json:"head"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse tea pulls: %w", err)
	}
	out := make([]PullRequest, 0, len(raw))
	for _, r := range raw {
		out = append(out, PullRequest{Head: r.Head, State: r.State})
	}
	return out, nil
}
