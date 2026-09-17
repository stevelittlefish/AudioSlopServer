// Package backend is an HTTP client for a single audio backend — the other side
// of the contract in CLAUDE.md. ASS uses it to submit jobs, poll their status,
// download artifacts, and park/unpark. It knows nothing about Docker or the
// arbiter; it just speaks the backend's HTTP.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Client talks to one backend at a base URL like http://localhost:5336.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client for the given base URL.
func New(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: &http.Client{}}
}

// Artifact is one output the backend reports for a job.
type Artifact struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
}

// Job is the backend's view of a job: its state and (once done) its artifacts.
type Job struct {
	JobID     string     `json:"job_id"`
	State     string     `json:"state"`
	Error     string     `json:"error"`
	Artifacts []Artifact `json:"artifacts"`
}

// Submit posts a job to the backend's /v1/<verb> endpoint, forwarding the
// caller's body and content type verbatim (audio upload, JSON params, whatever
// the service wants). Returns the backend's job id.
func (c *Client) Submit(ctx context.Context, verb string, body io.Reader, contentType string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/"+verb, body)
	if err != nil {
		return "", err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("backend submit %s: %s", verb, statusLine(resp))
	}
	var out struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decoding submit response: %w", err)
	}
	if out.JobID == "" {
		return "", fmt.Errorf("backend returned empty job_id")
	}
	return out.JobID, nil
}

// Status fetches the backend's current view of a job.
func (c *Client) Status(ctx context.Context, jobID string) (Job, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/jobs/"+jobID, nil)
	if err != nil {
		return Job{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Job{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return Job{}, fmt.Errorf("backend status %s: %s", jobID, statusLine(resp))
	}
	var j Job
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return Job{}, fmt.Errorf("decoding status: %w", err)
	}
	return j, nil
}

// Download opens one artifact's bytes. The caller must Close the returned reader.
func (c *Client) Download(ctx context.Context, jobID, name string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/v1/jobs/"+jobID+"/result/"+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		body := statusLine(resp)
		resp.Body.Close()
		return nil, fmt.Errorf("backend download %s/%s: %s", jobID, name, body)
	}
	return resp.Body, nil
}

// Park and Unpark toggle the backend's model between VRAM and CPU RAM. Backends
// that don't implement them simply won't be configured with evict = "park".
func (c *Client) Park(ctx context.Context) error   { return c.simplePost(ctx, "/park") }
func (c *Client) Unpark(ctx context.Context) error { return c.simplePost(ctx, "/unpark") }

// Health reports whether the backend answers its readiness probe. (Mostly the
// supervisor's job, but handy here too.)
func (c *Client) Health(ctx context.Context) error { return c.simpleGet(ctx, "/health") }

func (c *Client) simplePost(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	return c.doVoid(req, path)
}

func (c *Client) simpleGet(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	return c.doVoid(req, path)
}

func (c *Client) doVoid(req *http.Request, path string) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("backend %s: %s", path, resp.Status)
	}
	return nil
}

func statusLine(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if len(b) == 0 {
		return resp.Status
	}
	return resp.Status + ": " + string(b)
}
