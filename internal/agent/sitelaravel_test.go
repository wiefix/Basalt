package agent

import (
	"context"
	"crypto/sha256"
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

// pinnedServer serves body as a pinned download (sum: its SHA-256, or a
// wrong one).
func pinnedServer(t *testing.T, name string, body []byte, sum string) pinnedFile {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	t.Cleanup(srv.Close)
	if sum == "" {
		h := sha256.Sum256(body)
		sum = hex.EncodeToString(h[:])
	}
	return pinnedFile{Name: name, URL: srv.URL + "/" + name, SHA256: sum}
}

func testSiteAgent(t *testing.T, siteID int64) (*Agent, string) {
	root := t.TempDir()
	cfg := Config{DataDir: t.TempDir(), SitesRoot: root}
	a := &Agent{dir: cfg.DataDir, sites: newSitesManager(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	if err := a.sites.Ensure(context.Background(), siteID, "", php.Settings{}, ""); err != nil {
		t.Fatal(err)
	}
	return a, filepath.Join(root, "5")
}

// fakeComposer acts out composer create-project and artisan; fail makes
// the named step fail.
func fakeComposer(t *testing.T, calls *[]string, fail string) sitePHPRunner {
	return func(ctx context.Context, siteID int64, version, dir string, env, args []string) error {
		if version != "8.4" || !strings.HasPrefix(args[1], "open_basedir=") || !slices.Contains(env, "COMPOSER_NO_INTERACTION=1") {
			t.Errorf("php %s %v %v", version, args, env)
		}
		args = args[4:] // -d open_basedir=… -d memory_limit=…
		step := args[1] // composer.phar create-project, artisan <command>
		*calls = append(*calls, step)
		if step == fail {
			return errors.New("exit status 1: it went wrong")
		}
		if step != "create-project" {
			if _, err := os.Stat(filepath.Join(dir, ".env")); err != nil {
				t.Errorf("artisan without .env in %s", dir)
			}
			return nil
		}
		app := args[len(args)-1]
		for name, body := range map[string]string{
			"artisan":                 "<?php // artisan",
			".env.example":            "APP_NAME=Laravel\nAPP_ENV=local\nAPP_KEY=\nAPP_DEBUG=true\nAPP_URL=http://localhost\n\nDB_CONNECTION=sqlite\n# DB_HOST=127.0.0.1\n# DB_PORT=3306\n# DB_DATABASE=laravel\n# DB_USERNAME=root\n# DB_PASSWORD=\n",
			"public/index.php":        "<?php // front controller",
			"public/robots.txt":       "User-agent: *",
			"storage/logs/.gitignore": "*",
			"vendor/laravel/framework/src/Illuminate/Foundation/Application.php": "<?php\nclass Application {\n    const VERSION = '13.17.0';\n}\n",
		} {
			p := filepath.Join(app, name)
			os.MkdirAll(filepath.Dir(p), 0o755)
			os.WriteFile(p, []byte(body), 0o644)
		}
		return nil
	}
}

func TestSiteInstallLaravel(t *testing.T) {
	a, site := testSiteAgent(t, 5)
	a.composer = pinnedServer(t, "composer-test.phar", []byte("<?php // composer"), "")
	var calls []string
	a.sitePHP = fakeComposer(t, &calls, "migrate")
	ctx := context.Background()
	req := agentproto.SiteInstall{SiteID: 5, App: agentproto.AppLaravel, DBName: "s5_lv", DBUser: "s5_lv",
		DBPassword: "ABCDEFGHIJKLMNOP234567", PHPVersion: "8.4", URL: "https://example.com"}

	bad := req
	bad.URL = "https://example.com/\nAPP_DEBUG=true"
	if _, err := a.siteInstall(ctx, bad); err == nil || !strings.Contains(err.Error(), "address") {
		t.Fatalf("bad URL: %v", err)
	}

	// A failed step leaves the site as it was.
	if _, err := a.siteInstall(ctx, req); err == nil || !strings.Contains(err.Error(), "artisan migrate") || !strings.Contains(err.Error(), "it went wrong") {
		t.Fatalf("failed migrate: %v", err)
	}
	if strings.Join(calls, ",") != "create-project,package:discover,migrate" {
		t.Fatalf("calls %v", calls)
	}
	entries, _ := os.ReadDir(site)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "logs,public,tmp" {
		t.Fatalf("site after a failure: %v", names)
	}
	if _, err := os.Stat(filepath.Join(site, "public", "index.html")); err != nil {
		t.Fatal("the placeholder went with a failed install")
	}

	// A name Laravel needs that the site already has stops it before anything moves.
	os.Mkdir(filepath.Join(site, "vendor"), 0o755)
	calls = nil
	a.sitePHP = fakeComposer(t, &calls, "")
	if _, err := a.siteInstall(ctx, req); err == nil || !strings.Contains(err.Error(), "already has vendor") {
		t.Fatalf("taken name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(site, "artisan")); !os.IsNotExist(err) {
		t.Fatal("files moved despite the taken name")
	}
	if _, err := os.Stat(filepath.Join(site, laravelStage)); !os.IsNotExist(err) {
		t.Fatal("stage left")
	}
	os.Remove(filepath.Join(site, "vendor"))

	resp, err := a.siteInstall(ctx, req)
	if err != nil || resp.Version != "13.17.0" || resp.Files != 7 {
		t.Fatalf("install = %+v, %v", resp, err)
	}
	for _, p := range []string{"artisan", "public/index.php", "public/robots.txt", "storage/logs/.gitignore", ".env"} {
		if _, err := os.Stat(filepath.Join(site, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, p := range []string{"public/index.html", "public/public", laravelStage} {
		if _, err := os.Stat(filepath.Join(site, p)); !os.IsNotExist(err) {
			t.Errorf("%s exists", p)
		}
	}
	env, _ := os.ReadFile(filepath.Join(site, ".env"))
	for _, want := range []string{"APP_ENV=production\n", "APP_DEBUG=false\n", "APP_KEY=base64:", "APP_URL=https://example.com\n",
		"DB_CONNECTION=mysql\n", "DB_HOST=localhost\n", "DB_DATABASE=s5_lv\n", "DB_PASSWORD=ABCDEFGHIJKLMNOP234567\n", "APP_NAME=Laravel\n"} {
		if !strings.Contains(string(env), want) {
			t.Errorf(".env lacks %q:\n%s", want, env)
		}
	}
	if strings.Contains(string(env), "# DB_") || strings.Count(string(env), "DB_HOST") != 1 {
		t.Errorf(".env keeps commented lines:\n%s", env)
	}
	if fi, _ := os.Stat(filepath.Join(site, ".env")); fi.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode %v", fi.Mode().Perm())
	}
	// A second install finds the folder in use.
	if _, err := a.siteInstall(ctx, req); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("second install: %v", err)
	}
}

func TestDownloadPinned(t *testing.T) {
	a, _ := testSiteAgent(t, 5)
	ctx := context.Background()
	if _, err := a.downloadPinned(ctx, pinnedServer(t, "x.php", []byte("tampered"), strings.Repeat("0", 64))); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad checksum: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "downloads", "x.php")); !os.IsNotExist(err) {
		t.Fatal("a download that does not match was kept")
	}
	f := pinnedServer(t, "x.php", []byte("good"), "")
	p, err := a.downloadPinned(ctx, f)
	if b, _ := os.ReadFile(p); err != nil || string(b) != "good" {
		t.Fatalf("download: %v", err)
	}
	// A kept copy is used again.
	f.URL = "http://127.0.0.1:9/gone"
	if _, err := a.downloadPinned(ctx, f); err != nil {
		t.Fatalf("kept copy: %v", err)
	}
}

func TestSiteAdminer(t *testing.T) {
	a, site := testSiteAgent(t, 5)
	a.adminer = pinnedServer(t, "adminer-6.1.2-mysql-en.php", []byte("<?php // adminer"), "")
	ctx := context.Background()
	call := func(op string) agentproto.SiteAdminerResponse {
		t.Helper()
		r, err := a.siteAdminer(ctx, agentproto.SiteAdminer{SiteID: 5, Op: op})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return r
	}
	if r := call(agentproto.AdminerStatus); r.Path != "" {
		t.Fatalf("status before install: %+v", r)
	}
	// A page of the site's own with a matching name is not Adminer's.
	own := filepath.Join(site, "public", "adminer-"+strings.Repeat("a", 26)+".php")
	os.WriteFile(own, []byte("<?php // mine"), 0o644)

	r := call(agentproto.AdminerInstall)
	if !adminerPageRE.MatchString(strings.TrimPrefix(r.Path, "/")) || r.Version != "6.1.2" {
		t.Fatalf("install: %+v", r)
	}
	page, _ := os.ReadFile(filepath.Join(site, "public", r.Path))
	for _, want := range []string{"// Basalt Adminer 6.1.2.", "return array('localhost', $_GET['username']", "$name == 'server'", "include __DIR__ . '/../.basalt-adminer/adminer.php';"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(site, ".basalt-adminer", "adminer.php")); string(b) != "<?php // adminer" {
		t.Fatal("Adminer not outside public/")
	}
	if s := call(agentproto.AdminerStatus); s != r {
		t.Fatalf("status %+v, want %+v", s, r)
	}
	// Installing again moves it to a new address.
	r2 := call(agentproto.AdminerInstall)
	if r2.Path == r.Path {
		t.Fatal("same address after reinstall")
	}
	if _, err := os.Stat(filepath.Join(site, "public", r.Path)); !os.IsNotExist(err) {
		t.Fatal("old page left")
	}
	if r := call(agentproto.AdminerRemove); r.Path != "" {
		t.Fatalf("remove: %+v", r)
	}
	for _, p := range []string{filepath.Join("public", r2.Path), ".basalt-adminer"} {
		if _, err := os.Stat(filepath.Join(site, p)); !os.IsNotExist(err) {
			t.Errorf("%s left", p)
		}
	}
	if _, err := os.Stat(own); err != nil {
		t.Fatal("the site's own page went")
	}
	if call(agentproto.AdminerStatus).Path != "" {
		t.Fatal("status after remove")
	}
}
