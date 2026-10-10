package docker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ContainerConfig is the body of a container create request (the subset
// Basalt sets; field names follow the Engine API).
type ContainerConfig struct {
	Image            string              `json:"Image"`
	Cmd              []string            `json:"Cmd,omitempty"`
	Entrypoint       []string            `json:"Entrypoint,omitempty"`
	Env              []string            `json:"Env,omitempty"`
	User             string              `json:"User,omitempty"`
	WorkingDir       string              `json:"WorkingDir,omitempty"`
	Labels           map[string]string   `json:"Labels,omitempty"`
	ExposedPorts     map[string]struct{} `json:"ExposedPorts,omitempty"`
	Healthcheck      *HealthConfig       `json:"Healthcheck,omitempty"`
	StopTimeout      *int                `json:"StopTimeout,omitempty"`
	HostConfig       *HostConfig         `json:"HostConfig,omitempty"`
	NetworkingConfig *NetworkingConfig   `json:"NetworkingConfig,omitempty"`
}

// HealthConfig durations are nanoseconds, as in the Engine API.
type HealthConfig struct {
	Test        []string      `json:"Test,omitempty"`
	Interval    time.Duration `json:"Interval,omitempty"`
	Timeout     time.Duration `json:"Timeout,omitempty"`
	StartPeriod time.Duration `json:"StartPeriod,omitempty"`
	Retries     int           `json:"Retries,omitempty"`
}

type HostConfig struct {
	NetworkMode    string            `json:"NetworkMode,omitempty"`
	RestartPolicy  RestartPolicy     `json:"RestartPolicy"`
	CapDrop        []string          `json:"CapDrop,omitempty"`
	CapAdd         []string          `json:"CapAdd,omitempty"`
	SecurityOpt    []string          `json:"SecurityOpt,omitempty"`
	ReadonlyRootfs bool              `json:"ReadonlyRootfs,omitempty"`
	Init           *bool             `json:"Init,omitempty"`
	Memory         int64             `json:"Memory,omitempty"`   // bytes
	NanoCPUs       int64             `json:"NanoCpus,omitempty"` // 1e9 = one CPU
	PidsLimit      *int64            `json:"PidsLimit,omitempty"`
	Tmpfs          map[string]string `json:"Tmpfs,omitempty"`
	Mounts         []Mount           `json:"Mounts,omitempty"`
	LogConfig      *LogConfig        `json:"LogConfig,omitempty"`
	// PortBindings maps "port/proto" to host addresses. Basalt publishes
	// ports only on development machines whose container IPs the host
	// cannot reach.
	PortBindings map[string][]PortBinding `json:"PortBindings,omitempty"`
}

type PortBinding struct {
	HostIP   string `json:"HostIp,omitempty"`
	HostPort string `json:"HostPort,omitempty"` // "" picks a free port
}

type RestartPolicy struct {
	Name              string `json:"Name,omitempty"` // "", "no", "always", "unless-stopped", "on-failure"
	MaximumRetryCount int    `json:"MaximumRetryCount,omitempty"`
}

type Mount struct {
	Type     string `json:"Type"` // "volume", "bind", "tmpfs"
	Source   string `json:"Source,omitempty"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

type LogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config,omitempty"`
}

type NetworkingConfig struct {
	EndpointsConfig map[string]*EndpointSettings `json:"EndpointsConfig,omitempty"`
}

type EndpointSettings struct {
	Aliases   []string `json:"Aliases,omitempty"`
	IPAddress string   `json:"IPAddress,omitempty"`
}

// ContainerCreate creates (but does not start) a container and returns its ID.
func (c *Client) ContainerCreate(ctx context.Context, name string, cfg ContainerConfig) (string, error) {
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	var resp struct {
		ID string `json:"Id"`
	}
	if err := c.call(ctx, http.MethodPost, "/containers/create", q, cfg, &resp); err != nil {
		return "", err
	}
	return resp.ID, nil
}

// ContainerStart starts a container; starting a running one is not an error.
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

// ContainerStop sends SIGTERM and, after timeout, SIGKILL. Stopping a
// stopped container is not an error.
func (c *Client) ContainerStop(ctx context.Context, id string, timeout time.Duration) error {
	q := url.Values{"t": {strconv.Itoa(int(timeout.Seconds()))}}
	return c.call(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", q, nil, nil)
}

// ContainerRemove deletes a container (force kills a running one) and its
// anonymous volumes. A missing container is not an error.
func (c *Client) ContainerRemove(ctx context.Context, id string, force bool) error {
	q := url.Values{"v": {"1"}, "force": {strconv.FormatBool(force)}}
	err := c.call(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), q, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

type ContainerInfo struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Image   string `json:"Image"` // image ID
	Created string `json:"Created"`
	State   struct {
		Status     string `json:"Status"` // created, running, paused, restarting, removing, exited, dead
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		OOMKilled  bool   `json:"OOMKilled"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status string `json:"Status"` // starting, healthy, unhealthy
		} `json:"Health"`
	} `json:"State"`
	RestartCount int `json:"RestartCount"`
	Config       struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string   `json:"IPAddress"`
			Aliases   []string `json:"Aliases"`
		} `json:"Networks"`
		Ports map[string][]PortBinding `json:"Ports"`
	} `json:"NetworkSettings"`
}

// ContainerInspect returns a container; ErrNotFound if it does not exist.
func (c *Client) ContainerInspect(ctx context.Context, id string) (*ContainerInfo, error) {
	var info ContainerInfo
	if err := c.call(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	Labels  map[string]string `json:"Labels"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Created int64             `json:"Created"`
}

// ContainerList returns all containers (running or not) carrying every
// given label; an empty value matches any value of that label.
func (c *Client) ContainerList(ctx context.Context, labels map[string]string) ([]ContainerSummary, error) {
	q := url.Values{"all": {"1"}}
	if len(labels) > 0 {
		q.Set("filters", labelFilter(labels))
	}
	var out []ContainerSummary
	err := c.call(ctx, http.MethodGet, "/containers/json", q, nil, &out)
	return out, err
}

// LogsOptions selects container output.
type LogsOptions struct {
	Follow     bool
	Tail       int // lines from the end; 0 means all
	Since      time.Time
	Timestamps bool
}

// ContainerLogs returns the multiplexed output stream of a container that
// runs without a TTY. Use Demux to split it. The caller closes the stream.
func (c *Client) ContainerLogs(ctx context.Context, id string, o LogsOptions) (io.ReadCloser, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	if o.Follow {
		q.Set("follow", "1")
	}
	if o.Tail > 0 {
		q.Set("tail", strconv.Itoa(o.Tail))
	}
	if !o.Since.IsZero() {
		q.Set("since", strconv.FormatInt(o.Since.Unix(), 10))
	}
	if o.Timestamps {
		q.Set("timestamps", "1")
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// maxLogFrame bounds a single frame of a multiplexed stream.
const maxLogFrame = 1 << 20

// Demux splits a multiplexed stream (8-byte header: stream id, 3 zero bytes,
// big-endian payload length) into stdout and stderr until EOF.
func Demux(r io.Reader, stdout, stderr io.Writer) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		if n > maxLogFrame {
			return fmt.Errorf("docker: log frame of %d bytes", n)
		}
		w := stdout
		switch hdr[0] {
		case 1:
		case 2:
			w = stderr
		case 0: // stdin, never sent for logs
			w = io.Discard
		default:
			return fmt.Errorf("docker: invalid log stream %d", hdr[0])
		}
		if _, err := io.CopyN(w, r, int64(n)); err != nil {
			return err
		}
	}
}
