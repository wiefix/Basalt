// Package php manages per-site PHP-FPM pools on Debian/Ubuntu hosts
// (packages php<version>-fpm, layout /etc/php/<version>/fpm).
//
// Every site gets its own pool running as the site's system user, confined
// with open_basedir to the site directory, with its own temp, session and log
// paths and the dangerous process-control functions disabled.
package php

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

var versionRE = regexp.MustCompile(`^[5-9]\.[0-9]$`)

// Functions that let PHP code start processes, signal them or change
// identity. Sites that need them can use a container app instead.
var disabledFunctions = []string{
	"exec", "passthru", "shell_exec", "system", "proc_open", "popen",
	"pcntl_exec", "pcntl_fork", "pcntl_signal", "pcntl_alarm",
	"posix_kill", "posix_setuid", "posix_setgid", "posix_seteuid", "posix_setegid",
	"posix_setpgid", "posix_setsid", "posix_mkfifo", "dl",
}

// Host describes where PHP lives on this machine.
type Host struct {
	EtcDir string // /etc/php
	RunDir string // where pool sockets go, e.g. /run/basalt/php
	// PIDFile returns the master pid file for a version (default /run/php/php<v>-fpm.pid).
	PIDFile func(version string) string
	// Binary returns the php-fpm binary for a version (default /usr/sbin/php-fpm<v>).
	Binary func(version string) string
}

func DefaultHost(runDir string) *Host {
	return &Host{
		EtcDir:  "/etc/php",
		RunDir:  runDir,
		PIDFile: func(v string) string { return "/run/php/php" + v + "-fpm.pid" },
		Binary:  func(v string) string { return "/usr/sbin/php-fpm" + v },
	}
}

// Versions lists installed FPM versions, newest first.
func (h *Host) Versions() []string {
	entries, err := os.ReadDir(h.EtcDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		v := e.Name()
		if !versionRE.MatchString(v) {
			continue
		}
		if _, err := os.Stat(filepath.Join(h.EtcDir, v, "fpm", "pool.d")); err != nil {
			continue
		}
		if _, err := os.Stat(h.Binary(v)); err != nil {
			continue
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b string) int { return strings.Compare(b, a) })
	return out
}

func (h *Host) Socket(siteID int64) string {
	return filepath.Join(h.RunDir, "site-"+strconv.FormatInt(siteID, 10)+".sock")
}

func (h *Host) poolFile(version string, siteID int64) string {
	return filepath.Join(h.EtcDir, version, "fpm", "pool.d", "basalt-site-"+strconv.FormatInt(siteID, 10)+".conf")
}

// Pool is the configuration of one site's pool.
type Pool struct {
	SiteID      int64
	Version     string
	User        string // site system user (also its group)
	SocketGroup string // group allowed to connect (the proxy's group)
	SiteDir     string // /srv/basalt/sites/<id>
	MaxChildren int
	MemoryMB    int
	UploadMB    int
}

var safeValue = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// Render produces the pool file. All values are validated; nothing is
// interpolated that could add directives.
func (h *Host) Render(p Pool) (string, error) {
	if !versionRE.MatchString(p.Version) {
		return "", fmt.Errorf("invalid PHP version %q", p.Version)
	}
	for _, v := range []string{p.User, p.SocketGroup, p.SiteDir, h.Socket(p.SiteID)} {
		if !safeValue.MatchString(v) {
			return "", fmt.Errorf("unsafe value %q in PHP pool", v)
		}
	}
	if !filepath.IsAbs(p.SiteDir) || p.SiteID <= 0 {
		return "", errors.New("invalid site")
	}
	p.MaxChildren = clamp(p.MaxChildren, 1, 200, 10)
	p.MemoryMB = clamp(p.MemoryMB, 32, 8192, 256)
	p.UploadMB = clamp(p.UploadMB, 1, 4096, 64)

	d := p.SiteDir
	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "%s = %s\n", k, v) }
	fmt.Fprintf(&b, "; Managed by Basalt. Changes are overwritten.\n[basalt-site-%d]\n", p.SiteID)
	line("user", p.User)
	line("group", p.User)
	line("listen", h.Socket(p.SiteID))
	line("listen.owner", p.User)
	line("listen.group", p.SocketGroup)
	line("listen.mode", "0660")
	line("pm", "ondemand")
	line("pm.max_children", strconv.Itoa(p.MaxChildren))
	line("pm.process_idle_timeout", "30s")
	line("pm.max_requests", "1000")
	line("chdir", d+"/public")
	line("catch_workers_output", "no")
	line("security.limit_extensions", ".php")
	line("php_admin_value[open_basedir]", d+"/")
	line("php_admin_value[upload_tmp_dir]", d+"/tmp")
	line("php_admin_value[sys_temp_dir]", d+"/tmp")
	line("php_admin_value[session.save_path]", d+"/tmp")
	line("php_admin_value[error_log]", d+"/logs/php-error.log")
	line("php_admin_flag[log_errors]", "on")
	line("php_admin_value[disable_functions]", strings.Join(disabledFunctions, ","))
	line("php_admin_flag[allow_url_include]", "off")
	line("php_admin_value[memory_limit]", strconv.Itoa(p.MemoryMB)+"M")
	line("php_value[upload_max_filesize]", strconv.Itoa(p.UploadMB)+"M")
	line("php_value[post_max_size]", strconv.Itoa(p.UploadMB)+"M")
	return b.String(), nil
}

func clamp(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	return min(max(v, lo), hi)
}

// Apply installs (or replaces) the site's pool for p.Version, removes pools
// of the site under other versions, validates the configuration and reloads
// the affected FPM masters. On a failed validation the change is undone.
func (h *Host) Apply(ctx context.Context, p Pool) error {
	if !slices.Contains(h.Versions(), p.Version) {
		return fmt.Errorf("PHP %s is not installed on this server", p.Version)
	}
	conf, err := h.Render(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.RunDir, 0o755); err != nil {
		return err
	}
	path := h.poolFile(p.Version, p.SiteID)
	prev, prevErr := os.ReadFile(path)
	if prevErr == nil && string(prev) == conf {
		return nil // unchanged
	}
	if err := writeFile(path, []byte(conf)); err != nil {
		return err
	}
	if err := h.test(ctx, p.Version); err != nil {
		if prevErr == nil {
			writeFile(path, prev)
		} else {
			os.Remove(path)
		}
		return err
	}
	if err := h.reload(p.Version); err != nil {
		return err
	}
	// The site may have switched version: drop its other pools.
	for _, v := range h.Versions() {
		if v != p.Version {
			if err := h.removePool(v, p.SiteID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Remove deletes the site's pools under all versions.
func (h *Host) Remove(siteID int64) error {
	var errs []error
	for _, v := range h.Versions() {
		errs = append(errs, h.removePool(v, siteID))
	}
	return errors.Join(errs...)
}

func (h *Host) removePool(version string, siteID int64) error {
	err := os.Remove(h.poolFile(version, siteID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return h.reload(version)
}

func (h *Host) test(ctx context.Context, version string) error {
	conf := filepath.Join(h.EtcDir, version, "fpm", "php-fpm.conf")
	out, err := exec.CommandContext(ctx, h.Binary(version), "--test", "--fpm-config", conf).CombinedOutput()
	if err != nil {
		return fmt.Errorf("php-fpm %s rejected the configuration: %s", version, lastLine(string(out)))
	}
	return nil
}

// reload asks the FPM master to re-read its configuration gracefully
// (SIGUSR2), which works with and without systemd.
func (h *Host) reload(version string) error {
	b, err := os.ReadFile(h.PIDFile(version))
	if errors.Is(err, fs.ErrNotExist) {
		// Not running: start it, which also loads the new pool.
		if path, lerr := exec.LookPath("systemctl"); lerr == nil {
			unit := "php" + version + "-fpm.service"
			if out, serr := exec.Command(path, "start", unit).CombinedOutput(); serr != nil {
				return fmt.Errorf("php-fpm %s is not running and could not be started: %s", version, lastLine(string(out)))
			}
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("php-fpm %s is not running (%s)", version, h.PIDFile(version))
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return fmt.Errorf("php-fpm %s: bad pid file", version)
	}
	return syscall.Kill(pid, syscall.SIGUSR2)
}

func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
