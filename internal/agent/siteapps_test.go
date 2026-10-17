package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/php"
)

// fakeWordPress is a WordPress-shaped .tar.gz.
func fakeWordPress(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "wordpress/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		io.WriteString(tw, body)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestSiteInstallWordPress(t *testing.T) {
	archive := fakeWordPress(t, map[string]string{
		"wordpress/index.php":               "<?php // wp",
		"wordpress/wp-includes/version.php": "<?php\n$wp_version = '6.9.1';\n",
		"wordpress/wp-config-sample.php":    "<?php // sample",
		"elsewhere/evil.php":                "<?php // outside the folder",
		"wordpress/../../escape.php":        "<?php // escapes",
	})
	sum := sha1.Sum(archive)
	served := map[string][]byte{"/wp.tar.gz": archive, "/wp.tar.gz.sha1": []byte(hex.EncodeToString(sum[:]) + "\n")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := served[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer srv.Close()

	root := t.TempDir()
	cfg := Config{DataDir: t.TempDir(), SitesRoot: root, WordPressURL: srv.URL + "/wp.tar.gz"}
	a := &Agent{dir: cfg.DataDir, sites: newSitesManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil))), wordpressURL: cfg.WordPressURL}
	ctx := context.Background()
	if err := a.sites.Ensure(ctx, 5, "", php.Settings{}); err != nil {
		t.Fatal(err)
	}
	req := agentproto.SiteInstall{SiteID: 5, App: agentproto.AppWordPress, DBName: "s5_wp", DBUser: "s5_wp", DBPassword: "ABCDEFGHIJKLMNOP234567"}
	pub := filepath.Join(root, "5", "public")

	// A download that does not match its checksum leaves the site alone.
	served["/wp.tar.gz.sha1"] = []byte(strings.Repeat("0", 40))
	if _, err := a.siteInstall(ctx, req); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad checksum: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pub, "index.html")); err != nil {
		t.Fatal("the placeholder went with a failed download")
	}
	served["/wp.tar.gz.sha1"] = []byte(hex.EncodeToString(sum[:]))
	bad := req
	bad.DBPassword = "it's"
	if _, err := a.siteInstall(ctx, bad); err == nil {
		t.Fatal("a quote in the password was accepted")
	}

	resp, err := a.siteInstall(ctx, req)
	if err != nil || resp.Version != "6.9.1" || resp.Files != 4 { // "../../escape.php" stays inside public/
		t.Fatalf("install = %+v, %v", resp, err)
	}
	if _, err := os.Stat(filepath.Join(pub, "index.html")); !os.IsNotExist(err) {
		t.Fatal("placeholder page left")
	}
	if b, _ := os.ReadFile(filepath.Join(pub, "index.php")); string(b) != "<?php // wp" {
		t.Fatal("WordPress not unpacked into public/")
	}
	for _, p := range []string{filepath.Join(pub, "wordpress"), filepath.Join(pub, "evil.php"), filepath.Join(root, "escape.php"), filepath.Join(root, "5", "escape.php")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s exists", p)
		}
	}
	conf, _ := os.ReadFile(filepath.Join(pub, "wp-config.php"))
	for _, want := range []string{"define('DB_NAME', 's5_wp');", "define('DB_PASSWORD', 'ABCDEFGHIJKLMNOP234567');", "define('AUTH_KEY', '", "FS_METHOD", "require_once ABSPATH . 'wp-settings.php';"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("wp-config lacks %q", want)
		}
	}
	if fi, _ := os.Stat(filepath.Join(pub, "wp-config.php")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("wp-config mode %v", fi.Mode().Perm())
	}
	// A second install finds the folder in use.
	if _, err := a.siteInstall(ctx, req); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("second install: %v", err)
	}
}
