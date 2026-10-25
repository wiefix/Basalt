//go:build linux_integration

// Run inside a disposable Linux container as root, with php-fpm installed
// and running (see deploy/dev/linux-test.sh). It creates real system users.
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/proxy/caddyrun"
)

func TestLinuxIsolationAndPHP(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	p := startPanel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	proxyDir, _ := os.MkdirTemp("/tmp", "bp")
	defer os.RemoveAll(proxyDir)
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- caddyrun.Run(ctx, proxyDir, log) }()
	defer func() { cancel(); <-proxyDone }()

	sitesRoot := "/srv/basalt-it/sites"
	os.RemoveAll("/srv/basalt-it")
	os.MkdirAll(sitesRoot, 0o755)

	created := p.call(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "linux"})
	sid := jsonID(created["server"].(map[string]any)["id"])
	agentDir := t.TempDir()
	if _, err := agent.Enroll(ctx, agent.EnrollOptions{
		DataDir: agentDir, PanelURL: p.agentURL,
		Token:    created["enrollment"].(map[string]any)["token"].(string),
		CASHA256: p.hub.CAFingerprint(),
	}); err != nil {
		t.Fatal(err)
	}
	httpsPort := freePort(t)
	// A WordPress-shaped archive whose front page connects to the database
	// with the wp-config.php Basalt writes.
	var wp bytes.Buffer
	gz := gzip.NewWriter(&wp)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"wordpress/index.php":       `<?php require __DIR__ . '/wp-config.php'; $m = new mysqli(DB_HOST, DB_USER, DB_PASSWORD, DB_NAME); echo $m->connect_errno ? "fail" : "wp-ok";`,
		"wordpress/wp-settings.php": "<?php\n",
	} {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		io.WriteString(tw, body)
	}
	tw.Close()
	gz.Close()
	wpSum := sha1.Sum(wp.Bytes())
	wpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha1") {
			io.WriteString(w, hex.EncodeToString(wpSum[:]))
			return
		}
		w.Write(wp.Bytes())
	}))
	defer wpServer.Close()
	a, err := agent.New(agent.Config{
		DataDir: agentDir, ProxyDir: proxyDir, SitesRoot: sitesRoot,
		HTTPPort: freePort(t), HTTPSPort: httpsPort, SFTPListen: "127.0.0.1:0",
		Isolate: true, PHPRunDir: "/run/basalt-it/php", MySQLSocket: "/run/mysqld/mysqld.sock",
		Restic: "restic", WordPressURL: wpServer.URL + "/latest.tar.gz",
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	go a.Run(ctx)
	// Online comes with the connection, the host's facts with its hello a
	// moment later (hostname is written with them).
	eventually(t, "agent online", func() bool { s := p.server(sid); return s["status"] == "online" && s["hostname"] != nil })
	srv := p.server(sid)
	versions := srv["php_versions"].([]any)
	if srv["isolated"] != true || len(versions) == 0 {
		t.Fatalf("server facts: isolated=%v php=%v", srv["isolated"], versions)
	}
	// Real Linux metrics: memory from /proc/meminfo, uptime from /proc/uptime.
	eventually(t, "linux metrics", func() bool {
		m := p.call(200, "GET", "/api/v1/servers/"+sid+"/metrics?minutes=5", nil)["metrics"].([]any)
		if len(m) == 0 {
			return false
		}
		last := m[len(m)-1].(map[string]any)
		return last["mem_total"].(float64) > 0 && last["mem_used"].(float64) > 0 && last["uptime"].(float64) > 0
	})
	if srv["kernel"] == nil || srv["distro"] == nil {
		t.Errorf("host facts missing: kernel=%v distro=%v", srv["kernel"], srv["distro"])
	}
	t.Logf("host: %v / kernel %v", srv["distro"], srv["kernel"])
	phpVersion := versions[0].(string)
	t.Logf("PHP %s", phpVersion)
	get := httpsGetter(t, httpsPort)

	// Two sites: A runs PHP, B is static and holds a secret.
	ra := p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(sid), "name": "alpha", "domains": []string{"alpha.localhost"}, "tls": "internal", "php_version": phpVersion,
	})
	if ra["provision_error"] != nil {
		t.Fatalf("provision A: %v", ra["provision_error"])
	}
	idA := jsonID(ra["site"].(map[string]any)["id"])
	rb := p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(sid), "name": "beta", "domains": []string{"beta.localhost"}, "tls": "internal",
	})
	idB := jsonID(rb["site"].(map[string]any)["id"])

	// System users and directory layout.
	uA, err := user.Lookup("basalt-s" + idA)
	if err != nil {
		t.Fatalf("site user missing: %v", err)
	}
	www, err := user.LookupGroup("basalt-www")
	if err != nil {
		t.Fatal(err)
	}
	uidA, _ := strconv.Atoi(uA.Uid)
	wwwGID, _ := strconv.Atoi(www.Gid)
	dirA := filepath.Join(sitesRoot, idA)
	for path, want := range map[string]os.FileMode{
		dirA: 0o750 | os.ModeSetgid, dirA + "/public": 0o750 | os.ModeSetgid, dirA + "/tmp": 0o700,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if fi.Mode()&(os.ModePerm|os.ModeSetgid) != want || int(st.Uid) != uidA {
			t.Errorf("%s: mode %v uid %d; want %v uid %d", path, fi.Mode(), st.Uid, want, uidA)
		}
		if path != dirA+"/tmp" && int(st.Gid) != wwwGID {
			t.Errorf("%s: gid %d; want basalt-www %d", path, st.Gid, wwwGID)
		}
	}

	// A secret in site B, and a probe script in site A.
	q := func(p string) string { return "?path=" + url.QueryEscape(p) }
	p.raw("PUT", "/api/v1/sites/"+idB+"/files/content"+q("/public/secret.txt"), "", []byte("B-SECRET"))
	probe := `<?php
header("Content-Type: application/json");
file_put_contents(__DIR__."/written.txt", "from php");
echo json_encode([
  "user"   => posix_getpwuid(posix_geteuid())["name"],
  "other"  => @file_get_contents("` + filepath.Join(sitesRoot, idB, "public", "secret.txt") + `"),
  "passwd" => @file_get_contents("/etc/passwd") !== false,
  "exec"   => is_callable("shell_exec"),
  "route"  => $_SERVER["REQUEST_URI"],
]);`
	if code, _, b := p.raw("PUT", "/api/v1/sites/"+idA+"/files/content"+q("/public/index.php"), "", []byte(probe)); code != http.StatusCreated {
		t.Fatalf("upload probe: %d %s", code, b)
	}
	fi, _ := os.Stat(filepath.Join(dirA, "public", "index.php"))
	if st := fi.Sys().(*syscall.Stat_t); int(st.Uid) != uidA || int(st.Gid) != wwwGID {
		t.Fatalf("uploaded file owned by %d:%d; want %d:%d", st.Uid, st.Gid, uidA, wwwGID)
	}
	os.Remove(filepath.Join(dirA, "public", "index.html")) // placeholder would win over index.php

	var res map[string]any
	eventually(t, "PHP executes", func() bool {
		code, body := get("alpha.localhost", "/blog/hello-world?x=1")
		if code != 200 {
			return false
		}
		return json.Unmarshal([]byte(body), &res) == nil
	})
	t.Logf("probe: %v", res)
	if res["user"] != "basalt-s"+idA {
		t.Errorf("PHP runs as %v; want basalt-s%s", res["user"], idA)
	}
	if res["other"] != false && res["other"] != "" {
		t.Errorf("PHP read another site's file: %v", res["other"])
	}
	if res["passwd"] != false {
		t.Error("PHP read /etc/passwd (open_basedir not applied)")
	}
	if res["exec"] != false {
		t.Error("shell_exec is callable")
	}
	if res["route"] != "/blog/hello-world?x=1" {
		t.Errorf("front controller route = %v", res["route"])
	}
	// The file PHP wrote is owned by the site and served by the proxy.
	if code, body := get("alpha.localhost", "/written.txt"); code != 200 || body != "from php" {
		t.Fatalf("file written by PHP: %d %q", code, body)
	}

	// SFTP uploads are owned by the site user too.
	acc := p.call(http.StatusCreated, "POST", "/api/v1/sites/"+idA+"/sftp-users", map[string]any{"username": "alpha"})
	port := int(acc["connection"].(map[string]any)["port"].(float64))
	c, err := ssh.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port), &ssh.ClientConfig{
		User: "alpha", Auth: []ssh.AuthMethod{ssh.Password(acc["password"].(string))}, HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := sftp.NewClient(c)
	w, _ := sc.Create("/public/via-sftp.txt")
	io.WriteString(w, "x")
	w.Close()
	sc.Mkdir("/public/newdir")
	sc.Close()
	c.Close()
	for _, name := range []string{"via-sftp.txt", "newdir"} {
		fi, err := os.Stat(filepath.Join(dirA, "public", name))
		if err != nil {
			t.Fatal(err)
		}
		if st := fi.Sys().(*syscall.Stat_t); int(st.Uid) != uidA || int(st.Gid) != wwwGID {
			t.Errorf("sftp %s owned by %d:%d", name, st.Uid, st.Gid)
		}
	}

	// Switching to static: PHP stops, and its sources are never shown.
	p.call(200, "PUT", "/api/v1/sites/"+idA+"/php", map[string]any{"version": ""})
	eventually(t, "php source hidden after switching to static", func() bool {
		code, body := get("alpha.localhost", "/index.php")
		return code == 404 && !strings.Contains(body, "<?php")
	})
	poolGlob, _ := filepath.Glob("/etc/php/*/fpm/pool.d/basalt-site-" + idA + ".conf")
	if len(poolGlob) != 0 {
		t.Fatalf("pool left after switching to static: %v", poolGlob)
	}

	// Back to PHP.
	p.call(200, "PUT", "/api/v1/sites/"+idA+"/php", map[string]any{"version": phpVersion})
	eventually(t, "PHP back", func() bool { code, _ := get("alpha.localhost", "/"); return code == 200 })

	// PHP options reach the pool; a PHP warning lands in the log the panel shows.
	p.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/sites/"+idA+"/php-settings", map[string]any{"timezone": "UTC\nphp_admin_value[open_basedir] = /"})
	p.call(200, "PUT", "/api/v1/sites/"+idA+"/php-settings", map[string]any{"memory_mb": 128, "timezone": "Europe/Berlin", "max_execution_time": 45})
	p.raw("PUT", "/api/v1/sites/"+idA+"/files/content"+q("/public/ini.php"), "",
		[]byte(`<?php echo ini_get("memory_limit"), "|", ini_get("date.timezone"), "|", ini_get("max_execution_time"); trigger_error("basalt-test-warning", E_USER_WARNING);`))
	eventually(t, "PHP options applied", func() bool {
		_, body := get("alpha.localhost", "/ini.php")
		return body == "128M|Europe/Berlin|45"
	})
	eventually(t, "warning in the PHP log", func() bool {
		return strings.Contains(p.call(200, "GET", "/api/v1/sites/"+idA+"/php-log", nil)["log"].(string), "basalt-test-warning")
	})

	// MariaDB: each site gets its own database and user.
	if p.server(sid)["status"] != "online" {
		t.Fatal("agent offline")
	}
	dbA := p.call(http.StatusCreated, "POST", "/api/v1/sites/"+idA+"/databases", map[string]any{"name": "app"})
	credA := dbA["credentials"].(map[string]any)
	dbB := p.call(http.StatusCreated, "POST", "/api/v1/sites/"+idB+"/databases", map[string]any{"name": "app"})
	credB := dbB["credentials"].(map[string]any)
	if credA["database"] != "s"+idA+"_app" || credA["password"] == "" {
		t.Fatalf("credentials: %v", credA)
	}
	p.call(http.StatusConflict, "POST", "/api/v1/sites/"+idA+"/databases", map[string]any{"name": "app"})
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/sites/"+idA+"/databases", map[string]any{"name": "x`; DROP DATABASE mysql; --"})

	dbProbe := func(c map[string]any, target string) string {
		return `<?php
header("Content-Type: application/json");
try {
  $pdo = new PDO("mysql:unix_socket=/run/mysqld/mysqld.sock;dbname=` + target + `", "` + c["username"].(string) + `", "` + c["password"].(string) + `");
  $pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
  $pdo->exec("CREATE TABLE IF NOT EXISTS t (v VARCHAR(20))");
  $pdo->exec("INSERT INTO t VALUES ('hello-db')");
  echo json_encode(["ok" => $pdo->query("SELECT v FROM t LIMIT 1")->fetchColumn()]);
} catch (Throwable $e) { echo json_encode(["error" => $e->getMessage()]); }`
	}
	p.raw("PUT", "/api/v1/sites/"+idA+"/files/content"+q("/public/db.php"), "", []byte(dbProbe(credA, credA["database"].(string))))
	p.raw("PUT", "/api/v1/sites/"+idA+"/files/content"+q("/public/steal.php"), "", []byte(dbProbe(credB, credA["database"].(string))))
	var dbRes map[string]any
	_, body := get("alpha.localhost", "/db.php")
	json.Unmarshal([]byte(body), &dbRes)
	if dbRes["ok"] != "hello-db" {
		t.Fatalf("PHP could not use its database: %s", body)
	}
	_, body = get("alpha.localhost", "/steal.php")
	if !strings.Contains(body, "Access denied") {
		t.Fatalf("site B's credentials reached site A's database: %s", body)
	}
	t.Logf("database isolation: %s", body)

	// A password reset invalidates the old password.
	reset := p.call(200, "POST", "/api/v1/sites/"+idA+"/databases/"+jsonID(dbA["database"].(map[string]any)["id"])+"/password", nil)
	if reset["credentials"].(map[string]any)["password"] == credA["password"] {
		t.Fatal("password unchanged")
	}
	_, body = get("alpha.localhost", "/db.php")
	if !strings.Contains(body, "Access denied") {
		t.Fatalf("old database password still works: %s", body)
	}

	// Backups: files and a consistent dump of the database, restored after
	// both were damaged.
	eventually(t, "restic reported", func() bool { return p.server(sid)["restic"] != "" })
	os.RemoveAll("/var/backups/basalt-it")
	st := p.call(http.StatusCreated, "POST", "/api/v1/backup-storages", map[string]any{
		"name": "local", "kind": "local", "path": "/var/backups/basalt-it"})["storage"].(map[string]any)
	p.call(200, "PUT", "/api/v1/sites/"+idA+"/backup-policy", map[string]any{
		"storage_id": st["id"], "schedule": "manual", "keep_last": 3})
	bk := p.call(http.StatusAccepted, "POST", "/api/v1/sites/"+idA+"/backups", map[string]any{})["backup"].(map[string]any)
	done := waitBackup(t, p, idA, jsonID(bk["id"]))
	if done["status"] != "succeeded" || len(done["snapshots"].([]any)) != 2 {
		t.Fatalf("backup: %v\n%v", done, done["log"])
	}
	t.Logf("backup log:\n%s", done["log"])
	// The integrity check with the distribution's restic.
	ck := p.call(http.StatusAccepted, "POST", "/api/v1/backup-checks", map[string]any{"storage_id": st["id"], "server_id": mustAtoi(sid)})["check"].(map[string]any)
	var checked map[string]any
	eventually(t, "integrity check", func() bool {
		checked = p.call(200, "GET", "/api/v1/backup-checks/"+jsonID(ck["id"]), nil)["check"].(map[string]any)
		return checked["status"] != "running"
	})
	if checked["status"] != "succeeded" || !strings.Contains(checked["log"].(string), "no errors were found") {
		t.Fatalf("check: %v\n%v", checked, checked["log"])
	}
	mysql := func(sql string) string {
		out, err := exec.Command("mariadb", "-N", "-e", sql).CombinedOutput()
		if err != nil {
			t.Fatalf("mariadb: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	mysql("DELETE FROM " + credA["database"].(string) + ".t")
	p.raw("PUT", "/api/v1/sites/"+idA+"/files/content"+q("/public/db.php"), "", []byte("damaged"))
	var ids []string
	for _, sn := range done["snapshots"].([]any) {
		ids = append(ids, sn.(map[string]any)["id"].(string))
	}
	rs := p.call(http.StatusAccepted, "POST", "/api/v1/sites/"+idA+"/restore", map[string]any{"snapshots": ids})["backup"].(map[string]any)
	if r := waitBackup(t, p, idA, jsonID(rs["id"])); r["status"] != "succeeded" {
		t.Fatalf("restore: %v\n%v", r, r["log"])
	}
	if v := mysql("SELECT v FROM " + credA["database"].(string) + ".t"); v != "hello-db" {
		t.Fatalf("database after restore: %q", v)
	}
	// Disk usage counts the site's files and its database.
	if u := p.call(200, "POST", "/api/v1/sites/"+idA+"/usage", map[string]any{})["site"].(map[string]any)["usage"].(map[string]any); u["db_bytes"].(float64) <= 0 || u["disk_bytes"].(float64) <= 0 {
		t.Fatalf("usage = %v", u)
	}
	restored := filepath.Join(sitesRoot, idA, "public", "db.php")
	if b, _ := os.ReadFile(restored); !strings.Contains(string(b), "PDO") {
		t.Fatalf("file after restore: %q", b)
	}
	if fi, err := os.Stat(restored); err != nil || strconv.Itoa(int(fi.Sys().(*syscall.Stat_t).Uid)) != mustUID(t, "basalt-s"+idA) {
		t.Fatal("restored file is not owned by the site user")
	}
	if fi, _ := os.Stat("/var/backups/basalt-it"); fi == nil || fi.Mode().Perm() != 0o700 {
		t.Fatal("backup directory is not private")
	}
	// A copy of a folder of the backup, restored next to the site's files,
	// belongs to the site's user; a file downloads as it was.
	var filesSnap string
	for _, sn := range done["snapshots"].([]any) {
		if sn := sn.(map[string]any); sn["item"].(map[string]any)["kind"] == "site_files" {
			filesSnap = sn["id"].(string)
		}
	}
	snapBase := "/api/v1/sites/" + idA + "/snapshots/" + filesSnap
	cp := p.call(http.StatusCreated, "POST", snapBase+"/restore-copy", map[string]any{"path": "public"})
	copied := filepath.Join(sitesRoot, idA, cp["folder"].(string), "public", "db.php")
	for _, f := range []string{copied, filepath.Dir(copied)} {
		if fi, err := os.Stat(f); err != nil || strconv.Itoa(int(fi.Sys().(*syscall.Stat_t).Uid)) != mustUID(t, "basalt-s"+idA) {
			t.Fatalf("restored copy %s is not owned by the site user", f)
		}
	}
	if code, _, b := p.raw("GET", snapBase+"/download"+q("public/db.php"), "", nil); code != http.StatusOK || !strings.Contains(string(b), "PDO") {
		t.Fatalf("download from the backup: %d %q", code, b)
	}

	// WordPress in one click: a new database, the files, a wp-config.php
	// that connects as the site's database user.
	rw := p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(sid), "name": "wp", "domains": []string{"wp.localhost"}, "tls": "internal", "php_version": phpVersion,
	})
	idW := jsonID(rw["site"].(map[string]any)["id"])
	inst := p.call(http.StatusCreated, "POST", "/api/v1/sites/"+idW+"/install", map[string]any{"app": "wordpress"})
	if inst["database"].(map[string]any)["name"] != "s"+idW+"_wp" {
		t.Fatalf("install = %v", inst)
	}
	eventually(t, "WordPress connects to its database", func() bool {
		_, body := get("wp.localhost", "/")
		return body == "wp-ok"
	})
	if fi, err := os.Stat(filepath.Join(sitesRoot, idW, "public", "wp-config.php")); err != nil || fi.Mode().Perm() != 0o600 ||
		strconv.Itoa(int(fi.Sys().(*syscall.Stat_t).Uid)) != mustUID(t, "basalt-s"+idW) {
		t.Fatalf("wp-config.php: %v %v", fi, err)
	}
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/sites/"+idW+"/install", map[string]any{"app": "wordpress"})
	if dbs := p.call(200, "GET", "/api/v1/sites/"+idW+"/databases", nil)["databases"].([]any); len(dbs) != 1 {
		t.Fatalf("a failed install left a database: %v", dbs)
	}

	// Sites with databases cannot be deleted by accident.
	p.call(http.StatusConflict, "DELETE", "/api/v1/sites/"+idA, nil)
	p.call(http.StatusNoContent, "DELETE", "/api/v1/sites/"+idA+"/databases/"+jsonID(dbA["database"].(map[string]any)["id"]), nil)

	// Delete: user and pool go away.
	p.call(200, "DELETE", "/api/v1/sites/"+idA, nil)
	if _, err := user.Lookup("basalt-s" + idA); err == nil {
		t.Error("site user not removed")
	}
	if g, _ := filepath.Glob("/etc/php/*/fpm/pool.d/basalt-site-" + idA + ".conf"); len(g) != 0 {
		t.Errorf("pool not removed: %v", g)
	}

	// Back from the trash as a new site: every file goes to its user, the
	// www group stays on the folders that had it.
	trash := "/api/v1/servers/" + sid + "/trash"
	var entry string
	for _, e := range p.call(200, "GET", trash, nil)["entries"].([]any) {
		if e := e.(map[string]any); jsonID(e["site_id"]) == idA {
			entry = e["name"].(string)
		}
	}
	back := p.call(http.StatusCreated, "POST", trash+"/"+entry+"/restore", map[string]any{"name": "again", "domains": []string{"again.localhost"}, "tls": "internal"})
	idBack := jsonID(back["site"].(map[string]any)["id"])
	uidBack := mustUID(t, "basalt-s"+idBack)
	wwwGroup, _ := user.LookupGroup("basalt-www")
	n := 0
	filepath.WalkDir(filepath.Join(sitesRoot, idBack), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		fi, _ := d.Info()
		st := fi.Sys().(*syscall.Stat_t)
		if strconv.Itoa(int(st.Uid)) != uidBack {
			t.Errorf("%s belongs to uid %d, not basalt-s%s", p, st.Uid, idBack)
		}
		n++
		return nil
	})
	if pub, _ := os.Stat(filepath.Join(sitesRoot, idBack, "public")); strconv.Itoa(int(pub.Sys().(*syscall.Stat_t).Gid)) != wwwGroup.Gid {
		t.Error("public lost the www group")
	}
	if n < 4 {
		t.Fatalf("only %d entries restored", n)
	}
	if _, err := os.Stat(filepath.Join(sitesRoot, ".trash", entry)); !os.IsNotExist(err) {
		t.Error("still in the trash")
	}
}

func mustUID(t *testing.T, name string) string {
	t.Helper()
	u, err := user.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return u.Uid
}
