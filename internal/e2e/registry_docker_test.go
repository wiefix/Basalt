package e2e

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/server"
)

// TestPrivateRegistryWithDocker deploys from a password-protected registry
// running in the local Docker, and checks that old images are removed.
// Opt-in like the other Docker tests.
func TestPrivateRegistryWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	ctx := context.Background()
	dc := docker.New(sock)
	const user, pass = "deployer", "registry pass 123"
	reg := startRegistry(t, dc, user, pass)
	auth := &docker.AuthConfig{Username: user, Password: pass, ServerAddress: reg}

	// Two different images in the private repository.
	repo := reg + "/private/who"
	for tag, src := range map[string]string{"a": "traefik/whoami:v1.10", "b": "traefik/whoami:v1.9.0"} {
		if err := dc.Pull(ctx, src, nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := dc.Tag(ctx, src, repo+":"+tag); err != nil {
			t.Fatal(err)
		}
		if err := dc.Push(ctx, repo+":"+tag, auth, nil); err != nil {
			t.Fatalf("push %s: %v", tag, err)
		}
	}
	dropRefs := func() {
		for _, src := range []string{"traefik/whoami:v1.10", "traefik/whoami:v1.9.0"} {
			info, err := dc.ImageInspect(ctx, src)
			if err != nil {
				continue
			}
			for _, r := range append(info.RepoTags, info.RepoDigests...) {
				if strings.HasPrefix(r, reg+"/") {
					dc.ImageRemove(ctx, r)
				}
			}
		}
	}
	dropRefs() // deployments must pull from the registry
	t.Cleanup(dropRefs)

	p := startPanelWith(t, server.Options{ImagesKept: 1})
	h := startHostWith(t, p, sock)
	r := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": mustAtoi(h.id), "name": "private", "image": repo + ":a", "port": 80,
		"domains": []string{"private.localhost"}, "tls": "internal",
	})
	aid := jsonID(r["app"].(map[string]any)["id"])
	base := "/api/v1/apps/" + aid
	t.Cleanup(func() {
		list, _ := dc.ContainerList(context.Background(), map[string]string{"basalt.app": aid})
		for _, c := range list {
			dc.ContainerRemove(context.Background(), c.ID, true)
		}
		dc.NetworkRemove(context.Background(), "basalt-app-"+aid)
	})
	deploy := func(body map[string]any) (map[string]any, string) {
		t.Helper()
		dep := p.call(http.StatusAccepted, "POST", base+"/deploy", body)["deployment"].(map[string]any)
		return followDeployment(t, p, base, jsonID(dep["id"]))
	}

	// Without credentials the pull is refused, with a hint.
	if d, log := deploy(map[string]any{}); d["status"] != "failed" || !strings.Contains(log, "credentials for "+reg) {
		t.Fatalf("deployment without credentials: %v\n%s", d, log)
	}
	p.call(http.StatusCreated, "POST", "/api/v1/registries", map[string]string{"host": reg, "username": user, "password": pass})
	d1, log := deploy(map[string]any{})
	if d1["status"] != "succeeded" || !strings.Contains(log, "signed in to "+reg+" as "+user) || strings.Contains(log, pass) {
		t.Fatalf("deployment with credentials: %v\n%s", d1, log)
	}
	eventually(t, "private image served", func() bool {
		code, body := h.get("private.localhost", "/")
		return code == 200 && strings.Contains(body, "Hostname")
	})
	digestA := d1["image_digest"].(string)

	// Moving to :b removes image :a (only one image is kept).
	p.call(200, "PUT", base, map[string]any{"image": repo + ":b", "port": 80})
	d2, log := deploy(map[string]any{})
	if d2["status"] != "succeeded" || !strings.Contains(log, "Removed 1 image") {
		t.Fatalf("second deployment: %v\n%s", d2, log)
	}
	for _, ref := range []string{repo + ":a", digestA} {
		if _, err := dc.ImageInspect(ctx, ref); !errors.Is(err, docker.ErrNotFound) {
			t.Fatalf("%s still present after pruning: %v", ref, err)
		}
	}
	if _, err := dc.ImageInspect(ctx, repo+":b"); err != nil {
		t.Fatalf("the running image was removed: %v", err)
	}

	// Rolling back pulls the pruned image again, with the credentials.
	rb, log := deploy(map[string]any{"rollback_to": mustAtoi(jsonID(d1["id"]))})
	if rb["status"] != "succeeded" || rb["image"] != digestA {
		t.Fatalf("rollback: %v\n%s", rb, log)
	}

	// Deleting the app removes its images.
	p.call(200, "DELETE", base, nil)
	for _, ref := range []string{repo + ":b", digestA} {
		if _, err := dc.ImageInspect(ctx, ref); !errors.Is(err, docker.ErrNotFound) {
			t.Fatalf("%s left after deleting the app: %v", ref, err)
		}
	}
}

// startRegistry runs a registry with basic authentication and returns its
// address as image references name it (127.0.0.1:port, which Docker allows
// over plain HTTP).
func startRegistry(t *testing.T, dc *docker.Client, user, pass string) string {
	t.Helper()
	ctx := context.Background()
	const image = "registry:2.8.3"
	if err := dc.Pull(ctx, image, nil, nil); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "breg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	hash, _ := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err := os.WriteFile(filepath.Join(dir, "htpasswd"), []byte(user+":"+string(hash)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A fixed port: Docker Desktop's daemon (in its VM) reaches a published
	// loopback port only under the same number as on the host.
	port := strconv.Itoa(freePort(t))
	name := "basalt-test-registry-" + strings.ReplaceAll(time.Now().Format("150405.000"), ".", "")
	id, err := dc.ContainerCreate(ctx, name, docker.ContainerConfig{
		Image: image,
		Env: []string{"REGISTRY_AUTH=htpasswd", "REGISTRY_AUTH_HTPASSWD_REALM=basalt-test",
			"REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd"},
		ExposedPorts: map[string]struct{}{"5000/tcp": {}},
		HostConfig: &docker.HostConfig{
			Mounts:       []docker.Mount{{Type: "bind", Source: dir, Target: "/auth", ReadOnly: true}},
			PortBindings: map[string][]docker.PortBinding{"5000/tcp": {{HostIP: "127.0.0.1", HostPort: port}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dc.ContainerRemove(context.Background(), id, true) })
	if err := dc.ContainerStart(ctx, id); err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:" + port
	eventually(t, "registry up", func() bool {
		resp, err := http.Get("http://" + addr + "/v2/")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusUnauthorized
	})
	return addr
}
