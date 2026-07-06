package gitx

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Branch-pattern grammar (design §4a).
//
// A repo's afk_branch_pattern must contain the issue-number token <N>
// exactly once (defaults: "afk/<N>", incogni "issue-<N>"); its
// manual_branch_prefix is a literal prefix (defaults: "lab/", incogni
// "wip/"). Both are validated at repo create/PATCH. Nothing outside
// per-repo config may assume the literal "afk/" or "lab/" prefixes.
//
// Claims are enumerated with `git for-each-ref refs/heads/<PatternPrefix>*`
// (the glob is mandatory — a bare "refs/heads/issue-" matches nothing) and
// then strict-inverse-parsed with ParseBranch, which preserves v0
// parseAFKBranch's ignore-non-matching behavior: a stray branch under the
// prefix that is not an exact rendering never registers as a claim.

// NToken is the issue-number placeholder in an afk branch pattern.
const NToken = "<N>"

// ValidatePattern checks an afk_branch_pattern against the §4a grammar:
// <N> exactly once; ref-safe chars only ([A-Za-z0-9._/-] besides the
// token — no '~', no spaces); no leading or trailing '/'; no "..".
func ValidatePattern(pattern string) error {
	if strings.Count(pattern, NToken) != 1 {
		return fmt.Errorf("afk branch pattern %q: must contain %s exactly once", pattern, NToken)
	}
	literal := strings.Replace(pattern, NToken, "", 1)
	if c, ok := firstBadRefChar(literal); ok {
		return fmt.Errorf("afk branch pattern %q: invalid character %q (allowed: A-Za-z0-9._/- and %s)", pattern, string(c), NToken)
	}
	if strings.HasPrefix(pattern, "/") || strings.HasSuffix(pattern, "/") {
		return fmt.Errorf("afk branch pattern %q: must not start or end with '/'", pattern)
	}
	if strings.Contains(pattern, "..") {
		return fmt.Errorf("afk branch pattern %q: must not contain '..'", pattern)
	}
	return nil
}

// ValidateManualPrefix checks a manual_branch_prefix: non-empty, ref-safe
// chars only ([A-Za-z0-9._/-] — no '~', no spaces, and no <N>: it is a
// literal), no leading '/', no "..". A trailing '/' is allowed — the
// defaults "lab/" and "wip/" end with one.
func ValidateManualPrefix(prefix string) error {
	if prefix == "" {
		return errors.New("manual branch prefix: must not be empty")
	}
	if c, ok := firstBadRefChar(prefix); ok {
		return fmt.Errorf("manual branch prefix %q: invalid character %q (allowed: A-Za-z0-9._/-)", prefix, string(c))
	}
	if strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("manual branch prefix %q: must not start with '/'", prefix)
	}
	if strings.Contains(prefix, "..") {
		return fmt.Errorf("manual branch prefix %q: must not contain '..'", prefix)
	}
	return nil
}

// ValidatePatternPair validates both parts and rejects an overlapping pair:
// neither may be a prefix of the other's rendered form space, or a branch
// matching one classification would be mistaken for the other (design §4a's
// example: afk pattern "wip/<N>" with manual prefix "wip/").
func ValidatePatternPair(afkPattern, manualPrefix string) error {
	if err := ValidatePattern(afkPattern); err != nil {
		return err
	}
	if err := ValidateManualPrefix(manualPrefix); err != nil {
		return err
	}
	if patternsOverlap(afkPattern, manualPrefix) {
		return fmt.Errorf("afk branch pattern %q and manual branch prefix %q overlap: a branch could match both", afkPattern, manualPrefix)
	}
	return nil
}

// patternsOverlap reports whether some rendered afk branch (prefix + N +
// suffix, N ≥ 1 without leading zeros) starts with manualPrefix — the one
// condition under which a branch would classify as both afk and manual.
func patternsOverlap(afkPattern, manualPrefix string) bool {
	prefix, suffix := PatternPrefix(afkPattern), patternSuffix(afkPattern)
	if strings.HasPrefix(prefix, manualPrefix) {
		return true // every rendered afk branch starts with manualPrefix
	}
	if !strings.HasPrefix(manualPrefix, prefix) {
		return false
	}
	// manualPrefix = prefix + rest; overlap iff rest is a prefix of some
	// "<digits><suffix>" rendering.
	rest := manualPrefix[len(prefix):]
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	digits, tail := rest[:i], rest[i:]
	if digits == "" || digits[0] == '0' {
		return false // no valid N is empty or has a leading zero
	}
	if tail == "" {
		return true // rest is a prefix of some valid N's digits
	}
	// rest = digits + tail. N may consume the whole digit run (j ==
	// len(digits), remainder = tail) or, when the afk suffix itself starts
	// with digits, only a leading part of it: N = rest[:j], and the branch
	// prefix+N+suffix starts with manualPrefix iff suffix starts with
	// rest[j:]. Every rest[:j] is a valid N — it starts with digits[0],
	// which is already known to be nonzero.
	for j := 1; j <= len(digits); j++ {
		if strings.HasPrefix(suffix, rest[j:]) {
			return true
		}
	}
	return false
}

// RenderBranch renders the pattern with issue number n (n ≥ 1), e.g.
// ("afk/<N>", 7) → "afk/7".
func RenderBranch(pattern string, n int) string {
	return strings.Replace(pattern, NToken, strconv.Itoa(n), 1)
}

// ParseBranch is the strict inverse of RenderBranch: it extracts the issue
// number from ref and accepts only when re-rendering the pattern with that
// number reproduces ref exactly. Issue numbers start at 1 (v0
// parseAFKLabel rejects afk-0; mirrored here), and leading zeros are
// rejected by the exact-equality check. Any non-matching ref is simply not
// a claim — never an error.
func ParseBranch(pattern, ref string) (int, bool) {
	prefix, suffix, ok := strings.Cut(pattern, NToken)
	if !ok {
		return 0, false
	}
	if len(ref) < len(prefix)+len(suffix) ||
		!strings.HasPrefix(ref, prefix) || !strings.HasSuffix(ref, suffix) {
		return 0, false
	}
	n, err := strconv.Atoi(ref[len(prefix) : len(ref)-len(suffix)])
	if err != nil || n < 1 {
		return 0, false
	}
	if RenderBranch(pattern, n) != ref {
		return 0, false
	}
	return n, true
}

// PatternPrefix returns the pattern up to <N> — the literal prefix used to
// enumerate claims with `for-each-ref "refs/heads/<prefix>*"`.
func PatternPrefix(pattern string) string {
	prefix, _, _ := strings.Cut(pattern, NToken)
	return prefix
}

// patternSuffix returns the pattern after <N>.
func patternSuffix(pattern string) string {
	_, suffix, _ := strings.Cut(pattern, NToken)
	return suffix
}

// firstBadRefChar returns the first byte of s outside the ref-safe set
// [A-Za-z0-9._/-].
func firstBadRefChar(s string) (byte, bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '.', c == '_', c == '/', c == '-':
		default:
			return c, true
		}
	}
	return 0, false
}
