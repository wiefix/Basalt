package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// TestInotifyWatch: changes anywhere in a watched site, new folders
// included, mark it changed; its logs, tmp and temporary uploads, and
// other sites, do not.
func TestInotifyWatch(t *testing.T) {
	root := t.TempDir()
	site := filepath.Join(root, "7")
	for _, d := range []string{"public/a", "logs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(site, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(root, "8"), 0o755)
	w := newSiteWatcher(slog.New(slog.NewTextHandler(io.Discard, nil)), root)
	w.start()
	defer w.close()
	if w.backend == nil {
		t.Skip("no inotify here")
	}
	if err := w.apply(agentproto.SiteWatch{Sites: []int64{7}}); err != nil {
		t.Fatal(err)
	}
	changed := func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		c := w.changed[7]
		clear(w.changed)
		return c
	}
	expect := func(what string, want bool, do func()) {
		t.Helper()
		changed()
		do()
		wait := 2 * time.Second
		if !want {
			wait = 500 * time.Millisecond // events arrive within milliseconds
		}
		deadline := time.Now().Add(wait)
		for time.Now().Before(deadline) {
			if changed() {
				if !want {
					t.Fatalf("%s: reported", what)
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		if want {
			t.Fatalf("%s: not reported", what)
		}
	}
	write := func(p string) func() {
		return func() {
			if err := os.WriteFile(filepath.Join(site, p), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	expect("a file deep inside", true, write("public/a/index.html"))
	expect("the site's log", false, write("logs/access.log"))
	expect("its tmp", false, write("tmp/session"))
	expect("a temporary upload", false, write("public/.basalt-upload-abc"))
	expect("another site", false, func() { os.WriteFile(filepath.Join(root, "8", "x"), nil, 0o644) })
	expect("a new folder", true, func() { os.MkdirAll(filepath.Join(site, "public/new/deep"), 0o755) })
	expect("a file in the new folder", true, write("public/new/deep/f"))
	expect("a folder moved", true, func() { os.Rename(filepath.Join(site, "public/new"), filepath.Join(site, "public/moved")) })
	expect("a file in the moved folder", true, write("public/moved/deep/g"))
	expect("a removal", true, func() { os.Remove(filepath.Join(site, "public/a/index.html")) })

	// Unwatched: nothing more.
	w.apply(agentproto.SiteWatch{Sites: []int64{}})
	expect("after unwatching", false, write("public/a/z"))
}
