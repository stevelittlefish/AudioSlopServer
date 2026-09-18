package arbiter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stevelittlefish/AudioSlopServer/internal/config"
)

// fakeSup is a Supervisor that talks to httptest servers instead of Docker, and
// records every EnsureUp/Stop so tests can assert the swap actually happened.
type fakeSup struct {
	mu       sync.Mutex
	urls     map[string]string // service -> base URL of its fake backend
	ensured  []string
	stopped  []string
	parkHits map[string]int // service -> count of /park calls its server saw
	unpark   map[string]int // service -> count of /unpark calls
}

func newFakeSup(t *testing.T, services ...string) *fakeSup {
	f := &fakeSup{
		urls:     map[string]string{},
		parkHits: map[string]int{},
		unpark:   map[string]int{},
	}
	for _, svc := range services {
		svc := svc
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
		mux.HandleFunc("/park", func(w http.ResponseWriter, _ *http.Request) {
			f.mu.Lock()
			f.parkHits[svc]++
			f.mu.Unlock()
			w.WriteHeader(200)
		})
		mux.HandleFunc("/unpark", func(w http.ResponseWriter, _ *http.Request) {
			f.mu.Lock()
			f.unpark[svc]++
			f.mu.Unlock()
			w.WriteHeader(200)
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		f.urls[svc] = srv.URL
	}
	return f
}

func (f *fakeSup) EnsureUp(_ context.Context, service string) error {
	f.mu.Lock()
	f.ensured = append(f.ensured, service)
	f.mu.Unlock()
	return nil
}

func (f *fakeSup) Stop(_ context.Context, service string) error {
	f.mu.Lock()
	f.stopped = append(f.stopped, service)
	f.mu.Unlock()
	return nil
}

func (f *fakeSup) BaseURL(service string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.urls[service]
}

func testConfig() *config.Config {
	return &config.Config{
		GPU: config.GPU{MaxResident: 1},
		Services: map[string]config.Service{
			"demucs": {Image: "x", Port: 1, Evict: config.EvictPark},
			"yue":    {Image: "x", Port: 2, Evict: config.EvictStop},
		},
	}
}

// TestSwap is the heart of Slice 2: with one GPU slot, acquiring a different
// backend must evict the incumbent (park or stop per its policy) and promote the
// newcomer — the real evict-one / load-the-other.
func TestSwap(t *testing.T) {
	sup := newFakeSup(t, "demucs", "yue")
	a := New(sup, testConfig())
	ctx := context.Background()

	// 1. Acquire demucs -> cold start, pinned.
	rel1, err := a.Acquire(ctx, "demucs")
	if err != nil {
		t.Fatalf("acquire demucs: %v", err)
	}
	if got := a.Snapshot()["demucs"].Residency; got != "pinned" {
		t.Fatalf("demucs residency = %q, want pinned", got)
	}
	rel1()

	// 2. Acquire yue -> must park demucs (its policy) and pin yue.
	rel2, err := a.Acquire(ctx, "yue")
	if err != nil {
		t.Fatalf("acquire yue: %v", err)
	}
	snap := a.Snapshot()
	if snap["demucs"].Residency != "parked" {
		t.Fatalf("demucs residency = %q, want parked", snap["demucs"].Residency)
	}
	if snap["yue"].Residency != "pinned" {
		t.Fatalf("yue residency = %q, want pinned", snap["yue"].Residency)
	}
	if sup.parkHits["demucs"] != 1 {
		t.Fatalf("demucs /park hits = %d, want 1", sup.parkHits["demucs"])
	}
	rel2()

	// 3. Acquire demucs again -> yue (evict=stop) is stopped, demucs unparked.
	rel3, err := a.Acquire(ctx, "demucs")
	if err != nil {
		t.Fatalf("re-acquire demucs: %v", err)
	}
	snap = a.Snapshot()
	if snap["demucs"].Residency != "pinned" {
		t.Fatalf("demucs residency = %q, want pinned", snap["demucs"].Residency)
	}
	if snap["yue"].Residency != "stopped" {
		t.Fatalf("yue residency = %q, want stopped", snap["yue"].Residency)
	}
	if sup.unpark["demucs"] != 1 {
		t.Fatalf("demucs /unpark hits = %d, want 1 (fast path, not cold start)", sup.unpark["demucs"])
	}
	got := false
	for _, s := range sup.stopped {
		if s == "yue" {
			got = true
		}
	}
	if !got {
		t.Fatalf("yue was never stopped; stopped=%v", sup.stopped)
	}
	rel3()
}

// TestVRAMBudget is the point of the whole exercise: with a memory budget (not a
// pin count) multiple small backends co-reside, and a big newcomer evicts as many
// LRU residents as it takes to fit — not just one.
func TestVRAMBudget(t *testing.T) {
	sup := newFakeSup(t, "a", "b", "big")
	cfg := &config.Config{
		GPU: config.GPU{VRAMBudgetMB: 10000}, // MB gate, no pin cap
		Services: map[string]config.Service{
			"a":   {Image: "x", Port: 1, Evict: config.EvictPark, VRAMPinnedMB: 3000, VRAMParkedMB: 500},
			"b":   {Image: "x", Port: 2, Evict: config.EvictPark, VRAMPinnedMB: 3000, VRAMParkedMB: 500},
			"big": {Image: "x", Port: 3, Evict: config.EvictStop, VRAMPinnedMB: 8000},
		},
	}
	a := New(sup, cfg)
	ctx := context.Background()

	// a and b both fit at once (3000 + 3000 <= 10000): no eviction.
	r1, _ := a.Acquire(ctx, "a")
	r1()
	r2, _ := a.Acquire(ctx, "b")
	r2()
	snap := a.Snapshot()
	if snap["a"].Residency != "pinned" || snap["b"].Residency != "pinned" {
		t.Fatalf("a and b should co-reside; got a=%s b=%s", snap["a"].Residency, snap["b"].Residency)
	}

	// big (8000) doesn't fit beside 6000 committed. Both a and b are LRU + unleased,
	// and evicting only one (freeing 2500 after its park tax) still leaves 3500+8000
	// over budget — so BOTH must be parked before big fits.
	r3, err := a.Acquire(ctx, "big")
	if err != nil {
		t.Fatalf("acquire big: %v", err)
	}
	snap = a.Snapshot()
	if snap["big"].Residency != "pinned" {
		t.Fatalf("big residency = %q, want pinned", snap["big"].Residency)
	}
	if snap["a"].Residency != "parked" || snap["b"].Residency != "parked" {
		t.Fatalf("both a and b should be parked to make room; got a=%s b=%s", snap["a"].Residency, snap["b"].Residency)
	}
	r3()
}

// TestLeaseBlocksEviction proves a running job protects its backend: a swap that
// would evict a leased backend must wait until the lease is released.
func TestLeaseBlocksEviction(t *testing.T) {
	sup := newFakeSup(t, "demucs", "yue")
	a := New(sup, testConfig())
	ctx := context.Background()

	// Hold demucs with an un-released lease.
	relDemucs, err := a.Acquire(ctx, "demucs")
	if err != nil {
		t.Fatalf("acquire demucs: %v", err)
	}

	// Try to acquire yue in the background — it must block on demucs's lease.
	acquired := make(chan struct{})
	go func() {
		rel, err := a.Acquire(ctx, "yue")
		if err != nil {
			t.Errorf("acquire yue: %v", err)
			return
		}
		rel()
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("yue acquired while demucs still leased — eviction should have waited")
	case <-time.After(150 * time.Millisecond):
		// Good: still blocked.
	}

	// Release demucs; now yue's swap should proceed.
	relDemucs()
	select {
	case <-acquired:
		// Good.
	case <-time.After(2 * time.Second):
		t.Fatal("yue never acquired after demucs released")
	}
}

// TestSamePinnedIsFast confirms re-acquiring the already-pinned backend takes a
// lease without any eviction or restart churn.
func TestSamePinnedIsFast(t *testing.T) {
	sup := newFakeSup(t, "demucs", "yue")
	a := New(sup, testConfig())
	ctx := context.Background()

	r1, err := a.Acquire(ctx, "demucs")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	r2, err := a.Acquire(ctx, "demucs")
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if n := len(sup.ensured); n != 1 {
		t.Fatalf("EnsureUp called %d times, want 1 (second acquire is free)", n)
	}
	if l := a.Snapshot()["demucs"].Leases; l != 2 {
		t.Fatalf("demucs leases = %d, want 2", l)
	}
	r1()
	r2()
	if l := a.Snapshot()["demucs"].Leases; l != 0 {
		t.Fatalf("demucs leases = %d after release, want 0", l)
	}
}
