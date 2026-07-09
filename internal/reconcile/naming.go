package reconcile

import (
	"strconv"
	"strings"

	"git.cloonar.com/Cloonar/coding-lab/internal/gitx"
	"git.cloonar.com/Cloonar/coding-lab/internal/tmuxx"
)

// The AFK-label → branch derivation reconcile needs to compute which branch a
// live session occupies (ownedBranches). The AFK label format is fixed
// (afk-<N> / afk-auto-<N>, sessions-spawn port spec §2.7) while the branch it
// maps to is the repo's configured afk_branch_pattern — nothing here assumes
// the literal "afk/". The full AFK label renderer + the claims oracle are the
// M5 engine's; M3 needs only the inverse parse for ownership.

const (
	afkLabelPrefix = "afk-"
	afkAutoMarker  = "auto-"
)

// parseAFKLabel is the strict inverse of the fixed AFK label format: cut the
// "afk-" prefix (else not AFK), optionally cut "auto-" (an auto run), then
// require a positive integer. Both kinds must round-trip so ownedBranches maps
// every AFK session to its claim branch. Rejects afk-0, afk--1, afk-1x,
// afk-auto-auto-1, and every non-AFK label (port-spec reject table).
func parseAFKLabel(label string) (n int, auto, ok bool) {
	rest, found := strings.CutPrefix(label, afkLabelPrefix)
	if !found {
		return 0, false, false
	}
	if r, isAuto := strings.CutPrefix(rest, afkAutoMarker); isAuto {
		auto = true
		rest = r
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, false, false
	}
	return n, auto, true
}

// instanceBranch is the branch an instance labelled label of a repo occupies:
// an AFK label (afk-<N> / afk-auto-<N>) → the repo's afk_branch_pattern rendered
// with N (the claim branch — auto and manual AFK share it); any other label →
// the repo's manual_branch_prefix + label. This is identical to Start's branch
// derivation, so the owned set can never drift from what Start created.
func instanceBranch(afkPattern, manualPrefix, label string) string {
	if n, _, ok := parseAFKLabel(label); ok {
		return gitx.RenderBranch(afkPattern, n)
	}
	return manualPrefix + label
}

// ownedBranches is the set of branches live-or-mid-Start sessions of repoName
// occupy — the set reconciliation must NEVER tear down. Ownership, not the
// merged check, is what protects a run: a fresh fork of origin/<default> reads
// clean+merged, so removing this guard would silently destroy in-flight Starts.
// The login session and sessions of other repos are excluded (belongsTo is
// prefix-safe). The afk/<N> ambiguity (afk-<N> and afk-auto-<N> both → the one
// claim branch) is harmless — both collapse to the branch to protect.
func ownedBranches(afkPattern, manualPrefix string, sessions []string, repoName string) map[string]bool {
	owned := make(map[string]bool)
	for _, name := range sessions {
		if tmuxx.IsLoginSession(name) || !gitx.BelongsTo(name, repoName) {
			continue
		}
		_, label := gitx.ParseSessionName(name)
		owned[instanceBranch(afkPattern, manualPrefix, label)] = true
	}
	return owned
}
