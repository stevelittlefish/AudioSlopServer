package docker

import (
	"errors"
	"strings"
)

// The Engine API signals most of what we care about with HTTP status codes and
// a human message. These helpers classify the ones the supervisor treats as
// "not really a problem" so callers stay readable.

func asAPIError(err error) (*apiError, bool) {
	var e *apiError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// IsNotFound reports a 404 — the container (or image) doesn't exist. For the
// supervisor this is information, not failure.
func IsNotFound(err error) bool {
	e, ok := asAPIError(err)
	return ok && e.status == 404
}

// IsAlreadyStarted reports a start call on a container that's already running.
// The daemon usually answers 304 (which we don't treat as an error at all), but
// some versions phrase it in the message, so we check both.
func IsAlreadyStarted(err error) bool {
	e, ok := asAPIError(err)
	if !ok {
		return false
	}
	return e.status == 304 || strings.Contains(strings.ToLower(e.Message), "already started")
}

// IsAlreadyStopped reports a stop call on a container that isn't running.
func IsAlreadyStopped(err error) bool {
	e, ok := asAPIError(err)
	if !ok {
		return false
	}
	msg := strings.ToLower(e.Message)
	return e.status == 304 ||
		strings.Contains(msg, "is not running") ||
		strings.Contains(msg, "already stopped")
}
