//go:build !ui

// Package webui serves lab's SPA. Two variants selected by the `ui` build
// tag (design §8): this placeholder for untagged builds (API still works, UI
// is a hint page), and embed_ui.go serving the embedded dist for `-tags ui`.
package webui

import "net/http"

const placeholderHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>lab</title>
<style>
body { font-family: system-ui, sans-serif; max-width: 760px; margin: 4rem auto; padding: 0 1rem; }
code { background: #f3f4f6; padding: 0.1em 0.35em; border-radius: 4px; }
@media (prefers-color-scheme: dark) {
  body { background: #111827; color: #e5e7eb; }
  code { background: #1f2937; }
}
</style>
</head>
<body>
<h1>lab</h1>
<p>UI not embedded; use <code>make lab</code> or the nix package.</p>
</body>
</html>
`

// Handler serves the placeholder page for every non-API path.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(placeholderHTML))
	})
}
