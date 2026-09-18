package supervisor

import (
	"context"
	"errors"
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

// TestWaitHealthyLive_FailsFastOnDeadContainer: the backend never answers, but
// its "container" reports exited after a couple of probes. WaitHealthyLive must
// return that error promptly — NOT wait out the (here, long) context — because
// the caller holds the arbiter's swap slot while it waits.
func TestWaitHealthyLive_FailsFastOnDeadContainer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // never healthy
	}))
	defer srv.Close()

	var checks int32
	dead := errors.New("container exited during startup")
	alive := func(context.Context) error {
		if atomic.AddInt32(&checks, 1) >= 2 {
			return dead
		}
		return nil
	}

	// Generous context — the point is we return WELL before it, on the dead signal.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	err := WaitHealthyLive(ctx, srv.URL, 20*time.Millisecond, alive)
	if !errors.Is(err, dead) {
		t.Fatalf("expected the dead-container error, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("fail-fast took %v — should have bailed on the dead signal, not waited the ctx", elapsed)
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
