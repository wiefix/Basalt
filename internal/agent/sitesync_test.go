package agent

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// TestSiteSync: a manifest lists what a copy needs; reads and writes move
// files whole into place with their mode and time; a site's logs and tmp
// stay out.
func TestSiteSync(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	src := newSitesManager(Config{SitesRoot: t.TempDir()}, log)
	dst := newSitesManager(Config{SitesRoot: t.TempDir()}, log)
	sd := filepath.Join(src.root, "1")
	dd := filepath.Join(dst.root, "1")
	for _, d := range []string{"public/sub", "logs", "tmp"} {
		os.MkdirAll(filepath.Join(sd, d), 0o755)
	}
	os.MkdirAll(filepath.Join(dd, "public"), 0o755)
	os.Chmod(filepath.Join(dd, "public"), 0o755|fs.ModeSetgid)
	big := bytes.Repeat([]byte("0123456789"), 300_000) // 3 MB
	os.WriteFile(filepath.Join(sd, "public/index.html"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(sd, "public/sub/secret.txt"), []byte("s"), 0o600)
	os.WriteFile(filepath.Join(sd, "public/big.bin"), big, 0o644)
	os.WriteFile(filepath.Join(sd, "logs/access.log"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(sd, "tmp/session"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(sd, "public/.basalt-upload-123"), []byte("x"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(sd, "public/link"))
	os.Chmod(filepath.Join(sd, "public/sub"), 0o750)

	var entries []agentproto.ManifestEntry
	send := func(v any) error {
		entries = append(entries, v.(agentproto.ManifestFrame).Entries...)
		return nil
	}
	resp, err := src.manifest(ctx, agentproto.SiteManifestRequest{SiteID: 1}, send)
	if err != nil || resp.Count != 5 || len(entries) != 5 {
		t.Fatalf("manifest: %+v %v %+v", resp, err, entries)
	}
	byPath := map[string]agentproto.ManifestEntry{}
	for i, e := range entries {
		byPath[e.Path] = e
		if e.Path == "public/sub/secret.txt" && byPath["public/sub"].Path == "" {
			t.Fatal("a file came before its folder")
		}
		_ = i
	}
	if e := byPath["public/big.bin"]; e.Size != int64(len(big)) || e.MTime == 0 || e.Dir {
		t.Fatalf("big.bin: %+v", e)
	}
	if e := byPath["public/sub"]; !e.Dir || e.Mode != 0o750 {
		t.Fatalf("sub: %+v", e)
	}
	if _, err := src.manifest(ctx, agentproto.SiteManifestRequest{SiteID: 1}, nil); err == nil {
		t.Fatal("a manifest without a stream")
	}
	if _, err := src.manifest(ctx, agentproto.SiteManifestRequest{SiteID: 2}, send); err == nil {
		t.Fatal("manifest of a missing site")
	}

	// Copy: folders, then each file in chunks of up to 1 MB.
	w := agentproto.SiteSyncWriteRequest{SiteID: 1, Delete: []string{"public/gone.txt"}}
	for _, e := range entries {
		if e.Dir {
			w.Mkdir = append(w.Mkdir, e)
		}
	}
	if err := dst.syncWrite(w); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Dir {
			continue
		}
		for off := int64(0); ; {
			r, err := src.syncRead(agentproto.SiteSyncReadRequest{SiteID: 1, Reads: []agentproto.SyncRead{{Path: e.Path, Offset: off, Length: 1 << 20}}})
			if err != nil {
				t.Fatal(err)
			}
			c := r.Chunks[0]
			final := off+int64(len(c.Data)) >= c.Size
			if err := dst.syncWrite(agentproto.SiteSyncWriteRequest{SiteID: 1, Write: []agentproto.SyncWrite{{Path: e.Path, Offset: off,
				Data: c.Data, Final: final, Mode: e.Mode, MTime: c.MTime}}}); err != nil {
				t.Fatal(err)
			}
			if final {
				break
			}
			// Not in place before the last chunk.
			if _, err := os.Stat(filepath.Join(dd, e.Path)); err == nil {
				t.Fatalf("%s in place before its last chunk", e.Path)
			}
			off += int64(len(c.Data))
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dd, "public/big.bin")); !bytes.Equal(b, big) {
		t.Fatal("big.bin differs")
	}
	fi, _ := os.Stat(filepath.Join(dd, "public/sub/secret.txt"))
	si, _ := os.Stat(filepath.Join(sd, "public/sub/secret.txt"))
	if fi.Mode().Perm() != 0o600 || !fi.ModTime().Equal(si.ModTime()) {
		t.Fatalf("secret.txt: %v %v", fi.Mode(), fi.ModTime())
	}
	if fi, _ := os.Stat(filepath.Join(dd, "public/sub")); fi.Mode().Perm() != 0o750 {
		t.Fatalf("sub mode: %v", fi.Mode())
	}
	for _, p := range []string{"logs/access.log", "tmp/session", "public/link"} {
		if _, err := os.Lstat(filepath.Join(dd, p)); err == nil {
			t.Fatalf("%s was copied", p)
		}
	}
	// The setgid bit of the site's folders stays.
	if err := dst.syncWrite(agentproto.SiteSyncWriteRequest{SiteID: 1, Chmod: []agentproto.ManifestEntry{{Path: "public", Mode: 0o750}}}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dd, "public")); fi.Mode()&fs.ModeSetgid == 0 || fi.Mode().Perm() != 0o750 {
		t.Fatalf("public mode: %v", fi.Mode())
	}
	// No temporaries are left.
	des, _ := os.ReadDir(filepath.Join(dd, "public"))
	for _, d := range des {
		if strings.HasPrefix(d.Name(), ".basalt-upload-") {
			t.Fatalf("temporary left: %s", d.Name())
		}
	}

	// Gone and changed files, refused paths, out-of-order writes.
	r, err := src.syncRead(agentproto.SiteSyncReadRequest{SiteID: 1, Reads: []agentproto.SyncRead{{Path: "public/nope", Length: 10}, {Path: "public/link", Length: 10}}})
	if err != nil || !r.Chunks[0].Missing || !r.Chunks[1].Missing {
		t.Fatalf("missing reads: %+v %v", r, err)
	}
	for _, bad := range []agentproto.SiteSyncWriteRequest{
		{SiteID: 1, Delete: []string{"logs"}},
		{SiteID: 1, Delete: []string{"/"}},
		{SiteID: 1, Write: []agentproto.SyncWrite{{Path: "tmp/x", Data: []byte("x"), Final: true}}},
		{SiteID: 1, Write: []agentproto.SyncWrite{{Path: "public/x", Offset: 5, Data: []byte("x")}}},
		{SiteID: 1, Write: []agentproto.SyncWrite{{Path: "public/x", Mode: 0o4755, Final: true}}},
		{SiteID: 1, Write: []agentproto.SyncWrite{{Path: "public/x", Data: make([]byte, agentproto.MaxSyncData+1)}}},
		{SiteID: 0},
	} {
		if err := dst.syncWrite(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	// ".." stays inside the site.
	if err := dst.syncWrite(agentproto.SiteSyncWriteRequest{SiteID: 1, Write: []agentproto.SyncWrite{{Path: "../escape", Data: []byte("x"), Final: true, Mode: 0o644}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dd), "escape")); err == nil {
		t.Fatal("wrote outside the site")
	}
	if _, err := os.Stat(filepath.Join(dd, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := src.syncRead(agentproto.SiteSyncReadRequest{SiteID: 1, Reads: []agentproto.SyncRead{{Path: "logs/access.log", Length: 1}}}); err == nil {
		t.Fatal("read the site's logs")
	}
	// A deletion removes a folder with what it holds.
	if err := dst.syncWrite(agentproto.SiteSyncWriteRequest{SiteID: 1, Delete: []string{"public/sub"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dd, "public/sub")); err == nil {
		t.Fatal("folder not deleted")
	}
	_ = time.Now
}
