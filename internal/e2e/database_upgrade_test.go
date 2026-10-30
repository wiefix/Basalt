package e2e

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/docker"
)

// TestDatabaseUpgradeWithDocker moves a PostgreSQL database to a newer
// major version (its data dumped into a new cluster in the same volume)
// and checks that every database and role came along, the old data is
// gone and no rollback goes back. With BASALT_DB_ENGINE_TESTS, MariaDB and
// Redis too (they read their old files).
func TestDatabaseUpgradeWithDocker(t *testing.T) {
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
	ctx := context.Background()
	create := func(body map[string]any) string {
		body["server_id"] = sid
		id := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/apps", body)["app"].(map[string]any)["id"])
		t.Cleanup(func() {
			list, _ := dc.ContainerList(ctx, appLabels(id))
			for _, c := range list {
				dc.ContainerRemove(ctx, c.ID, true)
			}
			dc.NetworkRemove(ctx, appNet(id))
			dc.VolumeRemove(ctx, appNet(id)+"-data")
		})
		return id
	}
	follow := func(id string, dep map[string]any) (map[string]any, string) {
		t.Helper()
		return followDeployment(t, p, "/api/v1/apps/"+id, jsonID(dep["id"]))
	}
	deploy := func(id string) {
		t.Helper()
		dep := p.call(http.StatusAccepted, "POST", "/api/v1/apps/"+id+"/deploy", map[string]any{})["deployment"].(map[string]any)
		if d, log := follow(id, dep); d["status"] != "succeeded" {
			t.Fatalf("deployment: %v\n%s", d, log)
		}
	}
	// exec runs a command in the app's database container.
	exec_ := func(id string, args ...string) string {
		t.Helper()
		list, err := dc.ContainerList(ctx, appLabels(id))
		if err != nil || len(list) == 0 {
			t.Fatalf("containers of %s: %v %v", id, list, err)
		}
		out, err := exec.Command("docker", append([]string{"--host", "unix://" + sock, "exec", list[0].ID}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	upgrade := func(id, version string) (map[string]any, string) {
		t.Helper()
		r := p.call(http.StatusAccepted, "POST", "/api/v1/apps/"+id+"/upgrade", map[string]any{"version": version})
		return follow(id, r["deployment"].(map[string]any))
	}

	pg := create(map[string]any{"name": "pg", "source": "database", "engine": "postgres", "version": "16"})
	deploy(pg)
	first := jsonID(p.call(200, "GET", "/api/v1/apps/"+pg+"/deployments", nil)["deployments"].([]any)[0].(map[string]any)["id"])
	psql := func(db, sql string) string {
		return exec_(pg, "psql", "-U", "pg", "-d", db, "-tAc", sql)
	}
	psql("pg", "CREATE TABLE t (v text); INSERT INTO t VALUES ('kept')")
	psql("pg", "CREATE ROLE other LOGIN PASSWORD 'other-secret'")
	psql("pg", "CREATE DATABASE second OWNER other")
	psql("second", "CREATE TABLE s (n int); INSERT INTO s VALUES (42)")

	info := p.call(200, "GET", "/api/v1/apps/"+pg+"/upgrade", nil)
	if info["version"] != "16" || info["dump"] != true || len(info["targets"].([]any)) != 1 || info["targets"].([]any)[0] != "18" {
		t.Fatalf("upgrade info = %v", info)
	}
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/apps/"+pg+"/upgrade", map[string]any{"version": "17"})
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/apps/"+pg+"/upgrade", map[string]any{"version": "15"})
	if e := p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/apps/"+pg+"/upgrade", map[string]any{"version": "18", "backup": true}); !strings.Contains(e["error"].(string), "Backups") {
		t.Fatalf("backup without a policy: %v", e)
	}

	d, log := upgrade(pg, "18")
	t.Logf("upgrade:\n%s", log)
	if d["status"] != "succeeded" || !strings.Contains(log, "Copied") || !strings.Contains(log, "Removed the old version's data") {
		t.Fatalf("upgrade: %v\n%s", d, log)
	}
	app := p.call(200, "GET", "/api/v1/apps/"+pg, nil)["app"].(map[string]any)
	if app["version"] != "18" || app["image"] != "postgres:18-alpine" {
		t.Fatalf("after the upgrade: %v", app)
	}
	if v := psql("pg", "SHOW server_version_num"); !strings.HasPrefix(v, "18") {
		t.Fatalf("server version %s", v)
	}
	if got := psql("pg", "SELECT v FROM t"); got != "kept" {
		t.Fatalf("table after the upgrade: %q", got)
	}
	if got := psql("second", "SELECT n FROM s"); got != "42" {
		t.Fatalf("second database after the upgrade: %q", got)
	}
	// The other role signs in with its password, over TCP.
	if out := exec_(pg, "sh", "-c", `PGPASSWORD=other-secret psql -h 127.0.0.1 -U other -d second -tAc "SELECT current_user"`); out != "other" {
		t.Fatalf("other role: %q", out)
	}
	// Only the new version's directory is left in the volume.
	ls, err := exec.Command("docker", "--host", "unix://"+sock, "run", "--rm", "--network", "none", "--mount",
		"type=volume,source="+appNet(pg)+"-data,target=/v", "busybox:1.37", "ls", "-A", "/v").CombinedOutput()
	if err != nil || strings.TrimSpace(string(ls)) != "18" {
		t.Fatalf("volume after the upgrade: %v %q", err, ls)
	}
	// Leftover upgrade containers are gone.
	if out, _ := exec.Command("docker", "--host", "unix://"+sock, "ps", "-a", "--filter", "name="+appNet(pg)+"-upgrade", "-q").Output(); len(strings.TrimSpace(string(out))) != 0 {
		t.Fatalf("upgrade containers left: %s", out)
	}
	// No way back to 16.
	if e := p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/apps/"+pg+"/deploy", map[string]any{"rollback_to": mustAtoi(first)}); !strings.Contains(e["error"].(string), "upgraded") {
		t.Fatalf("rollback to 16: %v", e)
	}
	if info := p.call(200, "GET", "/api/v1/apps/"+pg+"/upgrade", nil); len(info["targets"].([]any)) != 0 {
		t.Fatalf("targets after the upgrade: %v", info)
	}
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/apps/"+pg+"/upgrade", map[string]any{"version": "18"})
	// The deployment after it (unchanged) keeps everything.
	deploy(pg)
	if got := psql("pg", "SELECT v FROM t"); got != "kept" {
		t.Fatalf("after a redeployment: %q", got)
	}

	if os.Getenv("BASALT_DB_ENGINE_TESTS") != "1" {
		return
	}
	my := create(map[string]any{"name": "my", "source": "database", "engine": "mariadb", "version": "11.4"})
	deploy(my)
	mysql := func(sql string) string {
		return exec_(my, "sh", "-c", `MYSQL_PWD="$MARIADB_ROOT_PASSWORD" mariadb -uroot -N -e "$0" my`, sql)
	}
	mysql("CREATE TABLE t (v text); INSERT INTO t VALUES ('kept')")
	if d, log := upgrade(my, "11.8"); d["status"] != "succeeded" {
		t.Fatalf("mariadb upgrade: %v\n%s", d, log)
	}
	if v := mysql("SELECT VERSION()"); !strings.HasPrefix(v, "11.8") || mysql("SELECT v FROM t") != "kept" {
		t.Fatalf("mariadb after the upgrade: %s", v)
	}

	rd := create(map[string]any{"name": "rd", "source": "database", "engine": "redis", "version": "7.4"})
	deploy(rd)
	redis := func(args ...string) string {
		return exec_(rd, append([]string{"sh", "-c", `REDISCLI_AUTH="$REDISCLI_AUTH" exec redis-cli "$@"`, "redis-cli"}, args...)...)
	}
	redis("SET", "k", "kept")
	redis("SAVE")
	if d, log := upgrade(rd, "8"); d["status"] != "succeeded" {
		t.Fatalf("redis upgrade: %v\n%s", d, log)
	}
	if v := redis("GET", "k"); v != "kept" {
		t.Fatalf("redis after the upgrade: %q", v)
	}
}
