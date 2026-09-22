// Package results is the on-disk home for harvested artifact bytes. The sqlite
// store remembers metadata and points here; this package owns the actual files.
//
// Layout: <root>/<job_id>/<artifact_name>. One directory per job keeps things
// tidy and makes "delete a job's outputs" a single RemoveAll.
package results

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Store is a rooted directory of job outputs.
type Store struct {
	root string
}

// New ensures root exists and returns a Store rooted there.
func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("creating results dir %q: %w", root, err)
	}
	return &Store{root: root}, nil
}

// Save streams r into <root>/<jobID>/<name> and returns the path and byte count.
// The artifact name is untrusted (it ultimately comes from a backend's job
// status), so we refuse anything that could climb out of the job directory.
func (s *Store) Save(jobID, name string, r io.Reader) (string, int64, error) {
	safe, err := safeName(name)
	if err != nil {
		return "", 0, err
	}
	dir := filepath.Join(s.root, safeName2(jobID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("creating job dir: %w", err)
	}
	path := filepath.Join(dir, safe)

	f, err := os.Create(path)
	if err != nil {
		return "", 0, fmt.Errorf("creating %q: %w", path, err)
	}
	n, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		return "", 0, fmt.Errorf("writing %q: %w", path, copyErr)
	}
	if closeErr != nil {
		return "", 0, fmt.Errorf("closing %q: %w", path, closeErr)
	}
	return path, n, nil
}

// RemoveJob deletes a job's entire output directory (<root>/<jobID>) and every
// artifact in it. Idempotent: a job with nothing on disk is not an error, which
// is exactly what the reaper wants — it's cleaning up, not auditing. The job id
// is sanitized the same way Save does, so we only ever RemoveAll inside root.
func (s *Store) RemoveJob(jobID string) error {
	dir := filepath.Join(s.root, safeName2(jobID))
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("removing job dir %q: %w", dir, err)
	}
	return nil
}

// safeName rejects artifact names that contain path separators or traversal, so
// a malicious or buggy backend can't write "../../etc/anything".
func safeName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty artifact name")
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." || strings.Contains(name, "..") {
		return "", fmt.Errorf("unsafe artifact name %q", name)
	}
	return name, nil
}

// safeName2 sanitizes the job id for use as a directory. Our ids are hex, but
// defense in depth is cheap: strip anything that isn't a tidy identifier char.
func safeName2(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, id)
}
