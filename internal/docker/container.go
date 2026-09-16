package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// State is the slice of container status ASS actually cares about.
type State struct {
	Exists  bool
	Running bool
	Status  string // "running", "exited", "created", ... (empty if !Exists)
	ID      string
}

// RunSpec describes a container to create. Deliberately minimal — the knobs a
// backend needs and nothing more. GPUDevice is a pointer so "no GPU" (this
// dev box, or a peasant laptop) is distinct from "GPU 0".
type RunSpec struct {
	Name      string
	Image     string
	Cmd       []string          // override the image's default command (usually empty)
	Env       []string          // "KEY=value"
	Port      int               // container port, published to the same host port
	GPUDevice *int              // nil = no GPU request at all
	Volumes   []string          // "host:container[:ro]"
	ShmSizeMB int               // 0 = daemon default
	Labels    map[string]string // stamped so ASS can recognize its own containers
}

// Inspect returns the container's state. A missing container is not an error —
// it's just Exists:false, which is exactly what the supervisor wants to know.
func (c *Client) Inspect(ctx context.Context, name string) (State, error) {
	var raw struct {
		ID    string `json:"Id"`
		State struct {
			Running bool   `json:"Running"`
			Status  string `json:"Status"`
		} `json:"State"`
	}
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &raw)
	if err != nil {
		if IsNotFound(err) {
			return State{Exists: false}, nil
		}
		return State{}, err
	}
	return State{
		Exists:  true,
		Running: raw.State.Running,
		Status:  raw.State.Status,
		ID:      raw.ID,
	}, nil
}

// Create creates (but does not start) a container from the spec. Returns the
// new container's ID.
func (c *Client) Create(ctx context.Context, spec RunSpec) (string, error) {
	portStr := strconv.Itoa(spec.Port) + "/tcp"

	type portBinding struct {
		HostPort string `json:"HostPort"`
	}
	type deviceRequest struct {
		Driver       string     `json:"Driver"`
		DeviceIDs    []string   `json:"DeviceIDs"`
		Capabilities [][]string `json:"Capabilities"`
	}

	hostConfig := map[string]any{
		"PortBindings": map[string][]portBinding{
			portStr: {{HostPort: strconv.Itoa(spec.Port)}},
		},
		"RestartPolicy": map[string]any{"Name": "no"}, // ASS owns the lifecycle, not Docker
	}
	if len(spec.Volumes) > 0 {
		hostConfig["Binds"] = spec.Volumes
	}
	if spec.ShmSizeMB > 0 {
		hostConfig["ShmSize"] = int64(spec.ShmSizeMB) * 1024 * 1024
	}
	// The whole point of ASS: request the one GPU, but only when there is one.
	if spec.GPUDevice != nil {
		hostConfig["DeviceRequests"] = []deviceRequest{{
			Driver:       "nvidia",
			DeviceIDs:    []string{strconv.Itoa(*spec.GPUDevice)},
			Capabilities: [][]string{{"gpu"}},
		}}
	}

	body := map[string]any{
		"Image":        spec.Image,
		"Env":          spec.Env,
		"ExposedPorts": map[string]struct{}{portStr: {}},
		"HostConfig":   hostConfig,
	}
	if len(spec.Cmd) > 0 {
		body["Cmd"] = spec.Cmd
	}
	if len(spec.Labels) > 0 {
		body["Labels"] = spec.Labels
	}

	var out struct {
		ID string `json:"Id"`
	}
	q := url.Values{"name": {spec.Name}}
	if err := c.do(ctx, http.MethodPost, "/containers/create?"+q.Encode(), body, &out); err != nil {
		return "", fmt.Errorf("creating container %q from %q: %w", spec.Name, spec.Image, err)
	}
	return out.ID, nil
}

// Start starts an existing container. Starting an already-running container is
// harmless (the daemon 304s), so callers don't have to check first.
func (c *Client) Start(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", nil, nil)
	if err != nil && !IsAlreadyStarted(err) {
		return fmt.Errorf("starting container %q: %w", name, err)
	}
	return nil
}

// Stop stops a running container, giving it timeoutSec to shut down cleanly
// before the daemon reaches for SIGKILL.
func (c *Client) Stop(ctx context.Context, name string, timeoutSec int) error {
	q := url.Values{"t": {strconv.Itoa(timeoutSec)}}
	err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/stop?"+q.Encode(), nil, nil)
	if err != nil && !IsNotFound(err) && !IsAlreadyStopped(err) {
		return fmt.Errorf("stopping container %q: %w", name, err)
	}
	return nil
}

// Remove deletes a container. force also kills it if it's still running.
func (c *Client) Remove(ctx context.Context, name string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "true")
	}
	err := c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name)+"?"+q.Encode(), nil, nil)
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("removing container %q: %w", name, err)
	}
	return nil
}

// PullNeeded reports whether the image is absent locally (so a caller can decide
// to pull). We don't pull automatically — on the real server images are built
// locally, and surprise multi-GB downloads are rude.
func (c *Client) ImageExists(ctx context.Context, image string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil, nil)
	if err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// apiError is the daemon's JSON error shape: {"message": "..."}.
type apiError struct {
	status  int
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("docker api %d: %s", e.status, e.Message)
}

func decodeAPIError(resp *http.Response) error {
	e := &apiError{status: resp.StatusCode}
	// Best effort: if the body isn't the expected JSON, we still return status.
	_ = json.NewDecoder(resp.Body).Decode(e)
	return e
}
