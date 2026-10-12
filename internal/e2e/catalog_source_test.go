package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/catalog"
)

// catalogServer publishes a signed catalog index over HTTP.
type catalogServer struct {
	mu        sync.Mutex
	index     []byte
	signature []byte
	srv       *httptest.Server
}

func (c *catalogServer) publish(t *testing.T, signer ssh.Signer, generated time.Time, entries ...catalog.IndexEntry) {
	t.Helper()
	b, _ := json.Marshal(catalog.Index{Version: catalog.IndexVersion, Generated: generated, Templates: entries})
	sig, err := catalog.SignSSHSig(b, signer, catalog.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.index, c.signature = b, sig
	c.mu.Unlock()
}

func (c *catalogServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch r.URL.Path {
	case "/catalog.json":
		w.Write(c.index)
	case "/catalog.json.sig":
		w.Write(c.signature)
	default:
		http.NotFound(w, r)
	}
}

// readTemplate reads a file of a built-in template.
func readTemplate(t *testing.T, id, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "catalog", "templates", id, file))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func demoEntry(revision int, greeting string) catalog.IndexEntry {
	return catalog.IndexEntry{
		Template: "id: remote-demo\nname: Remote demo\nversion: \"" + greeting + "\"\nrevision: " + string(rune('0'+revision)) +
			"\ninputs:\n  - {key: DOMAIN, label: Domain, type: domain, required: true}\nexpose: {service: web, port: 80}\n",
		Compose: "services:\n  web:\n    image: " + whoamiImage + "\n    environment:\n      WHOAMI_NAME: " + greeting + "\n",
	}
}

func TestCatalogSource(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(otherPriv)

	cs := &catalogServer{}
	cs.srv = httptest.NewServer(cs)
	t.Cleanup(cs.srv.Close)
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// The remote catalog has its own app and a newer WordPress.
	wpEntry := catalog.IndexEntry{Template: strings.Replace(readTemplate(t, "wordpress", "template.yaml"), "revision: 1", "revision: 7", 1),
		Compose: readTemplate(t, "wordpress", "compose.yaml")}
	cs.publish(t, signer, day, demoEntry(1, "one"), wpEntry)

	p := startPanel(t)
	h := startHost(t, p)
	sid := mustAtoi(h.id)
	if _, err := p.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	url := cs.srv.URL + "/catalog.json"
	src := func(want int, body map[string]any) map[string]any {
		return p.call(want, "PUT", "/api/v1/catalog/source", body)
	}
	// Plain http, a private address, the wrong key, a bad key, a missing file.
	src(http.StatusUnprocessableEntity, map[string]any{"url": url, "key": pub})
	if e := src(http.StatusUnprocessableEntity, map[string]any{"url": strings.Replace(url, "http://", "https://", 1), "key": pub}); !strings.Contains(e["error"].(string), "not a public address") {
		t.Fatalf("private address: %v", e)
	}
	if e := src(http.StatusUnprocessableEntity, map[string]any{"url": url, "allow_private": true}); !strings.Contains(e["error"].(string), "signed by another key") {
		t.Fatalf("Basalt's key: %v", e)
	}
	src(http.StatusUnprocessableEntity, map[string]any{"url": url, "key": "ssh-ed25519 nonsense", "allow_private": true})
	src(http.StatusUnprocessableEntity, map[string]any{"url": cs.srv.URL + "/missing.json", "key": pub, "allow_private": true})
	src(http.StatusUnprocessableEntity, map[string]any{"url": "ftp://example.com/catalog.json", "key": pub})
	if s := p.call(200, "GET", "/api/v1/catalog", nil)["source"]; s != nil {
		t.Fatalf("source saved after failures: %v", s)
	}

	s := src(200, map[string]any{"url": url, "key": pub, "allow_private": true})["source"].(map[string]any)
	if s["templates"] != float64(2) || s["error"] != nil {
		t.Fatalf("source = %v", s)
	}
	list := p.call(200, "GET", "/api/v1/catalog", nil)
	find := func(id string) map[string]any {
		for _, tpl := range list["templates"].([]any) {
			if tpl := tpl.(map[string]any); tpl["id"] == id {
				return tpl
			}
		}
		return nil
	}
	host := strings.TrimPrefix(cs.srv.URL, "http://")
	if d := find("remote-demo"); d == nil || d["source"] != host {
		t.Fatalf("remote template: %v", d)
	}
	if w := find("wordpress"); w["revision"] != float64(7) || w["source"] != host {
		t.Fatalf("wordpress: %v", w)
	}
	if g := find("ghost"); g["source"] != "builtin" {
		t.Fatalf("ghost: %v", g)
	}
	app := p.call(http.StatusCreated, "POST", "/api/v1/catalog/remote-demo/install", map[string]any{"server_id": sid,
		"inputs": map[string]string{"DOMAIN": "remote.localhost"}})["app"].(map[string]any)
	base := "/api/v1/apps/" + jsonID(app["id"])

	// A tampered index, then an older one: refused, the catalog stays.
	cs.mu.Lock()
	cs.index = append(cs.index[:len(cs.index)-1:len(cs.index)-1], ' ', '}')
	cs.mu.Unlock()
	if e := p.call(http.StatusBadGateway, "POST", "/api/v1/catalog/refresh", nil); !strings.Contains(e["error"].(string), "does not match") {
		t.Fatalf("tampered: %v", e)
	}
	cs.publish(t, signer, day.Add(-time.Hour), demoEntry(2, "older"))
	if e := p.call(http.StatusBadGateway, "POST", "/api/v1/catalog/refresh", nil); !strings.Contains(e["error"].(string), "older") {
		t.Fatalf("older: %v", e)
	}
	cs.publish(t, other, day.Add(time.Hour), demoEntry(2, "evil"))
	p.call(http.StatusBadGateway, "POST", "/api/v1/catalog/refresh", nil)
	s = p.call(200, "GET", "/api/v1/catalog", nil)["source"].(map[string]any)
	if s["templates"] != float64(2) || !strings.Contains(s["error"].(string), "another key") {
		t.Fatalf("source after failures = %v", s)
	}
	if tpl := p.call(200, "GET", base+"/template", nil); tpl["update_available"] != false {
		t.Fatalf("update from a refused index: %v", tpl)
	}

	// A newer, properly signed index brings an update.
	cs.publish(t, signer, day.Add(2*time.Hour), demoEntry(2, "two"), wpEntry)
	s = p.call(200, "POST", "/api/v1/catalog/refresh", nil)["source"].(map[string]any)
	if s["error"] != nil {
		t.Fatalf("refresh = %v", s)
	}
	if tpl := p.call(200, "GET", base+"/template", nil); tpl["update_available"] != true || tpl["template"].(map[string]any)["version"] != "two" {
		t.Fatalf("template = %v", tpl)
	}

	// Viewers see the catalog but change nothing.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer",
	})
	viewer := *p
	viewer.hc = &http.Client{Jar: newJar()}
	viewer.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	viewer.call(http.StatusForbidden, "PUT", "/api/v1/catalog/source", map[string]any{"url": ""})
	viewer.call(http.StatusForbidden, "POST", "/api/v1/catalog/refresh", nil)

	// Without the source, only the built-in templates remain.
	src(200, map[string]any{"url": ""})
	list = p.call(200, "GET", "/api/v1/catalog", nil)
	if find("remote-demo") != nil || find("wordpress")["revision"] != float64(1) || list["source"] != nil {
		t.Fatalf("after removal: %v", list["source"])
	}
	if tpl := p.call(200, "GET", base+"/template", nil); tpl["template"] != nil {
		t.Fatalf("template = %v", tpl)
	}
}
