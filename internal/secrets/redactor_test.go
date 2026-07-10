package secrets

import (
	"bytes"
	"errors"
	"net/url"
	"strings"
	"testing"
)

// --- helpers ---------------------------------------------------------------

// redactChunks runs input through a fresh Redactor built from values, writing
// it in chunks whose sizes cycle through sizes (a 0 exercises the empty
// Write), and returns the full output after Flush. It also pins the Write
// contract: every successful Write reports exactly len(p).
func redactChunks(t *testing.T, values map[string]string, input string, sizes []int) string {
	t.Helper()
	var buf bytes.Buffer
	r := NewRedactor(&buf, NewMatcher(values))
	data := []byte(input)
	for i, k := 0, 0; i < len(data); k++ {
		n := sizes[k%len(sizes)]
		if n > len(data)-i {
			n = len(data) - i
		}
		wn, err := r.Write(data[i : i+n])
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if wn != n {
			t.Fatalf("Write returned %d, want %d", wn, n)
		}
		i += n
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return buf.String()
}

// redactOnce writes the whole input in a single Write, then flushes.
func redactOnce(t *testing.T, values map[string]string, input string) string {
	t.Helper()
	size := len(input)
	if size == 0 {
		size = 1
	}
	return redactChunks(t, values, input, []int{size})
}

// failWriter fails every Write with a fixed error, counting the attempts.
type failWriter struct {
	calls int
	err   error
}

func (w *failWriter) Write(p []byte) (int, error) {
	w.calls++
	return 0, w.err
}

// --- 1. each form redacted ---------------------------------------------------

// Every derived form of a secret, printed inside surrounding text, is replaced
// by the literal token; the surrounding bytes come through byte-identical. The
// value changes under every encoding (its encodings contain '+', '=' and '%'),
// so each probe genuinely exercises a non-plaintext pattern.
func TestRedactor_EachForm(t *testing.T) {
	const v = "p@ss w/rd+1=2!Zk"
	values := map[string]string{"api": v}

	probes := []struct {
		name string
		enc  string
	}{
		{"exact", v},
		{"base64.Std", b64std(v)},
		{"base64.RawStd", b64rawstd(v)},
		{"base64.URL", b64url(v)},
		{"base64.RawURL", b64rawurl(v)},
		{"hex.lower", hexLower(v)},
		{"hex.upper", hexUpper(v)},
		{"url.Query", url.QueryEscape(v)},
		{"url.Path", url.PathEscape(v)},
	}
	for _, tc := range probes {
		t.Run(tc.name, func(t *testing.T) {
			input := "log[ " + tc.enc + " ]end"
			const want = "log[ [REDACTED:api] ]end"
			if got := redactOnce(t, values, input); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// The exact token format is part of the contract: "[REDACTED:" + NAME + "]".
func TestRedactor_TokenFormat(t *testing.T) {
	values := map[string]string{"DB_PASSWORD": "hunter2!"}
	got := redactOnce(t, values, "pw=hunter2! done")
	const want = "pw=[REDACTED:DB_PASSWORD] done"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// --- 2. chunking invariance (the rolling-buffer criterion) -------------------

// For a fixed input with secrets at the start of the stream, in the middle,
// back-to-back and at the very end — in plaintext and encoded forms — writing
// it in one Write, one byte per Write, or at deterministic uneven split
// points produces the identical final output after Flush.
func TestRedactor_ChunkingInvariance(t *testing.T) {
	const v = "p@ss w/rd+1=2!Zk"
	values := map[string]string{"api": v}

	input := v + " head " + hexLower(v) + " mid " + v + v + " tail " + b64std(v)
	const want = "[REDACTED:api] head [REDACTED:api] mid [REDACTED:api][REDACTED:api] tail [REDACTED:api]"

	if got := redactOnce(t, values, input); got != want {
		t.Fatalf("one Write: got %q, want %q", got, want)
	}

	// Two chunks, split at every byte position, including the degenerate ends.
	for pos := 0; pos <= len(input); pos++ {
		if got := redactChunks(t, values, input, []int{pos, len(input)}); got != want {
			t.Fatalf("split@%d: got %q, want %q", pos, got, want)
		}
	}

	// One byte per Write.
	if got := redactChunks(t, values, input, []int{1}); got != want {
		t.Fatalf("1-byte writes: got %q, want %q", got, want)
	}

	// Deterministic uneven chunk sizes; the 0 sneaks in mid-stream empty Writes.
	for _, sizes := range [][]int{{3, 1, 7, 2, 5}, {2, 0, 9}, {13}, {1, 31}} {
		if got := redactChunks(t, values, input, sizes); got != want {
			t.Fatalf("sizes %v: got %q, want %q", sizes, got, want)
		}
	}
}

// --- 3. overlapping and nested matches ---------------------------------------

// Overlapping occurrences of one secret collapse to a single token: with
// secret "aa", the stream "aaa" reports [0,2) and [1,3), and the output is
// exactly one token — even fed byte-at-a-time, where the second, straddling
// match arrives in a later Feed and the cursor must span Writes.
func TestRedactor_OverlappingOccurrences(t *testing.T) {
	values := map[string]string{"X": "aa"}

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"aaa", "aaa", "[REDACTED:X]"},
		{"aaaa", "aaaa", "[REDACTED:X]"},
		{"embedded", "zzaaazz", "zz[REDACTED:X]zz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactOnce(t, values, tc.input); got != tc.want {
				t.Fatalf("one Write: got %q, want %q", got, tc.want)
			}
			if got := redactChunks(t, values, tc.input, []int{1}); got != tc.want {
				t.Fatalf("1-byte writes: got %q, want %q", got, tc.want)
			}
			for pos := 0; pos <= len(tc.input); pos++ {
				if got := redactChunks(t, values, tc.input, []int{pos, len(tc.input)}); got != tc.want {
					t.Fatalf("split@%d: got %q, want %q", pos, got, tc.want)
				}
			}
		})
	}
}

// Two secrets where one value is a substring of the other. In a single Write
// the wider match places the token and the nested one is skipped. Under other
// chunkings the inner match can be reported first and keep its token (safety
// over fidelity: the name may differ), but no covered byte may ever leak.
func TestRedactor_NestedSecretValues(t *testing.T) {
	values := map[string]string{
		"outer": "wrapVALUEwrap",
		"inner": "VALUE",
	}
	const input = "a wrapVALUEwrap b VALUE c"

	// One Write: matches sort widest-first at the overlap, so the outer
	// occurrence is labelled outer and the standalone inner one inner.
	const want = "a [REDACTED:outer] b [REDACTED:inner] c"
	if got := redactOnce(t, values, input); got != want {
		t.Fatalf("one Write: got %q, want %q", got, want)
	}

	// Every chunking: no byte of either secret value reaches the output —
	// in particular no fragment of the outer value around an inner token.
	check := func(label, got string) {
		t.Helper()
		if strings.Contains(got, "VALUE") || strings.Contains(got, "wrap") {
			t.Fatalf("%s: secret bytes leaked: %q", label, got)
		}
		if !strings.HasPrefix(got, "a [REDACTED:") || !strings.HasSuffix(got, " c") || !strings.Contains(got, " b [REDACTED:inner] c") {
			t.Fatalf("%s: mangled output: %q", label, got)
		}
	}
	check("1-byte writes", redactChunks(t, values, input, []int{1}))
	for pos := 0; pos <= len(input); pos++ {
		check("split", redactChunks(t, values, input, []int{pos, len(input)}))
	}
}

// --- 4. multiple secrets ------------------------------------------------------

func TestRedactor_MultipleSecrets(t *testing.T) {
	values := map[string]string{
		"alpha": "AAA-secret!",
		"beta":  "BBB-other?",
	}
	input := "<AAA-secret!|BBB-other?|AAA-secret!>"
	const want = "<[REDACTED:alpha]|[REDACTED:beta]|[REDACTED:alpha]>"

	if got := redactOnce(t, values, input); got != want {
		t.Fatalf("one Write: got %q, want %q", got, want)
	}
	if got := redactChunks(t, values, input, []int{1}); got != want {
		t.Fatalf("1-byte writes: got %q, want %q", got, want)
	}
}

// --- 5. passthrough and the held-back tail ------------------------------------

// Non-secret input — including NULs and bytes above 0x7f — passes through
// byte-identical. An input shorter than the hold-back window is emitted
// entirely by Flush.
func TestRedactor_BinaryPassthrough(t *testing.T) {
	values := map[string]string{"s": "SUPERSECRETVALUE"} // hex form makes window 31

	t.Run("shorter than window", func(t *testing.T) {
		input := []byte{0x00, 0xff, 0x80, 'a', 0x00, 0x7f, 0xfe}
		var buf bytes.Buffer
		r := NewRedactor(&buf, NewMatcher(values))
		if _, err := r.Write(input); err != nil {
			t.Fatalf("Write: %v", err)
		}
		// Everything sits inside the hold-back window until Flush.
		if buf.Len() != 0 {
			t.Fatalf("bytes emitted before Flush: %q", buf.Bytes())
		}
		if err := r.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if !bytes.Equal(buf.Bytes(), input) {
			t.Fatalf("got %q, want %q", buf.Bytes(), input)
		}
	})

	t.Run("longer than window", func(t *testing.T) {
		input := make([]byte, 300)
		for i := range input {
			input[i] = byte(i * 7) // covers NULs and >0x7f, no ASCII secret run
		}
		got := redactChunks(t, values, string(input), []int{11, 1, 30})
		if got != string(input) {
			t.Fatalf("binary stream not byte-identical:\ngot  %q\nwant %q", got, input)
		}
	})
}

// A secret ending exactly at the end of the stream, with no trailing bytes,
// is still held back at the last Write and must be tokenised by Flush.
func TestRedactor_SecretAtEndOfStream(t *testing.T) {
	const v = "p@ss w/rd+1=2!Zk"
	values := map[string]string{"api": v}
	input := "prefix:" + v
	const want = "prefix:[REDACTED:api]"

	if got := redactOnce(t, values, input); got != want {
		t.Fatalf("one Write: got %q, want %q", got, want)
	}
	if got := redactChunks(t, values, input, []int{1}); got != want {
		t.Fatalf("1-byte writes: got %q, want %q", got, want)
	}
}

// --- 6. degenerate inputs -----------------------------------------------------

func TestRedactor_EmptyWriteIsNoOp(t *testing.T) {
	values := map[string]string{"s": "SECRET!"}
	var buf bytes.Buffer
	r := NewRedactor(&buf, NewMatcher(values))

	for _, p := range [][]byte{nil, {}} {
		n, err := r.Write(p)
		if n != 0 || err != nil {
			t.Fatalf("empty Write = (%d, %v), want (0, nil)", n, err)
		}
	}

	// Empty Writes interleaved with real ones must not disturb redaction.
	for _, chunk := range []string{"a SEC", "", "RET! b"} {
		if _, err := r.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got, want := buf.String(), "a [REDACTED:s] b"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A matcher with no patterns (nil map, or only empty values) makes the
// Redactor a pure passthrough: window is 0, so nothing is ever held back.
func TestRedactor_NoPatternsPassthrough(t *testing.T) {
	for _, values := range []map[string]string{nil, {"blank": ""}} {
		var buf bytes.Buffer
		r := NewRedactor(&buf, NewMatcher(values))
		const input = "anything at all \x00\xff"
		if _, err := r.Write([]byte(input)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		// No hold-back window: the bytes are through before Flush.
		if buf.String() != input {
			t.Fatalf("passthrough before Flush: got %q, want %q", buf.String(), input)
		}
		if err := r.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if buf.String() != input {
			t.Fatalf("passthrough after Flush: got %q, want %q", buf.String(), input)
		}
	}
}

// --- 7. errors and poisoning ---------------------------------------------------

// An underlying-writer error surfaces from the Write that hit it, and poisons
// the Redactor: every later Write and Flush returns the same error without
// touching the destination again.
func TestRedactor_WriterErrorPoisons(t *testing.T) {
	boom := errors.New("boom")
	values := map[string]string{"s": "zz"} // window 3: a 10-byte Write must emit

	fw := &failWriter{err: boom}
	r := NewRedactor(fw, NewMatcher(values))

	n, err := r.Write([]byte("0123456789"))
	if n != 0 || !errors.Is(err, boom) {
		t.Fatalf("Write = (%d, %v), want (0, boom)", n, err)
	}
	if fw.calls != 1 {
		t.Fatalf("dst.Write called %d times, want 1", fw.calls)
	}

	if n, err := r.Write([]byte("more")); n != 0 || !errors.Is(err, boom) {
		t.Fatalf("poisoned Write = (%d, %v), want (0, boom)", n, err)
	}
	if err := r.Flush(); !errors.Is(err, boom) {
		t.Fatalf("poisoned Flush = %v, want boom", err)
	}
	if fw.calls != 1 {
		t.Fatalf("dst.Write called %d times after poisoning, want still 1", fw.calls)
	}
}

// The same poisoning applies when the failure happens during Flush itself.
func TestRedactor_FlushErrorPoisons(t *testing.T) {
	boom := errors.New("boom")
	values := map[string]string{"s": "zz"}

	fw := &failWriter{err: boom}
	r := NewRedactor(fw, NewMatcher(values))

	// Two bytes stay inside the window, so Write never touches dst.
	if _, err := r.Write([]byte("ab")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if fw.calls != 0 {
		t.Fatalf("dst.Write called during held-back Write")
	}

	if err := r.Flush(); !errors.Is(err, boom) {
		t.Fatalf("Flush = %v, want boom", err)
	}
	if err := r.Flush(); !errors.Is(err, boom) {
		t.Fatalf("second Flush = %v, want boom", err)
	}
}

// Using the Redactor after a successful Flush is a programmer error and is
// reported as one.
func TestRedactor_UseAfterFlush(t *testing.T) {
	var buf bytes.Buffer
	r := NewRedactor(&buf, NewMatcher(map[string]string{"s": "zz"}))
	if _, err := r.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := r.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if n, err := r.Write([]byte("late")); n != 0 || !errors.Is(err, errUsedAfterFlush) {
		t.Fatalf("Write after Flush = (%d, %v), want (0, errUsedAfterFlush)", n, err)
	}
	if err := r.Flush(); !errors.Is(err, errUsedAfterFlush) {
		t.Fatalf("second Flush = %v, want errUsedAfterFlush", err)
	}
	if got := buf.String(); got != "hello" {
		t.Fatalf("output disturbed by post-Flush use: %q", got)
	}
}
