package secrets

import (
	"net/url"
	"strings"
	"testing"
)

// assertRedact runs Redact and checks the masked output and the exact
// (already-sorted) set of hit names.
func assertRedact(t *testing.T, r *Redactor, in, wantOut string, wantNames []string) {
	t.Helper()
	out, names := r.Redact(in)
	if out != wantOut {
		t.Fatalf("Redact(%q) out = %q, want %q", in, out, wantOut)
	}
	if len(names) != len(wantNames) {
		t.Fatalf("Redact(%q) names = %v, want %v", in, names, wantNames)
	}
	for i := range names {
		if names[i] != wantNames[i] {
			t.Fatalf("Redact(%q) names = %v, want %v", in, names, wantNames)
		}
	}
}

// --- 1. each derived form is masked -----------------------------------------

func TestRedact_EachForm(t *testing.T) {
	// Contains space, @, /, +, =, ! so its urlencoded form genuinely differs
	// from its exact form (see matcher_test.go's TestFeed_URLEncodedActuallyChanges
	// for the same rationale).
	const v = "p@ss w/rd+1=2!Zk"
	if url.QueryEscape(v) == v {
		t.Fatalf("test value does not change under QueryEscape")
	}
	r := NewRedactor(map[string]string{"api": v})

	cases := []struct {
		name string
		enc  string
	}{
		{"exact", v},
		{"base64.std", b64std(v)},
		{"base64.rawurl", b64rawurl(v)},
		{"hex.lower", hexLower(v)},
		{"hex.upper", hexUpper(v)},
		{"url.query", url.QueryEscape(v)},
		{"url.path", url.PathEscape(v)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := "prefix " + tc.enc + " suffix"
			want := "prefix [REDACTED:api] suffix"
			assertRedact(t, r, in, want, []string{"api"})
		})
	}
}

// --- 2. multiple distinct secrets in one string -----------------------------

func TestRedact_MultipleDistinctSecrets(t *testing.T) {
	const a = "AAA-secret!"
	const b = "BBB-other?"
	r := NewRedactor(map[string]string{"beta": b, "alpha": a})

	in := "<" + a + "|" + b + ">"
	want := "<[REDACTED:alpha]|[REDACTED:beta]>"
	assertRedact(t, r, in, want, []string{"alpha", "beta"})
}

// --- 3. multiple occurrences of one secret ----------------------------------

func TestRedact_MultipleOccurrencesOfOneSecret(t *testing.T) {
	const v = "TOKEN123!"
	r := NewRedactor(map[string]string{"s": v})

	in := "[" + v + "]-[" + v + "]"
	want := "[[REDACTED:s]]-[[REDACTED:s]]"
	assertRedact(t, r, in, want, []string{"s"})
}

// --- 4. hit names are sorted and deduplicated -------------------------------

func TestRedact_HitNamesSortedAndDeduped(t *testing.T) {
	r := NewRedactor(map[string]string{
		"zulu":  "zzz-val",
		"alpha": "aaa-val",
		"mike":  "mmm-val",
	})

	// zulu appears twice, alpha and mike once each, in an order that is not
	// already alphabetical, so a naive "order of first hit" would fail this.
	in := "zzz-val ... mmm-val ... zzz-val ... aaa-val"
	_, names := r.Redact(in)
	want := []string{"alpha", "mike", "zulu"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for i := range names {
		if names[i] != want[i] {
			t.Fatalf("names = %v, want %v", names, want)
		}
	}
}

// --- 5. no-hit passthrough ---------------------------------------------------

func TestRedact_NoHitPassthrough(t *testing.T) {
	r := NewRedactor(map[string]string{"s": "SOME-SECRET-VALUE"})
	in := "nothing interesting in this string at all"
	assertRedact(t, r, in, in, nil)
}

// --- 6. text already containing placeholder syntax passes through ----------

func TestRedact_PlaceholderTextPassesThrough(t *testing.T) {
	r := NewRedactor(map[string]string{"FOO": "the-real-secret-value"})
	in := "log line mentions [REDACTED:FOO] but not the real value"
	assertRedact(t, r, in, in, nil)
}

// --- 7. longest-pattern-first: no mangled nest ------------------------------

// bar's exact value contains foo's exact value as a byte substring. Because
// Redact masks the longer pattern first, bar's whole occurrence becomes a
// single clean placeholder; foo's shorter pattern never gets a chance to
// match inside it (its only occurrence in the text was consumed by bar's).
func TestRedact_LongestFirstNoMangledNest(t *testing.T) {
	const foo = "secretvalue"
	const bar = "Xsecretvalue"
	if !strings.Contains(bar, foo) {
		t.Fatalf("test setup: %q does not contain %q", bar, foo)
	}
	r := NewRedactor(map[string]string{"foo": foo, "bar": bar})

	in := "start " + bar + " end"
	want := "start [REDACTED:bar] end"
	assertRedact(t, r, in, want, []string{"bar"})
}

// --- 8. empty values map is the identity function ---------------------------

func TestRedact_EmptyValuesIsIdentity(t *testing.T) {
	r := NewRedactor(nil)
	in := "this looks like it could be SECRET123 but nothing is configured"
	assertRedact(t, r, in, in, nil)

	r2 := NewRedactor(map[string]string{"blank": ""})
	assertRedact(t, r2, in, in, nil)
}

// --- 9. idempotence -----------------------------------------------------------

func TestRedact_Idempotent(t *testing.T) {
	r := NewRedactor(map[string]string{
		"alpha": "AAA-secret!",
		"beta":  "BBB-other?",
	})
	in := "<AAA-secret!|BBB-other?|AAA-secret!>"

	once, names1 := r.Redact(in)
	twice, names2 := r.Redact(once)

	if once != twice {
		t.Fatalf("Redact is not idempotent: first=%q second=%q", once, twice)
	}
	if len(names2) != 0 {
		t.Fatalf("re-redacting an already-redacted string reported hits: %v", names2)
	}
	if len(names1) == 0 {
		t.Fatalf("first pass reported no hits, test is not exercising anything")
	}
}
