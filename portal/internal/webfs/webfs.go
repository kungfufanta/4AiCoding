// Package webfs embeds the built SPA (portal/web/dist) into the
// portal binary. The committed dist/ holds only a placeholder
// index.html so `go build ./...` works without a web build; the
// Dockerfile overwrites the directory with the real Vite output
// before compiling. S1 mounts the BFF API next to it with the
// same-origin cookie sessions (PORTAL_PLAN 3.1).
package webfs

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves the SPA. Unknown paths fall back to index.html so
// client-side routing keeps working after a hard refresh.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("webfs: embedded dist missing: " + err.Error())
	}
	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Security headers for the dashboard (PORTAL_PLAN 7).
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api") || strings.HasPrefix(r.URL.Path, "/healthz") || strings.HasPrefix(r.URL.Path, "/readyz") {
			http.NotFound(w, r)
			return
		}
		// Real files win; everything else falls back to the SPA shell.
		if _, err := fs.Stat(sub, strings.TrimPrefix(r.URL.Path, "/")); err != nil {
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})
}
