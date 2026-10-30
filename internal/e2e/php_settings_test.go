package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPHPSettingsAPI: a site's PHP options are validated and stored; the
// PHP error log is read from its end. (Applying them to PHP-FPM is in the
// Linux integration test.)
func TestPHPSettingsAPI(t *testing.T) {
	p := startPanel(t)
	h := startHost(t, p)
	r := p.call(http.StatusCreated, "POST", "/api/v1/sites", map[string]any{
		"server_id": mustAtoi(h.id), "name": "app", "domains": []string{"app.localhost"}, "tls": "internal",
	})
	sid := jsonID(r["site"].(map[string]any)["id"])
	base := "/api/v1/sites/" + sid

	for _, bad := range []map[string]any{
		{"memory_mb": 10}, {"upload_mb": 9999}, {"max_children": 201}, {"timezone": "Europe/Berlin\n"},
		{"timezone": "../etc"}, {"max_input_vars": 1},
	} {
		p.call(http.StatusUnprocessableEntity, "PUT", base+"/php-settings", bad)
	}
	// Only the listed options exist: no free-form ini values.
	p.call(http.StatusBadRequest, "PUT", base+"/php-settings", map[string]any{"open_basedir": "/"})
	p.call(http.StatusBadRequest, "PUT", base+"/php-settings", map[string]any{"max_children": 0.5})
	r = p.call(http.StatusOK, "PUT", base+"/php-settings", map[string]any{"memory_mb": 512, "upload_mb": 100, "display_errors": true, "timezone": "Asia/Tokyo"})
	st := p.call(http.StatusOK, "GET", base, nil)["site"].(map[string]any)["php_settings"].(map[string]any)
	if st["memory_mb"] != float64(512) || st["upload_mb"] != float64(100) || st["display_errors"] != true || st["timezone"] != "Asia/Tokyo" {
		t.Fatalf("settings = %v", st)
	}

	// Laravel processes: a PHP site on a server with systemd only (the
	// services themselves are in the php package and the install test).
	p.call(http.StatusUnprocessableEntity, "PUT", base+"/laravel", map[string]any{"workers": 9})
	p.call(http.StatusBadRequest, "PUT", base+"/laravel", map[string]any{"command": "id"})
	if e := p.call(http.StatusUnprocessableEntity, "PUT", base+"/laravel", map[string]any{"scheduler": true}); !strings.Contains(e["error"].(string), "PHP site") {
		t.Fatalf("scheduler of a static site: %v", e)
	}
	if l := p.call(http.StatusOK, "PUT", base+"/laravel", map[string]any{"scheduler": false, "workers": 0})["site"].(map[string]any)["laravel"].(map[string]any); l["scheduler"] != false || l["workers"] != float64(0) {
		t.Fatalf("laravel = %v", l)
	}
	// Their state, output and restart (on Linux with systemd: the install test).
	if pr := p.call(http.StatusOK, "GET", base+"/laravel", nil)["processes"].([]any); len(pr) != 0 {
		t.Fatalf("processes of a site without them: %v", pr)
	}
	if ls := p.call(http.StatusOK, "GET", base+"/laravel/log?lines=5", nil)["lines"].([]any); len(ls) != 0 {
		t.Fatalf("output of a static site: %v", ls)
	}
	p.call(http.StatusBadRequest, "GET", base+"/laravel/log?lines=0", nil)
	p.call(http.StatusBadRequest, "GET", base+"/laravel/log?lines=1001", nil)
	p.call(http.StatusUnprocessableEntity, "POST", base+"/laravel/restart", nil)

	// WordPress needs a PHP site (the install itself is in the Linux test).
	p.call(http.StatusUnprocessableEntity, "POST", base+"/install", map[string]any{"app": "drupal"})
	if e := p.call(http.StatusUnprocessableEntity, "POST", base+"/install", map[string]any{"app": "wordpress"}); !strings.Contains(e["error"].(string), "PHP") {
		t.Fatalf("install on a static site: %v", e)
	}

	// The log: empty before PHP writes it, then its end in whole lines.
	if l := p.call(http.StatusOK, "GET", base+"/php-log", nil); l["log"] != "" || l["size"] != float64(0) {
		t.Fatalf("empty log = %v", l)
	}
	logFile := filepath.Join(h.sitesRoot, sid, "logs", "php-error.log")
	os.WriteFile(logFile, []byte("[01-Jan-2026] PHP Warning: one\n[01-Jan-2026] PHP Fatal error: two\n"), 0o644)
	if l := p.call(http.StatusOK, "GET", base+"/php-log", nil); !strings.HasSuffix(l["log"].(string), "Fatal error: two\n") || l["truncated"] != false {
		t.Fatalf("log = %v", l)
	}
	big := strings.Repeat("old line that scrolled away\n", 20000) + "the newest line\n"
	os.WriteFile(logFile, []byte(big), 0o644)
	l := p.call(http.StatusOK, "GET", base+"/php-log", nil)
	text := l["log"].(string)
	if l["truncated"] != true || !strings.HasSuffix(text, "the newest line\n") || !strings.HasPrefix(text, "old line") || len(text) > 256<<10 {
		t.Fatalf("tail of a big log: truncated=%v len=%d start=%q", l["truncated"], len(text), text[:min(30, len(text))])
	}

	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": "v@example.com", "name": "V", "password": "a long password 1", "role": "viewer"})
	v := *p
	v.hc = &http.Client{Jar: newJar()}
	v.call(http.StatusOK, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "a long password 1"})
	v.call(http.StatusOK, "GET", base+"/php-log", nil)
	v.call(http.StatusForbidden, "PUT", base+"/php-settings", map[string]any{"memory_mb": 128})
	v.call(http.StatusForbidden, "PUT", base+"/laravel", map[string]any{})
	v.call(http.StatusOK, "GET", base+"/laravel/log", nil)
	v.call(http.StatusForbidden, "POST", base+"/laravel/restart", nil)
}
