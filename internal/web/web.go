// Package web serves ASS's human-facing console: the admin panel (Slice 3,
// Part B) and, later, the per-service test pages (Part C). It's deliberately
// dumb — server-rendered HTML shells with a sprinkle of vanilla JS that talk to
// the same JSON API everything else does (`/v1/backends`, the operator
// endpoints). No SPA, no framework, no build step (CLAUDE.md rules 1 & 4), and
// no JS on the server: the pages are static assets embedded into the binary.
package web

import (
	"embed"
	"net/http"

	"github.com/stevelittlefish/AudioSlopServer/internal/config"
)

//go:embed admin.html test.html
var assets embed.FS

// Register mounts the console routes onto mux. Called only when the web console
// is enabled (see config.WebEnabled); the operator endpoints it drives are
// registered separately by the api package under the same guard. cfg is used to
// 404 test pages for services that don't exist.
func Register(mux *http.ServeMux, cfg *config.Config) {
	admin := page("admin.html")
	test := page("test.html")
	// Exact root ("/{$}" matches only "/") sends people to the panel without
	// swallowing every unmatched path into a catch-all.
	mux.Handle("GET /{$}", http.RedirectHandler("/admin", http.StatusFound))
	mux.Handle("GET /admin", admin)
	// Per-service test page. The page itself reads the service from the URL and
	// drives the same job API; here we just reject unknown services up front.
	mux.HandleFunc("GET /test/{service}", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := cfg.Services[r.PathValue("service")]; !ok {
			http.Error(w, "unknown service", http.StatusNotFound)
			return
		}
		test.ServeHTTP(w, r)
	})
}

// page serves one embedded HTML file as a self-contained document.
func page(name string) http.Handler {
	body, err := assets.ReadFile(name)
	if err != nil {
		// Embedded at build time — if this fails the binary is broken, not the
		// request, so fail loud rather than 500 on every hit.
		panic("web: missing embedded asset " + name + ": " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	})
}
