package secrets

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// --- helpers ---------------------------------------------------------------

// feedSplit feeds stream to the matcher as two chunks split at pos.
func feedSplit(m *Matcher, stream []byte, pos int) []Match {
	var out []Match
	out = append(out, m.Feed(stream[:pos])...)
	out = append(out, m.Feed(stream[pos:])...)
	return out
}

// feedEvery feeds stream one byte per Feed call.
func feedEvery(m *Matcher, stream []byte) []Match {
	var out []Match
	for i := 0; i < len(stream); i++ {
		out = append(out, m.Feed(stream[i:i+1])...)
	}
	return out
}

func b64std(s string) string    { return base64.StdEncoding.EncodeToString([]byte(s)) }
func b64rawstd(s string) string { return base64.RawStdEncoding.EncodeToString([]byte(s)) }
func b64url(s string) string    { return base64.URLEncoding.EncodeToString([]byte(s)) }
func b64rawurl(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func hexLower(s string) string  { return hex.EncodeToString([]byte(s)) }
func hexUpper(s string) string  { return strings.ToUpper(hex.EncodeToString([]byte(s))) }

// key renders a match into a stable comparable string.
func key(m Match) string { return fmt.Sprintf("%s|%s|%d", m.Name, m.Form, m.End) }

func keys(ms []Match) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = key(m)
	}
	sort.Strings(out)
	return out
}

func mustSingle(t *testing.T, got []Match, wantName string, wantForm Form, wantEnd int64) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("want exactly 1 match, got %d: %+v", len(got), got)
	}
	if got[0].Name != wantName || got[0].Form != wantForm || got[0].End != wantEnd {
		t.Fatalf("match = %+v, want {Name:%q Form:%q End:%d}", got[0], wantName, wantForm, wantEnd)
	}
}

func mustContain(t *testing.T, got []Match, want Match) {
	t.Helper()
	for _, m := range got {
		if m == want {
			return
		}
	}
	t.Fatalf("want a match %+v, got %+v", want, got)
}

// --- 1. each form detected in a single Feed --------------------------------

func TestFeed_EachForm(t *testing.T) {
	// A value with characters that genuinely change under every encoding.
	const v = "p@ss w/rd+1=2!Zk"
	m := NewMatcher(map[string]string{"api": v})

	const prefix = "log[ "
	const suffix = " ]end"

	// Forms that do not nest inside one another: exactly one match expected.
	single := []struct {
		name string
		enc  string
		form Form
	}{
		{"exact", v, FormExact},
		{"hex.lower", hexLower(v), FormHex},
		{"hex.upper", hexUpper(v), FormHex},
		{"url.Query", url.QueryEscape(v), FormURLEncoded},
		{"url.Path", url.PathEscape(v), FormURLEncoded},
	}
	for _, tc := range single {
		t.Run(tc.name, func(t *testing.T) {
			m.Reset()
			stream := []byte(prefix + tc.enc + suffix)
			got := m.Feed(stream)
			mustSingle(t, got, "api", tc.form, int64(len(prefix)+len(tc.enc)))
		})
	}

	// All four base64 encodings are recognised as FormBase64. A padded
	// encoding legitimately also contains its unpadded form as a prefix
	// substring, so here we only assert the encoding's own match is present.
	b64 := []struct {
		name string
		enc  string
	}{
		{"base64.Std", b64std(v)},
		{"base64.RawStd", b64rawstd(v)},
		{"base64.URL", b64url(v)},
		{"base64.RawURL", b64rawurl(v)},
	}
	for _, tc := range b64 {
		t.Run(tc.name, func(t *testing.T) {
			m.Reset()
			stream := []byte(prefix + tc.enc + suffix)
			got := m.Feed(stream)
			mustContain(t, got, Match{Name: "api", Form: FormBase64, End: int64(len(prefix) + len(tc.enc))})
		})
	}
}

// A value containing space, +, / and = changes under QueryEscape, proving the
// urlencoded form is exercised distinctly from the exact form.
func TestFeed_URLEncodedActuallyChanges(t *testing.T) {
	const v = "a b+c/d=e"
	if url.QueryEscape(v) == v {
		t.Fatalf("test value does not change under QueryEscape")
	}
	m := NewMatcher(map[string]string{"tok": v})
	enc := url.QueryEscape(v)
	got := m.Feed([]byte("x=" + enc + "&y"))
	mustSingle(t, got, "tok", FormURLEncoded, int64(2+len(enc)))
}

// --- 2. chunk-boundary sweep -----------------------------------------------

// The boundary-correctness invariant: for every derived form embedded in
// noise, chopping the stream at any byte position (and one byte at a time)
// yields exactly the same match set as a single Feed over the whole stream.
func TestFeed_BoundarySweep(t *testing.T) {
	const v = "p@ss w/rd+1=2!Zk"
	m := NewMatcher(map[string]string{"api": v})

	const prefix = "GATEWAY log:: "
	const suffix = " ;; done"

	probes := []string{
		v,
		b64std(v), b64rawstd(v), b64url(v), b64rawurl(v),
		hexLower(v), hexUpper(v),
		url.QueryEscape(v), url.PathEscape(v),
	}

	for _, probe := range probes {
		stream := []byte(prefix + probe + suffix)

		// Baseline: the whole stream in one Feed.
		m.Reset()
		baseline := keys(m.Feed(stream))
		if len(baseline) == 0 {
			t.Fatalf("probe %q produced no baseline match", probe)
		}

		// Split at every byte position, including the degenerate ends.
		for pos := 0; pos <= len(stream); pos++ {
			m.Reset()
			got := keys(feedSplit(m, stream, pos))
			if !equalKeys(got, baseline) {
				t.Fatalf("probe %q split@%d: got %v, want %v", probe, pos, got, baseline)
			}
		}

		// One byte per Feed over the whole stream.
		m.Reset()
		if got := keys(feedEvery(m, stream)); !equalKeys(got, baseline) {
			t.Fatalf("probe %q 1-byte sweep: got %v, want %v", probe, got, baseline)
		}
	}
}

func equalKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- 3. no double-report at a chunk boundary -------------------------------

func TestFeed_NoDoubleReportAtBoundary(t *testing.T) {
	const v = "SECRETVALUE"
	m := NewMatcher(map[string]string{"s": v})

	const prefix = "abc"
	stream := []byte(prefix + v + "xyz")
	boundary := len(prefix) + len(v) // split exactly where the match ends

	first := m.Feed(stream[:boundary])
	mustSingle(t, first, "s", FormExact, int64(boundary))

	second := m.Feed(stream[boundary:])
	if len(second) != 0 {
		t.Fatalf("second Feed re-reported the boundary match: %+v", second)
	}
}

// --- 4. multiple occurrences and interleaved secrets -----------------------

func TestFeed_TwoOccurrencesOneChunk(t *testing.T) {
	const v = "TOKEN123!" // '!' makes it non-alphanumeric so exact != urlencoded
	m := NewMatcher(map[string]string{"s": v})
	stream := []byte("[" + v + "]-[" + v + "]")
	got := m.Feed(stream)
	if len(got) != 2 {
		t.Fatalf("want 2 matches, got %d: %+v", len(got), got)
	}
	want1 := int64(1 + len(v))
	want2 := int64(1 + len(v) + 3 + len(v))
	if got[0].End != want1 || got[1].End != want2 {
		t.Fatalf("ends = %d,%d want %d,%d", got[0].End, got[1].End, want1, want2)
	}
	if got[0].End >= got[1].End {
		t.Fatalf("matches not in ascending End order: %+v", got)
	}
}

func TestFeed_InterleavedSecrets(t *testing.T) {
	const a = "AAA-secret!"
	const b = "BBB-other?"
	m := NewMatcher(map[string]string{"alpha": a, "beta": b})
	stream := []byte("<" + a + "|" + b + "|" + a + ">")
	got := m.Feed(stream)
	if len(got) != 3 {
		t.Fatalf("want 3 matches, got %d: %+v", len(got), got)
	}
	wantNames := []string{"alpha", "beta", "alpha"}
	for i, name := range wantNames {
		if got[i].Name != name {
			t.Fatalf("match[%d].Name = %q, want %q (all: %+v)", i, got[i].Name, name, got)
		}
	}
}

// --- 5. dedupe / precedence ------------------------------------------------

func TestFeed_AlphanumericIsExactOnly(t *testing.T) {
	const v = "abc123XYZ" // url-encodes to itself -> FormExact wins
	if url.QueryEscape(v) != v || url.PathEscape(v) != v {
		t.Fatalf("value should be URL-encoding invariant")
	}
	m := NewMatcher(map[string]string{"k": v})

	// The plaintext (== its own url-encoding) reports once, as FormExact.
	got := m.Feed([]byte("--" + v + "--"))
	mustSingle(t, got, "k", FormExact, int64(2+len(v)))
	for _, mm := range got {
		if mm.Form == FormURLEncoded {
			t.Fatalf("alphanumeric value reported as urlencoded: %+v", got)
		}
	}
}

// --- 6. Reset ---------------------------------------------------------------

func TestReset_ClearsCarryAndOffset(t *testing.T) {
	const v = "SECRET"
	m := NewMatcher(map[string]string{"s": v})

	// Prime the carry window with the head of a would-be match, then Reset.
	m.Feed([]byte("SEC"))
	m.Reset()

	// The discarded "SEC" must not combine with a new "RET" to match.
	if got := m.Feed([]byte("RETxyz")); len(got) != 0 {
		t.Fatalf("Reset did not clear carry: got %+v", got)
	}

	// Offsets restart at 0 after Reset.
	m.Reset()
	got := m.Feed([]byte("SECRET"))
	mustSingle(t, got, "s", FormExact, int64(len(v)))
}

// --- 7. empty value / empty map / empty chunk ------------------------------

func TestEmptyValueIgnored(t *testing.T) {
	m := NewMatcher(map[string]string{"blank": "", "real": "hit"})
	got := m.Feed([]byte("some hit here"))
	// The empty value must not match anywhere; only "real" is found.
	if len(got) != 1 || got[0].Name != "real" || got[0].Form != FormExact {
		t.Fatalf("empty value leaked matches: %+v", got)
	}
}

func TestEmptyMapNeverMatches(t *testing.T) {
	m := NewMatcher(nil)
	if got := m.Feed([]byte("anything at all")); len(got) != 0 {
		t.Fatalf("empty-map matcher matched: %+v", got)
	}
}

func TestEmptyChunkIsNoOp(t *testing.T) {
	m := NewMatcher(map[string]string{"s": "xy"})
	if got := m.Feed(nil); got != nil {
		t.Fatalf("empty Feed returned %+v, want nil", got)
	}
	if got := m.Feed([]byte{}); got != nil {
		t.Fatalf("empty Feed returned %+v, want nil", got)
	}
	// Empty Feeds interleaved with real ones must not disturb offsets.
	m.Feed([]byte("x"))
	m.Feed(nil)
	got := m.Feed([]byte("y"))
	mustSingle(t, got, "s", FormExact, 2)
}

// --- 8. base64 of value embedded in a larger base64 document ---------------

func TestFeed_Base64WithinBase64Doc(t *testing.T) {
	const v = "correct horse battery staple"
	m := NewMatcher(map[string]string{"pw": v})

	inner := b64std(v)
	// Surround with other base64-alphabet noise; inner is a plain substring.
	const lead = "QUJDREVG"
	doc := lead + inner + "MTIzNDU2"
	got := m.Feed([]byte(doc))

	// The embedded (padded) encoding must be found as a FormBase64 match
	// ending just past it.
	mustContain(t, got, Match{Name: "pw", Form: FormBase64, End: int64(len(lead) + len(inner))})
}
