package privhelper

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

func TestTrash(t *testing.T) {
	root := t.TempDir()
	m, err := New(Config{SitesRoot: root}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	trash := filepath.Join(root, trashDir)
	now := time.Now()
	old := "4-" + strconv.FormatInt(now.Add(-31*24*time.Hour).Unix(), 10)
	recent := "5-" + strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)
	for _, n := range []string{old, recent, "notes"} {
		os.MkdirAll(filepath.Join(trash, n, "public"), 0o755)
		os.WriteFile(filepath.Join(trash, n, "public", "index.html"), make([]byte, 5000), 0o644)
	}
	outside := filepath.Join(t.TempDir(), "keep.txt")
	os.WriteFile(outside, []byte("x"), 0o644)
	os.Symlink(outside, filepath.Join(trash, old, "link"))

	l, err := m.Trash(context.Background(), agentproto.SiteTrash{})
	if err != nil || len(l.Entries) != 2 || l.Entries[0].Name != recent || l.Entries[0].SiteID != 5 || l.Entries[0].Bytes < 5000 {
		t.Fatalf("list = %+v, %v", l, err)
	}
	m.cleanTrash(now)
	for name, want := range map[string]bool{old: false, recent: true, "notes": true} {
		if _, err := os.Stat(filepath.Join(trash, name)); (err == nil) != want {
			t.Errorf("%s exists: %v, want %v", name, err == nil, want)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("cleaning followed a symlink out of the trash")
	}
	for _, bad := range []string{"../4-1", "notes", "4-1/../..", "0-1"} {
		if _, err := m.Trash(context.Background(), agentproto.SiteTrash{Remove: bad}); err == nil {
			t.Errorf("removed %q", bad)
		}
	}
	if _, err := m.Trash(context.Background(), agentproto.SiteTrash{Remove: "9-1"}); err == nil {
		t.Error("removed a missing entry")
	}
	l, err = m.Trash(context.Background(), agentproto.SiteTrash{Remove: recent})
	if err != nil || len(l.Entries) != 0 {
		t.Fatalf("after removing = %+v, %v", l, err)
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

// TestRemoveTemps: temporaries not written for a while go; fresh ones, a
// site's own files and whatever a symlink points at stay.
func TestRemoveTemps(t *testing.T) {
	root := t.TempDir()
	m, err := New(Config{SitesRoot: root}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	sd := filepath.Join(root, "1")
	os.MkdirAll(filepath.Join(sd, "public/sub"), 0o755)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, ".basalt-upload-x"), []byte("x"), 0o644)
	os.Symlink(outside, filepath.Join(sd, "public/out"))
	old := time.Now().Add(-7 * time.Hour)
	for _, f := range []string{"public/.basalt-upload-abc", "public/sub/.basalt-upload-sync-0011", "public/.basalt-upload-fresh", "public/page.html"} {
		os.WriteFile(filepath.Join(sd, f), []byte("x"), 0o644)
		if !strings.Contains(f, "fresh") {
			os.Chtimes(filepath.Join(sd, f), old, old)
		}
	}
	os.Chtimes(filepath.Join(outside, ".basalt-upload-x"), old, old)
	os.MkdirAll(filepath.Join(root, ".trash"), 0o755)
	m.RemoveTemps(context.Background(), time.Now().Add(-6*time.Hour))
	for f, want := range map[string]bool{
		"public/.basalt-upload-abc":           false,
		"public/sub/.basalt-upload-sync-0011": false,
		"public/.basalt-upload-fresh":         true,
		"public/page.html":                    true,
	} {
		if _, err := os.Stat(filepath.Join(sd, f)); (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", f, err == nil, want)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, ".basalt-upload-x")); err != nil {
		t.Error("a file behind a symlink was removed")
	}
}
