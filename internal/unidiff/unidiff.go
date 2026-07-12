// Package unidiff computes a small, dependency-free line-based unified diff
// (issue #146). It exists so a provider's chat mapper can turn an Edit-style
// old→new text pair into the `diff` rich view (provider.ToolView) without
// pulling a third-party diff library into the tree, and without shelling out to
// diff(1) (absent on minimal hosts, and a fork per tool call is not worth it).
//
// The output is deliberately partial by design: hunk headers and ' '/'-'/'+'
// prefixed lines with 3 lines of context, but NO ---/+++ file headers — the
// path travels beside the text in provider.ToolView.Path, so repeating it here
// would only be a second source of truth. One intentional deviation from
// diff(1): a hunk's line count is ALWAYS emitted, even when it is 1 (GNU diff
// abbreviates "@@ -1,1 +1,2 @@" to "@@ -1 +1,2 @@"); the always-count form is
// what issue #146 pins and it keeps the header a fixed, trivially-parsed shape.
package unidiff

import (
	"fmt"
	"strings"
)

// context is the number of unchanged lines shown around each change — the
// conventional 3, matching `diff -U3`.
const context = 3

// Guard thresholds: above either, Unified skips the O(m*n) LCS table and emits
// one whole-file hunk instead (see Unified). The line-count bound is the real
// protection against quadratic blow-up on adversarial input; the byte bound
// additionally catches a few enormous lines (e.g. a minified bundle) whose line
// count is small but whose comparison cost is not worth paying line by line.
const (
	maxBytes = 512 * 1024
	maxLines = 5000
)

// Unified computes a line-based unified diff of oldText → newText: hunk headers
// (@@ -l,c +l,c @@) and ' '/'-'/'+' prefixed lines with `context` lines of
// context around each change. It returns "" when the two inputs describe the
// same sequence of lines.
//
// A trailing newline is treated as a line TERMINATOR, not a line: "a\nb\n" and
// "a\nb" both split to ["a","b"], so a difference in the final newline alone is
// not reported (the "\ No newline at end of file" distinction diff(1) draws is
// deliberately dropped — it is noise for a rich edit view). As a consequence
// the empty string is zero lines, not one.
//
// Adjacent changes closer than 2*context unchanged lines share their context
// and merge into a single hunk; changes farther apart split into separate
// hunks so the unchanged middle is not printed.
func Unified(oldText, newText string) string {
	a := splitLines(oldText)
	b := splitLines(newText)

	if equalLines(a, b) {
		return "" // no line-level change (final-newline-only diffs included)
	}
	// Quadratic-DP guard: an adversarial pair could make the LCS table huge, so
	// above the thresholds we degrade to one whole-file hunk — still valid
	// unified output, just without minimal alignment.
	if len(oldText)+len(newText) > maxBytes || len(a)+len(b) > maxLines {
		return wholeFileHunk(a, b)
	}
	return render(diffLines(a, b))
}

// splitLines splits s into lines, dropping the phantom empty element a trailing
// newline produces so "a\n" and "a" are the same one-line sequence. The empty
// string yields no lines.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func equalLines(a, b []string) bool {
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

// dl is one diff line: tag is ' ' (common), '-' (deletion) or '+' (insertion);
// oldN/newN are its 1-based line numbers on the side(s) it occupies (the other
// side's number is 0 and unused).
type dl struct {
	tag  byte
	text string
	oldN int
	newN int
}

// diffLines produces the ' '/'-'/'+' edit script aligning a → b via a standard
// longest-common-subsequence DP. Correctness over cleverness: the O(m*n) table
// is fine because Unified only reaches here below the size guard, and the Edit
// tool strings it is built for are small. The tie-break (deletion before
// insertion on an equal LCS split) yields the conventional "-old then +new"
// ordering within a replacement.
func diffLines(a, b []string) []dl {
	m, n := len(a), len(b)
	// lcs[i][j] = length of the LCS of a[i:] and b[j:].
	lcs := make([][]int, m+1)
	for i := range lcs {
		lcs[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	ops := make([]dl, 0, m+n)
	i, j := 0, 0
	oldN, newN := 1, 1
	for i < m && j < n {
		switch {
		case a[i] == b[j]:
			ops = append(ops, dl{' ', a[i], oldN, newN})
			i, j, oldN, newN = i+1, j+1, oldN+1, newN+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, dl{'-', a[i], oldN, 0})
			i, oldN = i+1, oldN+1
		default:
			ops = append(ops, dl{'+', b[j], 0, newN})
			j, newN = j+1, newN+1
		}
	}
	for ; i < m; i, oldN = i+1, oldN+1 {
		ops = append(ops, dl{'-', a[i], oldN, 0})
	}
	for ; j < n; j, newN = j+1, newN+1 {
		ops = append(ops, dl{'+', b[j], 0, newN})
	}
	return ops
}

// render groups the edit script into hunks (leading/trailing context, adjacent
// changes merged) and emits the unified-diff text.
func render(ops []dl) string {
	var sb strings.Builder
	n := len(ops)
	for i := 0; i < n; {
		if ops[i].tag == ' ' {
			i++
			continue
		}
		// [changeStart,changeEnd) spans one hunk's changes: a maximal run of
		// non-context ops, extended across any gap of <= 2*context context ops
		// that is followed by another change (so their context windows merge).
		changeStart := i
		changeEnd := i
		for {
			for changeEnd < n && ops[changeEnd].tag != ' ' {
				changeEnd++
			}
			g := changeEnd
			for g < n && ops[g].tag == ' ' {
				g++
			}
			if g < n && g-changeEnd <= 2*context {
				changeEnd = g
				continue
			}
			break
		}
		hs := changeStart - context
		if hs < 0 {
			hs = 0
		}
		he := changeEnd + context
		if he > n {
			he = n
		}
		writeHunk(&sb, ops[hs:he])
		i = he
	}
	return sb.String()
}

// writeHunk emits one hunk: the @@ header computed from the slice's line
// numbers, then every op verbatim under its tag. A zero-count side uses the
// preceding line number (0 at the top of the file), the standard unified-diff
// convention — e.g. "@@ -0,0 +1,3 @@" for a pure insertion at the top.
func writeHunk(sb *strings.Builder, hunk []dl) {
	oldStart, oldCount := 0, 0
	newStart, newCount := 0, 0
	for _, d := range hunk {
		if d.tag == ' ' || d.tag == '-' {
			if oldCount == 0 {
				oldStart = d.oldN
			}
			oldCount++
		}
		if d.tag == ' ' || d.tag == '+' {
			if newCount == 0 {
				newStart = d.newN
			}
			newCount++
		}
	}
	fmt.Fprintf(sb, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
	for _, d := range hunk {
		sb.WriteByte(d.tag)
		sb.WriteString(d.text)
		sb.WriteByte('\n')
	}
}

// wholeFileHunk is the size-guard fallback: one hunk deleting every old line
// then adding every new line. Still valid unified output. A zero-length side
// uses start 0 (the empty-file convention).
func wholeFileHunk(a, b []string) string {
	oldStart, newStart := 1, 1
	if len(a) == 0 {
		oldStart = 0
	}
	if len(b) == 0 {
		newStart = 0
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", oldStart, len(a), newStart, len(b))
	for _, ln := range a {
		sb.WriteByte('-')
		sb.WriteString(ln)
		sb.WriteByte('\n')
	}
	for _, ln := range b {
		sb.WriteByte('+')
		sb.WriteString(ln)
		sb.WriteByte('\n')
	}
	return sb.String()
}
