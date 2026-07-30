package cronx

import (
	"fmt"
	"strconv"
	"strings"
)

// The three-letter names the month and day-of-week fields accept, matched
// case-insensitively. Sunday is stored as 0 here; the numeric 7 spelling folds
// onto the same bit after the field is parsed.
var (
	monthNames = map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}
	weekdayNames = map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}
)

// fieldSpec describes one of the five positions: the operator-facing field
// name that every error message carries, the inclusive numeric range it
// accepts, and the names it also accepts (nil for the numeric-only fields).
type fieldSpec struct {
	name     string
	min      int
	max      int
	names    map[string]int
	nameHint string // the accepted name span, for error messages
}

var (
	minuteSpec  = fieldSpec{name: "minute", min: 0, max: 59}
	hourSpec    = fieldSpec{name: "hour", min: 0, max: 23}
	domSpec     = fieldSpec{name: "day-of-month", min: 1, max: 31}
	monthSpec   = fieldSpec{name: "month", min: 1, max: 12, names: monthNames, nameHint: "jan-dec"}
	weekdaySpec = fieldSpec{name: "day-of-week", min: 0, max: 7, names: weekdayNames, nameHint: "sun-sat"}
)

// Parse validates a five-field cron expression and returns its Expr. It
// checks syntax and ranges only — an expression that is legal but can never
// match ("0 0 30 2 *") parses fine and simply never fires. Errors are
// operator-facing: each one names the field and the offending value, because
// they surface verbatim in the Schedule form.
func Parse(expr string) (Expr, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return Expr{}, fmt.Errorf("cron: expression %q has %d fields, want exactly 5: minute hour day-of-month month day-of-week", expr, len(fields))
	}

	minute, err := minuteSpec.parse(fields[0])
	if err != nil {
		return Expr{}, err
	}
	hour, err := hourSpec.parse(fields[1])
	if err != nil {
		return Expr{}, err
	}
	dom, err := domSpec.parse(fields[2])
	if err != nil {
		return Expr{}, err
	}
	month, err := monthSpec.parse(fields[3])
	if err != nil {
		return Expr{}, err
	}
	dow, err := weekdaySpec.parse(fields[4])
	if err != nil {
		return Expr{}, err
	}
	// Both 0 and 7 mean Sunday; fold 7 onto 0 so matching reads a single bit.
	if dow&(1<<7) != 0 {
		dow = dow&^(1<<7) | 1
	}

	return Expr{
		minute:        minute,
		hour:          uint32(hour),
		dom:           uint32(dom),
		month:         uint16(month),
		dow:           uint8(dow),
		domRestricted: fields[2] != "*",
		dowRestricted: fields[4] != "*",
	}, nil
}

// parse turns one field's text into its bitmask.
func (s fieldSpec) parse(field string) (uint64, error) {
	var bits uint64
	for _, item := range strings.Split(field, ",") {
		lo, hi, step, err := s.parseItem(item, field)
		if err != nil {
			return 0, err
		}
		// A step wider than the range leaves only the range start selected,
		// which is the standard reading of "10-40/100".
		for v := lo; v <= hi; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

// parseItem reads one comma-separated element as an inclusive lo..hi range
// with a step.
func (s fieldSpec) parseItem(item, field string) (lo, hi, step int, err error) {
	if item == "" {
		return 0, 0, 0, fmt.Errorf("cron: %s field %q has an empty element", s.name, field)
	}

	span := item
	step = 1
	if slash := strings.IndexByte(item, '/'); slash >= 0 {
		span = item[:slash]
		stepText := item[slash+1:]
		if stepText == "" || strings.ContainsRune(stepText, '/') {
			return 0, 0, 0, fmt.Errorf("cron: %s step %q is malformed: write */n or a-b/n with n at least 1", s.name, item)
		}
		n, ok := parseDigits(stepText)
		if !ok {
			return 0, 0, 0, fmt.Errorf("cron: %s step %q is invalid: write */n or a-b/n with a whole number n of at least 1", s.name, item)
		}
		if n == 0 {
			return 0, 0, 0, fmt.Errorf("cron: %s step 0 in %q must be at least 1", s.name, item)
		}
		if span != "*" && !strings.ContainsRune(span, '-') {
			return 0, 0, 0, fmt.Errorf("cron: %s step %q must apply to \"*\" or a range a-b, not a single value", s.name, item)
		}
		step = n
	}

	if span == "*" {
		return s.min, s.max, step, nil
	}
	// A dash at position 0 cannot open a range — no value starts with one —
	// so "-1" reads as a single (invalid) value and says so, instead of
	// reporting a range with a missing lower bound.
	if dash := strings.IndexByte(span, '-'); dash > 0 {
		lo, err = s.value(span[:dash], item)
		if err != nil {
			return 0, 0, 0, err
		}
		hi, err = s.value(span[dash+1:], item)
		if err != nil {
			return 0, 0, 0, err
		}
		if lo > hi {
			return 0, 0, 0, s.wrapError(item, lo, hi)
		}
		return lo, hi, step, nil
	}
	v, err := s.value(span, item)
	if err != nil {
		return 0, 0, 0, err
	}
	return v, v, step, nil
}

// value resolves a single token to its numeric value, range-checked. item is
// the element the token came from, quoted into the message when it adds
// context.
func (s fieldSpec) value(token, item string) (int, error) {
	if token == "" {
		return 0, fmt.Errorf("cron: %s element %q has an empty value", s.name, item)
	}
	if n, ok := parseDigits(token); ok {
		if n < s.min || n > s.max {
			return 0, fmt.Errorf("cron: %s value %d out of range %d-%d", s.name, n, s.min, s.max)
		}
		return n, nil
	}
	if allDigits(token) {
		// Numeric but too large for parseDigits to hold — still a range error.
		return 0, fmt.Errorf("cron: %s value %q out of range %d-%d", s.name, token, s.min, s.max)
	}
	if s.names != nil {
		if v, ok := s.names[strings.ToLower(token)]; ok {
			return v, nil
		}
		if allLetters(token) {
			return 0, fmt.Errorf("cron: %s name %q is unknown: want a three-letter name (%s)", s.name, token, s.nameHint)
		}
		return 0, fmt.Errorf("cron: %s value %q is invalid%s: want a number %d-%d or a three-letter name (%s)", s.name, token, within(token, item), s.min, s.max, s.nameHint)
	}
	return 0, fmt.Errorf("cron: %s value %q is invalid%s: want a number %d-%d (names are accepted only in the month and day-of-week fields)", s.name, token, within(token, item), s.min, s.max)
}

// wrapError rejects a descending range, naming the wrap-free spelling of what
// the operator meant.
func (s fieldSpec) wrapError(item string, lo, hi int) error {
	return fmt.Errorf("cron: %s range %q is not ascending (%d-%d): wrapping ranges are unsupported — write %q instead", s.name, item, lo, hi, s.unwrapped(lo, hi))
}

// unwrapped renders the ascending equivalent of a wrapping range: a two-part
// list, except in the day-of-week field, where Sunday sits at both ends (0 and
// 7) so a range ending on Sunday needs no wrap at all.
func (s fieldSpec) unwrapped(lo, hi int) string {
	if s.max == 7 && hi == 0 {
		return fmt.Sprintf("%d-7", lo)
	}
	head := fmt.Sprintf("%d-%d", lo, s.max)
	if lo == s.max {
		head = strconv.Itoa(lo)
	}
	tail := fmt.Sprintf("%d-%d", s.min, hi)
	if hi == s.min {
		tail = strconv.Itoa(hi)
	}
	return head + "," + tail
}

// within renders the " in \"...\"" context suffix, omitted when the token is
// the whole element and repeating it would say nothing.
func within(token, item string) string {
	if token == item {
		return ""
	}
	return fmt.Sprintf(" in %q", item)
}

// parseDigits reads an unsigned decimal token, reporting false for anything
// that is not all digits or does not fit comfortably in an int.
func parseDigits(s string) (int, bool) {
	if !allDigits(s) {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func allLetters(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i] | 0x20
		if c < 'a' || c > 'z' {
			return false
		}
	}
	return s != ""
}
