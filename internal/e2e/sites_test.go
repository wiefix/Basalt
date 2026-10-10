package e2e

import (
	"archive/zip"
	"fmt"
	"net"
	"strconv"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/proxy/caddyrun"
)

// host is an enrolled agent plus its proxy, both running.
type host struct {
	id        string
	sitesRoot string
	get       func(host, path string) (int, string)
}

func startHost(t *testing.T, p *panel) *host { return startHostWith(t, p, "") }

// startHostWith runs a host whose agent uses the Docker daemon at
// dockerSocket ("" disables apps).
func startHostWith(t *testing.T, p *panel, dockerSocket string) *host {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxyDir, _ := os.MkdirTemp("/tmp", "bp")
	proxyDone := make(chan error, 1)
	go func() { proxyDone <- caddyrun.Run(ctx, proxyDir, quiet) }()

	created := p.call(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "web"})
	id := jsonID(created["server"].(map[string]any)["id"])
	agentDir, sitesRoot := t.TempDir(), t.TempDir()
	if _, err := agent.Enroll(ctx, agent.EnrollOptions{
		DataDir: agentDir, PanelURL: p.agentURL,
		Token:    created["enrollment"].(map[string]any)["token"].(string),
		CASHA256: p.hub.CAFingerprint(),
	}); err != nil {
		t.Fatal(err)
	}
	httpsPort := freePort(t)
	a, err := agent.New(agent.Config{
		DataDir: agentDir, ProxyDir: proxyDir, SitesRoot: sitesRoot,
		HTTPPort: freePort(t), HTTPSPort: httpsPort, SFTPListen: "127.0.0.1:0", DockerSocket: dockerSocket,
	}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	agentDone := make(chan struct{})
	go func() { a.Run(ctx); close(agentDone) }()
	t.Cleanup(func() { cancel(); <-agentDone; <-proxyDone; os.RemoveAll(proxyDir) })
	eventually(t, "agent online", func() bool { return p.server(id)["status"] == "online" })
	return &host{id: id, sitesRoot: sitesRoot, get: httpsGetter(t, httpsPort)}
}

// raw performs a request with an arbitrary body and returns status and body.
func (p *panel) raw(method, path, contentType string, body []byte) (int, http.Header, []byte) {
	p.t.Helper()
	req, _ := http.NewRequest(method, p.api.URL+path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, b
}

func TestSitesAndFileManager(t *testing.T) {
	p := startPanel(t)
	h := startHost(t, p)

	// Create a site: directories, placeholder page and route appear.
	r := p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(h.id), "name": "blog", "domains": []string{"blog.localhost"}, "tls": "internal",
	})
	site := r["site"].(map[string]any)
	sid := jsonID(site["id"])
	if r["sync"].(map[string]any)["in_sync"] != true || r["provision_error"] != nil {
		t.Fatalf("site not provisioned: %v", r)
	}
	for _, d := range []string{"public", "logs", "tmp"} {
		if fi, err := os.Stat(filepath.Join(h.sitesRoot, sid, d)); err != nil || !fi.IsDir() {
			t.Fatalf("missing %s dir", d)
		}
	}
	eventually(t, "placeholder served", func() bool {
		code, body := h.get("blog.localhost", "/")
		return code == 200 && strings.Contains(body, "It works!")
	})
	p.call(http.StatusConflict, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(h.id), "name": "blog2", "domains": []string{"blog.localhost"},
	})

	base := "/api/v1/sites/" + sid + "/files"
	q := func(path string) string { return "?path=" + url.QueryEscape(path) }

	// Upload replaces the placeholder; the site serves it immediately.
	code, _, _ := p.raw("PUT", base+"/content"+q("/public/index.html"), "", []byte("<h1>My blog</h1>"))
	if code != http.StatusCreated {
		t.Fatalf("upload: %d", code)
	}
	if code, body := h.get("blog.localhost", "/"); code != 200 || body != "<h1>My blog</h1>" {
		t.Fatalf("after upload: %d %q", code, body)
	}

	// A multi-chunk upload (2.5 MiB) and download round trip.
	big := bytes.Repeat([]byte("0123456789abcdef"), 160*1024)
	if code, _, b := p.raw("PUT", base+"/content"+q("/public/big.bin"), "", big); code != http.StatusCreated {
		t.Fatalf("big upload: %d %s", code, b)
	}
	code, hdr, got := p.raw("GET", base+"/content"+q("/public/big.bin"), "", nil)
	if code != 200 || !bytes.Equal(got, big) {
		t.Fatalf("download: %d, %d bytes (want %d)", code, len(got), len(big))
	}
	if !strings.HasPrefix(hdr.Get("Content-Disposition"), "attachment") || !strings.Contains(hdr.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("download headers allow rendering in the panel origin: %v", hdr)
	}

	// Editor: write and read a text file; mkdir, rename, chmod.
	p.call(200, "POST", base+"/mkdir", map[string]any{"path": "/public/css"})
	p.call(200, "POST", base+"/write", map[string]any{"path": "/public/css/site.css", "content": "body{}"})
	if r := p.call(200, "GET", base+"/text"+q("/public/css/site.css"), nil); r["content"] != "body{}" {
		t.Fatalf("text = %v", r)
	}
	p.call(200, "POST", base+"/rename", map[string]any{"path": "/public/css/site.css", "to": "/public/css/main.css"})
	p.call(200, "POST", base+"/chmod", map[string]any{"path": "/public/css/main.css", "mode": "600"})
	p.call(http.StatusUnprocessableEntity, "POST", base+"/chmod", map[string]any{"path": "/public/css/main.css", "mode": "4755"})
	list := p.call(200, "GET", base+q("/public/css"), nil)["entries"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["name"] != "main.css" || list[0].(map[string]any)["mode"] != "0600" {
		t.Fatalf("list = %v", list)
	}
	p.call(http.StatusConflict, "POST", base+"/mkdir", map[string]any{"path": "/public/css"})

	// Upload a zip and extract it into the site.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	for name, body := range map[string]string{"about.html": "about", "img/logo.svg": "<svg/>", "../../escape.txt": "x"} {
		w, _ := zw.Create(name)
		io.WriteString(w, body)
	}
	zw.Close()
	p.raw("PUT", base+"/content"+q("/upload.zip"), "", zbuf.Bytes())
	ex := p.call(200, "POST", base+"/extract", map[string]any{"path": "/upload.zip", "to": "/public"})
	if ex["extract"].(map[string]any)["files"] != float64(3) {
		t.Fatalf("extract = %v", ex)
	}
	if code, body := h.get("blog.localhost", "/about.html"); code != 200 || body != "about" {
		t.Fatalf("extracted page: %d %q", code, body)
	}
	if _, err := os.Stat(filepath.Join(h.sitesRoot, "escape.txt")); err == nil {
		t.Fatal("zip entry escaped the site")
	}

	// API paths cannot leave the site either.
	os.WriteFile(filepath.Join(h.sitesRoot, "secret.txt"), []byte("SECRET"), 0o600)
	for _, path := range []string{"/../secret.txt", "../../secret.txt", "/public/../../secret.txt"} {
		code, _, body := p.raw("GET", base+"/content"+q(path), "", nil)
		if code == 200 || bytes.Contains(body, []byte("SECRET")) {
			t.Fatalf("download %s escaped: %d %q", path, code, body)
		}
	}
	p.call(http.StatusUnprocessableEntity, "POST", base+"/delete", map[string]any{"paths": []string{"/"}})

	// Compress and delete.
	p.call(200, "POST", base+"/compress", map[string]any{"paths": []string{"/public"}, "to": "/backup.zip"})
	p.call(200, "POST", base+"/delete", map[string]any{"paths": []string{"/public/big.bin", "/upload.zip"}})
	names := []string{}
	for _, e := range p.call(200, "GET", base+q("/"), nil)["entries"].([]any) {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "logs,public,tmp,backup.zip" {
		t.Fatalf("root listing = %v", names)
	}

	// Change domains.
	p.call(200, "PUT", "/api/v1/sites/"+sid+"/domains", map[string]any{"domains": []string{"news.localhost"}})
	eventually(t, "new domain served", func() bool { code, _ := h.get("news.localhost", "/about.html"); return code == 200 })

	// Deleting the site stops serving and moves files to the trash.
	r = p.call(200, "DELETE", "/api/v1/sites/"+sid, nil)
	if r["cleanup_error"] != nil {
		t.Fatalf("delete: %v", r)
	}
	if _, err := os.Stat(filepath.Join(h.sitesRoot, sid)); err == nil {
		t.Fatal("site directory still in place")
	}
	trash, _ := os.ReadDir(filepath.Join(h.sitesRoot, ".trash"))
	if len(trash) != 1 || !strings.HasPrefix(trash[0].Name(), sid+"-") {
		t.Fatalf("trash = %v", trash)
	}
	eventually(t, "site no longer served", func() bool { code, _ := h.get("news.localhost", "/"); return code != 200 })
	p.call(http.StatusNotFound, "GET", base+q("/"), nil)
}

func TestFileManagerPermissions(t *testing.T) {
	p := startPanel(t)
	h := startHost(t, p)
	r := p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(h.id), "name": "s", "domains": []string{"s.localhost"},
	})
	sid := jsonID(r["site"].(map[string]any)["id"])
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer",
	})

	viewer := *p
	viewer.hc = &http.Client{Jar: newJar()}
	viewer.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	base := "/api/v1/sites/" + sid + "/files"
	viewer.call(200, "GET", base+"?path=/", nil)
	viewer.call(http.StatusForbidden, "POST", base+"/mkdir", map[string]any{"path": "/x"})
	if code, _, _ := viewer.raw("PUT", base+"/content?path=/public/x", "", []byte("x")); code != http.StatusForbidden {
		t.Fatalf("viewer upload: %d", code)
	}
	viewer.call(http.StatusForbidden, "DELETE", "/api/v1/sites/"+sid, nil)
	_ = json.Valid
}

func TestSFTPAccounts(t *testing.T) {
	p := startPanel(t)
	h := startHost(t, p)
	r := p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(h.id), "name": "shop", "domains": []string{"shop.localhost"}, "tls": "internal",
	})
	sid := jsonID(r["site"].(map[string]any)["id"])
	base := "/api/v1/sites/" + sid + "/sftp-users"

	p.call(http.StatusUnprocessableEntity, "POST", base, map[string]any{"username": "Bad Name"})
	p.call(http.StatusUnprocessableEntity, "POST", base, map[string]any{"username": "keyonly", "password": "-"})
	r = p.call(http.StatusCreated, "POST", base, map[string]any{"username": "shop"})
	password := r["password"].(string)
	conn := r["connection"].(map[string]any)
	port := int(conn["port"].(float64))
	fingerprint := conn["host_key_fingerprint"].(string)
	if r["sync_error"] != nil || port == 0 || !strings.HasPrefix(fingerprint, "SHA256:") {
		t.Fatalf("create: %v", r)
	}
	p.call(http.StatusConflict, "POST", base, map[string]any{"username": "shop"})
	list := p.call(200, "GET", base, nil)["users"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["has_password"] != true {
		t.Fatalf("list = %v", list)
	}

	dial := func(pw string) (*ssh.Client, error) {
		return ssh.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port), &ssh.ClientConfig{
			User: "shop", Auth: []ssh.AuthMethod{ssh.Password(pw)},
			// Verify the host exactly as a user would: by the fingerprint the panel shows.
			HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
				if ssh.FingerprintSHA256(key) != fingerprint {
					return fmt.Errorf("host key mismatch")
				}
				return nil
			},
		})
	}
	c, err := dial(password)
	if err != nil {
		t.Fatalf("sftp login: %v", err)
	}
	sc, err := sftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	w, err := sc.Create("/public/index.html")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "via sftp")
	w.Close()
	sc.Close()
	c.Close()
	eventually(t, "sftp upload served", func() bool {
		code, body := h.get("shop.localhost", "/")
		return code == 200 && body == "via sftp"
	})

	// Deleting the account revokes access immediately.
	uid := jsonID(list[0].(map[string]any)["id"])
	if r := p.call(200, "DELETE", base+"/"+uid, nil); r["sync_error"] != nil {
		t.Fatalf("delete: %v", r)
	}
	if _, err := dial(password); err == nil {
		t.Fatal("deleted account can still log in")
	}

	// Deleting the site revokes its accounts too.
	r = p.call(http.StatusCreated, "POST", base, map[string]any{"username": "shop2"})
	pw2 := r["password"].(string)
	p.call(200, "DELETE", "/api/v1/sites/"+sid, nil)
	if _, err := ssh.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port), &ssh.ClientConfig{
		User: "shop2", Auth: []ssh.AuthMethod{ssh.Password(pw2)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}); err == nil {
		t.Fatal("account of a deleted site can still log in")
	}
}
