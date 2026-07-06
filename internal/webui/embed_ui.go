//go:build ui

package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// dist is copied from web/dist by `make build-ui` (and the nix package's
// preBuild) — go:embed cannot reach outside the package dir. Building with
// `-tags ui` before that copy fails loudly ("no matching files"), by design.
//
//go:embed all:dist
var distFS embed.FS

// Handler serves the embedded SPA with the standard fallback: real files as
// themselves, every unknown non-API path as index.html (client-side router).
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("webui: embedded dist missing: " + err.Error())
	}
	fileServer := http.FileServerFS(sub)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			// SPA fallback: unknown paths get the shell; the router takes over.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
