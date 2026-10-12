// Package mailserver describes the mail server Basalt runs (Stalwart in a
// container) and talks to its management API (JMAP).
//
// The mail server is an app with one service whose container the panel
// defines here, like the databases. Its administrator is a recovery
// account whose credentials the panel generates and passes in the
// environment; mailboxes and their passwords live in the mail server.
package mailserver

import (
	"crypto/rand"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
)

// Image is the Stalwart release Basalt runs, pinned by digest (a deployment
// then needs no registry when the image is on the server).
const Image = "stalwartlabs/stalwart:v0.16.25@sha256:74e5a7d55303ba525d939c6bf97ed4e010df7521f52d80afc22a815b66bd53f3"

const (
	// Service is the name of the mail server's only service.
	Service = "stalwart"
	// HTTPPort serves management, JMAP, autoconfig and MTA-STS; the proxy
	// publishes it on the mail host names.
	HTTPPort = 8080
	// AdminUser is the panel's administrator account in the mail server.
	AdminUser = "basalt"
	// EnvAdmin carries the administrator credentials (user:password).
	EnvAdmin = "STALWART_RECOVERY_ADMIN"
	envURL   = "STALWART_PUBLIC_URL"
)

// Volumes of the mail server: settings (config.json) and everything else.
var Volumes = []string{"etc", "data"}

// The data store settings Stalwart needs before its first start; with them
// it starts normally instead of in its interactive setup mode.
const configJSON = `{"@type":"RocksDb","path":"/var/lib/stalwart/"}`

var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// ValidHostname checks the mail server's host name (e.g. mail.example.com).
func ValidHostname(h string) error {
	if len(h) > 63 || !hostnameRE.MatchString(h) {
		return errors.New("the mail host name must be a full host name such as mail.example.com (at most 63 characters)")
	}
	return nil
}

// NewEnv returns the environment of a new mail server: fresh credentials
// for the panel's administrator account.
func NewEnv() map[string]string {
	return map[string]string{EnvAdmin: AdminUser + ":" + rand.Text()}
}

// Credentials returns the administrator user and password from the env.
func Credentials(env map[string]string) (user, password string, err error) {
	user, password, ok := strings.Cut(env[EnvAdmin], ":")
	if !ok || user == "" || password == "" {
		return "", "", errors.New("the mail server has no administrator credentials")
	}
	return user, password, nil
}

// Limits of the mail server container.
type Limits struct {
	MemoryMB int
	CPUs     float64
}

// ServiceSpec builds the mail server's container. hostPorts maps each
// mail port to the host port it is published on (normally the same).
func ServiceSpec(env map[string]string, hostname string, hostPorts map[int]int, l Limits) (agentproto.ServiceSpec, error) {
	if _, _, err := Credentials(env); err != nil {
		return agentproto.ServiceSpec{}, err
	}
	if err := ValidHostname(hostname); err != nil {
		return agentproto.ServiceSpec{}, err
	}
	var ports []agentproto.HostPort
	for _, p := range agentproto.MailPorts {
		host := p
		if h, ok := hostPorts[p]; ok {
			host = h
		}
		ports = append(ports, agentproto.HostPort{Container: p, Host: host})
	}
	return agentproto.ServiceSpec{
		Name:  Service,
		Image: Image,
		// Write the data store settings on the first start, then run.
		Entrypoint: []string{"/bin/sh", "-c",
			`[ -s /etc/stalwart/config.json ] || printf '%s' "$0" > /etc/stalwart/config.json; ` +
				`exec /usr/local/bin/stalwart --config /etc/stalwart/config.json`,
			configJSON},
		Env: map[string]string{EnvAdmin: env[EnvAdmin], envURL: "https://" + hostname},
		// Stalwart names itself (SMTP greeting, MX and SRV targets) after
		// the host name it runs on.
		Hostname: hostname,
		Mounts: []agentproto.ServiceMount{
			{Volume: "etc", Path: "/etc/stalwart"},
			{Volume: "data", Path: "/var/lib/stalwart"},
		},
		Health: &docker.HealthConfig{
			Test:          []string{"CMD", "curl", "-fsS", "-o", "/dev/null", "http://127.0.0.1:8080/healthz/live"},
			Interval:      30 * time.Second,
			Timeout:       5 * time.Second,
			StartPeriod:   2 * time.Minute,
			StartInterval: time.Second,
			Retries:       3,
		},
		Restart:   "unless-stopped",
		StopGrace: 60,
		MemoryMB:  l.MemoryMB,
		CPUs:      l.CPUs,
		Port:      HTTPPort,
		HostPorts: ports,
	}, nil
}
