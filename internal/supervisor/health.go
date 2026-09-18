package supervisor

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// WaitHealthy polls url until it answers 2xx or the context is done. Backends
// take their sweet time loading weights, so this is the difference between
// "container started" and "actually ready to take work". Returns nil once
// healthy, or an error describing how it gave up.
func WaitHealthy(ctx context.Context, url string, interval time.Duration) error {
	return WaitHealthyLive(ctx, url, interval, nil)
}

// WaitHealthyLive is WaitHealthy plus a liveness check run before each probe. If
// alive returns an error, we stop waiting IMMEDIATELY and return it — the point
// is a crash-looping backend (its container exited) must not be polled until the
// context expires. That matters because the caller holds the arbiter's single
// swap slot the whole time it waits here: a backend that will never come up would
// otherwise wedge every OTHER backend for the full job timeout (~30 min). alive
// may be nil, which is just WaitHealthy's old behaviour.
func WaitHealthyLive(ctx context.Context, url string, interval time.Duration, alive func(context.Context) error) error {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	// A short per-probe timeout so a hung backend doesn't eat the whole interval.
	client := &http.Client{Timeout: 2 * time.Second}

	var lastErr error
	for attempt := 1; ; attempt++ {
		// Dead-backend short-circuit: if the container has exited, there's nothing
		// left to become healthy — fail now and free the swap slot.
		if alive != nil {
			if err := alive(ctx); err != nil {
				return err
			}
		}

		// Try immediately, then on each tick — no reason to wait before the first.
		if err := probe(ctx, client, url); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("backend at %s never became healthy (%d attempts): last error: %v; ctx: %w",
				url, attempt, lastErr, ctx.Err())
		case <-time.After(interval):
			// go around again
		}
	}
}

func probe(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health returned %d", resp.StatusCode)
	}
	return nil
}
