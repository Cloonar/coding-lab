package tracker

import "testing"

// TestParseCloses pins the shared closing-keyword grammar's extraction form:
// every real directive's number, deduplicated and sorted ascending, with the
// same word/number boundaries the agent API's trailer injection relies on.
func TestParseCloses(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []int
	}{
		{"empty body", "", []int{}},
		{"no directive", "just prose about #7", []int{}},
		{"single closes", "Closes #7", []int{7}},
		{"case-insensitive", "cLoSeD #7", []int{7}},
		{"full keyword set", "close #1 closes #2 closed #3 fix #4 fixes #5 fixed #6 resolve #7 resolves #8 resolved #9", []int{1, 2, 3, 4, 5, 6, 7, 8, 9}},
		{"colon form", "Fixes: #3", []int{3}},
		{"dedup", "Closes #7\ncloses #7\nFixes #7", []int{7}},
		{"sorted ascending", "Closes #9, closes #2, fixes #5", []int{2, 5, 9}},
		{"left word boundary", "This discloses #7 in the docs.", []int{}},
		{"right number boundary", "Closes #7abc", []int{}},
		{"adjacent number is its own issue", "Closes #70", []int{70}},
		{"trailing punctuation ok", "Closes #7!", []int{7}},
		{"multiline mixed", "Does the thing.\n\nFixes #2\nCloses #1\ndiscloses #9", []int{1, 2}},
		{"zero is no issue", "Closes #0", []int{}},
		{"no keyword no match", "#7", []int{}},
		{"keyword without hash", "Closes 7", []int{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCloses(tt.body)
			if got == nil {
				t.Fatalf("ParseCloses(%q) = nil, want non-nil", tt.body)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ParseCloses(%q) = %v, want %v", tt.body, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("ParseCloses(%q) = %v, want %v", tt.body, got, tt.want)
				}
			}
		})
	}
}

// TestContainsCloses pins the membership check the agent API's ensureCloses
// uses: a real directive for exactly issue n, nothing looser.
func TestContainsCloses(t *testing.T) {
	tests := []struct {
		body string
		n    int
		want bool
	}{
		{"", 7, false},
		{"Closes #7", 7, true},
		{"closes #7", 7, true},
		{"CLOSES #7 and more", 7, true},
		{"Fixes #7", 7, true},
		{"Resolved #7 earlier.", 7, true},
		{"fix #7", 7, true},
		{"Fixes: #7", 7, true},
		{"Closes #70", 7, false},
		{"Closes #17", 7, false},
		{"See Closes #71, Closes #7.", 7, true},
		{"This discloses #7 in the docs.", 7, false},
		{"Closes #7abc", 7, false},
		{"Does the thing.", 7, false},
		// Leading zeros parse like ParseCloses (Atoi), not byte-compare:
		// "Fixes #07" IS a directive for issue 7 on the forge too.
		{"Fixes #07", 7, true},
		{"Closes #007 done", 7, true},
		{"Closes #07", 70, false},
	}
	for _, tt := range tests {
		if got := ContainsCloses(tt.body, tt.n); got != tt.want {
			t.Errorf("ContainsCloses(%q, %d) = %v, want %v", tt.body, tt.n, got, tt.want)
		}
	}
}
