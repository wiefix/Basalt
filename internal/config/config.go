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
}

// Agent configures `basalt agent`.
type Agent struct {
	Common
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
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
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
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
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
