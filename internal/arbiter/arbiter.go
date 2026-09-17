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
		// we evict? Pick an LRU victim if the card is full.
		victim, ok := a.planLocked(service)
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
		err := a.swap(ctx, victim, service)
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

// planLocked decides, for promoting `target`, whether we can proceed now and if
// so which resident to evict. Returns (victim, true) to go ahead — victim is ""
// when the card has spare capacity and nobody needs evicting. Returns ("", false)
// when the card is full and every resident is busy, so the caller must wait.
//
// Only *pinned* backends count against the GPU capacity: parked backends have
// handed their VRAM back (bar the context tax, a later budgeting concern). The
// victim is the least-recently-used pinned backend with no active leases.
func (a *Arbiter) planLocked(target string) (string, bool) {
	pinned := 0
	var victim string
	var victimUsed time.Time
	for name, st := range a.states {
		if st.res != resPinned {
			continue
		}
		pinned++
		if st.leases > 0 {
			continue // in use — can't evict this one
		}
		if victim == "" || st.lastUsed.Before(victimUsed) {
			victim, victimUsed = name, st.lastUsed
		}
	}

	if pinned < a.cfg.GPU.MaxResident {
		return "", true // room to spare — promote without evicting
	}
	if victim == "" {
		return "", false // full, and everything resident is busy: must wait
	}
	return victim, true
}

// swap does the slow part outside the arbiter lock: demote the victim (park or
// stop, per its config) and promote the target (unpark if it was parked, else a
// cold start). It re-takes the lock only to record the resulting residencies, so
// the map always reflects reality even if a step fails partway.
func (a *Arbiter) swap(ctx context.Context, victim, target string) error {
	if victim != "" {
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
		if err := backend.New(a.sup.BaseURL(victim)).Park(ctx); err != nil {
			return err
		}
		a.setResidency(victim, resParked)
	default: // EvictStop (also the safe default)
		log.Printf("[arbiter] stopping %s (container down, freeing all its RAM)", victim)
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
		if err := a.sup.EnsureUp(ctx, target); err != nil { // cheap: already running
			return err
		}
		if err := backend.New(a.sup.BaseURL(target)).Unpark(ctx); err != nil {
			return err
		}
	} else {
		log.Printf("[arbiter] cold-starting %s (weights from disk)", target)
		if err := a.sup.EnsureUp(ctx, target); err != nil {
			return err
		}
	}
	a.setResidency(target, resPinned)
	return nil
}

// setResidency records a backend's new residency under the lock.
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
}

// BackendState is a read-only snapshot for the /v1/backends view.
type BackendState struct {
	Residency string `json:"residency"`
	Leases    int    `json:"leases"`
	LastUsed  string `json:"last_used,omitempty"`
}

// Snapshot returns the current residency of every backend the arbiter has
// touched. Backends never acquired simply don't appear (they're stopped).
func (a *Arbiter) Snapshot() map[string]BackendState {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]BackendState, len(a.states))
	for name, st := range a.states {
		bs := BackendState{Residency: st.res.String(), Leases: st.leases}
		if !st.lastUsed.IsZero() {
			bs.LastUsed = st.lastUsed.UTC().Format(time.RFC3339)
		}
		out[name] = bs
	}
	return out
}
