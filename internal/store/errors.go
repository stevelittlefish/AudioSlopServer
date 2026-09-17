package store

import "errors"

// ErrNotFound is returned when a job or artifact doesn't exist. Callers turn
// this into a 404 rather than a 500 — a missing job is a client mistake, not a
// server meltdown.
var ErrNotFound = errors.New("not found")
