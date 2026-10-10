package dbengine

import (
	"net/url"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/agentproto"
)

// Every engine and version yields a service the agent accepts.
func TestServicesPassAgentValidation(t *testing.T) {
	for _, e := range All() {
		for _, v := range e.Versions {
			c := e.NewCredentials("shop-db")
			svc, err := e.Service("shop-db", v, e.Env(c), Limits{MemoryMB: 512, CPUs: 1})
			if err != nil {
				t.Fatalf("%s %s: %v", e.ID, v, err)
			}
			spec := agentproto.AppSpec{AppID: 7, DeploymentID: 1, Services: []agentproto.ServiceSpec{svc},
				ComposeVolumes: []string{Volume}, Attach: []int64{3, 4}}
			if err := spec.Validate(); err != nil {
				t.Fatalf("%s %s: %v", e.ID, v, err)
			}
			if svc.Health == nil || len(svc.Health.Test) == 0 || len(svc.Mounts) != 1 || svc.Restart != "unless-stopped" {
				t.Fatalf("%s %s: incomplete service %+v", e.ID, v, svc)
			}
			if !strings.HasPrefix(svc.Image, e.ID+":"+v) {
				t.Fatalf("%s %s: image %s", e.ID, v, svc.Image)
			}
		}
	}
}

func TestPostgresDataDirectory(t *testing.T) {
	e, _ := Get("postgres")
	env := e.Env(e.NewCredentials("pg"))
	for v, want := range map[string]string{"18": "/var/lib/postgresql", "17": "/var/lib/postgresql/data", "15": "/var/lib/postgresql/data"} {
		svc, _ := e.Service("pg", v, env, Limits{})
		if svc.Mounts[0].Path != want {
			t.Errorf("postgres %s mounts at %s, want %s", v, svc.Mounts[0].Path, want)
		}
	}
}

func TestCredentials(t *testing.T) {
	for _, e := range All() {
		c := e.NewCredentials("my-shop-database-with-a-rather-long-name")
		if got := e.CredentialsFrom(e.Env(c)); got != c {
			t.Fatalf("%s: round trip %+v != %+v", e.ID, got, c)
		}
		if len(c.Password) < 26 || e.SQL && (c.Database != "my_shop_database_with_a_rather_long_name" || len(c.User) > 32) {
			t.Fatalf("%s: credentials %+v", e.ID, c)
		}
		if (e.ID == "mysql" || e.ID == "mariadb") != (c.RootPassword != "") {
			t.Fatalf("%s: root password %q", e.ID, c.RootPassword)
		}
		u, err := url.Parse(e.URL("my-db", c))
		if err != nil || u.Hostname() != "my-db" {
			t.Fatalf("%s: url %v %v", e.ID, u, err)
		}
		if pw, _ := u.User.Password(); pw != c.Password {
			t.Fatalf("%s: url password %q", e.ID, pw)
		}
	}
	// Missing credentials are an error, not an unprotected database.
	e, _ := Get("redis")
	if _, err := e.Service("cache", "8", map[string]string{}, Limits{}); err == nil {
		t.Fatal("redis without a password accepted")
	}
}

func TestRejected(t *testing.T) {
	e, _ := Get("postgres")
	env := e.Env(e.NewCredentials("pg"))
	for _, name := range []string{"", "a", "Upper", "1db", "db-", "db_x", "db.x", strings.Repeat("a", 43)} {
		if _, err := e.Service(name, "17", env, Limits{}); err == nil {
			t.Errorf("name %q accepted", name)
		}
	}
	if _, err := e.Service("pg", "9.6", env, Limits{}); err == nil {
		t.Error("unoffered version accepted")
	}
	if _, ok := Get("mongodb"); ok {
		t.Error("unknown engine found")
	}
}
