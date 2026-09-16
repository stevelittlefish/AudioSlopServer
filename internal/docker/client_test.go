package docker

import (
	"context"
	"os"
	"testing"
	"time"
)

// testImage is small and already local on the dev box. It just needs to exist
// and be able to run `sleep`; it does no real work.
const testImage = "debian:bookworm-slim"

// TestContainerLifecycle drives the whole create/start/inspect/stop/remove cycle
// against the real daemon. It needs a Docker socket and the test image; it skips
// (rather than fails) when neither is available, so `go test ./...` on a CI box
// without Docker stays green.
func TestContainerLifecycle(t *testing.T) {
	if _, err := os.Stat(DefaultSocket); err != nil {
		t.Skipf("no docker socket at %s; skipping integration test", DefaultSocket)
	}

	c, err := New(DefaultSocket)
	if err != nil {
		t.Skipf("cannot reach docker daemon: %v", err)
	}
	t.Logf("negotiated API version %s", c.APIVersion())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if ok, err := c.ImageExists(ctx, testImage); err != nil {
		t.Fatalf("ImageExists: %v", err)
	} else if !ok {
		t.Skipf("test image %s not present locally; skipping", testImage)
	}

	const name = "ass-docker-selftest"
	// Belt and braces: remove any leftover from a previous crashed run.
	_ = c.Remove(ctx, name, true)

	// A container that just sits there. No GPU requested (this box has none).
	spec := RunSpec{
		Name:   name,
		Image:  testImage,
		Cmd:    []string{"sleep", "300"}, // just sit there until we stop it
		Port:   39917,                    // a real (unused-ish) port to publish
		Labels: map[string]string{"ass.selftest": "true"},
	}
	if _, err := c.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { _ = c.Remove(context.Background(), name, true) }()

	// Created but not started.
	st, err := c.Inspect(ctx, name)
	if err != nil {
		t.Fatalf("Inspect after create: %v", err)
	}
	if !st.Exists || st.Running {
		t.Fatalf("after create: want exists && !running, got %+v", st)
	}

	if err := c.Start(ctx, name); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st, err = c.Inspect(ctx, name)
	if err != nil {
		t.Fatalf("Inspect after start: %v", err)
	}
	if !st.Running {
		t.Fatalf("after start: want running, got %+v", st)
	}

	// Starting again must be harmless.
	if err := c.Start(ctx, name); err != nil {
		t.Fatalf("second Start should be a no-op, got: %v", err)
	}

	if err := c.Stop(ctx, name, 2); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st, err = c.Inspect(ctx, name)
	if err != nil {
		t.Fatalf("Inspect after stop: %v", err)
	}
	if st.Running {
		t.Fatalf("after stop: want !running, got %+v", st)
	}

	if err := c.Remove(ctx, name, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	st, err = c.Inspect(ctx, name)
	if err != nil {
		t.Fatalf("Inspect after remove: %v", err)
	}
	if st.Exists {
		t.Fatalf("after remove: want !exists, got %+v", st)
	}
}
