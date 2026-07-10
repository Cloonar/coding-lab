// Package secrets provides a streaming matcher that detects known secret
// values inside an arbitrary byte stream, recognising each value not only in
// its exact plaintext form but also in the encoded forms a secret is most
// likely to leak through: base64 (all four standard alphabets), hex (both
// cases) and URL percent-encoding.
//
// It is the shared kernel for the per-repo secrets follow-ups — broker output
// redaction, the git pre-push leak guard, tracker-write sanitisation and
// exposure detection (issues #105–#108) — each of which needs the same test:
// "does this byte stream contain a known secret, in any common encoding?".
//
// # Boundary-correctness invariant
//
// Feed may be handed the stream chopped into chunks at arbitrary positions —
// down to one byte at a time — and still reports exactly the same matches, at
// the same absolute [Start, End) offsets, as a single Feed over the whole
// stream. It achieves this by carrying over the last (maxPatternLen-1) bytes
// between calls and reporting a match only when its end offset falls inside
// the newly-fed chunk. An occurrence is therefore reported exactly once, at
// chunking-invariant offsets, no matter where the chunk boundaries land, and
// no occurrence spanning a boundary is lost.
//
// A Matcher is not safe for concurrent use: serialise Feed and Reset calls.
// The compiled patterns are immutable after NewMatcher and may be reused
// across Reset calls (which clear only the streaming state).
package secrets

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
)

// Form names the encoding under which a secret value was recognised.
type Form string

const (
	FormExact      Form = "exact"
	FormBase64     Form = "base64"
	FormHex        Form = "hex"
	FormURLEncoded Form = "urlencoded"
)

// Match reports one detected occurrence of a secret in the stream.
type Match struct {
	Name  string // secret name as given to NewMatcher
	Form  Form
	Start int64 // absolute stream offset of the first byte of the match
	End   int64 // absolute stream offset just past the last byte of the match
}

// pattern is one derived byte sequence to search for, tagged with the secret
// name it belongs to and the form that produced it.
type pattern struct {
	name  string
	form  Form
	bytes []byte
}

// Matcher scans a byte stream for the derived forms of a fixed set of secret
// values. It holds compiled patterns plus a small carry-over window of stream
// state so that matches spanning chunk boundaries are found. It is not safe
// for concurrent use.
type Matcher struct {
	patterns []pattern
	window   int // carry-over size: maxPatternLen-1 (0 when there are no patterns)

	carry     []byte // last up-to-window bytes of the stream fed so far
	streamLen int64  // total bytes fed since construction or the last Reset
}

// NewMatcher builds a matcher for the given secrets (name -> plaintext value).
// Empty values are ignored. Secret names are processed in sorted order so that
// results are deterministic when several matches share an End offset.
func NewMatcher(values map[string]string) *Matcher {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	m := &Matcher{}
	maxLen := 0
	for _, name := range names {
		v := values[name]
		if v == "" {
			continue
		}
		for _, d := range derive(v) {
			m.patterns = append(m.patterns, pattern{name: name, form: d.form, bytes: d.bytes})
			if len(d.bytes) > maxLen {
				maxLen = len(d.bytes)
			}
		}
	}
	if maxLen > 0 {
		m.window = maxLen - 1
	}
	return m
}

// derived is one (form, bytes) pair produced from a secret value.
type derived struct {
	form  Form
	bytes []byte
}

// derive returns the distinct byte patterns for a value, in a fixed
// generation order, deduplicated so that the first form to produce a given
// byte sequence wins.
//
// Precedence order (first match wins the Form label):
//
//	exact       — v
//	base64      — StdEncoding, RawStdEncoding, URLEncoding, RawURLEncoding
//	hex         — lowercase, uppercase
//	urlencoded  — url.QueryEscape, url.PathEscape
//
// So an alphanumeric value, whose URL-encoding equals its plaintext, is
// reported as FormExact; a value whose hex is all digits collapses its two
// hex cases into one; and so on. Deduplication is per-secret: two distinct
// secrets that derive identical patterns each report independently.
func derive(v string) []derived {
	b := []byte(v)
	var out []derived
	seen := make(map[string]struct{})
	add := func(form Form, p []byte) {
		if len(p) == 0 {
			return
		}
		if _, ok := seen[string(p)]; ok {
			return
		}
		seen[string(p)] = struct{}{}
		out = append(out, derived{form: form, bytes: p})
	}

	add(FormExact, b)

	add(FormBase64, []byte(base64.StdEncoding.EncodeToString(b)))
	add(FormBase64, []byte(base64.RawStdEncoding.EncodeToString(b)))
	add(FormBase64, []byte(base64.URLEncoding.EncodeToString(b)))
	add(FormBase64, []byte(base64.RawURLEncoding.EncodeToString(b)))

	lower := hex.EncodeToString(b)
	add(FormHex, []byte(lower))
	add(FormHex, []byte(strings.ToUpper(lower)))

	add(FormURLEncoded, []byte(url.QueryEscape(v)))
	add(FormURLEncoded, []byte(url.PathEscape(v)))

	return out
}

// Feed scans the next chunk. It returns every match whose final byte lies in
// this chunk, including matches spanning previous chunk boundaries, in
// ascending End order. Start and End are absolute stream offsets and both are
// chunking-invariant. Safe to call with empty or 1-byte chunks; an empty
// chunk is a no-op returning nil.
func (m *Matcher) Feed(chunk []byte) []Match {
	if len(chunk) == 0 {
		return nil
	}

	// buf is the carry window followed by the new chunk. Its first byte sits
	// at absolute offset base; any earlier byte could not start a match that
	// ends inside the new chunk, so carry+chunk is all we ever need to scan.
	base := m.streamLen - int64(len(m.carry))
	buf := make([]byte, 0, len(m.carry)+len(chunk))
	buf = append(buf, m.carry...)
	buf = append(buf, chunk...)

	// A match is "new" iff it ends past the pre-Feed stream length, i.e. its
	// last byte lies in the chunk just fed. This is what stops an occurrence
	// that already sat fully inside the carry window being reported twice.
	threshold := m.streamLen

	var matches []Match
	for i := range m.patterns {
		p := &m.patterns[i]
		from := 0
		for {
			idx := bytes.Index(buf[from:], p.bytes)
			if idx < 0 {
				break
			}
			start := from + idx
			absStart := base + int64(start)
			end := absStart + int64(len(p.bytes))
			if end > threshold {
				matches = append(matches, Match{Name: p.name, Form: p.form, Start: absStart, End: end})
			}
			from = start + 1 // advance by one so overlapping occurrences are found
		}
	}
	sort.SliceStable(matches, func(a, b int) bool { return matches[a].End < matches[b].End })

	m.streamLen += int64(len(chunk))
	if len(buf) > m.window {
		m.carry = append(m.carry[:0], buf[len(buf)-m.window:]...)
	} else {
		m.carry = append(m.carry[:0], buf...)
	}
	return matches
}

// Reset clears streaming state (carry-over window and offset) so the matcher
// can scan a new independent stream. The compiled patterns are reusable.
func (m *Matcher) Reset() {
	m.carry = m.carry[:0]
	m.streamLen = 0
}
