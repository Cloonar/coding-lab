package tracker

import "testing"

// TestChecksState covers the ChecksState aggregate contract (checks.go):
// ChecksNone iff zero rows; otherwise failure > pending > success precedence,
// with an unrecognized State ranking as failure — the opposite default from
// pulls.go's stateRank, deliberately, because a vocabulary bug must be loud.
func TestChecksState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []Check
		want   string
	}{
		{"zero rows is none", nil, ChecksNone},
		{"all success", []Check{{State: CheckSuccess}, {State: CheckSuccess}}, CheckSuccess},
		{"success and pending is pending", []Check{{State: CheckSuccess}, {State: CheckPending}}, CheckPending},
		{"pending and failure is failure", []Check{{State: CheckPending}, {State: CheckFailure}}, CheckFailure},
		{"failure and success is failure", []Check{{State: CheckFailure}, {State: CheckSuccess}}, CheckFailure},
		{"single pending", []Check{{State: CheckPending}}, CheckPending},
		{
			"unknown state among successes is failure",
			[]Check{{State: CheckSuccess}, {State: "flaky"}, {State: CheckSuccess}},
			CheckFailure,
		},
		{"unknown state alone is failure", []Check{{State: "weird"}}, CheckFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChecksState(tc.checks); got != tc.want {
				t.Errorf("ChecksState(%+v) = %q, want %q", tc.checks, got, tc.want)
			}
		})
	}
}
