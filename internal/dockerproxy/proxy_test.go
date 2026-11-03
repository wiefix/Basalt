package dockerproxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/docker"
)

const testBuildKit = "moby/buildkit:v1@sha256:00"

// fakeDocker is a Docker daemon that records what reaches it and knows
// three containers: app (Basalt's), bk (the helper's BuildKit) and host (a
// container with a bind mount of /, made outside Basalt).
type fakeDocker struct {
	mu   sync.Mutex
	seen []string
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if r.Header.Get("Upgrade") != "" {
		// Attach and exec: switch protocols and echo.
		c, rw, _ := w.(http.Hijacker).Hijack()
		defer c.Close()
		rw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		rw.Flush()
		line, _ := rw.ReadString('\n')
		rw.WriteString("echo " + line)
		rw.Flush()
		return
	}
	inspect := map[string]string{
		"app":  `{"Config":{"Image":"nginx"},"HostConfig":{"NetworkMode":"basalt-app-1","IpcMode":"private","CgroupnsMode":"private","MaskedPaths":["/proc/kcore"],"Mounts":[{"Type":"volume","Source":"basalt-app-1-data","Target":"/data"}],"SecurityOpt":["no-new-privileges:true"]}}`,
		"bk":   `{"Config":{"Image":"` + testBuildKit + `"},"HostConfig":{"Privileged":true}}`,
		"bk2":  `{"Config":{"Image":"evil"},"HostConfig":{"Privileged":true}}`,
		"host": `{"Config":{"Image":"debian"},"HostConfig":{"Binds":["/:/host"]}}`,
	}
	if strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json") && r.Method == http.MethodGet {
		id := strings.Split(r.URL.Path, "/")[2]
		if c, ok := inspect[id]; ok {
			io.WriteString(w, c)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"No such container"}`)
		return
	}
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"Id": "new", "ApiVersion": "1.44", "body": string(body)})
}

func (f *fakeDocker) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		return ""
	}
	return f.seen[len(f.seen)-1]
}

// start serves a fake daemon and the proxy in front of it; it returns the
// proxy's socket.
func start(t *testing.T) (*fakeDocker, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeDocker{}
	serve := func(name string, h http.Handler) string {
		path := filepath.Join(dir, name)
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: h}
		go srv.Serve(l)
		t.Cleanup(func() { srv.Close() })
		return path
	}
	up := serve("docker.sock", f)
	return f, serve("proxy.sock", New(up, testBuildKit, slog.New(slog.NewTextHandler(io.Discard, nil))))
}

func client(sock string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}}
}

// dockerRun is the body `docker run --rm --network none --mount
// type=volume,… busybox sh -c …` sends (docker 29).
const dockerRun = `{"Hostname":"","Domainname":"","User":"","AttachStdin":false,"AttachStdout":true,"AttachStderr":true,"Tty":false,"OpenStdin":false,"StdinOnce":false,"Env":null,"Cmd":["sh","-c","echo ok"],"Image":"busybox:1.37","Volumes":{},"WorkingDir":"","Entrypoint":null,"Labels":{},"HostConfig":{"Binds":null,"ContainerIDFile":"","LogConfig":{"Type":"none","Config":{}},"NetworkMode":"none","PortBindings":{},"RestartPolicy":{"Name":"no","MaximumRetryCount":0},"AutoRemove":true,"VolumeDriver":"","VolumesFrom":null,"ConsoleSize":[0,0],"CapAdd":null,"CapDrop":null,"CgroupnsMode":"","Dns":null,"DnsOptions":[],"DnsSearch":[],"ExtraHosts":null,"GroupAdd":null,"IpcMode":"","Cgroup":"","Links":null,"OomScoreAdj":0,"PidMode":"","Privileged":false,"PublishAllPorts":false,"ReadonlyRootfs":false,"SecurityOpt":null,"UTSMode":"","UsernsMode":"","ShmSize":0,"Isolation":"","CpuShares":0,"Memory":0,"NanoCpus":0,"CgroupParent":"","BlkioWeight":0,"BlkioWeightDevice":[],"BlkioDeviceReadBps":[],"BlkioDeviceWriteBps":[],"BlkioDeviceReadIOps":[],"BlkioDeviceWriteIOps":[],"CpuPeriod":0,"CpuQuota":0,"CpuRealtimePeriod":0,"CpuRealtimeRuntime":0,"CpusetCpus":"","CpusetMems":"","Devices":[],"DeviceCgroupRules":null,"DeviceRequests":null,"MemoryReservation":0,"MemorySwap":0,"MemorySwappiness":-1,"OomKillDisable":false,"PidsLimit":0,"Ulimits":[],"CpuCount":0,"CpuPercent":0,"IOMaximumIOps":0,"IOMaximumBandwidth":0,"Mounts":[{"Type":"volume","Source":"basalt-app-1-data","Target":"/v"}],"MaskedPaths":null,"ReadonlyPaths":null},"NetworkingConfig":{"EndpointsConfig":{}}}`

// hostConfig is a create body with the given HostConfig.
func hostConfig(hc string) string { return `{"Image":"nginx","HostConfig":` + hc + `}` }

func TestRequests(t *testing.T) {
	f, sock := start(t)
	hc := client(sock)
	for _, tc := range []struct {
		method, path, body string
		allowed            bool
	}{
		{"GET", "/_ping", "", true},
		{"HEAD", "/_ping", "", true},
		{"GET", "/v1.44/version", "", true},
		{"GET", "/v1.54/info", "", true},
		{"GET", "/v1.44/containers/json?all=1", "", true},
		{"POST", "/v1.54/containers/create", dockerRun, true},
		{"POST", "/v1.44/containers/create?name=basalt-app-1-d2", hostConfig(`{"NetworkMode":"basalt-app-1","CapDrop":["ALL"],"SecurityOpt":["no-new-privileges:true"],"Init":true,"PidsLimit":4096,"ReadonlyRootfs":true,"Tmpfs":{"/tmp":""},"PortBindings":{"25/tcp":[{"HostPort":"25"}]},"LogConfig":{"Type":"local","Config":{"max-size":"10m"}},"Mounts":[{"Type":"tmpfs","Target":"/run"}]}`), true},
		{"POST", "/containers/create", `{"Image":"nginx","NetworkingConfig":{"EndpointsConfig":{"basalt-app-1":{"Aliases":["web"]}}}}`, true},
		{"POST", "/containers/abc/start", "", true},
		{"POST", "/containers/abc/stop?t=10", "", true},
		{"POST", "/containers/abc/wait?condition=removed", "", true},
		{"DELETE", "/containers/abc?force=1&v=1", "", true},
		{"GET", "/containers/abc/logs?follow=1", "", true},
		{"GET", "/containers/abc/stats?stream=false", "", true},
		{"POST", "/exec/x1/start", `{"Detach":false}`, true},
		{"GET", "/exec/x1/json", "", true},
		{"POST", "/images/create?fromImage=nginx&tag=1", "", true},
		{"POST", "/images/load?quiet=0", "", true},
		{"GET", "/images/ghcr.io/a/b:1/json", "", true},
		{"POST", "/images/ghcr.io/a/b/push?tag=1", "", true},
		{"POST", "/images/basalt/app-1:build-x/tag?repo=r&tag=t", "", true},
		{"DELETE", "/images/sha256:abc", "", true},
		{"POST", "/build?t=x", "", true},
		{"POST", "/build/prune", "", true},
		{"POST", "/grpc", "", true},
		{"POST", "/session", "", true},
		{"POST", "/networks/create", `{"Name":"basalt-app-1","Labels":{"basalt.app":"1"}}`, true},
		{"GET", "/networks/basalt-app-1", "", true},
		{"DELETE", "/networks/basalt-app-1", "", true},
		{"POST", "/networks/basalt-app-1/connect", `{"Container":"abc","EndpointConfig":{"Aliases":["db"]}}`, true},
		{"POST", "/networks/basalt-app-1/disconnect", `{"Container":"abc","Force":true}`, true},
		{"POST", "/volumes/create", `{"Name":"basalt-app-1-data","Labels":{"basalt.app":"1"}}`, true},
		{"GET", "/volumes?filters=x", "", true},
		{"DELETE", "/volumes/basalt-app-1-data", "", true},

		// Containers that reach the host.
		{"POST", "/containers/create", hostConfig(`{"Privileged":true}`), false},
		{"POST", "/containers/create", hostConfig(`{"privileged":true}`), false},
		{"POST", "/containers/create", hostConfig(`{"Privileged":false,"PRIVILEGED":true}`), false},
		{"POST", "/containers/create", hostConfig(`{"Binds":["/:/host"]}`), false},
		{"POST", "/containers/create", hostConfig(`{"Mounts":[{"Type":"bind","Source":"/","Target":"/host"}]}`), false},
		{"POST", "/containers/create", hostConfig(`{"Mounts":[{"Source":"/","Target":"/host"}]}`), false},
		{"POST", "/containers/create", hostConfig(`{"Mounts":[{"Type":"image","Source":"x","Target":"/x"}]}`), false},
		{"POST", "/containers/create", hostConfig(`{"Mounts":[{"Type":"volume","Source":"basalt-x","Target":"/x","VolumeOptions":{"DriverConfig":{"Name":"local","Options":{"type":"none","o":"bind","device":"/"}}}}]}`), false},
		{"POST", "/containers/create", hostConfig(`{"Mounts":[{"Type":"volume","Source":"users-data","Target":"/x"}]}`), false},
		{"POST", "/containers/create", hostConfig(`{"Mounts":[{"Type":"volume","Source":"basalt-buildkit-state","Target":"/x"}]}`), false},
		{"POST", "/containers/create", hostConfig(`{"Mounts":[{"Type":"volume","Source":"basalt-x","Target":"/x","BindOptions":{"Propagation":"rshared"}}]}`), false},
		{"POST", "/containers/create", hostConfig(`{"NetworkMode":"host"}`), false},
		{"POST", "/containers/create", hostConfig(`{"NetworkMode":"container:abc"}`), false},
		{"POST", "/containers/create", hostConfig(`{"PidMode":"host"}`), false},
		{"POST", "/containers/create", hostConfig(`{"IpcMode":"host"}`), false},
		{"POST", "/containers/create", hostConfig(`{"UTSMode":"host"}`), false},
		{"POST", "/containers/create", hostConfig(`{"UsernsMode":"host"}`), false},
		{"POST", "/containers/create", hostConfig(`{"CgroupnsMode":"host"}`), false},
		{"POST", "/containers/create", hostConfig(`{"CapAdd":["SYS_ADMIN"]}`), false},
		{"POST", "/containers/create", hostConfig(`{"SecurityOpt":["seccomp=unconfined"]}`), false},
		{"POST", "/containers/create", hostConfig(`{"SecurityOpt":["apparmor=unconfined"]}`), false},
		{"POST", "/containers/create", hostConfig(`{"SecurityOpt":["systempaths=unconfined"]}`), false},
		{"POST", "/containers/create", hostConfig(`{"MaskedPaths":[]}`), false},
		{"POST", "/containers/create", hostConfig(`{"maskedPaths":[]}`), false},
		{"POST", "/containers/create", `{"Image":"nginx","hostconfig":{"MaskedPaths":[]}}`, false},
		{"POST", "/containers/create", hostConfig(`{"Mounts":[{"type":"bind","Source":"/","Target":"/h"}]}`), false},
		{"POST", "/containers/create", hostConfig(`{"NetworkMode":"bridge","networkmode":"host"}`), false},
		{"POST", "/containers/create", hostConfig(`{"Mounts":[{"Type":"volume","Source":"basalt-x","source":"users-data","Target":"/x"}]}`), false},
		{"POST", "/containers/create", hostConfig(`{"Mounts":[{"Type":"volume","source":"users-data","Target":"/x"}]}`), false},
		{"POST", "/containers/create", hostConfig(`{"ſecurityOpt":["seccomp=unconfined"]}`), false},
		{"POST", "/containers/create", hostConfig(`{"ReadonlyPaths":[]}`), false},
		{"POST", "/containers/create", hostConfig(`{"Devices":[{"PathOnHost":"/dev/sda","PathInContainer":"/dev/sda"}]}`), false},
		{"POST", "/containers/create", hostConfig(`{"DeviceRequests":[{"Count":-1,"Capabilities":[["gpu"]]}]}`), false},
		{"POST", "/containers/create", hostConfig(`{"DeviceCgroupRules":["a *:* rwm"]}`), false},
		{"POST", "/containers/create", hostConfig(`{"Sysctls":{"kernel.shm_rmid_forced":"1"}}`), false},
		{"POST", "/containers/create", hostConfig(`{"VolumesFrom":["abc"]}`), false},
		{"POST", "/containers/create", hostConfig(`{"CgroupParent":"/"}`), false},
		{"POST", "/containers/create", hostConfig(`{"Runtime":"sysbox"}`), false},
		{"POST", "/containers/create", hostConfig(`{"OomScoreAdj":-1000}`), false},
		{"POST", "/containers/create", hostConfig(`{"LogConfig":{"Type":"syslog","Config":{"syslog-address":"tcp://x:514"}}}`), false},
		{"POST", "/containers/create", hostConfig(`{"SomeNewOption":"on"}`), false},
		{"POST", "/containers/create", `{"Image":"nginx","NetworkingConfig":{"EndpointsConfig":{"host":{}}}}`, false},
		{"POST", "/containers/create", `{"Image":"nginx","NetworkingConfig":{"EndpointsConfig":{"basalt-app-1":{"DriverOpts":{"com.docker.network.endpoint.sysctls":"x"}}}}}`, false},
		{"POST", "/containers/create", `{"Image":"nginx","OnBuild":["RUN x"]}`, false},
		{"POST", "/containers/create", `not json`, false},

		// The proxy's checks of everything else.
		{"POST", "/containers/app/exec", `{"Cmd":["sh"],"Privileged":true}`, false},
		{"POST", "/containers/host/exec", `{"Cmd":["sh"]}`, false},
		{"POST", "/containers/host/attach?stream=1", "", false},
		{"POST", "/containers/bk/exec", `{"Cmd":["sh"]}`, false},
		{"POST", "/containers/bk/exec", `{"Cmd":["buildctl","dial-stdio"],"User":"root"}`, false},
		{"POST", "/containers/bk/exec", `{"Cmd":["buildctl","debug","workers"]}`, false},
		{"POST", "/containers/bk/exec", `{"Cmd":["buildctl","dial-stdio"],"user":"root"}`, false},
		{"POST", "/containers/bk2/exec", `{"Cmd":["buildctl","dial-stdio"]}`, false},
		{"POST", "/containers/bk/attach?stream=1", "", false},
		{"PUT", "/containers/bk/archive?path=/etc", "", false},
		{"GET", "/containers/app/archive?path=/", "", false},
		{"POST", "/containers/app/update", `{"Privileged":true}`, false},
		{"POST", "/containers/app/commit", "", false},
		{"POST", "/images/create?fromSrc=http://x/a.tar", "", false},
		{"POST", "/images/create", "", false},
		{"POST", "/build?networkmode=host", "", false},
		{"POST", "/networks/create", `{"Name":"x","Driver":"macvlan","Options":{"parent":"eth0"}}`, false},
		{"POST", "/networks/create", `{"Name":"x","Options":{"com.docker.network.bridge.name":"docker0"}}`, false},
		{"POST", "/networks/host/connect", `{"Container":"abc"}`, false},
		{"POST", "/volumes/create", `{"Name":"basalt-x","DriverOpts":{"type":"none","o":"bind","device":"/"}}`, false},
		{"POST", "/volumes/create", `{"Name":"basalt-x","Driver":"nfs"}`, false},
		{"POST", "/swarm/init", "", false},
		{"POST", "/plugins/pull", "", false},
		{"GET", "/events", "", false},
		{"POST", "/v1.44/../containers/create", "", false},
		{"POST", "/containers/..%2Fimages%2Fx/exec", `{"Cmd":["sh"]}`, false},
		{"POST", "/containers/app/./exec", `{"Cmd":["sh"]}`, false},
	} {
		before := f.last()
		req, _ := http.NewRequest(tc.method, "http://docker"+tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		reached := f.last() != before && strings.HasPrefix(f.last(), tc.method+" ")
		if tc.allowed && (resp.StatusCode != http.StatusOK || !reached) {
			t.Errorf("%s %s %s: refused (%d %s)", tc.method, tc.path, tc.body, resp.StatusCode, b)
		}
		if !tc.allowed && (resp.StatusCode == http.StatusOK || reached && !strings.HasSuffix(f.last(), "/json")) {
			t.Errorf("%s %s %s: passed (%d)", tc.method, tc.path, tc.body, resp.StatusCode)
		}
		if tc.allowed && tc.body != "" && tc.method == "POST" {
			// The body reaches the daemon as it was sent.
			var got map[string]string
			if json.Unmarshal(b, &got) != nil || got["body"] != tc.body {
				t.Errorf("%s %s: the daemon got %q", tc.method, tc.path, got["body"])
			}
		}
	}
}

func TestExecIntoBuildKit(t *testing.T) {
	_, sock := start(t)
	resp, err := client(sock).Post("http://docker/v1.54/containers/bk/exec", "application/json",
		strings.NewReader(`{"User":"","Privileged":false,"Tty":false,"AttachStdin":true,"AttachStderr":true,"AttachStdout":true,"DetachKeys":"","Env":null,"WorkingDir":"","Cmd":["buildctl","dial-stdio"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("buildx's dial-stdio refused: %d", resp.StatusCode)
	}
}

// TestUpgrade: attach and exec switch to a raw stream, which the proxy
// carries both ways.
func TestUpgrade(t *testing.T) {
	_, sock := start(t)
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "POST /v1.44/containers/app/attach?stream=1&stdout=1 HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", resp.StatusCode)
	}
	io.WriteString(c, "hello\n")
	line, err := br.ReadString('\n')
	if err != nil || line != "echo hello\n" {
		t.Fatalf("got %q, %v", line, err)
	}
}

// TestClient: the agent's Docker client works through the proxy, and a
// refusal reaches it as a Docker error with the reason.
func TestClient(t *testing.T) {
	_, sock := start(t)
	dc := docker.New(sock)
	ctx := context.Background()
	if _, err := dc.Version(ctx); err != nil {
		t.Fatal(err)
	}
	init := true
	if _, err := dc.ContainerCreate(ctx, "basalt-app-1-d1", docker.ContainerConfig{Image: "nginx", HostConfig: &docker.HostConfig{
		NetworkMode: "basalt-app-1", CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, Init: &init,
		Mounts: []docker.Mount{{Type: "volume", Source: "basalt-app-1-data", Target: "/data"}},
	}}); err != nil {
		t.Fatal(err)
	}
	_, err := dc.ContainerCreate(ctx, "x", docker.ContainerConfig{Image: "nginx", HostConfig: &docker.HostConfig{Privileged: true}})
	var de *docker.Error
	if !errors.As(err, &de) || de.Status != http.StatusForbidden || !strings.Contains(de.Message, "Privileged") {
		t.Fatalf("privileged container: %v", err)
	}
}

func TestNoDocker(t *testing.T) {
	dir := t.TempDir()
	h := New(filepath.Join(dir, "missing.sock"), "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	req, _ := http.NewRequest("GET", "/version", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not installed") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestIsZero(t *testing.T) {
	for v, want := range map[string]bool{
		"null": true, "false": true, "0": true, "0.0": true, `""`: true, "[]": true, "{}": true,
		`{"a":null,"b":{"c":0}}`: false, " { } ": true, " [ ] ": true, "true": false, "1": false, `"x"`: false,
		"[0]": false, `{"a":1}`: false, "-1": false,
	} {
		if got := isZero(json.RawMessage(v)); got != want {
			t.Errorf("isZero(%s) = %v", v, got)
		}
	}
}
