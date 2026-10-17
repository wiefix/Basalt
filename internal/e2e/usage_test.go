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
