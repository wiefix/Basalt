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
