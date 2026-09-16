package supervisor

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stevelittlefish/AudioSlopServer/internal/config"
	"github.com/stevelittlefish/AudioSlopServer/internal/docker"
)

// mockImage is built by scripts/build-mockbackend.sh. The test skips if it's
// absent rather than failing, so a fresh checkout without the image stays green.
const mockImage = "ass-mockbackend:local"

// TestEnsureUp_BringsMockBackendHealthy runs the real supervisor against the
// real daemon: create the mock backend container, start it, and wait for its
// /health — no GPU involved, which is the whole point.
func TestEnsureUp_BringsMockBackendHealthy(t *testing.T) {
	if _, err := os.Stat(docker.DefaultSocket); err != nil {
		t.Skipf("no docker socket; skipping")
	}
	d, err := docker.New(docker.DefaultSocket)
	if err != nil {
		t.Skipf("cannot reach docker: %v", err)
	}
	ctx := context.Background()
	if ok, err := d.ImageExists(ctx, mockImage); err != nil || !ok {
		t.Skipf("image %s not present (run scripts/build-mockbackend.sh); skipping", mockImage)
	}

	cfg := &config.Config{
		GPU: config.GPU{Enabled: false}, // this box has no GPU; that's the test
		Services: map[string]config.Service{
			"mock": {
				Image: mockImage,
				Port:  18099,
				Verb:  "separate",
				Env:   map[string]string{"JOB_MS": "200"},
				Evict: config.EvictStop,
			},
		},
	}

	sup := New(d, cfg)
	name := cfg.Services["mock"].ContainerName("mock")

	// Clean any leftovers, and clean up after ourselves no matter what.
	_ = d.Remove(ctx, name, true)
	defer func() { _ = d.Remove(context.Background(), name, true) }()

	upCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := sup.EnsureUp(upCtx, "mock"); err != nil {
		t.Fatalf("EnsureUp: %v", err)
	}

	// It claims healthy — verify we can actually talk to it through BaseURL.
	resp, err := http.Get(sup.BaseURL("mock") + "/v1/info")
	if err != nil {
		t.Fatalf("GET /v1/info after EnsureUp: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/info returned %d, want 200", resp.StatusCode)
	}

	// EnsureUp again must be idempotent (already running -> quick, still nil).
	if err := sup.EnsureUp(upCtx, "mock"); err != nil {
		t.Fatalf("second EnsureUp should be a no-op, got: %v", err)
	}

	// And Stop should take it down.
	if err := sup.Stop(ctx, "mock"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st, err := d.Inspect(ctx, name)
	if err != nil {
		t.Fatalf("Inspect after stop: %v", err)
	}
	if st.Running {
		t.Fatalf("after Stop: container still running")
	}
}
