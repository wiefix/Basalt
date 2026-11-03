package caddyrun

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
)

// onlyOpen hides Stat, like Caddy's wrapper around registered file systems:
// fs.Stat then opens the file.
type onlyOpen struct{ fs.FS }

func TestRootFSUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files regardless of mode")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "site")
	os.MkdirAll(filepath.Join(root, "locked"), 0o755)
	os.WriteFile(filepath.Join(root, "script.php"), []byte("<?php secret"), 0o644)
	os.WriteFile(filepath.Join(dir, "outside.php"), []byte("outside"), 0o644)
	os.Symlink(filepath.Join(dir, "outside.php"), filepath.Join(root, "escape.php"))
	os.Chmod(filepath.Join(root, "script.php"), 0o000)
	os.Chmod(filepath.Join(dir, "outside.php"), 0o000)
	os.Chmod(filepath.Join(root, "locked"), 0o311) // not listable
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "locked"), 0o755) })

	open := func(statUnreadable bool) fs.FS {
		f := &RootFS{Root: root, StatUnreadable: statUnreadable}
		if err := f.Provision(caddy.Context{}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Cleanup() })
		return onlyOpen{f}
	}

	strict := open(false)
	if _, err := fs.Stat(strict, "script.php"); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("strict: stat of unreadable file: %v, want permission error", err)
	}

	match := open(true)
	info, err := fs.Stat(match, "/script.php")
	if err != nil || info.IsDir() || info.Name() != "script.php" {
		t.Fatalf("stat_unreadable: stat = %v, %v", info, err)
	}
	f, err := match.Open("script.php")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(f); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("read of unreadable file: %v, want permission error", err)
	}
	f.Close()
	if info, err := fs.Stat(match, "locked"); err != nil || !info.IsDir() {
		t.Fatalf("unlistable directory: %v, %v", info, err)
	}
	if _, err := fs.ReadDir(match, "locked"); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("readdir of unlistable directory: %v, want permission error", err)
	}

	// Still confined: a symlink out of the root and missing files stay errors,
	// even when the target exists and is unreadable.
	if _, err := fs.Stat(match, "escape.php"); err == nil {
		t.Fatal("symlink out of the root resolved")
	}
	if _, err := fs.Stat(match, "missing.php"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := fs.Stat(match, "../outside.php"); err == nil {
		t.Fatal("parent path resolved")
	}
}

// TestRootFSReplaced: a folder replaced under the proxy (a restore swaps in
// a new one) is served within a second; a symlink put in its place to
// another site's folder is not followed.
func TestRootFSReplaced(t *testing.T) {
	dir := t.TempDir()
	site := filepath.Join(dir, "site")
	public := filepath.Join(site, "public")
	write := func(d, s string) {
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "index.html"), []byte(s), 0o644)
	}
	write(public, "v1")
	write(filepath.Join(dir, "other"), "another site")
	f := &RootFS{Root: public}
	if err := f.Provision(caddy.Context{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Cleanup() })
	read := func() string {
		b, err := fs.ReadFile(f, "index.html")
		if err != nil {
			return err.Error()
		}
		return string(b)
	}
	if got := read(); got != "v1" {
		t.Fatal(got)
	}

	write(filepath.Join(site, "new"), "v2")
	os.Rename(public, filepath.Join(site, "old"))
	os.Rename(filepath.Join(site, "new"), public)
	os.RemoveAll(filepath.Join(site, "old"))
	time.Sleep(recheck + 100*time.Millisecond)
	if got := read(); got != "v2" {
		t.Fatalf("after the folder was replaced: %q", got)
	}

	for _, target := range []string{filepath.Join(dir, "other"), "../other"} {
		os.RemoveAll(public)
		os.Symlink(target, public)
		time.Sleep(recheck + 100*time.Millisecond)
		if got := read(); got == "another site" {
			t.Fatalf("followed a symlink to %s", target)
		}
	}
}
