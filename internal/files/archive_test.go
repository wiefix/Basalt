package files

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body string
	mode       int64
	link       string // tar symlink target
}

func makeZip(t *testing.T, entries []entry) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != 0 {
			h.SetMode(os.FileMode(e.mode))
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(e.body))
	}
	zw.Close()
	return buf.Bytes()
}

func makeTarGz(t *testing.T, entries []entry) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if e.mode != 0 {
			h.Mode = e.mode
		}
		if e.link != "" {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.link, 0
		}
		if strings.HasSuffix(e.name, "/") {
			h.Typeflag, h.Size = tar.TypeDir, 0
		}
		tw.WriteHeader(h)
		tw.Write([]byte(e.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractZipSlipAndModes(t *testing.T) {
	s, base := setup(t)
	s.WriteFile("/a.zip", makeZip(t, []entry{
		{name: "site/index.html", body: "ok"},
		{name: "site/run.sh", body: "#!/bin/sh", mode: 0o4777}, // setuid must be dropped
		{name: "../../2/config.php", body: "pwned"},            // zip-slip
		{name: "/abs/evil.txt", body: "abs"},
		{name: "site/sub/", mode: int64(os.ModeDir | 0o777)},
	}))
	res, err := s.Extract("/a.zip", "/public", false, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(base, "sites", "2", "config.php")); string(b) != "SECRET" {
		t.Fatalf("zip-slip modified another site: %q", b)
	}
	// "../../2/config.php" is confined to public/2/config.php.
	if data, _, _ := s.Read("/public/2/config.php", 0, 0); string(data) != "pwned" {
		t.Fatalf("confined entry not written: %q", data)
	}
	if e, _ := s.Stat("/public/site/run.sh"); e.Mode != "0755" {
		t.Fatalf("run.sh mode = %s; want 0755", e.Mode)
	}
	if e, _ := s.Stat("/public/abs/evil.txt"); e.Type != "file" {
		t.Fatal("absolute entry not confined")
	}
	if res.Files != 4 || res.Dirs != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestExtractSkipsSymlinksAndExisting(t *testing.T) {
	s, base := setup(t)
	s.WriteFile("/a.tgz", makeTarGz(t, []entry{
		{name: "evil", link: "/etc/passwd"},
		{name: "index.html", body: "replaced"},
		{name: "dir/"},
		{name: "dir/new.txt", body: "new"},
	}))
	res, err := s.Extract("/a.tgz", "/public", false, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(base, "sites", "1", "public", "evil")); err == nil {
		t.Fatal("symlink extracted")
	}
	if data, _, _ := s.Read("/public/index.html", 0, 0); string(data) != "hi" {
		t.Fatal("existing file overwritten without overwrite flag")
	}
	if strings.Join(res.Skipped, ",") != "evil,index.html" || res.Files != 1 {
		t.Fatalf("result = %+v", res)
	}
	if _, err := s.Extract("/a.tgz", "/public", true, DefaultLimits); err != nil {
		t.Fatal(err)
	}
	if data, _, _ := s.Read("/public/index.html", 0, 0); string(data) != "replaced" {
		t.Fatal("overwrite flag ignored")
	}
}

func TestExtractLimits(t *testing.T) {
	s, _ := setup(t)
	// A bomb: tiny archive, large output.
	s.WriteFile("/bomb.zip", makeZip(t, []entry{{name: "big", body: strings.Repeat("0", 1<<20)}}))
	_, err := s.Extract("/bomb.zip", "/public", false, Limits{MaxTotalBytes: 1000, MaxFiles: 10})
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("bomb: %v", err)
	}
	if _, err := s.Stat("/public/big"); err == nil {
		t.Fatal("partial bomb output left behind")
	}
	many := make([]entry, 20)
	for i := range many {
		many[i] = entry{name: strings.Repeat("f", i+1), body: "x"}
	}
	s.WriteFile("/many.zip", makeZip(t, many))
	if _, err := s.Extract("/many.zip", "/public", false, Limits{MaxTotalBytes: 1 << 20, MaxFiles: 10}); !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("file count: %v", err)
	}
}

func TestCompressRoundTrip(t *testing.T) {
	s, _ := setup(t)
	s.WriteFile("/public/img/a.png", []byte("png"))
	if err := s.Compress([]string{"/public"}, "/backup.zip"); err != nil {
		t.Fatal(err)
	}
	if err := s.Compress([]string{"/public"}, "/backup.zip"); !errors.Is(err, ErrExists) {
		t.Fatalf("overwrote existing archive: %v", err)
	}
	s.Mkdir("/restore")
	res, err := s.Extract("/backup.zip", "/restore", false, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if data, _, _ := s.Read("/restore/public/img/a.png", 0, 0); string(data) != "png" {
		t.Fatalf("round trip lost data: %+v", res)
	}
	// Escaping symlinks inside the tree were not followed into the archive.
	if _, err := s.Stat("/restore/public/rel-dir-link"); err == nil {
		t.Fatal("symlink archived")
	}
	if _, err := s.Stat("/restore/public/rel-dir-link/config.php"); err == nil {
		t.Fatal("followed an escaping symlink while archiving")
	}
}
