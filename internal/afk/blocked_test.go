package afk

import (
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/tracker"
)

// blockedByBody wraps section text under a canonical "## Blocked by" heading —
// the shape the to-issues template renders.
func blockedByBody(section string) string { return "## Blocked by\n" + section }

// readyIssue is a ready-queue issue with a number and body; the gate reads only
// the body's "Blocked by" section.
func readyIssue(n int, body string) tracker.Issue {
	return tracker.Issue{Number: n, Body: body}
}

func numbersOf(issues []tracker.Issue) []int {
	out := make([]int, 0, len(issues))
	for _, is := range issues {
		out = append(out, is.Number)
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestParseBlockedBy pins the section-scoped local-ref grammar: only bounded
// "#<n>" inside a tolerant "Blocked by" ATX section counts, deduplicated and
// sorted ascending, never nil — the same boundary and result conventions as
// tracker.ParseCloses, narrowed to one markdown section.
func TestParseBlockedBy(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []int
	}{
		{"empty body", "", nil},
		{"no section at all", "Just prose about #12 and #34, no heading.", nil},
		{"none placeholder parses as unblocked", blockedByBody("None - can start immediately"), nil},
		{"single ref", blockedByBody("#5"), []int{5}},
		{"multiple refs dedup and sort ascending", blockedByBody("Depends on #9, #4, and #9 again."), []int{4, 9}},
		{
			"section-scoped: refs before and after the section do not count",
			"Intro relates to #99 here.\n\n## Blocked by\n\n- #7\n- #8\n\n## Notes\n\nAlso see #42 afterwards.",
			[]int{7, 8},
		},
		{"heading level 2 with space", "## Blocked by\n#7", []int{7}},
		{"heading level 1 mixed case", "# blocked BY\n#7", []int{7}},
		{"heading no space with colon", "###Blocked by:\n#7", []int{7}},
		{"heading level 6", "###### blocked by\n#7", []int{7}},
		{
			"section terminates at a higher-level heading",
			"### Blocked by\n#5\n\n# Top\n#6",
			[]int{5},
		},
		{
			"section terminates at a lower-level heading",
			"## Blocked by\n#5\n\n##### Sub\n#6",
			[]int{5},
		},
		{
			"bounded refs: #7abc no, x#12 no, owner/repo#12 no, (#13) and - #14 yes",
			blockedByBody("#7abc no, x#12 no, owner/repo#12 no, (#13) yes, - #14 yes"),
			[]int{13, 14},
		},
		{
			"#0 and absurdly long digit strings dropped",
			blockedByBody("#0 and #99999999999999999999999999 and #7"),
			[]int{7},
		},
		{
			"refs in prose and in list items both count",
			blockedByBody("This work depends on #3 landing first.\n\n- also #4\n- and #5"),
			[]int{3, 4, 5},
		},
		{
			"multiple Blocked by headings union their sections",
			"## Blocked by\n#3\n\n## Other\n#99\n\n## blocked by\n#4",
			[]int{3, 4},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseBlockedBy(tt.body)
			if got == nil {
				t.Fatalf("ParseBlockedBy(%q) = nil, want a non-nil slice", tt.body)
			}
			if !equalInts(got, tt.want) {
				t.Errorf("ParseBlockedBy(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

// TestForeignBlockedBy pins the cross-repo/full-URL logging fodder: the two
// canonical off-repo shapes inside the section, verbatim, in order, deduped,
// never nil — and never a local "#<n>" nor a foreign-shaped ref outside the
// section.
func TestForeignBlockedBy(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string
	}{
		{"none-case empty non-nil", blockedByBody("Nothing cross-repo, just #5."), nil},
		{"empty body", "", nil},
		{
			"cross-repo ref returned verbatim, local ref not",
			blockedByBody("Blocked by owner/repo#12 and local #34."),
			[]string{"owner/repo#12"},
		},
		{
			"full issue URL returned verbatim",
			blockedByBody("See https://git.cloonar.com/Cloonar/other/issues/12 for details."),
			[]string{"https://git.cloonar.com/Cloonar/other/issues/12"},
		},
		{
			"both shapes in order of appearance, deduplicated",
			blockedByBody("- owner/repo#12\n- https://git.cloonar.com/Cloonar/other/issues/34\n- owner/repo#12"),
			[]string{"owner/repo#12", "https://git.cloonar.com/Cloonar/other/issues/34"},
		},
		{
			"foreign-shaped refs outside the section do not count",
			"Intro owner/repo#99 before.\n\n## Blocked by\n#5\n\n## Notes\nSee other/repo#77 after.",
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ForeignBlockedBy(tt.body)
			if got == nil {
				t.Fatalf("ForeignBlockedBy(%q) = nil, want a non-nil slice", tt.body)
			}
			if !equalStrings(got, tt.want) {
				t.Errorf("ForeignBlockedBy(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

// TestPartitionBlocked pins the gate itself: a ready issue is blocked IFF one
// of its section refs is currently open, OpenBlockers names only those, input
// order is preserved, and unblocked is non-nil even when empty. A ref absent
// from open (closed/dangling/typo) fails toward progress; a still-open blocker
// blocks even when itself claimed/in-flight; a self-reference is a blocking
// 1-cycle.
func TestPartitionBlocked(t *testing.T) {
	type wantBlk struct {
		num          int
		openBlockers []int
	}
	tests := []struct {
		name          string
		ready         []tracker.Issue
		open          map[int]bool
		wantUnblocked []int
		wantBlocked   []wantBlk
	}{
		{
			"open blocker blocks, OpenBlockers names it",
			[]tracker.Issue{readyIssue(10, blockedByBody("#5"))},
			map[int]bool{5: true},
			nil,
			[]wantBlk{{10, []int{5}}},
		},
		{
			"blocker not in open set is unblocked (closed or dangling ref)",
			[]tracker.Issue{readyIssue(10, blockedByBody("#5"))},
			map[int]bool{},
			[]int{10},
			nil,
		},
		{
			"multiple refs, one open one closed, stays blocked until all resolve",
			[]tracker.Issue{readyIssue(10, blockedByBody("#5 and #6"))},
			map[int]bool{5: true},
			nil,
			[]wantBlk{{10, []int{5}}},
		},
		{
			"multiple open blockers reported sorted ascending",
			[]tracker.Issue{readyIssue(10, blockedByBody("#6, #5"))},
			map[int]bool{5: true, 6: true},
			nil,
			[]wantBlk{{10, []int{5, 6}}},
		},
		{
			"no refs is unblocked (None placeholder)",
			[]tracker.Issue{readyIssue(10, blockedByBody("None - can start immediately"))},
			map[int]bool{5: true},
			[]int{10},
			nil,
		},
		{
			"in-flight blocker still open still blocks",
			[]tracker.Issue{readyIssue(20, blockedByBody("#5"))},
			map[int]bool{5: true},
			nil,
			[]wantBlk{{20, []int{5}}},
		},
		{
			"self-reference is a blocking 1-cycle",
			[]tracker.Issue{readyIssue(10, blockedByBody("#10"))},
			map[int]bool{10: true},
			nil,
			[]wantBlk{{10, []int{10}}},
		},
		{
			"order preserved across mixed unblocked and blocked",
			[]tracker.Issue{
				readyIssue(7, blockedByBody("#1")),
				readyIssue(8, "No blockers here."),
				readyIssue(9, blockedByBody("#2")),
			},
			map[int]bool{1: true},
			[]int{8, 9},
			[]wantBlk{{7, []int{1}}},
		},
		{
			"all blocked leaves unblocked empty and non-nil",
			[]tracker.Issue{
				readyIssue(7, blockedByBody("#1")),
				readyIssue(8, blockedByBody("#2")),
				readyIssue(9, blockedByBody("#3")),
			},
			map[int]bool{1: true, 2: true, 3: true},
			nil,
			[]wantBlk{{7, []int{1}}, {8, []int{2}}, {9, []int{3}}},
		},
		{
			"empty input",
			nil,
			nil,
			nil,
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unblocked, blocked := PartitionBlocked(tt.ready, tt.open)
			if unblocked == nil {
				t.Fatal("PartitionBlocked unblocked is nil, want a non-nil slice")
			}
			if gotUn := numbersOf(unblocked); !equalInts(gotUn, tt.wantUnblocked) {
				t.Errorf("unblocked = %v, want %v", gotUn, tt.wantUnblocked)
			}
			if len(blocked) != len(tt.wantBlocked) {
				t.Fatalf("blocked = %+v, want %d entries %+v", blocked, len(tt.wantBlocked), tt.wantBlocked)
			}
			for i, b := range blocked {
				if b.Issue.Number != tt.wantBlocked[i].num {
					t.Errorf("blocked[%d].Issue.Number = #%d, want #%d (order preserved)", i, b.Issue.Number, tt.wantBlocked[i].num)
				}
				if !equalInts(b.OpenBlockers, tt.wantBlocked[i].openBlockers) {
					t.Errorf("blocked[%d].OpenBlockers = %v, want %v", i, b.OpenBlockers, tt.wantBlocked[i].openBlockers)
				}
			}
		})
	}
}
