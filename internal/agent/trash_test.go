package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestTrash(t *testing.T) {
	root := t.TempDir()
	m := newSitesManager(Config{SitesRoot: root}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

	l, err := m.trash(context.Background(), "")
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
		if _, err := m.trash(context.Background(), bad); err == nil {
			t.Errorf("removed %q", bad)
		}
	}
	if _, err := m.trash(context.Background(), "9-1"); err == nil {
		t.Error("removed a missing entry")
	}
	l, err = m.trash(context.Background(), recent)
	if err != nil || len(l.Entries) != 0 {
		t.Fatalf("after removing = %+v, %v", l, err)
	}
}
