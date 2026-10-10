package e2e

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
