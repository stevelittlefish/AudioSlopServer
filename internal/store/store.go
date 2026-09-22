// Package store is ASS's memory: a sqlite database of jobs and the artifacts
// they produced. It uses modernc.org/sqlite — pure Go, no CGO, no gcc, no
// "works on my machine" — matching the house stack.
//
// The store owns *metadata*. The artifact bytes live on disk (see package
// results); this table just remembers where and what.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Job states. A job marches queued -> running -> (succeeded | failed) and stops.
const (
	StateQueued    = "queued"
	StateRunning   = "running"
	StateSucceeded = "succeeded"
	StateFailed    = "failed"
)

// Job is one unit of work ASS is tracking on a client's behalf.
type Job struct {
	ID         string     `json:"job_id"`
	Service    string     `json:"service"`
	State      string     `json:"state"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Artifact is one output file a job produced, harvested into ASS's own store.
type Artifact struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`         // audio | stem | score | lyrics | metadata | other
	ContentType string `json:"content_type"` // real MIME
	Bytes       int64  `json:"bytes"`
	Path        string `json:"-"` // on-disk path in the results store; not for clients
}

// VRAMSample is one reading of a backend's GPU memory (MB), taken from its
// /v1/info after a job. PeakMB is the process high-water mark, so it's the
// inference peak even though we read it once the job is done.
type VRAMSample struct {
	AllocatedMB int `json:"allocated_mb"`
	ReservedMB  int `json:"reserved_mb"`
	PeakMB      int `json:"peak_mb"`
}

// VRAMSummary is the per-service rollup for the /v1/vram view: how much a service
// has actually used across all its samples. The Max* fields are the numbers that
// matter for setting vram_pinned_mb; Samples/LastAt show how much data backs them.
type VRAMSummary struct {
	Service        string    `json:"service"`
	Samples        int       `json:"samples"`
	MaxAllocatedMB int       `json:"max_allocated_mb"`
	MaxReservedMB  int       `json:"max_reserved_mb"`
	MaxPeakMB      int       `json:"max_peak_mb"`
	LastAt         time.Time `json:"last_at"`
}

// Store wraps the database. One per process.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the sqlite database at path and applies the
// schema. WAL mode keeps reads from blocking the one writer, which is plenty for
// a single-GPU box that will never be a write-storm.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating db dir %q: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite %q: %w", path, err)
	}
	// modernc's driver is fine with concurrent readers, but a single connection
	// sidesteps a whole genre of "database is locked" folklore for the writer.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database. Deferring this is the polite thing to do.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS jobs (
    id          TEXT PRIMARY KEY,
    service     TEXT NOT NULL,
    state       TEXT NOT NULL,
    error       TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,   -- unix nanos
    started_at  INTEGER,
    finished_at INTEGER
);
CREATE TABLE IF NOT EXISTS artifacts (
    job_id       TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    kind         TEXT NOT NULL,
    content_type TEXT NOT NULL,
    path         TEXT NOT NULL,
    bytes        INTEGER NOT NULL,
    PRIMARY KEY (job_id, name)
);
-- One row per VRAM reading ASS takes off a backend (from its /v1/info) right
-- after a job finishes, while the model is still resident. peak_mb is the real
-- prize: torch's high-water mark, so it's the inference peak even read post-job.
-- This is the raw data behind "how much does each service actually use" and the
-- calibration source for each service's vram_pinned_mb budget.
CREATE TABLE IF NOT EXISTS vram_samples (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    service      TEXT NOT NULL,
    job_id       TEXT,
    allocated_mb INTEGER NOT NULL,
    reserved_mb  INTEGER NOT NULL,
    peak_mb      INTEGER NOT NULL,
    at           INTEGER NOT NULL   -- unix nanos
);
CREATE INDEX IF NOT EXISTS vram_samples_service ON vram_samples(service);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("applying schema: %w", err)
	}
	return nil
}

// CreateJob inserts a fresh queued job and returns it, id and all.
func (s *Store) CreateJob(ctx context.Context, service string) (Job, error) {
	j := Job{
		ID:        newID(),
		Service:   service,
		State:     StateQueued,
		CreatedAt: time.Now(),
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (id, service, state, created_at) VALUES (?, ?, ?, ?)`,
		j.ID, j.Service, j.State, j.CreatedAt.UnixNano())
	if err != nil {
		return Job{}, fmt.Errorf("creating job: %w", err)
	}
	return j, nil
}

// MarkRunning stamps a job as running with a started_at of now.
func (s *Store) MarkRunning(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, started_at = ? WHERE id = ?`,
		StateRunning, time.Now().UnixNano(), id)
	return err
}

// MarkSucceeded stamps a job as succeeded with finished_at of now.
func (s *Store) MarkSucceeded(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, finished_at = ? WHERE id = ?`,
		StateSucceeded, time.Now().UnixNano(), id)
	return err
}

// MarkFailed records a failure and its reason.
func (s *Store) MarkFailed(ctx context.Context, id, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET state = ?, error = ?, finished_at = ? WHERE id = ?`,
		StateFailed, reason, time.Now().UnixNano(), id)
	return err
}

// AddArtifact records one harvested artifact for a job.
func (s *Store) AddArtifact(ctx context.Context, jobID string, a Artifact) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO artifacts (job_id, name, kind, content_type, path, bytes)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		jobID, a.Name, a.Kind, a.ContentType, a.Path, a.Bytes)
	if err != nil {
		return fmt.Errorf("adding artifact %q to job %q: %w", a.Name, jobID, err)
	}
	return nil
}

// GetJob returns the job and its artifacts. ErrNotFound if there's no such job.
func (s *Store) GetJob(ctx context.Context, id string) (Job, []Artifact, error) {
	var (
		j                 Job
		created           int64
		started, finished sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, service, state, error, created_at, started_at, finished_at
		 FROM jobs WHERE id = ?`, id).
		Scan(&j.ID, &j.Service, &j.State, &j.Error, &created, &started, &finished)
	if err == sql.ErrNoRows {
		return Job{}, nil, ErrNotFound
	}
	if err != nil {
		return Job{}, nil, err
	}
	j.CreatedAt = time.Unix(0, created)
	if started.Valid {
		t := time.Unix(0, started.Int64)
		j.StartedAt = &t
	}
	if finished.Valid {
		t := time.Unix(0, finished.Int64)
		j.FinishedAt = &t
	}

	arts, err := s.artifactsFor(ctx, id)
	if err != nil {
		return Job{}, nil, err
	}
	return j, arts, nil
}

// JobWithArtifacts is a job plus the artifacts it produced — the unit the jobs
// browser page renders one row per.
type JobWithArtifacts struct {
	Job
	Artifacts []Artifact `json:"artifacts"`
}

// ListJobs returns a page of jobs, newest first, each with its artifacts, plus
// the total job count so the UI can page. limit is clamped to something sane so
// a bad ?limit=999999 can't ask the box to marshal the whole history at once.
func (s *Store) ListJobs(ctx context.Context, limit, offset int) ([]JobWithArtifacts, int, error) {
	if limit <= 0 {
		limit = 25
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs`).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, service, state, error, created_at, started_at, finished_at
		 FROM jobs ORDER BY created_at DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []JobWithArtifacts{}
	for rows.Next() {
		var (
			j                 Job
			created           int64
			started, finished sql.NullInt64
		)
		if err := rows.Scan(&j.ID, &j.Service, &j.State, &j.Error, &created, &started, &finished); err != nil {
			return nil, 0, err
		}
		j.CreatedAt = time.Unix(0, created)
		if started.Valid {
			t := time.Unix(0, started.Int64)
			j.StartedAt = &t
		}
		if finished.Valid {
			t := time.Unix(0, finished.Int64)
			j.FinishedAt = &t
		}
		out = append(out, JobWithArtifacts{Job: j})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// Artifacts in a second pass: the rows cursor above holds the single
	// connection, so we can't query per-job while iterating it.
	for i := range out {
		arts, err := s.artifactsFor(ctx, out[i].ID)
		if err != nil {
			return nil, 0, err
		}
		out[i].Artifacts = arts
	}
	return out, total, nil
}

// GetArtifact returns one named artifact's metadata (including its on-disk path).
func (s *Store) GetArtifact(ctx context.Context, jobID, name string) (Artifact, error) {
	var a Artifact
	a.Name = name
	err := s.db.QueryRowContext(ctx,
		`SELECT kind, content_type, path, bytes FROM artifacts WHERE job_id = ? AND name = ?`,
		jobID, name).Scan(&a.Kind, &a.ContentType, &a.Path, &a.Bytes)
	if err == sql.ErrNoRows {
		return Artifact{}, ErrNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	return a, nil
}

func (s *Store) artifactsFor(ctx context.Context, jobID string) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, kind, content_type, path, bytes FROM artifacts WHERE job_id = ? ORDER BY name`,
		jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.Name, &a.Kind, &a.ContentType, &a.Path, &a.Bytes); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RecordVRAM stores one VRAM reading for a service (job_id optional — the job
// whose completion triggered the read). Best-effort telemetry: callers log and
// move on rather than failing a job over it.
func (s *Store) RecordVRAM(ctx context.Context, service, jobID string, v VRAMSample) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO vram_samples (service, job_id, allocated_mb, reserved_mb, peak_mb, at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		service, jobID, v.AllocatedMB, v.ReservedMB, v.PeakMB, time.Now().UnixNano())
	if err != nil {
		return fmt.Errorf("recording vram for %q: %w", service, err)
	}
	return nil
}

// VRAMSummary returns the per-service rollup of every VRAM sample recorded so
// far, ordered by service. The empty slice (not nil error) when nothing's been
// measured yet.
func (s *Store) VRAMSummary(ctx context.Context) ([]VRAMSummary, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT service, COUNT(*), MAX(allocated_mb), MAX(reserved_mb), MAX(peak_mb), MAX(at)
		 FROM vram_samples GROUP BY service ORDER BY service`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []VRAMSummary{}
	for rows.Next() {
		var v VRAMSummary
		var lastAt int64
		if err := rows.Scan(&v.Service, &v.Samples, &v.MaxAllocatedMB, &v.MaxReservedMB, &v.MaxPeakMB, &lastAt); err != nil {
			return nil, err
		}
		v.LastAt = time.Unix(0, lastAt)
		out = append(out, v)
	}
	return out, rows.Err()
}

// newID returns a short random hex id. Not a UUID, because we don't need one and
// a dependency for 16 random bytes would be embarrassing (rule 5).
func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand doesn't fail in practice; a dup id is astronomically unlikely
	return hex.EncodeToString(b[:])
}
