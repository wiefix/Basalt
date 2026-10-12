package agentproto

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
)

const (
	// TypeHealthApply replaces the health checks an agent runs (HealthApply).
	TypeHealthApply = "health_apply"
	// TypeHealthReport (agent -> panel, notification) carries results.
	TypeHealthReport = "health_report"
)

// Health check kinds.
const (
	HealthHTTP  = "http"
	HealthHTTPS = "https"
	HealthTCP   = "tcp"
)

// Bounds of health checks.
const (
	MaxHealthChecks   = 500
	MaxHealthInterval = 600 // seconds
	MaxHealthTimeout  = 30  // seconds
)

// MinHealthInterval is the shortest interval between probes, in seconds
// (tests shorten it).
var MinHealthInterval = 10

// HealthCheck is a probe every agent runs: an HTTP(S) request (an answer
// with a status below 400 passes) or a TCP connection to Host:Port.
type HealthCheck struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Host string `json:"host"` // IP address or host name
	Port int    `json:"port"`
	// HTTP(S): the path and the Host header (and TLS server name; empty:
	// Host); Insecure accepts any certificate.
	Path       string `json:"path,omitempty"`
	HostHeader string `json:"host_header,omitempty"`
	Insecure   bool   `json:"insecure,omitempty"`
	Interval   int    `json:"interval"` // seconds
	Timeout    int    `json:"timeout"`  // seconds
}

var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

func validHost(h string) bool {
	return net.ParseIP(h) != nil || (len(h) <= 253 && hostRE.MatchString(h))
}

// Validate checks a health check.
func (c *HealthCheck) Validate() error {
	switch {
	case c.ID <= 0:
		return errors.New("invalid health check id")
	case c.Kind != HealthHTTP && c.Kind != HealthHTTPS && c.Kind != HealthTCP:
		return errors.New("the kind is http, https or tcp")
	case !validHost(c.Host):
		return errors.New("the target is an IP address or a host name")
	case c.Port < 1 || c.Port > 65535:
		return errors.New("invalid port")
	case c.Interval < MinHealthInterval || c.Interval > MaxHealthInterval:
		return fmt.Errorf("the interval is %d to %d seconds", MinHealthInterval, MaxHealthInterval)
	case c.Timeout < 1 || c.Timeout > MaxHealthTimeout || c.Timeout >= c.Interval:
		return errors.New("the timeout is 1 to 30 seconds, shorter than the interval")
	}
	if c.Kind == HealthTCP {
		if c.Path != "" || c.HostHeader != "" || c.Insecure {
			return errors.New("a TCP check has no path, host header or TLS settings")
		}
		return nil
	}
	if !strings.HasPrefix(c.Path, "/") || len(c.Path) > 500 || strings.ContainsAny(c.Path, " \t\r\n#") {
		return errors.New("the path starts with / and has no spaces")
	}
	if c.HostHeader != "" && !validHost(c.HostHeader) {
		return errors.New("invalid host header")
	}
	return nil
}

// HealthApply is the complete set of checks; older revisions are ignored.
type HealthApply struct {
	Revision int64         `json:"revision"`
	Checks   []HealthCheck `json:"checks"`
}

// HealthResult is the outcome of one probe.
type HealthResult struct {
	CheckID   int64  `json:"check_id"`
	OK        bool   `json:"ok"`
	LatencyMS int    `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

type HealthReport struct {
	Results []HealthResult `json:"results"`
}
