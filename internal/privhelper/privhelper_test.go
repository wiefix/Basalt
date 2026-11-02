package privhelper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/php"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// socketPath is short enough for a unix socket (t.TempDir is not, on macOS).
func socketPath(t *testing.T) string {
	dir, err := os.MkdirTemp("", "ph")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "h.sock")
}

// fake answers like a helper, without changing the host.
func fake(calls *atomic.Int32) func(context.Context, string, json.RawMessage) (any, error) {
	return func(_ context.Context, msgType string, payload json.RawMessage) (any, error) {
		calls.Add(1)
		var req SiteRequest
		json.Unmarshal(payload, &req)
		switch msgType {
		case MsgSiteUserEnsure:
			return SiteUser{UID: int(req.SiteID) * 10, GID: int(req.SiteID) * 10}, nil
		case MsgSiteUserDelete:
			return nil, errors.New("userdel: user is busy")
		}
		return struct{}{}, nil
	}
}

func serve(t *testing.T, path string, uid int, calls *atomic.Int32) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// Closed when Serve returns: stopping more than once is fine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := Serve(ctx, path, uid, fake(calls), quiet); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fi, err := os.Lstat(path); err == nil && fi.Mode().Perm() == 0o600 {
			return func() {
				cancel()
				<-done
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper did not listen")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClientServer(t *testing.T) {
	path := socketPath(t)
	var calls atomic.Int32
	serve(t, path, os.Geteuid(), &calls)
	c := NewClient(path)
	defer c.Close()
	ctx := context.Background()

	uid, gid, err := c.EnsureSiteUser(ctx, 7)
	if err != nil || uid != 70 || gid != 70 {
		t.Fatalf("ensure: %d %d %v", uid, gid, err)
	}
	if err := c.HostIdentity(ctx); err != nil {
		t.Fatal(err)
	}
	// The helper's own words reach the agent, not wrapped as remote.
	if err := c.DeleteSiteUser(ctx, 7); err == nil || err.Error() != "userdel: user is busy" {
		t.Fatalf("delete: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("%d calls, want 3 (a refused request is not repeated)", calls.Load())
	}
}

func TestClientReconnects(t *testing.T) {
	path := socketPath(t)
	var calls atomic.Int32
	stop := serve(t, path, os.Geteuid(), &calls)
	c := NewClient(path)
	defer c.Close()
	if _, _, err := c.EnsureSiteUser(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	// The helper restarts (an update): the next call finds the connection
	// broken, connects again and goes through.
	stop()
	serve(t, path, os.Geteuid(), &calls)
	if uid, _, err := c.EnsureSiteUser(context.Background(), 2); err != nil || uid != 20 {
		t.Fatalf("after restart: %d %v", uid, err)
	}
}

func TestServerRefusesOtherUsers(t *testing.T) {
	path := socketPath(t)
	var calls atomic.Int32
	// The agent is someone else: this process gets no answers.
	serve(t, path, os.Geteuid()+1, &calls)
	c := NewClient(path)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.EnsureSiteUser(ctx, 1); err == nil {
		t.Fatal("a process of another user got an answer")
	}
	if calls.Load() != 0 {
		t.Fatalf("%d requests served", calls.Load())
	}
}

func TestServeSocketFile(t *testing.T) {
	path := socketPath(t)
	// Not a socket: never removed.
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), path, os.Geteuid(), fake(new(atomic.Int32)), quiet); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("serve over a file: %v", err)
	}
	os.Remove(path)
	// A socket left by a helper that was killed is replaced.
	var calls atomic.Int32
	stop := serve(t, path, os.Geteuid(), &calls)
	stop()
	serve(t, path, os.Geteuid(), &calls)
	if _, _, err := NewClient(path).EnsureSiteUser(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
}

func TestHelperChecksRequests(t *testing.T) {
	if _, err := New(Config{SitesRoot: "sites"}, quiet); err == nil {
		t.Error("relative sites root accepted")
	}
	if _, err := New(Config{SitesRoot: "/srv/sites", WWWGroup: "www-data"}, quiet); err == nil {
		t.Error("a group outside basalt- accepted")
	}
	h, err := New(Config{SitesRoot: "/srv/sites"}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{`{"site_id":0}`, `{"site_id":-3}`} {
		for _, msg := range []string{MsgSiteUserEnsure, MsgSiteUserDelete} {
			if _, err := h.Handle(ctx, msg, json.RawMessage(id)); !errors.Is(err, errSiteID) {
				t.Errorf("%s %s: %v", msg, id, err)
			}
		}
	}
	if _, err := h.Handle(ctx, "run", json.RawMessage(`{"cmd":"id"}`)); err == nil || !strings.Contains(err.Error(), "unknown request") {
		t.Errorf("unknown request: %v", err)
	}
}

// phpHelper is a helper whose PHP is in a temporary folder: PHP 8.4 with
// each site as a systemd service; systemctl calls are recorded.
func phpHelper(t *testing.T) (*Helper, *php.Host, *[]string) {
	t.Helper()
	dir := t.TempDir()
	etc := filepath.Join(dir, "etc")
	os.MkdirAll(filepath.Join(etc, "8.4", "fpm", "pool.d"), 0o755)
	fpm := filepath.Join(dir, "php-fpm8.4")
	os.WriteFile(fpm, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	var calls []string
	host := &php.Host{EtcDir: etc, RunDir: filepath.Join(dir, "run"), UnitDir: filepath.Join(dir, "units"), ConfDir: filepath.Join(dir, "conf"),
		PIDFile: func(string) string { return filepath.Join(dir, "none.pid") }, Binary: func(string) string { return fpm }, Units: true,
		Systemctl: func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil }}
	os.MkdirAll(host.UnitDir, 0o755)
	h, err := New(Config{SitesRoot: "/srv/sites", SiteDBDir: "/run/db", MariaDBConf: filepath.Join(dir, "mysql", "90-basalt.cnf"), PHP: host}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return h, host, &calls
}

func handle(t *testing.T, h *Helper, msgType string, req any) error {
	t.Helper()
	b, _ := json.Marshal(req)
	_, err := h.Handle(context.Background(), msgType, b)
	return err
}

// TestSitePHP: the agent names a site and its settings; the user, folders
// and sockets come from the helper's configuration.
func TestSitePHP(t *testing.T) {
	h, host, calls := phpHelper(t)
	err := handle(t, h, MsgSitePHPApply, SitePHP{SiteID: 7, Version: "8.4", Settings: php.Settings{MemoryMB: 128}, DBSocket: true})
	if err != nil {
		t.Fatal(err)
	}
	conf, _ := os.ReadFile(filepath.Join(host.ConfDir, "site-7.conf"))
	for _, want := range []string{"user = basalt-s7", "listen.group = basalt-www", "listen = " + filepath.Join(host.RunDir, "site-7.sock"),
		"php_admin_value[open_basedir] = /srv/sites/7/", "php_admin_value[pdo_mysql.default_socket] = /run/db/site-7.sock",
		"php_admin_value[memory_limit] = 128M"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("configuration lacks %q:\n%s", want, conf)
		}
	}
	if got := strings.Join(*calls, ";"); got != "daemon-reload;enable basalt-php-site-7.service;restart basalt-php-site-7.service" {
		t.Errorf("systemctl calls: %s", got)
	}
	// Without the database socket, the server's own MariaDB.
	if err := handle(t, h, MsgSitePHPApply, SitePHP{SiteID: 7, Version: "8.4"}); err != nil {
		t.Fatal(err)
	}
	if conf, _ := os.ReadFile(filepath.Join(host.ConfDir, "site-7.conf")); strings.Contains(string(conf), "default_socket") {
		t.Error("database socket kept")
	}

	for _, bad := range []SitePHP{{SiteID: 0, Version: "8.4"}, {SiteID: -1, Version: "8.4"}, {SiteID: 8, Version: "8.4\nuser = root"},
		{SiteID: 8, Version: "7.0"}, {SiteID: 8, Version: "8.4", Settings: php.Settings{Timezone: "UTC\nopen_basedir = /"}}} {
		if err := handle(t, h, MsgSitePHPApply, bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(host.ConfDir, "site-8.conf")); err == nil {
		t.Error("a rejected site's configuration was written")
	}

	if err := handle(t, h, MsgSitePHPRemove, SiteRequest{SiteID: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(host.UnitDir, "basalt-php-site-7.service")); err == nil {
		t.Error("the site's service is still there")
	}
	if err := handle(t, h, MsgSitePHPRemove, SiteRequest{}); err == nil {
		t.Error("site 0 removed")
	}
}

// TestSiteLaravel: processes need the site's artisan, in the folder the
// helper knows; unknown operations are refused.
func TestSiteLaravel(t *testing.T) {
	h, _, _ := phpHelper(t)
	err := handle(t, h, MsgSiteLaravelApply, SiteLaravel{SitePHP: SitePHP{SiteID: 7, Version: "8.4"}, Laravel: php.Laravel{Scheduler: true}})
	if !errors.Is(err, php.ErrNoArtisan) {
		t.Errorf("Laravel without artisan: %v", err)
	}
	if err := handle(t, h, MsgSiteLaravelApply, SiteLaravel{SitePHP: SitePHP{SiteID: 7, Version: "8.4"}, Laravel: php.Laravel{Workers: 9}}); err == nil {
		t.Error("9 workers accepted")
	}
	if err := handle(t, h, MsgSiteLaravel, agentproto.SiteLaravel{SiteID: 7, Op: "rm"}); err == nil {
		t.Error("unknown operation accepted")
	}
	if err := handle(t, h, MsgSiteLaravel, agentproto.SiteLaravel{SiteID: 0, Op: agentproto.LaravelStatus}); err == nil {
		t.Error("site 0 accepted")
	}
}

func TestMariaDBConfig(t *testing.T) {
	h, _, _ := phpHelper(t)
	cfg := MariaDBConfig{ServerID: 3, Filters: map[string][]string{"basalt_2": {"s1_wp"}}}
	if err := handle(t, h, MsgMariaDBConfig, cfg); err == nil {
		t.Error("written without MariaDB's configuration folder")
	}
	os.MkdirAll(filepath.Dir(h.cfg.MariaDBConf), 0o755)
	if err := handle(t, h, MsgMariaDBConfig, cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(h.cfg.MariaDBConf)
	if !strings.Contains(string(b), "server_id = 3\n") || !strings.Contains(string(b), `basalt_2.replicate_wild_do_table = s1\\_wp.%`) {
		t.Errorf("option file:\n%s", b)
	}
	for _, bad := range []MariaDBConfig{{ServerID: 0}, {ServerID: 1 << 33},
		{ServerID: 3, Filters: map[string][]string{"basalt_2\nlog_bin": nil}},
		{ServerID: 3, Filters: map[string][]string{"basalt_2": {"s1_wp\ninit_file = /tmp/x"}}}} {
		if err := handle(t, h, MsgMariaDBConfig, bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if b2, _ := os.ReadFile(h.cfg.MariaDBConf); string(b2) != string(b) {
		t.Error("a rejected configuration was written")
	}
}

// TestClientPHP: the requests and answers make it through the socket.
func TestClientPHP(t *testing.T) {
	h, host, _ := phpHelper(t)
	path := socketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); Serve(ctx, path, os.Geteuid(), h.Handle, quiet) }()
	defer func() { cancel(); <-done }()
	c := NewClient(path)
	defer c.Close()
	if err := c.ApplySitePHP(ctx, SitePHP{SiteID: 5, Version: "8.4", Settings: php.Settings{UploadMB: 32}}); err != nil {
		t.Fatal(err)
	}
	if conf, _ := os.ReadFile(filepath.Join(host.ConfDir, "site-5.conf")); !strings.Contains(string(conf), "upload_max_filesize] = 32M") {
		t.Errorf("configuration:\n%s", conf)
	}
	ids, err := c.RestartUpdatedPHP(ctx)
	if err != nil || ids == nil {
		t.Errorf("restart updated: %v %v", ids, err)
	}
	resp, err := c.LaravelProcesses(ctx, agentproto.SiteLaravel{SiteID: 5, Op: agentproto.LaravelStatus})
	if err != nil || len(resp.Processes) != 0 {
		t.Errorf("status: %+v %v", resp, err)
	}
	if err := c.ApplySitePHP(ctx, SitePHP{SiteID: 5, Version: "9.9"}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("unknown version: %v", err)
	}
}
