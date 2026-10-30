package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
)

// pinnedFile is a download whose SHA-256 is part of the agent.
type pinnedFile struct {
	Name   string // file name in the downloads folder
	URL    string
	SHA256 string
}

var (
	composerPhar = pinnedFile{
		Name:   "composer-2.10.3.phar",
		URL:    "https://getcomposer.org/download/2.10.3/composer.phar",
		SHA256: "7a2d379d5b8ffdaa028580ef26494c36d2feef4b178d3dd1473a4dbc5e17c8d6",
	}
	adminerPHP = pinnedFile{
		Name:   "adminer-6.1.2-mysql-en.php",
		URL:    "https://github.com/vrana/adminer/releases/download/v6.1.2/adminer-6.1.2-mysql-en.php",
		SHA256: "7526b56dc5ecee5b5cc88206ac8f3a757392c1b991838efa71bf9f7abbb8172e",
	}
)

const (
	maxPinned      = 20 << 20
	laravelTimeout = 12 * time.Minute
	// laravelStage is the folder, in the site's own folder, Laravel is
	// put together in before its files take their places.
	laravelStage = ".basalt-install"
)

// sitePHPRunner runs the site's PHP CLI (php<version> args...) as the
// site's user in dir.
type sitePHPRunner func(ctx context.Context, siteID int64, version, dir string, env, args []string) error

func (a *Agent) runSitePHP(ctx context.Context, siteID int64, version, dir string, env, args []string) error {
	if a.sitePHP != nil {
		return a.sitePHP(ctx, siteID, version, dir, env, args)
	}
	return a.sites.runAsSite(ctx, siteID, dir, env, "/usr/bin/php"+version, args...)
}

var appURLRE = regexp.MustCompile(`^https?://[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)

// installLaravel makes the site a new Laravel project: Composer creates it
// in a folder of the site, as the site's user; it gets its .env (MariaDB,
// a fresh key) and its tables; only then do its files take their places,
// public/ becoming the site's public folder. A failure leaves the site as
// it was.
func (a *Agent) installLaravel(ctx context.Context, req agentproto.SiteInstall, site *files.Site, placeholder bool) (agentproto.SiteInstallResponse, error) {
	if a.sitePHP == nil && !slices.Contains(a.sites.PHPVersions(), req.PHPVersion) {
		return agentproto.SiteInstallResponse{}, fmt.Errorf("PHP %q is not installed on this server", req.PHPVersion)
	}
	if req.URL == "" {
		req.URL = "http://localhost"
	}
	if !appURLRE.MatchString(req.URL) {
		return agentproto.SiteInstallResponse{}, errors.New("invalid site address")
	}
	// Laravel's top-level names must be free (logs/ and tmp/ are the
	// site's; a stage left by an agent that stopped midway goes).
	if _, err := site.Stat("/" + laravelStage); err == nil {
		if err := site.Delete("/" + laravelStage); err != nil {
			return agentproto.SiteInstallResponse{}, err
		}
	}
	phar, err := a.downloadPinned(ctx, a.pinned(composerPhar, a.composer))
	if err != nil {
		return agentproto.SiteInstallResponse{}, fmt.Errorf("download Composer: %w", err)
	}
	dir, err := a.sites.dir(req.SiteID)
	if err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	stage := "/" + laravelStage
	if err := site.Mkdir(stage); err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	done := false
	defer func() {
		if !done {
			site.Delete(stage)
		}
	}()
	if err := copyIntoSite(site, stage+"/composer.phar", phar); err != nil {
		return agentproto.SiteInstallResponse{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, laravelTimeout)
	defer cancel()
	hostStage := filepath.Join(dir, laravelStage)
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + filepath.Join(dir, "tmp"),
		"TMPDIR=" + filepath.Join(dir, "tmp"),
		"COMPOSER_HOME=" + filepath.Join(hostStage, "composer"),
		"COMPOSER_CACHE_DIR=" + filepath.Join(hostStage, "cache"),
		"COMPOSER_NO_INTERACTION=1",
		"LANG=C.UTF-8",
	}
	// The fence of the site's PHP-FPM pool (nothing outside its folder),
	// with /dev/null, which Composer reads.
	php := func(wd string, args ...string) error {
		return a.runSitePHP(ctx, req.SiteID, req.PHPVersion, wd, env,
			append([]string{"-d", "open_basedir=" + dir + "/:/dev/null", "-d", "memory_limit=1G"}, args...))
	}
	// No scripts: Laravel's would write a SQLite .env and migrate it.
	if err := php(hostStage, filepath.Join(hostStage, "composer.phar"), "create-project", "--no-dev", "--no-scripts",
		"--no-progress", "--prefer-dist", "--no-interaction", "laravel/laravel", filepath.Join(hostStage, "app")); err != nil {
		return agentproto.SiteInstallResponse{}, fmt.Errorf("composer create-project: %w", err)
	}
	app := stage + "/app"
	example, _, err := site.Read(app+"/.env.example", 0, 64<<10)
	if err != nil {
		return agentproto.SiteInstallResponse{}, fmt.Errorf("the project has no .env.example: %w", err)
	}
	key := make([]byte, 32)
	rand.Read(key)
	env2 := laravelEnv(string(example), [][2]string{
		{"APP_ENV", "production"},
		{"APP_KEY", "base64:" + base64.StdEncoding.EncodeToString(key)},
		{"APP_DEBUG", "false"},
		{"APP_URL", req.URL},
		{"DB_CONNECTION", "mysql"},
		{"DB_HOST", "localhost"},
		{"DB_PORT", "3306"},
		{"DB_DATABASE", req.DBName},
		{"DB_USERNAME", req.DBUser},
		{"DB_PASSWORD", req.DBPassword},
	})
	if err := site.WriteFile(app+"/.env", []byte(env2)); err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	// Only PHP (the site's user) reads the key and the database password.
	if err := site.Chmod(app+"/.env", 0o600); err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	hostApp := filepath.Join(hostStage, "app")
	if err := php(hostApp, "artisan", "package:discover", "--no-interaction"); err != nil {
		return agentproto.SiteInstallResponse{}, fmt.Errorf("artisan package:discover: %w", err)
	}
	if err := php(hostApp, "artisan", "migrate", "--force", "--no-interaction"); err != nil {
		return agentproto.SiteInstallResponse{}, fmt.Errorf("artisan migrate: %w", err)
	}

	if err := laravelStorage(site, app); err != nil {
		return agentproto.SiteInstallResponse{}, err
	}

	count := 0
	if err := site.Walk(func(p string, _ bool) bool { return !strings.HasPrefix(p+"/", laravelStage+"/") }, func(e files.WalkEntry) error {
		if strings.HasPrefix(e.Path, laravelStage+"/app/") && !e.Dir {
			count++
		}
		return nil
	}); err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	if err := moveLaravel(site, app, placeholder); err != nil {
		return agentproto.SiteInstallResponse{}, err
	}
	done = true
	site.Delete(stage)
	version := ""
	if b, _, err := site.Read("/vendor/laravel/framework/src/Illuminate/Foundation/Application.php", 0, 16<<10); err == nil {
		version = phpStringVar(string(b), "const VERSION")
	}
	return agentproto.SiteInstallResponse{Version: version, Files: count}, nil
}

// moveLaravel gives the project's files their places: public/'s entries
// into the site's public folder, the rest beside it. Every name is checked
// before anything moves.
func moveLaravel(site *files.Site, app string, placeholder bool) error {
	top, err := site.List(app)
	if err != nil {
		return err
	}
	pub, err := site.List(app + "/public")
	if err != nil {
		return fmt.Errorf("the project has no public folder: %w", err)
	}
	for _, e := range top {
		if e.Name == "public" {
			continue
		}
		if _, err := site.Stat("/" + e.Name); err == nil {
			return fmt.Errorf("the site already has %s, which Laravel needs: install into a new site", e.Name)
		}
	}
	if placeholder {
		if err := site.Delete("/public/index.html"); err != nil {
			return err
		}
	}
	for _, e := range pub {
		if err := site.Rename(app+"/public/"+e.Name, "/public/"+e.Name); err != nil {
			return err
		}
	}
	for _, e := range top {
		if e.Name == "public" {
			continue
		}
		if err := site.Rename(app+"/"+e.Name, "/"+e.Name); err != nil {
			return err
		}
	}
	return nil
}

// laravelStorage does what artisan storage:link would without a link out
// of public/ (the proxy follows none): the files of the public disk move to
// public/storage and storage/app/public becomes a link to them.
func laravelStorage(site *files.Site, app string) error {
	disk, link := app+"/storage/app/public", app+"/public/storage"
	if e, err := site.Stat(disk); err != nil || e.Type != "dir" {
		return nil // a project without one
	}
	if _, err := site.Stat(link); err == nil {
		return nil
	}
	if err := site.Rename(disk, link); err != nil {
		return err
	}
	return site.Symlink("../../public/storage", disk)
}

var envKeyRE = regexp.MustCompile(`^#?\s*([A-Z][A-Z0-9_]*)=`)

// laravelEnv is .env.example with values set: a key's line (commented out
// or not) is replaced where it first appears, a key it lacks is appended.
// The values are fixed-alphabet strings.
func laravelEnv(example string, set [][2]string) string {
	want := map[string]string{}
	for _, kv := range set {
		want[kv[0]] = kv[1]
	}
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(example))
	for sc.Scan() {
		line := sc.Text()
		if m := envKeyRE.FindStringSubmatch(line); m != nil {
			if v, ok := want[m[1]]; ok {
				fmt.Fprintf(&b, "%s=%s\n", m[1], v)
				delete(want, m[1])
				continue
			}
		}
		b.WriteString(line + "\n")
	}
	for _, kv := range set {
		if _, ok := want[kv[0]]; ok {
			fmt.Fprintf(&b, "%s=%s\n", kv[0], kv[1])
		}
	}
	return b.String()
}

// pinned is f, or the test's override of it.
func (a *Agent) pinned(f, override pinnedFile) pinnedFile {
	if override.URL != "" {
		return override
	}
	return f
}

// downloadPinned returns the downloaded file f, checked against its
// SHA-256; a copy from an earlier download is checked and kept.
func (a *Agent) downloadPinned(ctx context.Context, f pinnedFile) (string, error) {
	dir := filepath.Join(a.dir, "downloads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, f.Name)
	if fileSHA256(path) == f.SHA256 {
		return path, nil
	}
	hc := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", f.URL, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, "pinned-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxPinned+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxPinned {
		err = fmt.Errorf("%s is too large", f.URL)
	}
	if err != nil {
		return "", err
	}
	if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return "", errors.New("the download does not match its checksum")
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// copyIntoSite writes the agent's file src to p in the site, as the site's.
func copyIntoSite(site *files.Site, p, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	u, err := site.BeginUpload(p)
	if err != nil {
		return err
	}
	buf := make([]byte, files.MaxChunk)
	var off int64
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			if werr := u.Write(off, buf[:n]); werr != nil {
				u.Abort()
				return werr
			}
			off += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			u.Abort()
			return err
		}
	}
	return u.Commit()
}
