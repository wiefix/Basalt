package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestHostPathsScrub(t *testing.T) {
	h := newHostPaths(Config{
		DataDir:   "/var/lib/basalt",
		SitesRoot: "/var/lib/basalt/sites",
		ProxyDir:  "/var/lib/basalt-proxy",
	})
	pathErr := &fs.PathError{Op: "open", Path: "/var/lib/basalt/sites/12/public/index.php", Err: fs.ErrPermission}
	for _, tc := range []struct{ in, want string }{
		{fmt.Errorf("write file: %w", pathErr).Error(), "write file: open <site>/public/index.php: permission denied"},
		{"stat /var/lib/basalt/sites/7: no such file", "stat <site>: no such file"},
		{"mkdir /var/lib/basalt/sites: exists", "mkdir <sites>: exists"},
		{"read /var/lib/basalt/agent.json and /var/lib/basalt-proxy/config.json",
			"read <agent-data>/agent.json and <proxy-data>/config.json"},
		{"/var/lib/basaltx is someone else's", "/var/lib/basaltx is someone else's"},
		{"php-fpm 8.4 is not running", "php-fpm 8.4 is not running"},
	} {
		if got := h.scrub(errors.New(tc.in)).Error(); got != tc.want {
			t.Errorf("scrub(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Errors without host paths keep their identity (errors.Is still works
	// inside the agent), nil stays nil.
	if err := h.scrub(fs.ErrNotExist); err != fs.ErrNotExist {
		t.Errorf("scrub changed an error without paths: %v", err)
	}
	if h.scrub(nil) != nil {
		t.Error("scrub(nil) != nil")
	}
	// Relative directories (none configured) are skipped.
	if len(newHostPaths(Config{DataDir: "data"})) != 0 {
		t.Error("relative directory used")
	}
}

// TestOwnDirSymlink: a site's user who swaps one of its folders for a
// symlink does not get the agent to change the target.
func TestOwnDirSymlink(t *testing.T) {
	site, outside := t.TempDir(), t.TempDir()
	if err := os.Chmod(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(site, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(site, "tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("logs", filepath.Join(site, "public")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(site)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := ownDir(r, "tmp", -1, -1, 0o700); err == nil {
		t.Error("followed a symlink out of the site")
	}
	if err := ownDir(r, "file", -1, -1, 0o700); err == nil {
		t.Error("changed a file as a folder")
	}
	if err := ownDir(r, "logs", -1, -1, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ownDir(r, "public", -1, -1, 0o750); err == nil {
		t.Error("changed a folder through a symlink")
	}
	for dir, want := range map[string]fs.FileMode{outside: 0o755, filepath.Join(site, "logs"): 0o700} {
		if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v, %v; want %v", dir, fi.Mode().Perm(), err, want)
		}
	}
}
