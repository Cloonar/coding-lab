package tracker

import "testing"

// TestParseVerdict pins ADR-0048's grammar: the FIRST line only, an EXACT
// prefix match (no leading-whitespace tolerance) — matching opensWithVerdict-
// Marker's prior unexported behavior in internal/agentapi byte-for-byte, so
// the relocation changed no consumer's semantics.
func TestParseVerdict(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantWord string
		wantOK   bool
	}{
		{"reject", "[autoland] verdict: reject", "reject", true},
		{"pass", "[autoland] verdict: pass", "pass", true},
		{"fix-done", "[autoland] verdict: fix-done", "fix-done", true},
		{"unknown word", "[autoland] verdict: anything-at-all", "anything-at-all", true},
		{"prefix only", "[autoland] verdict:", "", true},
		{"mid-body quote", "the lander posted: [autoland] verdict: reject", "", false},
		{"line 2 marker", "quoting what I saw:\n[autoland] verdict: pass", "", false},
		{"leading whitespace", "   [autoland] verdict: pass", "", false},
		{"empty body", "", "", false},
		{"CRLF first line", "[autoland] verdict: pass\r\n\r\nfindings below", "pass", true},
		{"findings below a real marker", "[autoland] verdict: reject\n\nneeds tests before this lands", "reject", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			word, ok := ParseVerdict(tt.body)
			if word != tt.wantWord || ok != tt.wantOK {
				t.Errorf("ParseVerdict(%q) = (%q, %v), want (%q, %v)", tt.body, word, ok, tt.wantWord, tt.wantOK)
			}
		})
	}
}
