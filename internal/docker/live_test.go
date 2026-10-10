package docker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveDaemon runs against the local Docker daemon. It pulls an image, so
// it is opt-in: BASALT_DOCKER_TESTS=1 (see `make test-docker`).
func TestLiveDaemon(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	c := New(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	v, err := c.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("docker %s, API %s", v.Version, v.APIVersion)

	const image = "busybox:1.37"
	pulls := 0
	if err := c.Pull(ctx, image, nil, func(PullProgress) { pulls++ }); err != nil {
		t.Fatal(err)
	}
	if pulls == 0 {
		t.Error("no pull progress reported")
	}
	if err := c.Pull(ctx, "busybox:this-tag-does-not-exist", nil, nil); err == nil {
		t.Fatal("pull of a missing tag succeeded")
	}
	if _, err := c.ImageInspect(ctx, image); err != nil {
		t.Fatal(err)
	}

	suffix := time.Now().Format("150405.000")
	suffix = strings.ReplaceAll(suffix, ".", "")
	netName := "basalt-test-" + suffix
	netID, err := c.NetworkCreate(ctx, NetworkConfig{Name: netName, Labels: map[string]string{"basalt.test": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.NetworkRemove(context.Background(), netID)

	name := "basalt-test-" + suffix
	pids := int64(64)
	id, err := c.ContainerCreate(ctx, name, ContainerConfig{
		Image:  image,
		Cmd:    []string{"sh", "-c", "echo hello-out; echo hello-err >&2; echo $GREETING; sleep 300"},
		Env:    []string{"GREETING=hi from env"},
		Labels: map[string]string{"basalt.test": suffix},
		HostConfig: &HostConfig{
			NetworkMode: netName,
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges:true"},
			PidsLimit:   &pids,
			Memory:      64 << 20,
		},
		NetworkingConfig: &NetworkingConfig{EndpointsConfig: map[string]*EndpointSettings{netName: {Aliases: []string{"web"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.ContainerRemove(context.Background(), id, true)
	if _, err := c.ContainerCreate(ctx, name, ContainerConfig{Image: image}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	if err := c.ContainerStart(ctx, id); err != nil {
		t.Fatal(err)
	}
	info, err := c.ContainerInspect(ctx, id)
	if err != nil || !info.State.Running || info.Config.Labels["basalt.test"] != suffix {
		t.Fatalf("inspect: %+v %v", info, err)
	}
	if n := info.NetworkSettings.Networks[netName]; n.IPAddress == "" {
		t.Fatalf("container not on its network: %+v", info.NetworkSettings.Networks)
	}

	list, err := c.ContainerList(ctx, map[string]string{"basalt.test": suffix})
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("list = %+v, %v", list, err)
	}

	var out, errb bytes.Buffer
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "hi from env") && time.Now().Before(deadline) {
		out.Reset()
		errb.Reset()
		rc, err := c.ContainerLogs(ctx, id, LogsOptions{Tail: 100})
		if err != nil {
			t.Fatal(err)
		}
		Demux(rc, &out, &errb)
		rc.Close()
		time.Sleep(100 * time.Millisecond)
	}
	if out.String() != "hello-out\nhi from env\n" || errb.String() != "hello-err\n" {
		t.Fatalf("logs: out=%q err=%q", out.String(), errb.String())
	}

	if err := c.ContainerStop(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c.ContainerStop(ctx, id, time.Second); err != nil {
		t.Fatalf("stopping a stopped container: %v", err)
	}
	if err := c.ContainerRemove(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ContainerInspect(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed container still there: %v", err)
	}
	if err := c.NetworkRemove(ctx, netID); err != nil {
		t.Fatal(err)
	}
}
