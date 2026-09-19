// Package arbiter is the brain of ASS: it decides who gets the GPU. Slice 1 had
// one backend and no decisions to make; Slice 2 has several, and exactly one card
// to share between them. The arbiter enforces the single invariant the whole
// project exists for — at most a budgeted few models are resident on the GPU at
// once — and performs the swap when a job needs a backend that isn't.
//
// The GPU is the lock. Only one swap happens at a time; everyone else queues
// behind it. An in-flight job holds a *lease* on its backend so it can't be
// evicted out from under a running job (that's what protects harvest-on-
// completion: the backend stays put until ASS has pulled the results).
//
// Eviction is lazy (ComfyUI-style, not Ollama-style): a resident model stays
// pinned until a *different* model actually needs the card, then the
// least-recently-used resident is demoted — parked or stopped per its config.
package arbiter

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/stevelittlefish/AudioSlopServer/internal/backend"
	"github.com/stevelittlefish/AudioSlopServer/internal/config"
)

// Supervisor is the muscle the arbiter commands: it makes containers exist,
// run, and answer, or takes them down. An interface so the arbiter is testable
// without a Docker daemon.
type Supervisor interface {
	EnsureUp(ctx context.Context, service string) error
	Stop(ctx context.Context, service string) error
	BaseURL(service string) string
}

// residency is where a backend's weights are, cheapest-to-restore last.
type residency int

const (
	// resStopped: not resident. Container is down (or never started); weights on
	// disk. A cold start is needed to use it.
	resStopped residency = iota
	// resParked: alive, weights moved to CPU RAM, GPU freed. A quick /unpark
	// brings it back. Only reachable for backends with evict = "park".
	resParked
	// resPinned: holds the GPU, ready to serve. The hot model.
	resPinned
)

func (r residency) String() string {
	switch r {
	case resPinned:
		return "pinned"
	case resParked:
		return "parked"
	default:
		return "stopped"
	}
}

// state tracks one backend's residency, how many jobs are currently using it,
// and when it was last touched (for LRU victim selection).
type state struct {
	res      residency
	leases   int
	lastUsed time.Time
	// phase is a transient, human-facing note about what's happening to this
	// backend RIGHT NOW during a swap ("starting", "waiting for health",
	// "parking", …). Empty when the backend is settled. It exists purely so the
	// admin console can tell an operator "it's mid-load, hang tight" instead of a
	// dead greyed-out button. Set under mu, cleared the moment residency settles.
	phase      string
	phaseSince time.Time
}

// Arbiter owns the residency map and serializes swaps. All mutable state lives
// behind mu; the cond wakes waiters when a lease is released or a swap finishes.
type Arbiter struct {
	sup Supervisor
	cfg *config.Config

	mu     sync.Mutex
	cond   *sync.Cond
	states map[string]*state
	// swapping is true while one goroutine is mid-swap (talking to Docker / the
	// backend over HTTP). The GPU is the lock: only one swap at a time.
	swapping bool
	now      func() time.Time // injectable clock for tests
}

// New builds an arbiter over a supervisor and the loaded config.
func New(sup Supervisor, cfg *config.Config) *Arbiter {
	a := &Arbiter{
		sup:    sup,
		cfg:    cfg,
		states: make(map[string]*state),
		now:    time.Now,
	}
	a.cond = sync.NewCond(&a.mu)
	return a
}

// BaseURL passes through to the supervisor, so the engine can reach a backend it
// just acquired without also depending on the supervisor directly.
func (a *Arbiter) BaseURL(service string) string { return a.sup.BaseURL(service) }

// Acquire makes service resident on the GPU and returns a release func the
// caller MUST call when done (defer it). Between Acquire and release the backend
// holds a lease and cannot be evicted, so a job's results survive until harvest.
//
// If service is already pinned this is nearly free. Otherwise the caller queues:
// behind an in-flight swap, or behind the eviction of a busy resident, and then
// drives the swap itself. One swap happens at a time, project-wide.
func (a *Arbiter) Acquire(ctx context.Context, service string) (func(), error) {
	if _, ok := a.cfg.Services[service]; !ok {
		return nil, fmt.Errorf("unknown service %q", service)
	}

	// Waiting on a sync.Cond can't select on ctx, so post a broadcaster that
	// wakes the loop if the caller's context is cancelled (timeout / shutdown).
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
	defer a.mu.Unlock()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		st := a.states[service]
		if st == nil {
			st = &state{res: resStopped}
			a.states[service] = st
		}

		// Fast path: already on the card. Take a lease and go.
		if st.res == resPinned {
			st.leases++
			st.lastUsed = a.now()
			return a.releaser(service), nil
		}

		// Someone else is mid-swap. Wait and reassess — the world will look
		// different when they're done.
		if a.swapping {
			a.cond.Wait()
			continue
		}

		// We need to make room and promote `service`. Is there capacity, or must
		// we evict? planLocked returns the LRU victims to demote first (maybe none).
		victims, ok := a.planLocked(service)
		if !ok {
			// The card is full and every resident is busy (has leases). Wait for
			// one to drain, then try again.
			a.cond.Wait()
			continue
		}

		// Claim the swap. Release the lock while we do the slow Docker/HTTP work,
		// so status reads and same-backend leases aren't blocked on our I/O.
		a.swapping = true
		a.mu.Unlock()
		err := a.swap(ctx, victims, service)
		a.mu.Lock()
		a.swapping = false
		a.cond.Broadcast() // whatever happened, waiters must reassess

		if err != nil {
			return nil, err
		}
		// swap succeeded: victim (if any) demoted, service pinned. Record it and
		// take our lease. states were updated inside swap under the relock.
		st = a.states[service]
		st.leases++
		st.lastUsed = a.now()
		return a.releaser(service), nil
	}
}

// releaser returns the func handed back to the caller: drop the lease and wake
// anyone waiting to evict this backend.
func (a *Arbiter) releaser(service string) func() {
	return func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		st := a.states[service]
		if st == nil || st.leases == 0 {
			log.Printf("[arbiter] release of %s with no lease — double release?", service)
			return
		}
		st.leases--
		st.lastUsed = a.now()
		a.cond.Broadcast()
	}
}

// vramPinned / vramParked are what a service costs the card in each residency.
// Parked keeps only the context tax; stopped keeps nothing (its parked cost is
// normalized to 0 in config). Both are declared per service — see config.
func (a *Arbiter) vramPinned(name string) int { return a.cfg.Services[name].VRAMPinnedMB }
func (a *Arbiter) vramParked(name string) int { return a.cfg.Services[name].VRAMParkedMB }

// planLocked decides, for promoting `target`, whether we can proceed now and, if
// so, which residents to evict first. Returns (victims, true) to go ahead —
// victims is empty when the card has room and nobody needs evicting, or a list
// (LRU-first) that must be demoted before target fits. Returns (nil, false) when
// the card is full and everything left resident is busy, so the caller must wait.
//
// Two gates, both optional. When gpu.vram_budget_mb is set, VRAM MB is the real
// limit: every pinned backend costs its vram_pinned_mb, every parked backend its
// vram_parked_mb context tax, and their sum plus target must stay under budget.
// When gpu.max_resident is set (>0), it's an additional hard cap on the pin
// count. With no budget configured we fall back to the pure count gate (the
// pre-budget behaviour, max_resident defaulting to 1).
//
// One eviction often isn't enough under a budget — evicting demucs won't make
// room for ACE-Step — so we evict the LRU non-leased pinned backends one at a
// time until target fits, or run out of evictable backends and must wait.
func (a *Arbiter) planLocked(target string) ([]string, bool) {
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

	// Promoting target: it becomes pinned. If it was parked its tax is already in
	// committed, so promotion only adds the difference; if stopped it adds the lot.
	targetCur := 0
	if st := a.states[target]; st != nil && st.res == resParked {
		targetCur = a.vramParked(target)
	}
	projCommitted := committed + a.vramPinned(target) - targetCur
	projCount := pinnedCount + 1

	fits := func() bool {
		if budgeted && projCommitted > a.cfg.GPU.VRAMBudgetMB {
			return false
		}
		if capped && projCount > a.cfg.GPU.MaxResident {
			return false
		}
		return true
	}
	if fits() {
		return nil, true
	}

	// Doesn't fit: evict the cheapest victims first among the pinned backends with
	// no active lease (and never the target itself). "Cheapest" = lowest configured
	// priority, ties broken LRU. Each eviction frees its pinned cost, minus whatever
	// it retains parked (a stop victim retains nothing; a park victim keeps its
	// context tax on the card).
	type cand struct {
		name     string
		priority int
		used     time.Time
	}
	var cands []cand
	for name, st := range a.states {
		if st.res == resPinned && st.leases == 0 && name != target {
			cands = append(cands, cand{name, a.cfg.Services[name].Priority, st.lastUsed})
		}
	}
	// Lowest priority dies first; among equals, least-recently-used dies first.
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].priority != cands[j].priority {
			return cands[i].priority < cands[j].priority
		}
		return cands[i].used.Before(cands[j].used)
	})

	var victims []string
	for _, c := range cands {
		retained := 0
		if a.cfg.Services[c.name].Evict == config.EvictPark {
			retained = a.vramParked(c.name)
		}
		projCommitted -= a.vramPinned(c.name) - retained
		projCount--
		victims = append(victims, c.name)
		if fits() {
			return victims, true
		}
	}
	return nil, false // full, and everything evictable has been counted: must wait
}

// swap does the slow part outside the arbiter lock: demote each victim (park or
// stop, per its config) and promote the target (unpark if it was parked, else a
// cold start). It re-takes the lock only to record the resulting residencies, so
// the map always reflects reality even if a step fails partway.
func (a *Arbiter) swap(ctx context.Context, victims []string, target string) error {
	// Whatever happens, don't leave a stale "starting…" note stuck on a card if a
	// step errors out partway. setResidency clears phases on the happy path;
	// this catches the sad one.
	defer a.clearPhases(append(append([]string{}, victims...), target)...)
	for _, victim := range victims {
		if err := a.demote(ctx, victim); err != nil {
			return fmt.Errorf("evicting %s: %w", victim, err)
		}
	}
	if err := a.promote(ctx, target); err != nil {
		return fmt.Errorf("bringing up %s: %w", target, err)
	}
	return nil
}

// demote frees the GPU held by victim, using its configured eviction policy.
// park keeps the container alive (weights to CPU) for a fast return; stop kills
// it outright. On success the victim's recorded residency is updated.
func (a *Arbiter) demote(ctx context.Context, victim string) error {
	svc := a.cfg.Services[victim]
	switch svc.Evict {
	case config.EvictPark:
		log.Printf("[arbiter] parking %s (weights -> CPU RAM, freeing GPU)", victim)
		a.setPhase(victim, "parking (freeing GPU)")
		if err := backend.New(a.sup.BaseURL(victim)).Park(ctx); err != nil {
			return err
		}
		a.setResidency(victim, resParked)
	default: // EvictStop (also the safe default)
		log.Printf("[arbiter] stopping %s (container down, freeing all its RAM)", victim)
		a.setPhase(victim, "stopping (freeing GPU)")
		if err := a.sup.Stop(ctx, victim); err != nil {
			return err
		}
		a.setResidency(victim, resStopped)
	}
	return nil
}

// promote gets target onto the GPU. If it was parked the container is already
// alive, so we just ensure it's healthy and /unpark it (the cheap path). If it
// was stopped, EnsureUp cold-starts it. Either way it ends up pinned.
func (a *Arbiter) promote(ctx context.Context, target string) error {
	a.mu.Lock()
	wasParked := a.states[target] != nil && a.states[target].res == resParked
	a.mu.Unlock()

	if wasParked {
		log.Printf("[arbiter] unparking %s (weights -> GPU, the fast path)", target)
		a.setPhase(target, "unparking (weights -> GPU)")
		if err := a.sup.EnsureUp(ctx, target); err != nil { // cheap: already running
			return err
		}
		if err := backend.New(a.sup.BaseURL(target)).Unpark(ctx); err != nil {
			return err
		}
	} else {
		log.Printf("[arbiter] cold-starting %s (weights from disk)", target)
		a.setPhase(target, "cold-starting (container + model)")
		if err := a.sup.EnsureUp(ctx, target); err != nil {
			return err
		}
	}
	a.setResidency(target, resPinned)
	return nil
}

// setResidency records a backend's new residency under the lock. Reaching a
// settled residency clears any transient phase note — the work is done.
func (a *Arbiter) setResidency(service string, r residency) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.states[service]
	if st == nil {
		st = &state{}
		a.states[service] = st
	}
	st.res = r
	st.lastUsed = a.now()
	st.phase = ""
	st.phaseSince = time.Time{}
}

// setPhase records what's happening to a backend mid-swap, for the status view.
// A blank phase clears it. Safe on a backend the arbiter hasn't seen yet.
func (a *Arbiter) setPhase(service, phase string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.states[service]
	if st == nil {
		st = &state{res: resStopped}
		a.states[service] = st
	}
	st.phase = phase
	if phase == "" {
		st.phaseSince = time.Time{}
	} else if st.phaseSince.IsZero() || st.phase != phase {
		st.phaseSince = a.now()
	}
}

// clearPhases wipes any lingering phase note from a set of backends. swap defers
// this so a failed swap doesn't leave "starting…" stuck on a card forever.
func (a *Arbiter) clearPhases(services ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range services {
		if st := a.states[s]; st != nil {
			st.phase = ""
			st.phaseSince = time.Time{}
		}
	}
}

// BackendState is a read-only snapshot for the /v1/backends view. VRAMMB is what
// this backend is costing the card right now (its pinned cost while pinned, its
// park tax while parked, 0 while stopped) — the raw material for spotting why a
// swap evicted something, or how close to gpu.vram_budget_mb you are.
type BackendState struct {
	Residency string `json:"residency"`
	Leases    int    `json:"leases"`
	LastUsed  string `json:"last_used,omitempty"`
	VRAMMB    int    `json:"vram_mb"`
	// Phase is a transient note about an in-flight swap ("cold-starting…",
	// "parking…"). Empty when settled. PhaseSince lets the UI show an elapsed
	// timer so a long load looks alive, not hung.
	Phase      string `json:"phase,omitempty"`
	PhaseSince string `json:"phase_since,omitempty"`
}

// Snapshot returns the current residency of every backend the arbiter has
// touched. Backends never acquired simply don't appear (they're stopped).
func (a *Arbiter) Snapshot() map[string]BackendState {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]BackendState, len(a.states))
	for name, st := range a.states {
		bs := BackendState{Residency: st.res.String(), Leases: st.leases}
		switch st.res {
		case resPinned:
			bs.VRAMMB = a.vramPinned(name)
		case resParked:
			bs.VRAMMB = a.vramParked(name)
		}
		if !st.lastUsed.IsZero() {
			bs.LastUsed = st.lastUsed.UTC().Format(time.RFC3339)
		}
		if st.phase != "" {
			bs.Phase = st.phase
			if !st.phaseSince.IsZero() {
				bs.PhaseSince = st.phaseSince.UTC().Format(time.RFC3339)
			}
		}
		out[name] = bs
	}
	return out
}
