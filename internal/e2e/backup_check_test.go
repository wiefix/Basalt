package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/server"
)

// TestBackupChecks checks the integrity of backup repositories: a sound
// one, a damaged one (notified like a failed backup), a missing one and the
// panel's own. Needs restic.
func TestBackupChecks(t *testing.T) {
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	got := make(chan map[string]any, 16)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		got <- m
	}))
	t.Cleanup(receiver.Close)
	p := startPanelWith(t, server.Options{Restic: bin, NotifyRetries: []time.Duration{0}})
	h := startHost(t, p)
	eventually(t, "restic version reported", func() bool { return p.server(h.id)["restic"] != "" })
	serverName := p.server(h.id)["name"].(string)
	p.call(http.StatusCreated, "POST", "/api/v1/notifications", map[string]any{"name": "ops", "kind": "webhook",
		"url": receiver.URL, "allow_private": true, "events": []string{"backup.failed"}})

	dir := filepath.Join(t.TempDir(), "backups")
	st := p.call(http.StatusCreated, "POST", "/api/v1/backup-storages", map[string]any{
		"name": "local", "kind": "local", "path": dir})["storage"].(map[string]any)
	stid := st["id"].(float64)
	sid := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(h.id), "name": "blog", "domains": []string{"blog.localhost"}, "tls": "internal",
	})["site"].(map[string]any)["id"])
	base := "/api/v1/sites/" + sid
	if code, _, b := p.raw("PUT", base+"/files/content?path=%2Fpublic%2Findex.html", "", []byte(strings.Repeat("hello ", 500))); code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, b)
	}

	// Nothing to check before a backup was made.
	if list := p.call(200, "GET", "/api/v1/backup-checks", nil)["repositories"].([]any); len(list) != 0 {
		t.Fatalf("repositories before a backup: %v", list)
	}
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/backup-checks", map[string]any{"storage_id": stid, "server_id": mustAtoi(h.id)})
	p.call(200, "PUT", base+"/backup-policy", map[string]any{"storage_id": stid, "schedule": "manual", "keep_last": 3})
	b := p.call(http.StatusAccepted, "POST", base+"/backups", map[string]any{})["backup"].(map[string]any)
	if done := waitBackup(t, p, sid, jsonID(b["id"])); done["status"] != "succeeded" {
		t.Fatalf("backup: %v", done)
	}

	list := p.call(200, "GET", "/api/v1/backup-checks", nil)["repositories"].([]any)
	if len(list) != 1 {
		t.Fatalf("repositories: %v", list)
	}
	repo := list[0].(map[string]any)
	if repo["storage_id"] != stid || repo["server"] != serverName || repo["last"] != nil || repo["next_at"] == nil {
		t.Fatalf("repository: %v", repo)
	}
	for _, bad := range []map[string]any{
		{"storage_id": stid, "server_id": 999},
		{"storage_id": stid, "server_id": 0}, // the panel has no backups there
		{"storage_id": 999, "server_id": mustAtoi(h.id)},
	} {
		p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/backup-checks", bad)
	}
	check := func(serverID int64) map[string]any {
		t.Helper()
		c := p.call(http.StatusAccepted, "POST", "/api/v1/backup-checks", map[string]any{"storage_id": stid, "server_id": serverID})["check"].(map[string]any)
		var done map[string]any
		eventually(t, "check", func() bool {
			done = p.call(200, "GET", "/api/v1/backup-checks/"+jsonID(c["id"]), nil)["check"].(map[string]any)
			return done["status"] != "running"
		})
		return done
	}
	ok := check(mustAtoi(h.id))
	if ok["status"] != "succeeded" || !strings.Contains(ok["log"].(string), "no errors were found") ||
		!regexp.MustCompile(`^[1-9][0-9]?/52$`).MatchString(ok["read_data"].(string)) || ok["trigger"] != "manual" {
		t.Fatalf("check: %v", ok)
	}
	t.Logf("check log:\n%s", ok["log"])
	repo = p.call(200, "GET", "/api/v1/backup-checks", nil)["repositories"].([]any)[0].(map[string]any)
	if last := repo["last"].(map[string]any); last["status"] != "succeeded" || last["log"] != nil {
		t.Fatalf("latest check: %v", repo)
	}
	select {
	case m := <-got:
		t.Fatalf("notified of a sound repository: %v", m)
	default:
	}

	// Damaged data: the check fails and notifies.
	packs, _ := filepath.Glob(filepath.Join(dir, "server-"+h.id, "data", "*", "*"))
	for _, pack := range packs {
		os.Chmod(pack, 0o644)
		f, err := os.OpenFile(pack, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteAt([]byte("XXXX"), 40)
		f.Close()
	}
	// The trees are damaged too: the structure check reads them.
	damaged := check(mustAtoi(h.id))
	if damaged["status"] != "failed" || !strings.Contains(damaged["error"].(string), "repository contains errors") {
		t.Fatalf("damaged: %v\n%v", damaged, damaged["log"])
	}
	select {
	case m := <-got:
		if m["event"] != "backup.failed" || m["title"] != "Integrity check of the backups of "+serverName+" failed" ||
			!strings.HasPrefix(m["text"].(string), "Storage local: ") {
			t.Fatalf("message %v", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notification")
	}

	// A repository that disappeared is a failure too.
	os.RemoveAll(filepath.Join(dir, "server-"+h.id))
	if gone := check(mustAtoi(h.id)); gone["status"] != "failed" || !strings.Contains(gone["error"].(string), "repository is missing or incomplete") {
		t.Fatalf("missing: %v", gone)
	}
	<-got

	// The panel's own repository is checked by the panel.
	p.call(200, "PUT", "/api/v1/panel-backup/policy", map[string]any{"storage_id": stid, "schedule": "manual", "keep_last": 3})
	p.call(200, "PUT", "/api/v1/panel-backup/recovery", map[string]any{"password": "owner password 123", "passphrase": "violet tractor sings at dawn"})
	pb := p.call(http.StatusAccepted, "POST", "/api/v1/panel-backup/run", map[string]any{})["backup"].(map[string]any)
	eventually(t, "panel backup", func() bool {
		return p.call(200, "GET", "/api/v1/panel-backup/runs/"+jsonID(pb["id"]), nil)["backup"].(map[string]any)["status"] == "succeeded"
	})
	if list := p.call(200, "GET", "/api/v1/backup-checks", nil)["repositories"].([]any); len(list) != 2 ||
		list[0].(map[string]any)["server_id"] != float64(0) {
		t.Fatalf("repositories: %v", list)
	}
	if panel := check(0); panel["status"] != "succeeded" || panel["server_id"] != nil {
		t.Fatalf("panel check: %v\n%v", panel, panel["log"])
	}

	// Admins only.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": "m@example.com", "name": "M", "password": "member password 1", "role": "member"})
	m := *p
	m.hc = &http.Client{Jar: newJar()}
	m.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "m@example.com", "password": "member password 1"})
	m.call(http.StatusForbidden, "GET", "/api/v1/backup-checks", nil)
	m.call(http.StatusForbidden, "POST", "/api/v1/backup-checks", map[string]any{"storage_id": stid, "server_id": 0})
	m.call(http.StatusForbidden, "GET", "/api/v1/backup-checks/"+jsonID(ok["id"]), nil)
	p.call(http.StatusNotFound, "GET", "/api/v1/backup-checks/999", nil)

	var n int
	p.db.R.QueryRow(`SELECT count(*) FROM audit_log WHERE action IN ('backup.check_started', 'backup.check_failed', 'backup.check_succeeded')`).Scan(&n)
	if n != 8 { // four checks started and finished
		t.Fatalf("audit entries: %d", n)
	}
}
