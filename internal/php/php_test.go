package php

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRenderPool(t *testing.T) {
	h := DefaultHost("/run/basalt/php")
	conf, err := h.Render(Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "basalt-www", SiteDir: "/srv/basalt/sites/7"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[basalt-site-7]", "user = basalt-s7", "listen = /run/basalt/php/site-7.sock",
		"listen.group = basalt-www", "listen.mode = 0660",
		"php_admin_value[open_basedir] = /srv/basalt/sites/7/",
		"php_admin_value[session.save_path] = /srv/basalt/sites/7/tmp",
		"shell_exec", "pm.max_children = 10", "memory_limit] = 256M",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("pool lacks %q", want)
		}
	}
	if strings.Contains(conf, "default_socket") {
		t.Error("a database socket without one asked for")
	}
	// A copy of a site reaches its server's databases through its socket.
	conf, err = h.Render(Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "basalt-www", SiteDir: "/srv/basalt/sites/7",
		DBSocket: "/run/basalt/db/site-7.sock"})
	if err != nil || !strings.Contains(conf, "php_admin_value[mysqli.default_socket] = /run/basalt/db/site-7.sock\n") ||
		!strings.Contains(conf, "php_admin_value[pdo_mysql.default_socket] = /run/basalt/db/site-7.sock\n") {
		t.Fatalf("pool with a database socket: %v\n%s", err, conf)
	}
}

func TestRenderRejectsInjection(t *testing.T) {
	h := DefaultHost("/run/basalt/php")
	bad := []Pool{
		{SiteID: 1, Version: "8.4\nuser=root", User: "basalt-s1", SocketGroup: "g", SiteDir: "/srv/x"},
		{SiteID: 1, Version: "8.4", User: "basalt-s1\n[evil]", SocketGroup: "g", SiteDir: "/srv/x"},
		{SiteID: 1, Version: "8.4", User: "basalt-s1", SocketGroup: "g", SiteDir: "/srv/x;rm"},
		{SiteID: 1, Version: "8.4", User: "basalt-s1", SocketGroup: "g", SiteDir: "relative"},
		{SiteID: 0, Version: "8.4", User: "basalt-s1", SocketGroup: "g", SiteDir: "/srv/x"},
		{SiteID: 1, Version: "8.4", User: "basalt-s1", SocketGroup: "g", SiteDir: "/srv/x", DBSocket: "/run/x.sock\nuser = root"},
	}
	for i, p := range bad {
		if _, err := h.Render(p); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestRenderSettings(t *testing.T) {
	h := DefaultHost("/run/basalt/php")
	conf, err := h.Render(Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "basalt-www", SiteDir: "/srv/basalt/sites/7",
		Settings: Settings{MemoryMB: 512, UploadMB: 200, MaxChildren: 4, MaxExecutionTime: 120, MaxInputVars: 5000, DisplayErrors: true, Timezone: "Europe/Berlin"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"memory_limit] = 512M", "upload_max_filesize] = 200M", "post_max_size] = 200M", "pm.max_children = 4",
		"max_execution_time] = 120", "max_input_vars] = 5000", "date.timezone] = Europe/Berlin",
		"php_flag[display_errors] = on", "request_terminate_timeout = 150s",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("pool lacks %q", want)
		}
	}
	def, _ := h.Render(Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "basalt-www", SiteDir: "/srv/basalt/sites/7"})
	for _, want := range []string{"max_execution_time] = 30", "date.timezone] = UTC", "display_errors] = off", "max_input_vars] = 1000"} {
		if !strings.Contains(def, want) {
			t.Errorf("default pool lacks %q", want)
		}
	}
	for i, bad := range []Settings{
		{MemoryMB: 16}, {UploadMB: 5000}, {MaxChildren: 201}, {MaxExecutionTime: -1}, {MaxInputVars: 50},
		{Timezone: "UTC\nphp_admin_value[open_basedir] = /"}, {Timezone: "Europe/Berlin;x"}, {Timezone: "europe/berlin"},
		{CPUPercent: 5}, {CPUPercent: 2000}, {RAMMB: 10},
	} {
		if bad.Validate() == nil {
			t.Errorf("case %d valid", i)
		}
		if _, err := h.Render(Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "g", SiteDir: "/srv/x", Settings: bad}); err == nil {
			t.Errorf("case %d rendered", i)
		}
	}
	for _, ok := range []string{"UTC", "Europe/Berlin", "America/Argentina/Buenos_Aires", "Etc/GMT+3"} {
		if (Settings{Timezone: ok}).Validate() != nil {
			t.Errorf("%s refused", ok)
		}
	}
}

// TestApplyUnits: in systemd mode a site gets its own FPM configuration
// and service with its limits; the service is restarted only when needed,
// a rejected configuration is rolled back, and the site's pool leaves the
// shared master.
func TestApplyUnits(t *testing.T) {
	dir := t.TempDir()
	fpm := filepath.Join(dir, "php-fpm8.4")
	writeScript := func(exit int) {
		os.WriteFile(fpm, []byte(fmt.Sprintf("#!/bin/sh\necho 'test result' >&2\nexit %d\n", exit)), 0o755)
	}
	writeScript(0)
	etc := filepath.Join(dir, "etc")
	os.MkdirAll(filepath.Join(etc, "8.4", "fpm", "pool.d"), 0o755)
	// A pool of the site in the shared master, from before.
	shared := filepath.Join(etc, "8.4", "fpm", "pool.d", "basalt-site-3.conf")
	os.WriteFile(shared, []byte("[basalt-site-3]\n"), 0o644)
	master := exec.Command("sleep", "60")
	master.Start()
	defer master.Process.Kill()
	pidFile := filepath.Join(dir, "fpm.pid")
	os.WriteFile(pidFile, []byte(strconv.Itoa(master.Process.Pid)), 0o644)

	var calls []string
	h := &Host{EtcDir: etc, RunDir: filepath.Join(dir, "run"), UnitDir: filepath.Join(dir, "units"), ConfDir: filepath.Join(dir, "conf"),
		PIDFile: func(string) string { return pidFile }, Binary: func(string) string { return fpm }, Units: true,
		Systemctl: func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil }}
	os.MkdirAll(h.UnitDir, 0o755)
	pool := Pool{SiteID: 3, Version: "8.4", User: "basalt-s3", SocketGroup: "basalt-www", SiteDir: "/srv/basalt/sites/3",
		Settings: Settings{CPUPercent: 50, RAMMB: 512, MaxChildren: 4}}
	apply := func(p Pool) []string {
		t.Helper()
		calls = nil
		if err := h.Apply(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		return calls
	}

	if got := apply(pool); strings.Join(got, ";") != "daemon-reload;enable basalt-php-site-3.service;restart basalt-php-site-3.service" {
		t.Fatalf("first apply: %v", got)
	}
	unit, _ := os.ReadFile(filepath.Join(h.UnitDir, "basalt-php-site-3.service"))
	for _, want := range []string{"Type=notify", "ExecStart=" + fpm + " --nodaemonize --fpm-config " + filepath.Join(h.ConfDir, "site-3.conf"),
		"CPUQuota=50%", "MemoryMax=512M", "TasksMax=12", "Slice=basalt-sites.slice"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit lacks %q", want)
		}
	}
	conf, _ := os.ReadFile(filepath.Join(h.ConfDir, "site-3.conf"))
	for _, want := range []string{"[global]", "daemonize = no", "error_log = syslog", "[basalt-site-3]", "user = basalt-s3", "pm.max_children = 4"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("site configuration lacks %q", want)
		}
	}
	if _, err := os.Stat(shared); !os.IsNotExist(err) {
		t.Fatal("the shared pool stayed")
	}

	if got := apply(pool); strings.Join(got, ";") != "start basalt-php-site-3.service" {
		t.Fatalf("unchanged: %v", got)
	}
	pool.MemoryMB = 128
	if got := apply(pool); strings.Join(got, ";") != "reload-or-restart basalt-php-site-3.service" {
		t.Fatalf("pool change: %v", got)
	}
	pool.CPUPercent = 0
	if got := apply(pool); len(got) != 3 || got[2] != "restart basalt-php-site-3.service" {
		t.Fatalf("limit change: %v", got)
	}
	if unit, _ := os.ReadFile(filepath.Join(h.UnitDir, "basalt-php-site-3.service")); strings.Contains(string(unit), "CPUQuota") {
		t.Fatal("CPU limit kept")
	}

	// PHP-FPM rejects the configuration: the previous one stays.
	before, _ := os.ReadFile(filepath.Join(h.ConfDir, "site-3.conf"))
	writeScript(1)
	pool.MemoryMB = 256
	calls = nil
	if err := h.Apply(context.Background(), pool); err == nil || len(calls) != 0 {
		t.Fatalf("rejected configuration: %v %v", err, calls)
	}
	if after, _ := os.ReadFile(filepath.Join(h.ConfDir, "site-3.conf")); string(after) != string(before) {
		t.Fatal("rejected configuration not rolled back")
	}

	calls = nil
	if err := h.Remove(3); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ";") != "disable --now basalt-php-site-3.service;daemon-reload" {
		t.Fatalf("remove: %v", calls)
	}
	for _, f := range []string{filepath.Join(h.UnitDir, "basalt-php-site-3.service"), filepath.Join(h.ConfDir, "site-3.conf")} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s left", f)
		}
	}
}

// TestRestartUpdated: a site whose PHP master started before its binary or
// an extension was installed is restarted; one started after, and one not
// running, are left alone.
func TestRestartUpdated(t *testing.T) {
	dir := t.TempDir()
	h := &Host{RunDir: filepath.Join(dir, "run"), UnitDir: filepath.Join(dir, "units"), ExtensionDir: filepath.Join(dir, "ext"), Units: true}
	var calls []string
	h.Systemctl = func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil }
	for _, d := range []string{h.RunDir, h.UnitDir, filepath.Join(h.ExtensionDir, "20250925")} {
		os.MkdirAll(d, 0o755)
	}
	bin85, bin84 := filepath.Join(dir, "php-fpm8.5"), filepath.Join(dir, "php-fpm8.4")
	os.WriteFile(bin85, []byte("new"), 0o755) // installed now
	os.WriteFile(bin84, []byte("old"), 0o755)
	now := time.Now()
	site := func(id int, bin string, started time.Time) {
		os.WriteFile(filepath.Join(h.UnitDir, UnitName(int64(id))), []byte("[Service]\nType=notify\nExecStart="+bin+" --nodaemonize --fpm-config /etc/basalt/php/site-"+strconv.Itoa(id)+".conf\n"), 0o644)
		if started.IsZero() {
			return
		}
		pid := filepath.Join(h.RunDir, "site-"+strconv.Itoa(id)+".pid")
		os.WriteFile(pid, []byte("123\n"), 0o644)
		os.Chtimes(pid, started, started)
	}
	site(1, bin85, now.Add(time.Hour))  // started after the update
	site(2, bin85, now.Add(-time.Hour)) // before: runs the old binary
	site(3, bin85, time.Time{})         // stopped
	got, err := h.RestartUpdated()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int64{2}) || strings.Join(calls, ";") != "restart basalt-php-site-2.service" {
		t.Fatalf("restarted %v, calls %v", got, calls)
	}
	// An extension installed after site 1 started (php8.5-mysql…).
	calls = nil
	ext := filepath.Join(h.ExtensionDir, "20250925", "mysqli.so")
	os.WriteFile(ext, nil, 0o644)
	site(1, bin85, now.Add(-time.Minute)) // before the extension arrived
	site(2, bin85, now.Add(time.Hour))    // restarted since
	if got, _ := h.RestartUpdated(); !slices.Equal(got, []int64{1}) {
		t.Fatalf("after an extension update: %v %v", got, calls)
	}
	h.Units = false
	if got, _ := h.RestartUpdated(); got != nil {
		t.Fatalf("without units: %v", got)
	}
}

// TestChangeTimeLinux: a file put in place as dpkg does (its packaged
// modification time kept) has a change time of when it was installed.
func TestChangeTimeLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "php-fpm8.5")
	start := time.Now().Add(-time.Second)
	built := time.Now().Add(-72 * time.Hour)
	os.WriteFile(f+".dpkg-new", []byte("x"), 0o755)
	os.Chtimes(f+".dpkg-new", built, built)
	if err := os.Rename(f+".dpkg-new", f); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().After(start) || !changeTime(fi).After(start) {
		t.Fatalf("modified %v, changed %v, started %v", fi.ModTime(), changeTime(fi), start)
	}
}
