package main

import (
	"net"
	"strings"
	"testing"

	"git.cloonar.com/Cloonar/coding-lab/internal/config"
)

// bindPreviewListeners (issue #106): nothing bound with previews off, the
// preview listener alone without a redirect address, both with one — and a
// failing second bind leaves nothing open behind it.
func TestBindPreviewListeners(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		p, r, err := bindPreviewListeners(config.Config{})
		if err != nil || p != nil || r != nil {
			t.Fatalf("off = (%v, %v, %v), want nothing bound", p, r, err)
		}
	})
	t.Run("preview only", func(t *testing.T) {
		p, r, err := bindPreviewListeners(config.Config{PreviewAddr: "127.0.0.1:0"})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = p.Close() }()
		if p == nil || r != nil {
			t.Fatalf("got (%v, %v), want the preview listener alone", p, r)
		}
	})
	t.Run("both", func(t *testing.T) {
		p, r, err := bindPreviewListeners(config.Config{PreviewAddr: "127.0.0.1:0", PreviewRedirectAddr: "127.0.0.1:0"})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = p.Close(); _ = r.Close() }()
		if p.Addr().String() == r.Addr().String() {
			t.Fatalf("both listeners on %s", p.Addr())
		}
	})
	t.Run("redirect bind fails", func(t *testing.T) {
		taken, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = taken.Close() }()
		// Reserve a free port for the preview listener, then release it, so
		// the test can prove that port is free again after the failure.
		probe, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		previewAddr := probe.Addr().String()
		_ = probe.Close()

		p, r, err := bindPreviewListeners(config.Config{PreviewAddr: previewAddr, PreviewRedirectAddr: taken.Addr().String()})
		if err == nil || p != nil || r != nil {
			t.Fatalf("got (%v, %v, %v), want an error and nothing bound", p, r, err)
		}
		if !strings.Contains(err.Error(), "redirect") {
			t.Errorf("error %q does not name the redirect listener", err)
		}
		again, err := net.Listen("tcp", previewAddr)
		if err != nil {
			t.Fatalf("the preview listener was left open after the redirect bind failed: %v", err)
		}
		_ = again.Close()
	})
}
