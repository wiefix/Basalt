package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/docker"
)

// TestDatabaseAppAPI needs no Docker: creation, credentials, links, roles.
func TestDatabaseAppAPI(t *testing.T) {
	p := startPanel(t)
	h := startHost(t, p)
	sid := mustAtoi(h.id)
	if _, err := p.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	create := func(want int, body map[string]any) map[string]any {
		body["server_id"] = sid
		return p.call(want, "POST", "/api/v1/apps", body)
	}
	if e := p.call(200, "GET", "/api/v1/database-engines", nil)["engines"].([]any); len(e) != 4 {
		t.Fatalf("engines = %v", e)
	}
	for _, bad := range []map[string]any{
		{"name": "pg", "source": "database", "engine": "oracle"},
		{"name": "pg", "source": "database", "engine": "postgres", "version": "9.6"},
		{"name": "Bad_Name", "source": "database", "engine": "postgres"},
		{"name": "app", "source": "database", "engine": "redis"},
		{"name": "pg", "source": "database", "engine": "postgres", "domains": []string{"pg.example.com"}},
	} {
		create(http.StatusUnprocessableEntity, bad)
	}
	db := create(http.StatusCreated, map[string]any{"name": "pg", "source": "database", "engine": "postgres", "memory_mb": 256})["app"].(map[string]any)
	if db["engine"] != "postgres" || db["version"] != "18" || db["image"] != "postgres:18-alpine" || db["port"] != float64(0) {
		t.Fatalf("database = %v", db)
	}
	base := "/api/v1/apps/" + jsonID(db["id"])

	// Credentials are generated and stored encrypted.
	conn := p.call(200, "GET", base+"/connection", nil)["connection"].(map[string]any)
	creds := conn["credentials"].(map[string]any)
	pw := creds["password"].(string)
	if conn["host"] != "pg" || conn["port"] != float64(5432) || creds["database"] != "pg" || len(pw) < 26 ||
		conn["url"] != "postgres://pg:"+pw+"@pg:5432/pg" || conn["env_var"] != "DATABASE_URL" {
		t.Fatalf("connection = %v", conn)
	}
	var sealed []byte
	p.db.R.QueryRow(`SELECT env FROM apps WHERE id = ?`, mustAtoi(jsonID(db["id"]))).Scan(&sealed)
	if len(sealed) == 0 || strings.Contains(string(sealed), pw) {
		t.Fatal("credentials stored in clear")
	}
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/env", map[string]any{"env": map[string]string{"POSTGRES_PASSWORD": "x"}})
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/domains", map[string]any{"domains": []string{"pg.example.com"}})
	upd := p.call(200, "PUT", base, map[string]any{"image": "evil:1", "memory_mb": 512, "cpus": 1.5})["app"].(map[string]any)
	if upd["image"] != "postgres:18-alpine" || upd["memory_mb"] != float64(512) || upd["version"] != "18" {
		t.Fatalf("updated = %v", upd)
	}

	// Links: to apps on the same server, not to databases; the URL can go
	// straight into the app's environment.
	web := create(http.StatusCreated, map[string]any{"name": "web", "source": "image", "image": "nginx:1.27", "port": 80})["app"].(map[string]any)
	wid := mustAtoi(jsonID(web["id"]))
	p.call(200, "PUT", "/api/v1/apps/"+jsonID(web["id"])+"/env", map[string]any{"env": map[string]string{"KEEP": "1"}})
	cache := create(http.StatusCreated, map[string]any{"name": "cache", "source": "database", "engine": "redis"})["app"].(map[string]any)
	p.call(http.StatusUnprocessableEntity, "POST", base+"/links", map[string]any{"app_id": mustAtoi(jsonID(cache["id"]))})
	p.call(http.StatusUnprocessableEntity, "POST", base+"/links", map[string]any{"app_id": 999})
	p.call(http.StatusUnprocessableEntity, "POST", base+"/links", map[string]any{"app_id": wid, "env_var": "1BAD"})
	stack := create(http.StatusCreated, map[string]any{"name": "stack", "source": "compose",
		"compose": "services:\n  pg:\n    image: postgres:17\n  web:\n    image: nginx:1.27\n"})["app"].(map[string]any)
	p.call(http.StatusConflict, "POST", base+"/links", map[string]any{"app_id": mustAtoi(jsonID(stack["id"]))})

	r := p.call(200, "POST", base+"/links", map[string]any{"app_id": wid, "env_var": "DATABASE_URL"})
	if links := r["app"].(map[string]any)["links"].([]any); len(links) != 1 || links[0] != float64(wid) {
		t.Fatalf("links = %v", r["app"])
	}
	if !strings.Contains(jsonString(r["attach_error"]), "Docker is not available") {
		t.Fatalf("attach_error = %v", r["attach_error"])
	}
	env := p.call(200, "GET", "/api/v1/apps/"+jsonID(web["id"])+"/env", nil)["env"].(map[string]any)
	if env["DATABASE_URL"] != conn["url"] || env["KEEP"] != "1" {
		t.Fatalf("app env = %v", env)
	}
	// A linked compose app cannot take the database's name for a service.
	p.call(http.StatusConflict, "POST", base+"/links", map[string]any{"app_id": mustAtoi(jsonID(stack["id"]))})
	stack2 := create(http.StatusCreated, map[string]any{"name": "stack2", "source": "compose",
		"compose": "services:\n  web:\n    image: nginx:1.27\n"})["app"].(map[string]any)
	p.call(200, "POST", base+"/links", map[string]any{"app_id": mustAtoi(jsonID(stack2["id"]))})
	p.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/apps/"+jsonID(stack2["id"])+"/compose",
		map[string]any{"compose": "services:\n  pg:\n    image: postgres:17\n"})

	p.call(200, "DELETE", base+"/links/"+jsonID(web["id"]), nil)
	p.call(http.StatusNotFound, "DELETE", base+"/links/"+jsonID(web["id"]), nil)
	p.call(http.StatusNotFound, "POST", "/api/v1/apps/"+jsonID(web["id"])+"/links", map[string]any{"app_id": 1})

	// Deleting a linked app drops the link.
	p.call(200, "DELETE", "/api/v1/apps/"+jsonID(stack2["id"]), nil)
	if l := p.call(200, "GET", base, nil)["app"].(map[string]any)["links"]; len(l.([]any)) != 0 {
		t.Fatalf("links after delete = %v", l)
	}

	// The deployment reaches the agent; status and logs are per service.
	dep := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
	if d, log := followDeployment(t, p, base, jsonID(dep["id"])); d["status"] != "failed" || !strings.Contains(log, "Docker is not available") {
		t.Fatalf("deployment: %v %q", d, log)
	}

	// Viewers see the database but not its credentials.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer"})
	v := *p
	v.hc = &http.Client{Jar: newJar()}
	v.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	if s := jsonString(v.call(200, "GET", base, nil)); strings.Contains(s, pw) || !strings.Contains(s, "postgres") {
		t.Fatalf("viewer view = %s", s)
	}
	v.call(http.StatusForbidden, "GET", base+"/connection", nil)
	v.call(http.StatusForbidden, "POST", base+"/links", map[string]any{"app_id": wid})
	p.call(http.StatusNotFound, "GET", "/api/v1/apps/"+jsonID(web["id"])+"/connection", nil)
}

// TestDatabaseWithDocker runs PostgreSQL and Redis on the local Docker
// daemon and reaches them from a linked app's network. Opt-in.
func TestDatabaseWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	p := startPanel(t)
	h := startHostWith(t, p, sock)
	dc := docker.New(sock)
	sid := mustAtoi(h.id)
	create := func(body map[string]any) string {
		body["server_id"] = sid
		id := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/apps", body)["app"].(map[string]any)["id"])
		t.Cleanup(func() {
			ctx := context.Background()
			list, _ := dc.ContainerList(ctx, appLabels(id))
			for _, c := range list {
				dc.ContainerRemove(ctx, c.ID, true)
			}
			dc.NetworkRemove(ctx, appNet(id))
			dc.VolumeRemove(ctx, appNet(id)+"-data")
		})
		return id
	}
	deploy := func(id string) (map[string]any, string) {
		t.Helper()
		base := "/api/v1/apps/" + id
		dep := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
		return followDeployment(t, p, base, jsonID(dep["id"]))
	}
	// run starts a throwaway container on an app's network, as the app's
	// own containers would be, and returns its output.
	run := func(network string, args ...string) (string, error) {
		out, err := exec.Command("docker", append([]string{"--host", "unix://" + sock, "run", "--rm", "--network", network}, args...)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	web := create(map[string]any{"name": "web", "source": "image", "image": "traefik/whoami:v1.10", "port": 80})
	pg := create(map[string]any{"name": "pg", "source": "database", "engine": "postgres"})
	d, log := deploy(pg)
	if d["status"] != "succeeded" || !strings.Contains(log, "healthy") {
		t.Fatalf("postgres deployment: %v\n%s", d, log)
	}
	t.Logf("postgres deployment:\n%s", log)
	if st := p.call(200, "GET", "/api/v1/apps/"+pg+"/status", nil)["status"].(map[string]any); st["state"] != "running" || st["health"] != "healthy" {
		t.Fatalf("status = %v", st)
	}

	// Linked while running: the app's network reaches it by name, with the
	// URL put into the app's environment.
	r := p.call(200, "POST", "/api/v1/apps/"+pg+"/links", map[string]any{"app_id": mustAtoi(web), "env_var": "DATABASE_URL"})
	if r["attach_error"] != nil {
		t.Fatalf("attach: %v", r["attach_error"])
	}
	url := p.call(200, "GET", "/api/v1/apps/"+web+"/env", nil)["env"].(map[string]any)["DATABASE_URL"].(string)
	if out, err := run(appNet(web), "postgres:18-alpine", "psql", url, "-tAc", "CREATE TABLE t (v text); INSERT INTO t VALUES ('kept'); SELECT 1"); err != nil || !strings.HasSuffix(out, "1") {
		t.Fatalf("psql from the app network: %v %s", err, out)
	}
	// Other networks do not reach it.
	if _, err := run("bridge", "postgres:18-alpine", "pg_isready", "-h", "pg", "-t", "2"); err == nil {
		t.Fatal("database reachable outside linked apps")
	}

	// Redeploying an unchanged database keeps its container and links; a
	// new limit replaces the container (stopped first), data and links stay.
	if d, log := deploy(pg); d["status"] != "succeeded" || !strings.Contains(log, "Service pg is unchanged") {
		t.Fatalf("redeploy: %v\n%s", d, log)
	}
	p.call(200, "PUT", "/api/v1/apps/"+pg, map[string]any{"memory_mb": 300})
	if d, log := deploy(pg); d["status"] != "succeeded" || !strings.Contains(log, "Stopping service pg") {
		t.Fatalf("deploy with a limit: %v\n%s", d, log)
	}
	if out, err := run(appNet(web), "postgres:18-alpine", "psql", url, "-tAc", "SELECT v FROM t"); err != nil || out != "kept" {
		t.Fatalf("data after replacement: %v %s", err, out)
	}

	// A new password: changed in place, the linked app's URL follows; the
	// old one no longer signs in.
	pw := p.call(200, "POST", "/api/v1/apps/"+pg+"/password", map[string]any{})
	if apps := pw["updated_apps"].([]any); len(apps) != 1 || apps[0] != "web" {
		t.Fatalf("password change = %v", pw)
	}
	newURL := p.call(200, "GET", "/api/v1/apps/"+web+"/env", nil)["env"].(map[string]any)["DATABASE_URL"].(string)
	if newURL == url || newURL != pw["connection"].(map[string]any)["url"] {
		t.Fatalf("app URL %q after the change (was %q)", newURL, url)
	}
	if out, err := run(appNet(web), "postgres:18-alpine", "psql", newURL, "-tAc", "SELECT v FROM t"); err != nil || out != "kept" {
		t.Fatalf("new password: %v %s", err, out)
	}
	if _, err := run(appNet(web), "postgres:18-alpine", "psql", url, "-tAc", "SELECT 1"); err == nil {
		t.Fatal("the old password still signs in")
	}
	url = newURL

	// Unlinked: no longer reachable.
	p.call(200, "DELETE", "/api/v1/apps/"+pg+"/links/"+web, nil)
	if _, err := run(appNet(web), "postgres:18-alpine", "pg_isready", "-h", "pg", "-t", "2"); err == nil {
		t.Fatal("database reachable after unlinking")
	}

	// Redis, linked before its first deployment.
	cache := create(map[string]any{"name": "cache", "source": "database", "engine": "redis"})
	p.call(200, "POST", "/api/v1/apps/"+cache+"/links", map[string]any{"app_id": mustAtoi(web), "env_var": "REDIS_URL"})
	if d, log := deploy(cache); d["status"] != "succeeded" {
		t.Fatalf("redis deployment: %v\n%s", d, log)
	}
	redisURL := p.call(200, "GET", "/api/v1/apps/"+web+"/env", nil)["env"].(map[string]any)["REDIS_URL"].(string)
	if out, err := run(appNet(web), "redis:8-alpine", "redis-cli", "-u", redisURL, "--no-auth-warning", "ping"); err != nil || out != "PONG" {
		t.Fatalf("redis from the app network: %v %s", err, out)
	}
	if out, _ := run(appNet(web), "redis:8-alpine", "redis-cli", "-h", "cache", "ping"); strings.Contains(out, "PONG") {
		t.Fatal("redis answers without a password")
	}
	// Redis takes a new password with a new container.
	p.call(200, "POST", "/api/v1/apps/"+cache+"/password", map[string]any{})
	deps := p.call(200, "GET", "/api/v1/apps/"+cache+"/deployments", nil)["deployments"].([]any)
	if d, log := followDeployment(t, p, "/api/v1/apps/"+cache, jsonID(deps[0].(map[string]any)["id"])); d["status"] != "succeeded" {
		t.Fatalf("redis redeployment: %v\n%s", d, log)
	}
	newRedis := p.call(200, "GET", "/api/v1/apps/"+web+"/env", nil)["env"].(map[string]any)["REDIS_URL"].(string)
	if out, err := run(appNet(web), "redis:8-alpine", "redis-cli", "-u", newRedis, "--no-auth-warning", "ping"); err != nil || out != "PONG" || newRedis == redisURL {
		t.Fatalf("redis with the new password: %v %s", err, out)
	}
	if out, _ := run(appNet(web), "redis:8-alpine", "redis-cli", "-u", redisURL, "--no-auth-warning", "ping"); out == "PONG" {
		t.Fatal("redis still takes the old password")
	}

	// Deleting the app detaches the database from its network.
	p.call(200, "DELETE", "/api/v1/apps/"+web, nil)
	if _, err := dc.NetworkInspect(context.Background(), appNet(web)); err == nil {
		t.Fatal("app network left behind")
	}
	if st := p.call(200, "GET", "/api/v1/apps/"+cache+"/status", nil)["status"].(map[string]any); st["state"] != "running" {
		t.Fatalf("redis after the app was deleted: %v", st)
	}
	p.call(200, "DELETE", "/api/v1/apps/"+pg+"?volumes=1", nil)
	p.call(200, "DELETE", "/api/v1/apps/"+cache+"?volumes=1", nil)
	if vols, _ := dc.VolumeList(context.Background(), appLabels(pg)); len(vols) != 0 {
		t.Fatalf("volumes left: %v", vols)
	}
}

// TestEveryEngineWithDocker deploys the default version of every engine and
// signs in with the client of its image. Opt-in and slow (large images):
// run it when changing engines or versions.
func TestEveryEngineWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DB_ENGINE_TESTS") != "1" {
		t.Skip("set BASALT_DB_ENGINE_TESTS=1 to run every engine against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	p := startPanel(t)
	h := startHostWith(t, p, sock)
	dc := docker.New(sock)
	web := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
		"server_id": mustAtoi(h.id), "name": "web", "source": "image", "image": "traefik/whoami:v1.10", "port": 80,
	})["app"].(map[string]any)["id"])
	t.Cleanup(func() { p.call(200, "DELETE", "/api/v1/apps/"+web, nil) })
	_, haveRestic := exec.LookPath("restic")
	var storage any
	if haveRestic == nil {
		eventually(t, "restic reported", func() bool { return p.server(h.id)["restic"] != "" })
		storage = p.call(http.StatusCreated, "POST", "/api/v1/backup-storages", map[string]any{
			"name": "local", "kind": "local", "path": filepath.Join(t.TempDir(), "backups")})["storage"].(map[string]any)["id"]
	}
	for _, e := range p.call(200, "GET", "/api/v1/database-engines", nil)["engines"].([]any) {
		engine := e.(map[string]any)["id"].(string)
		t.Run(engine, func(t *testing.T) {
			app := p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{
				"server_id": mustAtoi(h.id), "name": engine + "-db", "source": "database", "engine": engine,
			})["app"].(map[string]any)
			id, image := jsonID(app["id"]), app["image"].(string)
			base := "/api/v1/apps/" + id
			t.Cleanup(func() {
				p.call(200, "DELETE", base+"?volumes=1", nil)
				dc.VolumeRemove(context.Background(), appNet(id)+"-data")
			})
			p.call(200, "POST", base+"/links", map[string]any{"app_id": mustAtoi(web)})
			dep := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
			if d, log := followDeployment(t, p, base, jsonID(dep["id"])); d["status"] != "succeeded" {
				t.Fatalf("deployment: %v\n%s", d, log)
			}
			c := p.call(200, "GET", base+"/connection", nil)["connection"].(map[string]any)
			cr := c["credentials"].(map[string]any)
			host, pw := c["host"].(string), cr["password"].(string)
			user, _ := cr["user"].(string) // none for Redis
			db, _ := cr["database"].(string)
			var args []string
			switch engine {
			case "postgres":
				args = []string{"psql", c["url"].(string), "-tAc", "SELECT 1"}
			case "mysql":
				args = []string{"mysql", "-h", host, "-u", user, "-p" + pw, db, "-sNe", "SELECT 1"}
			case "mariadb":
				args = []string{"mariadb", "-h", host, "-u", user, "-p" + pw, db, "-sNe", "SELECT 1"}
			case "redis":
				args = []string{"redis-cli", "-u", c["url"].(string), "--no-auth-warning", "ping"}
			}
			out, err := exec.Command("docker", append([]string{"--host", "unix://" + sock, "run", "--rm",
				"--network", appNet(web), image}, args...)...).CombinedOutput()
			if s := strings.TrimSpace(string(out)); err != nil || !strings.HasSuffix(s, "1") && s != "PONG" {
				t.Fatalf("client: %v %s", err, s)
			}
			if engine != "redis" {
				// A new password signs in; the old one no longer does.
				nc := p.call(200, "POST", base+"/password", map[string]any{})["connection"].(map[string]any)
				npw := nc["credentials"].(map[string]any)["password"].(string)
				sign := func(pass, url string) error {
					var a []string
					switch engine {
					case "postgres":
						a = []string{"psql", url, "-tAc", "SELECT 1"}
					case "mysql":
						a = []string{"mysql", "-h", host, "-u", user, "-p" + pass, db, "-sNe", "SELECT 1"}
					case "mariadb":
						a = []string{"mariadb", "-h", host, "-u", user, "-p" + pass, db, "-sNe", "SELECT 1"}
					}
					out, err := exec.Command("docker", append([]string{"--host", "unix://" + sock, "run", "--rm",
						"--network", appNet(web), image}, a...)...).CombinedOutput()
					if err == nil && !strings.HasSuffix(strings.TrimSpace(string(out)), "1") {
						err = fmt.Errorf("%s", out)
					}
					return err
				}
				if err := sign(npw, nc["url"].(string)); err != nil {
					t.Fatalf("new password: %v", err)
				}
				if sign(pw, c["url"].(string)) == nil {
					t.Fatal("the old password still signs in")
				}
				pw = npw
			}
			if storage == nil {
				return
			}
			// Backup and restore: a value changed after the backup comes back.
			query := func(q string) string {
				t.Helper()
				ctr := strings.TrimSpace(dockerOut(t, sock, "ps", "-q", "--filter", "label=basalt.namespace="+e2eNamespace, "--filter", "label=basalt.app="+id))
				var args []string
				switch engine {
				case "postgres":
					args = []string{"psql", "-U", user, "-d", db, "-tAc", q}
				case "mysql":
					args = []string{"sh", "-c", `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -uroot -N "$0" -e "$1"`, db, q}
				case "mariadb":
					args = []string{"sh", "-c", `MYSQL_PWD="$MARIADB_ROOT_PASSWORD" mariadb -uroot -N "$0" -e "$1"`, db, q}
				case "redis":
					args = append([]string{"redis-cli"}, strings.Fields(q)...)
				}
				return strings.TrimSpace(dockerOut(t, sock, append([]string{"exec", ctr}, args...)...))
			}
			if engine == "redis" {
				query("SET k kept")
				time.Sleep(1500 * time.Millisecond) // appendfsync everysec
			} else {
				query("CREATE TABLE t (v VARCHAR(10)); INSERT INTO t VALUES ('kept')")
			}
			p.call(200, "PUT", base+"/backup-policy", map[string]any{"storage_id": storage, "schedule": "manual", "keep_last": 2})
			appBackupRound(t, p, id, func() {
				if engine == "redis" {
					query("SET k damaged")
				} else {
					query("UPDATE t SET v = 'damaged'")
				}
			})
			got := ""
			eventually(t, "restored value", func() bool {
				if engine == "redis" {
					got = query("GET k")
				} else {
					got = query("SELECT v FROM t")
				}
				return got == "kept"
			})
		})
	}
}

func dockerOut(t *testing.T, sock string, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"--host", "unix://" + sock}, args...)...).CombinedOutput()
	if err != nil {
		t.Logf("docker %v: %v %s", args, err, out)
	}
	return string(out)
}
