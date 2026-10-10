package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/agentproto"
)

const wordpress = `
services:
  wordpress:
    image: wordpress:6.6
    restart: always
    ports:
      - "8080:80"
    environment:
      WORDPRESS_DB_HOST: db
      WORDPRESS_DB_PASSWORD: ${DB_PASSWORD}
      FROM_ENV:
    volumes:
      - wp:/var/www/html
    depends_on:
      db:
        condition: service_healthy
      migrate:
        condition: service_completed_successfully
    deploy:
      resources:
        limits: {memory: 512M, cpus: "0.5"}
  db:
    image: mariadb:11.4
    environment:
      MARIADB_ROOT_PASSWORD: ${DB_PASSWORD}
    volumes:
      - db:/var/lib/mysql
      - type: tmpfs
        target: /run/mysqld
        tmpfs: {size: 1048576}
    healthcheck:
      test: ["CMD", "healthcheck.sh", "--connect"]
      interval: 5s
      retries: 10
    networks:
      default:
        aliases: [database]
  migrate:
    image: alpine:3.20
    command: ["sh", "-c", "echo migrated"]
volumes:
  wp:
  db:
`

func TestParseWordPress(t *testing.T) {
	p, err := Parse(context.Background(), []byte(wordpress), map[string]string{"DB_PASSWORD": "s3cret", "FROM_ENV": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Services) != 3 || p.Services[0].Name != "db" || p.Services[2].Name != "wordpress" {
		t.Fatalf("services = %+v", p.Services)
	}
	db, migrate, wp := p.Services[0], p.Services[1], p.Services[2]
	if wp.Env["WORDPRESS_DB_PASSWORD"] != "s3cret" || wp.Env["FROM_ENV"] != "x" || db.Env["MARIADB_ROOT_PASSWORD"] != "s3cret" {
		t.Fatalf("interpolation: %v %v", wp.Env, db.Env)
	}
	if wp.Restart != "always" || db.Restart != "unless-stopped" || migrate.Restart != "no" || !migrate.Job || wp.Job {
		t.Fatalf("restart/job: %s %s %s %v", wp.Restart, db.Restart, migrate.Restart, migrate.Job)
	}
	if wp.MemoryMB != 512 || wp.CPUs != 0.5 {
		t.Fatalf("limits: %d MB, %v CPUs", wp.MemoryMB, wp.CPUs)
	}
	if len(db.Mounts) != 1 || db.Mounts[0] != (agentproto.ServiceMount{Volume: "db", Path: "/var/lib/mysql"}) ||
		db.Tmpfs["/run/mysqld"] != "size=1048576" || len(db.Aliases) != 1 || db.Aliases[0] != "database" {
		t.Fatalf("db storage/network: %+v", db)
	}
	if db.Health == nil || db.Health.Retries != 10 || db.Health.Interval.Seconds() != 5 {
		t.Fatalf("health = %+v", db.Health)
	}
	if strings.Join(p.Volumes, ",") != "db,wp" {
		t.Fatalf("volumes = %v", p.Volumes)
	}
	if s, port := p.DefaultPublished(); s != "wordpress" || port != 80 {
		t.Fatalf("default published = %s:%d", s, port)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "ports are not published") {
		t.Fatalf("warnings = %v", p.Warnings)
	}
	if err := p.Publish("migrate", 80); err == nil {
		t.Fatal("a job was published")
	}
	if err := p.Publish("nope", 80); err == nil {
		t.Fatal("unknown service published")
	}
	if err := p.Publish("wordpress", 80); err != nil || p.Services[2].Port != 80 {
		t.Fatalf("publish: %v", err)
	}
	order, err := agentproto.ServiceOrder(p.Services)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range order {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "db,migrate,wordpress" {
		t.Fatalf("start order = %v", names)
	}
}

func TestMissingVariableIsAWarning(t *testing.T) {
	p, err := Parse(context.Background(), []byte("services:\n  a:\n    image: nginx:1.27\n    environment:\n      X: ${UNSET_VAR}\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "UNSET_VAR") {
		t.Fatalf("warnings = %v", p.Warnings)
	}
	if _, err := Parse(context.Background(), []byte("services:\n  a:\n    image: ${IMG:?set IMG}\n"), nil); err == nil {
		t.Fatal("a required variable was not enforced")
	}
}

func TestRefused(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.env")
	os.WriteFile(secret, []byte("LEAK=yes\n"), 0o600)
	base := filepath.Join(dir, "base.yaml")
	os.WriteFile(base, []byte("services:\n  x:\n    image: nginx:1.27\n    privileged: false\n"), 0o600)

	cases := map[string]string{
		"privileged":       "services:\n  a:\n    image: nginx:1.27\n    privileged: true\n",
		"cap_add":          "services:\n  a:\n    image: nginx:1.27\n    cap_add: [SYS_ADMIN]\n",
		"network_mode":     "services:\n  a:\n    image: nginx:1.27\n    network_mode: host\n",
		"pid":              "services:\n  a:\n    image: nginx:1.27\n    pid: host\n",
		"devices":          "services:\n  a:\n    image: nginx:1.27\n    devices: [\"/dev/sda:/dev/sda\"]\n",
		"bind mount":       "services:\n  a:\n    image: nginx:1.27\n    volumes: [\"/etc:/host-etc\"]\n",
		"relative bind":    "services:\n  a:\n    image: nginx:1.27\n    volumes: [\"./data:/data\"]\n",
		"docker socket":    "services:\n  a:\n    image: nginx:1.27\n    volumes: [\"/var/run/docker.sock:/var/run/docker.sock\"]\n",
		"anonymous volume": "services:\n  a:\n    image: nginx:1.27\n    volumes: [\"/data\"]\n",
		"undeclared":       "services:\n  a:\n    image: nginx:1.27\n    volumes: [\"data:/data\"]\n",
		"driver_opts bind": "services:\n  a:\n    image: nginx:1.27\n    volumes: [\"d:/d\"]\nvolumes:\n  d:\n    driver_opts: {type: none, o: bind, device: /etc}\n",
		"external volume":  "services:\n  a:\n    image: nginx:1.27\n    volumes: [\"d:/d\"]\nvolumes:\n  d:\n    external: true\n",
		"build":            "services:\n  a:\n    build: .\n",
		"env_file":         "services:\n  a:\n    image: nginx:1.27\n    env_file: " + secret + "\n",
		"extends file":     "services:\n  a:\n    extends:\n      file: " + base + "\n      service: x\n",
		"include":          "include:\n  - " + base + "\nservices:\n  a:\n    image: nginx:1.27\n",
		"secrets":          "services:\n  a:\n    image: nginx:1.27\nsecrets:\n  s:\n    file: " + secret + "\n",
		"security_opt":     "services:\n  a:\n    image: nginx:1.27\n    security_opt: [\"seccomp:unconfined\"]\n",
		"sysctls":          "services:\n  a:\n    image: nginx:1.27\n    sysctls: {net.ipv4.ip_forward: 1}\n",
		"replicas":         "services:\n  a:\n    image: nginx:1.27\n    deploy: {replicas: 3}\n",
		"basalt label":     "services:\n  a:\n    image: nginx:1.27\n    labels: {basalt.app: \"1\"}\n",
		"cycle":            "services:\n  a:\n    image: nginx:1.27\n    depends_on: [b]\n  b:\n    image: nginx:1.27\n    depends_on: [a]\n",
		"bad image":        "services:\n  a:\n    image: \"nginx; rm -rf /\"\n",
		"no image":         "services:\n  a:\n    command: [x]\n",
		"profiles":         "services:\n  a:\n    image: nginx:1.27\n    profiles: [debug]\n  b:\n    image: nginx:1.27\n",
		"fixed ip":         "services:\n  a:\n    image: nginx:1.27\n    networks:\n      n:\n        ipv4_address: 10.0.0.5\nnetworks:\n  n: {}\n",
		"bad env name":     "services:\n  a:\n    image: nginx:1.27\n    environment: {\"a-b\": x}\n",
		"empty":            "",
		"no services":      "volumes:\n  d: {}\n",
		"not yaml":         "services: [",
		"bad restart":      "services:\n  a:\n    image: nginx:1.27\n    restart: sometimes\n",
	}
	for name, y := range cases {
		p, err := Parse(context.Background(), []byte(y), nil)
		if err == nil {
			t.Errorf("%s: accepted: %+v", name, p.Services)
			continue
		}
		if strings.Contains(err.Error(), "LEAK") || strings.Contains(err.Error(), dir) && name != "env_file" && name != "extends file" && name != "include" && name != "secrets" {
			t.Errorf("%s: error reveals file contents or paths: %v", name, err)
		}
	}
}

func TestLimits(t *testing.T) {
	big := "services:\n" + strings.Repeat("  # padding padding padding padding padding padding padding\n", 5000)
	if _, err := Parse(context.Background(), []byte(big), nil); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("oversized file: %v", err)
	}
	// A YAML alias bomb is cut short by compose-go's node visit limit.
	bomb := "x-a: &a [x, x, x, x, x, x, x, x, x, x]\nx-b: &b [*a, *a, *a, *a, *a, *a, *a, *a, *a, *a]\n" +
		"x-c: &c [*b, *b, *b, *b, *b, *b, *b, *b, *b, *b]\nx-d: &d [*c, *c, *c, *c, *c, *c, *c, *c, *c, *c]\n" +
		"x-e: &e [*d, *d, *d, *d, *d, *d, *d, *d, *d, *d]\nx-f: &f [*e, *e, *e, *e, *e, *e, *e, *e, *e, *e]\n" +
		"services:\n  a:\n    image: nginx:1.27\n    labels: {x: *f}\n"
	if _, err := Parse(context.Background(), []byte(bomb), nil); err == nil {
		t.Fatal("alias bomb accepted")
	}
}
