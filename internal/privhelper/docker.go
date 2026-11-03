package privhelper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/dockerproxy"
)

// MsgBuildKit makes sure the BuildKit container of builds without a
// Dockerfile runs (BuildKitRequest → BuildKitContainer).
const MsgBuildKit = "buildkit"

// BuildKitRequest names the agent's Docker namespace ("": none).
type BuildKitRequest struct {
	Namespace string `json:"namespace,omitempty"`
}

// BuildKitContainer is the container buildx reaches BuildKit in.
type BuildKitContainer struct {
	Name string `json:"name"`
}

// ServeDocker serves Docker's API of upstream (Docker's own socket) on a
// unix socket at path, through dockerproxy, to processes of uid only.
func ServeDocker(ctx context.Context, path string, uid int, upstream string, log *slog.Logger) error {
	l, err := listen(path, uid, log)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           dockerproxy.New(upstream, agentproto.BuildKitImage, log),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	log.Info("serving Docker", "socket", path, "docker", upstream, "agent_uid", uid)
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
		return err
	}
	return nil
}

var namespaceRE = regexp.MustCompile(`^[a-z0-9]{0,16}$`)

// BuildKitName is the BuildKit container of the agent's Docker namespace;
// its state volume is the name + "-state".
func BuildKitName(namespace string) string {
	if namespace == "" {
		return "basalt-buildkit"
	}
	return "basalt-" + namespace + "-buildkit"
}

const (
	labelBuildKit = "basalt.buildkit"
	// buildKitReady is what BuildKit logs once it serves.
	buildKitReady = "running server on"
	buildKitWait  = 30 * time.Second
)

// BuildKit makes sure the BuildKit container runs: privileged (BuildKit
// sandboxes the build steps itself), of the pinned image, with nothing
// the agent chose but the namespace. The agent's buildx reaches it with
// "buildctl dial-stdio", the one command the Docker proxy lets it run in a
// privileged container. Docker's own builder cannot export a build into
// an image store that is not containerd's, so builds without a Dockerfile
// need this one.
func (h *Helper) BuildKit(ctx context.Context, namespace string) (string, error) {
	if !namespaceRE.MatchString(namespace) {
		return "", errors.New("invalid Docker namespace")
	}
	if h.cfg.DockerSocket == "" || !docker.Available(h.cfg.DockerSocket) {
		return "", errors.New("Docker is not available on this server")
	}
	h.buildKitMu.Lock()
	defer h.buildKitMu.Unlock()
	dc, name := docker.New(h.cfg.DockerSocket), BuildKitName(namespace)
	c, err := dc.ContainerInspect(ctx, name)
	switch {
	case err == nil && c.Config.Image == agentproto.BuildKitImage && c.HostConfig.Privileged && c.Config.Labels[labelBuildKit] == "1":
		if c.State.Running {
			return name, nil
		}
		if err := dc.ContainerStart(ctx, name); err != nil {
			return "", fmt.Errorf("start BuildKit: %w", err)
		}
		return name, waitBuildKit(ctx, dc, name)
	case err == nil:
		// Another image (an update pinned a newer one), or not ours.
		if err := dc.ContainerRemove(ctx, name, true); err != nil {
			return "", fmt.Errorf("remove the old BuildKit container: %w", err)
		}
	case !errors.Is(err, docker.ErrNotFound):
		return "", err
	}
	// The builder buildx made itself before the helper did (its cache goes).
	old := "buildx_buildkit_" + strings.TrimSuffix(name, "-buildkit") + "-railpack0"
	if err := dc.ContainerRemove(ctx, old, true); err == nil {
		dc.VolumeRemove(ctx, old+"_state")
	}
	if _, err := dc.ImageInspect(ctx, agentproto.BuildKitImage); err != nil {
		h.log.Info("pulling BuildKit", "image", agentproto.BuildKitImage)
		if err := dc.Pull(ctx, agentproto.BuildKitImage, nil, nil); err != nil {
			return "", fmt.Errorf("pull BuildKit: %w", err)
		}
	}
	info, err := dc.Info(ctx)
	if err != nil {
		return "", err
	}
	userns := ""
	if info.Security().UserNS {
		// A privileged container cannot run in remapped user namespaces.
		userns = "host"
	}
	init := true
	if _, err := dc.ContainerCreate(ctx, name, docker.ContainerConfig{
		Image:  agentproto.BuildKitImage,
		Labels: map[string]string{labelBuildKit: "1"},
		HostConfig: &docker.HostConfig{
			Privileged:    true,
			UsernsMode:    userns,
			RestartPolicy: docker.RestartPolicy{Name: "unless-stopped"},
			Init:          &init,
			Mounts:        []docker.Mount{{Type: "volume", Source: name + "-state", Target: "/var/lib/buildkit"}},
			LogConfig:     &docker.LogConfig{Type: "local", Config: map[string]string{"max-size": "1m", "max-file": "2"}},
		},
	}); err != nil {
		return "", fmt.Errorf("create the BuildKit container: %w", err)
	}
	if err := dc.ContainerStart(ctx, name); err != nil {
		return "", fmt.Errorf("start BuildKit: %w", err)
	}
	h.log.Info("BuildKit container created", "name", name)
	return name, waitBuildKit(ctx, dc, name)
}

// waitBuildKit waits until BuildKit serves (a build dialing it sooner
// would fail).
func waitBuildKit(ctx context.Context, dc *docker.Client, name string) error {
	deadline := time.Now().Add(buildKitWait)
	for {
		rc, err := dc.ContainerLogs(ctx, name, docker.LogsOptions{Tail: 100})
		if err == nil {
			var out bytes.Buffer
			docker.Demux(rc, &out, &out)
			rc.Close()
			if strings.Contains(out.String(), buildKitReady) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.New("BuildKit did not start in time")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
