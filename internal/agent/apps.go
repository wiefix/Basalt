package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
)

// Container labels. Docker is the source of truth for what runs on the
// host: the agent rebuilds its view from these after any restart.
const (
	labelApp        = "basalt.app"
	labelDeployment = "basalt.deployment"
	labelPort       = "basalt.port"
	labelVolume     = "basalt.volume"
	// Compose apps: the service a container runs and a hash of its
	// settings, which tells whether a deployment must replace it.
	labelService = "basalt.service"
	labelConfig  = "basalt.config"
)

const (
	appReadyTimeout = 2 * time.Minute
	appStopTimeout  = 10 * time.Second
	// appDrainDelay lets requests the proxy sent to the previous container
	// just before the switch finish before that container is stopped.
	appDrainDelay = 2 * time.Second
	appRefreshGap = 10 * time.Second
	maxDeployLog  = 256 << 10
	maxAppLogs    = 1 << 20
	maxLogTail    = 5000
)

// Capabilities dropped from Docker's default set: none are needed by web
// apps, and they help attacks on the host network or devices.
var appCapDrop = []string{"NET_RAW", "MKNOD", "AUDIT_WRITE", "SETFCAP"}

// appsManager runs apps as Docker containers, one container per app at a
// time (two during a deployment). Each app has its own bridge network, so
// apps cannot reach each other; the proxy reaches the current container on
// that network, without publishing ports on the host.
type appsManager struct {
	log    *slog.Logger
	socket string
	dc     *docker.Client
	// publish binds app ports to 127.0.0.1 instead of dialling container
	// IPs, for development hosts (Docker Desktop) whose container networks
	// are not routable from the host.
	publish bool
	// onChange is called when the upstream of any app changes; it must
	// deliver Upstreams to the proxy.
	onChange func()

	lmu   sync.Mutex
	locks map[int64]*sync.Mutex

	umu       sync.Mutex
	upstreams map[int64]string
	lastErr   string

	dataDir   string        // builds and known hosts live here
	gitCAFile string        // extra CA bundle for https git remotes
	builds    chan struct{} // limits concurrent builds
}

func newAppsManager(cfg Config, log *slog.Logger) *appsManager {
	m := &appsManager{
		log: log, socket: cfg.DockerSocket, publish: runtime.GOOS != "linux",
		locks: map[int64]*sync.Mutex{}, upstreams: map[int64]string{}, onChange: func() {},
		dataDir: cfg.DataDir, gitCAFile: cfg.GitCAFile, builds: make(chan struct{}, maxConcurrentBuilds),
	}
	if cfg.DockerSocket != "" {
		m.dc = docker.New(cfg.DockerSocket)
	}
	return m
}

func appNetwork(appID int64) string { return "basalt-app-" + strconv.FormatInt(appID, 10) }

func appContainer(appID, deploymentID int64) string {
	return appNetwork(appID) + "-d" + strconv.FormatInt(deploymentID, 10)
}

func appVolume(appID int64, name string) string { return appNetwork(appID) + "-" + name }

func (m *appsManager) client() (*docker.Client, error) {
	if m.dc == nil || !docker.Available(m.socket) {
		return nil, errors.New("Docker is not available on this server")
	}
	return m.dc, nil
}

// lock serialises operations on one app.
func (m *appsManager) lock(appID int64) func() {
	m.lmu.Lock()
	l := m.locks[appID]
	if l == nil {
		l = &sync.Mutex{}
		m.locks[appID] = l
	}
	m.lmu.Unlock()
	l.Lock()
	return l.Unlock
}

// Upstreams returns the dial address of every app's current container.
func (m *appsManager) Upstreams() map[int64]string {
	m.umu.Lock()
	defer m.umu.Unlock()
	return maps.Clone(m.upstreams)
}

// deployLog collects the messages shown to the user, bounded in size, and
// streams them to the panel as they happen when it follows the deployment.
type deployLog struct {
	bytes.Buffer
	stream func(any) error // nil when not followed
}

func (l *deployLog) printf(format string, args ...any) {
	l.write(fmt.Sprintf(time.Now().UTC().Format("15:04:05")+" "+format+"\n", args...))
}

func (l *deployLog) write(s string) {
	if l.full() {
		return
	}
	l.forceWrite(s)
}

func (l *deployLog) full() bool { return l.Len() >= maxDeployLog }

// forceWrite appends even to a full log, for the final lines that explain
// the outcome.
func (l *deployLog) forceWrite(s string) {
	l.WriteString(s)
	if l.stream != nil {
		// A lost frame only affects the live view; the full log is in the reply.
		_ = l.stream(agentproto.LogChunk{Text: s})
	}
}

// Deploy starts a new container for spec and, once it is ready, switches
// the proxy to it and stops the previous one. If the new container fails,
// the previous one keeps serving. stream (may be nil) receives the log as
// it is written.
func (m *appsManager) Deploy(ctx context.Context, spec agentproto.AppSpec, stream func(any) error) agentproto.AppDeployResponse {
	lg := deployLog{stream: stream}
	fail := func(err error) agentproto.AppDeployResponse {
		lg.forceWrite(fmt.Sprintf("%s Deployment failed: %v\n", time.Now().UTC().Format("15:04:05"), err))
		return agentproto.AppDeployResponse{Error: err.Error(), Log: lg.String()}
	}
	if err := spec.Validate(); err != nil {
		return fail(err)
	}
	dc, err := m.client()
	if err != nil {
		return fail(err)
	}
	defer m.lock(spec.AppID)()

	if len(spec.Services) > 0 {
		digests, err := m.deployCompose(ctx, dc, spec, &lg)
		if err != nil {
			return fail(err)
		}
		lg.printf("Deployment succeeded")
		return agentproto.AppDeployResponse{OK: true, Log: lg.String(), Digests: digests}
	}

	var digest, imageID, commit, subject string
	if spec.Git != nil {
		if commit, subject, err = m.buildImage(ctx, dc, spec, &lg); err != nil {
			return fail(err)
		}
		info, err := dc.ImageInspect(ctx, spec.Image)
		if err != nil {
			return fail(err)
		}
		digest, imageID = info.ID, info.ID
	} else if digest, imageID, err = m.pull(ctx, dc, spec.Image, spec.RegistryAuth, &lg); err != nil {
		return fail(err)
	}
	if err := m.ensureNetwork(ctx, dc, spec.AppID); err != nil {
		return fail(err)
	}
	for _, v := range spec.Volumes {
		labels := map[string]string{labelApp: strconv.FormatInt(spec.AppID, 10), labelVolume: v.Name}
		if err := dc.VolumeCreate(ctx, appVolume(spec.AppID, v.Name), labels); err != nil {
			return fail(fmt.Errorf("create volume %s: %w", v.Name, err))
		}
	}

	name := appContainer(spec.AppID, spec.DeploymentID)
	// A container left by an earlier attempt of the same deployment.
	if err := dc.ContainerRemove(ctx, name, true); err != nil {
		return fail(err)
	}
	id, err := dc.ContainerCreate(ctx, name, m.containerConfig(spec))
	if err != nil {
		return fail(fmt.Errorf("create container: %w", err))
	}
	lg.printf("Starting container %s", name)
	if err := dc.ContainerStart(ctx, id); err != nil {
		dc.ContainerRemove(context.WithoutCancel(ctx), id, true)
		return fail(fmt.Errorf("start container: %w", err))
	}
	if err := m.waitReady(ctx, dc, spec, id, &lg); err != nil {
		lg.printf("Last output of the new container:")
		lg.write(m.tail(context.WithoutCancel(ctx), dc, id, 50))
		dc.ContainerRemove(context.WithoutCancel(ctx), id, true)
		return fail(err)
	}

	// The new container has the highest deployment ID, so it becomes the
	// upstream; the proxy switches before the old one stops.
	m.refresh(ctx)
	lg.printf("Traffic switched to the new container")
	select {
	case <-ctx.Done():
	case <-time.After(appDrainDelay):
	}
	m.removeOlder(ctx, dc, spec.AppID, spec.DeploymentID, &lg)
	if n := m.pruneImages(ctx, dc, spec.PruneImages, imageID); n > 0 {
		lg.printf("Removed %d image(s) of older deployments", n)
	}
	lg.printf("Deployment succeeded")
	return agentproto.AppDeployResponse{OK: true, Log: lg.String(), ContainerID: id, ImageDigest: digest,
		Commit: commit, CommitMessage: subject}
}

// pull fetches the image unless it is pinned by digest and already present.
// It returns the repository digest of the image and its local ID.
func (m *appsManager) pull(ctx context.Context, dc *docker.Client, ref string, auth *docker.AuthConfig, lg *deployLog) (digest, id string, err error) {
	present := false
	if strings.Contains(ref, "@sha256:") {
		_, err := dc.ImageInspect(ctx, ref)
		present = err == nil
	}
	if present {
		lg.printf("Image %s is already present", ref)
	} else {
		if auth != nil {
			lg.printf("Pulling %s (signed in to %s as %s)", ref, docker.RegistryHost(ref), auth.Username)
		} else {
			lg.printf("Pulling %s", ref)
		}
		layers := 0
		err := dc.Pull(ctx, ref, auth, func(p docker.PullProgress) {
			switch {
			case p.Status == "Pull complete":
				layers++
			case strings.HasPrefix(p.Status, "Digest:"), strings.HasPrefix(p.Status, "Status:"):
				lg.printf("%s", p.Status)
			}
		})
		if err != nil {
			if auth == nil && docker.RegistryHost(ref) != docker.DockerHub {
				return "", "", fmt.Errorf("pull %s: %w (a private image needs credentials for %s under Registries)", ref, err, docker.RegistryHost(ref))
			}
			return "", "", fmt.Errorf("pull %s: %w", ref, err)
		}
		if layers > 0 {
			lg.printf("Downloaded %d layers", layers)
		}
	}
	info, err := dc.ImageInspect(ctx, ref)
	if err != nil {
		return "", "", err
	}
	if len(info.RepoDigests) > 0 {
		return info.RepoDigests[0], info.ID, nil
	}
	return info.ID, info.ID, nil
}

// pruneImages removes the references to images of earlier deployments and
// returns how many it removed. Docker deletes an image once no reference to
// it is left, and refuses while a container uses it, so images still
// running or shared with other references stay. A tag is removed only while
// it still points at the pruned image, never the image of keepID.
func (m *appsManager) pruneImages(ctx context.Context, dc *docker.Client, refs []agentproto.ImageRef, keepID string) int {
	removed := 0
	for _, r := range refs {
		target := r.Digest
		if target == "" {
			target = r.Ref
		}
		info, err := dc.ImageInspect(ctx, target)
		if err != nil || info.ID == keepID {
			continue // already gone, or what now runs
		}
		gone := false
		if r.Ref != target {
			if t, err := dc.ImageInspect(ctx, r.Ref); err == nil && t.ID == info.ID {
				// Removing a repository's last tag drops its digest too.
				gone = m.removeImageRef(ctx, dc, r.Ref)
			}
		}
		if m.removeImageRef(ctx, dc, target) || gone {
			removed++
		}
	}
	return removed
}

func (m *appsManager) removeImageRef(ctx context.Context, dc *docker.Client, ref string) bool {
	err := dc.ImageRemove(ctx, ref)
	switch {
	case err == nil:
		return true
	case errors.Is(err, docker.ErrNotFound), errors.Is(err, docker.ErrConflict):
		return false // gone, or still in use
	}
	m.log.Warn("remove old image", "image", ref, "err", err)
	return false
}

func (m *appsManager) ensureNetwork(ctx context.Context, dc *docker.Client, appID int64) error {
	name := appNetwork(appID)
	if _, err := dc.NetworkInspect(ctx, name); err == nil {
		return nil
	} else if !errors.Is(err, docker.ErrNotFound) {
		return err
	}
	_, err := dc.NetworkCreate(ctx, docker.NetworkConfig{
		Name: name, Labels: map[string]string{labelApp: strconv.FormatInt(appID, 10)},
	})
	if errors.Is(err, docker.ErrConflict) {
		return nil // created concurrently
	}
	return err
}

func (m *appsManager) containerConfig(spec agentproto.AppSpec) docker.ContainerConfig {
	env := make([]string, 0, len(spec.Env))
	for _, k := range slices.Sorted(maps.Keys(spec.Env)) {
		env = append(env, k+"="+spec.Env[k])
	}
	init, pids := true, int64(4096)
	hc := &docker.HostConfig{
		NetworkMode:   appNetwork(spec.AppID),
		RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
		CapDrop:       appCapDrop,
		SecurityOpt:   []string{"no-new-privileges:true"},
		Init:          &init,
		PidsLimit:     &pids,
		Memory:        int64(spec.MemoryMB) << 20,
		NanoCPUs:      int64(spec.CPUs * 1e9),
		LogConfig:     &docker.LogConfig{Type: "local", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
	}
	for _, v := range spec.Volumes {
		hc.Mounts = append(hc.Mounts, docker.Mount{Type: "volume", Source: appVolume(spec.AppID, v.Name), Target: v.Path})
	}
	cfg := docker.ContainerConfig{
		Image: spec.Image,
		Env:   env,
		Labels: map[string]string{
			labelApp:        strconv.FormatInt(spec.AppID, 10),
			labelDeployment: strconv.FormatInt(spec.DeploymentID, 10),
			labelPort:       strconv.Itoa(spec.Port),
		},
		HostConfig: hc,
		NetworkingConfig: &docker.NetworkingConfig{EndpointsConfig: map[string]*docker.EndpointSettings{
			appNetwork(spec.AppID): {Aliases: []string{"app"}},
		}},
	}
	if m.publish && spec.Port > 0 {
		p := strconv.Itoa(spec.Port) + "/tcp"
		cfg.ExposedPorts = map[string]struct{}{p: {}}
		hc.PortBindings = map[string][]docker.PortBinding{p: {{HostIP: "127.0.0.1"}}}
	}
	return cfg
}

// waitReady waits until the container is healthy (when the image defines a
// health check), answers HTTP on its port (any status: the app is up), or,
// without a port, keeps running for a few seconds. An HTTP answer rather
// than an open TCP port: port forwarders accept connections before the app
// listens.
func (m *appsManager) waitReady(ctx context.Context, dc *docker.Client, spec agentproto.AppSpec, id string, lg *deployLog) error {
	return m.waitContainer(ctx, dc, spec.AppID, spec.Port, id, "", lg)
}

// waitContainer is waitReady for any container of an app; prefix (e.g.
// "Service db: ") starts its log lines.
func (m *appsManager) waitContainer(ctx context.Context, dc *docker.Client, appID int64, port int, id, prefix string, lg *deployLog) error {
	ctx, cancel := context.WithTimeout(ctx, appReadyTimeout)
	defer cancel()
	start := time.Now()
	logged := false
	say := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		if prefix != "" {
			msg = prefix + strings.ToLower(msg[:1]) + msg[1:]
		}
		lg.printf("%s", msg)
	}
	for {
		info, err := dc.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		st := info.State
		switch {
		case st.Restarting || info.RestartCount > 0:
			return fmt.Errorf("%sthe container keeps stopping and restarting", prefix)
		case st.Status == "exited" || st.Status == "dead":
			return fmt.Errorf("%sthe container stopped (exit code %d)", prefix, st.ExitCode)
		case st.Health != nil:
			if !logged {
				say("Waiting for the health check")
				logged = true
			}
			if st.Health.Status == "healthy" {
				say("Container is healthy")
				return nil
			}
			if st.Health.Status == "unhealthy" {
				return fmt.Errorf("%sthe container's health check failed", prefix)
			}
		case port > 0:
			if !logged {
				say("Waiting for HTTP on port %d", port)
				logged = true
			}
			if addr := m.upstreamOf(info, appID, port); addr != "" && answersHTTP(ctx, addr) {
				say("Port %d is serving", port)
				return nil
			}
		case time.Since(start) > 3*time.Second:
			say("Container is running")
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%sthe container did not become ready within %s", prefix, appReadyTimeout)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// answersHTTP reports whether something answers an HTTP request at addr.
func answersHTTP(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "basalt-readiness")
	resp, err := readinessClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

var readinessClient = &http.Client{
	Transport:     &http.Transport{DisableKeepAlives: true, Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// upstreamOf returns where the proxy reaches port of a container.
func (m *appsManager) upstreamOf(info *docker.ContainerInfo, appID int64, port int) string {
	if m.publish {
		for _, b := range info.NetworkSettings.Ports[strconv.Itoa(port)+"/tcp"] {
			if b.HostPort != "" {
				return net.JoinHostPort("127.0.0.1", b.HostPort)
			}
		}
		return ""
	}
	if n, ok := info.NetworkSettings.Networks[appNetwork(appID)]; ok && n.IPAddress != "" {
		return net.JoinHostPort(n.IPAddress, strconv.Itoa(port))
	}
	return ""
}

// containers returns the app's containers, newest deployment first.
func (m *appsManager) containers(ctx context.Context, dc *docker.Client, appID int64) ([]docker.ContainerSummary, error) {
	labels := map[string]string{labelApp: ""}
	if appID > 0 {
		labels[labelApp] = strconv.FormatInt(appID, 10)
	}
	list, err := dc.ContainerList(ctx, labels)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(list, func(a, b docker.ContainerSummary) int {
		return -cmpInt(labelInt(a.Labels, labelDeployment), labelInt(b.Labels, labelDeployment))
	})
	return list, nil
}

func labelInt(labels map[string]string, key string) int64 {
	n, _ := strconv.ParseInt(labels[key], 10, 64)
	return n
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// removeOlder stops and removes containers of earlier deployments.
func (m *appsManager) removeOlder(ctx context.Context, dc *docker.Client, appID, deploymentID int64, lg *deployLog) {
	list, err := m.containers(ctx, dc, appID)
	if err != nil {
		lg.printf("Could not list old containers: %v", err)
		return
	}
	for _, c := range list {
		if labelInt(c.Labels, labelDeployment) >= deploymentID {
			continue
		}
		if c.State == "running" {
			lg.printf("Stopping the previous container")
			if err := dc.ContainerStop(ctx, c.ID, appStopTimeout); err != nil {
				lg.printf("Stop failed: %v", err)
			}
		}
		if err := dc.ContainerRemove(ctx, c.ID, true); err != nil {
			lg.printf("Remove failed: %v", err)
		}
	}
}

// refresh recomputes every app's upstream from Docker and notifies the
// proxy when anything changed (e.g. container IPs after a Docker restart).
func (m *appsManager) refresh(ctx context.Context) {
	dc, err := m.client()
	if err != nil {
		return
	}
	list, err := m.containers(ctx, dc, 0)
	if err != nil {
		m.report(err)
		return
	}
	next := map[int64]string{}
	for _, c := range list { // newest deployment first
		app := labelInt(c.Labels, labelApp)
		port := int(labelInt(c.Labels, labelPort))
		if _, done := next[app]; done || app <= 0 || port <= 0 || c.State != "running" {
			continue
		}
		info, err := dc.ContainerInspect(ctx, c.ID)
		if err != nil {
			continue
		}
		if up := m.upstreamOf(info, app, port); up != "" {
			next[app] = up
		}
	}
	m.umu.Lock()
	changed := !maps.Equal(next, m.upstreams)
	m.upstreams = next
	m.lastErr = ""
	m.umu.Unlock()
	if changed {
		m.onChange()
	}
}

func (m *appsManager) report(err error) {
	m.umu.Lock()
	defer m.umu.Unlock()
	if err.Error() != m.lastErr {
		m.lastErr = err.Error()
		m.log.Warn("docker", "err", err)
	}
}

func (m *appsManager) run(ctx context.Context) {
	if m.dc == nil {
		return
	}
	t := time.NewTicker(appRefreshGap)
	defer t.Stop()
	for {
		m.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// current is the container serving the app (or one service of a compose
// app): the newest running one, or the newest of any state.
func (m *appsManager) current(ctx context.Context, dc *docker.Client, appID int64, service string) (*docker.ContainerSummary, error) {
	list, err := m.containers(ctx, dc, appID)
	if err != nil {
		return nil, err
	}
	if service != "" {
		list = slices.DeleteFunc(list, func(c docker.ContainerSummary) bool { return c.Labels[labelService] != service })
	}
	if len(list) == 0 {
		return nil, nil
	}
	for i := range list {
		if list[i].State == "running" {
			return &list[i], nil
		}
	}
	return &list[0], nil
}

func (m *appsManager) Status(ctx context.Context, appID int64, service string) (agentproto.AppStatus, error) {
	dc, err := m.client()
	if err != nil {
		return agentproto.AppStatus{}, err
	}
	list, err := m.containers(ctx, dc, appID)
	if err != nil {
		return agentproto.AppStatus{}, err
	}
	if slices.ContainsFunc(list, func(c docker.ContainerSummary) bool { return c.Labels[labelService] != "" }) {
		return m.composeStatus(ctx, dc, list, service)
	}
	c, err := m.current(ctx, dc, appID, "")
	if err != nil {
		return agentproto.AppStatus{}, err
	}
	if c == nil {
		return agentproto.AppStatus{State: "missing"}, nil
	}
	info, err := dc.ContainerInspect(ctx, c.ID)
	if err != nil {
		return agentproto.AppStatus{}, err
	}
	st := agentproto.AppStatus{
		State: info.State.Status, DeploymentID: labelInt(info.Config.Labels, labelDeployment),
		RestartCount: info.RestartCount, ExitCode: info.State.ExitCode, OOMKilled: info.State.OOMKilled,
		Error: info.State.Error,
	}
	if info.State.Health != nil {
		st.Health = info.State.Health.Status
	}
	if t, err := time.Parse(time.RFC3339Nano, info.State.StartedAt); err == nil && !t.IsZero() && t.Year() > 1 {
		st.StartedAt = t
	}
	return st, nil
}

// Logs returns the last lines of the current container's output (of one
// service for a compose app), stdout and stderr interleaved.
func (m *appsManager) Logs(ctx context.Context, appID int64, service string, tail int) (string, error) {
	dc, err := m.client()
	if err != nil {
		return "", err
	}
	c, err := m.current(ctx, dc, appID, service)
	if err != nil || c == nil {
		return "", err
	}
	return m.tail(ctx, dc, c.ID, min(max(tail, 1), maxLogTail)), nil
}

func (m *appsManager) tail(ctx context.Context, dc *docker.Client, id string, lines int) string {
	rc, err := dc.ContainerLogs(ctx, id, docker.LogsOptions{Tail: lines, Timestamps: true})
	if err != nil {
		return fmt.Sprintf("(logs unavailable: %v)\n", err)
	}
	defer rc.Close()
	var buf bytes.Buffer
	w := &limitedWriter{w: &buf, n: maxAppLogs}
	docker.Demux(rc, w, w)
	return buf.String()
}

// limitedWriter keeps the first n bytes and silently drops the rest.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// Delete removes the app's containers, network and the given images, and
// its volumes when asked to.
func (m *appsManager) Delete(ctx context.Context, appID int64, volumes bool, images []agentproto.ImageRef) error {
	if appID <= 0 {
		return errors.New("invalid app id")
	}
	dc, err := m.client()
	if err != nil {
		return err
	}
	defer m.lock(appID)()
	list, err := m.containers(ctx, dc, appID)
	if err != nil {
		return err
	}
	for _, c := range list {
		if err := dc.ContainerRemove(ctx, c.ID, true); err != nil {
			return err
		}
	}
	// Databases linked to the app joined its network: detach them first.
	if info, err := dc.NetworkInspect(ctx, appNetwork(appID)); err == nil {
		for id := range info.Containers {
			if err := dc.NetworkDisconnect(ctx, info.ID, id, true); err != nil && !errors.Is(err, docker.ErrNotFound) {
				return err
			}
		}
	}
	if err := dc.NetworkRemove(ctx, appNetwork(appID)); err != nil {
		return err
	}
	if err := agentproto.ValidateImageRefs(images); err != nil {
		return err
	}
	m.pruneImages(ctx, dc, images, "")
	if volumes {
		vols, err := dc.VolumeList(ctx, map[string]string{labelApp: strconv.FormatInt(appID, 10)})
		if err != nil {
			return err
		}
		for _, v := range vols {
			if err := dc.VolumeRemove(ctx, v.Name); err != nil {
				return err
			}
		}
	}
	m.log.Info("app removed", "app_id", appID, "volumes", volumes)
	m.refresh(ctx)
	return nil
}

// Sync removes containers of apps the panel no longer knows. Their volumes
// are kept: data is never deleted implicitly.
func (m *appsManager) Sync(ctx context.Context, known []int64) error {
	dc, err := m.client()
	if err != nil {
		return nil // nothing can be running
	}
	list, err := m.containers(ctx, dc, 0)
	if err != nil {
		return err
	}
	orphans := map[int64]bool{}
	for _, c := range list {
		if app := labelInt(c.Labels, labelApp); app > 0 && !slices.Contains(known, app) {
			orphans[app] = true
		}
	}
	for app := range orphans {
		m.log.Warn("removing containers of an app deleted from the panel", "app_id", app)
		if err := m.Delete(ctx, app, false, nil); err != nil {
			return err
		}
	}
	return nil
}
