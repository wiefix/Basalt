package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	a.wpCLI = pinnedServer(t, "wp-cli-test.phar", []byte("<?php // wp-cli"), "")
	ctx := context.Background()
	if err := a.sites.Ensure(ctx, 5, "", php.Settings{}, ""); err != nil {
		t.Fatal(err)
	}
	const password = "Secret-Admin-Password-42"
	req := agentproto.SiteInstall{SiteID: 5, App: agentproto.AppWordPress, DBName: "s5_wp", DBUser: "s5_wp", DBPassword: "ABCDEFGHIJKLMNOP234567",
		PHPVersion: "8.4", URL: "https://example.com", Title: "My blog", AdminUser: "editor", AdminEmail: "me@example.com", AdminPassword: password}
	site := filepath.Join(root, "5")
	pub := filepath.Join(site, "public")

	// wp-cli runs on the unpacked WordPress, with its wp-config.php, while
	// public/ still holds the placeholder only.
	var calls []string
	fail := ""
	a.sitePHP = func(ctx context.Context, siteID int64, version, dir string, env, args []string, stdin string) error {
		if version != "8.4" || !strings.HasPrefix(args[1], "open_basedir="+site+"/") || !strings.HasSuffix(args[4], "/wp-cli.phar") {
			t.Errorf("php %s %v", version, args)
		}
		if args[5] != "--path="+filepath.Join(site, installStage, "wordpress") {
			t.Errorf("wp-cli path %s", args[5])
		}
		if strings.Contains(strings.Join(args, " "), password) {
			t.Error("the admin password is on the command line")
		}
		step := args[6] + " " + args[7]
		calls = append(calls, step)
		if step == "core install" {
			want := []string{"--url=https://example.com", "--title=My blog", "--admin_user=editor", "--admin_email=me@example.com", "--skip-email", "--prompt=admin_password"}
			if !slices.Equal(args[8:], want) || stdin != password+"\n" {
				t.Errorf("core install %v, stdin %q", args[8:], stdin)
			}
			if _, err := os.Stat(filepath.Join(site, installStage, "wordpress", "wp-config.php")); err != nil {
				t.Error("wp-cli runs without wp-config.php")
			}
			if entries, _ := os.ReadDir(pub); len(entries) != 1 || entries[0].Name() != "index.html" {
				t.Error("WordPress reached public/ before it was installed")
			}
		}
		if step == fail {
			return errors.New("exit status 1: Error: Error establishing a database connection.")
		}
		return nil
	}

	// A download that does not match its checksum leaves the site alone.
	served["/wp.tar.gz.sha1"] = []byte(strings.Repeat("0", 40))
	if _, err := a.siteInstall(ctx, req); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad checksum: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pub, "index.html")); err != nil {
		t.Fatal("the placeholder went with a failed download")
	}
	served["/wp.tar.gz.sha1"] = []byte(hex.EncodeToString(sum[:]))
	for _, bad := range []func(*agentproto.SiteInstall){
		func(r *agentproto.SiteInstall) { r.DBPassword = "it's" },
		func(r *agentproto.SiteInstall) { r.AdminUser = "--allow-root" },
		func(r *agentproto.SiteInstall) { r.AdminEmail = "me@example.com --debug" },
		func(r *agentproto.SiteInstall) { r.Title = "two\nlines" },
		func(r *agentproto.SiteInstall) { r.AdminPassword = "short" },
		func(r *agentproto.SiteInstall) { r.AdminPassword = password + "\nmore" },
		func(r *agentproto.SiteInstall) { r.URL = "" },
	} {
		r := req
		bad(&r)
		if _, err := a.siteInstall(ctx, r); err == nil {
			t.Fatalf("accepted %+v", r)
		}
	}
	if calls != nil {
		t.Fatalf("wp-cli ran for a bad request: %v", calls)
	}

	// A failed wp-cli leaves the site as it was.
	fail = "core install"
	if _, err := a.siteInstall(ctx, req); err == nil || !strings.Contains(err.Error(), "wp core install") || !strings.Contains(err.Error(), "database connection") {
		t.Fatalf("failed install: %v", err)
	}
	if entries, _ := os.ReadDir(site); len(entries) != 3 { // logs, public, tmp
		t.Fatalf("site after a failure: %v", entries)
	}
	if entries, _ := os.ReadDir(pub); len(entries) != 1 {
		t.Fatalf("public/ after a failure: %v", entries)
	}

	fail, calls = "", nil
	resp, err := a.siteInstall(ctx, req)
	if err != nil || resp.Version != "6.9.1" || resp.Files != 4 { // "../../escape.php" stays inside the folder
		t.Fatalf("install = %+v, %v", resp, err)
	}
	if strings.Join(calls, ",") != "core install,rewrite structure" {
		t.Fatalf("calls %v", calls)
	}
	if _, err := os.Stat(filepath.Join(pub, "index.html")); !os.IsNotExist(err) {
		t.Fatal("placeholder page left")
	}
	if b, _ := os.ReadFile(filepath.Join(pub, "index.php")); string(b) != "<?php // wp" {
		t.Fatal("WordPress not moved into public/")
	}
	for _, p := range []string{filepath.Join(pub, "wordpress"), filepath.Join(pub, "evil.php"), filepath.Join(pub, "wp-cli.phar"),
		filepath.Join(root, "escape.php"), filepath.Join(site, "escape.php"), filepath.Join(site, installStage)} {
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
	if strings.Contains(string(conf), password) {
		t.Error("the admin password is in wp-config.php")
	}
	if fi, _ := os.Stat(filepath.Join(pub, "wp-config.php")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("wp-config mode %v", fi.Mode().Perm())
	}
	// A second install finds the folder in use.
	if _, err := a.siteInstall(ctx, req); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("second install: %v", err)
	}
}
