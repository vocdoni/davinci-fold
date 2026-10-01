package helpers

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Docker is a minimal Docker Engine API client over the daemon's unix
// socket. The live end-to-end run uses it to crash and restart the
// orchestrator container and to read its logs.
type Docker struct {
	http *http.Client
}

// NewDocker returns a client of the Docker daemon at DOCKER_HOST, if it is a
// unix socket, or else at /var/run/docker.sock.
func NewDocker() *Docker {
	socket := "/var/run/docker.sock"
	if h, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://"); ok {
		socket = h
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	return &Docker{http: &http.Client{Transport: transport, Timeout: 2 * time.Minute}}
}

// do sends a request to the Engine API and returns the response body. 304
// (already started or stopped) counts as success.
func (d *Docker) do(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		return nil, fmt.Errorf("docker %s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(body))
	}
	return body, nil
}

func containerPath(name, action string) string {
	return "/containers/" + url.PathEscape(name) + "/" + action
}

// Kill sends SIGKILL to a container and waits until it is no longer running.
func (d *Docker) Kill(ctx context.Context, name string) error {
	if _, err := d.do(ctx, http.MethodPost, containerPath(name, "kill?signal=SIGKILL")); err != nil {
		return err
	}
	_, err := d.do(ctx, http.MethodPost, containerPath(name, "wait?condition=not-running"))
	return err
}

// Start starts a container.
func (d *Docker) Start(ctx context.Context, name string) error {
	_, err := d.do(ctx, http.MethodPost, containerPath(name, "start"))
	return err
}

// Stop stops a container, killing it if it has not exited after timeout.
func (d *Docker) Stop(ctx context.Context, name string, timeout time.Duration) error {
	_, err := d.do(ctx, http.MethodPost, containerPath(name, fmt.Sprintf("stop?t=%d", int(timeout.Seconds()))))
	return err
}

// Logs returns the stdout and stderr a container without a TTY wrote, across
// its restarts.
func (d *Docker) Logs(ctx context.Context, name string) ([]byte, error) {
	raw, err := d.do(ctx, http.MethodGet, containerPath(name, "logs?stdout=1&stderr=1"))
	if err != nil {
		return nil, err
	}
	// Without a TTY both streams are multiplexed in frames: an 8-byte header
	// (stream, three zero bytes, big-endian payload size), then the payload.
	var out []byte
	for len(raw) > 0 {
		if len(raw) < 8 {
			return nil, errors.New("docker logs: truncated frame header")
		}
		n := binary.BigEndian.Uint32(raw[4:8])
		raw = raw[8:]
		if uint64(n) > uint64(len(raw)) {
			return nil, errors.New("docker logs: truncated frame")
		}
		out = append(out, raw[:n]...)
		raw = raw[n:]
	}
	return out, nil
}
