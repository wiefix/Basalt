package privhelper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
)

// fakeDaemon answers every request like Docker's /version.
func fakeDaemon(t *testing.T) string {
	t.Helper()
	path := socketPath(t)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Version":"29.0.0","ApiVersion":"1.52"}`)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return path
}

func serveDocker(t *testing.T, uid int, upstream string) string {
	t.Helper()
	path := socketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := ServeDocker(ctx, path, uid, upstream, quiet); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	for deadline := time.Now().Add(5 * time.Second); ; {
		if fi, err := os.Lstat(path); err == nil && fi.Mode().Perm() == 0o600 {
			return path
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper did not listen")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeDocker(t *testing.T) {
	up := fakeDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dc := docker.New(serveDocker(t, os.Geteuid(), up))
	if v, err := dc.Version(ctx); err != nil || v.Version != "29.0.0" {
		t.Fatalf("version: %v %v", v, err)
	}
	if _, err := dc.ContainerCreate(ctx, "x", docker.ContainerConfig{Image: "a", HostConfig: &docker.HostConfig{Privileged: true}}); err == nil || !strings.Contains(err.Error(), "refused by Basalt") {
		t.Fatalf("privileged container: %v", err)
	}
	// Only the agent's user.
	if _, err := docker.New(serveDocker(t, os.Geteuid()+1, up)).Version(ctx); err == nil {
		t.Fatal("a process of another user got an answer")
	}
	// No Docker: the agent learns it is not installed.
	_, err := docker.New(serveDocker(t, os.Geteuid(), filepath.Join(t.TempDir(), "none.sock"))).Version(ctx)
	if de := (*docker.Error)(nil); !errors.As(err, &de) || de.Status != http.StatusServiceUnavailable {
		t.Fatalf("without Docker: %v", err)
	}
}

func TestBuildKitRequest(t *testing.T) {
	h, err := New(Config{SitesRoot: "/srv/sites"}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"../x", "Big", "a-b", strings.Repeat("a", 17)} {
		if _, err := h.Handle(context.Background(), MsgBuildKit, json.RawMessage(`{"namespace":"`+ns+`"}`)); err == nil || !strings.Contains(err.Error(), "namespace") {
			t.Errorf("namespace %q: %v", ns, err)
		}
	}
	if _, err := h.BuildKit(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("without Docker: %v", err)
	}
	if BuildKitName("") != "basalt-buildkit" || BuildKitName("e2e") != "basalt-e2e-buildkit" {
		t.Error("names")
	}
}

// TestBuildKitLive makes the BuildKit container on Docker
// (BASALT_DOCKER_TESTS=1): privileged, the pinned image, served; asked
// again it is the same container.
func TestBuildKitLive(t *testing.T) {
	sock := os.Getenv("DOCKER_SOCKET")
	if os.Getenv("BASALT_DOCKER_TESTS") == "" || sock == "" {
		t.Skip("set BASALT_DOCKER_TESTS=1 and DOCKER_SOCKET")
	}
	h, err := New(Config{SitesRoot: "/srv/sites", DockerSocket: sock}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dc := docker.New(sock)
	name := BuildKitName("phtest")
	t.Cleanup(func() {
		dc.ContainerRemove(context.Background(), name, true)
		dc.VolumeRemove(context.Background(), name+"-state")
	})
	if got, err := h.BuildKit(ctx, "phtest"); err != nil || got != name {
		t.Fatalf("%q %v", got, err)
	}
	c, err := dc.ContainerInspect(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if !c.HostConfig.Privileged || c.Config.Image != agentproto.BuildKitImage || !c.State.Running {
		t.Fatalf("container: %+v", c)
	}
	if _, err := h.BuildKit(ctx, "phtest"); err != nil {
		t.Fatal(err)
	}
	if c2, _ := dc.ContainerInspect(ctx, name); c2 == nil || c2.ID != c.ID {
		t.Fatal("the container was made again")
	}
	// The agent's buildx may dial it through the proxy, nothing more.
	proxy := serveDocker(t, os.Geteuid(), sock)
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", proxy)
		}}}
	for cmd, ok := range map[string]bool{`["buildctl","dial-stdio"]`: true, `["sh","-c","id"]`: false} {
		resp, err := hc.Post("http://docker/v1.44/containers/"+name+"/exec", "application/json",
			strings.NewReader(`{"AttachStdout":true,"Cmd":`+cmd+`}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if (resp.StatusCode == http.StatusCreated) != ok {
			t.Errorf("exec %s: %d", cmd, resp.StatusCode)
		}
	}
}
