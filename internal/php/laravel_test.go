package php

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderLaravel(t *testing.T) {
	h := DefaultHost("/run/basalt/php")
	pool := Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "basalt-www", SiteDir: "/srv/basalt/sites/7",
		Settings: Settings{CPUPercent: 50, RAMMB: 512, MemoryMB: 128, Timezone: "Europe/Berlin"}}
	units, ini, err := h.RenderLaravel(pool, Laravel{Scheduler: true, Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range units {
		names = append(names, n)
	}
	if len(units) != 4 || units["basalt-sites-laravel7.slice"] == "" || units["basalt-php-site-7-schedule.service"] == "" ||
		units["basalt-php-site-7-queue-1.service"] == "" || units["basalt-php-site-7-queue-2.service"] == "" {
		t.Fatalf("units: %v", names)
	}
	for _, want := range []string{"[Slice]", "CPUQuota=50%", "MemoryMax=512M", "TasksMax=256"} {
		if !strings.Contains(units["basalt-sites-laravel7.slice"], want) {
			t.Errorf("slice lacks %q", want)
		}
	}
	sched := units["basalt-php-site-7-schedule.service"]
	for _, want := range []string{
		"User=basalt-s7\n", "Group=basalt-s7\n", "WorkingDirectory=/srv/basalt/sites/7\n",
		"Environment=PHP_INI_SCAN_DIR=:/etc/basalt/php/site-7-cli ",
		"ExecStart=/usr/bin/php8.4 artisan schedule:work --no-interaction\n",
		"Slice=basalt-sites-laravel7.slice\n", "NoNewPrivileges=yes\n", "CapabilityBoundingSet=\n",
		"ProtectSystem=strict\n", "ReadWritePaths=/srv/basalt/sites/7\n", "ProtectHome=yes\n",
		"SystemCallFilter=@system-service\n", "Restart=always\n",
	} {
		if !strings.Contains(sched, want) {
			t.Errorf("scheduler unit lacks %q", want)
		}
	}
	if !strings.Contains(units["basalt-php-site-7-queue-2.service"], "artisan queue:work --sleep=3 --tries=3 --max-time=3600 --no-interaction\n") {
		t.Error("worker command")
	}
	for _, want := range []string{"open_basedir = /srv/basalt/sites/7/\n", "memory_limit = 128M\n", "date.timezone = Europe/Berlin\n",
		"error_log = /srv/basalt/sites/7/logs/php-error.log\n", "sys_temp_dir = /srv/basalt/sites/7/tmp\n"} {
		if !strings.Contains(ini, want) {
			t.Errorf("ini lacks %q", want)
		}
	}

	// Without limits the slice has none; a site under /home keeps its home.
	pool.Settings, pool.SiteDir = Settings{}, "/home/sites/7"
	units, _, err = h.RenderLaravel(pool, Laravel{Workers: 1})
	if err != nil || len(units) != 2 || strings.Contains(units["basalt-sites-laravel7.slice"], "CPUQuota") ||
		strings.Contains(units["basalt-php-site-7-queue-1.service"], "ProtectHome") {
		t.Fatalf("no limits: %v %v", err, units)
	}
	// Values that could add directives, and too many workers, are refused.
	for i, c := range []struct {
		p Pool
		l Laravel
	}{
		{Pool{SiteID: 7, Version: "8.4", User: "basalt-s7\nUser=root", SocketGroup: "g", SiteDir: "/srv/x"}, Laravel{Scheduler: true}},
		{Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "g", SiteDir: "/srv/x\nUser=root"}, Laravel{Scheduler: true}},
		{Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "g", SiteDir: "/srv/x"}, Laravel{Workers: MaxLaravelWorkers + 1}},
		{Pool{SiteID: 7, Version: "8.4", User: "basalt-s7", SocketGroup: "g", SiteDir: "/srv/x"}, Laravel{Workers: -1}},
	} {
		if _, _, err := h.RenderLaravel(c.p, c.l); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestApplyLaravel(t *testing.T) {
	dir := t.TempDir()
	etc := filepath.Join(dir, "etc")
	os.MkdirAll(filepath.Join(etc, "8.4", "fpm", "pool.d"), 0o755)
	fpm := filepath.Join(dir, "php-fpm8.4")
	os.WriteFile(fpm, []byte("#!/bin/sh\n"), 0o755)
	var calls []string
	h := &Host{EtcDir: etc, RunDir: filepath.Join(dir, "run"), UnitDir: filepath.Join(dir, "units"), ConfDir: filepath.Join(dir, "conf"),
		Binary: func(string) string { return fpm }, Units: true,
		Systemctl: func(args ...string) error { calls = append(calls, strings.Join(args, " ")); return nil }}
	os.MkdirAll(h.UnitDir, 0o755)
	site := filepath.Join(dir, "sites", "3")
	os.MkdirAll(site, 0o755)
	pool := Pool{SiteID: 3, Version: "8.4", User: "basalt-s3", SocketGroup: "basalt-www", SiteDir: site}
	apply := func(p Pool, l Laravel) string {
		t.Helper()
		calls = nil
		if err := h.ApplyLaravel(context.Background(), p, l); err != nil {
			t.Fatal(err)
		}
		return strings.Join(calls, ";")
	}

	// Not a Laravel project.
	if err := h.ApplyLaravel(context.Background(), pool, Laravel{Scheduler: true}); !errors.Is(err, ErrNoArtisan) {
		t.Fatalf("no artisan: %v", err)
	}
	os.WriteFile(filepath.Join(site, "artisan"), []byte("<?php\n"), 0o644)

	if got := apply(pool, Laravel{Scheduler: true, Workers: 1}); got != "daemon-reload;"+
		"enable basalt-php-site-3-queue-1.service;restart basalt-php-site-3-queue-1.service;"+
		"enable basalt-php-site-3-schedule.service;restart basalt-php-site-3-schedule.service" {
		t.Fatalf("first apply: %s", got)
	}
	if _, err := os.Stat(filepath.Join(h.ConfDir, "site-3-cli", "basalt.ini")); err != nil {
		t.Fatal(err)
	}
	if got := apply(pool, Laravel{Scheduler: true, Workers: 1}); got != "start basalt-php-site-3-queue-1.service;start basalt-php-site-3-schedule.service" {
		t.Fatalf("unchanged: %s", got)
	}
	// A new worker starts; the others go on.
	if got := apply(pool, Laravel{Scheduler: true, Workers: 2}); got != "daemon-reload;"+
		"enable basalt-php-site-3-queue-2.service;restart basalt-php-site-3-queue-2.service;"+
		"start basalt-php-site-3-queue-1.service;start basalt-php-site-3-schedule.service" {
		t.Fatalf("one more worker: %s", got)
	}
	// New limits restart everything.
	pool.Settings.RAMMB = 256
	if got := apply(pool, Laravel{Scheduler: true, Workers: 2}); got != "daemon-reload;"+
		"enable basalt-php-site-3-queue-1.service;restart basalt-php-site-3-queue-1.service;"+
		"enable basalt-php-site-3-queue-2.service;restart basalt-php-site-3-queue-2.service;"+
		"enable basalt-php-site-3-schedule.service;restart basalt-php-site-3-schedule.service" {
		t.Fatalf("limits: %s", got)
	}
	// The scheduler and a worker no longer wanted go.
	if got := apply(pool, Laravel{Workers: 1}); got != "disable --now basalt-php-site-3-queue-2.service;"+
		"disable --now basalt-php-site-3-schedule.service;daemon-reload;start basalt-php-site-3-queue-1.service" {
		t.Fatalf("fewer: %s", got)
	}
	if _, err := os.Stat(filepath.Join(h.UnitDir, "basalt-php-site-3-schedule.service")); !os.IsNotExist(err) {
		t.Fatal("scheduler unit left")
	}

	// Removing the site's PHP removes them all.
	calls = nil
	if err := h.Remove(3); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ";"); got != "disable --now basalt-php-site-3-queue-1.service;daemon-reload" {
		t.Fatalf("remove: %s", got)
	}
	left, _ := os.ReadDir(h.UnitDir)
	if len(left) != 0 {
		t.Fatalf("units left: %v", left)
	}
	if _, err := os.Stat(filepath.Join(h.ConfDir, "site-3-cli")); !os.IsNotExist(err) {
		t.Fatal("ini left")
	}

	// Without systemd units only "none" is possible.
	h.Units = false
	if err := h.ApplyLaravel(context.Background(), pool, Laravel{Workers: 1}); err == nil {
		t.Fatal("applied without systemd")
	}
	if err := h.ApplyLaravel(context.Background(), pool, Laravel{}); err != nil {
		t.Fatal(err)
	}
}
