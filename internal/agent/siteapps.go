package agent

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/mariadb"
)

const (
	defaultWordPressURL = "https://wordpress.org/latest.tar.gz"
	maxAppArchive       = 200 << 20
)

var appLimits = files.Limits{MaxTotalBytes: 1 << 30, MaxFiles: 50_000}

// siteInstall puts an application into the site's public folder, which
// must hold nothing but the placeholder page.
func (a *Agent) siteInstall(ctx context.Context, req agentproto.SiteInstall) (agentproto.SiteInstallResponse, error) {
	if req.App != agentproto.AppWordPress && req.App != agentproto.AppLaravel && req.App != agentproto.AppSymfony {
		return agentproto.SiteInstallResponse{}, fmt.Errorf("unknown application %q", req.App)
	}
	if !mariadb.ValidName(req.DBName) || !mariadb.ValidName(req.DBUser) || req.DBPassword == "" ||
		strings.ContainsAny(req.DBPassword, "'\\\x00\n") || len(req.DBPassword) > 128 {
		return agentproto.SiteInstallResponse{}, errors.New("invalid database credentials")
	}
	site, err := a.sites.open(req.SiteID)
	if err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	defer site.Close()
	entries, err := site.List("/public")
	if err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	for _, e := range entries {
		if e.Name != "index.html" {
			return agentproto.SiteInstallResponse{}, errors.New("the site's public folder is not empty: install into a new site, or delete its files first")
		}
	}
	switch req.App {
	case agentproto.AppLaravel:
		return a.installComposerApp(ctx, req, site, len(entries) > 0, laravelApp)
	case agentproto.AppSymfony:
		return a.installComposerApp(ctx, req, site, len(entries) > 0, symfonyApp)
	}

	// Download and check the archive before any of it reaches the site.
	archive, err := a.downloadVerified(ctx, a.wordpressURLOr())
	if err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	defer os.Remove(archive)
	f, err := os.Open(archive)
	if err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return agentproto.SiteInstallResponse{}, fmt.Errorf("read the WordPress archive: %w", err)
	}
	if len(entries) > 0 {
		if err := site.Delete("/public/index.html"); err != nil {
			return agentproto.SiteInstallResponse{}, err
		}
	}
	res, err := site.ExtractTarStrip(gz, "/public", "wordpress/", appLimits)
	if err != nil {
		return agentproto.SiteInstallResponse{}, fmt.Errorf("unpack WordPress: %w", err)
	}
	if res.Files == 0 {
		return agentproto.SiteInstallResponse{}, errors.New("the archive holds no WordPress files")
	}
	cfg, err := wordpressConfig(req)
	if err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	if err := site.WriteFile("/public/wp-config.php", cfg); err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	// Only PHP (the site's user) reads the database password.
	if err := site.Chmod("/public/wp-config.php", 0o600); err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	version := ""
	if b, _, err := site.Read("/public/wp-includes/version.php", 0, 4096); err == nil {
		version = phpStringVar(string(b), "$wp_version")
	}
	return agentproto.SiteInstallResponse{Version: version, Files: res.Files}, nil
}

func (a *Agent) wordpressURLOr() string {
	if a.wordpressURL != "" {
		return a.wordpressURL
	}
	return defaultWordPressURL
}

// downloadVerified saves url into the agent's data directory after checking
// it against the SHA-1 published next to it (url + ".sha1"), over HTTPS.
func (a *Agent) downloadVerified(ctx context.Context, url string) (string, error) {
	hc := &http.Client{Timeout: 10 * time.Minute}
	get := func(u string, limit int64, w io.Writer) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: %s", u, resp.Status)
		}
		n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
		if err == nil && n > limit {
			err = fmt.Errorf("%s is too large", u)
		}
		return err
	}
	var sum strings.Builder
	if err := get(url+".sha1", 128, &sum); err != nil {
		return "", fmt.Errorf("download the checksum: %w", err)
	}
	want := strings.ToLower(strings.Fields(sum.String() + " x")[0])
	if b, err := hex.DecodeString(want); err != nil || len(b) != sha1.Size {
		return "", errors.New("invalid checksum file")
	}
	dir := filepath.Join(a.dir, "downloads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "app-*.tar.gz")
	if err != nil {
		return "", err
	}
	h := sha1.New()
	err = get(url, maxAppArchive, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != want {
		err = errors.New("the download does not match its published checksum")
	}
	if err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("download WordPress: %w", err)
	}
	return f.Name(), nil
}

// wordpressConfig writes wp-config.php with the database and fresh keys.
// Every value is a fixed-alphabet string, quoted for PHP.
func wordpressConfig(req agentproto.SiteInstall) ([]byte, error) {
	var b strings.Builder
	b.WriteString("<?php\n// Written by Basalt when WordPress was installed. Yours to change.\n")
	def := func(k, v string) { fmt.Fprintf(&b, "define('%s', '%s');\n", k, v) }
	def("DB_NAME", req.DBName)
	def("DB_USER", req.DBUser)
	def("DB_PASSWORD", req.DBPassword)
	def("DB_HOST", "localhost")
	def("DB_CHARSET", "utf8mb4")
	def("DB_COLLATE", "")
	for _, k := range []string{"AUTH_KEY", "SECURE_AUTH_KEY", "LOGGED_IN_KEY", "NONCE_KEY", "AUTH_SALT", "SECURE_AUTH_SALT", "LOGGED_IN_SALT", "NONCE_SALT"} {
		def(k, rand.Text()+rand.Text()+rand.Text())
	}
	b.WriteString("$table_prefix = 'wp_';\n")
	b.WriteString("define('WP_DEBUG', false);\n")
	// Plugins and updates write files as the site's user, without FTP.
	b.WriteString("define('FS_METHOD', 'direct');\n")
	b.WriteString("if (!defined('ABSPATH')) {\n\tdefine('ABSPATH', __DIR__ . '/');\n}\nrequire_once ABSPATH . 'wp-settings.php';\n")
	return []byte(b.String()), nil
}

// phpStringVar reads `$name = '...';` from PHP source.
func phpStringVar(src, name string) string {
	i := strings.Index(src, name)
	if i < 0 {
		return ""
	}
	rest := src[i+len(name):]
	q := strings.IndexAny(rest, "'\"")
	if q < 0 {
		return ""
	}
	end := strings.IndexByte(rest[q+1:], rest[q])
	if end < 0 || end > 40 {
		return ""
	}
	return rest[q+1 : q+1+end]
}
