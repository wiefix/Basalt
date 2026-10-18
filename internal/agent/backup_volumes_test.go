package agent

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/restic"
)

// volumeTar is a volume as the helper container archives it.
func volumeTar(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	add := func(h *tar.Header, body string) {
		h.ModTime, h.Size = mt, int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	add(&tar.Header{Typeflag: tar.TypeDir, Name: "./", Mode: 0o755}, "")
	add(&tar.Header{Typeflag: tar.TypeDir, Name: "./data/", Mode: 0o750, Uid: 1234, Gid: 1234}, "")
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "./data/a.txt", Mode: 0o640, Uid: 1234, Gid: 1234}, "alpha")
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "./data/sub/b.txt", Mode: 0o644, Uid: 1234, Gid: 1234}, "beta") // no folder header
	add(&tar.Header{Typeflag: tar.TypeSymlink, Name: "./link", Linkname: "/etc/passwd", Mode: 0o777}, "")
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "./run", Mode: 0o4755}, "#!/bin/sh")
	add(&tar.Header{Typeflag: tar.TypeReg, Name: "../escape", Mode: 0o644}, "no")
	tw.Close()
	return buf.Bytes()
}

func TestVolumeSnapshotFiles(t *testing.T) {
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	b := &backupManager{binary: bin, dataDir: filepath.Join(dir, "agent"), sites: &sitesManager{root: filepath.Join(dir, "sites")},
		slots: make(chan struct{}, 2), locks: map[string]*sync.Mutex{}}
	spec := agentproto.BackupRepo{Kind: agentproto.StorageLocal, Path: filepath.Join(dir, "repo"), Password: "pw"}
	repo, err := b.repo(spec)
	if err != nil {
		t.Fatal(err)
	}
	r := b.runner(nil)
	if _, err := r.Init(ctx, repo); err != nil {
		t.Fatal(err)
	}
	stdin := func(content []byte, file string, tags ...string) string {
		p := filepath.Join(dir, file)
		os.WriteFile(p, content, 0o600)
		s, err := r.Backup(ctx, repo, restic.BackupOptions{Host: backupHost, Tags: tags, StdinCommand: []string{"cat", p}, StdinFilename: file})
		if err != nil {
			t.Fatal(err)
		}
		return s.SnapshotID
	}
	vol := stdin(volumeTar(t), "volume-files.tar", "res=app-7", "part=volume-files")
	dump := stdin([]byte("CREATE TABLE t (x int);\n"), "dump-shop.sql", "res=app-7", "part=dump-shop")
	req := func(p, snap string) agentproto.SnapshotFilesRequest {
		return agentproto.SnapshotFilesRequest{Repo: spec, Resource: "app-7", AppID: 7, Part: "volume-files", Snapshot: snap, Path: p}
	}
	names := func(es []agentproto.SnapshotEntry) string {
		var out []string
		for _, e := range es {
			out = append(out, e.Name+":"+e.Type)
		}
		return strings.Join(out, " ")
	}

	// Browsing: folders first, a folder the archive only implies, nothing outside.
	top, err := b.Browse(ctx, req("", vol))
	if err != nil || names(top.Entries) != "data:dir link:symlink run:file" {
		t.Fatalf("top: %+v %v", top, err)
	}
	data, err := b.Browse(ctx, req("data", vol))
	if err != nil || names(data.Entries) != "sub:dir a.txt:file" || data.Entries[1].Size != 5 || data.Entries[1].Mode != "0640" {
		t.Fatalf("data: %+v %v", data, err)
	}
	if _, err := b.Browse(ctx, req("run", vol)); err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Fatalf("browse a file: %v", err)
	}
	if _, err := b.Browse(ctx, req("missing", vol)); !errors.Is(err, errNotInBackup) {
		t.Fatalf("browse missing: %v", err)
	}
	// The snapshot must hold this part of this app.
	other := req("", vol)
	other.Part = "volume-other"
	if _, err := b.Browse(ctx, other); err == nil {
		t.Fatal("snapshot of another volume accepted")
	}
	other = req("", vol)
	other.Resource, other.AppID = "app-8", 8
	if _, err := b.Browse(ctx, other); err == nil {
		t.Fatal("snapshot of another app accepted")
	}
	if _, err := b.Browse(ctx, req("", dump)); err == nil {
		t.Fatal("dump snapshot browsed as the volume")
	}

	read := func(e agentproto.ExportResponse) []byte {
		t.Helper()
		bs, err := os.ReadFile(filepath.Join(b.exportDir(), e.ID))
		if err != nil {
			t.Fatal(err)
		}
		b.ExportClose(agentproto.ExportClose{ID: e.ID})
		return bs
	}
	zipNames := func(bs []byte) string {
		t.Helper()
		zr, err := zip.NewReader(bytes.NewReader(bs), int64(len(bs)))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range zr.File {
			out = append(out, f.Name)
		}
		slices.Sort(out)
		return strings.Join(out, " ")
	}
	// Downloads: a file as it is, a folder (and the volume) as a zip without links.
	e, err := b.Export(ctx, req("data/a.txt", vol))
	if err != nil || e.Name != "a.txt" || string(read(e)) != "alpha" {
		t.Fatalf("export file: %+v %v", e, err)
	}
	e, err = b.Export(ctx, req("data", vol))
	if err != nil || e.Name != "data.zip" {
		t.Fatalf("export folder: %+v %v", e, err)
	}
	if got := zipNames(read(e)); got != "a.txt sub/b.txt" {
		t.Fatalf("folder zip: %s", got)
	}
	e, err = b.Export(ctx, req("", vol))
	if err != nil || e.Name != "volume-files-"+vol[:8]+".zip" {
		t.Fatalf("export volume: %+v %v", e, err)
	}
	if got := zipNames(read(e)); got != "data/ data/a.txt data/sub/b.txt run" {
		t.Fatalf("volume zip: %s", got)
	}
	if _, err := b.Export(ctx, req("link", vol)); err == nil {
		t.Fatal("symbolic link downloaded")
	}
	if _, err := b.Export(ctx, req("nope", vol)); !errors.Is(err, errNotInBackup) {
		t.Fatalf("export missing: %v", err)
	}
	// A database dump.
	dreq := agentproto.SnapshotFilesRequest{Repo: spec, Resource: "app-7", AppID: 7, Part: "dump-shop", Snapshot: dump}
	e, err = b.Export(ctx, dreq)
	if err != nil || e.Name != "shop-"+dump[:8]+".sql" || !strings.HasPrefix(string(read(e)), "CREATE TABLE") {
		t.Fatalf("export dump: %+v %v", e, err)
	}
	if _, err := b.Browse(ctx, dreq); err == nil {
		t.Fatal("dump browsed")
	}

	// Restoring a copy: what the extracting command receives.
	copyOf := func(rel string) (agentproto.RestoreCopyResponse, map[string]*tar.Header, error) {
		out := filepath.Join(dir, "copy.tar")
		res, err := b.copyIntoVolume(ctx, r, repo, vol, "files", rel, []string{"sh", "-c", "cat > " + out})
		got := map[string]*tar.Header{}
		if err == nil {
			f, _ := os.Open(out)
			defer f.Close()
			tr := tar.NewReader(f)
			for {
				h, err := tr.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				got[h.Name] = h
			}
		}
		return res, got, err
	}
	res, got, err := copyOf("data")
	if err != nil || res.Files != 2 || res.Dirs != 1 || res.Bytes != 9 || len(got) != 3 {
		t.Fatalf("copy folder: %+v %v %v", res, got, err)
	}
	if h := got["data/a.txt"]; h == nil || h.Uid != 1234 || h.Mode != 0o640 {
		t.Fatalf("copy keeps owner and mode: %+v", h)
	}
	res, got, err = copyOf("")
	if err != nil || res.Skipped != 1 || got["link"] != nil || got["run"] == nil || got["run"].Mode != 0o755 {
		t.Fatalf("copy volume: %+v %v %v", res, got, err)
	}
	if _, got, err = copyOf("data/sub/b.txt"); err != nil || len(got) != 1 || got["b.txt"] == nil {
		t.Fatalf("copy file: %v %v", got, err)
	}
	if _, _, err := copyOf("nope"); !errors.Is(err, errNotInBackup) {
		t.Fatalf("copy missing: %v", err)
	}
}
