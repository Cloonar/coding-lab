package main

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// instanceSep separates a project name from an instance's label inside a tmux
// session name: every instance is <project>~<label> (ADR-0017). "~" is
// deliberate on two counts.
//
// Collision-freedom: the project-name and label sanitisers only ever emit
// [A-Za-z0-9_-] (see sessionName / sanitizeLabel), so "~" can never occur inside
// a sanitised segment. The FIRST "~" is therefore always the project/label
// boundary — parseSessionName splits there and nowhere else, unambiguous even
// against a project literally named "foo-2" (its label carries the "~", not its
// name).
//
// tmux-safety: "~" is the one separator with no special meaning in tmux's
// target-pane parser, which the bare-name capture/send path in Sessions relies
// on (@ % $ : . = are all reserved by tmux).
const instanceSep = "~"

// labBranchPrefix namespaces a manual instance's working branch (lab/<label>),
// parallel to afkBranchPrefix (afk/<N>) for AFK runs. Both live on disk in the
// reference repo; the kind a session name encodes (parseAFKLabel) decides which
// one instanceBranch derives.
const labBranchPrefix = "lab/"

// instanceID identifies one running instance of a project. Every instance — AFK
// or manual — is <project>~<label>; <label> is afk-<N> / afk-auto-<N> for an AFK
// run (parseAFKLabel), <userlabel>-<timestamp> or bare <timestamp> for a manual
// one. Branch, worktree dir, and the rendered identity all derive from the pair
// (see instanceBranch / worktreeDir / parseManualLabel). Slots are gone
// (ADR-0017): the label alone is the instance's identity.
type instanceID struct {
	Project string
	Label   string
}

// composeSessionName renders an instanceID to its tmux session name —
// <project>~<label>. The inverse of parseSessionName.
func composeSessionName(id instanceID) string {
	return id.Project + instanceSep + id.Label
}

// parseSessionName recovers the instanceID from a session name by splitting on
// the FIRST "~" (ADR-0017). Project names and labels are both "~"-free after
// sanitising, so that first separator is always the boundary. A name with no
// separator can no longer come from composeSessionName (every instance is
// labelled now); it is handled defensively as a project of that whole name with
// an empty label — a hand-made or pre-ADR-0017 session.
func parseSessionName(name string) instanceID {
	project, label, found := strings.Cut(name, instanceSep)
	if !found {
		return instanceID{Project: name}
	}
	return instanceID{Project: project, Label: label}
}

// belongsTo reports whether session name is an instance of project: a
// "project~…" instance (every real instance is labelled), or — defensively — the
// bare project name of a legacy/hand-made session. The separator guard is what
// makes this prefix-safe: "foo" does not own "foobar" (no separator between
// them), and "foo~x" belongs to "foo" while "foobar~x" does not.
func belongsTo(name, project string) bool {
	return name == project || strings.HasPrefix(name, project+instanceSep)
}

// instanceBranch is the branch an instance works on, derived from its session
// name: afk/<N> for an AFK run (the claim branch, ADR-0013), lab/<label> for a
// manual one. The kind is read from the label via parseAFKLabel, so a single
// session name fixes both the branch namespace and (for AFK) the issue number.
func instanceBranch(id instanceID) string {
	if n, _, ok := parseAFKLabel(id.Label); ok {
		return afkBranch(n)
	}
	return labBranchPrefix + id.Label
}

// worktreeDir is the directory name (under worktreeRoot) backing an instance's
// worktree: <project>-<N> for an AFK run (unchanged — N is the issue number),
// <project>-<label> for a manual one. The "-" join (worktreeSep) is deliberate,
// not "~": a "<name>~<digit>" path component matches the Windows 8.3 short-name
// pattern (PROGRA~1) that claude flags as a path-confusion risk, stalling every
// unattended edit — see worktreeSep. A manual label is already "~"-free
// (sanitised), so <project>-<label> is safe too.
func worktreeDir(id instanceID) string {
	if n, _, ok := parseAFKLabel(id.Label); ok {
		return id.Project + worktreeSep + strconv.Itoa(n)
	}
	return id.Project + worktreeSep + id.Label
}

// manualTimestampFmt is the readable, lexically-sortable stamp every manual label
// carries: YYYYMMDD-HHMM (Go's reference time). Minute resolution is enough to
// disambiguate human-paced New-instance clicks; a same-minute collision is bumped
// (uniqueManualLabel).
const manualTimestampFmt = "20060102-1504"

// manualTimestampRe matches the trailing YYYYMMDD-HHMM stamp of a manual label,
// alone (unlabelled) or after a "<user>-" prefix. The leading group is greedy so
// a dashed user label ("my-feature-20260608-1530") keeps its dashes; the two
// trailing 2-digit groups are the HH and MM rendered as the clock time.
var manualTimestampRe = regexp.MustCompile(`^(?:(.+)-)?(\d{8})-(\d{2})(\d{2})$`)

// manualLabel renders a manual instance's label from an optional sanitised user
// label and a time: "<user>-20260608-1530" or bare "20260608-1530" when
// unlabelled. The branch (lab/<label>), worktree dir (<project>-<label>), and the
// rendered "<user> · 15:30" all derive from this one string.
func manualLabel(user string, t time.Time) string {
	ts := t.Format(manualTimestampFmt)
	if user == "" {
		return ts
	}
	return user + "-" + ts
}

// uniqueManualLabel builds a manual instance's label from an optional user label
// and time t, bumping t a minute at a time until the composed session name is
// free among taken (the live session set). Bumping the minute — rather than
// suffixing a counter — keeps the label a well-formed <user>-<timestamp>, so the
// session name, the lab/<label> branch, and the <project>-<label> worktree dir it
// all seeds stay unique AND parseable, and the rendered clock time stays distinct
// between same-minute siblings.
func uniqueManualLabel(project, user string, t time.Time, taken map[string]bool) string {
	for {
		label := manualLabel(user, t)
		if !taken[composeSessionName(instanceID{Project: project, Label: label})] {
			return label
		}
		t = t.Add(time.Minute)
	}
}

// parseManualLabel splits a manual instance's label into its user-supplied
// portion (empty when unlabelled) and the HH:MM clock time its timestamp encodes,
// for rendering "<user> · 15:30" / "15:30". A label with no well-formed trailing
// timestamp (a hand-made or pre-ADR-0017 session) falls back to the whole label
// as the user portion with no time. Callers gate on parseAFKLabel first, so an
// AFK label never reaches here.
func parseManualLabel(label string) (user, hhmm string) {
	m := manualTimestampRe.FindStringSubmatch(label)
	if m == nil {
		return label, ""
	}
	return m[1], m[3] + ":" + m[4]
}

// sanitizeLabel reduces a user-supplied label to the same safe character set as
// a project name, after trimming surrounding whitespace. The result only ever
// contains [A-Za-z0-9_-], so a label can never introduce the "~" separator or
// otherwise break the session-name scheme or tmux targeting. An empty or
// all-whitespace label yields "" (an unlabelled instance).
func sanitizeLabel(raw string) string {
	return sessionName(strings.TrimSpace(raw))
}

// afkLabelPrefix marks a labelled instance as an AFK run. A manual run's full
// label is afk-<issue>; an auto run's is afk-auto-<issue> (see afkAutoMarker), so
// the session-name scheme carries both the issue number AND the run's kind for
// free: composeSessionName(instanceID{Project, Label: afkLabel(N, auto)}) yields
// <project>~afk-<N> or <project>~afk-auto-<N>, which parseSessionName +
// parseAFKLabel recover. Both forms use only [a-z0-9-], so they survive
// sanitizeLabel unchanged.
const afkLabelPrefix = "afk-"

// afkAutoMarker distinguishes an automatically-scheduled run from a manually
// started one, restart-proof, by sitting between the afk- prefix and the issue
// number: afk-auto-<N>. It MUST keep parseAFKLabel able to recover the issue
// number — a naive afk-<auto-N> style that left "auto-N" where the bare number
// was expected would fail strconv.Atoi and make the reaper silently stop reaping
// auto runs (it only acts on parseAFKRun matches). The "auto-" text is [a-z-]
// only, so it survives sanitizeLabel and never introduces the "~" separator.
const afkAutoMarker = "auto-"

// afkLabel renders an AFK run's instance label for issue number n. An auto run
// carries the afkAutoMarker so the scheduler — and a post-restart re-adoption
// from session names — can tell it from a manual run; both forms round-trip
// through parseAFKLabel.
func afkLabel(n int, auto bool) string {
	if auto {
		return afkLabelPrefix + afkAutoMarker + strconv.Itoa(n)
	}
	return afkLabelPrefix + strconv.Itoa(n)
}

// parseAFKLabel recognises an AFK-run instance label — manual afk-<N> or auto
// afk-auto-<N> — and returns the issue number N and whether it is an auto run. ok
// is false for any other label: an ordinary user label (its trailing timestamp's
// dashes defeat the strconv.Atoi below), or a malformed afk-… / afk-auto-… whose
// numeric suffix is missing, non-numeric, or not a positive issue number — so a
// hand-typed "afk-x" or a user label like "afk-feature" is never mistaken for an
// AFK run. CRITICAL: both kinds must stay recognised here, since the reaper acts
// only on parseAFKRun (hence parseAFKLabel) matches; dropping either would leave
// that kind un-reaped, holding its slot forever.
func parseAFKLabel(label string) (n int, auto, ok bool) {
	rest, found := strings.CutPrefix(label, afkLabelPrefix)
	if !found {
		return 0, false, false
	}
	if num, isAuto := strings.CutPrefix(rest, afkAutoMarker); isAuto {
		rest, auto = num, true
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, false, false
	}
	return n, auto, true
}
