package cronx

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Expected dates and weekday sets in this file were computed independently of
// the implementation (a calendar library, not cronx), so a wrong day rule or a
// wrong month length fails a test instead of agreeing with itself.

const (
	minuteLayout = "2006-01-02 15:04"
	secondLayout = "2006-01-02 15:04:05"
)

// at parses "2006-01-02 15:04[:05]" in loc.
func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	layout := minuteLayout
	if strings.Count(s, ":") == 2 {
		layout = secondLayout
	}
	parsed, err := time.ParseInLocation(layout, s, loc)
	if err != nil {
		t.Fatalf("bad time literal %q: %v", s, err)
	}
	return parsed
}

// utc is at in UTC, the location almost every case uses.
func utc(t *testing.T, s string) time.Time {
	t.Helper()
	return at(t, time.UTC, s)
}

func mustParse(t *testing.T, expr string) Expr {
	t.Helper()
	e, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): unexpected error: %v", expr, err)
	}
	return e
}

func TestParseAccepts(t *testing.T) {
	exprs := []string{
		"* * * * *",
		"0 0 * * *",
		"59 23 31 12 7",
		"0 0 1 1 0",
		"5,10,15 * * * *",
		"0-30 * * * *",
		"*/15 * * * *",
		"*/1 * * * *",
		"10-40/10 * * * *",
		"0-59/59 * * * *",
		"10-40/100 * * * *",
		"0 0 * jan *",
		"0 0 * DEC *",
		"0 0 * mar-jun *",
		"0 0 * * mon-fri",
		"0 0 * * SUN",
		"0 0 * * Sat",
		"0 0 * * 5-7",
		"0 0 * jan-mar/2 *",
		"0 0 * * mon-fri/2",
		"0 0 13 * 5",
		"5,10-20/3 */6 1,15 jan-mar mon-fri",
		"*\t*  *   *  *",
		"  0 0 * * *  ",
		"*,5 * * * *",
	}
	for _, expr := range exprs {
		t.Run(expr, func(t *testing.T) {
			if _, err := Parse(expr); err != nil {
				t.Fatalf("Parse(%q): unexpected error: %v", expr, err)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want string // substring the operator-facing message must carry
	}{
		{"empty", "", `has 0 fields, want exactly 5`},
		{"blank", "     ", `has 0 fields, want exactly 5`},
		{"four fields", "* * * *", `has 4 fields, want exactly 5`},
		{"six fields", "* * * * * *", `has 6 fields, want exactly 5`},
		{"seconds field", "0 0 12 * * *", `has 6 fields, want exactly 5`},
		{"macro", "@daily", `has 1 fields, want exactly 5`},
		{"garbage", "not a cron expression at all", `has 6 fields, want exactly 5`},

		{"minute high", "63 * * * *", `cron: minute value 63 out of range 0-59`},
		{"minute 60", "60 * * * *", `cron: minute value 60 out of range 0-59`},
		{"hour high", "0 24 * * *", `cron: hour value 24 out of range 0-23`},
		{"dom zero", "0 0 0 * *", `cron: day-of-month value 0 out of range 1-31`},
		{"dom high", "0 0 32 * *", `cron: day-of-month value 32 out of range 1-31`},
		{"month zero", "0 0 * 0 *", `cron: month value 0 out of range 1-12`},
		{"month high", "0 0 * 13 *", `cron: month value 13 out of range 1-12`},
		{"weekday high", "0 0 * * 8", `cron: day-of-week value 8 out of range 0-7`},
		{"huge value", "999999999999999999999 * * * *", `cron: minute value "999999999999999999999" out of range 0-59`},
		{"range endpoint out of range", "0-60 * * * *", `cron: minute value 60 out of range 0-59`},

		{"name in minute", "mon * * * *", `cron: minute value "mon" is invalid`},
		{"name in minute names hint", "mon * * * *", `names are accepted only in the month and day-of-week fields`},
		{"name in hour", "0 noon * * *", `cron: hour value "noon" is invalid`},
		{"name in day-of-month", "0 0 fri * *", `cron: day-of-month value "fri" is invalid`},
		{"unknown month name", "0 0 * foo *", `cron: month name "foo" is unknown: want a three-letter name (jan-dec)`},
		{"unknown weekday name", "0 0 * * funday", `cron: day-of-week name "funday" is unknown: want a three-letter name (sun-sat)`},
		{"weekday name in month", "0 0 * mon *", `cron: month name "mon" is unknown`},
		{"month name in weekday", "0 0 * * jan", `cron: day-of-week name "jan" is unknown`},
		{"negative", "0 0 * * -1", `cron: day-of-week value "-1" is invalid`},
		{"exponent", "1e2 * * * *", `cron: minute value "1e2" is invalid`},
		{"last-day operator", "0 0 L * *", `cron: day-of-month value "L" is invalid`},
		{"nth-weekday operator", "0 0 * * 5#2", `cron: day-of-week value "5#2" is invalid`},

		{"step zero", "*/0 * * * *", `cron: minute step 0 in "*/0" must be at least 1`},
		{"range step zero", "10-40/0 * * * *", `cron: minute step 0 in "10-40/0" must be at least 1`},
		{"empty step", "*/ * * * *", `cron: minute step "*/" is malformed`},
		{"double step", "*/2/2 * * * *", `cron: minute step "*/2/2" is malformed`},
		{"non-numeric step", "*/abc * * * *", `cron: minute step "*/abc" is invalid`},
		{"step on single value", "5/10 * * * *", `cron: minute step "5/10" must apply to "*" or a range a-b, not a single value`},
		{"step with no range", "/5 * * * *", `cron: minute step "/5" must apply to "*" or a range a-b`},

		{"empty list element", "1,,2 * * * *", `cron: minute field "1,,2" has an empty element`},
		{"leading comma", ",5 * * * *", `cron: minute field ",5" has an empty element`},
		{"trailing comma", "0 0 * * 1,", `cron: day-of-week field "1," has an empty element`},
		{"bare dash", "- * * * *", `cron: minute value "-" is invalid`},
		{"open range", "5- * * * *", `cron: minute element "5-" has an empty value`},
		{"triple range", "1-2-3 * * * *", `cron: minute value "2-3" is invalid in "1-2-3"`},

		{"wrapping minutes", "22-2 * * * *", `cron: minute range "22-2" is not ascending (22-2): wrapping ranges are unsupported — write "22-59,0-2" instead`},
		{"wrapping hours", "0 22-6 * * *", `cron: hour range "22-6" is not ascending (22-6): wrapping ranges are unsupported — write "22-23,0-6" instead`},
		{"wrapping months", "0 0 * dec-jan *", `cron: month range "dec-jan" is not ascending (12-1): wrapping ranges are unsupported — write "12,1" instead`},
		// Sunday is both 0 and 7, so the weekend span the operator meant is
		// expressible without a wrap — the message says so.
		{"wrapping weekdays", "0 0 * * fri-sun", `cron: day-of-week range "fri-sun" is not ascending (5-0): wrapping ranges are unsupported — write "5-7" instead`},
		{"wrapping weekday numbers", "0 0 * * 6-0", `cron: day-of-week range "6-0" is not ascending (6-0): wrapping ranges are unsupported — write "6-7" instead`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.expr)
			if err == nil {
				t.Fatalf("Parse(%q): want an error, got none", tc.expr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse(%q) error\n got: %v\nwant substring: %s", tc.expr, err, tc.want)
			}
		})
	}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		name string
		expr string
		when string
		want bool
	}{
		{"every minute", "* * * * *", "2026-07-30 14:39", true},
		{"minute hit", "39 * * * *", "2026-07-30 14:39", true},
		{"minute miss", "38 * * * *", "2026-07-30 14:39", false},
		{"hour hit", "0 14 * * *", "2026-07-30 14:00", true},
		{"hour miss", "0 14 * * *", "2026-07-30 15:00", false},
		{"midnight", "0 0 * * *", "2026-07-30 00:00", true},
		{"list hit", "5,10,15 * * * *", "2026-07-30 14:10", true},
		{"list miss", "5,10,15 * * * *", "2026-07-30 14:11", false},
		{"range hit", "0 9-17 * * *", "2026-07-30 17:00", true},
		{"range miss above", "0 9-17 * * *", "2026-07-30 18:00", false},
		{"range miss below", "0 9-17 * * *", "2026-07-30 08:00", false},

		{"step hit 0", "*/15 * * * *", "2026-07-30 14:00", true},
		{"step hit 15", "*/15 * * * *", "2026-07-30 14:15", true},
		{"step hit 45", "*/15 * * * *", "2026-07-30 14:45", true},
		{"step miss", "*/15 * * * *", "2026-07-30 14:07", false},
		{"range step start", "10-40/10 * * * *", "2026-07-30 14:10", true},
		{"range step middle", "10-40/10 * * * *", "2026-07-30 14:30", true},
		{"range step end", "10-40/10 * * * *", "2026-07-30 14:40", true},
		{"range step between", "10-40/10 * * * *", "2026-07-30 14:35", false},
		{"range step past end", "10-40/10 * * * *", "2026-07-30 14:50", false},
		{"step wider than range keeps start", "10-40/100 * * * *", "2026-07-30 14:10", true},
		{"step wider than range drops rest", "10-40/100 * * * *", "2026-07-30 14:40", false},
		{"star step wider than range", "*/100 * * * *", "2026-07-30 14:00", true},
		{"star step wider than range miss", "*/100 * * * *", "2026-07-30 14:01", false},

		{"month number hit", "0 0 1 7 *", "2026-07-01 00:00", true},
		{"month number miss", "0 0 1 7 *", "2026-08-01 00:00", false},
		{"month name hit", "0 0 1 jul *", "2026-07-01 00:00", true},
		{"month name uppercase", "0 0 1 JUL *", "2026-07-01 00:00", true},
		{"month name range hit", "0 0 1 mar-jun *", "2026-06-01 00:00", true},
		{"month name range miss", "0 0 1 mar-jun *", "2026-07-01 00:00", false},
		{"month step", "0 0 1 jan-dec/3 *", "2026-07-01 00:00", true},
		{"month step miss", "0 0 1 jan-dec/3 *", "2026-08-01 00:00", false},

		// 2026-07-30 is a Thursday, 2026-07-31 a Friday, 2026-08-02 a Sunday,
		// 2026-08-03 a Monday.
		{"weekday number", "0 0 * * 4", "2026-07-30 00:00", true},
		{"weekday number miss", "0 0 * * 3", "2026-07-30 00:00", false},
		{"weekday name", "0 0 * * thu", "2026-07-30 00:00", true},
		{"weekday name uppercase", "0 0 * * THU", "2026-07-30 00:00", true},
		{"weekday name mixed case", "0 0 * * Thu", "2026-07-30 00:00", true},
		{"weekday range", "0 0 * * mon-fri", "2026-07-30 00:00", true},
		{"weekday range excludes sunday", "0 0 * * mon-fri", "2026-08-02 00:00", false},
		{"sunday as 0", "0 0 * * 0", "2026-08-02 00:00", true},
		{"sunday as 7", "0 0 * * 7", "2026-08-02 00:00", true},
		{"sunday name", "0 0 * * sun", "2026-08-02 00:00", true},
		{"sunday 7 excludes monday", "0 0 * * 7", "2026-08-03 00:00", false},
		{"weekend via 5-7 sunday", "0 0 * * 5-7", "2026-08-02 00:00", true},
		{"weekend via 5-7 friday", "0 0 * * 5-7", "2026-07-31 00:00", true},
		{"weekend via 5-7 monday", "0 0 * * 5-7", "2026-08-03 00:00", false},

		{"leap day in leap year", "0 0 29 2 *", "2028-02-29 00:00", true},
		{"leap day wrong month", "0 0 29 2 *", "2028-03-29 00:00", false},
		{"new year", "0 0 1 1 *", "2027-01-01 00:00", true},
		{"last minute of the year", "59 23 31 12 *", "2026-12-31 23:59", true},
		{"last minute of the year, wrong day", "59 23 31 12 *", "2026-12-30 23:59", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := mustParse(t, tc.expr)
			if got := e.Matches(utc(t, tc.when)); got != tc.want {
				t.Fatalf("Parse(%q).Matches(%s) = %v, want %v", tc.expr, tc.when, got, tc.want)
			}
		})
	}
}

func TestMatchesIgnoresSecondsAndNanoseconds(t *testing.T) {
	e := mustParse(t, "39 14 * * *")
	base := utc(t, "2026-07-30 14:39:00")
	for _, offset := range []time.Duration{0, time.Second, 30 * time.Second, 59*time.Second + 999999999} {
		if !e.Matches(base.Add(offset)) {
			t.Fatalf("Matches(%s) = false, want true — sub-minute components must be ignored", base.Add(offset))
		}
	}
	if e.Matches(base.Add(time.Minute)) {
		t.Fatal("Matches(14:40) = true, want false")
	}
}

func TestZeroExprMatchesNothing(t *testing.T) {
	var e Expr
	if e.Matches(utc(t, "2026-07-30 14:39")) {
		t.Fatal("zero Expr matched a time; it selects nothing")
	}
	if next, ok := e.Next(utc(t, "2026-07-30 14:39")); ok {
		t.Fatalf("zero Expr.Next = %v, want no match", next)
	}
}

// TestDayRule pins the day-of-month / day-of-week OR rule by listing, for one
// full month, every day the expression selects at noon. January 2026 starts on
// a Thursday; its Fridays are the 2nd, 9th, 16th, 23rd and 30th, and the 13th
// is a Tuesday — so an OR of "the 13th" and "every Friday" is visibly wider
// than either half.
func TestDayRule(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want []int
	}{
		{"both fields star selects every day", "0 12 * * *", allDays(31)},
		{"day-of-month only", "0 12 13 * *", []int{13}},
		{"day-of-week only", "0 12 * * 5", []int{2, 9, 16, 23, 30}},
		{"both restricted is an OR", "0 12 13 * 5", []int{2, 9, 13, 16, 23, 30}},
		{"OR with a stepped day-of-month", "0 12 */10 * 5", []int{1, 2, 9, 11, 16, 21, 23, 30, 31}},
		{"OR with a range day-of-month", "0 12 1-7 * mon", []int{1, 2, 3, 4, 5, 6, 7, 12, 19, 26}},
		{"stepped day-of-week", "0 12 * * */2", []int{1, 3, 4, 6, 8, 10, 11, 13, 15, 17, 18, 20, 22, 24, 25, 27, 29, 31}},
		// "*/1" selects every value but is still a restricted field, so it
		// turns the day rule into an OR — and an OR with every day is every
		// day, which is exactly how it differs from a bare "*".
		{"*/1 day-of-month is restricted", "0 12 */1 * 5", allDays(31)},
		{"list day-of-month", "0 12 1,15 * *", []int{1, 15}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := mustParse(t, tc.expr)
			var got []int
			for day := 1; day <= 31; day++ {
				if e.Matches(time.Date(2026, time.January, day, 12, 0, 0, 0, time.UTC)) {
					got = append(got, day)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("Parse(%q) selects January days %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

func TestNext(t *testing.T) {
	cases := []struct {
		name  string
		expr  string
		after string
		want  string // empty means: no match within the search bound
	}{
		{"strictly after a matching minute", "* * * * *", "2026-07-30 12:05:00", "2026-07-30 12:06"},
		{"sub-minute input truncates forward", "* * * * *", "2026-07-30 12:05:30", "2026-07-30 12:06"},
		{"sub-minute input on the last nanosecond", "* * * * *", "2026-07-30 12:05:59", "2026-07-30 12:06"},
		{"strictly after its own match", "5 12 * * *", "2026-07-30 12:05:00", "2026-07-31 12:05"},
		{"sub-minute input on its own match", "5 12 * * *", "2026-07-30 12:05:30", "2026-07-31 12:05"},
		{"within the hour", "*/15 * * * *", "2026-07-30 12:00", "2026-07-30 12:15"},
		{"across the hour", "*/15 * * * *", "2026-07-30 12:46", "2026-07-30 13:00"},
		{"across midnight", "0 0 * * *", "2026-07-30 00:00", "2026-07-31 00:00"},
		{"step wider than range fires once an hour", "10-40/100 * * * *", "2026-07-30 00:11", "2026-07-30 01:10"},

		{"next monday", "0 6 * * mon", "2026-07-30 09:00", "2026-08-03 06:00"},
		{"weekdays skip the weekend", "0 22 * * 1-5", "2026-07-31 22:00", "2026-08-03 22:00"},
		{"sunday via 7", "0 6 * * 7", "2026-07-30 09:00", "2026-08-02 06:00"},

		{"month rollover", "0 0 1 * *", "2026-07-30 00:00", "2026-08-01 00:00"},
		{"the 31st skips short months", "0 0 31 * *", "2026-01-31 00:00", "2026-03-31 00:00"},
		{"the 31st skips a 30-day month", "0 0 31 * *", "2026-03-31 00:00", "2026-05-31 00:00"},
		{"year rollover", "59 23 31 12 *", "2026-12-31 23:59", "2027-12-31 23:59"},
		{"annual cadence", "30 4 1 1 *", "2026-05-01 12:00", "2027-01-01 04:30"},

		{"leap day skips three years", "0 0 29 2 *", "2025-06-01 00:00", "2028-02-29 00:00"},
		{"leap day from the day itself", "0 0 29 2 *", "2028-02-29 00:00", "2032-02-29 00:00"},
		{"february 30th never fires", "0 0 30 2 *", "2026-01-01 00:00", ""},
		{"february 31st never fires", "0 0 31 2 *", "2026-01-01 00:00", ""},
		// 2100 is not a leap year, so the gap from 2096 to 2104 is eight
		// years — past the five-year search bound.
		{"leap day past the search bound", "0 0 29 2 *", "2096-03-01 00:00", ""},
		{"leap day inside the search bound", "0 0 29 2 *", "2100-03-01 00:00", "2104-02-29 00:00"},

		// January 2026: the 13th is a Tuesday, the Fridays are 2, 9, 16, 23, 30.
		{"OR rule reaches the friday first", "0 12 13 * 5", "2026-01-01 00:00", "2026-01-02 12:00"},
		{"OR rule reaches the 13th between fridays", "0 12 13 * 5", "2026-01-09 12:00", "2026-01-13 12:00"},
		{"OR rule returns to fridays", "0 12 13 * 5", "2026-01-13 12:00", "2026-01-16 12:00"},

		{"named month", "0 9 1 mar *", "2026-07-30 00:00", "2027-03-01 09:00"},
		{"named month range", "0 9 1 mar-jun *", "2026-07-30 00:00", "2027-03-01 09:00"},
		{"quarterly", "0 9 1 jan,apr,jul,oct *", "2026-07-30 00:00", "2026-10-01 09:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := mustParse(t, tc.expr)
			after := utc(t, tc.after)
			got, ok := e.Next(after)
			if tc.want == "" {
				if ok {
					t.Fatalf("Parse(%q).Next(%s) = %s, want no match", tc.expr, tc.after, got.Format(secondLayout))
				}
				return
			}
			if !ok {
				t.Fatalf("Parse(%q).Next(%s): no match, want %s", tc.expr, tc.after, tc.want)
			}
			want := utc(t, tc.want)
			if !got.Equal(want) {
				t.Fatalf("Parse(%q).Next(%s) = %s, want %s", tc.expr, tc.after, got.Format(secondLayout), want.Format(secondLayout))
			}
			if !e.Matches(got) {
				t.Fatalf("Parse(%q).Next(%s) = %s, which does not Match", tc.expr, tc.after, got)
			}
		})
	}
}

func TestNextTruncatesAndKeepsLocation(t *testing.T) {
	loc := time.FixedZone("lab", 5*3600+1800) // a half-hour offset, on purpose
	e := mustParse(t, "*/5 * * * *")
	after := at(t, loc, "2026-07-30 12:03:27")
	got, ok := e.Next(after)
	if !ok {
		t.Fatal("Next: no match")
	}
	if got.Second() != 0 || got.Nanosecond() != 0 {
		t.Fatalf("Next = %s, want it truncated to the minute", got)
	}
	if got.Location() != loc {
		t.Fatalf("Next location = %s, want %s", got.Location(), loc)
	}
	if want := at(t, loc, "2026-07-30 12:05:00"); !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}
}

// TestNextChainMatchesMinuteScan is the arithmetic proof: over a window of
// minutes, the sequence Next produces must equal the sequence a brute-force
// scan of every single minute finds through Matches. Anything Next skips or
// invents shows up here.
func TestNextChainMatchesMinuteScan(t *testing.T) {
	exprs := []string{
		"* * * * *",
		"*/7 3 * * *",
		"0 12 13 * 5",
		"30 2 * * *",
		"0 0 1,15 * *",
		"5,10-20/3 */6 * jan-mar mon-fri",
		"59 23 31 12 *",
		"0 0 29 2 *",
		"0 0 31 * *",
		"*/13 */5 */3 * *",
	}
	windows := []struct {
		name    string
		start   string
		minutes int
	}{
		{"january", "2026-01-01 00:00", 45 * 24 * 60},
		{"year rollover", "2026-12-01 00:00", 45 * 24 * 60},
		{"leap february", "2028-02-01 00:00", 40 * 24 * 60},
	}
	for _, w := range windows {
		for _, expr := range exprs {
			t.Run(w.name+" "+expr, func(t *testing.T) {
				e := mustParse(t, expr)
				start := utc(t, w.start)
				end := start.Add(time.Duration(w.minutes) * time.Minute)

				var want []time.Time
				for i := 1; i <= w.minutes; i++ {
					m := start.Add(time.Duration(i) * time.Minute)
					if e.Matches(m) {
						want = append(want, m)
					}
				}

				var got []time.Time
				for cursor := start; ; {
					next, ok := e.Next(cursor)
					if !ok || next.After(end) {
						break
					}
					got = append(got, next)
					cursor = next
				}

				if len(got) != len(want) {
					t.Fatalf("Next chain produced %d firings, minute scan found %d", len(got), len(want))
				}
				for i := range got {
					if !got[i].Equal(want[i]) {
						t.Fatalf("firing %d: Next chain %s, minute scan %s", i, got[i], want[i])
					}
				}
			})
		}
	}
}

// TestNextChainStaysSaneOverManyIterations walks each expression far past its
// own period, asserting the chain never repeats, never goes backwards, and
// never lands on a minute the expression does not select.
func TestNextChainStaysSaneOverManyIterations(t *testing.T) {
	cases := []struct {
		expr       string
		start      string
		iterations int
	}{
		{"*/7 3 * * *", "2026-01-01 00:00", 100},
		{"0 12 13 * 5", "2026-01-01 00:00", 100},
		{"59 23 * * sun", "2026-06-15 00:00", 100},
		{"0 0 1 * *", "2026-01-01 00:00", 100},
		{"0 0 1,15 jan,jul mon", "2026-01-01 00:00", 100},
		// Leap days come every four years, so 20 iterations already reach
		// 2080; going further would eventually hit the 2100/2104 gap, which
		// is wider than the search bound (TestNext covers that on purpose).
		{"0 0 29 2 *", "2000-01-01 00:00", 20},
	}
	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			e := mustParse(t, tc.expr)
			cursor := utc(t, tc.start)
			for i := 0; i < tc.iterations; i++ {
				next, ok := e.Next(cursor)
				if !ok {
					t.Fatalf("iteration %d: Next(%s) found no match", i, cursor)
				}
				if !next.After(cursor) {
					t.Fatalf("iteration %d: Next(%s) = %s, which is not strictly later", i, cursor, next)
				}
				if !e.Matches(next) {
					t.Fatalf("iteration %d: Next(%s) = %s, which does not Match", i, cursor, next)
				}
				if next.Second() != 0 || next.Nanosecond() != 0 {
					t.Fatalf("iteration %d: Next(%s) = %s, want it truncated to the minute", i, cursor, next)
				}
				cursor = next
			}
		})
	}
}

// TestDSTSpringForward: a firing whose wall clock minute does not exist on the
// transition date is absent that date and resumes the next day. Europe/Berlin
// jumps 02:00 CET to 03:00 CEST on 2026-03-29, so 02:30 never happens.
func TestDSTSpringForward(t *testing.T) {
	loc := berlin(t)
	e := mustParse(t, "30 2 * * *")

	got, ok := e.Next(at(t, loc, "2026-03-28 12:00"))
	if !ok {
		t.Fatal("Next: no match")
	}
	want := at(t, loc, "2026-03-30 02:30")
	if !got.Equal(want) {
		t.Fatalf("Next = %s, want %s — the 02:30 firing on 2026-03-29 does not exist and must be skipped", got, want)
	}

	// Nothing on the transition date matches, either, however it is reached.
	for _, absent := range []string{"2026-03-29 02:30"} {
		if candidate := at(t, loc, absent); candidate.Hour() == 2 {
			t.Fatalf("expected %s to normalize out of the gap, got %s", absent, candidate)
		}
	}

	// An hour that does exist on that date still fires.
	e3 := mustParse(t, "30 3 * * *")
	got3, ok := e3.Next(at(t, loc, "2026-03-29 00:00"))
	if !ok {
		t.Fatal("Next: no match for the 03:30 firing")
	}
	if want3 := at(t, loc, "2026-03-29 03:30"); !got3.Equal(want3) {
		t.Fatalf("Next = %s, want %s", got3, want3)
	}
}

// TestDSTFallBack: a repeated wall clock minute fires once, not twice.
// Europe/Berlin repeats 02:00-02:59 on 2026-10-25.
func TestDSTFallBack(t *testing.T) {
	loc := berlin(t)
	e := mustParse(t, "30 2 * * *")

	got, ok := e.Next(at(t, loc, "2026-10-24 12:00"))
	if !ok {
		t.Fatal("Next: no match")
	}
	if got.Year() != 2026 || got.Month() != time.October || got.Day() != 25 || got.Hour() != 2 || got.Minute() != 30 {
		t.Fatalf("Next = %s, want the 02:30 firing on 2026-10-25", got)
	}
	// Which of the two instants that wall clock minute resolves to is the
	// location's business (see the package doc); both are legitimate.
	_, offset := got.Zone()
	if offset != 3600 && offset != 7200 {
		t.Fatalf("Next = %s has offset %ds, want one of the two Berlin offsets", got, offset)
	}

	// The second pass over 02:30 is not a second firing: the chain moves on
	// to the next day.
	after, ok := e.Next(got)
	if !ok {
		t.Fatal("Next: no match after the fall-back firing")
	}
	if want := at(t, loc, "2026-10-26 02:30"); !after.Equal(want) {
		t.Fatalf("Next after the fall-back firing = %s, want %s — the repeated hour must fire once", after, want)
	}

	// The repeated hour yields one firing per wall clock minute, not two: an
	// every-15-minutes cadence covers the 25-hour transition date with 96
	// firings (one per wall clock quarter hour), not the 100 that firing
	// twice through the repeated hour would produce.
	quarterly := mustParse(t, "*/15 * * * *")
	count := 0
	cursor := at(t, loc, "2026-10-24 23:59")
	end := at(t, loc, "2026-10-26 00:00")
	for {
		next, ok := quarterly.Next(cursor)
		if !ok || !next.Before(end) {
			break
		}
		count++
		cursor = next
	}
	if count != 96 {
		t.Fatalf("*/15 produced %d firings on the fall-back date, want 96 (one per wall clock quarter hour)", count)
	}
	// That day really is 25 hours long, so the 96 firings span an extra hour.
	if span := end.Sub(at(t, loc, "2026-10-25 00:00")); span != 25*time.Hour {
		t.Fatalf("2026-10-25 in Berlin spans %v, want 25h — the fall-back date under test", span)
	}
}

// TestNextInZoneWithOffset runs the brute-force agreement check in a non-UTC
// location, over a window with no transition in it, so a plain offset cannot
// break the arithmetic.
func TestNextInZoneWithOffset(t *testing.T) {
	loc := berlin(t)
	e := mustParse(t, "5,35 */2 * * mon-fri")
	start := at(t, loc, "2026-05-01 00:00")
	const minutes = 20 * 24 * 60
	end := start.Add(minutes * time.Minute)

	var want []time.Time
	for i := 1; i <= minutes; i++ {
		m := start.Add(time.Duration(i) * time.Minute)
		if e.Matches(m) {
			want = append(want, m)
		}
	}
	var got []time.Time
	for cursor := start; ; {
		next, ok := e.Next(cursor)
		if !ok || next.After(end) {
			break
		}
		got = append(got, next)
		cursor = next
	}
	if len(got) != len(want) {
		t.Fatalf("Next chain produced %d firings, minute scan found %d", len(got), len(want))
	}
	for i := range got {
		if !got[i].Equal(want[i]) {
			t.Fatalf("firing %d: Next chain %s, minute scan %s", i, got[i], want[i])
		}
	}
}

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("Europe/Berlin unavailable (no tzdata): %v", err)
	}
	return loc
}

func allDays(n int) []int {
	days := make([]int, n)
	for i := range days {
		days[i] = i + 1
	}
	return days
}
