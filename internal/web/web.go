// Package web serves ASS's human-facing console: the admin panel (Slice 3,
// Part B) and the per-service test pages (Part C). It's deliberately dumb —
// server-rendered HTML shells with a sprinkle of vanilla JS that talk to the
// same JSON API everything else does (`/v1/backends`, the job endpoints, the
// operator endpoints). No SPA, no framework, no build step (CLAUDE.md rules 1 &
// 4), and no JS on the server: the pages are static assets embedded into the
// binary.
//
// Each backend gets its OWN bespoke test page, because the services genuinely
// don't look alike — demucs takes a file and spits stems, SA3 wants a prompt
// and a duration, ACE-Step wants a caption + lyrics + a task type and (for
// cover/repaint) a source clip. One "generic" form pretending they're the same
// was a lie; now every page speaks its backend's actual dialect. Shared chrome
// (the CSS and the poll/render/submit plumbing) lives in two static files both
// pages pull in, so the per-service pages carry only their form and its build().
package web

import (
	"embed"
	"net/http"

	"github.com/stevelittlefish/AudioSlopServer/internal/config"
)

//go:embed admin.html test.html test-demucs.html test-stableaudio.html test-acestep.html test-yue.html assets/test.css assets/test-common.js assets/logo.svg
var assets embed.FS

// servicePages maps a configured service name to its bespoke test page. A
// service without an entry here falls back to the generic raw-params form
// (test.html) — enough to poke a new backend before it earns a real page.
var servicePages = map[string]string{
	"demucs":      "test-demucs.html",
	"stableaudio": "test-stableaudio.html",
	"acestep":     "test-acestep.html",
	"yue":         "test-yue.html",
}

// Register mounts the console routes onto mux. Called only when the web console
// is enabled (see config.WebEnabled); the operator endpoints it drives are
// registered separately by the api package under the same guard. cfg is used to
// 404 test pages for services that don't exist.
func Register(mux *http.ServeMux, cfg *config.Config) {
	admin := page("admin.html")
	generic := page("test.html")

	// Exact root ("/{$}" matches only "/") sends people to the panel without
	// swallowing every unmatched path into a catch-all.
	mux.Handle("GET /{$}", http.RedirectHandler("/admin", http.StatusFound))
	mux.Handle("GET /admin", admin)

	// Shared page assets (styling + the common runtime). Served from the same
	// embed FS; content types are set explicitly since we don't rely on the
	// http.FileServer sniffer for two well-known types.
	mux.Handle("GET /assets/test.css", asset("assets/test.css", "text/css; charset=utf-8"))
	mux.Handle("GET /assets/test-common.js", asset("assets/test-common.js", "text/javascript; charset=utf-8"))

	// The logo, used both as the header mark and (via <link rel="icon">) the
	// favicon — one SVG, no PNG/ICO conversion step (CLAUDE.md: no build step).
	// /favicon.ico is served too so the browser's automatic probe doesn't 404.
	logo := asset("assets/logo.svg", "image/svg+xml")
	mux.Handle("GET /assets/logo.svg", logo)
	mux.Handle("GET /favicon.ico", logo)

	// Per-service test page. We reject unknown services up front, then serve
	// the bespoke page if one exists or the generic fallback otherwise. The
	// page reads the service from the URL and drives the same job API.
	mux.HandleFunc("GET /test/{service}", func(w http.ResponseWriter, r *http.Request) {
		service := r.PathValue("service")
		if _, ok := cfg.Services[service]; !ok {
			http.Error(w, "unknown service", http.StatusNotFound)
			return
		}
		name, ok := servicePages[service]
		if !ok {
			generic.ServeHTTP(w, r)
			return
		}
		page(name).ServeHTTP(w, r)
	})
}

// page serves one embedded HTML file as a self-contained document.
func page(name string) http.Handler {
	return asset(name, "text/html; charset=utf-8")
}

// asset serves one embedded file with a fixed content type. Missing files are a
// broken build (embedded at compile time), not a bad request, so we panic
// rather than 500 on every hit.
func asset(name, contentType string) http.Handler {
	body, err := assets.ReadFile(name)
	if err != nil {
		panic("web: missing embedded asset " + name + ": " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	})
}
