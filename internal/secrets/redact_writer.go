package secrets

import (
	"errors"
	"io"
	"sort"
)

// errUsedAfterFlush is returned by Write and Flush once Flush has run: a
// RedactingWriter is a one-shot stream filter, and touching it after the stream has
// been finalised is a programmer error.
var errUsedAfterFlush = errors.New("secrets: RedactingWriter used after Flush")

// RedactingWriter is an io.Writer that forwards a byte stream to an underlying
// writer with every occurrence of a known secret — in any form its Matcher
// recognises — replaced by the literal token "[REDACTED:NAME]", where NAME is
// the secret's name as given to NewMatcher. Bytes not covered by any match
// pass through byte-identical. It is the broker-output-redaction consumer
// (issue #105) of the matcher kernel shared by issues #105–#108.
//
// # Why the hold-back window is sufficient
//
// A secret may arrive split across Write calls at any position, so after each
// Write the RedactingWriter must decide which pending bytes can no longer be part of
// a match. Every match not yet reported ends past the current stream length
// L, and no pattern is longer than window+1 bytes (window = maxPatternLen-1,
// the Matcher's carry-over size), so every future match starts at offset
// L-window or later. A byte at an offset below L-window can therefore never
// be covered by a match that has not already been reported: the RedactingWriter
// holds back exactly the trailing window bytes of un-redacted stream
// positions and emits the rest. With no patterns at all (window == 0) it
// degenerates to a pure passthrough.
//
// The same rule releases queued tokens: a token replacing [Start, End) is
// emittable once End <= L-window, because every future match starts at
// >= L-window >= End and so cannot overlap the token's region.
//
// # Overlapping matches
//
// No byte covered by any reported match may ever reach the destination. Each
// Feed's matches are processed sorted by Start ascending, then End
// descending, then Name ascending, against a redaction cursor that spans Feed
// calls. A match starting at or past the cursor becomes a fresh token:
// plaintext up to Start stays, the token stands in for [Start, End), and the
// cursor moves to End. A match straddling the cursor (Start < cursor < End)
// consumes — deletes, without emitting a second token — every pending
// plaintext byte in [Start, End); tokens already queued inside that region
// stay queued, so an occurrence swallowed by a wider late-arriving match
// keeps the token (and the name) of whichever match was reported first:
// safety over fidelity. A match with End <= cursor is only ever reported in
// the same Feed as, and sorted after, a match that already consumed its whole
// region (a later Feed's matches all end past the fed stream, hence past the
// cursor), so it is skipped.
//
// The cursor must span Feed calls because an overlap can arrive later: with
// secret "aa", the stream "aaa" fed one byte at a time reports [0,2) and then
// [1,3) in different Feeds, and the output must be exactly one token with no
// stray "a".
//
// # Why Flush cannot leak
//
// Flush emits everything still pending — queued tokens and held-back
// plaintext — verbatim, in stream order. Feed reports every match whose final
// byte has been fed, and Write folds each report into the pending state
// before returning; once the stream ends there is no unreported match left,
// so every pending plaintext byte is provably covered by no match.
//
// # Write contract and errors
//
// On success Write returns (len(p), nil) even though the byte count actually
// forwarded to dst differs (held-back tail, token substitution): the RedactingWriter
// has accepted, and will account for, every byte of p, which is what the
// io.Writer contract describes. If the underlying writer returns an error,
// Write returns (0, err) and the RedactingWriter is poisoned — every subsequent
// Write and Flush returns that same error — because an unknown suffix of the
// decided output was lost and silently resuming could reorder the stream.
//
// Like the Matcher it owns, a RedactingWriter is not safe for concurrent use: use
// one RedactingWriter per stream.
type RedactingWriter struct {
	dst io.Writer
	m   *Matcher

	// pend is the undecided suffix of the output, in stream order: plaintext
	// and token segments at strictly increasing offsets. Consumed-but-not-
	// tokenised regions appear as gaps. After the last token segment there is
	// at most one plaintext segment, and it always ends at the total number
	// of bytes fed.
	pend []segment

	// cursor is the redaction cursor: the stream offset just past the
	// furthest match region consumed so far. It spans Feed calls.
	cursor int64

	err     error // first underlying-writer error; poisons the RedactingWriter
	flushed bool
}

// segment is one pending run of output: either raw plaintext bytes or a
// redaction token standing in for the stream region [start, end).
type segment struct {
	token bool
	name  string // secret name (token segments only)
	start int64  // absolute stream offset of the first covered byte
	end   int64  // absolute stream offset just past the last covered byte
	data  []byte // the covered bytes (plaintext segments only)
}

// NewRedactingWriter returns a RedactingWriter forwarding redacted output to dst. The
// RedactingWriter owns m from this point on: m must be freshly constructed or Reset,
// and nothing else may Feed or Reset it for the RedactingWriter's lifetime.
func NewRedactingWriter(dst io.Writer, m *Matcher) *RedactingWriter {
	return &RedactingWriter{dst: dst, m: m}
}

// Write feeds the next chunk of the stream, forwarding whatever became safe
// to emit. On success it returns (len(p), nil) — see the type comment for why
// that differs from the forwarded byte count. Once the underlying writer has
// failed it returns (0, err) with the poisoning error, and after Flush it
// returns errUsedAfterFlush.
func (r *RedactingWriter) Write(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.flushed {
		return 0, errUsedAfterFlush
	}
	if len(p) == 0 {
		return 0, nil
	}

	matches := r.m.Feed(p)
	r.appendPlaintext(p)
	r.apply(matches)
	if err := r.emit(r.m.streamLen - int64(r.m.window)); err != nil {
		r.err = err
		return 0, err
	}
	return len(p), nil
}

// Flush emits everything still pending — queued tokens and the held-back
// plaintext tail, in stream order — and marks the stream finished. Call it
// exactly once, when the stream ends: any Write or Flush afterwards returns
// an error. On a poisoned RedactingWriter, Flush returns the poisoning error and
// writes nothing.
func (r *RedactingWriter) Flush() error {
	if r.err != nil {
		return r.err
	}
	if r.flushed {
		return errUsedAfterFlush
	}
	r.flushed = true
	if err := r.emit(r.m.streamLen); err != nil {
		r.err = err
		return err
	}
	return nil
}

// appendPlaintext queues the just-fed chunk as pending plaintext, merging it
// into a trailing plaintext segment when there is one. The bytes are copied:
// the caller may reuse p after Write returns.
func (r *RedactingWriter) appendPlaintext(p []byte) {
	end := r.m.streamLen // Feed has already run, so this includes p
	if k := len(r.pend); k > 0 && !r.pend[k-1].token {
		s := &r.pend[k-1]
		s.data = append(s.data, p...)
		s.end = end
		return
	}
	r.pend = append(r.pend, segment{
		start: end - int64(len(p)),
		end:   end,
		data:  append([]byte(nil), p...),
	})
}

// apply folds one Feed's reported matches into the pending segments, in
// Start-ascending, End-descending, Name-ascending order, so that at equal
// starts the widest match places the token and the rest nest inside it.
func (r *RedactingWriter) apply(matches []Match) {
	if len(matches) == 0 {
		return
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		if a.End != b.End {
			return a.End > b.End
		}
		return a.Name < b.Name
	})
	for _, mt := range matches {
		switch {
		case mt.End <= r.cursor:
			// Nested inside a region consumed earlier in this same Feed:
			// its bytes are already deleted or tokenised.
		case mt.Start < r.cursor:
			// Straddles the consumed region: consume the whole match range,
			// no second token (safety over fidelity).
			r.consume(mt.Start, mt.End)
			r.cursor = mt.End
		default:
			r.redact(mt)
		}
	}
}

// redact replaces the match's region — which lies inside the single trailing
// plaintext segment, because Start is at or past both the cursor and anything
// already emitted — with a token segment.
func (r *RedactingWriter) redact(mt Match) {
	last := len(r.pend) - 1
	s := r.pend[last]
	r.pend = r.pend[:last]
	if mt.Start > s.start {
		r.pend = append(r.pend, segment{start: s.start, end: mt.Start, data: s.data[:mt.Start-s.start]})
	}
	r.pend = append(r.pend, segment{token: true, name: mt.Name, start: mt.Start, end: mt.End})
	if mt.End < s.end {
		r.pend = append(r.pend, segment{start: mt.End, end: s.end, data: s.data[mt.End-s.start:]})
	}
	r.cursor = mt.End
}

// consume deletes every pending plaintext byte in [start, end) without
// emitting anything in its place. Queued tokens inside the range are kept.
// The hold-back rule guarantees no byte of the range was emitted already:
// every match starts at or past the previous stream length minus the window,
// which is at or past everything emitted so far.
func (r *RedactingWriter) consume(start, end int64) {
	out := make([]segment, 0, len(r.pend)+1)
	for _, s := range r.pend {
		if s.token || s.end <= start || s.start >= end {
			out = append(out, s)
			continue
		}
		if s.start < start {
			out = append(out, segment{start: s.start, end: start, data: s.data[:start-s.start]})
		}
		if s.end > end {
			out = append(out, segment{start: end, end: s.end, data: s.data[end-s.start:]})
		}
	}
	r.pend = out
}

// emit writes to dst every pending segment lying wholly below the given
// stream offset, plus the leading part of a plaintext segment straddling it.
// A token straddling the limit is held back whole, and emission stops there
// to preserve stream order (everything behind it sits at even higher
// offsets). All emittable output goes to dst in a single Write.
func (r *RedactingWriter) emit(limit int64) error {
	var out []byte
	i := 0
	for i < len(r.pend) {
		s := &r.pend[i]
		if s.token {
			if s.end > limit {
				break
			}
			out = append(out, "[REDACTED:"...)
			out = append(out, s.name...)
			out = append(out, ']')
			i++
			continue
		}
		if s.end <= limit {
			out = append(out, s.data...)
			i++
			continue
		}
		if keep := limit - s.start; keep > 0 {
			out = append(out, s.data[:keep]...)
			s.data = s.data[keep:]
			s.start = limit
		}
		break
	}
	r.pend = r.pend[i:]
	if len(out) == 0 {
		return nil
	}
	_, err := r.dst.Write(out)
	return err
}
