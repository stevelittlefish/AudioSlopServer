package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestWaitHealthy_BecomesHealthy: the first couple of probes fail, then the
// backend comes up. WaitHealthy should keep trying and then return nil.
func TestWaitHealthy_BecomesHealthy(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // "still loading weights"
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := WaitHealthy(ctx, srv.URL, 20*time.Millisecond); err != nil {
		t.Fatalf("expected eventual health, got: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got < 3 {
		t.Fatalf("expected at least 3 probes, got %d", got)
	}
}

// TestWaitHealthy_NeverHealthy: the backend never comes up, so WaitHealthy must
// give up when the context expires rather than spinning forever.
func TestWaitHealthy_NeverHealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if err := WaitHealthy(ctx, srv.URL, 20*time.Millisecond); err == nil {
		t.Fatal("expected an error when the backend never becomes healthy")
	}
}
