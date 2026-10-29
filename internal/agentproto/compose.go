package agentproto

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/docker"
)

// Limits on compose apps.
const (
	MaxServices       = 50
	MaxComposeVolumes = 50
)

// ServiceSpec is one service of a compose app, as the panel converted it
// from the compose file: only what Basalt supports, nothing that reaches
// the host (bind mounts, host network, privileges, devices).
type ServiceSpec struct {
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Command    []string          `json:"command,omitempty"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	User       string            `json:"user,omitempty"`
	WorkingDir string            `json:"working_dir,omitempty"`
	Hostname   string            `json:"hostname,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	// Mounts are the app's named volumes (ComposeVolumes) mounted here.
	Mounts []ServiceMount `json:"mounts,omitempty"`
	// Tmpfs maps a path to its mount options (e.g. "size=64m").
	Tmpfs map[string]string `json:"tmpfs,omitempty"`
	// Aliases are extra names the service is reached by on the app network
	// (its own name always works).
	Aliases   []string             `json:"aliases,omitempty"`
	DependsOn []ServiceDependency  `json:"depends_on,omitempty"`
	Health    *docker.HealthConfig `json:"health,omitempty"`
	// Restart is "no", "always", "unless-stopped" or "on-failure".
	Restart    string   `json:"restart"`
	MaxRetries int      `json:"max_retries,omitempty"` // on-failure only
	MemoryMB   int      `json:"memory_mb,omitempty"`
	CPUs       float64  `json:"cpus,omitempty"`
	ShmSizeMB  int      `json:"shm_size_mb,omitempty"`
	ReadOnly   bool     `json:"read_only,omitempty"`
	Init       *bool    `json:"init,omitempty"`
	StopGrace  int      `json:"stop_grace,omitempty"` // seconds
	StopSignal string   `json:"stop_signal,omitempty"`
	CapDrop    []string `json:"cap_drop,omitempty"`
	ExtraHosts []string `json:"extra_hosts,omitempty"` // "name:ip"
	Tty        bool     `json:"tty,omitempty"`
	OpenStdin  bool     `json:"open_stdin,omitempty"`
	// Port is the HTTP port of the service the app's domains point to; at
	// most one service has one.
	Port int `json:"port,omitempty"`
	// Job marks a service that runs to completion (others depend on it
	// with condition "completed"); it runs again on every deployment.
	Job bool `json:"job,omitempty"`
	// HostPorts are published on all of the host's addresses. Only the
	// mail server gets them (AppSpec.Mail), and only for mail ports.
	HostPorts []HostPort `json:"host_ports,omitempty"`
	// Build, for a compose app from git (AppSpec.Git), builds Image from
	// the repository instead of pulling it.
	Build *ServiceBuild `json:"build,omitempty"`
}

// ServiceBuild builds a service's image from the app's repository.
type ServiceBuild struct {
	Context    string `json:"context"`    // directory in the repository
	Dockerfile string `json:"dockerfile"` // path in the repository
	Target     string `json:"target,omitempty"`
	// Args are build arguments. Unlike AppSpec.Git.BuildEnv they are
	// visible in the image's history, as with docker compose.
	Args    map[string]string `json:"args,omitempty"`
	NoCache bool              `json:"no_cache,omitempty"`
	Pull    bool              `json:"pull,omitempty"`
	// ReuseImage is the image an earlier deployment built for the
	// service from the same commit; when it is still on the host it is
	// used instead of building (rollbacks).
	ReuseImage string `json:"reuse_image,omitempty"`
}

// MaxBuildArgs is how many build arguments a service may have.
const MaxBuildArgs = 50

var buildTargetRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Validate checks a service build.
func (b *ServiceBuild) Validate() error {
	if err := ValidateRepoPath(b.Context, true); err != nil {
		return fmt.Errorf("build context: %w", err)
	}
	if err := ValidateRepoPath(b.Dockerfile, false); err != nil {
		return fmt.Errorf("dockerfile: %w", err)
	}
	if b.Target != "" && !buildTargetRE.MatchString(b.Target) {
		return fmt.Errorf("invalid build target %q", b.Target)
	}
	if len(b.Args) > MaxBuildArgs {
		return fmt.Errorf("at most %d build arguments", MaxBuildArgs)
	}
	for k, v := range b.Args {
		// The docker command reads them from its environment: names it
		// reads itself are refused (BUILDKIT_SYNTAX would change the
		// frontend that builds the image).
		if err := ValidBuildEnvName(k); err != nil {
			return fmt.Errorf("build argument: %w", err)
		}
		if len(v) > MaxEnvValueSize || strings.ContainsRune(v, 0) {
			return fmt.Errorf("invalid value of build argument %s", k)
		}
	}
	if b.ReuseImage != "" && !docker.ValidRef(b.ReuseImage) {
		return errors.New("invalid image to reuse")
	}
	return nil
}

// HostPort publishes container TCP port Container on the host's port Host.
type HostPort struct {
	Container int `json:"container"`
	Host      int `json:"host"`
}

// MailPorts are the container ports a mail server may publish: SMTP,
// submission (implicit TLS and STARTTLS) and IMAP over TLS.
var MailPorts = []int{25, 465, 587, 993}

// ServiceMount mounts one of the app's named volumes.
type ServiceMount struct {
	Volume   string `json:"volume"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// Dependency conditions.
const (
	DependStarted   = "started"
	DependHealthy   = "healthy"
	DependCompleted = "completed"
)

type ServiceDependency struct {
	Service   string `json:"service"`
	Condition string `json:"condition"`
}

var (
	// Service and volume names become part of container and volume names.
	ServiceNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
	hostnameRE    = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,61}[a-zA-Z0-9])?$`)
	userRE        = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}(:[a-zA-Z0-9_.-]{1,64})?$`)
	signalRE      = regexp.MustCompile(`^(SIG)?[A-Z0-9+]{1,16}$`)
	capRE         = regexp.MustCompile(`^(CAP_)?[A-Z_]{2,32}$|^ALL$`)
	tmpfsOptRE    = regexp.MustCompile(`^[a-z0-9=,]{0,128}$`)
	extraHostRE   = regexp.MustCompile(`^[a-zA-Z0-9.-]{1,253}:[0-9a-fA-F.:\[\]]{2,64}$`)
)

func validPath(p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p && p != "/" && len(p) <= 512 && !strings.ContainsAny(p, ",:\x00")
}

func validArgs(args []string) bool {
	if len(args) > 256 {
		return false
	}
	n := 0
	for _, a := range args {
		n += len(a)
		if strings.ContainsRune(a, 0) {
			return false
		}
	}
	return n <= 64<<10
}

// Validate checks one service against the app's volumes.
func (s *ServiceSpec) Validate(volumes map[string]bool) error {
	if !ServiceNameRE.MatchString(s.Name) {
		return fmt.Errorf("invalid service name %q", s.Name)
	}
	fail := func(format string, a ...any) error {
		return fmt.Errorf("service %s: %s", s.Name, fmt.Sprintf(format, a...))
	}
	if !docker.ValidRef(s.Image) {
		return fail("invalid image reference %q", s.Image)
	}
	if !validArgs(s.Command) || !validArgs(s.Entrypoint) {
		return fail("command or entrypoint too long")
	}
	if err := ValidateEnv(s.Env); err != nil {
		return fail("%v", err)
	}
	if s.User != "" && !userRE.MatchString(s.User) {
		return fail("invalid user %q", s.User)
	}
	if s.WorkingDir != "" && (!path.IsAbs(s.WorkingDir) || len(s.WorkingDir) > 512 || strings.ContainsRune(s.WorkingDir, 0)) {
		return fail("working_dir must be an absolute path")
	}
	if s.Hostname != "" && !hostnameRE.MatchString(s.Hostname) {
		return fail("invalid hostname %q", s.Hostname)
	}
	if len(s.Labels) > 100 {
		return fail("too many labels")
	}
	for k, v := range s.Labels {
		if k == "" || len(k) > 256 || len(v) > 4096 || strings.HasPrefix(k, "basalt.") || strings.ContainsRune(k+v, 0) {
			return fail("invalid label %q (labels starting with basalt. are reserved)", k)
		}
	}
	paths := map[string]bool{}
	for _, m := range s.Mounts {
		if !volumes[m.Volume] {
			return fail("mounts unknown volume %q", m.Volume)
		}
		if !validPath(m.Path) || paths[m.Path] {
			return fail("invalid or repeated mount path %q", m.Path)
		}
		paths[m.Path] = true
	}
	if len(s.Tmpfs) > 20 {
		return fail("too many tmpfs mounts")
	}
	for p, opts := range s.Tmpfs {
		if !validPath(p) || paths[p] || !tmpfsOptRE.MatchString(opts) {
			return fail("invalid tmpfs mount %q", p)
		}
		paths[p] = true
	}
	if len(s.Aliases) > 20 {
		return fail("too many aliases")
	}
	for _, a := range s.Aliases {
		if !hostnameRE.MatchString(a) {
			return fail("invalid network alias %q", a)
		}
	}
	for _, d := range s.DependsOn {
		switch d.Condition {
		case DependStarted, DependHealthy, DependCompleted:
		default:
			return fail("invalid dependency condition %q", d.Condition)
		}
	}
	if h := s.Health; h != nil {
		if len(h.Test) == 0 || !validArgs(h.Test) || h.Retries < 0 || h.Retries > 1000 {
			return fail("invalid healthcheck")
		}
		for _, d := range []time.Duration{h.Interval, h.Timeout, h.StartPeriod, h.StartInterval} {
			if d < 0 || d > 24*time.Hour {
				return fail("invalid healthcheck duration")
			}
		}
	}
	switch s.Restart {
	case "no", "always", "unless-stopped", "on-failure":
	default:
		return fail("invalid restart policy %q", s.Restart)
	}
	switch {
	case s.MaxRetries < 0 || s.MaxRetries > 1000:
		return fail("invalid restart retries")
	case s.MemoryMB < 0 || s.MemoryMB > 1<<20 || s.MemoryMB > 0 && s.MemoryMB < 6:
		return fail("invalid memory limit")
	case s.CPUs < 0 || s.CPUs > 1024:
		return fail("invalid CPU limit")
	case s.ShmSizeMB < 0 || s.ShmSizeMB > 64<<10:
		return fail("invalid shm_size")
	case s.StopGrace < 0 || s.StopGrace > 3600:
		return fail("invalid stop_grace_period")
	case s.StopSignal != "" && !signalRE.MatchString(s.StopSignal):
		return fail("invalid stop_signal")
	case s.Port < 0 || s.Port > 65535:
		return fail("invalid port")
	case len(s.CapDrop) > 50 || len(s.ExtraHosts) > 50:
		return fail("too many cap_drop or extra_hosts entries")
	}
	for _, c := range s.CapDrop {
		if !capRE.MatchString(c) {
			return fail("invalid capability %q", c)
		}
	}
	seenPorts := map[int]bool{}
	for _, p := range s.HostPorts {
		if !slices.Contains(MailPorts, p.Container) || p.Host < 1 || p.Host > 65535 || seenPorts[p.Host] {
			return fail("invalid host port %d:%d", p.Host, p.Container)
		}
		seenPorts[p.Host] = true
	}
	for _, h := range s.ExtraHosts {
		if !extraHostRE.MatchString(h) {
			return fail("invalid extra_hosts entry %q", h)
		}
	}
	if s.Build != nil {
		if err := s.Build.Validate(); err != nil {
			return fail("%v", err)
		}
	}
	return nil
}

// ValidateCompose checks the services of a compose deployment as a whole:
// names, volumes, dependencies (which must form no cycle) and that at most
// one service is published.
func (s *AppSpec) ValidateCompose() error {
	if len(s.Services) > MaxServices {
		return fmt.Errorf("at most %d services", MaxServices)
	}
	if len(s.ComposeVolumes) > MaxComposeVolumes {
		return fmt.Errorf("at most %d volumes", MaxComposeVolumes)
	}
	vols := map[string]bool{}
	for _, v := range s.ComposeVolumes {
		if !ServiceNameRE.MatchString(v) || vols[v] {
			return fmt.Errorf("invalid or repeated volume name %q", v)
		}
		vols[v] = true
	}
	names := map[string]bool{}
	published := 0
	for i := range s.Services {
		svc := &s.Services[i]
		if err := svc.Validate(vols); err != nil {
			return err
		}
		if svc.Build != nil && (s.Git == nil || s.Git.Builder != BuilderCompose) {
			return fmt.Errorf("service %s: only a compose app from git builds images", svc.Name)
		}
		if len(svc.HostPorts) > 0 && (!s.Mail || len(s.Services) != 1) {
			return fmt.Errorf("service %s: only the mail server publishes host ports", svc.Name)
		}
		if names[svc.Name] {
			return fmt.Errorf("service %s is defined twice", svc.Name)
		}
		names[svc.Name] = true
		if svc.Port > 0 {
			published++
		}
	}
	if published > 1 {
		return errors.New("only one service can be published")
	}
	for _, svc := range s.Services {
		for _, d := range svc.DependsOn {
			if !names[d.Service] || d.Service == svc.Name {
				return fmt.Errorf("service %s depends on unknown service %q", svc.Name, d.Service)
			}
		}
	}
	if _, err := ServiceOrder(s.Services); err != nil {
		return err
	}
	if a := s.RegistryAuths; len(a) > 50 {
		return errors.New("too many registry credentials")
	}
	if s.Git != nil {
		if s.Git.Builder != BuilderCompose {
			return errors.New("a compose app from git uses the compose builder")
		}
		if err := s.Git.Validate(); err != nil {
			return err
		}
	}
	if err := ValidateImageRefs(s.PruneImages); err != nil {
		return err
	}
	return ValidateAttach(s.AppID, s.Attach)
}

// ServiceOrder returns the services in an order that starts every service
// after the ones it depends on (by name, among equals), or an error if the
// dependencies form a cycle.
func ServiceOrder(services []ServiceSpec) ([]ServiceSpec, error) {
	byName := map[string]ServiceSpec{}
	for _, s := range services {
		byName[s.Name] = s
	}
	const (
		unvisited = iota
		visiting
		done
	)
	state := map[string]int{}
	var out []ServiceSpec
	var visit func(name string, chain []string) error
	visit = func(name string, chain []string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("services depend on each other in a cycle: %s", strings.Join(append(chain, name), " -> "))
		}
		state[name] = visiting
		svc := byName[name]
		deps := make([]string, 0, len(svc.DependsOn))
		for _, d := range svc.DependsOn {
			deps = append(deps, d.Service)
		}
		slices.Sort(deps)
		for _, d := range deps {
			if err := visit(d, append(chain, name)); err != nil {
				return err
			}
		}
		state[name] = done
		out = append(out, svc)
		return nil
	}
	names := make([]string, 0, len(services))
	for _, s := range services {
		names = append(names, s.Name)
	}
	slices.Sort(names)
	for _, n := range names {
		if err := visit(n, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}
