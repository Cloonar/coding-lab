package gitx

import (
	"regexp"
	"strings"
	"time"
)

// The naming algebra (port of v0 instance.go — the tables in the
// git-worktrees port-spec §2.2–§2.6 are the contract):
//
//   session name  = <repoName>~<label>        (SessionSep, split on FIRST '~')
//   worktree dir  = <repoName>-<label>        (WorktreeSep — NEVER '~' in a path)
//   manual label  = [<user>-]YYYYMMDD-HHMM    (minute-bumped on collision)
//
// The session name is simultaneously the tmux identity, the runs.session_name
// key, and the `claude --remote-control` argument; branch and worktree dir
// derive deterministically from the same (repo, label) pair.

// SessionSep separates a repo name from an instance's label inside a tmux
// session name: every instance is <repoName>~<label>. "~" is deliberate on
// two counts. Collision-freedom: SanitizeRepoName and SanitizeLabel only
// ever emit [A-Za-z0-9_-], so "~" can never occur inside a sanitized
// segment — the FIRST "~" is always the repo/label boundary. tmux-safety:
// "~" is the one separator with no special meaning in tmux's target-pane
// parser (@ % $ : . = are all reserved).
const SessionSep = "~"

// WorktreeSep joins a repo name and a label into a worktree directory name.
// Deliberately "-", not "~": the worktree path becomes the agent's cwd, and
// a "<name>~<digit>" path component matches the Windows 8.3 short-name
// pattern (PROGRA~1) that claude flags as a path-confusion risk — forcing
// manual approval on every file edit and silently stalling an unattended
// run in any --permission-mode. "-" also matches the name git derives for
// the worktree's admin dir under .git/worktrees/.
const WorktreeSep = "-"

// ComposeSessionName renders the tmux session name for an instance of repo
// with the given label — <repoName>~<label>. The inverse of
// ParseSessionName.
func ComposeSessionName(repo, label string) string {
	return repo + SessionSep + label
}

// ParseSessionName recovers (repo, label) from a session name by splitting
// on the FIRST "~". Repo names and labels are both "~"-free after
// sanitizing, so that first separator is always the boundary. A name with
// no separator is handled defensively as a repo of that whole name with an
// empty label (a hand-made session).
func ParseSessionName(name string) (repo, label string) {
	repo, label, _ = strings.Cut(name, SessionSep)
	return repo, label
}

// BelongsTo reports whether session name is an instance of repo: a
// "repo~…" instance, or — defensively — the bare repo name of a hand-made
// session. The separator guard is what makes this prefix-safe: "foo" does
// not own "foobar" (no separator between them), and "foo~x" belongs to
// "foo" while "foobar~x" does not.
func BelongsTo(name, repo string) bool {
	return name == repo || strings.HasPrefix(name, repo+SessionSep)
}

// WorktreeDir is the directory name (under the worktrees root) backing an
// instance's worktree: <repoName>-<label> for a manual instance; an AFK run
// passes the bare issue number as label (<repoName>-<N>, both AFK kinds).
// The result never contains "~" (see WorktreeSep) — both segments are
// sanitized to [A-Za-z0-9_-] before they reach here.
func WorktreeDir(repo, label string) string {
	return repo + WorktreeSep + label
}

// SanitizeLabel reduces a user-supplied label to the same safe character
// set as a repo name (v0 sanitizeLabel: trim, '/'→'-', every other byte
// outside [A-Za-z0-9_-] → '_'). The result can never introduce the "~"
// separator or break tmux targeting. An empty or all-whitespace label
// yields "" (an unlabelled instance).
func SanitizeLabel(raw string) string {
	return SanitizeRepoName(raw)
}

// manualTimestampFmt is the readable, lexically-sortable stamp every manual
// label carries: YYYYMMDD-HHMM (Go's reference time). Minute resolution is
// enough to disambiguate human-paced New-instance clicks; a same-minute
// collision is bumped by UniqueManualLabel.
const manualTimestampFmt = "20060102-1504"

// manualTimestampRe matches the trailing YYYYMMDD-HHMM stamp of a manual
// label, alone (unlabelled) or after a "<user>-" prefix. The leading group
// is greedy so a dashed user label ("my-feature-20260608-1530") keeps its
// dashes; the two trailing 2-digit groups are the HH and MM rendered as the
// clock time.
var manualTimestampRe = regexp.MustCompile(`^(?:(.+)-)?(\d{8})-(\d{2})(\d{2})$`)

// ManualLabel renders a manual instance's label from an optional sanitized
// user label and a time: "<user>-20260608-1530" or bare "20260608-1530"
// when unlabelled. The branch (manual_branch_prefix + label), worktree dir
// (<repoName>-<label>), and the rendered "<user> · 15:30" all derive from
// this one string.
func ManualLabel(user string, t time.Time) string {
	ts := t.Format(manualTimestampFmt)
	if user == "" {
		return ts
	}
	return user + "-" + ts
}

// UniqueManualLabel builds a manual instance's label from an optional
// sanitized user label and time t, bumping t a minute at a time until the
// composed session name is free among taken (the live session set, keyed by
// FULL session name — another repo's same-minute stamp never bumps this
// one). Bumping the minute — rather than suffixing a counter — keeps the
// label a well-formed <user>-<timestamp>, so the session name, branch, and
// worktree dir it all seeds stay unique AND parseable, and the rendered
// clock time stays distinct between same-minute siblings. Clock-injected:
// the caller passes now().
func UniqueManualLabel(repo, user string, t time.Time, taken map[string]bool) string {
	for {
		label := ManualLabel(user, t)
		if !taken[ComposeSessionName(repo, label)] {
			return label
		}
		t = t.Add(time.Minute)
	}
}

// ParseManualLabel splits a manual instance's label into its user-supplied
// portion (empty when unlabelled) and the HH:MM clock time its timestamp
// encodes, for rendering "<user> · 15:30" / "15:30". A label with no
// well-formed trailing timestamp (a hand-made session) falls back to the
// whole label as the user portion with no time. Callers gate on the AFK
// label check first, so an AFK label never reaches here.
func ParseManualLabel(label string) (user, hhmm string) {
	m := manualTimestampRe.FindStringSubmatch(label)
	if m == nil {
		return label, ""
	}
	return m[1], m[3] + ":" + m[4]
}
