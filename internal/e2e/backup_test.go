package e2e

import (
	"archive/zip"
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/secrets"
	"github.com/wiefix/basalt/internal/server"
)

// waitBackup waits for a backup or restore run to end and returns it with
// its log.
func waitBackup(t *testing.T, p *panel, sid, bid string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		b := p.call(200, "GET", "/api/v1/sites/"+sid+"/backups/"+bid, nil)["backup"].(map[string]any)
		if b["status"] != "running" {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s still running", bid)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestSiteBackups backs a site up to a local storage with restic and
// restores it. Needs restic.
func TestSiteBackups(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic is not installed")
	}
	p := startPanel(t)
	h := startHost(t, p)
	eventually(t, "restic version reported", func() bool { return p.server(h.id)["restic"] != "" })
	r := p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(h.id), "name": "blog", "domains": []string{"blog.localhost"}, "tls": "internal",
	})
	sid := jsonID(r["site"].(map[string]any)["id"])
	base := "/api/v1/sites/" + sid
	put := func(path, content string) {
		t.Helper()
		if code, _, b := p.raw("PUT", base+"/files/content?path="+url.QueryEscape(path), "", []byte(content)); code != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", path, code, b)
		}
	}
	put("/public/index.html", "version one")
	os.WriteFile(filepath.Join(h.sitesRoot, sid, "tmp", "sess_x"), []byte("temp"), 0o600)

	// Storages: validation, a local directory outside the sites.
	for _, bad := range []map[string]any{
		{"name": "x", "kind": "local", "path": "relative/dir"},
		{"name": "x", "kind": "local", "path": "/var/../etc"},
		{"name": "x", "kind": "s3", "endpoint": "ftp://s3", "bucket": "b", "access_key": "a", "secret_key": "s"},
		{"name": "x", "kind": "s3", "endpoint": "https://s3.example.com", "bucket": "Bad_Bucket", "access_key": "a", "secret_key": "s"},
		{"name": "x", "kind": "sftp", "host": "-oProxyCommand=x", "user": "u", "path": "b"},
		{"name": "x", "kind": "nfs", "path": "/x"},
	} {
		p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/backup-storages", bad)
	}
	sftp := p.call(http.StatusCreated, "POST", "/api/v1/backup-storages", map[string]any{
		"name": "box", "kind": "sftp", "host": "backup.example.com", "user": "u1", "path": "basalt"})["storage"].(map[string]any)
	if !strings.HasPrefix(sftp["ssh_public_key"].(string), "ssh-ed25519 ") || sftp["port"] != float64(22) {
		t.Fatalf("sftp storage = %v", sftp)
	}
	// Inside the sites directory: site users would see every backup.
	inside := p.call(http.StatusCreated, "POST", "/api/v1/backup-storages", map[string]any{
		"name": "inside", "kind": "local", "path": h.sitesRoot + "/backups"})["storage"].(map[string]any)
	if c := p.call(200, "POST", "/api/v1/backup-storages/"+jsonID(inside["id"])+"/check", map[string]any{"server_id": mustAtoi(h.id)}); c["ok"] != false {
		t.Fatalf("check inside sites = %v", c)
	}
	dir := filepath.Join(t.TempDir(), "backups")
	st := p.call(http.StatusCreated, "POST", "/api/v1/backup-storages", map[string]any{
		"name": "local", "kind": "local", "path": dir})["storage"].(map[string]any)
	stid := jsonID(st["id"])
	if c := p.call(200, "POST", "/api/v1/backup-storages/"+stid+"/check", map[string]any{"server_id": mustAtoi(h.id)}); c["ok"] != true {
		t.Fatalf("check = %v", c)
	}
	if _, err := os.Stat(filepath.Join(dir, "server-"+h.id, "config")); err != nil {
		t.Fatalf("repository not created: %v", err)
	}
	pw := p.call(200, "GET", "/api/v1/backup-storages/"+stid+"/password", nil)["password"].(string)
	if len(pw) < 40 {
		t.Fatalf("password %q", pw)
	}
	var sealed []byte
	p.db.R.QueryRow(`SELECT password FROM backup_storages WHERE id = ?`, mustAtoi(stid)).Scan(&sealed)
	if strings.Contains(string(sealed), pw) {
		t.Fatal("repository password stored in clear")
	}

	// A policy, then a backup now.
	p.call(http.StatusUnprocessableEntity, "POST", base+"/backups", map[string]any{})
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/backup-policy", map[string]any{"storage_id": mustAtoi(stid), "schedule": "weekly", "keep_last": 3})
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/backup-policy", map[string]any{"storage_id": mustAtoi(stid), "schedule": "daily", "keep_last": 0})
	pol := p.call(200, "PUT", base+"/backup-policy", map[string]any{"storage_id": mustAtoi(stid), "schedule": "daily",
		"at_minute": 120, "keep_last": 2, "keep_daily": 7})["policy"].(map[string]any)
	if pol["next_run"] == nil {
		t.Fatalf("policy = %v", pol)
	}
	p.call(http.StatusConflict, "DELETE", "/api/v1/backup-storages/"+stid, nil)

	run := func() map[string]any {
		t.Helper()
		b := p.call(http.StatusAccepted, "POST", base+"/backups", map[string]any{})["backup"].(map[string]any)
		done := waitBackup(t, p, sid, jsonID(b["id"]))
		if done["status"] != "succeeded" {
			t.Fatalf("backup: %v\n%v", done, done["log"])
		}
		return done
	}
	b1 := run()
	if snaps := b1["snapshots"].([]any); len(snaps) != 1 || !strings.Contains(fmt.Sprint(b1["log"]), "Backup finished") {
		t.Fatalf("backup = %v", b1)
	}
	t.Logf("backup log:\n%s", b1["log"])

	put("/public/index.html", "version two")
	put("/public/new.html", "added later")
	run()
	run()

	// keep_last 2 and keep_daily 7: restic also keeps the oldest snapshot
	// while there is less history than the policy covers.
	snaps := p.call(200, "GET", base+"/snapshots", nil)["snapshots"].([]any)
	if len(snaps) < 2 || len(snaps) > 3 {
		t.Fatalf("snapshots: %v", snaps)
	}
	var first string
	for _, s := range snaps {
		first = s.(map[string]any)["id"].(string) // oldest kept is last
	}
	if first == "" {
		t.Fatal("no snapshots")
	}
	if list := p.call(200, "GET", base+"/backups", nil); len(list["backups"].([]any)) != 3 || list["restic"] == "" {
		t.Fatalf("backups = %v", list)
	}

	// Restore the first backup's snapshot if it is still kept, else the
	// oldest one; files written after it disappear.
	put("/public/index.html", "broken")
	snapID := b1["snapshots"].([]any)[0].(map[string]any)["id"].(string)
	found := false
	for _, s := range snaps {
		if s.(map[string]any)["id"] == snapID {
			found = true
		}
	}
	want := "version one"
	if !found {
		snapID, want = first, "version two"
	}
	p.call(http.StatusUnprocessableEntity, "POST", base+"/restore", map[string]any{"snapshots": []string{"deadbeefdeadbeef"}})
	rs := p.call(http.StatusAccepted, "POST", base+"/restore", map[string]any{"snapshots": []string{snapID}})["backup"].(map[string]any)
	if done := waitBackup(t, p, sid, jsonID(rs["id"])); done["status"] != "succeeded" || done["kind"] != "restore" {
		t.Fatalf("restore: %v\n%v", done, done["log"])
	}
	got := p.call(200, "GET", base+"/files/text?path="+url.QueryEscape("/public/index.html"), nil)["content"]
	if got != want {
		t.Fatalf("restored content %q, want %q", got, want)
	}
	if want == "version one" {
		if _, err := os.Stat(filepath.Join(h.sitesRoot, sid, "public", "new.html")); !os.IsNotExist(err) {
			t.Fatal("file added after the snapshot survived the restore")
		}
	}
	// tmp/ survives as a directory; its contents were not backed up.
	if fi, err := os.Stat(filepath.Join(h.sitesRoot, sid, "tmp")); err != nil || !fi.IsDir() {
		t.Fatal("tmp directory removed by the restore")
	}

	// Roles: viewers see runs, not logs; members run backups; admins restore.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": "m@example.com", "name": "M", "password": "member password 1", "role": "member"})
	m := *p
	m.hc = &http.Client{Jar: newJar()}
	m.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "m@example.com", "password": "member password 1"})
	m.call(200, "GET", base+"/backups", nil)
	m.call(http.StatusForbidden, "POST", base+"/restore", map[string]any{"snapshots": []string{snapID}})
	m.call(http.StatusForbidden, "GET", "/api/v1/backup-storages/"+stid+"/password", nil)
	m.call(http.StatusForbidden, "PUT", base+"/backup-policy", map[string]any{"storage_id": mustAtoi(stid), "schedule": "manual", "keep_last": 1})

	// Without a policy the storage can go; its backups stay where they are.
	p.call(200, "DELETE", base+"/backup-policy", nil)
	p.call(200, "DELETE", "/api/v1/backup-storages/"+stid, nil)
	if _, err := os.Stat(filepath.Join(dir, "server-"+h.id, "config")); err != nil {
		t.Fatal("deleting the storage removed the repository")
	}
}

// waitAppBackup is waitBackup for an app's runs.
func waitAppBackup(t *testing.T, p *panel, aid, bid string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		b := p.call(200, "GET", "/api/v1/apps/"+aid+"/backups/"+bid, nil)["backup"].(map[string]any)
		if b["status"] != "running" {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s still running", bid)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// appBackupRound backs an app up, lets damage happen, restores every part
// of that backup and returns the restore's log.
func appBackupRound(t *testing.T, p *panel, aid string, damage func()) string {
	t.Helper()
	base := "/api/v1/apps/" + aid
	b := p.call(http.StatusAccepted, "POST", base+"/backups", map[string]any{})["backup"].(map[string]any)
	done := waitAppBackup(t, p, aid, jsonID(b["id"]))
	if done["status"] != "succeeded" {
		t.Fatalf("backup: %v\n%v", done, done["log"])
	}
	t.Logf("backup log:\n%s", done["log"])
	damage()
	var ids []string
	for _, sn := range done["snapshots"].([]any) {
		ids = append(ids, sn.(map[string]any)["id"].(string))
	}
	rs := p.call(http.StatusAccepted, "POST", base+"/restore", map[string]any{"snapshots": ids})["backup"].(map[string]any)
	r := waitAppBackup(t, p, aid, jsonID(rs["id"]))
	if r["status"] != "succeeded" {
		t.Fatalf("restore: %v\n%v", r, r["log"])
	}
	return r["log"].(string)
}

// TestAppBackupsWithDocker backs up a volume and a PostgreSQL database and
// restores them. Needs Docker and restic; opt-in.
func TestAppBackupsWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic is not installed")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	p := startPanel(t)
	h := startHostWith(t, p, sock)
	eventually(t, "restic reported", func() bool { return p.server(h.id)["restic"] != "" })
	sid := mustAtoi(h.id)
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", append([]string{"--host", "unix://" + sock}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	st := p.call(http.StatusCreated, "POST", "/api/v1/backup-storages", map[string]any{
		"name": "local", "kind": "local", "path": filepath.Join(t.TempDir(), "backups")})["storage"].(map[string]any)
	policy := map[string]any{"storage_id": st["id"], "schedule": "manual", "keep_last": 3}
	deploy := func(aid string) {
		t.Helper()
		dep := p.call(http.StatusAccepted, "POST", "/api/v1/apps/"+aid+"/deploy", map[string]any{})["deployment"].(map[string]any)
		if d, log := followDeployment(t, p, "/api/v1/apps/"+aid, jsonID(dep["id"])); d["status"] != "succeeded" {
			t.Fatalf("deployment: %v\n%s", d, log)
		}
	}
	cleanup := func(aid string) {
		t.Cleanup(func() { p.call(200, "DELETE", "/api/v1/apps/"+aid+"?volumes=1", nil) })
	}

	// An app without volumes has nothing to back up.
	plain := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": sid, "name": "plain",
		"image": "traefik/whoami:v1.10", "port": 80})["app"].(map[string]any)["id"])
	cleanup(plain)
	p.call(200, "PUT", "/api/v1/apps/"+plain+"/backup-policy", policy)
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/apps/"+plain+"/backups", map[string]any{})

	// A volume: nginx serves its content.
	web := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": sid, "name": "site",
		"image": "nginx:1.27-alpine", "port": 80, "domains": []string{"vol.localhost"}, "tls": "internal",
		"volumes": []map[string]string{{"name": "html", "path": "/usr/share/nginx/html"}}})["app"].(map[string]any)["id"])
	cleanup(web)
	deploy(web)
	vol := appNet(web) + "-html"
	inVolume := func(script string) string {
		return docker("run", "--rm", "-v", vol+":/d", "busybox:1.37", "sh", "-c", script)
	}
	inVolume("rm -f /d/*; echo version-one > /d/index.html; chown 1234:1234 /d/index.html")
	eventually(t, "volume served", func() bool { _, b := h.get("vol.localhost", "/"); return strings.Contains(b, "version-one") })
	p.call(200, "PUT", "/api/v1/apps/"+web+"/backup-policy", policy)
	log := appBackupRound(t, p, web, func() {
		inVolume("echo damaged > /d/index.html; echo extra > /d/extra.html")
	})
	if !strings.Contains(log, "Stopping") || !strings.Contains(log, "Started 1 container(s) again") {
		t.Fatalf("restore log:\n%s", log)
	}
	eventually(t, "restored volume served", func() bool { _, b := h.get("vol.localhost", "/"); return strings.Contains(b, "version-one") })
	if got := inVolume("ls /d; stat -c %u /d/index.html"); got != "index.html\n1234" {
		t.Fatalf("volume after restore: %q", got)
	}

	// Files of the volume's backup: browse, download, restore a copy.
	var volSnap string
	for _, sn := range p.call(200, "GET", "/api/v1/apps/"+web+"/snapshots", nil)["snapshots"].([]any) {
		if it := sn.(map[string]any)["item"].(map[string]any); it["kind"] == "volume" && it["volume"] == "html" {
			volSnap = sn.(map[string]any)["id"].(string)
			break
		}
	}
	snapBase := "/api/v1/apps/" + web + "/snapshots/" + volSnap
	files := p.call(200, "GET", snapBase+"/files?volume=html&path=", nil)["entries"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["name"] != "index.html" || files[0].(map[string]any)["size"] != float64(12) {
		t.Fatalf("volume files = %v", files)
	}
	p.call(http.StatusUnprocessableEntity, "GET", snapBase+"/files?path=", nil)              // no volume named
	p.call(http.StatusUnprocessableEntity, "GET", snapBase+"/files?volume=other&path=", nil) // not this snapshot's
	p.call(http.StatusNotFound, "GET", snapBase+"/files?volume=html&path=nope", nil)
	if code, hdr, body := p.raw("GET", snapBase+"/download?volume=html&path=index.html", "", nil); code != 200 || string(body) != "version-one\n" ||
		!strings.Contains(hdr.Get("Content-Disposition"), "index.html") {
		t.Fatalf("download from the volume: %d %q", code, body)
	}
	if code, hdr, body := p.raw("GET", snapBase+"/download?volume=html&path=", "", nil); code != 200 ||
		!strings.Contains(hdr.Get("Content-Disposition"), "volume-html-") {
		t.Fatalf("download the volume: %d %v", code, hdr)
	} else if zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body))); err != nil || len(zr.File) != 1 || zr.File[0].Name != "index.html" {
		t.Fatalf("volume zip: %v", err)
	}
	p.call(http.StatusUnprocessableEntity, "POST", snapBase+"/restore-copy", map[string]any{"path": "index.html", "volume": "gone"})
	copied := p.call(http.StatusCreated, "POST", snapBase+"/restore-copy", map[string]any{"path": "index.html", "volume": "html"})
	folder := copied["folder"].(string)
	if !strings.HasPrefix(folder, "restored-") || copied["files"] != float64(1) {
		t.Fatalf("restore copy: %v", copied)
	}
	if got := inVolume("cat /d/" + folder + "/index.html; stat -c %u /d/" + folder + "/index.html; cat /d/index.html"); got != "version-one\n1234\nversion-one" {
		t.Fatalf("copy in the volume: %q", got)
	}

	// PostgreSQL: a logical dump, restored over changed data.
	pg := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": sid, "name": "pg",
		"source": "database", "engine": "postgres"})["app"].(map[string]any)["id"])
	cleanup(pg)
	deploy(pg)
	pgContainer := func() string {
		return docker("ps", "-q", "--filter", "label=basalt.namespace="+e2eNamespace, "--filter", "label=basalt.app="+pg)
	}
	psql := func(sql string) string {
		return docker("exec", pgContainer(), "psql", "-U", "pg", "-d", "pg", "-tAc", sql)
	}
	psql("CREATE TABLE t (v text); INSERT INTO t VALUES ('kept')")
	p.call(200, "PUT", "/api/v1/apps/"+pg+"/backup-policy", policy)
	if items := p.call(200, "GET", "/api/v1/apps/"+pg+"/backups", nil)["items"].([]any); len(items) != 1 ||
		items[0].(map[string]any)["kind"] != "db_dump" {
		t.Fatalf("items = %v", items)
	}
	appBackupRound(t, p, pg, func() {
		psql("UPDATE t SET v = 'damaged'; CREATE TABLE extra (x int)")
	})
	if v := psql("SELECT v FROM t"); v != "kept" {
		t.Fatalf("table after restore: %q", v)
	}
	if v := psql("SELECT count(*) FROM pg_tables WHERE tablename = 'extra'"); v != "1" {
		// --clean drops and recreates what the dump holds; other tables stay.
		t.Fatalf("unrelated table: %q", v)
	}
	snaps := p.call(200, "GET", "/api/v1/apps/"+pg+"/snapshots", nil)["snapshots"].([]any)
	if len(snaps) != 1 || snaps[0].(map[string]any)["item"].(map[string]any)["kind"] != "db_dump" {
		t.Fatalf("snapshots = %v", snaps)
	}
	// The dump downloads as it was made.
	dumpBase := "/api/v1/apps/" + pg + "/snapshots/" + snaps[0].(map[string]any)["id"].(string)
	if code, hdr, body := p.raw("GET", dumpBase+"/download?database=pg", "", nil); code != 200 || !strings.Contains(string(body), "kept") ||
		!strings.Contains(hdr.Get("Content-Disposition"), "pg-") {
		t.Fatalf("dump download: %d %v %.200s", code, hdr, body)
	}
	p.call(http.StatusUnprocessableEntity, "GET", dumpBase+"/files?database=pg", nil)
	p.call(http.StatusUnprocessableEntity, "GET", dumpBase+"/download?database=pg&path=x", nil)
}

// TestPanelBackup backs the panel up with restic, then reads the snapshot
// back with the restic command and the recovery passphrase. Needs restic.
func TestPanelBackup(t *testing.T) {
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	p := startPanelWith(t, server.Options{Restic: bin})
	got := p.call(200, "GET", "/api/v1/panel-backup", nil)
	if got["recovery"].(map[string]any)["set"] != false || got["restic"] == "" {
		t.Fatalf("initial state = %v", got)
	}
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/panel-backup/run", map[string]any{})
	dir := filepath.Join(t.TempDir(), "backups")
	st := p.call(http.StatusCreated, "POST", "/api/v1/backup-storages", map[string]any{
		"name": "local", "kind": "local", "path": dir})["storage"].(map[string]any)
	p.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/panel-backup/policy", map[string]any{"storage_id": st["id"], "schedule": "daily"})
	p.call(200, "PUT", "/api/v1/panel-backup/policy", map[string]any{"storage_id": st["id"], "schedule": "daily",
		"at_minute": 60, "keep_last": 3, "keep_daily": 7})
	p.call(http.StatusConflict, "DELETE", "/api/v1/backup-storages/"+jsonID(st["id"]), nil)
	// Without a recovery passphrase a backup would be useless.
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/panel-backup/run", map[string]any{})

	const phrase = "violet tractor sings at dawn"
	p.call(http.StatusForbidden, "PUT", "/api/v1/panel-backup/recovery", map[string]any{"password": "wrong", "passphrase": phrase})
	p.call(http.StatusUnprocessableEntity, "PUT", "/api/v1/panel-backup/recovery", map[string]any{"password": "owner password 123", "passphrase": "short"})
	// Admins manage backups, only the owner sets the passphrase.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": "a@example.com", "name": "A", "password": "admin password 1", "role": "admin"})
	a := *p
	a.hc = &http.Client{Jar: newJar()}
	a.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "a@example.com", "password": "admin password 1"})
	a.call(http.StatusForbidden, "PUT", "/api/v1/panel-backup/recovery", map[string]any{"password": "admin password 1", "passphrase": phrase})
	p.call(200, "PUT", "/api/v1/panel-backup/recovery", map[string]any{"password": "owner password 123", "passphrase": phrase})
	var stored string
	p.db.R.QueryRow(`SELECT value FROM settings WHERE key = 'recovery_key'`).Scan(&stored)
	if stored == "" || strings.Contains(stored, phrase) {
		t.Fatal("recovery key not stored, or the passphrase is")
	}

	b := a.call(http.StatusAccepted, "POST", "/api/v1/panel-backup/run", map[string]any{})["backup"].(map[string]any)
	var run map[string]any
	eventually(t, "panel backup", func() bool {
		run = p.call(200, "GET", "/api/v1/panel-backup/runs/"+jsonID(b["id"]), nil)["backup"].(map[string]any)
		return run["status"] != "running"
	})
	if run["status"] != "succeeded" || run["snapshot"] == "" {
		t.Fatalf("panel backup: %v\n%v", run, run["log"])
	}
	t.Logf("panel backup log:\n%s", run["log"])
	if _, err := os.Stat(filepath.Join(p.dataDir, "panel-backup")); !os.IsNotExist(err) {
		t.Fatal("staging directory left behind")
	}

	// Disaster recovery by hand: restic restore with the storage password,
	// then the key with the passphrase.
	pw := p.call(200, "GET", "/api/v1/backup-storages/"+jsonID(st["id"])+"/password", nil)["password"].(string)
	target := t.TempDir()
	cmd := exec.Command(bin, "-r", filepath.Join(dir, "panel"), "restore", "latest", "--target", target)
	cmd.Env = append(os.Environ(), "RESTIC_PASSWORD="+pw)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restic restore: %v %s", err, out)
	}
	var restored string
	filepath.WalkDir(target, func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Name() == "master-key.json" {
			restored = filepath.Dir(path)
		}
		return nil
	})
	wrapped, err := os.ReadFile(filepath.Join(restored, "master-key.json"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := secrets.UnwrapKey(wrapped, phrase)
	if err != nil {
		t.Fatal(err)
	}
	if orig, _ := os.ReadFile(filepath.Join(p.dataDir, "master.key")); !bytes.Equal(orig, key) {
		t.Fatal("recovered key differs")
	}
	for _, f := range []string{"basalt.db", "RESTORE.txt"} {
		if _, err := os.Stat(filepath.Join(restored, f)); err != nil {
			t.Fatalf("%s missing from the snapshot", f)
		}
	}
	if list := p.call(200, "GET", "/api/v1/panel-backup", nil)["backups"].([]any); len(list) != 1 {
		t.Fatalf("runs = %v", list)
	}
}
