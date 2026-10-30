package php

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// MaxLaravelWorkers bounds a site's queue workers.
const MaxLaravelWorkers = 8

// Laravel are the background processes of a site that is a Laravel
// project: its scheduler (artisan schedule:work, which runs schedule:run
// every minute) and queue workers (artisan queue:work). Zero: none.
type Laravel struct {
	Scheduler bool `json:"scheduler"`
	Workers   int  `json:"workers"` // 0–MaxLaravelWorkers
}

func (l Laravel) Validate() error {
	if l.Workers < 0 || l.Workers > MaxLaravelWorkers {
		return fmt.Errorf("queue workers must be 0–%d", MaxLaravelWorkers)
	}
	return nil
}

// IsZero reports whether no background process is wanted.
func (l Laravel) IsZero() bool { return !l.Scheduler && l.Workers == 0 }

// laravelPrefix starts the names of a site's Laravel services; they share
// the slice laravelSlice, which carries the site's CPU and memory limits.
func laravelPrefix(siteID int64) string {
	return "basalt-php-site-" + strconv.FormatInt(siteID, 10) + "-"
}

func laravelSlice(siteID int64) string {
	return "basalt-sites-laravel" + strconv.FormatInt(siteID, 10) + ".slice"
}

// laravelINIDir holds the ini file every PHP CLI process of the site's
// background processes reads (PHP_INI_SCAN_DIR is inherited by the PHP
// processes they start, such as the scheduler's commands).
func (h *Host) laravelINIDir(siteID int64) string {
	return filepath.Join(h.ConfDir, "site-"+strconv.FormatInt(siteID, 10)+"-cli")
}

// RenderLaravel produces the site's Laravel units (name → content) and the
// ini file of its PHP CLI. Each process runs as the site's user in a
// systemd sandbox: nothing but the site's folder is writable, no
// capabilities, no devices, the services' system calls only.
func (h *Host) RenderLaravel(p Pool, l Laravel) (units map[string]string, ini string, err error) {
	if err := l.Validate(); err != nil {
		return nil, "", err
	}
	if _, err := h.Render(p); err != nil { // the same checks of every value
		return nil, "", err
	}
	id := strconv.FormatInt(p.SiteID, 10)
	d := p.SiteDir
	iniDir := h.laravelINIDir(p.SiteID)
	if !safeValue.MatchString(iniDir) {
		return nil, "", fmt.Errorf("unsafe value %q in PHP configuration", iniDir)
	}
	st := p.Settings
	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "%s = %s\n", k, v) }
	b.WriteString("; Managed by Basalt. Changes are overwritten.\n")
	line("open_basedir", d+"/")
	line("sys_temp_dir", d+"/tmp")
	line("upload_tmp_dir", d+"/tmp")
	line("session.save_path", d+"/tmp")
	line("error_log", d+"/logs/php-error.log")
	line("log_errors", "On")
	line("allow_url_include", "Off")
	line("memory_limit", strconv.Itoa(clamp(st.MemoryMB, 32, 8192, 256))+"M")
	tz := st.Timezone
	if tz == "" {
		tz = "UTC"
	}
	line("date.timezone", tz)
	if p.DBSocket != "" {
		line("mysqli.default_socket", p.DBSocket)
		line("pdo_mysql.default_socket", p.DBSocket)
	}
	ini = b.String()

	units = map[string]string{}
	var s strings.Builder
	fmt.Fprintf(&s, "# Managed by Basalt. Changes are overwritten.\n[Unit]\nDescription=Laravel processes of Basalt site %s\n\n[Slice]\n", id)
	if st.CPUPercent > 0 {
		fmt.Fprintf(&s, "CPUQuota=%d%%\n", st.CPUPercent)
	}
	if st.RAMMB > 0 {
		fmt.Fprintf(&s, "MemoryMax=%dM\nMemorySwapMax=0\n", st.RAMMB)
	}
	s.WriteString("TasksMax=256\n")
	units[laravelSlice(p.SiteID)] = s.String()

	service := func(desc, ident, args string) string {
		var u strings.Builder
		fmt.Fprintf(&u, "# Managed by Basalt. Changes are overwritten.\n[Unit]\nDescription=%s for Basalt site %s\n", desc, id)
		u.WriteString("After=network.target mariadb.service\n\n[Service]\n")
		fmt.Fprintf(&u, "User=%s\nGroup=%s\nWorkingDirectory=%s\n", p.User, p.User, d)
		fmt.Fprintf(&u, "Environment=PHP_INI_SCAN_DIR=:%s HOME=%s/tmp TMPDIR=%s/tmp LANG=C.UTF-8\n", iniDir, d, d)
		fmt.Fprintf(&u, "ExecStart=%s artisan %s --no-interaction\n", h.cli(p.Version), args)
		u.WriteString("Restart=always\nRestartSec=5\n")
		fmt.Fprintf(&u, "Slice=%s\nSyslogIdentifier=basalt-site-%s-%s\n", laravelSlice(p.SiteID), id, ident)
		u.WriteString("NoNewPrivileges=yes\nCapabilityBoundingSet=\nPrivateTmp=yes\nPrivateDevices=yes\n")
		u.WriteString("ProtectSystem=strict\n")
		fmt.Fprintf(&u, "ReadWritePaths=%s\n", d)
		if !underHome(d) {
			u.WriteString("ProtectHome=yes\n")
		}
		u.WriteString("ProtectKernelTunables=yes\nProtectKernelModules=yes\nProtectKernelLogs=yes\nProtectControlGroups=yes\n")
		u.WriteString("ProtectClock=yes\nProtectHostname=yes\nProtectProc=invisible\nRestrictNamespaces=yes\nRestrictRealtime=yes\n")
		u.WriteString("LockPersonality=yes\nRestrictAddressFamilies=AF_UNIX AF_INET AF_INET6\n")
		u.WriteString("SystemCallArchitectures=native\nSystemCallFilter=@system-service\nSystemCallErrorNumber=EPERM\n")
		u.WriteString("\n[Install]\nWantedBy=multi-user.target\n")
		return u.String()
	}
	if l.Scheduler {
		units[laravelPrefix(p.SiteID)+"schedule.service"] = service("Laravel scheduler", "schedule", "schedule:work")
	}
	for i := 1; i <= l.Workers; i++ {
		n := strconv.Itoa(i)
		// Workers end every hour (and start again) to pick up new code.
		units[laravelPrefix(p.SiteID)+"queue-"+n+".service"] = service("Laravel queue worker "+n, "queue-"+n,
			"queue:work --sleep=3 --tries=3 --max-time=3600")
	}
	return units, ini, nil
}

func underHome(d string) bool {
	for _, h := range []string{"/home/", "/root/", "/run/user/"} {
		if strings.HasPrefix(d+"/", h) {
			return true
		}
	}
	return false
}

func (h *Host) cli(version string) string {
	if h.CLI != nil {
		return h.CLI(version)
	}
	return "/usr/bin/php" + version
}

// ErrNoArtisan: background processes were asked for a site that is not a
// Laravel project.
var ErrNoArtisan = errors.New("the site has no artisan file in its folder: Laravel processes need a Laravel project")

// ApplyLaravel makes the site's Laravel processes those of l: changed
// units are written and their services restarted, services no longer
// wanted are stopped and removed. Zero l removes everything. It needs each
// site's PHP to run as systemd services (Units).
func (h *Host) ApplyLaravel(ctx context.Context, p Pool, l Laravel) error {
	if l.IsZero() {
		return h.removeLaravel(p.SiteID)
	}
	if !h.Units {
		return errors.New("Laravel processes need systemd on the server")
	}
	if fi, err := os.Lstat(filepath.Join(p.SiteDir, "artisan")); err != nil || !fi.Mode().IsRegular() {
		h.removeLaravel(p.SiteID)
		return ErrNoArtisan
	}
	if !slices.Contains(h.Versions(), p.Version) {
		return fmt.Errorf("PHP %s is not installed on this server", p.Version)
	}
	units, ini, err := h.RenderLaravel(p, l)
	if err != nil {
		return err
	}
	iniDir := h.laravelINIDir(p.SiteID)
	if err := os.MkdirAll(iniDir, 0o755); err != nil {
		return err
	}
	iniPath := filepath.Join(iniDir, "basalt.ini")
	prevINI, _ := os.ReadFile(iniPath)
	iniChanged := string(prevINI) != ini
	if iniChanged {
		if err := writeFile(iniPath, []byte(ini)); err != nil {
			return err
		}
	}

	existing, err := h.laravelUnits(p.SiteID)
	if err != nil {
		return err
	}
	reload := false
	var stale, changed, same []string
	for _, name := range existing {
		if _, ok := units[name]; !ok {
			stale = append(stale, name)
		}
	}
	names := make([]string, 0, len(units))
	for name := range units {
		names = append(names, name)
	}
	slices.Sort(names)
	sliceChanged := false
	for _, name := range names {
		path := filepath.Join(h.UnitDir, name)
		prev, err := os.ReadFile(path)
		if err == nil && string(prev) == units[name] {
			if strings.HasSuffix(name, ".service") {
				same = append(same, name)
			}
			continue
		}
		if err := writeFile(path, []byte(units[name])); err != nil {
			return err
		}
		reload = true
		if strings.HasSuffix(name, ".slice") {
			sliceChanged = true
		} else {
			changed = append(changed, name)
		}
	}
	var errs []error
	for _, name := range stale {
		errs = append(errs, h.Systemctl("disable", "--now", name))
		errs = append(errs, os.Remove(filepath.Join(h.UnitDir, name)))
		reload = true
	}
	if reload {
		if err := h.Systemctl("daemon-reload"); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	// The PHP configuration and the limits are taken when a process starts.
	if iniChanged || sliceChanged {
		changed, same = append(changed, same...), nil
		slices.Sort(changed)
	}
	for _, name := range changed {
		errs = append(errs, h.Systemctl("enable", name), h.Systemctl("restart", name))
	}
	for _, name := range same {
		errs = append(errs, h.Systemctl("start", name)) // e.g. stopped by hand
	}
	return errors.Join(errs...)
}

// laravelUnits lists the site's Laravel unit files (services and slice).
func (h *Host) laravelUnits(siteID int64) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(h.UnitDir, laravelPrefix(siteID)+"*.service"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	if _, err := os.Stat(filepath.Join(h.UnitDir, laravelSlice(siteID))); err == nil {
		out = append(out, laravelSlice(siteID))
	}
	return out, nil
}

// removeLaravel stops and removes the site's Laravel processes.
func (h *Host) removeLaravel(siteID int64) error {
	if h.UnitDir == "" || h.Systemctl == nil {
		return nil
	}
	units, err := h.laravelUnits(siteID)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range units {
		if strings.HasSuffix(name, ".service") {
			errs = append(errs, h.Systemctl("disable", "--now", name))
		}
		errs = append(errs, os.Remove(filepath.Join(h.UnitDir, name)))
	}
	if len(units) > 0 {
		errs = append(errs, h.Systemctl("daemon-reload"))
	}
	if h.ConfDir != "" {
		if err := os.RemoveAll(h.laravelINIDir(siteID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
