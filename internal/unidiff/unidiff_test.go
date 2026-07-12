package unidiff

import (
	"strings"
	"testing"
)

// The exact-output cases: for a line diff, the whole rendered string is the
// contract (a chip renderer parses it back), so we pin it byte-for-byte rather
// than probing fields. Each want was hand-derived against the unified-diff
// convention (diff(1) is not in the build sandbox) — see the per-case notes.
func TestUnified_exact(t *testing.T) {
	cases := []struct {
		name       string
		oldT, newT string
		want       string
	}{
		{
			// Byte-identical → no change.
			name: "equal", oldT: "a\nb\nc\n", newT: "a\nb\nc\n", want: "",
		},
		{
			// Final-newline-only difference is not a line change.
			name: "equal ignoring trailing newline", oldT: "a\nb\n", newT: "a\nb", want: "",
		},
		{
			name: "both empty", oldT: "", newT: "", want: "",
		},
		{
			// Insert "b" between "a" and "c": one line grows both counts.
			name: "pure insertion", oldT: "a\nc\n", newT: "a\nb\nc\n",
			want: "@@ -1,2 +1,3 @@\n a\n+b\n c\n",
		},
		{
			// Delete the middle "b".
			name: "pure deletion", oldT: "a\nb\nc\n", newT: "a\nc\n",
			want: "@@ -1,3 +1,2 @@\n a\n-b\n c\n",
		},
		{
			// Replace "b"→"B": deletion then insertion, one line of context
			// each side (all the file affords).
			name: "single-line replacement", oldT: "a\nb\nc\n", newT: "a\nB\nc\n",
			want: "@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n",
		},
		{
			// Empty old side → the -0,0 insertion convention.
			name: "empty old all-added", oldT: "", newT: "a\nb\nc\n",
			want: "@@ -0,0 +1,3 @@\n+a\n+b\n+c\n",
		},
		{
			// Empty new side → the +0,0 deletion convention.
			name: "empty new all-deleted", oldT: "a\nb\nc\n", newT: "",
			want: "@@ -1,3 +0,0 @@\n-a\n-b\n-c\n",
		},
		{
			// No trailing newline on either input: identical to the newline-
			// terminated form (a\n is a terminator, not a line).
			name: "no trailing newline", oldT: "a\nb", newT: "a\nB",
			want: "@@ -1,2 +1,2 @@\n a\n-b\n+B\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Unified(tc.oldT, tc.newT); got != tc.want {
				t.Errorf("Unified(%q, %q) =\n%q\nwant\n%q", tc.oldT, tc.newT, got, tc.want)
			}
		})
	}
}

// A final-newline difference is symmetric — neither direction reports a change.
func TestUnified_trailingNewlineSymmetric(t *testing.T) {
	if got := Unified("a\nb", "a\nb\n"); got != "" {
		t.Errorf("adding a trailing newline reported a change: %q", got)
	}
}

// Two changes separated by more than 2*context unchanged lines split into two
// hunks so the untouched middle is not printed.
func TestUnified_multiHunk(t *testing.T) {
	// 12 lines; replace line 2 and line 11, leaving 8 unchanged lines (3..10)
	// between them (> 2*context = 6).
	oldLines := make([]string, 12)
	newLines := make([]string, 12)
	for i := range oldLines {
		oldLines[i] = itoa(i + 1)
		newLines[i] = oldLines[i]
	}
	newLines[1] = "2x"
	newLines[10] = "11x"
	got := Unified(join(oldLines), join(newLines))
	if n := strings.Count(got, "@@ -"); n != 2 {
		t.Fatalf("hunks = %d; want 2 (changes far apart)\n%s", n, got)
	}
	// Neither hunk should carry the untouched middle lines.
	if strings.Contains(got, "\n 6\n") {
		t.Errorf("far-apart middle line leaked into a hunk:\n%s", got)
	}
}

// Two changes separated by fewer than 2*context unchanged lines share their
// context and collapse into one hunk.
func TestUnified_adjacentMerge(t *testing.T) {
	// 7 lines; replace line 2 and line 6, leaving 3 unchanged lines (3..5)
	// between them (<= 2*context = 6) → merge.
	oldLines := make([]string, 7)
	newLines := make([]string, 7)
	for i := range oldLines {
		oldLines[i] = itoa(i + 1)
		newLines[i] = oldLines[i]
	}
	newLines[1] = "2x"
	newLines[5] = "6x"
	got := Unified(join(oldLines), join(newLines))
	if n := strings.Count(got, "@@ -"); n != 1 {
		t.Fatalf("hunks = %d; want 1 (changes merge)\n%s", n, got)
	}
	// The single hunk must span both changes.
	if !strings.Contains(got, "-2\n") || !strings.Contains(got, "+2x\n") ||
		!strings.Contains(got, "-6\n") || !strings.Contains(got, "+6x\n") {
		t.Errorf("merged hunk missing one of the two changes:\n%s", got)
	}
}

// The size guard: above either threshold Unified skips the LCS and emits one
// whole-file hunk (all deletions, then all insertions, no context lines).
func TestUnified_guardFallback(t *testing.T) {
	t.Run("line count", func(t *testing.T) {
		// 3000 + 3000 lines > maxLines (5000).
		oldT := strings.Repeat("a\n", 3000)
		newT := strings.Repeat("b\n", 3000)
		got := Unified(oldT, newT)
		if !strings.HasPrefix(got, "@@ -1,3000 +1,3000 @@\n") {
			t.Fatalf("header = %q; want the whole-file header", firstLine(got))
		}
		if n := strings.Count(got, "@@ -"); n != 1 {
			t.Errorf("hunks = %d; want exactly 1 whole-file hunk", n)
		}
		// Whole-file output carries no context lines.
		if strings.Contains(got, "\n a") || strings.Contains(got, "\n b") {
			t.Error("whole-file fallback emitted context lines")
		}
		// All deletions precede all insertions.
		if strings.LastIndex(got, "\n-a") > strings.Index(got, "\n+b") {
			t.Error("whole-file fallback interleaved -/+ lines")
		}
	})
	t.Run("byte size", func(t *testing.T) {
		// Two lines only, but their combined bytes exceed maxBytes (512 KB).
		oldT := strings.Repeat("x", 300*1024)
		newT := strings.Repeat("y", 300*1024)
		got := Unified(oldT, newT)
		if !strings.HasPrefix(got, "@@ -1,1 +1,1 @@\n-") {
			t.Fatalf("header/first line = %q; want the whole-file 1,1 shape", firstLine(got))
		}
		if !strings.Contains(got, "\n+") {
			t.Error("byte-guard fallback dropped the insertion side")
		}
	})
}

// --- tiny stdlib-free helpers (the package imports nothing but strings/fmt) --

func join(lines []string) string {
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
