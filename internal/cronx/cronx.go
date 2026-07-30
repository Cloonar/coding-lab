// Package cronx parses and evaluates five-field cron expressions at minute
// granularity, with no dependencies (ADR-0062). It is the single cron
// implementation behind a Schedule's cadence: write-time validation, the
// server-rendered next-firings preview, and the engine's due check all read
// the same Expr, so the SPA, the API, and the engine can never disagree about
// when a Schedule fires.
//
// # Grammar
//
// Five whitespace-separated fields, in order:
//
//	minute        0-59
//	hour          0-23
//	day-of-month  1-31
//	month         1-12   (also jan-dec)
//	day-of-week   0-7    (also sun-sat; 0 and 7 both mean Sunday)
//
// Each field is "*", a single value, a comma-separated list ("1,15,30"), an
// ascending range ("9-17"), or "*" / a range with a step ("*/15",
// "10-40/10"); a step must be at least 1 and applies to "*" or a range, never
// to a bare value. Month and day-of-week additionally accept case-insensitive
// three-letter names, in ranges too ("mar-jun", "mon-fri"). Every other field
// is numeric only, so "mon" in the minute field is a parse error rather than a
// silent 0.
//
// Deliberately absent, because a Schedule's cadence has no use for them and a
// closed grammar is the point: a seconds field, @-macros (@daily and kin),
// timezone syntax, and the non-standard L/W/# operators.
//
// # Wrapping ranges are rejected
//
// A range must ascend: "22-2" (minute) and "fri-sun" (day-of-week, 5-0) are
// parse errors naming the offending field, not silent wraps. Wrap-around is
// always expressible as a list ("22-59,0-2"), and because Sunday is both 0 and
// 7, a weekend span needs no wrap either: write "5-7" for Friday through
// Sunday.
//
// # Matching
//
// A time matches when its minute, hour, and month are all selected AND the day
// is selected. The day rule is the classic vixie-cron OR: when both the
// day-of-month and the day-of-week field are restricted, the day matches when
// EITHER matches ("0 12 13 * 5" fires at noon on every 13th and on every
// Friday); when only one is restricted, that one decides; when both are "*",
// every day matches. Restricted means the field text is not exactly "*" — so
// "*/1" and "*,5" count as restricted even though they select every value,
// and pairing either with a day-of-week field turns the day rule into an OR.
//
// # Time, location, and DST
//
// Matching is at minute granularity: seconds and nanoseconds are ignored, and
// every field is read from the time's own location — callers pass server-local
// times (a Schedule has no timezone of its own). Next searches wall-clock
// minutes in the location of the time it is given, which yields deterministic,
// deliberately dumb behavior across a DST transition:
//
//   - Spring forward, where wall clock minutes are skipped: a firing inside
//     the gap is simply absent that date. With Europe/Berlin jumping 02:00 to
//     03:00, "30 2 * * *" does not fire on that date at all and resumes the
//     next day.
//   - Fall back, where wall clock minutes repeat: iterating Next produces one
//     firing for the repeated minute, not two. Each wall clock minute resolves
//     to exactly one instant — whichever of the two time.Date picks for that
//     location, which Go leaves undefined and which differs between zones east
//     and west of UTC — and Next never generates the other one.
//
// Nothing here tries to be clever about either case; no catch-up, no doubling.
package cronx

import "time"

// searchYears bounds Next's forward scan. An expression can be perfectly
// legal and still never match — "0 0 30 2 *" asks for February 30th — so the
// scan has to give up somewhere; five years is far past any cadence an
// operator would write and keeps the worst case at a few thousand cheap day
// checks.
const searchYears = 5

// Expr is a parsed five-field cron expression: the selected values of each
// field as a bitmask, plus whether the two day fields are restricted (the day
// OR rule needs to know). It is immutable and allocation-free to evaluate.
// The zero Expr selects nothing and matches no time; only Parse produces a
// usable Expr.
type Expr struct {
	minute uint64 // bits 0-59
	hour   uint32 // bits 0-23
	dom    uint32 // bits 1-31
	month  uint16 // bits 1-12
	dow    uint8  // bits 0-6, Sunday = 0 (a parsed 7 folds onto bit 0)

	domRestricted bool // day-of-month field text is not exactly "*"
	dowRestricted bool // day-of-week field text is not exactly "*"
}

// Matches reports whether t falls in a minute the expression selects. Seconds
// and nanoseconds are ignored, and every field is read in t's own location.
func (e Expr) Matches(t time.Time) bool {
	if !bit(e.minute, t.Minute()) {
		return false
	}
	if !bit(uint64(e.hour), t.Hour()) {
		return false
	}
	if !bit(uint64(e.month), int(t.Month())) {
		return false
	}
	return e.dayMatches(t.Day(), t.Weekday())
}

// Next returns the first minute strictly after `after` that the expression
// matches — in after's location, with seconds and nanoseconds zeroed. The
// strictness matters at a firing site: feeding a match back in returns the
// following match, never the same one again.
//
// The second result is false when no match exists within searchYears of
// `after` (an expression like "0 0 30 2 *" never matches at all).
func (e Expr) Next(after time.Time) (time.Time, bool) {
	loc := after.Location()
	limit := after.AddDate(searchYears, 0, 0)
	limitY, limitMonth, limitD := limit.Date()
	limitM := int(limitMonth)

	y, month, d := after.Date()
	m := int(month)
	for {
		if y > limitY || (y == limitY && (m > limitM || (m == limitM && d > limitD))) {
			return time.Time{}, false
		}
		// Skipping whole days (and whole months) on the calendar fields
		// alone is what keeps the never-matching case cheap: no time value
		// is constructed until a day can actually carry a firing.
		if !bit(uint64(e.month), m) {
			d = 1
			y, m = nextMonth(y, m)
			continue
		}
		if e.dayMatches(d, weekdayOf(y, m, d)) {
			for h := 0; h < 24; h++ {
				if !bit(uint64(e.hour), h) {
					continue
				}
				for mi := 0; mi < 60; mi++ {
					if !bit(e.minute, mi) {
						continue
					}
					candidate := time.Date(y, time.Month(m), d, h, mi, 0, 0, loc)
					if !candidate.After(after) {
						continue
					}
					// A wall clock minute inside a spring-forward gap does
					// not exist: time.Date normalizes it into the other side
					// of the transition, so the reconstructed fields differ
					// and the firing is absent that day (see the package doc).
					if candidate.Day() != d || candidate.Hour() != h || candidate.Minute() != mi {
						continue
					}
					if candidate.After(limit) {
						return time.Time{}, false
					}
					return candidate, true
				}
			}
		}
		d++
		if d > daysInMonth(y, m) {
			d = 1
			y, m = nextMonth(y, m)
		}
	}
}

// dayMatches applies the day rule: OR when both day fields are restricted,
// otherwise whichever one is restricted decides, and an unrestricted pair
// matches every day.
func (e Expr) dayMatches(day int, wd time.Weekday) bool {
	domHit := bit(uint64(e.dom), day)
	dowHit := bit(uint64(e.dow), int(wd))
	switch {
	case e.domRestricted && e.dowRestricted:
		return domHit || dowHit
	case e.domRestricted:
		return domHit
	case e.dowRestricted:
		return dowHit
	default:
		return true
	}
}

// bit reports whether value v is selected in mask. v is always within its
// field's range here, so no bounds dance is needed.
func bit(mask uint64, v int) bool { return mask&(1<<uint(v)) != 0 }

// weekdayOf returns the civil weekday of a calendar date. It reads the date at
// noon UTC deliberately: the weekday of a date is location-independent, and
// noon can never be shifted onto a neighboring date by a DST transition the
// way midnight can.
func weekdayOf(y, m, d int) time.Weekday {
	return time.Date(y, time.Month(m), d, 12, 0, 0, 0, time.UTC).Weekday()
}

// daysInMonth returns the length of month m of year y, leap years included
// (day 0 of the following month is the last day of this one).
func daysInMonth(y, m int) int {
	return time.Date(y, time.Month(m)+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// nextMonth advances a year/month pair by one month.
func nextMonth(y, m int) (int, int) {
	if m == 12 {
		return y + 1, 1
	}
	return y, m + 1
}
