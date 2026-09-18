package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stevelittlefish/AudioSlopServer/internal/config"
)

// cfg with the three real services plus one un-paged one, to exercise both the
// bespoke-page and generic-fallback branches.
func testMux() *http.ServeMux {
	mux := http.NewServeMux()
	Register(mux, &config.Config{Services: map[string]config.Service{
		"demucs":      {},
		"stableaudio": {},
		"acestep":     {},
		"yue":         {},
		"whisper":     {}, // configured but has no bespoke page -> generic fallback
	}})
	return mux
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestPerServicePages(t *testing.T) {
	mux := testMux()
	cases := map[string]string{
		"/test/demucs":      "Separate stems",
		"/test/stableaudio": "Generate audio",
		"/test/acestep":     "Generate a song",
		"/test/yue":         "YuE2-3B", // both acestep+yue say "Generate a song"; this marker is yue-only
	}
	for path, marker := range cases {
		rec := get(t, mux, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200", path, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, marker) {
			t.Errorf("%s: body missing bespoke marker %q", path, marker)
		}
	}
}

func TestUnknownServiceIs404(t *testing.T) {
	if rec := get(t, testMux(), "/test/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown service: got %d, want 404", rec.Code)
	}
}

func TestConfiguredServiceWithoutPageFallsBackToGeneric(t *testing.T) {
	rec := get(t, testMux(), "/test/whisper")
	if rec.Code != http.StatusOK {
		t.Fatalf("whisper: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "generic") {
		t.Errorf("whisper: expected the generic fallback page")
	}
}

func TestSharedAssetsServed(t *testing.T) {
	mux := testMux()
	for path, ct := range map[string]string{
		"/assets/test.css":       "text/css",
		"/assets/test-common.js": "text/javascript",
	} {
		rec := get(t, mux, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200", path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, ct) {
			t.Errorf("%s: content-type %q, want prefix %q", path, got, ct)
		}
	}
}

func TestRootRedirectsToAdmin(t *testing.T) {
	rec := get(t, testMux(), "/")
	if rec.Code != http.StatusFound {
		t.Fatalf("/: got %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/admin" {
		t.Errorf("/: redirect to %q, want /admin", loc)
	}
}
