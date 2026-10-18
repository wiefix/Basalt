package e2e

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestSnapshotFiles: browse a backup of a site's files, download a file or
// a folder (zip), and restore a copy into a new folder of the site.
func TestSnapshotFiles(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic is not installed")
	}
	p := startPanel(t)
	h := startHost(t, p)
	eventually(t, "restic version reported", func() bool { return p.server(h.id)["restic"] != "" })
	st := p.call(http.StatusCreated, "POST", "/api/v1/backup-storages", map[string]any{
		"name": "local", "kind": "local", "path": filepath.Join(t.TempDir(), "backups")})["storage"].(map[string]any)

	// newSite makes a site with files and a backup policy.
	newSite := func(name string, files map[string]string) string {
		t.Helper()
		r := p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
			"server_id": mustAtoi(h.id), "name": name, "domains": []string{name + ".localhost"}, "tls": "internal",
		})
		sid := jsonID(r["site"].(map[string]any)["id"])
		for path, content := range files {
			full := filepath.Join(h.sitesRoot, sid, path)
			os.MkdirAll(filepath.Dir(full), 0o755)
			if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		p.call(http.StatusOK, "PUT", "/api/v1/sites/"+sid+"/backup-policy", map[string]any{
			"storage_id": mustAtoi(jsonID(st["id"])), "schedule": "manual", "keep_last": 5})
		return sid
	}
	backup := func(sid string) string {
		t.Helper()
		base := "/api/v1/sites/" + sid
		b := p.call(http.StatusAccepted, "POST", base+"/backups", map[string]any{})["backup"].(map[string]any)
		if done := waitBackup(t, p, sid, jsonID(b["id"])); done["status"] != "succeeded" {
			t.Fatalf("backup: %v\n%v", done, done["log"])
		}
		for _, s := range p.call(http.StatusOK, "GET", base+"/snapshots", nil)["snapshots"].([]any) {
			s := s.(map[string]any)
			if s["item"].(map[string]any)["kind"] == "site_files" {
				return s["id"].(string) // newest first
			}
		}
		t.Fatal("no snapshot of the files")
		return ""
	}

	sid := newSite("docs", map[string]string{
		"public/index.html":      "one",
		"public/img/a.png":       "png bytes",
		"public/img/sub/b c.txt": "b",
	})
	dir := filepath.Join(h.sitesRoot, sid)
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "public", "evil")); err != nil {
		t.Fatal(err)
	}
	snap := backup(sid)
	base := "/api/v1/sites/" + sid + "/snapshots/" + snap

	names := func(r map[string]any) []string {
		var out []string
		for _, e := range r["entries"].([]any) {
			e := e.(map[string]any)
			out = append(out, e["type"].(string)+":"+e["name"].(string))
		}
		return out
	}
	// Browse: folders first, the symlink shown as such.
	if got := names(p.call(http.StatusOK, "GET", base+"/files?path=", nil)); !slices.Contains(got, "dir:public") || !slices.Contains(got, "dir:tmp") {
		t.Fatalf("top = %v", got)
	}
	pub := p.call(http.StatusOK, "GET", base+"/files?path=public", nil)
	if got := names(pub); !slices.Equal(got, []string{"dir:img", "symlink:evil", "file:index.html"}) || pub["path"] != "public" {
		t.Fatalf("public = %v", pub)
	}
	if e := pub["entries"].([]any)[2].(map[string]any); e["size"] != float64(3) || e["mode"] != "0644" {
		t.Fatalf("index.html = %v", e)
	}
	if got := names(p.call(http.StatusOK, "GET", base+"/files?path="+url.QueryEscape("/public/img/"), nil)); !slices.Equal(got, []string{"dir:sub", "file:a.png"}) {
		t.Fatalf("img = %v", got)
	}
	// Paths stay inside the site's folder of the snapshot.
	p.call(http.StatusNotFound, "GET", base+"/files?path="+url.QueryEscape("../../etc"), nil)
	p.call(http.StatusNotFound, "GET", base+"/files?path=nope", nil)
	p.call(http.StatusUnprocessableEntity, "GET", base+"/files?path=public/index.html", nil)
	p.call(http.StatusUnprocessableEntity, "GET", base+"/files?path="+url.QueryEscape("a\nb"), nil)
	p.call(http.StatusNotFound, "GET", "/api/v1/sites/"+sid+"/snapshots/xyz/files", nil)
	p.call(http.StatusUnprocessableEntity, "GET", "/api/v1/sites/"+sid+"/snapshots/deadbeef/files", nil)

	// Another site's backup is not reachable through this one.
	other := newSite("other", map[string]string{"public/secret.txt": "theirs"})
	otherSnap := backup(other)
	p.call(http.StatusUnprocessableEntity, "GET", "/api/v1/sites/"+sid+"/snapshots/"+otherSnap+"/files?path=public", nil)
	p.call(http.StatusUnprocessableEntity, "GET", "/api/v1/sites/"+sid+"/snapshots/"+otherSnap+"/download?path=public/secret.txt", nil)
	// A site has no volumes; the files' snapshot is no database dump.
	p.call(http.StatusUnprocessableEntity, "GET", base+"/files?volume=html", nil)
	p.call(http.StatusUnprocessableEntity, "GET", base+"/download?database=s1_wp", nil)
	p.call(http.StatusUnprocessableEntity, "GET", base+"/download?database="+url.QueryEscape("a;b"), nil)

	// Downloads: a file as it was, a folder as a zip with relative names.
	code, hdr, body := p.raw("GET", base+"/download?path=public/index.html", "", nil)
	if code != http.StatusOK || string(body) != "one" || !strings.Contains(hdr.Get("Content-Disposition"), `attachment; filename=index.html`) ||
		!strings.HasPrefix(hdr.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("download: %d %q %v", code, body, hdr)
	}
	zipNames := func(path string) map[string]string {
		t.Helper()
		code, hdr, body := p.raw("GET", base+"/download?path="+url.QueryEscape(path), "", nil)
		if code != http.StatusOK {
			t.Fatalf("download %s: %d %s", path, code, body)
		}
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatalf("zip of %s: %v (%s)", path, err, hdr.Get("Content-Disposition"))
		}
		out := map[string]string{"": hdr.Get("Content-Disposition")}
		for _, f := range zr.File {
			if f.FileInfo().Mode().IsRegular() {
				rc, _ := f.Open()
				b, _ := io.ReadAll(rc)
				rc.Close()
				out[f.Name] = string(b)
			}
		}
		return out
	}
	if z := zipNames("public/img"); z["a.png"] != "png bytes" || z["sub/b c.txt"] != "b" || !strings.Contains(z[""], "img.zip") {
		t.Fatalf("img.zip = %v", z)
	}
	if z := zipNames(""); z["public/index.html"] != "one" || !strings.Contains(z[""], "site-"+sid+"-"+snap[:8]+".zip") {
		t.Fatalf("site zip = %v", z)
	}
	p.call(http.StatusUnprocessableEntity, "GET", base+"/download?path=public/evil", nil)
	if left, _ := os.ReadDir(filepath.Join(h.dataDir, "exports")); len(left) != 0 {
		t.Fatalf("exported files left on the server: %v", left)
	}

	// Restore a copy: into a new folder; the current files stay.
	os.WriteFile(filepath.Join(dir, "public", "index.html"), []byte("changed"), 0o644)
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	c1 := p.call(http.StatusCreated, "POST", base+"/restore-copy", map[string]any{"path": "/public/index.html"})
	f1 := c1["folder"].(string)
	if !strings.HasPrefix(f1, "restored-") || c1["files"] != float64(1) || read(f1+"/index.html") != "one" || read("public/index.html") != "changed" {
		t.Fatalf("copy of a file = %v", c1)
	}
	c2 := p.call(http.StatusCreated, "POST", base+"/restore-copy", map[string]any{"path": "public"})
	f2 := c2["folder"].(string)
	if f2 == f1 || c2["files"] != float64(3) || c2["skipped"] != float64(1) || read(f2+"/public/img/sub/b c.txt") != "b" {
		t.Fatalf("copy of a folder = %v", c2)
	}
	if _, err := os.Lstat(filepath.Join(dir, f2, "public", "evil")); !os.IsNotExist(err) {
		t.Fatal("a symlink was restored")
	}
	if fi, err := os.Stat(filepath.Join(dir, f2, "public", "index.html")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("restored mode: %v %v", fi, err)
	}
	c3 := p.call(http.StatusCreated, "POST", base+"/restore-copy", map[string]any{"path": ""})
	if read(c3["folder"].(string)+"/public/img/a.png") != "png bytes" {
		t.Fatalf("copy of the site = %v", c3)
	}
	p.call(http.StatusNotFound, "POST", base+"/restore-copy", map[string]any{"path": "missing"})

	// Roles: viewers cannot read backups; members can.
	for email, role := range map[string]string{"v@example.com": "viewer", "m@example.com": "member"} {
		p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": email, "name": role, "password": "a long password 1", "role": role})
	}
	login := func(email string) *panel {
		u := *p
		u.hc = &http.Client{Jar: newJar()}
		u.call(http.StatusOK, "POST", "/api/v1/auth/login", map[string]string{"email": email, "password": "a long password 1"})
		return &u
	}
	v := login("v@example.com")
	v.call(http.StatusForbidden, "GET", base+"/files", nil)
	v.call(http.StatusForbidden, "GET", base+"/download?path=public/index.html", nil)
	v.call(http.StatusForbidden, "POST", base+"/restore-copy", map[string]any{"path": "public"})
	login("m@example.com").call(http.StatusOK, "GET", base+"/files?path=public", nil)

	var downloads, copies int
	p.db.R.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'backup.file_downloaded'`).Scan(&downloads)
	p.db.R.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'backup.copy_restored'`).Scan(&copies)
	if downloads != 3 || copies != 3 {
		t.Fatalf("audit: %d downloads, %d copies", downloads, copies)
	}
}
