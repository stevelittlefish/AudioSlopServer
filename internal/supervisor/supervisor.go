// Package supervisor brings backends up and takes them down. It's the muscle
// between the arbiter's decisions ("make demucs resident") and the Docker daemon
// that actually runs the containers. It does not decide *who* should be up —
// that's the arbiter's job (Slice 2). It just makes it so, reliably.
package supervisor

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/stevelittlefish/AudioSlopServer/internal/config"
	"github.com/stevelittlefish/AudioSlopServer/internal/docker"
)

// Supervisor manages the lifecycle of every configured backend against one
// Docker daemon.
type Supervisor struct {
	docker *docker.Client
	cfg    *config.Config
}

// New wires a supervisor to a daemon client and the loaded config.
func New(d *docker.Client, cfg *config.Config) *Supervisor {
	return &Supervisor{docker: d, cfg: cfg}
}

// BaseURL is where ASS reaches a backend's HTTP API. Host mode (dev) publishes
// the container port to the same host port, so localhost:<port> is it. Container
// mode (deploy) will address by container name instead — a future change that
// lands here and nowhere else, per the topology note.
func (s *Supervisor) BaseURL(service string) string {
	svc := s.cfg.Services[service]
	return "http://localhost:" + strconv.Itoa(svc.Port)
}

// EnsureUp makes the named backend exist, run, and pass its health check. It's
// idempotent: calling it on an already-healthy backend is a couple of cheap
// HTTP round-trips and returns nil. The context bounds how long we'll wait for
// health before giving up.
func (s *Supervisor) EnsureUp(ctx context.Context, service string) error {
	svc, ok := s.cfg.Services[service]
	if !ok {
		return fmt.Errorf("unknown service %q", service)
	}
	name := svc.ContainerName(service)

	state, err := s.docker.Inspect(ctx, name)
	if err != nil {
		return fmt.Errorf("inspecting %q: %w", name, err)
	}

	switch {
	case state.Running:
		// Already up. Trust it enough to health-check, not enough to recreate.
		log.Printf("[supervisor] %s already running (%s)", name, state.ID[:min(12, len(state.ID))])
	case state.Exists:
		// Exists but stopped — likely stale config from a previous run. Recreate
		// from current config so a port/env change in TOML actually takes effect,
		// rather than silently starting yesterday's container.
		log.Printf("[supervisor] %s exists but stopped — recreating from current config", name)
		if err := s.docker.Remove(ctx, name, true); err != nil {
			return fmt.Errorf("removing stale %q: %w", name, err)
		}
		if err := s.create(ctx, service, svc, name); err != nil {
			return err
		}
		if err := s.docker.Start(ctx, name); err != nil {
			return err
		}
	default:
		// Doesn't exist. Create and start.
		if err := s.create(ctx, service, svc, name); err != nil {
			return err
		}
		if err := s.docker.Start(ctx, name); err != nil {
			return err
		}
	}

	// Now the important part: wait until it actually answers.
	url := s.BaseURL(service) + "/health"
	log.Printf("[supervisor] waiting for %s to be healthy at %s", name, url)
	if err := WaitHealthy(ctx, url, 500*time.Millisecond); err != nil {
		return fmt.Errorf("backend %q: %w", service, err)
	}
	log.Printf("[supervisor] %s is healthy", name)
	return nil
}

// create builds the RunSpec from config and creates (but doesn't start) the
// container. It refuses early with a clear message if the image isn't present,
// because "no such image" three layers deep is nobody's idea of fun.
func (s *Supervisor) create(ctx context.Context, service string, svc config.Service, name string) error {
	if ok, err := s.docker.ImageExists(ctx, svc.Image); err != nil {
		return fmt.Errorf("checking image %q: %w", svc.Image, err)
	} else if !ok {
		return fmt.Errorf("image %q for service %q not present locally — build or pull it first",
			svc.Image, service)
	}

	spec := docker.RunSpec{
		Name:      name,
		Image:     svc.Image,
		Port:      svc.Port,
		Env:       envSlice(svc, service),
		Volumes:   svc.Volumes,
		ShmSizeMB: svc.ShmSizeMB,
		Labels:    map[string]string{"ass.service": service},
	}
	// The whole reason ASS exists: hand the one GPU to whoever's resident — but
	// only when there is one. On this dev box GPU.Enabled is false, so no request
	// is made and the backend runs on CPU.
	if s.cfg.GPU.Enabled {
		dev := s.cfg.GPU.Device
		spec.GPUDevice = &dev
	}

	log.Printf("[supervisor] creating %s from %s (port %d, gpu=%v)", name, svc.Image, svc.Port, s.cfg.GPU.Enabled)
	if _, err := s.docker.Create(ctx, spec); err != nil {
		return err
	}
	return nil
}

// Stop stops a backend's container without removing it. Used when demoting a
// backend to the `stopped` state (freeing its RAM).
func (s *Supervisor) Stop(ctx context.Context, service string) error {
	svc, ok := s.cfg.Services[service]
	if !ok {
		return fmt.Errorf("unknown service %q", service)
	}
	name := svc.ContainerName(service)
	log.Printf("[supervisor] stopping %s", name)
	return s.docker.Stop(ctx, name, 30)
}

// envSlice turns the service's config into Docker's "KEY=value" env list. We
// always tell the backend which PORT and VERB to use so a single image can be
// pointed at any port; the user's own [services.x.env] is layered on top.
func envSlice(svc config.Service, _ string) []string {
	env := []string{
		"PORT=" + strconv.Itoa(svc.Port),
	}
	if svc.Verb != "" {
		env = append(env, "VERB="+svc.Verb)
	}
	for k, v := range svc.Env {
		env = append(env, k+"="+v)
	}
	return env
}
