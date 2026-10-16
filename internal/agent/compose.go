package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
)

// jobTimeout bounds a service that runs to completion (e.g. migrations).
const jobTimeout = 15 * time.Minute

func serviceContainer(appID, deploymentID int64, service string) string {
	return appNetwork(appID) + "-" + service + "-d" + strconv.FormatInt(deploymentID, 10)
}

// deployCompose runs a compose app: services start in dependency order,
// each after the ones it depends on are ready. A service whose settings and
// image did not change keeps its container. A changed service without
// volumes gets its new container before the old one stops (no downtime);
// one with volumes is stopped first, so two containers never share its
// data. If any service fails, everything is put back as it was. It returns
// the image each service runs.
func (m *appsManager) deployCompose(ctx context.Context, dc *docker.Client, spec agentproto.AppSpec, lg *deployLog) (map[string]string, error) {
	order, err := agentproto.ServiceOrder(spec.Services)
	if err != nil {
		return nil, err
	}
	digests, imageIDs := map[string]string{}, map[string]string{}
	pulled := map[string][2]string{}
	for _, svc := range order {
		r, ok := pulled[svc.Image]
		if !ok {
			digest, id, err := m.pull(ctx, dc, svc.Image, authFor(spec.RegistryAuths, svc.Image), lg)
			if err != nil {
				return nil, err
			}
			r = [2]string{digest, id}
			pulled[svc.Image] = r
		}
		digests[svc.Name], imageIDs[svc.Name] = r[0], r[1]
	}
	if err := m.ensureNetwork(ctx, dc, spec.AppID); err != nil {
		return nil, err
	}
	for _, v := range spec.ComposeVolumes {
		labels := map[string]string{labelApp: strconv.FormatInt(spec.AppID, 10), labelVolume: v}
		if err := dc.VolumeCreate(ctx, appVolume(spec.AppID, v), labels); err != nil {
			return nil, fmt.Errorf("create volume %s: %w", v, err)
		}
	}
	existing, err := m.containers(ctx, dc, spec.AppID) // newest first
	if err != nil {
		return nil, err
	}
	byService := map[string][]docker.ContainerSummary{}
	for _, c := range existing {
		byService[c.Labels[labelService]] = append(byService[c.Labels[labelService]], c)
	}

	// undo puts back what this deployment changed, newest change first.
	var undo []func(context.Context)
	rollback := func(err error) (map[string]string, error) {
		bg := context.WithoutCancel(ctx)
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i](bg)
		}
		if len(undo) > 0 {
			lg.printf("Restored the previous containers")
		}
		return nil, err
	}
	var retired []docker.ContainerSummary // replaced containers, removed at the end
	active := map[string]string{}         // container ID -> service, after this deployment
	switched := false                     // the published service has a new container
	for _, svc := range order {
		hash := serviceHash(spec.AppID, svc, imageIDs[svc.Name])
		olds := byService[svc.Name]
		if i := slices.IndexFunc(olds, func(c docker.ContainerSummary) bool {
			return c.State == "running" && c.Labels[labelConfig] == hash
		}); i >= 0 && !svc.Job {
			lg.printf("Service %s is unchanged", svc.Name)
			retired = append(retired, slices.Delete(slices.Clone(olds), i, i+1)...)
			active[olds[i].ID] = svc.Name
			if err := m.waitDependency(ctx, dc, svc, olds[i].ID, lg); err != nil {
				return rollback(err)
			}
			continue
		}
		switched = switched || svc.Port > 0
		if len(svc.Mounts) > 0 {
			// Its data must not be used by two containers at once.
			for _, o := range olds {
				if o.State != "running" {
					continue
				}
				lg.printf("Stopping service %s to replace it", svc.Name)
				if err := dc.ContainerStop(ctx, o.ID, stopTimeout(svc)); err != nil {
					return rollback(fmt.Errorf("stop %s: %w", svc.Name, err))
				}
				id := o.ID
				undo = append(undo, func(ctx context.Context) { dc.ContainerStart(ctx, id) })
			}
		}
		name := serviceContainer(spec.AppID, spec.DeploymentID, svc.Name)
		if err := dc.ContainerRemove(ctx, name, true); err != nil { // left by an earlier attempt
			return rollback(err)
		}
		id, err := dc.ContainerCreate(ctx, name, m.serviceConfig(spec, svc, hash))
		if err != nil {
			return rollback(fmt.Errorf("create container for %s: %w", svc.Name, err))
		}
		undo = append(undo, func(ctx context.Context) { dc.ContainerRemove(ctx, id, true) })
		lg.printf("Starting service %s", svc.Name)
		if err := dc.ContainerStart(ctx, id); err != nil {
			return rollback(fmt.Errorf("start %s: %w", svc.Name, err))
		}
		if svc.Job {
			err = m.waitJob(ctx, dc, svc.Name, id, lg)
		} else {
			err = m.waitContainer(ctx, dc, spec.AppID, svc.Port, id, "Service "+svc.Name+": ", lg)
		}
		if err != nil {
			lg.printf("Last output of service %s:", svc.Name)
			lg.write(m.tail(context.WithoutCancel(ctx), dc, id, 50))
			return rollback(err)
		}
		retired = append(retired, olds...)
		if !svc.Job {
			active[id] = svc.Name
		}
	}
	// Every service is ready: the deployment completes, a cancellation no
	// longer stops it halfway.
	ctx = context.WithoutCancel(ctx)
	// Join the networks of the apps that use this one (a database). The
	// deployment has succeeded by now; a failure here is reported, and the
	// next deployment or link change tries again.
	if err := m.attach(ctx, dc, spec.AppID, active, spec.Attach); err != nil {
		lg.printf("Warning: %v", err)
	}

	// Services removed from the compose file.
	for name, list := range byService {
		if !slices.ContainsFunc(spec.Services, func(s agentproto.ServiceSpec) bool { return s.Name == name }) {
			if name != "" {
				lg.printf("Removing service %s: it is no longer in the compose file", name)
			}
			retired = append(retired, list...)
		}
	}
	if switched {
		m.refresh(ctx)
		lg.printf("Traffic switched to the new container")
	}
	if slices.ContainsFunc(retired, func(c docker.ContainerSummary) bool { return c.State == "running" }) {
		select {
		case <-ctx.Done():
		case <-time.After(appDrainDelay):
		}
	}
	for _, c := range retired {
		if c.State == "running" {
			if err := dc.ContainerStop(ctx, c.ID, appStopTimeout); err != nil {
				lg.printf("Stop failed: %v", err)
			}
		}
		if err := dc.ContainerRemove(ctx, c.ID, true); err != nil {
			lg.printf("Remove failed: %v", err)
		}
	}
	m.refresh(ctx)
	return digests, nil
}

// attach connects the containers (ID -> service name) to the networks of
// the apps in attach, reachable there by their service name (and their
// host name, if they have a full one: the mail server's, which its TLS
// certificate names), and disconnects them from the networks of other apps.
func (m *appsManager) attach(ctx context.Context, dc *docker.Client, appID int64, containers map[string]string, attach []int64) error {
	want := map[string]int64{}
	for _, id := range attach {
		want[appNetwork(id)] = id
	}
	var errs []error
	for _, cid := range slices.Sorted(maps.Keys(containers)) {
		service := containers[cid]
		info, err := dc.ContainerInspect(ctx, cid)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for name := range info.NetworkSettings.Networks {
			if _, ok := want[name]; !ok && name != appNetwork(appID) && strings.HasPrefix(name, "basalt-app-") {
				if err := dc.NetworkDisconnect(ctx, name, cid, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
					errs = append(errs, fmt.Errorf("disconnect %s from %s: %w", service, name, err))
				}
			}
		}
		for _, name := range slices.Sorted(maps.Keys(want)) {
			if _, ok := info.NetworkSettings.Networks[name]; ok {
				continue
			}
			if err := m.ensureNetwork(ctx, dc, want[name]); err != nil {
				errs = append(errs, err)
				continue
			}
			aliases := []string{service}
			if h := info.Config.Hostname; strings.Contains(h, ".") {
				aliases = append(aliases, h)
			}
			ep := &docker.EndpointSettings{Aliases: aliases}
			if err := dc.NetworkConnect(ctx, name, cid, ep); err != nil && !errors.Is(err, docker.ErrConflict) {
				errs = append(errs, fmt.Errorf("connect %s to the network of app %d: %w", service, want[name], err))
			}
		}
	}
	return errors.Join(errs...)
}

// Attach makes the running services of an app join exactly the networks of
// the apps in req.Attach.
func (m *appsManager) Attach(ctx context.Context, req agentproto.AppAttach) error {
	if req.AppID <= 0 {
		return errors.New("invalid app id")
	}
	if err := agentproto.ValidateAttach(req.AppID, req.Attach); err != nil {
		return err
	}
	dc, err := m.client()
	if err != nil {
		return err
	}
	defer m.lock(req.AppID)()
	list, err := m.containers(ctx, dc, req.AppID) // newest first
	if err != nil {
		return err
	}
	current := map[string]string{}
	seen := map[string]bool{}
	for _, c := range list {
		svc := c.Labels[labelService]
		if svc == "" || seen[svc] || c.State != "running" {
			continue
		}
		seen[svc] = true
		current[c.ID] = svc
	}
	return m.attach(ctx, dc, req.AppID, current, req.Attach)
}

// authFor picks the credentials for the registry of image.
func authFor(auths []docker.AuthConfig, image string) *docker.AuthConfig {
	addr := docker.ServerAddress(docker.RegistryHost(image))
	for i := range auths {
		if auths[i].ServerAddress == addr {
			return &auths[i]
		}
	}
	return nil
}

// serviceHash identifies everything a service's container is created from;
// a container with the same hash needs no replacement. The image counts by
// its ID: the same image named by tag or by digest is the same.
func serviceHash(appID int64, svc agentproto.ServiceSpec, imageID string) string {
	svc.Image = ""
	b, _ := json.Marshal(struct {
		App     int64
		Service agentproto.ServiceSpec
		Image   string
	}{appID, svc, imageID})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

func stopTimeout(svc agentproto.ServiceSpec) time.Duration {
	if svc.StopGrace > 0 {
		return time.Duration(svc.StopGrace) * time.Second
	}
	return appStopTimeout
}

// waitDependency checks, for a service kept as it was, that it is healthy
// when something depends on it being healthy.
func (m *appsManager) waitDependency(ctx context.Context, dc *docker.Client, svc agentproto.ServiceSpec, id string, lg *deployLog) error {
	if svc.Health == nil || len(svc.Health.Test) == 0 || svc.Health.Test[0] == "NONE" {
		return nil
	}
	info, err := dc.ContainerInspect(ctx, id)
	if err != nil {
		return err
	}
	if info.State.Health == nil || info.State.Health.Status == "healthy" {
		return nil
	}
	return m.waitContainer(ctx, dc, 0, 0, id, "Service "+svc.Name+": ", lg)
}

// waitJob waits for a service that runs to completion to exit successfully.
func (m *appsManager) waitJob(ctx context.Context, dc *docker.Client, name, id string, lg *deployLog) error {
	ctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	lg.printf("Service %s: waiting for it to finish", name)
	for {
		info, err := dc.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		switch st := info.State; {
		case st.Status == "exited" && st.ExitCode == 0 && info.RestartCount == 0:
			lg.printf("Service %s finished", name)
			return nil
		case st.Status == "exited" || st.Status == "dead" || st.Restarting || info.RestartCount > 0:
			return fmt.Errorf("service %s failed (exit code %d)", name, st.ExitCode)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("service %s did not finish within %s", name, jobTimeout)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (m *appsManager) serviceConfig(spec agentproto.AppSpec, svc agentproto.ServiceSpec, hash string) docker.ContainerConfig {
	env := make([]string, 0, len(svc.Env))
	for _, k := range slices.Sorted(maps.Keys(svc.Env)) {
		env = append(env, k+"="+svc.Env[k])
	}
	init, pids := true, int64(4096)
	if svc.Init != nil {
		init = *svc.Init
	}
	capDrop := slices.Clone(appCapDrop)
	for _, c := range svc.CapDrop {
		if !slices.Contains(capDrop, c) {
			capDrop = append(capDrop, c)
		}
	}
	hc := &docker.HostConfig{
		NetworkMode:    appNetwork(spec.AppID),
		RestartPolicy:  docker.RestartPolicy{Name: svc.Restart, MaximumRetryCount: svc.MaxRetries},
		CapDrop:        capDrop,
		SecurityOpt:    []string{"no-new-privileges:true"},
		Init:           &init,
		PidsLimit:      &pids,
		Memory:         int64(svc.MemoryMB) << 20,
		NanoCPUs:       int64(svc.CPUs * 1e9),
		ReadonlyRootfs: svc.ReadOnly,
		ShmSize:        int64(svc.ShmSizeMB) << 20,
		ExtraHosts:     svc.ExtraHosts,
		LogConfig:      &docker.LogConfig{Type: "local", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
	}
	if len(svc.Tmpfs) > 0 {
		hc.Tmpfs = maps.Clone(svc.Tmpfs)
	}
	for _, mnt := range svc.Mounts {
		hc.Mounts = append(hc.Mounts, docker.Mount{Type: "volume", Source: appVolume(spec.AppID, mnt.Volume),
			Target: mnt.Path, ReadOnly: mnt.ReadOnly})
	}
	labels := maps.Clone(svc.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelApp] = strconv.FormatInt(spec.AppID, 10)
	labels[labelDeployment] = strconv.FormatInt(spec.DeploymentID, 10)
	labels[labelService] = svc.Name
	labels[labelConfig] = hash
	if svc.Port > 0 {
		labels[labelPort] = strconv.Itoa(svc.Port)
	}
	cfg := docker.ContainerConfig{
		Image: svc.Image, Cmd: svc.Command, Entrypoint: svc.Entrypoint, Env: env,
		User: svc.User, WorkingDir: svc.WorkingDir, Hostname: svc.Hostname,
		Tty: svc.Tty, OpenStdin: svc.OpenStdin, StopSignal: svc.StopSignal,
		Labels: labels, Healthcheck: svc.Health, HostConfig: hc,
		NetworkingConfig: &docker.NetworkingConfig{EndpointsConfig: map[string]*docker.EndpointSettings{
			appNetwork(spec.AppID): {Aliases: append([]string{svc.Name}, svc.Aliases...)},
		}},
	}
	if svc.StopGrace > 0 {
		t := svc.StopGrace
		cfg.StopTimeout = &t
	}
	if m.publish && svc.Port > 0 {
		p := strconv.Itoa(svc.Port) + "/tcp"
		cfg.ExposedPorts = map[string]struct{}{p: {}}
		hc.PortBindings = map[string][]docker.PortBinding{p: {{HostIP: "127.0.0.1"}}}
	}
	// The mail server's ports, on every address of the host. A container
	// with host ports always has volumes, so the old one is stopped first.
	for _, hp := range svc.HostPorts {
		p := strconv.Itoa(hp.Container) + "/tcp"
		if cfg.ExposedPorts == nil {
			cfg.ExposedPorts = map[string]struct{}{}
		}
		if hc.PortBindings == nil {
			hc.PortBindings = map[string][]docker.PortBinding{}
		}
		cfg.ExposedPorts[p] = struct{}{}
		hc.PortBindings[p] = []docker.PortBinding{{HostPort: strconv.Itoa(hp.Host)}}
	}
	return cfg
}

// composeStatus describes every service of a compose app from its
// containers (newest first); the top-level fields describe service, or
// the published one.
func (m *appsManager) composeStatus(ctx context.Context, dc *docker.Client, list []docker.ContainerSummary, service string) (agentproto.AppStatus, error) {
	current := map[string]docker.ContainerSummary{}
	for _, c := range list {
		name := c.Labels[labelService]
		if prev, ok := current[name]; name == "" || ok && (prev.State == "running" || c.State != "running") {
			continue
		}
		current[name] = c
	}
	var st agentproto.AppStatus
	main := ""
	for _, name := range slices.Sorted(maps.Keys(current)) {
		c := current[name]
		info, err := dc.ContainerInspect(ctx, c.ID)
		if err != nil {
			continue // removed meanwhile
		}
		s := agentproto.ServiceStatus{
			Name: name, Image: info.Config.Image, State: info.State.Status,
			DeploymentID: labelInt(info.Config.Labels, labelDeployment), RestartCount: info.RestartCount,
			ExitCode: info.State.ExitCode, OOMKilled: info.State.OOMKilled,
		}
		if info.State.Health != nil {
			s.Health = info.State.Health.Status
		}
		if t, err := time.Parse(time.RFC3339Nano, info.State.StartedAt); err == nil && t.Year() > 1 {
			s.StartedAt = t
		}
		st.Services = append(st.Services, s)
		if name == service || service == "" && main == "" && c.Labels[labelPort] != "" {
			main = name
			st.State, st.Health, st.DeploymentID, st.StartedAt = s.State, s.Health, s.DeploymentID, s.StartedAt
			st.RestartCount, st.ExitCode, st.OOMKilled, st.Error = s.RestartCount, s.ExitCode, s.OOMKilled, info.State.Error
		}
	}
	if main == "" {
		st.State = "running"
		for _, s := range st.Services {
			if s.State != "running" && !(s.State == "exited" && s.ExitCode == 0) {
				st.State = s.State
				break
			}
		}
	}
	return st, nil
}
