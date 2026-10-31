package dbengine

import (
	"net/url"
	"slices"
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
				ComposeVolumes: []string{Volume}, Attach: []int64{3, 4}, MongoFCV: e.FeatureCompat(v)}
			if err := spec.Validate(); err != nil {
				t.Fatalf("%s %s: %v", e.ID, v, err)
			}
			if spec.MongoFCV = "9.0; db.dropDatabase()"; spec.Validate() == nil {
				t.Fatalf("%s %s: invalid feature compatibility version accepted", e.ID, v)
			}
			if svc.Health == nil || len(svc.Health.Test) == 0 || len(svc.Mounts) != 1 || svc.Restart != "unless-stopped" {
				t.Fatalf("%s %s: incomplete service %+v", e.ID, v, svc)
			}
			if !strings.HasPrefix(svc.Image, e.image+":"+v) {
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
		if len(c.Password) < 26 || !e.KeyValue() && (c.Database != "my_shop_database_with_a_rather_long_name" || len(c.User) > 32) {
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
		if !e.KeyValue() && (u.User.Username() != c.User || u.Path != "/"+c.Database) {
			t.Fatalf("%s: url user or database %v", e.ID, u)
		}
	}
	// Missing credentials are an error, not an unprotected database.
	for _, id := range []string{"redis", "valkey", "mongodb"} {
		e, _ := Get(id)
		if _, err := e.Service("cache", e.Versions[0], map[string]string{}, Limits{}); err == nil {
			t.Fatalf("%s without a password accepted", id)
		}
	}
	// MongoDB's user is defined in the admin database.
	mg, _ := Get("mongodb")
	if u := mg.URL("docs", Credentials{Database: "docs", User: "docs", Password: "pw"}); u != "mongodb://docs:pw@docs:27017/docs?authSource=admin" {
		t.Fatalf("mongodb url %s", u)
	}
	vk, _ := Get("valkey")
	if u := vk.URL("cache", Credentials{Password: "pw"}); u != "redis://default:pw@cache:6379" {
		t.Fatalf("valkey url %s", u)
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
	if _, ok := Get("mongo"); ok {
		t.Error("unknown engine found")
	}
}

func TestUpgradeTargets(t *testing.T) {
	for _, c := range []struct {
		engine, from string
		want         []string
	}{
		{"postgres", "15", []string{"18"}}, // dumped into the newest version
		{"postgres", "17", []string{"18"}},
		{"postgres", "18", nil},
		{"mariadb", "10.11", []string{"11.4"}}, // one release at a time
		{"mariadb", "11.4", []string{"11.8"}},
		{"mariadb", "11.8", nil},
		{"redis", "7.4", []string{"8"}},
		{"redis", "8", nil},
		{"valkey", "8", []string{"9"}},
		{"valkey", "9", nil},
		{"mongodb", "8.0", []string{"9.0"}}, // one release at a time
		{"mongodb", "9.0", nil},
		{"mysql", "8.4", nil},
		{"postgres", "9.6", nil},
	} {
		e, _ := Get(c.engine)
		if got := e.UpgradeTargets(c.from); !slices.Equal(got, c.want) {
			t.Errorf("%s %s: %v, want %v", c.engine, c.from, got, c.want)
		}
	}
	pg, _ := Get("postgres")
	my, _ := Get("mariadb")
	if !pg.DumpUpgrade() || my.DumpUpgrade() {
		t.Error("DumpUpgrade")
	}
	svc, err := my.Service("db", "11.8", my.Env(my.NewCredentials("db")), Limits{})
	if err != nil || svc.Env["MARIADB_AUTO_UPGRADE"] != "1" {
		t.Fatalf("mariadb upgrades its files at start: %v %v", svc.Env, err)
	}
}

func TestProtocol(t *testing.T) {
	for _, e := range All() {
		d := agentproto.DBExpose{AppID: 1, Service: "db", TargetPort: e.Port, Port: 15000, Allow: []string{"192.0.2.1"},
			Protocol: e.Protocol(), RequireTLS: true}
		if err := d.Validate(); err != nil {
			t.Errorf("%s: %v", e.ID, err)
		}
	}
	for id, want := range map[string]string{"mariadb": "mysql", "valkey": "redis", "mongodb": "mongodb", "postgres": "postgres"} {
		if e, _ := Get(id); e.Protocol() != want {
			t.Errorf("%s speaks %s", id, e.Protocol())
		}
	}
}

func TestKeyValueAndPassword(t *testing.T) {
	for _, e := range All() {
		kv := e.ID == "redis" || e.ID == "valkey"
		if e.KeyValue() != kv || e.PasswordRedeploys() != (kv || e.ID == "mongodb") {
			t.Errorf("%s: KeyValue %v, PasswordRedeploys %v", e.ID, e.KeyValue(), e.PasswordRedeploys())
		}
		for _, v := range e.Versions {
			if fcv := e.FeatureCompat(v); (e.ID == "mongodb") != (fcv == v) {
				t.Errorf("%s %s: feature compatibility %q", e.ID, v, fcv)
			}
		}
	}
	// The health check must not take the server MongoDB's image runs while
	// it initialises (on 127.0.0.1) for the real one.
	mg, _ := Get("mongodb")
	svc, err := mg.Service("docs", "9.0", mg.Env(mg.NewCredentials("docs")), Limits{})
	if err != nil || svc.Mounts[0].Path != "/data/db" || !strings.Contains(svc.Health.Test[1], "hostname -I") {
		t.Fatalf("mongodb service %+v %v", svc, err)
	}
	vk, _ := Get("valkey")
	svc, err = vk.Service("cache", "9", vk.Env(vk.NewCredentials("cache")), Limits{})
	if err != nil || svc.Command[0] != "valkey-server" || svc.Env["REDISCLI_AUTH"] == "" || len(svc.Env) != 1 {
		t.Fatalf("valkey service %+v %v", svc, err)
	}
}
