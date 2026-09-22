// Package engine is the orchestration glue for a single job's life: bring the
// backend up, forward the work, poll until done, harvest the artifacts into
// ASS's own store, and record every state change.
//
// Slice 1 handles one backend at a time with no swapping — the arbiter that
// decides *who* is resident arrives in Slice 2. What's here is the vertical
// path a job travels, end to end.
package engine

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"time"

	"github.com/stevelittlefish/AudioSlopServer/internal/arbiter"
	"github.com/stevelittlefish/AudioSlopServer/internal/backend"
	"github.com/stevelittlefish/AudioSlopServer/internal/config"
	"github.com/stevelittlefish/AudioSlopServer/internal/results"
	"github.com/stevelittlefish/AudioSlopServer/internal/store"
)

// Arbiter is the slice of the arbiter the engine needs: get a backend resident
// (holding a lease so it can't be evicted mid-job) and know where to reach it.
// An interface keeps the engine testable without a real Docker daemon.
type Arbiter interface {
	Acquire(ctx context.Context, service string) (func(), error)
	BaseURL(service string) string
}

// Engine wires together the arbiter, the job store, and the results store.
type Engine struct {
	cfg     *config.Config
	arb     Arbiter
	store   *store.Store
	results *results.Store

	// How long a single job may take end to end before we give up on it. Real
	// generation is minutes; this is deliberately generous.
	jobTimeout time.Duration
	// How often we poll a backend for job status.
	pollInterval time.Duration

	// OnJobDone, if set, is called (non-blocking, best-effort) whenever a job
	// reaches a terminal state — the reaper hangs its "a fresh result just landed,
	// maybe sweep disk" trigger here. Nil by default; the engine works fine without
	// it, and it must never block the job path.
	OnJobDone func()
}

// New builds an engine.
func New(cfg *config.Config, arb Arbiter, st *store.Store, res *results.Store) *Engine {
	return &Engine{
		cfg:          cfg,
		arb:          arb,
		store:        st,
		results:      res,
		jobTimeout:   30 * time.Minute,
		pollInterval: time.Second,
	}
}

// Submit creates a job and starts working it in the background, returning the
// ASS job id immediately. The body is buffered so the caller's request can
// return right away; large uploads streaming to a temp file is a later concern.
func (e *Engine) Submit(ctx context.Context, service string, body []byte, contentType string) (store.Job, error) {
	if _, ok := e.cfg.Services[service]; !ok {
		return store.Job{}, fmt.Errorf("unknown service %q", service)
	}
	j, err := e.store.CreateJob(ctx, service)
	if err != nil {
		return store.Job{}, err
	}
	// Detached context: the job outlives the HTTP request that started it.
	go e.process(j.ID, service, body, contentType)
	return j, nil
}

// process runs the whole job lifecycle. Any failure marks the job failed with a
// reason a human can actually read.
func (e *Engine) process(jobID, service string, body []byte, contentType string) {
	ctx, cancel := context.WithTimeout(context.Background(), e.jobTimeout)
	defer cancel()

	svc := e.cfg.Services[service]

	// 1. Acquire the backend from the arbiter: make it resident on the GPU
	//    (queuing behind any model swap) and hold a lease so it can't be evicted
	//    until we've harvested the results. release drops the lease.
	release, err := e.arb.Acquire(ctx, service)
	if err != nil {
		e.fail(jobID, fmt.Sprintf("acquiring %s: %v", service, err))
		return
	}
	defer release()
	client := backend.New(e.arb.BaseURL(service))

	// 2. Forward the work to the backend.
	backendJobID, err := client.Submit(ctx, svc.Verb, bytes.NewReader(body), contentType)
	if err != nil {
		e.fail(jobID, fmt.Sprintf("submitting to %s: %v", service, err))
		return
	}
	if err := e.store.MarkRunning(ctx, jobID); err != nil {
		log.Printf("[engine] job %s: MarkRunning: %v", jobID, err)
	}
	log.Printf("[engine] job %s -> %s backend job %s", jobID, service, backendJobID)

	// 3. Poll until the backend finishes.
	final, err := e.poll(ctx, client, backendJobID)
	if err != nil {
		e.fail(jobID, fmt.Sprintf("polling %s: %v", service, err))
		return
	}
	if final.State == store.StateFailed {
		e.fail(jobID, fmt.Sprintf("backend reported failure: %s", final.Error))
		return
	}

	// 3.5. Record the backend's real VRAM use while the model is still resident
	//      and lease-held. peak_mb captured the inference peak, so this is the
	//      calibration data for the service's vram_pinned_mb budget. Best-effort:
	//      a telemetry read must never fail a job.
	e.recordVRAM(ctx, service, jobID, client)

	// 4. Harvest artifacts into our own store BEFORE the backend can be evicted.
	for _, a := range final.Artifacts {
		if err := e.harvest(ctx, client, jobID, backendJobID, a); err != nil {
			e.fail(jobID, fmt.Sprintf("harvesting %s: %v", a.Name, err))
			return
		}
	}

	if err := e.store.MarkSucceeded(ctx, jobID); err != nil {
		log.Printf("[engine] job %s: MarkSucceeded: %v", jobID, err)
	}
	log.Printf("[engine] job %s succeeded with %d artifact(s)", jobID, len(final.Artifacts))
	e.jobDone()
}

// jobDone fires the terminal-state hook if one is wired, ignoring a nil hook so
// callers don't have to guard it. Best-effort: the reaper's Trigger is itself
// non-blocking, so this never stalls the job path.
func (e *Engine) jobDone() {
	if e.OnJobDone != nil {
		e.OnJobDone()
	}
}

// BackendInfo makes a backend resident (queuing behind any swap) and returns its
// /v1/info body verbatim. It's the read-only counterpart to Submit: ASS holds no
// CUDA context and can't read a card, so a client that wants what only the backend
// knows — the loaded model, its capabilities, the Stable Audio LoRA list — asks
// ASS, which reads it over HTTP and forwards the bytes. The lease is dropped as
// soon as the read returns; nothing here pins the backend beyond the call.
func (e *Engine) BackendInfo(ctx context.Context, service string) ([]byte, error) {
	if _, ok := e.cfg.Services[service]; !ok {
		return nil, fmt.Errorf("unknown service %q", service)
	}
	release, err := e.arb.Acquire(ctx, service)
	if err != nil {
		return nil, fmt.Errorf("acquiring %s: %w", service, err)
	}
	defer release()
	return backend.New(e.arb.BaseURL(service)).InfoRaw(ctx)
}

// recordVRAM reads the backend's self-reported GPU memory and stores it. Purely
// telemetry — every failure path just logs, because the job already succeeded and
// a missing sample is not worth failing it over. A backend on a GPU-less box
// reports cuda=false; we skip those so the table holds only real numbers.
func (e *Engine) recordVRAM(ctx context.Context, service, jobID string, client *backend.Client) {
	info, err := client.Info(ctx)
	if err != nil {
		log.Printf("[engine] job %s: reading %s VRAM: %v", jobID, service, err)
		return
	}
	if !info.VRAM.CUDA {
		return
	}
	v := store.VRAMSample{
		AllocatedMB: info.VRAM.AllocatedMB,
		ReservedMB:  info.VRAM.ReservedMB,
		PeakMB:      info.VRAM.PeakMB,
	}
	if err := e.store.RecordVRAM(ctx, service, jobID, v); err != nil {
		log.Printf("[engine] job %s: recording %s VRAM: %v", jobID, service, err)
		return
	}
	log.Printf("[engine] %s VRAM: allocated=%dMB reserved=%dMB peak=%dMB", service, v.AllocatedMB, v.ReservedMB, v.PeakMB)
}

// poll waits for the backend job to reach a terminal state.
func (e *Engine) poll(ctx context.Context, client *backend.Client, backendJobID string) (backend.Job, error) {
	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()
	for {
		j, err := client.Status(ctx, backendJobID)
		if err != nil {
			return backend.Job{}, err
		}
		switch j.State {
		case store.StateSucceeded, store.StateFailed:
			return j, nil
		}
		select {
		case <-ctx.Done():
			return backend.Job{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// harvest downloads one artifact and records it in our store.
func (e *Engine) harvest(ctx context.Context, client *backend.Client, jobID, backendJobID string, a backend.Artifact) error {
	rc, err := client.Download(ctx, backendJobID, a.Name)
	if err != nil {
		return err
	}
	defer rc.Close()

	path, n, err := e.results.Save(jobID, a.Name, rc)
	if err != nil {
		return err
	}
	return e.store.AddArtifact(ctx, jobID, store.Artifact{
		Name:        a.Name,
		Kind:        a.Kind,
		ContentType: a.ContentType,
		Bytes:       n,
		Path:        path,
	})
}

// fail marks a job failed and logs why. Uses a fresh short context so the
// failure is recorded even if the job's own context has expired.
func (e *Engine) fail(jobID, reason string) {
	log.Printf("[engine] job %s FAILED: %s", jobID, reason)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.store.MarkFailed(ctx, jobID, reason); err != nil {
		log.Printf("[engine] job %s: MarkFailed also failed: %v", jobID, err)
	}
	e.jobDone()
}

// compile-time check that the real arbiter satisfies our interface.
var _ Arbiter = (*arbiter.Arbiter)(nil)
