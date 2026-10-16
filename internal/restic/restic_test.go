package restic

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runner(t *testing.T) *Runner {
	t.Helper()
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	r := &Runner{Binary: bin, CacheDir: t.TempDir()}
	if _, err := r.Version(context.Background()); err != nil {
		t.Skip(err)
	}
	return r
}

func TestBackupRestore(t *testing.T) {
	ctx := context.Background()
	r := runner(t)
	var lines []string
	r.Line = func(s string) { lines = append(lines, s) }
	repo := Repo{Location: filepath.Join(t.TempDir(), "repo"), Password: "correct horse battery"}

	if _, err := r.Snapshots(ctx, repo, "h", nil); !errors.Is(err, ErrNoRepository) {
		t.Fatalf("snapshots of a missing repository: %v", err)
	}
	if created, err := r.Init(ctx, repo); err != nil || !created {
		t.Fatalf("init: %v %v", created, err)
	}
	if created, err := r.Init(ctx, repo); err != nil || created {
		t.Fatalf("second init: %v %v", created, err)
	}

	src := t.TempDir()
	write := func(name, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(src, name)), 0o755)
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("public/index.html", "v1")
	write("tmp/cache", "skip me")
	tags := []string{"res=site-1", "part=files"}
	s1, err := r.Backup(ctx, repo, BackupOptions{Host: "h", Tags: tags, Paths: []string{src}, Excludes: []string{filepath.Join(src, "tmp")}})
	if err != nil || s1.SnapshotID == "" || s1.FilesNew != 1 {
		t.Fatalf("backup: %+v %v", s1, err)
	}
	write("public/index.html", "v2")
	if _, err := r.Backup(ctx, repo, BackupOptions{Host: "h", Tags: tags, Paths: []string{src}}); err != nil {
		t.Fatal(err)
	}
	dump, err := r.Backup(ctx, repo, BackupOptions{Host: "h", Tags: []string{"res=site-1", "part=db"},
		StdinCommand: []string{"sh", "-c", "echo 'CREATE TABLE t;'"}, StdinFilename: "shop.sql"})
	if err != nil {
		t.Fatal(err)
	}
	// A failing dump leaves no snapshot behind.
	_, err = r.Backup(ctx, repo, BackupOptions{Host: "h", Tags: []string{"part=bad"},
		StdinCommand: []string{"sh", "-c", "echo partial; exit 2"}, StdinFilename: "bad.sql"})
	if err == nil || !strings.Contains(err.Error(), "exit status 2") {
		t.Fatalf("failing dump: %v", err)
	}
	if list, _ := r.Snapshots(ctx, repo, "h", []string{"part=bad"}); len(list) != 0 {
		t.Fatalf("snapshot of a failed dump: %v", list)
	}

	// Restore the first snapshot over changed and extra files.
	write("public/extra.txt", "new")
	if err := r.RestoreTree(ctx, repo, s1.SnapshotID, src, src); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(src, "public/index.html")); string(b) != "v1" {
		t.Fatalf("restored content %q", b)
	}
	if _, err := os.Stat(filepath.Join(src, "public/extra.txt")); !os.IsNotExist(err) {
		t.Fatal("file not in the snapshot was kept")
	}
	var buf bytes.Buffer
	if err := r.Dump(ctx, repo, dump.SnapshotID, "/shop.sql", &buf); err != nil || buf.String() != "CREATE TABLE t;\n" {
		t.Fatalf("dump: %q %v", buf.String(), err)
	}

	// Retention works per set of tags: the database snapshot stays.
	if err := r.Forget(ctx, repo, "h", []string{"res=site-1"}, Keep{Last: 1}); err != nil {
		t.Fatal(err)
	}
	if list, _ := r.Snapshots(ctx, repo, "h", []string{"res=site-1"}); len(list) != 2 {
		t.Fatalf("after forget: %d snapshots", len(list))
	}
	if err := r.Prune(ctx, repo); err != nil {
		t.Fatal(err)
	}

	// A wrong password fails without echoing the password.
	bad := repo
	bad.Password = "wrong password here"
	if _, err := r.Snapshots(ctx, bad, "h", nil); err == nil || strings.Contains(err.Error(), "wrong password here") {
		t.Fatalf("wrong password: %v", err)
	}
	for _, l := range lines {
		if strings.Contains(l, repo.Password) {
			t.Fatalf("password in output: %q", l)
		}
	}
}

func TestVersionAtLeast(t *testing.T) {
	for _, c := range []struct {
		have string
		ok   bool
	}{{"0.17.0", true}, {"0.19.1", true}, {"1.0.0", true}, {"0.16.4", false}, {"0.9.9", false}} {
		if versionAtLeast(c.have, MinVersion) != c.ok {
			t.Errorf("%s: want %v", c.have, c.ok)
		}
	}
}

func TestCheck(t *testing.T) {
	ctx := context.Background()
	r := runner(t)
	var lines []string
	r.Line = func(s string) { lines = append(lines, s) }
	repo := Repo{Location: filepath.Join(t.TempDir(), "repo"), Password: "correct horse battery"}
	if err := r.Check(ctx, repo, ""); !errors.Is(err, ErrNoRepository) {
		t.Fatalf("check of a missing repository: %v", err)
	}
	if _, err := r.Init(ctx, repo); err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "a"), []byte(strings.Repeat("data ", 1000)), 0o644)
	if _, err := r.Backup(ctx, repo, BackupOptions{Host: "h", Paths: []string{src}}); err != nil {
		t.Fatal(err)
	}
	lines = nil
	if err := r.Check(ctx, repo, "1/1"); err != nil {
		t.Fatalf("check: %v (%q)", err, lines)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "no errors were found") || strings.Contains(joined, "[0:") {
		t.Fatalf("check output: %q", lines)
	}
	// A damaged pack file is found when the data is read.
	packs, _ := filepath.Glob(filepath.Join(repo.Location, "data", "*", "*"))
	if len(packs) == 0 {
		t.Fatal("no pack files")
	}
	for _, p := range packs {
		os.Chmod(p, 0o644)
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteAt([]byte("XXXX"), 40)
		f.Close()
	}
	lines = nil
	err := r.Check(ctx, repo, "1/1")
	if err == nil || !strings.Contains(err.Error(), "repository contains errors") {
		t.Fatalf("damaged repository: %v", err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "pack ") {
		t.Fatalf("findings not in the output: %q", lines)
	}
}
