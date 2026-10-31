package files

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// setup creates sites/1 (the site under test) and sites/2 holding a secret,
// plus a file outside both.
func setup(t *testing.T) (*Site, string) {
	t.Helper()
	base := t.TempDir()
	site := filepath.Join(base, "sites", "1")
	other := filepath.Join(base, "sites", "2")
	for _, d := range []string{filepath.Join(site, "public", "img"), other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(site, "public", "index.html"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(other, "config.php"), []byte("SECRET"), 0o600)
	os.WriteFile(filepath.Join(base, "outside.txt"), []byte("SECRET"), 0o600)
	// Planted escape symlinks (e.g. uploaded over SFTP).
	os.Symlink(filepath.Join(other, "config.php"), filepath.Join(site, "public", "abs-link"))
	os.Symlink("../../2", filepath.Join(site, "public", "rel-dir-link"))
	os.Symlink("index.html", filepath.Join(site, "public", "inner-link"))

	s, err := Open(site)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, base
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"": ".", "/": ".", ".": ".", "/public/x": "public/x", "public/../x": "x",
		"../../etc/passwd": "etc/passwd", `..\..\x`: "x", "a//b/": "a/b",
	} {
		if got, err := Clean(in); err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Clean("a\x00b"); err == nil {
		t.Error("NUL byte accepted")
	}
}

func TestListAndStat(t *testing.T) {
	s, _ := setup(t)
	entries, err := s.List("/public")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name+":"+e.Type)
	}
	if got := strings.Join(names, ","); got != "img:dir,abs-link:symlink,index.html:file,inner-link:symlink,rel-dir-link:symlink" {
		t.Fatalf("list = %s", got)
	}
	if e, _ := s.Stat("/public/index.html"); e.Size != 2 || e.Mode != "0644" {
		t.Fatalf("stat = %+v", e)
	}
	if _, err := s.List("/public/index.html"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("list of file: %v", err)
	}
}

func TestNothingEscapesTheSite(t *testing.T) {
	s, base := setup(t)
	reads := []string{
		"/public/abs-link", "/public/rel-dir-link/config.php", "../2/config.php",
		"/../../outside.txt", "public/../../2/config.php",
	}
	for _, p := range reads {
		if data, _, err := s.Read(p, 0, 0); err == nil || bytes.Contains(data, []byte("SECRET")) {
			t.Errorf("Read(%q) = %q, %v; must fail", p, data, err)
		}
	}
	if _, err := s.List("/public/rel-dir-link"); err == nil {
		t.Error("listing through an escaping symlink succeeded")
	}
	// Writes through escaping symlinks must not touch the other site.
	s.WriteFile("/public/rel-dir-link/config.php", []byte("pwned"))
	if err := s.Chmod("/public/rel-dir-link/config.php", 0o777); err == nil {
		t.Error("chmod through escaping symlink succeeded")
	}
	s.Rename("/public/rel-dir-link/config.php", "/public/stolen.php")
	if b, _ := os.ReadFile(filepath.Join(base, "sites", "2", "config.php")); string(b) != "SECRET" {
		t.Fatalf("other site's file modified: %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(base, "sites", "2", "config.php")); fi.Mode().Perm() != 0o600 {
		t.Fatal("other site's file permissions changed")
	}

	// Deleting a symlink removes the link, never the target.
	if err := s.Delete("/public/abs-link"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("/public/rel-dir-link"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "sites", "2", "config.php")); err != nil {
		t.Fatal("deleting a symlink removed its target")
	}
	// Symlinks that stay inside are fine.
	if data, _, err := s.Read("/public/inner-link", 0, 0); err != nil || string(data) != "hi" {
		t.Fatalf("inner symlink: %q %v", data, err)
	}
}

func TestSiteRootIsProtected(t *testing.T) {
	s, _ := setup(t)
	for name, err := range map[string]error{
		"delete": s.Delete("/"), "delete dotdot": s.Delete("/.."),
		"rename": s.Rename("/", "/x"), "rename into root": s.Rename("/public", "/"),
	} {
		if !errors.Is(err, ErrRootTouched) {
			t.Errorf("%s: %v; want ErrRootTouched", name, err)
		}
	}
	if err := s.Rename("/public", "/public/img/x"); err == nil {
		t.Error("moved a directory into itself")
	}
}

func TestUploadIsAtomic(t *testing.T) {
	s, _ := setup(t)
	u, err := s.BeginUpload("/public/index.html")
	if err != nil {
		t.Fatal(err)
	}
	u.Write(0, []byte("new "))
	if err := u.Write(0, []byte("dup")); err == nil {
		t.Fatal("out-of-order chunk accepted")
	}
	u.Write(4, []byte("content"))

	// Before commit readers still see the old file; the temp file is hidden.
	if data, _, _ := s.Read("/public/index.html", 0, 0); string(data) != "hi" {
		t.Fatalf("partial upload visible: %q", data)
	}
	entries, _ := s.List("/public")
	for _, e := range entries {
		if strings.HasPrefix(e.Name, uploadPrefix) {
			t.Fatal("upload temporary listed")
		}
	}
	if err := u.Commit(); err != nil {
		t.Fatal(err)
	}
	if data, _, _ := s.Read("/public/index.html", 0, 0); string(data) != "new content" {
		t.Fatalf("after commit: %q", data)
	}

	u2, _ := s.BeginUpload("/public/aborted.txt")
	u2.Write(0, []byte("x"))
	u2.Abort()
	if _, err := s.Stat("/public/aborted.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("aborted upload left a file")
	}
	if _, err := s.BeginUpload("/missing-dir/x"); err == nil {
		t.Fatal("upload into a missing directory accepted")
	}
	if _, err := s.BeginUpload("/public"); !errors.Is(err, ErrIsDir) {
		t.Fatal("upload over a directory accepted")
	}
}

func TestReadChunks(t *testing.T) {
	s, _ := setup(t)
	s.WriteFile("/public/data.bin", []byte("0123456789"))
	data, size, err := s.Read("/public/data.bin", 3, 4)
	if err != nil || string(data) != "3456" || size != 10 {
		t.Fatalf("chunk = %q %d %v", data, size, err)
	}
	if data, _, _ := s.Read("/public/data.bin", 10, 4); len(data) != 0 {
		t.Fatal("read at EOF returned data")
	}
	if _, _, err := s.Read("/public/data.bin", 11, 4); err == nil {
		t.Fatal("offset past EOF accepted")
	}
}

func TestMkdirRenameChmod(t *testing.T) {
	s, _ := setup(t)
	if err := s.Mkdir("/public/new"); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir("/public/new"); !errors.Is(err, ErrExists) {
		t.Fatalf("mkdir existing: %v", err)
	}
	if err := s.Rename("/public/index.html", "/public/img"); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto existing: %v", err)
	}
	if err := s.Rename("/public/index.html", "/public/new/home.html"); err != nil {
		t.Fatal(err)
	}
	if err := s.Chmod("/public/new/home.html", 0o600); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Stat("/public/new/home.html"); e.Mode != "0600" {
		t.Fatalf("mode = %s", e.Mode)
	}
	if err := s.Chmod("/public/new/home.html", 0o4755); !errors.Is(err, ErrBadMode) {
		t.Fatal("setuid accepted")
	}
	if err := s.Chmod("/public/inner-link", 0o600); err == nil {
		t.Fatal("chmod on symlink accepted")
	}
	// WriteFile keeps the mode of the file it replaces.
	s.WriteFile("/public/new/home.html", []byte("v2"))
	if e, _ := s.Stat("/public/new/home.html"); e.Mode != "0600" {
		t.Fatalf("mode after overwrite = %s", e.Mode)
	}
}

func TestSymlinkStaysInSite(t *testing.T) {
	s, _ := setup(t)
	s.Mkdir("/storage")
	if err := s.Symlink("../public/img", "/storage/public"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Readlink("/storage/public"); got != "../public/img" {
		t.Fatalf("target %q", got)
	}
	for target, link := range map[string]string{
		"../../2":     "/storage/out",
		"/etc/passwd": "/storage/abs",
		"../..":       "/storage/up",
		"":            "/storage/empty",
		"x":           "/",
	} {
		if err := s.Symlink(target, link); err == nil {
			t.Errorf("link %s -> %q created", link, target)
		}
	}
}

func TestListPageLargeDirectory(t *testing.T) {
	s, _ := setup(t)
	dir := filepath.Join(s.root.Name(), "big")
	os.Mkdir(dir, 0o755)
	const n = 20000
	for i := range n {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.txt", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.Mkdir(filepath.Join(dir, "zz-dir"), 0o755)
	os.WriteFile(filepath.Join(dir, uploadPrefix+"x"), nil, 0o644) // hidden, not counted

	var names []string
	pages := 0
	for cursor := ""; ; pages++ {
		pg, err := s.ListPage("/big", cursor, MaxPage)
		if err != nil {
			t.Fatal(err)
		}
		if pg.Total != n+1 || len(pg.Entries) > MaxPage {
			t.Fatalf("page %d: total %d, %d entries", pages, pg.Total, len(pg.Entries))
		}
		for _, e := range pg.Entries {
			names = append(names, e.Name)
		}
		if pg.Next == "" {
			break
		}
		cursor = pg.Next
	}
	if pages != n/MaxPage || len(names) != n+1 || names[0] != "zz-dir" || names[1] != "f00000.txt" || names[n] != fmt.Sprintf("f%05d.txt", n-1) {
		t.Fatalf("%d pages, %d names: %v … %v", pages+1, len(names), names[:2], names[len(names)-1])
	}
	if !slices.IsSorted(names[1:]) {
		t.Fatal("files not sorted")
	}
}

func TestListPageCursor(t *testing.T) {
	s, _ := setup(t)
	dir := filepath.Join(s.root.Name(), "d")
	os.Mkdir(dir, 0o755)
	for _, n := range []string{"b", "a", "C", "e"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o644)
	}
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	list := func(cursor string, limit int) ([]string, string) {
		t.Helper()
		pg, err := s.ListPage("/d", cursor, limit)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range pg.Entries {
			names = append(names, e.Name)
		}
		return names, pg.Next
	}
	// Directories first, then names ignoring case.
	got, next := list("", 3)
	if !slices.Equal(got, []string{"sub", "a", "b"}) || next != "f/b" {
		t.Fatalf("page 1 = %v, next %q", got, next)
	}
	// Changes between pages neither repeat nor skip entries after the cursor.
	os.Remove(filepath.Join(dir, "b"))
	os.Remove(filepath.Join(dir, "a"))
	os.WriteFile(filepath.Join(dir, "0"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "d"), nil, 0o644)
	got, next = list(next, 2)
	if !slices.Equal(got, []string{"C", "d"}) || next != "f/d" {
		t.Fatalf("page 2 = %v, next %q", got, next)
	}
	if got, next = list(next, 3); !slices.Equal(got, []string{"e"}) || next != "" {
		t.Fatalf("page 3 = %v, next %q", got, next)
	}
	// Names equal but for case (on case-sensitive file systems) keep one order.
	if (dirKey{false, "b", "B"}).compare(dirKey{false, "b", "b"}) >= 0 {
		t.Error("names equal ignoring case are not ordered")
	}
	for _, c := range []string{"x", "x/a", "d/", "/a"} {
		if _, err := s.ListPage("/d", c, 3); err == nil {
			t.Errorf("cursor %q accepted", c)
		}
	}
}
