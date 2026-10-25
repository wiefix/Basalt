package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSiteUsage: a site's disk space is measured on request; going over
// its soft quota is notified once, until it is back under.
func TestSiteUsage(t *testing.T) {
	got := make(chan map[string]any, 8)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &m)
		got <- m
	}))
	t.Cleanup(receiver.Close)

	p := startPanel(t)
	h := startHost(t, p)
	p.call(http.StatusCreated, "POST", "/api/v1/notifications", map[string]any{"name": "hook", "kind": "webhook",
		"url": receiver.URL, "allow_private": true, "events": []string{"site.quota"}})
	r := p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(h.id), "name": "big", "domains": []string{"big.localhost"}, "tls": "internal",
	})
	site := r["site"].(map[string]any)
	sid := jsonID(site["id"])
	base := "/api/v1/sites/" + sid
	if u := site["usage"].(map[string]any); u["at"] != nil || u["disk_bytes"] != float64(0) {
		t.Fatalf("usage before measuring = %v", u)
	}
	dir := filepath.Join(h.sitesRoot, sid)
	os.WriteFile(filepath.Join(dir, "public", "data.bin"), make([]byte, 600<<10), 0o644)
	os.Symlink("/", filepath.Join(dir, "public", "root")) // not followed

	usage := func() map[string]any {
		t.Helper()
		return p.call(http.StatusOK, "POST", base+"/usage", map[string]any{})["site"].(map[string]any)
	}
	s := usage()
	u := s["usage"].(map[string]any)
	if b := u["disk_bytes"].(float64); b < 600<<10 || b > 10<<20 || u["disk_files"].(float64) < 3 || u["at"] == nil {
		t.Fatalf("usage = %v", u)
	}

	p.call(http.StatusUnprocessableEntity, "PUT", base+"/quota", map[string]any{"quota_mb": -1})
	if s := p.call(http.StatusOK, "PUT", base+"/quota", map[string]any{"quota_mb": 1})["site"].(map[string]any); s["over_quota"] != false || s["quota_mb"] != float64(1) {
		t.Fatalf("quota = %v", s)
	}
	os.WriteFile(filepath.Join(dir, "public", "more.bin"), make([]byte, 2<<20), 0o644)
	if s := usage(); s["over_quota"] != true {
		t.Fatalf("over quota = %v", s)
	}
	select {
	case m := <-got:
		if m["event"] != "site.quota" || !strings.Contains(m["title"].(string), "big is over its disk quota") {
			t.Fatalf("notification = %v", m)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no quota notification")
	}
	usage() // still over: no second notification
	os.Remove(filepath.Join(dir, "public", "more.bin"))
	if s := usage(); s["over_quota"] != false {
		t.Fatalf("back under = %v", s)
	}
	select {
	case m := <-got:
		t.Fatalf("unexpected notification %v", m)
	case <-time.After(300 * time.Millisecond):
	}

	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": "m@example.com", "name": "M", "password": "a long password 1", "role": "member"})
	m := *p
	m.hc = &http.Client{Jar: newJar()}
	m.call(http.StatusOK, "POST", "/api/v1/auth/login", map[string]string{"email": "m@example.com", "password": "a long password 1"})
	m.call(http.StatusOK, "POST", base+"/usage", map[string]any{})
	m.call(http.StatusForbidden, "PUT", base+"/quota", map[string]any{"quota_mb": 100})
}

// TestSiteTrash: a deleted site's files wait in the server's trash, where
// an admin sees and removes them.
func TestSiteTrash(t *testing.T) {
	p := startPanel(t)
	h := startHost(t, p)
	r := p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(h.id), "name": "gone", "domains": []string{"gone.localhost"}, "tls": "internal",
	})
	sid := jsonID(r["site"].(map[string]any)["id"])
	p.call(http.StatusOK, "DELETE", "/api/v1/sites/"+sid, nil)
	trash := "/api/v1/servers/" + h.id + "/trash"
	l := p.call(http.StatusOK, "GET", trash, nil)
	entries := l["entries"].([]any)
	if len(entries) != 1 || jsonID(entries[0].(map[string]any)["site_id"]) != sid || l["keep_days"] != float64(30) {
		t.Fatalf("trash = %v", l)
	}
	name := entries[0].(map[string]any)["name"].(string)
	p.call(http.StatusUnprocessableEntity, "DELETE", trash+"/..%2F1-1", nil)
	p.call(http.StatusNotFound, "DELETE", trash+"/99-1", nil)
	if l := p.call(http.StatusOK, "DELETE", trash+"/"+name, nil); len(l["entries"].([]any)) != 0 {
		t.Fatalf("after removing = %v", l)
	}
	if _, err := os.Stat(filepath.Join(h.sitesRoot, ".trash", name)); !os.IsNotExist(err) {
		t.Fatal("files still in the trash")
	}

	// Another deleted site comes back from the trash as a new site, with
	// its files.
	r = p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(h.id), "name": "back", "domains": []string{"back.localhost"}, "tls": "internal",
	})
	old := jsonID(r["site"].(map[string]any)["id"])
	if code, _, b := p.raw("PUT", "/api/v1/sites/"+old+"/files/content?path=%2Fpublic%2Fkept.txt", "", []byte("kept")); code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, b)
	}
	p.call(http.StatusOK, "DELETE", "/api/v1/sites/"+old, nil)
	name = p.call(http.StatusOK, "GET", trash, nil)["entries"].([]any)[0].(map[string]any)["name"].(string)
	restore := func(code int, entry, site string) map[string]any {
		return p.call(code, "POST", trash+"/"+entry+"/restore", map[string]any{"name": site, "domains": []string{site + ".localhost"}, "tls": "internal"})
	}
	restore(http.StatusUnprocessableEntity, "..%2Fx", "nope")
	restore(http.StatusUnprocessableEntity, "99-1", "nope") // no such folder: nothing is left behind
	if l := p.call(http.StatusOK, "GET", "/api/v1/sites", nil)["sites"].([]any); len(l) != 0 {
		t.Fatalf("a failed restore left a site: %v", l)
	}
	restore(http.StatusUnprocessableEntity, name, "bad name!")
	nid := jsonID(restore(http.StatusCreated, name, "back")["site"].(map[string]any)["id"])
	if code, _, b := p.raw("GET", "/api/v1/sites/"+nid+"/files/content?path=%2Fpublic%2Fkept.txt", "", nil); code != http.StatusOK || string(b) != "kept" {
		t.Fatalf("restored file: %d %q", code, b)
	}
	if l := p.call(http.StatusOK, "GET", trash, nil)["entries"].([]any); len(l) != 0 {
		t.Fatalf("still in the trash: %v", l)
	}
	var n int
	p.db.R.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'site.restored'`).Scan(&n)
	if n != 1 {
		t.Fatal("no audit entry")
	}
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": "m@example.com", "name": "M", "password": "a long password 1", "role": "member"})
	m := *p
	m.hc = &http.Client{Jar: newJar()}
	m.call(http.StatusOK, "POST", "/api/v1/auth/login", map[string]string{"email": "m@example.com", "password": "a long password 1"})
	m.call(http.StatusForbidden, "GET", trash, nil)
	m.call(http.StatusForbidden, "POST", trash+"/1-1/restore", map[string]any{"name": "x"})
}
