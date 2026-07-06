package gitx

import (
	"testing"
	"time"

	"git.cloonar.com/Cloonar/coding-lab/internal/testutil"
)

func TestParseCloneProgress(t *testing.T) {
	tests := []struct {
		line        string
		wantPhase   string
		wantPercent int
		wantOK      bool
	}{
		// Verbatim shapes from a real `git clone --bare --progress` stderr.
		{"remote: Counting objects:   9% (1/11)", PhaseCounting, 9, true},
		{"remote: Counting objects: 100% (11/11), done.", PhaseCounting, 100, true},
		{"remote: Compressing objects:  40% (4/10)", PhaseCompressing, 40, true},
		{"remote: Compressing objects: 100% (10/10), done.", PhaseCompressing, 100, true},
		{"Receiving objects:  36% (4/11)", PhaseReceiving, 36, true},
		{"Receiving objects: 100% (11/11), 24.00 KiB | 24.00 MiB/s, done.", PhaseReceiving, 100, true},

		// Non-progress lines.
		{"Cloning into bare repository 'dest.git'...", "", 0, false},
		{"remote: Enumerating objects: 11, done.", "", 0, false},
		{"remote: Total 11 (delta 3), reused 0 (delta 0), pack-reused 0 (from 0)", "", 0, false},
		{"Resolving deltas:  50% (2/4)", "", 0, false},
		{"fatal: repository '/no/such/repo' does not exist", "", 0, false},
		{"", "", 0, false},
	}
	for _, tt := range tests {
		phase, percent, ok := parseCloneProgress(tt.line)
		if phase != tt.wantPhase || percent != tt.wantPercent || ok != tt.wantOK {
			t.Errorf("parseCloneProgress(%q) = (%q, %d, %v), want (%q, %d, %v)",
				tt.line, phase, percent, ok, tt.wantPhase, tt.wantPercent, tt.wantOK)
		}
	}
}

func TestProgressThrottle(t *testing.T) {
	clock := testutil.NewFakeClock(time.Date(2026, 6, 8, 15, 30, 0, 0, time.UTC))
	th := &progressThrottle{now: clock.Now, interval: 250 * time.Millisecond}

	steps := []struct {
		advance time.Duration
		phase   string
		percent int
		want    bool
		reason  string
	}{
		{0, PhaseCounting, 9, true, "first callback always passes"},
		{0, PhaseCounting, 18, false, "same phase within interval"},
		{100 * time.Millisecond, PhaseCounting, 27, false, "still within interval"},
		{200 * time.Millisecond, PhaseCounting, 36, true, "interval elapsed"},
		{0, PhaseCounting, 100, true, "reaching 100% always passes"},
		{0, PhaseCounting, 100, false, "repeated 100% (the ', done.' line) suppressed"},
		{0, PhaseCompressing, 10, true, "phase change always passes"},
		{0, PhaseCompressing, 20, false, "back to throttling within the new phase"},
		{0, PhaseReceiving, 5, true, "phase change always passes"},
		{300 * time.Millisecond, PhaseReceiving, 80, true, "interval elapsed"},
	}
	for i, s := range steps {
		clock.Advance(s.advance)
		if got := th.allow(s.phase, s.percent); got != s.want {
			t.Errorf("step %d (%s): allow(%q, %d) = %v, want %v",
				i, s.reason, s.phase, s.percent, got, s.want)
		}
	}
}

func TestLineTail(t *testing.T) {
	tail := &lineTail{max: 3}
	for _, l := range []string{"a", "b", "c", "d", "e"} {
		tail.add(l)
	}
	if got, want := tail.String(), "c\nd\ne"; got != want {
		t.Errorf("lineTail.String() = %q, want %q", got, want)
	}
}
