package e2e

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// TestSiteReplicas: a site served by a second server gets the primary's
// files there, then every change, deletions included, while the copy's own
// logs stay; what cannot be replicated is refused.
func TestSiteReplicas(t *testing.T) {
	p := startPanel(t)
	h1, h2 := startHost(t, p), startHost(t, p)
	for i, h := range []*host{h1, h2} {
		p.call(200, "PUT", "/api/v1/servers/"+h.id+"/public-ips", map[string]any{"ips": []string{"192.0.2." + strconv.Itoa(21+i)}})
	}
	p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]string{"name": "example.test"})
	site := func(name string, domains []string) string {
		return jsonID(p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{"server_id": mustAtoi(h1.id), "name": name,
			"domains": domains, "tls": "internal"})["site"].(map[string]any)["id"])
	}
	blog := site("blog", []string{"blog.example.test"})
	base := "/api/v1/sites/" + blog
	put := func(path string, content []byte) {
		t.Helper()
		if code, _, b := p.raw("PUT", base+"/files/content?path="+url.QueryEscape(path), "", content); code != http.StatusCreated {
			t.Fatalf("upload %s: %d %s", path, code, b)
		}
	}
	big := bytes.Repeat([]byte("basalt "), 400_000) // 2.8 MB: several chunks
	put("/public/index.html", []byte("one"))
	put("/public/keep.txt", []byte("keep"))
	p.call(200, "POST", base+"/files/mkdir", map[string]any{"path": "/public/assets"})
	put("/public/assets/app.js", big)
	os.WriteFile(filepath.Join(h1.sitesRoot, blog, "logs", "primary.log"), []byte("x"), 0o644)

	replicas := base + "/replicas"
	h2id := mustAtoi(h2.id)
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/sites/"+site("outside", []string{"x.example.org"})+"/replicas", map[string]any{"server_id": h2id})
	p.call(http.StatusUnprocessableEntity, "POST", replicas, map[string]any{"server_id": 999})
	p.call(http.StatusConflict, "POST", replicas, map[string]any{"server_id": mustAtoi(h1.id)})
	p.call(http.StatusConflict, "POST", base+"/sync", nil) // nothing to copy yet

	start := time.Now() // the first copy starts at once
	r := p.call(http.StatusCreated, "POST", replicas, map[string]any{"server_id": h2id})
	if jsonString(r["site"].(map[string]any)["replicas"]) != "["+h2.id+"]" || len(r["placements"].([]any)) != 2 {
		t.Fatalf("add replica: %v", r)
	}
	p.call(http.StatusConflict, "POST", replicas, map[string]any{"server_id": h2id})
	// A database would live on one server.
	p.call(http.StatusUnprocessableEntity, "POST", base+"/databases", map[string]any{"name": "wp"})

	copyOf := func(path string) []byte {
		b, _ := os.ReadFile(filepath.Join(h2.sitesRoot, blog, filepath.FromSlash(path)))
		return b
	}
	synced := func(after time.Time) func() bool {
		return func() bool {
			pl := p.call(200, "GET", base, nil)["placements"].([]any)[1].(map[string]any)
			v, _ := pl["synced_at"].(string)
			at, _ := time.Parse(time.RFC3339, v)
			return pl["sync_error"] == nil && !at.Before(after.Truncate(time.Second)) && pl["syncing"] == nil
		}
	}
	eventually(t, "the first copy", synced(start))
	if string(copyOf("public/index.html")) != "one" || !bytes.Equal(copyOf("public/assets/app.js"), big) || string(copyOf("public/keep.txt")) != "keep" {
		t.Fatal("files not copied")
	}
	if copyOf("logs/primary.log") != nil {
		t.Fatal("the primary's logs were copied")
	}
	fi1, _ := os.Stat(filepath.Join(h1.sitesRoot, blog, "public", "assets", "app.js"))
	fi2, _ := os.Stat(filepath.Join(h2.sitesRoot, blog, "public", "assets", "app.js"))
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatalf("mtime %v, copy %v", fi1.ModTime(), fi2.ModTime())
	}

	// Changes on the primary arrive; what the copy has alone goes; the
	// copy's logs stay.
	put("/public/index.html", []byte("two"))
	p.call(200, "POST", base+"/files/delete", map[string]any{"paths": []string{"/public/keep.txt", "/public/assets"}})
	os.WriteFile(filepath.Join(h2.sitesRoot, blog, "public", "local.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(h2.sitesRoot, blog, "logs", "replica.log"), []byte("x"), 0o644)
	time.Sleep(1100 * time.Millisecond) // a new synced_at second
	start = time.Now()
	p.call(http.StatusAccepted, "POST", base+"/sync", nil)
	eventually(t, "the second copy", synced(start))
	if string(copyOf("public/index.html")) != "two" || copyOf("public/keep.txt") != nil || copyOf("public/local.txt") != nil {
		t.Fatal("changes not copied")
	}
	if _, err := os.Stat(filepath.Join(h2.sitesRoot, blog, "public", "assets")); err == nil {
		t.Fatal("deleted folder still there")
	}
	if copyOf("logs/replica.log") == nil {
		t.Fatal("the copy's own logs went")
	}

	// A change on the primary is copied at once: its agent watches the
	// files (inotify, Linux) and tells the panel; elsewhere the test tells
	// it. The panel's round every minute does not run here.
	h1id := mustAtoi(h1.id)
	blogID := mustAtoi(blog)
	put("/public/index.html", []byte("three"))
	if runtime.GOOS != "linux" {
		p.srv.SiteChanged(h1id, []int64{blogID})
	}
	eventually(t, "the change copied by itself", func() bool { return string(copyOf("public/index.html")) == "three" })

	// Both servers serve the domain with DNS-01; removing the replica moves
	// its copy to that server's trash.
	for _, h := range []*host{h1, h2} {
		found := false
		for _, rt := range p.call(200, "GET", "/api/v1/servers/"+h.id+"/routes", nil)["routes"].([]any) {
			if rt := rt.(map[string]any); rt["site_id"] != nil && jsonID(rt["site_id"]) == blog && rt["dns_challenge"] == true {
				found = true
			}
		}
		if !found {
			t.Fatalf("no DNS-01 route of the site on %s", h.id)
		}
	}
	if n := len(p.call(200, "GET", "/api/v1/health-checks", nil)["checks"].([]any)); n != 2 {
		t.Fatalf("%d checks", n)
	}
	p.call(http.StatusNotFound, "DELETE", replicas+"/"+h1.id, nil)
	r = p.call(200, "DELETE", replicas+"/"+h2.id, nil)
	if r["cleanup_error"] != nil || jsonString(r["site"].(map[string]any)["replicas"]) != "[]" {
		t.Fatalf("remove replica: %v", r)
	}
	if _, err := os.Stat(filepath.Join(h2.sitesRoot, blog)); err == nil {
		t.Fatal("the copy stayed in place")
	}
	if n := len(p.call(200, "GET", "/api/v1/health-checks", nil)["checks"].([]any)); n != 0 {
		t.Fatalf("%d checks left", n)
	}
	if string(copyOf("public/index.html")) != "" {
		t.Fatal("copy left")
	}

	// A replicated site goes from every server.
	p.call(http.StatusCreated, "POST", replicas, map[string]any{"server_id": h2id})
	if r := p.call(200, "DELETE", base, nil); r["cleanup_error"] != nil {
		t.Fatalf("delete: %v", r)
	}
	for _, h := range []*host{h1, h2} {
		if _, err := os.Stat(filepath.Join(h.sitesRoot, blog)); err == nil {
			t.Fatalf("the site stayed on %s", h.id)
		}
	}
}
