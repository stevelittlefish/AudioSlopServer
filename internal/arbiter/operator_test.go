package arbiter

import (
	"context"
	"errors"
	"testing"
)

// TestOperatorParkStop covers the by-hand residency controls: park a pinned
// backend, stop it, and the idempotent no-ops on both.
func TestOperatorParkStop(t *testing.T) {
	sup := newFakeSup(t, "demucs", "yue")
	a := New(sup, testConfig())
	ctx := context.Background()

	// Bring demucs up.
	rel, err := a.Acquire(ctx, "demucs")
	if err != nil {
		t.Fatalf("acquire demucs: %v", err)
	}
	rel()

	// Park it by hand -> parked, and its /park endpoint was hit.
	if err := a.Park(ctx, "demucs"); err != nil {
		t.Fatalf("park demucs: %v", err)
	}
	if got := a.Snapshot()["demucs"].Residency; got != "parked" {
		t.Fatalf("residency = %q, want parked", got)
	}
	if sup.parkHits["demucs"] != 1 {
		t.Fatalf("park hits = %d, want 1", sup.parkHits["demucs"])
	}

	// Parking again is a no-op (no second /park call).
	if err := a.Park(ctx, "demucs"); err != nil {
		t.Fatalf("re-park demucs: %v", err)
	}
	if sup.parkHits["demucs"] != 1 {
		t.Fatalf("park hits after re-park = %d, want 1", sup.parkHits["demucs"])
	}

	// Stop it -> stopped, container taken down.
	if err := a.Stop(ctx, "demucs"); err != nil {
		t.Fatalf("stop demucs: %v", err)
	}
	if got := a.Snapshot()["demucs"].Residency; got != "stopped" {
		t.Fatalf("residency = %q, want stopped", got)
	}
	if len(sup.stopped) != 1 || sup.stopped[0] != "demucs" {
		t.Fatalf("stopped = %v, want [demucs]", sup.stopped)
	}
}

// TestOperatorUnpark: a parked backend can be brought back onto the card by
// hand, evicting the incumbent if the single slot is taken.
func TestOperatorUnpark(t *testing.T) {
	sup := newFakeSup(t, "demucs", "yue")
	a := New(sup, testConfig())
	ctx := context.Background()

	// demucs pinned, then parked.
	rel, _ := a.Acquire(ctx, "demucs")
	rel()
	if err := a.Park(ctx, "demucs"); err != nil {
		t.Fatalf("park: %v", err)
	}

	// Unpark demucs -> back to pinned via the fast /unpark path.
	if err := a.Unpark(ctx, "demucs"); err != nil {
		t.Fatalf("unpark demucs: %v", err)
	}
	if got := a.Snapshot()["demucs"].Residency; got != "pinned" {
		t.Fatalf("residency = %q, want pinned", got)
	}
	if sup.unpark["demucs"] != 1 {
		t.Fatalf("unpark hits = %d, want 1", sup.unpark["demucs"])
	}

	// Park demucs, pin yue, then unpark demucs -> must evict yue (stop) to make
	// room on the single slot.
	if err := a.Park(ctx, "demucs"); err != nil {
		t.Fatalf("re-park: %v", err)
	}
	rel2, _ := a.Acquire(ctx, "yue")
	rel2()
	if err := a.Unpark(ctx, "demucs"); err != nil {
		t.Fatalf("unpark demucs over yue: %v", err)
	}
	snap := a.Snapshot()
	if snap["demucs"].Residency != "pinned" {
		t.Fatalf("demucs = %q, want pinned", snap["demucs"].Residency)
	}
	if snap["yue"].Residency != "stopped" {
		t.Fatalf("yue = %q, want stopped (evicted)", snap["yue"].Residency)
	}
}

// TestOperatorWarm: the "load it now" button. A stopped backend gets cold-started
// to pinned (no lease left behind), and warming over a full slot evicts the LRU
// idle resident just like a job would.
func TestOperatorWarm(t *testing.T) {
	sup := newFakeSup(t, "demucs", "yue")
	a := New(sup, testConfig())
	ctx := context.Background()

	// Warm demucs from stopped -> pinned via a cold start (EnsureUp, no lease).
	if err := a.Warm(ctx, "demucs"); err != nil {
		t.Fatalf("warm demucs: %v", err)
	}
	snap := a.Snapshot()
	if snap["demucs"].Residency != "pinned" {
		t.Fatalf("demucs = %q, want pinned", snap["demucs"].Residency)
	}
	if snap["demucs"].Leases != 0 {
		t.Fatalf("warm should leave no lease; leases = %d", snap["demucs"].Leases)
	}

	// Warming an already-pinned backend is an idempotent no-op.
	if err := a.Warm(ctx, "demucs"); err != nil {
		t.Fatalf("warm already-pinned: %v", err)
	}

	// Warm yue on a full single slot -> must evict idle demucs (park, its policy).
	if err := a.Warm(ctx, "yue"); err != nil {
		t.Fatalf("warm yue over demucs: %v", err)
	}
	snap = a.Snapshot()
	if snap["yue"].Residency != "pinned" {
		t.Fatalf("yue = %q, want pinned", snap["yue"].Residency)
	}
	if snap["demucs"].Residency != "parked" {
		t.Fatalf("demucs = %q, want parked (evicted to make room)", snap["demucs"].Residency)
	}
}

// TestOperatorLeaseProtection: an operator can't evict a backend with an
// in-flight job — the lease wins, and the action is refused with LeaseHeldError.
func TestOperatorLeaseProtection(t *testing.T) {
	sup := newFakeSup(t, "demucs", "yue")
	a := New(sup, testConfig())
	ctx := context.Background()

	// Hold a lease on demucs (a job in flight).
	rel, err := a.Acquire(ctx, "demucs")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Park and Stop must both be refused while the lease is held.
	var leaseErr *LeaseHeldError
	if err := a.Park(ctx, "demucs"); !errors.As(err, &leaseErr) {
		t.Fatalf("park with lease: err = %v, want LeaseHeldError", err)
	}
	if err := a.Stop(ctx, "demucs"); !errors.As(err, &leaseErr) {
		t.Fatalf("stop with lease: err = %v, want LeaseHeldError", err)
	}
	// Nothing was actually done.
	if got := a.Snapshot()["demucs"].Residency; got != "pinned" {
		t.Fatalf("residency = %q, want pinned (untouched)", got)
	}
	if sup.parkHits["demucs"] != 0 || len(sup.stopped) != 0 {
		t.Fatalf("backend was touched despite lease: park=%d stop=%v", sup.parkHits["demucs"], sup.stopped)
	}

	// Drop the lease; now park succeeds.
	rel()
	if err := a.Park(ctx, "demucs"); err != nil {
		t.Fatalf("park after release: %v", err)
	}
}

// TestOperatorErrors covers the typed-error cases the API maps to status codes.
func TestOperatorErrors(t *testing.T) {
	sup := newFakeSup(t, "demucs", "yue")
	a := New(sup, testConfig())
	ctx := context.Background()

	if err := a.Park(ctx, "nope"); !errors.Is(err, ErrUnknownService) {
		t.Fatalf("park unknown: err = %v, want ErrUnknownService", err)
	}
	// yue's evict policy is stop, so it can't be parked.
	if err := a.Park(ctx, "yue"); !errors.Is(err, ErrParkUnsupported) {
		t.Fatalf("park yue: err = %v, want ErrParkUnsupported", err)
	}
	// demucs isn't resident yet -> can't park.
	if err := a.Park(ctx, "demucs"); !errors.Is(err, ErrNotResident) {
		t.Fatalf("park stopped demucs: err = %v, want ErrNotResident", err)
	}
	// demucs isn't parked -> can't unpark.
	if err := a.Unpark(ctx, "demucs"); !errors.Is(err, ErrNotParked) {
		t.Fatalf("unpark stopped demucs: err = %v, want ErrNotParked", err)
	}
}

// TestUnloadAll: stops every idle resident, skips the busy ones, and reports
// both. Idempotent when nothing is resident.
func TestUnloadAll(t *testing.T) {
	sup := newFakeSup(t, "demucs", "yue")
	cfg := testConfig()
	cfg.GPU.MaxResident = 2 // let both sit resident so there's more to unload
	a := New(sup, cfg)
	ctx := context.Background()

	// Nothing resident -> empty result, no error.
	res, err := a.UnloadAll(ctx)
	if err != nil {
		t.Fatalf("unload-all (empty): %v", err)
	}
	if len(res.Unloaded) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("empty unload = %+v, want nothing", res)
	}

	// Pin both; keep a lease on yue so it must be skipped.
	rel1, _ := a.Acquire(ctx, "demucs")
	rel1()
	relYue, _ := a.Acquire(ctx, "yue")

	res, err = a.UnloadAll(ctx)
	if err != nil {
		t.Fatalf("unload-all: %v", err)
	}
	if len(res.Unloaded) != 1 || res.Unloaded[0] != "demucs" {
		t.Fatalf("unloaded = %v, want [demucs]", res.Unloaded)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Service != "yue" || res.Skipped[0].Leases != 1 {
		t.Fatalf("skipped = %+v, want [{yue 1}]", res.Skipped)
	}
	if got := a.Snapshot()["demucs"].Residency; got != "stopped" {
		t.Fatalf("demucs = %q, want stopped", got)
	}
	if got := a.Snapshot()["yue"].Residency; got != "pinned" {
		t.Fatalf("yue = %q, want pinned (skipped)", got)
	}

	// Release yue and unload again -> now it goes too.
	relYue()
	res, err = a.UnloadAll(ctx)
	if err != nil {
		t.Fatalf("unload-all (2): %v", err)
	}
	if len(res.Unloaded) != 1 || res.Unloaded[0] != "yue" {
		t.Fatalf("unloaded = %v, want [yue]", res.Unloaded)
	}
}
