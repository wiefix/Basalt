package agent

import (
	"slices"
	"testing"

	"github.com/wiefix/basalt/internal/agentproto"
)

func TestServiceConfig(t *testing.T) {
	m := &appsManager{}
	spec := agentproto.AppSpec{AppID: 4, DeploymentID: 9}
	svc := agentproto.ServiceSpec{Name: "db", Image: "postgres:16", Restart: "on-failure", MaxRetries: 3,
		Mounts: []agentproto.ServiceMount{{Volume: "data", Path: "/var/lib/postgresql/data"}},
		Labels: map[string]string{"team": "a"}, CapDrop: []string{"NET_RAW", "CHOWN"}, Aliases: []string{"database"},
		Env: map[string]string{"B": "2", "A": "1"}, MemoryMB: 256}
	cfg := m.serviceConfig(spec, svc, "h")
	hc := cfg.HostConfig
	if hc.NetworkMode != "basalt-app-4" || hc.Mounts[0].Source != "basalt-app-4-data" || hc.Memory != 256<<20 {
		t.Fatalf("host config = %+v", hc)
	}
	if hc.RestartPolicy.Name != "on-failure" || hc.RestartPolicy.MaximumRetryCount != 3 {
		t.Fatalf("restart = %+v", hc.RestartPolicy)
	}
	if !slices.Contains(hc.CapDrop, "CHOWN") || !slices.Contains(hc.CapDrop, "NET_RAW") || len(hc.CapDrop) != len(appCapDrop)+1 {
		t.Fatalf("cap_drop = %v", hc.CapDrop)
	}
	if !slices.Contains(hc.SecurityOpt, "no-new-privileges:true") || hc.PidsLimit == nil || !*hc.Init {
		t.Fatalf("hardening lost: %+v", hc)
	}
	if cfg.Labels["basalt.service"] != "db" || cfg.Labels["basalt.config"] != "h" || cfg.Labels["basalt.app"] != "4" ||
		cfg.Labels["team"] != "a" || cfg.Labels["basalt.port"] != "" {
		t.Fatalf("labels = %v", cfg.Labels)
	}
	if a := cfg.NetworkingConfig.EndpointsConfig["basalt-app-4"].Aliases; !slices.Equal(a, []string{"db", "database"}) {
		t.Fatalf("aliases = %v", a)
	}
	if !slices.Equal(cfg.Env, []string{"A=1", "B=2"}) {
		t.Fatalf("env = %v", cfg.Env)
	}

	// The same image by tag or by digest is the same container.
	pinned := svc
	pinned.Image = "postgres@sha256:" + string(make([]byte, 0))
	if serviceHash(4, svc, "sha256:x") != serviceHash(4, pinned, "sha256:x") {
		t.Fatal("pinning the image by digest changes the hash")
	}
	changed := svc
	changed.Env = map[string]string{"A": "1", "B": "3"}
	if serviceHash(4, svc, "sha256:x") == serviceHash(4, changed, "sha256:x") || serviceHash(4, svc, "sha256:y") == serviceHash(4, svc, "sha256:x") {
		t.Fatal("a change of environment or image keeps the hash")
	}
}

// TestDockerNamespace: an agent with a Docker namespace names and labels
// its resources apart, and sees only its own; without one, the names are
// the usual ones and namespaced resources are not its own.
func TestDockerNamespace(t *testing.T) {
	plain, ns := &appsManager{}, &appsManager{namespace: "e2e"}
	if plain.container(3, 7) != "basalt-app-3-d7" || plain.volume(3, "data") != "basalt-app-3-data" {
		t.Fatalf("usual names: %s %s", plain.container(3, 7), plain.volume(3, "data"))
	}
	if ns.network(3) != "basalt-e2e-app-3" || ns.container(3, 7) != "basalt-e2e-app-3-d7" ||
		ns.serviceContainer(3, 7, "db") != "basalt-e2e-app-3-db-d7" || ns.volume(3, "data") != "basalt-e2e-app-3-data" {
		t.Fatalf("namespaced names: %s %s %s", ns.network(3), ns.container(3, 7), ns.volume(3, "data"))
	}
	if l := plain.appLabels(3, labelVolume, "data"); len(l) != 2 || l[labelApp] != "3" || l[labelVolume] != "data" {
		t.Fatalf("usual labels: %v", l)
	}
	l := ns.appLabels(3)
	if l[labelApp] != "3" || l[labelNamespace] != "e2e" {
		t.Fatalf("namespaced labels: %v", l)
	}
	if !ns.ours(l) || plain.ours(l) || ns.ours(plain.appLabels(3)) || !plain.ours(plain.appLabels(3)) {
		t.Fatal("a resource is claimed by the wrong agent")
	}
	cfg := ns.serviceConfig(agentproto.AppSpec{AppID: 3, DeploymentID: 7},
		agentproto.ServiceSpec{Name: "db", Image: "postgres:16", Mounts: []agentproto.ServiceMount{{Volume: "data", Path: "/d"}}}, "h")
	if cfg.HostConfig.NetworkMode != "basalt-e2e-app-3" || cfg.HostConfig.Mounts[0].Source != "basalt-e2e-app-3-data" ||
		cfg.Labels[labelNamespace] != "e2e" || cfg.NetworkingConfig.EndpointsConfig["basalt-e2e-app-3"] == nil {
		t.Fatalf("compose service config: %+v %v", cfg.HostConfig, cfg.Labels)
	}
	if c := ns.containerConfig(agentproto.AppSpec{AppID: 3, DeploymentID: 7, Image: "nginx:1.27", Port: 80}); c.Labels[labelNamespace] != "e2e" ||
		c.Labels[labelDeployment] != "7" || c.HostConfig.NetworkMode != "basalt-e2e-app-3" {
		t.Fatalf("app container config: %v", c.Labels)
	}
}
