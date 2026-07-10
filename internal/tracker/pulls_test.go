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
