package events

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// WriteSSE writes one event in Server-Sent Events framing:
//
//	event: <type>\n
//	data: <json>\n
//	\n
//
// and flushes when flusher is non-nil. A nil Payload is written as `{}` so
// data is always one line of valid JSON (payloads are compact envelopes;
// json.Marshal never emits newlines for them).
func WriteSSE(w io.Writer, flusher http.Flusher, e Event) error {
	data := []byte("{}")
	if e.Payload != nil {
		var err error
		data, err = json.Marshal(e.Payload)
		if err != nil {
			return fmt.Errorf("marshal sse payload for %q: %w", e.Type, err)
		}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data); err != nil {
		return fmt.Errorf("write sse event %q: %w", e.Type, err)
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}
