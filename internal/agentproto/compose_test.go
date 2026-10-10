package agentproto

import (
	"strings"
	"testing"
)

func TestValidateCompose(t *testing.T) {
	ok := func() AppSpec {
		return AppSpec{AppID: 1, DeploymentID: 2, ComposeVolumes: []string{"data"}, Services: []ServiceSpec{
			{Name: "web", Image: "nginx:1.27", Restart: "unless-stopped", Port: 80,
				DependsOn: []ServiceDependency{{Service: "db", Condition: DependHealthy}}},
			{Name: "db", Image: "postgres:16", Restart: "always", Mounts: []ServiceMount{{Volume: "data", Path: "/var/lib/postgresql/data"}}},
		}}
	}
	s := ok()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*AppSpec){
		"unknown volume":     func(s *AppSpec) { s.Services[1].Mounts[0].Volume = "other" },
		"mount path escape":  func(s *AppSpec) { s.Services[1].Mounts[0].Path = "/var/../etc" },
		"mount root":         func(s *AppSpec) { s.Services[1].Mounts[0].Path = "/" },
		"two published":      func(s *AppSpec) { s.Services[1].Port = 5432 },
		"reserved label":     func(s *AppSpec) { s.Services[0].Labels = map[string]string{"basalt.port": "1"} },
		"tmpfs option":       func(s *AppSpec) { s.Services[0].Tmpfs = map[string]string{"/tmp": "exec,dev,suid;rm"} },
		"bad image":          func(s *AppSpec) { s.Services[0].Image = "-v /:/host nginx" },
		"bad restart":        func(s *AppSpec) { s.Services[0].Restart = "" },
		"unknown dependency": func(s *AppSpec) { s.Services[0].DependsOn[0].Service = "cache" },
		"self dependency":    func(s *AppSpec) { s.Services[0].DependsOn[0].Service = "web" },
		"cycle": func(s *AppSpec) {
			s.Services[1].DependsOn = []ServiceDependency{{Service: "web", Condition: DependStarted}}
		},
		"duplicate service": func(s *AppSpec) { s.Services[1].Name = "web" },
		"bad name":          func(s *AppSpec) { s.Services[0].Name = "../web" },
		"bad cap":           func(s *AppSpec) { s.Services[0].CapDrop = []string{"all; rm"} },
		"bad extra host":    func(s *AppSpec) { s.Services[0].ExtraHosts = []string{"a b:1.2.3.4"} },
		"bad volume name":   func(s *AppSpec) { s.ComposeVolumes = []string{"../x"} },
	} {
		s := ok()
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestServiceOrder(t *testing.T) {
	svcs := []ServiceSpec{
		{Name: "web", DependsOn: []ServiceDependency{{Service: "db"}, {Service: "cache"}}},
		{Name: "worker", DependsOn: []ServiceDependency{{Service: "db"}}},
		{Name: "db"}, {Name: "cache"},
	}
	order, err := ServiceOrder(svcs)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range order {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "cache,db,web,worker" {
		t.Fatalf("order = %s", got)
	}
}
