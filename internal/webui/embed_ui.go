//go:build ui

package webui

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

func init() {
	// .webmanifest is not in Go's builtin mime table and /etc/mime.types is
	// absent on minimal servers (NixOS); without this the PWA manifest would
	// ship as application/octet-stream.
	if err := mime.AddExtensionType(".webmanifest", "application/manifest+json"); err != nil {
		panic("webui: registering .webmanifest mime type: " + err.Error())
	}
}

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
			// Asset-like paths (hashed bundles, anything with an extension)
			// get a real 404, never the shell: answering HTML for a stale
			// /assets/index-<hash>.js would be parsed as a module script
			// (white screen) and cached by the service worker as if it were
			// the asset. Router paths (/repos/repo_123/issues) carry no
			// extension and still fall back.
			if strings.HasPrefix(p, "assets/") || path.Ext(p) != "" {
				http.NotFound(w, r)
				return
			}
			// SPA fallback: unknown paths get the shell; the router takes over.
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
