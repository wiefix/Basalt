// Package compose turns a docker-compose file into the services Basalt runs
// for a compose app.
//
// The file is parsed with compose-go, which never touches the file system
// here: include, extends, env_file and label_file are refused, and
// ${VARIABLES} come from the app's environment only. Services build their
// image (build:) only in a file from the app's git repository (ParseRepo),
// from paths inside that repository. The parsed model is
// then checked against an allowlist: anything that would reach the host
// (bind mounts, host networking, privileges, devices, ...) or that Basalt
// does not implement is rejected with an explanation, rather than ignored.
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/sirupsen/logrus"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
)

// MaxSize bounds a compose file.
const MaxSize = agentproto.MaxComposeFile

// SilenceLibraryLogs stops compose-go from printing to stderr through
// logrus' global logger: Parse reports what matters as warnings. Call once
// in a process that parses compose files.
func SilenceLibraryLogs() { logrus.SetOutput(io.Discard) }

// Project is a validated compose file.
type Project struct {
	// Services, sorted by name. None is published (Port 0); see Publish.
	Services []agentproto.ServiceSpec
	// Volumes are the named volumes declared in the file.
	Volumes []string
	// Ports lists, per service, the container ports the file mentions
	// (ports and expose), to suggest which one to publish.
	Ports map[string][]int
	// Warnings describe parts of the file Basalt accepts but does not apply.
	Warnings []string

	repo bool   // the file is from the app's repository: services may build
	dir  string // the file's directory in the repository
}

// BuildImage is the image a service that builds is given by Parse; the
// panel replaces it with the tag the deployment builds.
func BuildImage(service string) string { return "basalt/build:" + service }

// Error is a problem with the compose file, safe to show to the user.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errf(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// Parse loads and validates a compose file; env provides the values of
// ${VARIABLES}.
func Parse(ctx context.Context, content []byte, env map[string]string) (*Project, error) {
	return parse(ctx, content, env, "")
}

// ParseRepo is Parse for a compose file from the app's git repository, in
// its directory dir ("." for the root): services may build their image
// from it (ServiceSpec.Build, with its Image set to BuildImage), with paths
// relative to dir as in compose, which must stay inside the repository.
func ParseRepo(ctx context.Context, content []byte, env map[string]string, dir string) (*Project, error) {
	if err := agentproto.ValidateRepoPath(dir, true); err != nil {
		return nil, err
	}
	return parse(ctx, content, env, dir)
}

func parse(ctx context.Context, content []byte, env map[string]string, dir string) (*Project, error) {
	repo := dir != ""
	if len(content) > MaxSize {
		return nil, errf("the compose file is larger than %d KB", MaxSize>>10)
	}
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, errf("the compose file is empty")
	}
	environment := maps.Clone(env)
	if environment == nil {
		environment = map[string]string{}
	}
	missing := map[string]bool{}
	project, err := loader.LoadWithContext(ctx, types.ConfigDetails{
		WorkingDir:  "/nonexistent",
		ConfigFiles: []types.ConfigFile{{Filename: "compose.yaml", Content: content}},
		Environment: environment,
	}, func(o *loader.Options) {
		o.SetProjectName("app", true)
		// Nothing may be read from the panel's disk.
		o.SkipInclude = true
		o.SkipExtends = true
		o.SkipResolveEnvironment = true
		o.SkipResolveLabels = true
		o.ResolvePaths = false
		lookup := o.Interpolate.LookupValue
		o.Interpolate.LookupValue = func(key string) (string, bool) {
			v, ok := lookup(key)
			if !ok && key != "COMPOSE_PROJECT_NAME" {
				missing[key] = true
			}
			return v, ok
		}
	})
	if err != nil {
		return nil, errf("%s", loadError(err))
	}
	p := &Project{Ports: map[string][]int{}, repo: repo, dir: dir}
	if err := p.checkTopLevel(project, content); err != nil {
		return nil, err
	}
	jobs := map[string]bool{}
	for _, s := range project.Services {
		for dep, d := range s.DependsOn {
			if d.Condition == types.ServiceConditionCompletedSuccessfully {
				jobs[dep] = true
			}
		}
	}
	names := slices.Sorted(maps.Keys(project.Services))
	if len(names) == 0 {
		return nil, errf("the compose file has no services")
	}
	if len(names) > agentproto.MaxServices {
		return nil, errf("at most %d services", agentproto.MaxServices)
	}
	declared := map[string]bool{}
	for _, v := range p.Volumes {
		declared[v] = true
	}
	for _, name := range names {
		svc, err := p.convert(project.Services[name], declared, jobs[name])
		if err != nil {
			return nil, err
		}
		p.Services = append(p.Services, svc)
	}
	for _, k := range slices.Sorted(maps.Keys(missing)) {
		p.Warnings = append(p.Warnings, fmt.Sprintf("variable %s is not set: it was replaced by an empty value (add it under Environment)", k))
	}
	spec := agentproto.AppSpec{Services: p.Services, ComposeVolumes: p.Volumes}
	if repo {
		spec.Git = &agentproto.GitSource{URL: "https://example.com/r", Branch: "main", Dockerfile: "Dockerfile", Context: ".",
			Builder: agentproto.BuilderCompose, ComposeFile: "compose.yaml"}
	}
	if err := spec.ValidateCompose(); err != nil {
		return nil, errf("%v", err)
	}
	return p, nil
}

// loadError shortens compose-go's errors, which may mention the made-up
// file name.
func loadError(err error) string {
	msg := strings.TrimPrefix(err.Error(), "validating compose.yaml: ")
	msg = strings.ReplaceAll(msg, "compose.yaml: ", "")
	return strings.TrimSuffix(msg, ": invalid compose project")
}

// Publish marks service as the one the app's domains point to, at port.
func (p *Project) Publish(service string, port int) error {
	if service == "" {
		return nil
	}
	for i := range p.Services {
		if p.Services[i].Name == service {
			if p.Services[i].Job {
				return errf("service %s runs to completion and cannot be published", service)
			}
			p.Services[i].Port = port
			return nil
		}
	}
	return errf("the compose file has no service %q", service)
}

// DefaultPublished suggests the service and port to publish: the first
// service (by name) that mentions a port.
func (p *Project) DefaultPublished() (string, int) {
	for _, s := range p.Services {
		if ports := p.Ports[s.Name]; len(ports) > 0 && !s.Job {
			return s.Name, ports[0]
		}
	}
	return "", 0
}

func (p *Project) warn(format string, a ...any) {
	w := fmt.Sprintf(format, a...)
	if !slices.Contains(p.Warnings, w) {
		p.Warnings = append(p.Warnings, w)
	}
}

func (p *Project) checkTopLevel(project *types.Project, content []byte) error {
	switch {
	case len(project.Secrets) > 0:
		return errf("secrets are not supported: use environment variables (stored encrypted)")
	case len(project.Configs) > 0:
		return errf("configs are not supported")
	case len(project.Models) > 0:
		return errf("models are not supported")
	case len(project.Jobs) > 0:
		return errf("jobs are not supported: use a service that others depend on with condition service_completed_successfully")
	case len(project.DisabledServices) > 0:
		return errf("services with profiles are not supported (%s)", strings.Join(slices.Sorted(maps.Keys(project.DisabledServices)), ", "))
	}
	if bytes.Contains(content, []byte("include:")) && hasTopLevelKey(content, "include") {
		return errf("include is not supported: put everything in one file")
	}
	for _, name := range slices.Sorted(maps.Keys(project.Volumes)) {
		v := project.Volumes[name]
		switch {
		case !agentproto.ServiceNameRE.MatchString(name):
			return errf("invalid volume name %q", name)
		case bool(v.External):
			return errf("volume %s: external volumes are not supported", name)
		case v.Driver != "" && v.Driver != "local":
			return errf("volume %s: only local volumes are supported", name)
		case len(v.DriverOpts) > 0:
			return errf("volume %s: driver_opts are not allowed (they can mount the server's files)", name)
		}
		p.Volumes = append(p.Volumes, name)
	}
	if len(p.Volumes) > agentproto.MaxComposeVolumes {
		return errf("at most %d volumes", agentproto.MaxComposeVolumes)
	}
	for _, name := range slices.Sorted(maps.Keys(project.Networks)) {
		n := project.Networks[name]
		switch {
		case bool(n.External):
			return errf("network %s: external networks are not supported", name)
		case n.Driver != "" && n.Driver != "bridge":
			return errf("network %s: only bridge networks are supported", name)
		case len(n.DriverOpts) > 0:
			return errf("network %s: driver_opts are not supported", name)
		}
		if name != "default" {
			p.warn("network %s: all services of an app share one private network", name)
		}
	}
	return nil
}

// hasTopLevelKey reports whether the YAML document has key at column 0.
func hasTopLevelKey(content []byte, key string) bool {
	for _, line := range bytes.Split(content, []byte("\n")) {
		if bytes.HasPrefix(line, []byte(key+":")) {
			return true
		}
	}
	return false
}

// Service attributes that are refused, with the reason shown.
var refused = map[string]string{
	"privileged":          "privileged containers control the whole server",
	"cap_add":             "extra capabilities weaken the container's isolation",
	"network_mode":        "the container would leave the app's private network",
	"net":                 "the container would leave the app's private network",
	"pid":                 "sharing the process namespace weakens isolation",
	"ipc":                 "sharing the IPC namespace weakens isolation",
	"uts":                 "sharing the UTS namespace weakens isolation",
	"userns_mode":         "it changes the user namespace",
	"cgroup":              "it changes the cgroup namespace",
	"cgroup_parent":       "it changes resource accounting",
	"devices":             "host devices cannot be passed to containers",
	"device_cgroup_rules": "host devices cannot be passed to containers",
	"gpus":                "GPUs are not supported yet",
	"security_opt":        "security options are set by Basalt",
	"sysctls":             "kernel parameters cannot be changed",
	"volumes_from":        "use named volumes",
	"external_links":      "apps cannot reach containers outside them",
	"env_file":            "put the variables under Environment and reference them as ${NAME}",
	"label_file":          "files on the server cannot be read",
	"extends":             "put everything in one file",
	"secrets":             "use environment variables (stored encrypted)",
	"configs":             "configs are not supported",
	"use_api_socket":      "containers never get access to Docker",
	"runtime":             "the container runtime cannot be changed",
	"isolation":           "the isolation technology cannot be changed",
	"logging":             "logs are kept by Basalt",
	"log_driver":          "logs are kept by Basalt",
	"log_opt":             "logs are kept by Basalt",
	"oom_kill_disable":    "it can freeze the server under memory pressure",
	"oom_score_adj":       "it changes how the server handles memory pressure",
	"storage_opt":         "storage options are not supported",
	"volume_driver":       "only local volumes are supported",
	"credential_spec":     "not supported",
	"provider":            "providers are not supported",
	"models":              "models are not supported",
	"pre_start":           "lifecycle hooks are not supported",
	"post_start":          "lifecycle hooks are not supported",
	"pre_stop":            "lifecycle hooks are not supported",
	"group_add":           "not supported",
	"platform":            "the server's own platform is used",
	"blkio_config":        "not supported",
	"ulimits":             "not supported yet",
	"pids_limit":          "the process limit is set by Basalt",
	"annotations":         "not supported",
	"dns":                 "not supported yet",
	"dns_opt":             "not supported yet",
	"dns_search":          "not supported yet",
	"mac_address":         "not supported",
}

// Service attributes that are accepted. Those also in ignored are not applied.
var accepted = map[string]bool{
	"image": true, "command": true, "entrypoint": true, "environment": true, "user": true,
	"working_dir": true, "hostname": true, "domainname": true, "labels": true, "volumes": true,
	"tmpfs": true, "networks": true, "depends_on": true, "healthcheck": true, "restart": true,
	"mem_limit": true, "mem_reservation": true, "cpus": true, "shm_size": true, "read_only": true,
	"init": true, "stop_grace_period": true, "stop_signal": true, "cap_drop": true,
	"extra_hosts": true, "tty": true, "stdin_open": true, "ports": true, "expose": true,
	"container_name": true, "links": true, "deploy": true, "pull_policy": true, "scale": true,
	"profiles": true, "develop": true, "attach": true, "build": true,
}

// Build attributes that are accepted.
var acceptedBuild = map[string]bool{
	"context": true, "dockerfile": true, "args": true, "target": true, "no_cache": true, "pull": true,
}

func (p *Project) convert(s types.ServiceConfig, volumes map[string]bool, job bool) (agentproto.ServiceSpec, error) {
	name := s.Name
	svc := agentproto.ServiceSpec{Name: name, Job: job}
	if !agentproto.ServiceNameRE.MatchString(name) {
		return svc, errf("invalid service name %q (letters, digits, ., _ and -)", name)
	}
	fail := func(format string, a ...any) error {
		return errf("service %s: %s", name, fmt.Sprintf(format, a...))
	}

	// The allowlist runs over the normalized model, so attributes added
	// to compose-go later are refused until reviewed.
	b, err := json.Marshal(s)
	if err != nil {
		return svc, err
	}
	var attrs map[string]json.RawMessage
	if err := json.Unmarshal(b, &attrs); err != nil {
		return svc, err
	}
	for _, k := range slices.Sorted(maps.Keys(attrs)) {
		if string(attrs[k]) == "null" {
			continue
		}
		if why, ok := refused[k]; ok {
			return svc, fail("%s is not allowed: %s", k, why)
		}
		if !accepted[k] {
			return svc, fail("%s is not supported", k)
		}
	}

	if s.Build != nil {
		if !p.repo {
			return svc, fail("build is not allowed: a compose file stored in Basalt runs ready images; to build them, create the app from a git repository with Docker Compose")
		}
		b, err := p.build(s.Build, attrs["build"])
		if err != nil {
			return svc, fail("%v", err)
		}
		svc.Build, svc.Image = b, BuildImage(name)
	} else {
		if s.Image == "" {
			return svc, fail("an image is required")
		}
		svc.Image = s.Image
		if !docker.ValidRef(svc.Image) {
			return svc, fail("invalid image reference %q", svc.Image)
		}
	}
	svc.Command, svc.Entrypoint = s.Command, s.Entrypoint
	svc.User, svc.WorkingDir, svc.Hostname = s.User, s.WorkingDir, s.Hostname
	svc.ReadOnly, svc.Init, svc.Tty, svc.OpenStdin = s.ReadOnly, s.Init, s.Tty, s.StdinOpen
	svc.StopSignal = s.StopSignal
	svc.CapDrop = s.CapDrop
	if s.DomainName != "" {
		p.warn("service %s: domainname is ignored", name)
	}
	if len(s.Environment) > 0 {
		svc.Env = map[string]string{}
		for k, v := range s.Environment {
			if v != nil {
				svc.Env[k] = *v
			}
		}
	}
	if len(s.Labels) > 0 {
		svc.Labels = maps.Clone(map[string]string(s.Labels))
	}
	if s.StopGracePeriod != nil {
		svc.StopGrace = int(math.Ceil(time.Duration(*s.StopGracePeriod).Seconds()))
	}

	// Storage.
	for _, v := range s.Volumes {
		switch v.Type {
		case types.VolumeTypeVolume:
			if v.Source == "" {
				return svc, fail("give the volume at %s a name and declare it under volumes:, or its data is lost on every deployment", v.Target)
			}
			if !volumes[v.Source] {
				return svc, fail("volume %s is not declared under volumes:", v.Source)
			}
			if v.Volume != nil && v.Volume.Subpath != "" {
				return svc, fail("volume subpath is not supported")
			}
			svc.Mounts = append(svc.Mounts, agentproto.ServiceMount{Volume: v.Source, Path: v.Target, ReadOnly: v.ReadOnly})
		case types.VolumeTypeTmpfs:
			opts := ""
			if v.Tmpfs != nil && v.Tmpfs.Size > 0 {
				opts = "size=" + strconv.FormatInt(int64(v.Tmpfs.Size), 10)
			}
			if svc.Tmpfs == nil {
				svc.Tmpfs = map[string]string{}
			}
			svc.Tmpfs[v.Target] = opts
		case types.VolumeTypeBind:
			return svc, fail("bind mount %s is not allowed: it would expose the server's files; use a named volume", v.Source)
		default:
			return svc, fail("%s mounts are not supported", v.Type)
		}
	}
	for _, t := range s.Tmpfs {
		path, opts, _ := strings.Cut(t, ":")
		if svc.Tmpfs == nil {
			svc.Tmpfs = map[string]string{}
		}
		svc.Tmpfs[path] = opts
	}

	// Network.
	networks := slices.Sorted(maps.Keys(s.Networks))
	for _, n := range networks {
		c := s.Networks[n]
		if c == nil {
			continue
		}
		if c.Ipv4Address != "" || c.Ipv6Address != "" || c.MacAddress != "" || len(c.LinkLocalIPs) > 0 || len(c.DriverOpts) > 0 {
			return svc, fail("fixed addresses and network options are not supported")
		}
		for _, a := range c.Aliases {
			if a != name && !slices.Contains(svc.Aliases, a) {
				svc.Aliases = append(svc.Aliases, a)
			}
		}
	}
	for h, ips := range s.ExtraHosts {
		for _, ip := range ips {
			svc.ExtraHosts = append(svc.ExtraHosts, h+":"+ip)
		}
	}
	slices.Sort(svc.ExtraHosts)
	for _, pc := range s.Ports {
		if pc.Target > 0 && !slices.Contains(p.Ports[name], int(pc.Target)) {
			p.Ports[name] = append(p.Ports[name], int(pc.Target))
		}
		if pc.Published != "" {
			p.warn("service %s: ports are not published on the server; publish one service on the app's domains instead", name)
		}
	}
	for _, e := range s.Expose {
		digits := strings.TrimFunc(strings.SplitN(strings.SplitN(e, "/", 2)[0], "-", 2)[0], func(r rune) bool { return r < '0' || r > '9' })
		if n, err := strconv.Atoi(digits); err == nil && n > 0 && n <= 65535 && !slices.Contains(p.Ports[name], n) {
			p.Ports[name] = append(p.Ports[name], n)
		}
	}
	if s.ContainerName != "" {
		p.warn("service %s: container_name is ignored; other services reach it as %s", name, name)
	}
	if len(s.Links) > 0 {
		p.warn("service %s: links are not needed; services reach each other by name", name)
	}

	// Dependencies.
	for _, dep := range slices.Sorted(maps.Keys(s.DependsOn)) {
		d := agentproto.ServiceDependency{Service: dep}
		switch s.DependsOn[dep].Condition {
		case "", types.ServiceConditionStarted:
			d.Condition = agentproto.DependStarted
		case types.ServiceConditionHealthy:
			d.Condition = agentproto.DependHealthy
		case types.ServiceConditionCompletedSuccessfully:
			d.Condition = agentproto.DependCompleted
		default:
			return svc, fail("depends_on %s: unsupported condition %q", dep, s.DependsOn[dep].Condition)
		}
		svc.DependsOn = append(svc.DependsOn, d)
	}

	// Health check.
	if h := s.HealthCheck; h != nil {
		switch {
		case h.Disable || len(h.Test) > 0 && h.Test[0] == "NONE":
			svc.Health = &docker.HealthConfig{Test: []string{"NONE"}}
		case len(h.Test) > 0:
			hc := &docker.HealthConfig{Test: h.Test}
			if h.Test[0] != "CMD" && h.Test[0] != "CMD-SHELL" {
				return svc, fail("healthcheck test must start with CMD or CMD-SHELL")
			}
			dur := func(d *types.Duration) time.Duration {
				if d == nil {
					return 0
				}
				return time.Duration(*d)
			}
			hc.Interval, hc.Timeout = dur(h.Interval), dur(h.Timeout)
			hc.StartPeriod, hc.StartInterval = dur(h.StartPeriod), dur(h.StartInterval)
			if h.Retries != nil {
				hc.Retries = int(min(*h.Retries, 1000))
			}
			svc.Health = hc
		}
	}

	// Lifecycle and resources.
	restart, retries, _ := strings.Cut(s.Restart, ":")
	switch restart {
	case "":
		svc.Restart = "unless-stopped"
		if job {
			svc.Restart = "no"
		}
	case "no", "always", "unless-stopped", "on-failure":
		svc.Restart = restart
		if retries != "" {
			n, err := strconv.Atoi(retries)
			if err != nil || restart != "on-failure" {
				return svc, fail("invalid restart policy %q", s.Restart)
			}
			svc.MaxRetries = n
		}
	default:
		return svc, fail("invalid restart policy %q", s.Restart)
	}
	if s.Scale != nil && *s.Scale != 1 {
		return svc, fail("running several copies of a service is not supported yet")
	}
	memory, cpus := int64(s.MemLimit), float64(s.CPUS)
	if d := s.Deploy; d != nil {
		if d.Replicas != nil && *d.Replicas != 1 {
			return svc, fail("running several copies of a service is not supported yet")
		}
		if d.Mode != "" && d.Mode != "replicated" {
			return svc, fail("deploy mode %s is not supported", d.Mode)
		}
		if l := d.Resources.Limits; l != nil {
			if l.MemoryBytes > 0 {
				memory = int64(l.MemoryBytes)
			}
			if l.NanoCPUs > 0 {
				cpus = float64(l.NanoCPUs)
			}
			if len(l.Devices) > 0 || len(l.GenericResources) > 0 {
				return svc, fail("device reservations are not supported")
			}
		}
		if r := d.Resources.Reservations; r != nil && (len(r.Devices) > 0 || len(r.GenericResources) > 0) {
			return svc, fail("device reservations are not supported")
		}
		if len(d.Placement.Constraints) > 0 || len(d.Placement.Preferences) > 0 {
			p.warn("service %s: placement constraints are ignored: the app runs on its server", name)
		}
	}
	if memory > 0 {
		svc.MemoryMB = int((memory + 1<<20 - 1) >> 20)
	}
	svc.CPUs = math.Round(cpus*1000) / 1000
	if s.ShmSize > 0 {
		svc.ShmSizeMB = int((int64(s.ShmSize) + 1<<20 - 1) >> 20)
	}
	if err := svc.Validate(volumes); err != nil {
		return svc, errf("%v", err)
	}
	return svc, nil
}

// build converts the build section of a service: the context is relative
// to the compose file's directory and the Dockerfile to the context, as in
// compose; both end up as paths inside the repository.
func (p *Project) build(b *types.BuildConfig, raw json.RawMessage) (*agentproto.ServiceBuild, error) {
	var attrs map[string]json.RawMessage
	if err := json.Unmarshal(raw, &attrs); err != nil {
		return nil, err
	}
	for _, k := range slices.Sorted(maps.Keys(attrs)) {
		if v := string(attrs[k]); v == "null" || v == "false" || v == `""` {
			continue
		}
		if !acceptedBuild[k] {
			return nil, fmt.Errorf("build %s is not supported (only context, dockerfile, args, target, no_cache and pull)", k)
		}
	}
	ctx := b.Context
	if ctx == "" {
		ctx = "."
	}
	if strings.Contains(ctx, "://") || strings.Contains(ctx, "@") {
		return nil, fmt.Errorf("build context %s: only directories of the repository can be built", ctx)
	}
	if path.IsAbs(ctx) {
		return nil, fmt.Errorf("build context %s: use a path relative to the repository", ctx)
	}
	ctx = path.Join(p.dir, ctx)
	if err := agentproto.ValidateRepoPath(ctx, true); err != nil {
		return nil, fmt.Errorf("build context: %w (outside the repository)", err)
	}
	dockerfile := b.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if path.IsAbs(dockerfile) {
		return nil, fmt.Errorf("dockerfile %s: use a path relative to the build context", dockerfile)
	}
	dockerfile = path.Join(ctx, dockerfile)
	if err := agentproto.ValidateRepoPath(dockerfile, false); err != nil {
		return nil, fmt.Errorf("dockerfile: %w", err)
	}
	out := &agentproto.ServiceBuild{Context: ctx, Dockerfile: dockerfile, Target: b.Target, NoCache: b.NoCache, Pull: b.Pull}
	for _, k := range slices.Sorted(maps.Keys(b.Args)) {
		// compose-go fills arguments given without a value from the
		// app's environment, and drops those it does not have.
		v := b.Args[k]
		if v == nil {
			continue
		}
		if out.Args == nil {
			out.Args = map[string]string{}
		}
		out.Args[k] = *v
	}
	return out, out.Validate()
}
