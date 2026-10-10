package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDaemon serves h on a unix socket and returns a client for it.
func fakeDaemon(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	// Unix socket paths are limited to ~104 bytes; keep it short.
	dir, err := os.MkdirTemp("/tmp", "bd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	if !Available(sock) {
		t.Fatal("socket not detected")
	}
	return New(sock)
}

func TestVersionedPathsAndErrors(t *testing.T) {
	var paths []string
	c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.URL.Path == "/v1.44/version":
			io.WriteString(w, `{"Version":"27.3.1","ApiVersion":"1.47","Os":"linux","Arch":"amd64"}`)
		case strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/missing"):
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"No such container: missing"}`)
		case r.URL.Path == "/v1.44/containers/create":
			var cfg ContainerConfig
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil || cfg.Image != "nginx:1.27" || cfg.HostConfig.RestartPolicy.Name != "unless-stopped" {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(409)
			io.WriteString(w, `{"message":"Conflict. The container name \"/web\" is already in use"}`)
		case strings.HasPrefix(r.URL.Path, "/v1.44/containers/gone"):
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"No such container: gone"}`)
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(304) // already started
		case r.URL.Path == "/v1.44/images/ghcr.io/org/app:1.0/json":
			io.WriteString(w, `{"Id":"sha256:abc","RepoDigests":["ghcr.io/org/app@sha256:def"]}`)
		default:
			w.WriteHeader(500)
		}
	})
	ctx := context.Background()

	v, err := c.Version(ctx)
	if err != nil || v.Version != "27.3.1" {
		t.Fatalf("Version = %+v, %v", v, err)
	}
	if _, err := c.ContainerInspect(ctx, "missing"); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "No such container") {
		t.Fatalf("inspect missing: %v", err)
	}
	_, err = c.ContainerCreate(ctx, "web", ContainerConfig{Image: "nginx:1.27", HostConfig: &HostConfig{RestartPolicy: RestartPolicy{Name: "unless-stopped"}}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("create conflict: %v", err)
	}
	if err := c.ContainerRemove(ctx, "gone", true); err != nil {
		t.Fatalf("removing a missing container: %v", err)
	}
	if err := c.ContainerStart(ctx, "web"); err != nil {
		t.Fatalf("start of a running container: %v", err)
	}
	if info, err := c.ImageInspect(ctx, "ghcr.io/org/app:1.0"); err != nil || info.ID != "sha256:abc" {
		t.Fatalf("image inspect: %+v %v", info, err)
	}
	if !strings.HasPrefix(paths[0], "GET /v1.44/") {
		t.Fatalf("unversioned request: %v", paths)
	}
	if !strings.Contains(strings.Join(paths, "\n"), "POST /v1.44/containers/create?name=web") {
		t.Fatalf("paths = %v", paths)
	}
}

func TestVersionTooOld(t *testing.T) {
	c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Version":"20.10.24","ApiVersion":"1.41"}`)
	})
	if _, err := c.Version(context.Background()); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("old daemon accepted: %v", err)
	}
}

func TestPullReportsStreamErrors(t *testing.T) {
	var query string
	c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		io.WriteString(w, `{"status":"Pulling from library/nope"}`+"\n")
		io.WriteString(w, `{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}`+"\n")
	})
	var lines []string
	err := c.Pull(context.Background(), "registry.example.com:5000/nope:2", nil, func(p PullProgress) { lines = append(lines, p.Status) })
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("pull error lost: %v", err)
	}
	if len(lines) != 1 || query != "fromImage=registry.example.com%3A5000%2Fnope&tag=2" {
		t.Fatalf("lines=%v query=%s", lines, query)
	}
}

func TestSplitRef(t *testing.T) {
	for in, want := range map[string][2]string{
		"nginx":                         {"nginx", "latest"},
		"nginx:1.27":                    {"nginx", "1.27"},
		"localhost:5000/app":            {"localhost:5000/app", "latest"},
		"localhost:5000/app:v2":         {"localhost:5000/app", "v2"},
		"ghcr.io/a/b@sha256:0123abcd":   {"ghcr.io/a/b", "sha256:0123abcd"},
		"ghcr.io/a/b:1@sha256:0123abcd": {"ghcr.io/a/b:1", "sha256:0123abcd"},
	} {
		repo, tag := SplitRef(in)
		if repo != want[0] || tag != want[1] {
			t.Errorf("SplitRef(%q) = %q, %q; want %q", in, repo, tag, want)
		}
	}
}

func TestDemux(t *testing.T) {
	var stream bytes.Buffer
	frame := func(id byte, s string) {
		hdr := [8]byte{id}
		binary.BigEndian.PutUint32(hdr[4:], uint32(len(s)))
		stream.Write(hdr[:])
		stream.WriteString(s)
	}
	frame(1, "out 1\n")
	frame(2, "err 1\n")
	frame(1, "out 2\n")
	var out, errb bytes.Buffer
	if err := Demux(&stream, &out, &errb); err != nil {
		t.Fatal(err)
	}
	if out.String() != "out 1\nout 2\n" || errb.String() != "err 1\n" {
		t.Fatalf("out=%q err=%q", out.String(), errb.String())
	}

	bad := []byte{9, 0, 0, 0, 0, 0, 0, 1, 'x'}
	if err := Demux(bytes.NewReader(bad), io.Discard, io.Discard); err == nil {
		t.Fatal("invalid stream id accepted")
	}
	huge := []byte{1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}
	if err := Demux(bytes.NewReader(huge), io.Discard, io.Discard); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestLabelFilter(t *testing.T) {
	var f map[string][]string
	json.Unmarshal([]byte(labelFilter(map[string]string{"basalt.app": "7"})), &f)
	if len(f["label"]) != 1 || f["label"][0] != "basalt.app=7" {
		t.Fatalf("filter = %v", f)
	}
}

func TestValidRef(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("ab", 32)
	for _, ok := range []string{"nginx", "nginx:1.27-alpine", "library/nginx", "ghcr.io/org/app:v1.2.3",
		"localhost:5000/team/app", "registry.example.com:443/a/b_c/d-e:latest", "n8nio/n8n" + digest, "a/b:1" + digest} {
		if !ValidRef(ok) {
			t.Errorf("ValidRef(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "Nginx", "nginx:", ":tag", "nginx:-x", "a//b", "-a/b", "nginx@sha256:short",
		"nginx; rm -rf /", "nginx --privileged", "a/b:" + strings.Repeat("x", 129), "../etc"} {
		if ValidRef(bad) {
			t.Errorf("ValidRef(%q) = true", bad)
		}
	}
}

func TestRegistryHost(t *testing.T) {
	for ref, want := range map[string]string{
		"nginx": "docker.io", "library/nginx:1": "docker.io", "n8nio/n8n": "docker.io",
		"ghcr.io/org/app:v1": "ghcr.io", "GHCR.io/org/app": "ghcr.io", "localhost:5000/app": "localhost:5000",
		"localhost/app": "localhost", "127.0.0.1:5000/a/b@sha256:00": "127.0.0.1:5000",
		"index.docker.io/library/nginx": "docker.io", "registry.example.com/x": "registry.example.com",
	} {
		if got := RegistryHost(ref); got != want {
			t.Errorf("RegistryHost(%q) = %q; want %q", ref, got, want)
		}
	}
}

func TestPullSendsCredentials(t *testing.T) {
	var got AuthConfig
	c := fakeDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		b, err := base64.URLEncoding.DecodeString(r.Header.Get("X-Registry-Auth"))
		if err == nil {
			json.Unmarshal(b, &got)
		}
		io.WriteString(w, `{"status":"Status: Downloaded newer image"}`+"\n")
	})
	auth := &AuthConfig{Username: "bot", Password: "s3cr3t+/=", ServerAddress: "ghcr.io"}
	if err := c.Pull(context.Background(), "ghcr.io/org/private:1", auth, nil); err != nil {
		t.Fatal(err)
	}
	if got != *auth {
		t.Fatalf("daemon received %+v", got)
	}
}

func TestSecurityAndMemory(t *testing.T) {
	info := Info{CgroupVersion: "2", SecurityOptions: []string{
		"name=apparmor", "name=seccomp,profile=builtin", "name=userns", "name=cgroupns"}}
	if s := info.Security(); !s.UserNS || !s.AppArmor || s.Seccomp != "builtin" || !s.CgroupV2 || s.Rootless {
		t.Fatalf("security = %+v", s)
	}
	if s := (Info{SecurityOptions: []string{"name=rootless", "name=seccomp"}}).Security(); !s.Rootless || s.Seccomp != "default" || s.UserNS {
		t.Fatalf("security = %+v", s)
	}
	var st Stats
	st.MemoryStats.Usage = 100
	st.MemoryStats.Stats = map[string]uint64{"inactive_file": 30}
	if st.MemoryUsed() != 70 {
		t.Fatalf("v2 memory = %d", st.MemoryUsed())
	}
	st.MemoryStats.Stats = map[string]uint64{"total_inactive_file": 500}
	if st.MemoryUsed() != 0 {
		t.Fatalf("v1 memory = %d", st.MemoryUsed())
	}
}
