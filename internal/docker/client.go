// Package docker is a deliberately small client for the Docker Engine API,
// spoken directly over the unix socket with net/http. No SDK.
//
// The official SDK (github.com/docker/docker) drags in containerd and roughly a
// hundred transitive packages to wrap the very same REST calls we make here in
// a few hundred lines. We need container CRUD — create, start, stop, inspect,
// remove — which is plain JSON in and JSON out. The SDK's real value is the
// hairy streaming/attach/build machinery we never touch. So we skip it (rule 5).
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// DefaultSocket is where the Engine listens on a normal Linux box.
const DefaultSocket = "/var/run/docker.sock"

// Client talks to one Docker daemon over its unix socket.
type Client struct {
	http   *http.Client
	apiVer string // negotiated, e.g. "v1.55"
	socket string
}

// New dials the socket and negotiates an API version the daemon actually
// supports, so we don't 400 ourselves by pinning something too new.
func New(socket string) (*Client, error) {
	if socket == "" {
		socket = DefaultSocket
	}
	// A transport that ignores the URL host and always dials the socket. The
	// "http://docker" in request URLs below is a placeholder the daemon ignores.
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
		Timeout: 30 * time.Second,
	}
	c := &Client{http: hc, socket: socket}

	// Version negotiation: ask the daemon what it speaks and use exactly that.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var v struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := c.do(ctx, http.MethodGet, "/version", nil, &v); err != nil {
		return nil, fmt.Errorf("docker: negotiating version over %s: %w", socket, err)
	}
	if v.APIVersion == "" {
		return nil, fmt.Errorf("docker: daemon reported no ApiVersion (is %s a Docker socket?)", socket)
	}
	c.apiVer = "v" + v.APIVersion
	return c, nil
}

// APIVersion reports the negotiated version, mostly for logging and sanity.
func (c *Client) APIVersion() string { return c.apiVer }

// do performs one Engine API call. The /version probe runs before apiVer is set
// (unversioned path); everything after is prefixed with the negotiated version.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	url := "http://docker"
	if c.apiVer != "" {
		url += "/" + c.apiVer
	}
	url += path

	var reqBody *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		reqBody = bytes.NewReader(raw)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return decodeAPIError(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding response from %s: %w", path, err)
		}
	}
	return nil
}
