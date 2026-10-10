// Package docker is a small client for the Docker Engine API over the local
// unix socket. It covers only what Basalt needs (images, containers,
// networks, logs) and pins one API version, so daemon upgrades cannot change
// the behaviour of the calls underneath us.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// APIVersion is the Engine API version every request is made with
// (Docker Engine 25.0 and later).
const APIVersion = "1.44"

const maxJSONResponse = 16 << 20

var (
	ErrNotFound = errors.New("docker: not found")
	ErrConflict = errors.New("docker: conflict")
)

// Error is a failure reported by the daemon.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("docker: %s (HTTP %d)", e.Message, e.Status) }

func (e *Error) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrConflict:
		return e.Status == http.StatusConflict
	}
	return false
}

type Client struct {
	socket string
	hc     *http.Client
}

// New returns a client for the daemon listening on socket. It does not
// connect; use Version to check the daemon.
func New(socket string) *Client {
	return &Client{socket: socket, hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:          4,
		IdleConnTimeout:       time.Minute,
		ResponseHeaderTimeout: 2 * time.Minute, // long operations stream their headers early
	}}}
}

// Available reports whether a socket exists at path.
func Available(socket string) bool {
	fi, err := os.Stat(socket)
	return err == nil && fi.Mode()&os.ModeSocket != 0
}

// do sends a request to /v<APIVersion><path>. body, if not nil, is sent as
// JSON. Non-2xx responses (other than 304) become *Error.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	return c.doHeader(ctx, method, path, query, body, nil)
}

// doHeader is do with extra request headers (e.g. registry credentials).
func (c *Client) doHeader(ctx context.Context, method, path string, query url.Values, body any, header http.Header) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	u := "http://docker/v" + APIVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	return resp, nil
}

func readError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var m struct {
		Message string `json:"message"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		msg = m.Message
	}
	if msg == "" {
		msg = resp.Status
	}
	return &Error{Status: resp.StatusCode, Message: msg}
}

// call performs a request and decodes a JSON response into out (may be nil).
func (c *Client) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONResponse))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONResponse)).Decode(out); err != nil {
		return fmt.Errorf("docker: decode %s %s: %w", method, path, err)
	}
	return nil
}

// --- system ---

type Version struct {
	Version       string `json:"Version"`
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	OS            string `json:"Os"`
	Arch          string `json:"Arch"`
}

// Version returns the daemon version and fails if the daemon cannot speak
// APIVersion.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	if err := c.call(ctx, http.MethodGet, "/version", nil, nil, &v); err != nil {
		return v, err
	}
	if !versionAtLeast(v.APIVersion, APIVersion) {
		return v, fmt.Errorf("docker %s (API %s) is too old; API %s or newer is required", v.Version, v.APIVersion, APIVersion)
	}
	return v, nil
}

// versionAtLeast compares "major.minor" API versions.
func versionAtLeast(have, want string) bool {
	parse := func(s string) (int, int) {
		maj, min, _ := strings.Cut(s, ".")
		a, _ := strconv.Atoi(maj)
		b, _ := strconv.Atoi(min)
		return a, b
	}
	ha, hb := parse(have)
	wa, wb := parse(want)
	return ha > wa || ha == wa && hb >= wb
}

// labelFilter encodes a filters= query value matching all given labels.
func labelFilter(labels map[string]string) string {
	var l []string
	for k, v := range labels {
		if v == "" {
			l = append(l, k)
		} else {
			l = append(l, k+"="+v)
		}
	}
	b, _ := json.Marshal(map[string][]string{"label": l})
	return string(b)
}
