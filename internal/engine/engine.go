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

	"github.com/stevelittlefish/AudioSlopServer/internal/backend"
	"github.com/stevelittlefish/AudioSlopServer/internal/config"
	"github.com/stevelittlefish/AudioSlopServer/internal/results"
	"github.com/stevelittlefish/AudioSlopServer/internal/store"
	"github.com/stevelittlefish/AudioSlopServer/internal/supervisor"
)

// Supervisor is the slice of the supervisor the engine needs. An interface keeps
// the engine testable without a real Docker daemon.
type Supervisor interface {
	EnsureUp(ctx context.Context, service string) error
	BaseURL(service string) string
}

// Engine wires together the supervisor, the job store, and the results store.
type Engine struct {
	cfg     *config.Config
	sup     Supervisor
	store   *store.Store
	results *results.Store

	// How long a single job may take end to end before we give up on it. Real
	// generation is minutes; this is deliberately generous.
	jobTimeout time.Duration
	// How often we poll a backend for job status.
	pollInterval time.Duration
}

// New builds an engine.
func New(cfg *config.Config, sup Supervisor, st *store.Store, res *results.Store) *Engine {
	return &Engine{
		cfg:          cfg,
		sup:          sup,
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

	// 1. Make the backend resident and healthy. (Slice 2: this queues behind a
	//    model swap; for now it just brings the one backend up.)
	if err := e.sup.EnsureUp(ctx, service); err != nil {
		e.fail(jobID, fmt.Sprintf("bringing up %s: %v", service, err))
		return
	}
	client := backend.New(e.sup.BaseURL(service))

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
}

// compile-time check that the real supervisor satisfies our interface.
var _ Supervisor = (*supervisor.Supervisor)(nil)
