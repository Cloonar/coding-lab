package tracker

import "testing"

// TestPullState covers the head-match + open-beats-merged-beats-closed
// precedence contract (design §4d, §11 TestPullState open-beats-closed).
func TestPullState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pulls []PullRef
		head  string
		want  string
		ok    bool
	}{
		{"empty", nil, "afk/7", "", false},
		{"no match", []PullRef{{HeadBranch: "afk/9", State: PullOpen}}, "afk/7", "", false},
		{"single open", []PullRef{{HeadBranch: "afk/7", State: PullOpen}}, "afk/7", PullOpen, true},
		{"single merged", []PullRef{{HeadBranch: "afk/7", State: PullMerged}}, "afk/7", PullMerged, true},
		{"single closed", []PullRef{{HeadBranch: "afk/7", State: PullClosed}}, "afk/7", PullClosed, true},
		{
			"open beats closed",
			[]PullRef{{HeadBranch: "afk/7", State: PullClosed}, {HeadBranch: "afk/7", State: PullOpen}},
			"afk/7", PullOpen, true,
		},
		{
			"open beats closed regardless of order",
			[]PullRef{{HeadBranch: "afk/7", State: PullOpen}, {HeadBranch: "afk/7", State: PullClosed}},
			"afk/7", PullOpen, true,
		},
		{
			"merged beats closed",
			[]PullRef{{HeadBranch: "afk/7", State: PullClosed}, {HeadBranch: "afk/7", State: PullMerged}},
			"afk/7", PullMerged, true,
		},
		{
			"open beats merged",
			[]PullRef{{HeadBranch: "afk/7", State: PullMerged}, {HeadBranch: "afk/7", State: PullOpen}},
			"afk/7", PullOpen, true,
		},
		{
			"picks the matching head among many",
			[]PullRef{
				{HeadBranch: "afk/1", State: PullOpen},
				{HeadBranch: "afk/7", State: PullClosed},
				{HeadBranch: "afk/9", State: PullMerged},
			},
			"afk/7", PullClosed, true,
		},
		{
			"unknown state on the only match is ignored",
			[]PullRef{{HeadBranch: "afk/7", State: "draft"}},
			"afk/7", "", false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PullState(tc.pulls, tc.head)
			if got != tc.want || ok != tc.ok {
				t.Errorf("PullState(%+v, %q) = (%q, %v); want (%q, %v)", tc.pulls, tc.head, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestDonePull covers which head-matching pull is the done-signal: open|merged
// matches, closed|absent|unknown does not, and open beats merged on a same-head
// collision — the returned PullRef must be the winning one (checked by Number).
func TestDonePull(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pulls   []PullRef
		head    string
		wantOK  bool
		wantNum int
	}{
		{"no match", []PullRef{{Number: 1, HeadBranch: "afk/9", State: PullOpen}}, "afk/7", false, 0},
		{"open only", []PullRef{{Number: 2, HeadBranch: "afk/7", State: PullOpen}}, "afk/7", true, 2},
		{"merged only", []PullRef{{Number: 3, HeadBranch: "afk/7", State: PullMerged}}, "afk/7", true, 3},
		{"closed only is no match", []PullRef{{Number: 4, HeadBranch: "afk/7", State: PullClosed}}, "afk/7", false, 0},
		{
			"open beats merged — open's ref wins",
			[]PullRef{{Number: 5, HeadBranch: "afk/7", State: PullMerged}, {Number: 6, HeadBranch: "afk/7", State: PullOpen}},
			"afk/7", true, 6,
		},
		{
			"closed beside merged — merged's ref wins",
			[]PullRef{{Number: 7, HeadBranch: "afk/7", State: PullClosed}, {Number: 8, HeadBranch: "afk/7", State: PullMerged}},
			"afk/7", true, 8,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DonePull(tc.pulls, tc.head)
			if ok != tc.wantOK {
				t.Fatalf("DonePull(%+v, %q) ok = %v; want %v", tc.pulls, tc.head, ok, tc.wantOK)
			}
			if ok && got.Number != tc.wantNum {
				t.Errorf("winning pull Number = %d; want %d", got.Number, tc.wantNum)
			}
		})
	}
}

// TestPRPresent covers the done-signal reading: open|merged ⇒ done, closed or
// absent ⇒ not done (§4d, port-spec §3.3 reaper interpretation table).
func TestPRPresent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pulls []PullRef
		head  string
		want  bool
	}{
		{"open is done", []PullRef{{HeadBranch: "afk/7", State: PullOpen}}, "afk/7", true},
		{"merged is done", []PullRef{{HeadBranch: "afk/7", State: PullMerged}}, "afk/7", true},
		{"closed-unmerged is not done", []PullRef{{HeadBranch: "afk/7", State: PullClosed}}, "afk/7", false},
		{"absent is not done", nil, "afk/7", false},
		{
			"open beside a closed collision is done",
			[]PullRef{{HeadBranch: "afk/7", State: PullClosed}, {HeadBranch: "afk/7", State: PullOpen}},
			"afk/7", true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PRPresent(tc.pulls, tc.head); got != tc.want {
				t.Errorf("PRPresent(%+v, %q) = %v; want %v", tc.pulls, tc.head, got, tc.want)
			}
		})
	}
}

// TestIssuePulls covers the issue-list join (issue #88): which OPEN pull, if
// any, resolves each issue — by claim-branch match against the repo's
// afk_branch_pattern, or by a closing directive in Closes — with the highest
// pull number winning a collision and merged / closed-unmerged pulls counting
// as no pull at all. want maps issue number → the winning pull's number.
func TestIssuePulls(t *testing.T) {
	const pattern = "afk/<N>"
	open := func(n int, head string, closes ...int) PullRef {
		if closes == nil {
			closes = []int{}
		}
		return PullRef{Number: n, HeadBranch: head, State: PullOpen, Closes: closes}
	}
	for _, tc := range []struct {
		name    string
		pulls   []PullRef
		pattern string
		want    map[int]int
	}{
		{"empty", nil, pattern, map[int]int{}},
		{"branch match", []PullRef{open(100, "afk/47")}, pattern, map[int]int{47: 100}},
		{"closes match on a non-claim branch", []PullRef{open(100, "feature/x", 47)}, pattern, map[int]int{47: 100}},
		{
			"branch and closes name the same issue once",
			[]PullRef{open(100, "afk/47", 47)}, pattern, map[int]int{47: 100},
		},
		{
			"a pull closing two issues attaches to both",
			[]PullRef{open(100, "feature/x", 47, 48)}, pattern, map[int]int{47: 100, 48: 100},
		},
		{
			"branch issue plus extra closes",
			[]PullRef{open(100, "afk/47", 12)}, pattern, map[int]int{47: 100, 12: 100},
		},
		{
			"newest (highest number) wins regardless of order",
			[]PullRef{open(120, "feature/y", 47), open(100, "afk/47"), open(110, "feature/z", 47)},
			pattern, map[int]int{47: 120},
		},
		{
			"merged and closed-unmerged are no pull",
			[]PullRef{
				{Number: 100, HeadBranch: "afk/47", State: PullMerged, Closes: []int{47}},
				{Number: 101, HeadBranch: "afk/48", State: PullClosed, Closes: []int{48}},
			},
			pattern, map[int]int{},
		},
		{
			"a closed newer pull never shadows an older open one",
			[]PullRef{open(100, "afk/47"), {Number: 130, HeadBranch: "afk/47", State: PullClosed, Closes: []int{}}},
			pattern, map[int]int{47: 100},
		},
		{"non-matching branch and no closes", []PullRef{open(100, "feature/x")}, pattern, map[int]int{}},
		{"not an exact rendering (leading zero)", []PullRef{open(100, "afk/047")}, pattern, map[int]int{}},
		{"custom pattern", []PullRef{open(100, "issue-9"), open(101, "afk/9")}, "issue-<N>", map[int]int{9: 100}},
		{"nil Closes is no closes", []PullRef{{Number: 100, HeadBranch: "x", State: PullOpen}}, pattern, map[int]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := IssuePulls(tc.pulls, tc.pattern)
			if got == nil {
				t.Fatal("IssuePulls returned nil; want a non-nil map")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("IssuePulls = %+v; want issue→pull %v", got, tc.want)
			}
			for issue, pull := range tc.want {
				if got[issue].Number != pull {
					t.Errorf("issue %d → pull %d; want %d (full %+v)", issue, got[issue].Number, pull, got)
				}
			}
		})
	}
}
