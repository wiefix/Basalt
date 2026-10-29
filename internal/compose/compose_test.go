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

func TestParseRepoBuild(t *testing.T) {
	const y = `
services:
  web:
    build:
      context: ./web
      dockerfile: docker/Dockerfile.prod
      target: runtime
      args:
        NODE_ENV: production
        FROM_ENV:
        MISSING:
    image: myorg/web:latest
    ports: ["3000"]
    depends_on: [migrate]
  migrate:
    build: .
    command: ["./migrate"]
  cache:
    image: redis:7.4
`
	env := map[string]string{"FROM_ENV": "x"}
	if _, err := Parse(context.Background(), []byte(y), env); err == nil || !strings.Contains(err.Error(), "git repository") {
		t.Fatalf("build outside a repository: %v", err)
	}
	p, err := ParseRepo(context.Background(), []byte(y), env, ".")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]agentproto.ServiceSpec{}
	for _, s := range p.Services {
		byName[s.Name] = s
	}
	web := byName["web"]
	if web.Image != BuildImage("web") || web.Build == nil {
		t.Fatalf("web = %+v", web)
	}
	if b := *web.Build; b.Context != "web" || b.Dockerfile != "web/docker/Dockerfile.prod" || b.Target != "runtime" ||
		len(b.Args) != 2 || b.Args["NODE_ENV"] != "production" || b.Args["FROM_ENV"] != "x" {
		t.Fatalf("web build = %+v", b)
	}
	if b := byName["migrate"].Build; b == nil || b.Context != "." || b.Dockerfile != "Dockerfile" {
		t.Fatalf("migrate build = %+v", b)
	}
	if byName["cache"].Build != nil || byName["cache"].Image != "redis:7.4" {
		t.Fatalf("cache = %+v", byName["cache"])
	}

	for name, b := range map[string]string{
		"parent context":    "build: ../other",
		"absolute context":  "build: /etc",
		"remote context":    "build: https://github.com/o/r.git",
		"escape dockerfile": "build: {context: web, dockerfile: ../../../etc/Dockerfile}",
		"abs dockerfile":    "build: {context: ., dockerfile: /etc/Dockerfile}",
		"inline":            "build: {context: ., dockerfile_inline: \"FROM alpine\"}",
		"ssh":               "build: {context: ., ssh: [default]}",
		"secrets":           "build: {context: ., secrets: [s]}",
		"extra contexts":    "build: {context: ., additional_contexts: {x: /etc}}",
		"network":           "build: {context: ., network: host}",
		"privileged":        "build: {context: ., privileged: true}",
		"buildkit arg":      "build: {context: ., args: {BUILDKIT_SYNTAX: evil/frontend}}",
		"docker arg":        "build: {context: ., args: {DOCKER_HOST: tcp://x}}",
		"bad target":        "build: {context: ., target: \"-x\"}",
	} {
		y := "services:\n  a:\n    " + b + "\n"
		if p, err := ParseRepo(context.Background(), []byte(y), nil, "."); err == nil {
			t.Errorf("%s: accepted: %+v", name, p.Services[0].Build)
		}
	}
}

// TestParseRepoDir: paths are relative to the compose file's directory, and
// may reach elsewhere in the repository, not outside it.
func TestParseRepoDir(t *testing.T) {
	y := "services:\n  a:\n    build: ./web\n  b:\n    build: {context: ../shared, dockerfile: docker/Dockerfile}\n"
	p, err := ParseRepo(context.Background(), []byte(y), nil, "deploy/prod")
	if err != nil {
		t.Fatal(err)
	}
	if a, b := p.Services[0].Build, p.Services[1].Build; a.Context != "deploy/prod/web" || a.Dockerfile != "deploy/prod/web/Dockerfile" ||
		b.Context != "deploy/shared" || b.Dockerfile != "deploy/shared/docker/Dockerfile" {
		t.Fatalf("builds = %+v, %+v", a, b)
	}
	if _, err := ParseRepo(context.Background(), []byte("services:\n  a:\n    build: ../../..\n"), nil, "deploy/prod"); err == nil {
		t.Fatal("context outside the repository accepted")
	}
	if _, err := ParseRepo(context.Background(), []byte(y), nil, "../x"); err == nil {
		t.Fatal("directory outside the repository accepted")
	}
}
