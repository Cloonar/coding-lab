package provider

// Content-hash pins (issue #175). The hash is a wire identity the SPA and the
// tailer's backpatch detection both key on, so its format, stability, and
// change-sensitivity are contract — and its determinism rests entirely on the
// Message struct tree staying map-free (encoding/json emits struct fields in
// declaration order but sorts map keys only per-marshal of THAT map; a map
// anywhere in the tree would still marshal deterministically today, yet the
// tree is pinned map-free so the canonical-JSON reasoning stays trivially
// checkable — see messageTreeIsMapFree).

import (
	"reflect"
	"testing"
)

func hashFixtureMessage() Message {
	return Message{Seq: 2, Kind: MessageTool, Time: "2026-07-18T10:00:00Z",
		Tool: &ToolInfo{Name: "Bash", Title: "go test", Input: "go test ./...", Status: "running",
			View: &ToolView{Kind: ToolViewCommand, Command: "go test ./..."}}}
}

// HashMessages stamps a 16-hex value, identical for identical content and
// regardless of any pre-set ContentHash (idempotent re-stamping), and any
// rendered change — here the tool status flip plus output landing — changes it.
func TestHashMessages_stableAndChangeSensitive(t *testing.T) {
	a := []Message{hashFixtureMessage()}
	HashMessages(a)
	h := a[0].ContentHash
	if len(h) != 16 {
		t.Fatalf("ContentHash = %q; want 16 hex chars", h)
	}
	for _, c := range h {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Fatalf("ContentHash = %q; want lowercase hex only", h)
		}
	}
	// Re-stamping the already-hashed message yields the same value: the hash
	// covers the content with ContentHash cleared, never itself.
	HashMessages(a)
	if a[0].ContentHash != h {
		t.Errorf("re-stamp changed the hash: %q → %q", h, a[0].ContentHash)
	}
	// A fresh identical message hashes identically.
	b := []Message{hashFixtureMessage()}
	HashMessages(b)
	if b[0].ContentHash != h {
		t.Errorf("identical content hashed differently: %q vs %q", b[0].ContentHash, h)
	}
	// The tool result landing (status flip + output) changes it.
	c := []Message{hashFixtureMessage()}
	c[0].Tool.Status = "ok"
	c[0].Tool.Output = "PASS"
	HashMessages(c)
	if c[0].ContentHash == h {
		t.Error("hash unchanged after the tool status flip")
	}
}

// The Message struct tree must stay map-free (issue #175): the content hash's
// canonical-JSON determinism is argued from "structs marshal in declaration
// order, no maps anywhere". This walks the reachable type graph so a future
// field addition that smuggles in a map fails here, at the invariant, instead
// of as an unexplainable hash flake.
func TestMessageTreeHasNoMaps(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
			typ = typ.Elem()
		}
		switch typ.Kind() {
		case reflect.Map:
			t.Errorf("map at %s — the Message tree must stay map-free (content-hash determinism, issue #175)", path)
		case reflect.Struct:
			if seen[typ] {
				return
			}
			seen[typ] = true
			for i := range typ.NumField() {
				f := typ.Field(i)
				walk(f.Type, path+"."+f.Name)
			}
		}
	}
	walk(reflect.TypeOf(Message{}), "Message")
}
