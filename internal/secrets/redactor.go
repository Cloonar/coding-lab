package secrets

import (
	"sort"
	"strings"
)

// redactPattern is one derived byte pattern to search for and mask, tagged
// with the secret name it belongs to. Unlike Matcher's pattern, it carries
// no Form: Redact only ever reports which secret was hit, not how.
type redactPattern struct {
	name string
	text string
}

// Redactor masks every occurrence of a known secret value — in any derived
// form (see derive) — in a string, replacing it with "[REDACTED:NAME]".
//
// A Redactor is immutable after NewRedactor and safe for concurrent use:
// unlike Matcher, which carries streaming state across Feed calls and must
// be serialised, Redact is a pure function of its receiver and the input
// string. Callers may share one Redactor across goroutines without a lock.
type Redactor struct {
	patterns []redactPattern
}

// NewRedactor builds a redactor for the given secrets (name -> plaintext
// value), deriving the same set of forms as NewMatcher (exact, the four
// base64 alphabets, both hex cases, and the two URL-escape variants; see
// derive for the precedence and dedup rules). Empty values are ignored;
// names are processed in sorted order so that construction is deterministic.
//
// Patterns are stored longest-byte-length first (ties broken by name) — see
// Redact for why that order matters.
func NewRedactor(values map[string]string) *Redactor {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	var patterns []redactPattern
	for _, name := range names {
		v := values[name]
		if v == "" {
			continue
		}
		for _, d := range derive(v) {
			patterns = append(patterns, redactPattern{name: name, text: string(d.bytes)})
		}
	}

	sort.SliceStable(patterns, func(i, j int) bool {
		if len(patterns[i].text) != len(patterns[j].text) {
			return len(patterns[i].text) > len(patterns[j].text)
		}
		return patterns[i].name < patterns[j].name
	})

	return &Redactor{patterns: patterns}
}

// Redact masks every occurrence of every known secret value in s, returning
// the masked string and the sorted, de-duplicated names of secrets that were
// hit. When nothing matches, it returns (s, nil) unchanged — including the
// cheap fast path: a single scan over the patterns decides whether any
// occurs at all, and if none does, Redact returns immediately without
// allocating a new string.
//
// Replacement proceeds longest-pattern-first (the order NewRedactor stored
// patterns in). This matters when one secret's derived pattern is a byte
// substring of another's — e.g. secret A's exact value happens to occur
// inside secret B's base64 encoding. Masking B first consumes the whole
// occurrence in one placeholder; only then is A's (now already-consumed)
// substring no longer there to match. Reversing the order would instead
// mask the inner occurrence first, leaving a mangled nest such as
// "...[REDACTED:A]...” sitting inside what should have been a single
// "[REDACTED:B]". Because the placeholder text itself never reproduces a
// secret's derived bytes, this process is idempotent: redacting an
// already-redacted string is a no-op.
func (r *Redactor) Redact(s string) (string, []string) {
	if len(r.patterns) == 0 {
		return s, nil
	}

	// Fast path: if no pattern occurs anywhere in s, return s unchanged
	// without ever calling strings.ReplaceAll (and without allocating).
	any := false
	for i := range r.patterns {
		if strings.Contains(s, r.patterns[i].text) {
			any = true
			break
		}
	}
	if !any {
		return s, nil
	}

	hit := make(map[string]struct{})
	out := s
	for i := range r.patterns {
		p := &r.patterns[i]
		if !strings.Contains(out, p.text) {
			continue
		}
		out = strings.ReplaceAll(out, p.text, "[REDACTED:"+p.name+"]")
		hit[p.name] = struct{}{}
	}
	if len(hit) == 0 {
		return s, nil
	}

	names := make([]string, 0, len(hit))
	for name := range hit {
		names = append(names, name)
	}
	sort.Strings(names)
	return out, names
}
