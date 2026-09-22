// Package reaper enforces ASS's disk retention policy. Harvested results
// otherwise pile up forever (~80 MB/job in practice — ~8 GB per 100 jobs), so on
// a box with finite disk the reaper deletes the OLDEST finished jobs — both their
// sqlite rows and their on-disk artifact bytes — until the store is back within
// the configured limits. Nobody weeps for last week's slop.
//
// It never touches live work: only terminal jobs (succeeded/failed) are
// candidates, so a queued or running job can't be reaped out from under a client.
package reaper

import (
	"context"
	"log"
	"time"

	"github.com/stevelittlefish/AudioSlopServer/internal/config"
	"github.com/stevelittlefish/AudioSlopServer/internal/results"
	"github.com/stevelittlefish/AudioSlopServer/internal/store"
)

// Reaper deletes old job results to keep disk usage under the retention limits.
type Reaper struct {
	cfg     config.Retention
	store   *store.Store
	results *results.Store
	trigger chan struct{}
}

// New builds a reaper. It does nothing until Run is called.
func New(cfg config.Retention, st *store.Store, res *results.Store) *Reaper {
	return &Reaper{cfg: cfg, store: st, results: res, trigger: make(chan struct{}, 1)}
}

// Run sweeps once immediately, then on every tick and every Trigger, until ctx is
// cancelled. Blocks — run it in a goroutine. A zero SweepInterval disables only
// the periodic tick; the startup sweep and Trigger-driven sweeps still fire.
func (r *Reaper) Run(ctx context.Context) {
	r.SweepNow(ctx)

	var tickC <-chan time.Time
	if d := r.cfg.SweepInterval.Duration; d > 0 {
		t := time.NewTicker(d)
		defer t.Stop()
		tickC = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tickC:
			r.SweepNow(ctx)
		case <-r.trigger:
			r.SweepNow(ctx)
		}
	}
}

// Trigger asks for a sweep soon without blocking the caller. Coalesced: a burst
// of triggers between sweeps collapses into a single one, so the engine can fire
// it after every job completes for free.
func (r *Reaper) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}

// SweepNow enforces the policy once: work out who dies, then for each victim
// delete the bytes first, then the record. Bytes-first is deliberate — if we die
// mid-sweep, an orphaned record (row with no files) is a harmless lie the next
// sweep tidies, whereas an orphaned file (bytes with no row) leaks disk forever.
func (r *Reaper) SweepNow(ctx context.Context) {
	jobs, err := r.store.TerminalJobsOldestFirst(ctx)
	if err != nil {
		log.Printf("[reaper] listing jobs: %v", err)
		return
	}
	victims := r.plan(jobs, time.Now())
	if len(victims) == 0 {
		return
	}

	var freed int64
	var n int
	for _, v := range victims {
		if err := r.results.RemoveJob(v.ID); err != nil {
			// Leave the record intact so next sweep retries, rather than deleting the
			// row and orphaning the bytes we just failed to remove.
			log.Printf("[reaper] job %s: removing files: %v", v.ID, err)
			continue
		}
		if err := r.store.DeleteJob(ctx, v.ID); err != nil {
			log.Printf("[reaper] job %s: deleting record: %v", v.ID, err)
			continue
		}
		freed += v.Bytes
		n++
	}
	if n > 0 {
		log.Printf("[reaper] reclaimed %d job(s), ~%d MB", n, freed/(1024*1024))
	}
}

// plan picks the victims from a list of terminal jobs ordered oldest-ended first.
// A job dies if it trips ANY configured limit — too old, over the job count, or
// needed to drag total bytes back under budget — and we walk oldest-first so the
// cheapest history to lose goes first and the count/byte totals shrink as we go.
func (r *Reaper) plan(jobs []store.TerminalJob, now time.Time) []store.TerminalJob {
	var total int64
	for _, j := range jobs {
		total += j.Bytes
	}
	budget := r.cfg.MaxTotalMB * 1024 * 1024

	var victims []store.TerminalJob
	remaining := len(jobs)
	for _, j := range jobs {
		reap := false
		if r.cfg.MaxAge.Duration > 0 && now.Sub(j.EndedAt) > r.cfg.MaxAge.Duration {
			reap = true
		}
		if r.cfg.MaxJobs > 0 && remaining > r.cfg.MaxJobs {
			reap = true
		}
		if budget > 0 && total > budget {
			reap = true
		}
		if reap {
			victims = append(victims, j)
			remaining--
			total -= j.Bytes
		}
	}
	return victims
}
