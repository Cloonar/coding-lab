package store

import (
	"strings"
	"testing"
	"time"
)

// trickyInstants is strictly increasing chronologically (after millisecond
// truncation, which is what fmtTime stores).
var trickyInstants = []time.Time{
	{}, // zero time: 0001-01-01T00:00:00.000Z
	time.Date(1999, 12, 31, 23, 59, 59, 999_000_000, time.UTC),
	time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), // century rollover
	time.Date(2009, 12, 31, 23, 59, 59, 900_000_000, time.UTC),
	time.Date(2010, 1, 1, 0, 0, 0, 1_000_000, time.UTC),                          // .001 right after year change
	time.Date(2026, 7, 5, 8, 0, 0, 6_000_000, time.UTC),                          // .006
	time.Date(2026, 7, 5, 8, 0, 0, 60_000_000, time.UTC),                         // .060 — would sort before .6 without fixed width
	time.Date(2026, 7, 5, 8, 0, 0, 600_000_000, time.UTC),                        // .600
	time.Date(2026, 7, 5, 10, 0, 0, 601_000_000, time.FixedZone("CEST", 2*3600)), // = 08:00:00.601Z
	time.Date(2026, 12, 31, 23, 59, 59, 999_999_999, time.UTC),                   // truncates to .999
	time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
}

func TestFmtTimeFixedWidth(t *testing.T) {
	for _, instant := range trickyInstants {
		got := fmtTime(instant)
		if len(got) != 24 {
			t.Errorf("fmtTime(%v) = %q: len %d, want 24", instant, got, len(got))
		}
		if !strings.HasSuffix(got, "Z") {
			t.Errorf("fmtTime(%v) = %q: want trailing Z (always UTC)", instant, got)
		}
	}
}

func TestFmtTimeLexicographicIsChronological(t *testing.T) {
	for i := 0; i < len(trickyInstants); i++ {
		for j := i + 1; j < len(trickyInstants); j++ {
			si, sj := fmtTime(trickyInstants[i]), fmtTime(trickyInstants[j])
			if si >= sj {
				t.Errorf("fmtTime order broken: %v -> %q not < %v -> %q",
					trickyInstants[i], si, trickyInstants[j], sj)
			}
		}
	}
}

func TestFmtTimeParseTimeRoundtrip(t *testing.T) {
	for _, instant := range trickyInstants {
		got, err := parseTime(fmtTime(instant))
		if err != nil {
			t.Fatalf("parseTime(fmtTime(%v)): %v", instant, err)
		}
		want := storedTime(instant)
		if !got.Equal(want) {
			t.Errorf("roundtrip(%v) = %v, want %v (ms-truncated UTC)", instant, got, want)
		}
	}
}

func TestFmtTimeTruncatesFractionNeverRounds(t *testing.T) {
	instant := time.Date(2026, 7, 5, 8, 0, 0, 999_999_999, time.UTC)
	if got := fmtTime(instant); got != "2026-07-05T08:00:00.999Z" {
		t.Fatalf("fmtTime(.999999999) = %q, want truncation to .999 (rounding would flip lexicographic order)", got)
	}
}

func TestParseTimeRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "not a time", "2026-07-05", "2026-07-05 08:00:00"} {
		if _, err := parseTime(bad); err == nil {
			t.Errorf("parseTime(%q) succeeded, want error", bad)
		}
	}
}
