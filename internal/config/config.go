// Package config parses command-line flags for each subcommand.
// Every flag can also be set through a BASALT_* environment variable;
// an explicit flag wins over the environment.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Common holds settings shared by all long-running subcommands.
type Common struct {
	DataDir   string
	LogLevel  string
	LogFormat string
}

// Server configures `basalt server`.
type Server struct {
	Common
	Listen string
	// InsecureCookies drops the Secure cookie attribute for local development
	// over plain HTTP. Never enable in production.
	InsecureCookies bool
	// AgentListen is where agents connect (mutual TLS).
	AgentListen string
	// AgentURL is the address agents use to reach AgentListen.
	AgentURL string
	// TrustedProxies may set X-Forwarded-For (comma-separated IPs/CIDRs).
	TrustedProxies string
	// BinariesDir optionally holds basalt-<os>-<arch> builds served to
	// agents on other architectures.
	BinariesDir string
	Restic      string
}

// LocalEnroll configures `basalt server local-enroll`.
type LocalEnroll struct {
	Common
	AgentURL string
	Name     string
}

// AgentEnroll configures `basalt agent enroll`.
type AgentEnroll struct {
	Common
	PanelURL string
	Token    string
	CASHA256 string
}

// Agent configures `basalt agent`.
type Agent struct {
	Common
	ProxyDir      string
	SitesRoot     string
	HTTPPort      int
	HTTPSPort     int
	ACMEDirectory string
	SFTPListen    string
	DNSListen     string
	Isolate       string // "auto", "on", "off"
	ProxyUser     string
	MySQLSocket   string
	DockerSocket  string
	GitCAFile     string
	Restic        string
	PanelDomain   string
	PanelUpstream string
}

// Proxy configures `basalt proxy`.
type Proxy struct {
	Common
}

// ParseProxy parses flags for `basalt proxy`.
func ParseProxy(args []string, output io.Writer) (Proxy, error) {
	var cfg Proxy
	fs := flag.NewFlagSet("basalt proxy", flag.ContinueOnError)
	fs.SetOutput(output)
	cfg.register(fs, "/var/lib/basalt/proxy")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return cfg, cfg.validate()
}

func (c *Common) register(fs *flag.FlagSet, defaultDataDir string) {
	fs.StringVar(&c.DataDir, "data-dir", env("DATA_DIR", defaultDataDir), "directory for persistent state")
	fs.StringVar(&c.LogLevel, "log-level", env("LOG_LEVEL", "info"), "log level: debug, info, warn, error")
	fs.StringVar(&c.LogFormat, "log-format", env("LOG_FORMAT", "text"), "log format: text or json")
}

func (c *Common) validate() error {
	if c.DataDir == "" {
		return errors.New("data-dir must not be empty")
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return fmt.Errorf("resolve data-dir: %w", err)
	}
	c.DataDir = abs
	return nil
}

// ParseServer parses flags for `basalt server`.
func ParseServer(args []string, output io.Writer) (Server, error) {
	var cfg Server
	fs := flag.NewFlagSet("basalt server", flag.ContinueOnError)
	fs.SetOutput(output)
	cfg.register(fs, "/var/lib/basalt/server")
	fs.StringVar(&cfg.Listen, "listen", env("LISTEN", "127.0.0.1:8080"), "HTTP listen address")
	insecure, err := envBool("INSECURE_COOKIES")
	if err != nil {
		return cfg, err
	}
	fs.BoolVar(&cfg.InsecureCookies, "insecure-cookies", insecure, "allow session cookies over plain HTTP (development only)")
	fs.StringVar(&cfg.AgentListen, "agent-listen", env("AGENT_LISTEN", ":8443"), "agent channel listen address (mutual TLS)")
	fs.StringVar(&cfg.AgentURL, "agent-url", env("AGENT_URL", ""), "URL agents use to reach the agent channel (default https://<hostname>:<agent port>)")
	fs.StringVar(&cfg.TrustedProxies, "trusted-proxies", env("TRUSTED_PROXIES", ""), "comma-separated IPs/CIDRs of reverse proxies allowed to set X-Forwarded-For")
	fs.StringVar(&cfg.Restic, "restic", env("RESTIC", "restic"), "restic binary used for backups of the panel")
	fs.StringVar(&cfg.BinariesDir, "binaries-dir", env("BINARIES_DIR", ""), "directory with basalt-<os>-<arch> binaries to serve to agents")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	_, agentPort, err := net.SplitHostPort(cfg.AgentListen)
	if err != nil {
		return cfg, fmt.Errorf("invalid agent listen address %q: %w", cfg.AgentListen, err)
	}
	if cfg.AgentURL == "" {
		host, _ := os.Hostname()
		cfg.AgentURL = "https://" + net.JoinHostPort(host, agentPort)
	}
	if !strings.HasPrefix(cfg.AgentURL, "https://") {
		return cfg, fmt.Errorf("agent-url must start with https://")
	}
	if _, _, err := net.SplitHostPort(cfg.Listen); err != nil {
		return cfg, fmt.Errorf("invalid listen address %q: %w", cfg.Listen, err)
	}
	return cfg, nil
}

// ParseAgent parses flags for `basalt agent`.
func ParseAgent(args []string, output io.Writer) (Agent, error) {
	var cfg Agent
	fs := flag.NewFlagSet("basalt agent", flag.ContinueOnError)
	fs.SetOutput(output)
	cfg.register(fs, "/var/lib/basalt/agent")
	fs.StringVar(&cfg.ProxyDir, "proxy-dir", env("PROXY_DIR", "/var/lib/basalt/proxy"), "data directory of `basalt proxy` on this host")
	fs.StringVar(&cfg.SitesRoot, "sites-root", env("SITES_ROOT", "/srv/basalt/sites"), "directory that contains all site files")
	fs.IntVar(&cfg.HTTPPort, "http-port", 80, "public HTTP port of the proxy")
	fs.IntVar(&cfg.HTTPSPort, "https-port", 443, "public HTTPS port of the proxy")
	fs.StringVar(&cfg.ACMEDirectory, "acme-directory", env("ACME_DIRECTORY", ""), "ACME directory URL override (e.g. Let's Encrypt staging)")
	fs.StringVar(&cfg.SFTPListen, "sftp-listen", env("SFTP_LISTEN", ":2222"), "SFTP listen address (empty disables SFTP)")
	fs.StringVar(&cfg.DNSListen, "dns-listen", env("DNS_LISTEN", ""), "authoritative DNS listen address, UDP and TCP (e.g. :53; empty disables DNS)")
	fs.StringVar(&cfg.Isolate, "isolate", env("ISOLATE", "auto"), "per-site system users: auto (on when root on Linux), on, off")
	fs.StringVar(&cfg.ProxyUser, "proxy-user", env("PROXY_USER", ""), "system user `basalt proxy` runs as (added to the web group)")
	fs.StringVar(&cfg.PanelDomain, "panel-domain", env("PANEL_DOMAIN", ""), "publish the panel UI on this domain or IP through the local proxy (panel host only)")
	fs.StringVar(&cfg.PanelUpstream, "panel-upstream", env("PANEL_UPSTREAM", "127.0.0.1:8080"), "address of the panel's HTTP listener")
	fs.StringVar(&cfg.MySQLSocket, "mysql-socket", env("MYSQL_SOCKET", "/run/mysqld/mysqld.sock"), "local MariaDB/MySQL socket (empty disables databases)")
	fs.StringVar(&cfg.DockerSocket, "docker-socket", env("DOCKER_SOCKET", "/var/run/docker.sock"), "local Docker Engine socket (empty disables apps)")
	fs.StringVar(&cfg.Restic, "restic", env("RESTIC", "restic"), "restic binary used for backups")
	fs.StringVar(&cfg.GitCAFile, "git-ca-file", env("GIT_CA_FILE", ""), "extra CA certificates (PEM) trusted for https git repositories")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	switch cfg.Isolate {
	case "auto", "on", "off":
	default:
		return cfg, fmt.Errorf("--isolate must be auto, on or off")
	}
	for _, p := range []int{cfg.HTTPPort, cfg.HTTPSPort} {
		if p < 1 || p > 65535 {
			return cfg, fmt.Errorf("invalid port %d", p)
		}
	}
	for _, d := range []*string{&cfg.ProxyDir, &cfg.SitesRoot} {
		abs, err := filepath.Abs(*d)
		if err != nil {
			return cfg, err
		}
		*d = abs
	}
	return cfg, cfg.validate()
}

// ParseAgentEnroll parses flags for `basalt agent enroll`.
func ParseAgentEnroll(args []string, output io.Writer) (AgentEnroll, error) {
	var cfg AgentEnroll
	fs := flag.NewFlagSet("basalt agent enroll", flag.ContinueOnError)
	fs.SetOutput(output)
	cfg.register(fs, "/var/lib/basalt/agent")
	fs.StringVar(&cfg.PanelURL, "panel", "", "agent channel URL of the panel, e.g. https://panel.example.com:8443")
	fs.StringVar(&cfg.Token, "token", env("ENROLL_TOKEN", ""), "one-time enrollment token")
	fs.StringVar(&cfg.CASHA256, "ca-sha256", "", "SHA-256 fingerprint of the panel's agent CA")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if cfg.PanelURL == "" || cfg.Token == "" || cfg.CASHA256 == "" {
		return cfg, errors.New("--panel, --token and --ca-sha256 are required")
	}
	return cfg, cfg.validate()
}

// ParseLocalEnroll parses flags for `basalt server local-enroll`.
func ParseLocalEnroll(args []string, output io.Writer) (LocalEnroll, error) {
	var cfg LocalEnroll
	fs := flag.NewFlagSet("basalt server local-enroll", flag.ContinueOnError)
	fs.SetOutput(output)
	cfg.register(fs, "/var/lib/basalt/server")
	fs.StringVar(&cfg.AgentURL, "agent-url", "https://127.0.0.1:8443", "agent channel URL as seen from this host")
	fs.StringVar(&cfg.Name, "name", "local", "name of the server entry")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	return cfg, cfg.validate()
}

func envBool(key string) (bool, error) {
	v, ok := os.LookupEnv("BASALT_" + key)
	if !ok || v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("BASALT_%s: invalid boolean %q", key, v)
	}
	return b, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv("BASALT_" + key); ok {
		return v
	}
	return fallback
}
