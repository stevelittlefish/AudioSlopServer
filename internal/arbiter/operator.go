// Operator-driven residency control: the by-hand counterpart to the automatic
// swapping the arbiter does for jobs. These back the web console's admin panel
// (Slice 3, Part A) — park, unpark, stop a named backend, or unload everything —
// but they're plain HTTP actions, useful with curl long before any UI exists.
//
// The golden rule: operators go THROUGH the arbiter, never around it. Every
// action here respects the same two invariants the job path does:
//
//   - The GPU is the lock. An operator action claims the single swap slot and
//     waits out any in-flight swap, so a human button-press can't race the
//     arbiter mid-eviction.
//   - Leases are sacred. A backend with an in-flight job holds a lease and will
//     NOT be evicted out from under it; the action is refused (LeaseHeldError)
//     rather than yanking results away before harvest. Come back when it drains.
package arbiter

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/stevelittlefish/AudioSlopServer/internal/backend"
	"github.com/stevelittlefish/AudioSlopServer/internal/config"
)

// Sentinel errors so the API layer can map operator failures to HTTP codes
// without string-matching. Wrapped with %w, so use errors.Is / errors.As.
var (
	// ErrUnknownService: no such backend in the config. -> 404
	ErrUnknownService = errors.New("unknown service")
	// ErrParkUnsupported: asked to park a backend whose evict policy isn't
	// "park" (it has no /park endpoint to call). -> 400
	ErrParkUnsupported = errors.New("backend does not support park")
	// ErrNotResident: asked to park a backend that isn't on the GPU. -> 409
	ErrNotResident = errors.New("backend is not resident on the GPU")
	// ErrNotParked: asked to unpark a backend that isn't parked. -> 409
	ErrNotParked = errors.New("backend is not parked")
	// ErrGPUBusy: can't make room (the card is full and every resident is
	// leased). -> 409
	ErrGPUBusy = errors.New("GPU is full and every resident backend is busy")
)

// LeaseHeldError is returned when an operator action would evict a backend that
// has in-flight jobs. It carries how many, so the API can say so. -> 409
type LeaseHeldError struct {
	Service string
	Leases  int
}

func (e *LeaseHeldError) Error() string {
	return fmt.Sprintf("%s has %d in-flight job(s) holding it; not evicting", e.Service, e.Leases)
}

// Park demotes a pinned backend to parked (weights -> CPU RAM, GPU freed),
// keeping its container alive for a fast return. Idempotent: parking an
// already-parked backend is a no-op. Refused if the backend has active leases,
// isn't resident, or its evict policy isn't "park".
func (a *Arbiter) Park(ctx context.Context, service string) error {
	svc, ok := a.cfg.Services[service]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownService, service)
	}
	if svc.Evict != config.EvictPark {
		return fmt.Errorf("%s: %w (evict policy is %q, not \"park\")", service, ErrParkUnsupported, svc.Evict)
	}
	return a.operate(ctx, func() (func() error, error) {
		st := a.states[service]
		if st == nil || st.res == resStopped {
			return nil, fmt.Errorf("%s: %w", service, ErrNotResident)
		}
		if st.res == resParked {
			return nil, nil // already parked — nothing to do
		}
		if st.leases > 0 {
			return nil, &LeaseHeldError{Service: service, Leases: st.leases}
		}
		return func() error { return a.parkBackend(ctx, service) }, nil
	})
}

// Unpark promotes a parked backend back onto the GPU. Because that consumes a
// GPU slot, it goes through the same capacity planning a job would: if the card
// is full it evicts the least-recently-used idle resident first. Idempotent for
// an already-pinned backend; refused if the backend isn't parked, or if the card
// is full and every resident is busy.
func (a *Arbiter) Unpark(ctx context.Context, service string) error {
	if _, ok := a.cfg.Services[service]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownService, service)
	}
	return a.operate(ctx, func() (func() error, error) {
		st := a.states[service]
		if st != nil && st.res == resPinned {
			return nil, nil // already on the card
		}
		if st == nil || st.res != resParked {
			cur := "stopped"
			if st != nil {
				cur = st.res.String()
			}
			return nil, fmt.Errorf("%s: %w (residency is %q)", service, ErrNotParked, cur)
		}
		victim, ok := a.planLocked(service)
		if !ok {
			return nil, fmt.Errorf("%w: cannot make room to unpark %s", ErrGPUBusy, service)
		}
		// swap demotes the victim (if any, per its policy) and unparks target.
		return func() error { return a.swap(ctx, victim, service) }, nil
	})
}

// Warm brings a backend onto the GPU proactively — the operator's "load it now"
// button, so the first real job doesn't eat the cold start. It's the inverse of
// Stop and goes through the same capacity planning a job would: if the card is
// full it evicts the LRU idle residents first (per budget), then promotes the
// target (cold-start if stopped, unpark if parked) to pinned. No lease is taken,
// so lazy eviction leaves it pinned until something else needs the card.
// Idempotent for an already-pinned backend; refused only if the card can't be
// cleared (everything resident is busy).
func (a *Arbiter) Warm(ctx context.Context, service string) error {
	if _, ok := a.cfg.Services[service]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownService, service)
	}
	return a.operate(ctx, func() (func() error, error) {
		st := a.states[service]
		if st != nil && st.res == resPinned {
			return nil, nil // already hot
		}
		victims, ok := a.planLocked(service)
		if !ok {
			return nil, fmt.Errorf("%w: cannot make room to load %s", ErrGPUBusy, service)
		}
		return func() error { return a.swap(ctx, victims, service) }, nil
	})
}

// Stop takes a backend's container all the way down (weights back to disk),
// whatever its residency or evict policy — the fullest possible unload of a
// single backend. Idempotent: stopping an already-stopped backend is a no-op.
// Refused if the backend has active leases.
func (a *Arbiter) Stop(ctx context.Context, service string) error {
	if _, ok := a.cfg.Services[service]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownService, service)
	}
	return a.operate(ctx, func() (func() error, error) {
		st := a.states[service]
		if st == nil || st.res == resStopped {
			return nil, nil // already down
		}
		if st.leases > 0 {
			return nil, &LeaseHeldError{Service: service, Leases: st.leases}
		}
		return func() error { return a.stopBackend(ctx, service) }, nil
	})
}

// SkippedBackend records a backend UnloadAll couldn't touch because it was busy.
type SkippedBackend struct {
	Service string `json:"service"`
	Leases  int    `json:"leases"`
}

// UnloadResult reports what UnloadAll did: which backends it stopped, and which
// it skipped (leased) and why.
type UnloadResult struct {
	Unloaded []string         `json:"unloaded"`
	Skipped  []SkippedBackend `json:"skipped,omitempty"`
}

// UnloadAll hands the whole GPU back: it stops every resident backend (pinned or
// parked) that isn't busy, in one swap-slot hold. Leased backends are left alone
// and reported in Skipped rather than failing the whole call — the operator gets
// back as much of the card as is safe to reclaim right now.
func (a *Arbiter) UnloadAll(ctx context.Context) (UnloadResult, error) {
	var res UnloadResult
	err := a.operate(ctx, func() (func() error, error) {
		var toStop []string
		for name, st := range a.states {
			if st.res == resStopped {
				continue
			}
			if st.leases > 0 {
				res.Skipped = append(res.Skipped, SkippedBackend{Service: name, Leases: st.leases})
				continue
			}
			toStop = append(toStop, name)
		}
		// Deterministic order (map iteration is random) — nicer logs and tests.
		sort.Strings(toStop)
		sort.Slice(res.Skipped, func(i, j int) bool { return res.Skipped[i].Service < res.Skipped[j].Service })
		if len(toStop) == 0 {
			return nil, nil
		}
		return func() error {
			var firstErr error
			for _, name := range toStop {
				if err := a.stopBackend(ctx, name); err != nil {
					log.Printf("[arbiter] unload-all: stopping %s failed: %v", name, err)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				res.Unloaded = append(res.Unloaded, name)
			}
			return firstErr
		}, nil
	})
	return res, err
}

// --- internals ------------------------------------------------------------

// operate is the shared spine of every operator action. It claims the single GPU
// swap slot (waiting out any in-flight swap, ctx-aware), runs plan() under the
// lock to decide + validate, then runs the returned action outside the lock for
// the slow Docker/HTTP work. plan returns (nil, nil) for an idempotent no-op.
//
// Holding the swap slot is exactly the discipline the job path uses, so operator
// actions and automatic swaps never run concurrently.
func (a *Arbiter) operate(ctx context.Context, plan func() (func() error, error)) error {
	// cond.Wait can't select on ctx; wake the loop if the caller's ctx is done.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			a.mu.Lock()
			a.cond.Broadcast()
			a.mu.Unlock()
		case <-stop:
		}
	}()

	a.mu.Lock()
	for a.swapping {
		if err := ctx.Err(); err != nil {
			a.mu.Unlock()
			return err
		}
		a.cond.Wait()
	}
	if err := ctx.Err(); err != nil {
		a.mu.Unlock()
		return err
	}

	action, err := plan()
	if err != nil || action == nil {
		a.mu.Unlock()
		return err
	}

	a.swapping = true
	a.mu.Unlock()
	aerr := action()
	a.mu.Lock()
	a.swapping = false
	a.cond.Broadcast()
	a.mu.Unlock()
	return aerr
}

// parkBackend forces a park (weights -> CPU) and records it. Unlike demote, it
// doesn't consult the evict policy — the caller (Park) already checked it.
func (a *Arbiter) parkBackend(ctx context.Context, service string) error {
	log.Printf("[arbiter] operator: parking %s (weights -> CPU RAM, freeing GPU)", service)
	if err := backend.New(a.sup.BaseURL(service)).Park(ctx); err != nil {
		return err
	}
	a.setResidency(service, resParked)
	return nil
}

// stopBackend forces the container down and records it, whatever the policy.
func (a *Arbiter) stopBackend(ctx context.Context, service string) error {
	log.Printf("[arbiter] operator: stopping %s (container down, freeing all its RAM)", service)
	if err := a.sup.Stop(ctx, service); err != nil {
		return err
	}
	a.setResidency(service, resStopped)
	return nil
}

// --- preload-all ------------------------------------------------------------

// PreloadResult reports what PreloadAll did: which backends it cold-started and
// parked, and which it skipped and why (already resident, not parkable, or no
// room without stopping something).
type PreloadResult struct {
	Preloaded []string      `json:"preloaded"`
	Skipped   []PreloadSkip `json:"skipped,omitempty"`
}

// PreloadSkip is one backend PreloadAll left alone, with a reason a human can
// read off the toast.
type PreloadSkip struct {
	Service string `json:"service"`
	Reason  string `json:"reason"`
}

// PreloadAll is the inverse of UnloadAll: get every parkable backend into
// system RAM ahead of time, so the first real job of the day pays a 2–5s unpark
// instead of a 10–60s cold start. For each stopped backend with evict = "park",
// largest pinned cost first (the transient room only shrinks as park taxes pile
// up, so the big ones go while there's space): cold-start it onto the card, then
// immediately park it.
//
// The one rule: it never STOPS anything. If the card is too full to cold-start
// the next candidate, it may park idle pinned residents to make room (that's
// where they'd end up anyway), but a backend that's leased, or whose policy is
// stop, stays exactly where it is and the candidate is skipped instead.
//
// Each candidate is its own swap-slot hold, so real jobs can slip in between
// steps. That's deliberate: simplest thing that works, and a job grabbing a
// freshly warmed backend is a feature, not a race.
func (a *Arbiter) PreloadAll(ctx context.Context) (PreloadResult, error) {
	var res PreloadResult

	// Candidates, in a deterministic order: parkable, biggest first, then by name.
	var cands []string
	for name, svc := range a.cfg.Services {
		if svc.Evict == config.EvictPark {
			cands = append(cands, name)
		} else {
			res.Skipped = append(res.Skipped, PreloadSkip{name, "evict policy is stop, not parkable"})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		pi, pj := a.vramPinned(cands[i]), a.vramPinned(cands[j])
		if pi != pj {
			return pi > pj
		}
		return cands[i] < cands[j]
	})

	var firstErr error
	for _, name := range cands {
		err := a.operate(ctx, func() (func() error, error) {
			if st := a.states[name]; st != nil && st.res != resStopped {
				res.Skipped = append(res.Skipped, PreloadSkip{name, "already " + st.res.String()})
				return nil, nil
			}
			victims, ok := a.preloadPlanLocked(name)
			if !ok {
				res.Skipped = append(res.Skipped, PreloadSkip{name, "no room on the card without stopping something"})
				return nil, nil
			}
			if reason := a.ramCheckLocked(name); reason != "" {
				res.Skipped = append(res.Skipped, PreloadSkip{name, reason})
				return nil, nil
			}
			return func() error {
				defer a.clearPhases(append(append([]string{}, victims...), name)...)
				for _, v := range victims {
					a.setPhase(v, "parking (making room to preload)")
					if err := a.parkBackend(ctx, v); err != nil {
						return fmt.Errorf("parking %s to make room: %w", v, err)
					}
				}
				if err := a.promote(ctx, name); err != nil {
					return fmt.Errorf("cold-starting %s: %w", name, err)
				}
				a.setPhase(name, "parking (preload done, weights -> RAM)")
				if err := a.parkBackend(ctx, name); err != nil {
					return fmt.Errorf("parking %s after preload: %w", name, err)
				}
				res.Preloaded = append(res.Preloaded, name)
				return nil
			}, nil
		})
		if err != nil {
			log.Printf("[arbiter] preload-all: %s: %v", name, err)
			res.Skipped = append(res.Skipped, PreloadSkip{name, err.Error()})
			if firstErr == nil {
				firstErr = err
			}
			if ctx.Err() != nil {
				break // the caller's gone; don't keep grinding through the list
			}
		}
	}
	sort.Slice(res.Skipped, func(i, j int) bool { return res.Skipped[i].Service < res.Skipped[j].Service })
	return res, firstErr
}

// preloadPlanLocked is planLocked's gentler sibling: can `target` be cold-started
// onto the card RIGHT NOW without stopping anything? Returns the idle pinned
// park-policy residents (LRU first) that must be parked to make room — possibly
// none — or (nil, false) if even parking all of them isn't enough. Stop-policy
// and leased residents are never touched; that's the whole point of the caller.
func (a *Arbiter) preloadPlanLocked(target string) ([]string, bool) {
	budgeted := a.cfg.GPU.VRAMBudgetMB > 0
	capped := a.cfg.GPU.MaxResident > 0

	committed, pinnedCount := 0, 0
	for name, st := range a.states {
		switch st.res {
		case resPinned:
			committed += a.vramPinned(name)
			pinnedCount++
		case resParked:
			committed += a.vramParked(name)
		}
	}
	proj := committed + a.vramPinned(target)
	count := pinnedCount + 1
	fits := func() bool {
		return !(budgeted && proj > a.cfg.GPU.VRAMBudgetMB) && !(capped && count > a.cfg.GPU.MaxResident)
	}
	if fits() {
		return nil, true
	}

	type cand struct {
		name string
		used time.Time
	}
	var cands []cand
	for name, st := range a.states {
		if st.res == resPinned && st.leases == 0 && name != target &&
			a.cfg.Services[name].Evict == config.EvictPark {
			cands = append(cands, cand{name, st.lastUsed})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].used.Before(cands[j].used) })

	var victims []string
	for _, c := range cands {
		proj -= a.vramPinned(c.name) - a.vramParked(c.name)
		count--
		victims = append(victims, c.name)
		if fits() {
			return victims, true
		}
	}
	return nil, false
}

// ramCheckLocked guards the peasant box: parking N models means N sets of
// weights in system RAM. If memory.ram_budget_mb is set, the sum of every alive
// (pinned or parked) backend's ram_reserve_mb plus the candidate's must fit.
// Returns "" when fine, else a reason. This is the first place the RAM budget
// actually bites — the job path doesn't check it (yet), because lazy eviction
// only ever parks one thing at a time; preload is what stacks them up.
func (a *Arbiter) ramCheckLocked(target string) string {
	budget := a.cfg.Memory.RAMBudgetMB
	if budget <= 0 {
		return ""
	}
	used := 0
	for name, st := range a.states {
		if st.res != resStopped {
			used += a.cfg.Services[name].RAMReserveMB
		}
	}
	need := a.cfg.Services[target].RAMReserveMB
	if used+need > budget {
		return fmt.Sprintf("would exceed memory.ram_budget_mb (%d + %d > %d MB)", used, need, budget)
	}
	return ""
}
