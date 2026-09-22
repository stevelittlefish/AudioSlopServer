// Package api is ASS's front door: the unified HTTP surface clients talk to.
// It normalizes requests into the engine and reads job/artifact state back out
// of the store. It does no orchestration itself — that's the engine's job.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/stevelittlefish/AudioSlopServer/internal/arbiter"
	"github.com/stevelittlefish/AudioSlopServer/internal/config"
	"github.com/stevelittlefish/AudioSlopServer/internal/engine"
	"github.com/stevelittlefish/AudioSlopServer/internal/store"
	"github.com/stevelittlefish/AudioSlopServer/internal/web"
)

// API holds the dependencies the handlers need.
type API struct {
	cfg     *config.Config
	engine  *engine.Engine
	store   *store.Store
	arbiter *arbiter.Arbiter
}

// New builds the API.
func New(cfg *config.Config, eng *engine.Engine, st *store.Store, arb *arbiter.Arbiter) *API {
	return &API{cfg: cfg, engine: eng, store: st, arbiter: arb}
}

// Handler returns the fully-routed http.Handler. Uses Go's method+wildcard
// patterns, so no router dependency (rule 5).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("GET /v1/backends", a.handleBackends)
	mux.HandleFunc("GET /v1/vram", a.handleVRAM)
	// Operator controls (Slice 3, Part A): drive a backend's residency by hand.
	// All route through the arbiter, so leases still protect in-flight jobs.
	// Gated behind [web] (on by default) — these are real "free/kill the GPU"
	// buttons and there's no auth yet, so a box that wants API-only turns them off.
	if a.cfg.WebEnabled() {
		mux.HandleFunc("POST /v1/backends/unload-all", a.handleUnloadAll)
		mux.HandleFunc("POST /v1/backends/preload-all", a.handlePreloadAll)
		mux.HandleFunc("POST /v1/backends/{service}/load", a.handleLoadBackend)
		mux.HandleFunc("POST /v1/backends/{service}/park", a.handleParkBackend)
		mux.HandleFunc("POST /v1/backends/{service}/unpark", a.handleUnparkBackend)
		mux.HandleFunc("POST /v1/backends/{service}/stop", a.handleStopBackend)
		web.Register(mux, a.cfg) // the admin console pages that drive the above
	} else {
		log.Printf("[api] web console disabled (web.enabled = false) — operator controls not served")
	}
	// Read-only backend introspection: forward a backend's own /v1/info (model,
	// capabilities, the Stable Audio LoRA list). ASS holds no CUDA context and
	// can't read a card, so clients that want what the backend knows ask here and
	// ASS proxies it. Not gated behind [web] — it reads, it can't free or kill the
	// GPU — but it will make the backend resident to answer.
	mux.HandleFunc("GET /v1/backends/{service}/info", a.handleBackendInfo)
	mux.HandleFunc("POST /v1/{service}/jobs", a.handleSubmit)
	mux.HandleFunc("GET /v1/jobs", a.handleListJobs)
	mux.HandleFunc("GET /v1/jobs/{id}", a.handleJob)
	mux.HandleFunc("GET /v1/jobs/{id}/result", a.handleResultList)
	mux.HandleFunc("GET /v1/jobs/{id}/result/{name}", a.handleResultFile)
	return logging(mux)
}

// --- handlers -------------------------------------------------------------

func (a *API) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "ASS"})
}

// handleSubmit accepts a job for a service, buffers the body, and hands it to
// the engine. Returns 202 with the ASS job id — the work continues async.
func (a *API) handleSubmit(w http.ResponseWriter, r *http.Request) {
	service := r.PathValue("service")
	if _, ok := a.cfg.Services[service]; !ok {
		writeErr(w, http.StatusNotFound, "unknown service %q", service)
		return
	}
	// Buffer the body so the request can return before the job finishes. Large
	// uploads streaming to a temp file is a later refinement (see TODO).
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "reading body: %v", err)
		return
	}
	j, err := a.engine.Submit(r.Context(), service, body, r.Header.Get("Content-Type"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "submitting job: %v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": j.ID})
}

// handleBackendInfo forwards a backend's /v1/info to the caller, making the
// backend resident first if it isn't. The body is passed through verbatim (it's
// already JSON), so a client sees exactly what the backend reports.
func (a *API) handleBackendInfo(w http.ResponseWriter, r *http.Request) {
	service := r.PathValue("service")
	if _, ok := a.cfg.Services[service]; !ok {
		writeErr(w, http.StatusNotFound, "unknown service %q", service)
		return
	}
	raw, err := a.engine.BackendInfo(r.Context(), service)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "reading %s info: %v", service, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// jobView is the client-facing job shape: the stored job plus its artifacts.
type jobView struct {
	store.Job
	Artifacts []store.Artifact `json:"artifacts"`
}

// handleListJobs returns a page of jobs (newest first) with their artifacts, for
// the jobs browser page. ?limit= and ?offset= page through the history; total
// rides along so the UI can render "showing X–Y of Z".
func (a *API) handleListJobs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 25
	}
	jobs, total, err := a.store.ListJobs(r.Context(), limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "listing jobs: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs": jobs, "total": total, "limit": limit, "offset": offset,
	})
}

func (a *API) handleJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, arts, err := a.store.GetJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such job %q", id)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reading job: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, jobView{Job: j, Artifacts: arts})
}

// handleResultList returns the artifact list, or — as a convenience — streams
// the single artifact directly when a job has exactly one.
func (a *API) handleResultList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, arts, err := a.store.GetJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such job %q", id)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reading job: %v", err)
		return
	}
	if len(arts) == 1 {
		a.serveArtifact(w, r, id, arts[0].Name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifacts": arts})
}

func (a *API) handleResultFile(w http.ResponseWriter, r *http.Request) {
	a.serveArtifact(w, r, r.PathValue("id"), r.PathValue("name"))
}

// serveArtifact streams one artifact's bytes from ASS's own results store —
// available even after the backend that made it has been evicted.
func (a *API) serveArtifact(w http.ResponseWriter, r *http.Request, jobID, name string) {
	art, err := a.store.GetArtifact(r.Context(), jobID, name)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such artifact %q for job %q", name, jobID)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "reading artifact: %v", err)
		return
	}
	f, err := os.Open(art.Path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "opening artifact bytes: %v", err)
		return
	}
	defer f.Close()

	if art.ContentType != "" {
		w.Header().Set("Content-Type", art.ContentType)
	}
	w.Header().Set("Content-Disposition", "inline; filename="+strconv.Quote(name))

	var mod time.Time
	if fi, statErr := f.Stat(); statErr == nil {
		mod = fi.ModTime()
	}
	http.ServeContent(w, r, name, mod, f)
}

func (a *API) handleBackends(w http.ResponseWriter, r *http.Request) {
	type backendView struct {
		Name      string `json:"name"`
		Image     string `json:"image"`
		Verb      string `json:"verb"`
		Evict     string `json:"evict"`
		Residency string `json:"residency"` // pinned | parked | stopped
		Leases    int    `json:"leases"`    // in-flight jobs holding this backend
		LastUsed  string `json:"last_used,omitempty"`
		// VRAMMB: what this backend costs the card RIGHT NOW per its config —
		// vram_pinned_mb while pinned, the vram_parked_mb context tax while
		// parked, 0 while stopped. Summed across backends it's the arbiter's
		// own idea of "used", which is what it budgets against.
		VRAMMB int `json:"vram_mb"`
		// VRAMPinnedMB is the service's configured pinned reservation, and
		// OverBudget flags that this single reservation is larger than the whole
		// gpu.vram_budget_mb — so it can't be budgeted to fit. ASS still loads it
		// (evicting everything and hoping the real usage stays under the card), but
		// the UI should warn: a heavy job on this backend may OOM. Static, from
		// config — shown even while the backend is stopped.
		VRAMPinnedMB int  `json:"vram_pinned_mb"`
		OverBudget   bool `json:"over_budget"`
		// Phase / PhaseSince: what's happening to this backend mid-swap, so the
		// admin console can show "cold-starting… 0:42" instead of a dead button.
		Phase      string `json:"phase,omitempty"`
		PhaseSince string `json:"phase_since,omitempty"`
	}
	snap := a.arbiter.Snapshot()
	budgeted := a.cfg.GPU.VRAMBudgetMB > 0
	var out []backendView
	for name, svc := range a.cfg.Services {
		bv := backendView{
			Name: name, Image: svc.Image, Verb: svc.Verb, Evict: string(svc.Evict),
			Residency:    "stopped", // default: never touched = not resident
			VRAMPinnedMB: svc.VRAMPinnedMB,
			OverBudget:   budgeted && svc.VRAMPinnedMB > a.cfg.GPU.VRAMBudgetMB,
		}
		if s, ok := snap[name]; ok {
			bv.Residency, bv.Leases, bv.LastUsed = s.Residency, s.Leases, s.LastUsed
			bv.Phase, bv.PhaseSince = s.Phase, s.PhaseSince
			bv.VRAMMB = s.VRAMMB
		}
		out = append(out, bv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": out})
}

// handleVRAM serves the per-service VRAM rollup: how much each backend has
// actually used, from the samples ASS records after every job. The budgeted
// figure from config rides along so you can eyeball measured-vs-budget in one
// place — the point being to catch a service whose real peak is creeping toward
// (or past) its vram_pinned_mb.
func (a *API) handleVRAM(w http.ResponseWriter, r *http.Request) {
	summaries, err := a.store.VRAMSummary(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	type row struct {
		store.VRAMSummary
		BudgetPinnedMB int `json:"budget_pinned_mb"`
		BudgetParkedMB int `json:"budget_parked_mb"`
	}
	out := []row{}
	for _, s := range summaries {
		svc := a.cfg.Services[s.Service]
		out = append(out, row{VRAMSummary: s, BudgetPinnedMB: svc.VRAMPinnedMB, BudgetParkedMB: svc.VRAMParkedMB})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"vram_budget_mb": a.cfg.GPU.VRAMBudgetMB,
		"services":       out,
	})
}

// --- operator controls ----------------------------------------------------

// backendOp is the shape of the arbiter's park/unpark/stop methods.
type backendOp func(ctx context.Context, service string) error

func (a *API) handleLoadBackend(w http.ResponseWriter, r *http.Request) {
	a.doBackendOp(w, r, a.arbiter.Warm)
}
func (a *API) handleParkBackend(w http.ResponseWriter, r *http.Request) {
	a.doBackendOp(w, r, a.arbiter.Park)
}
func (a *API) handleUnparkBackend(w http.ResponseWriter, r *http.Request) {
	a.doBackendOp(w, r, a.arbiter.Unpark)
}
func (a *API) handleStopBackend(w http.ResponseWriter, r *http.Request) {
	a.doBackendOp(w, r, a.arbiter.Stop)
}

// doBackendOp runs one operator action against a named backend and reports the
// resulting residency, so the caller (or the admin panel) sees the new state.
func (a *API) doBackendOp(w http.ResponseWriter, r *http.Request, op backendOp) {
	service := r.PathValue("service")
	if _, ok := a.cfg.Services[service]; !ok {
		writeErr(w, http.StatusNotFound, "unknown service %q", service)
		return
	}
	if err := op(r.Context(), service); err != nil {
		a.writeOpErr(w, err)
		return
	}
	resp := map[string]any{"status": "ok", "service": service}
	if s, ok := a.arbiter.Snapshot()[service]; ok {
		resp["residency"] = s.Residency
	} else {
		resp["residency"] = "stopped"
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *API) handleUnloadAll(w http.ResponseWriter, r *http.Request) {
	res, err := a.arbiter.UnloadAll(r.Context())
	if err != nil {
		// A partial failure still carries what did get unloaded.
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(), "unloaded": res.Unloaded, "skipped": res.Skipped,
		})
		return
	}
	if res.Unloaded == nil {
		res.Unloaded = []string{} // render [] not null for an empty result
	}
	writeJSON(w, http.StatusOK, res)
}

// handlePreloadAll: cold-start and park every parkable backend that fits, so
// the day's first requests unpark instead of cold-starting. Slow — it's N model
// loads in a row — so the client should expect to wait a while for the body.
func (a *API) handlePreloadAll(w http.ResponseWriter, r *http.Request) {
	res, err := a.arbiter.PreloadAll(r.Context())
	if res.Preloaded == nil {
		res.Preloaded = []string{} // [] not null
	}
	if err != nil {
		// Partial success still carries what did get preloaded.
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(), "preloaded": res.Preloaded, "skipped": res.Skipped,
		})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// writeOpErr maps the arbiter's typed operator errors onto HTTP status codes.
func (a *API) writeOpErr(w http.ResponseWriter, err error) {
	var leaseErr *arbiter.LeaseHeldError
	switch {
	case errors.As(err, &leaseErr):
		writeErr(w, http.StatusConflict, "%v", err)
	case errors.Is(err, arbiter.ErrNotResident),
		errors.Is(err, arbiter.ErrNotParked),
		errors.Is(err, arbiter.ErrGPUBusy):
		writeErr(w, http.StatusConflict, "%v", err)
	case errors.Is(err, arbiter.ErrParkUnsupported):
		writeErr(w, http.StatusBadRequest, "%v", err)
	case errors.Is(err, arbiter.ErrUnknownService):
		writeErr(w, http.StatusNotFound, "%v", err)
	default:
		writeErr(w, http.StatusInternalServerError, "%v", err)
	}
}

// --- helpers --------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// logging is the world's most modest middleware: one line per request, because
// when it breaks at 2am you'll want it (rule 2 says to be witty; 2am says to be
// useful; this is a truce).
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[api] %s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
