package privhelper

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/release"
	"github.com/wiefix/basalt/internal/restic"
	"github.com/wiefix/basalt/internal/sshsig"
)

// served starts a helper with cfg on a socket and returns a client of it.
func served(t *testing.T, cfg Config) (*Helper, *Client) {
	t.Helper()
	h, err := New(cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	path := socketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Serve(ctx, path, os.Geteuid(), h.Handle, quiet)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	c := NewClient(path)
	t.Cleanup(c.Close)
	return h, c
}

// TestResticThroughHelper: restic runs in the helper; a backup reads the
// agent's pipe, a dump writes into one, sites are named by id only.
func TestResticThroughHelper(t *testing.T) {
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	dir := t.TempDir()
	sites := filepath.Join(dir, "sites")
	os.MkdirAll(filepath.Join(sites, "4", "public"), 0o755)
	os.WriteFile(filepath.Join(sites, "4", "public", "index.html"), []byte("v1"), 0o644)
	os.MkdirAll(filepath.Join(sites, "4", "tmp"), 0o755)
	os.WriteFile(filepath.Join(sites, "4", "tmp", "session"), []byte("x"), 0o644)
	_, c := served(t, Config{SitesRoot: sites, Restic: bin, StateDir: filepath.Join(dir, "state"), AgentDataDir: filepath.Join(dir, "agent")})
	ctx := context.Background()
	repo := agentproto.BackupRepo{Kind: agentproto.StorageLocal, Path: filepath.Join(dir, "repo"), Password: "pw"}
	run := func(req ResticRequest, stdio Stdio) (ResticResult, error) {
		req.Repo = repo
		return c.Restic(ctx, req, stdio, nil)
	}

	if res, err := c.Restic(ctx, ResticRequest{Op: ResticVersion}, Stdio{}, nil); err != nil || res.Version == "" {
		t.Fatalf("version: %+v %v", res, err)
	}
	// Before the repository exists: restic's exit code comes through.
	if _, err := run(ResticRequest{Op: ResticSnapshots, Host: "basalt"}, Stdio{}); !errors.Is(err, restic.ErrNoRepository) {
		t.Fatalf("no repository: %v", err)
	}
	if res, err := run(ResticRequest{Op: ResticInit}, Stdio{}); err != nil || !res.Created {
		t.Fatalf("init: %+v %v", res, err)
	}

	// A backup of what the agent writes into a pipe.
	backupStdin := func(content, file string, tags ...string) string {
		t.Helper()
		pr, pw, _ := os.Pipe()
		go func() {
			pw.WriteString(content)
			pw.Close()
		}()
		res, err := run(ResticRequest{Op: ResticBackupStdin, Host: "basalt", Tags: tags, StdinFilename: file}, Stdio{In: pr})
		pr.Close()
		if err != nil || res.Summary == nil || res.Summary.SnapshotID == "" {
			t.Fatalf("backup from a pipe: %+v %v", res, err)
		}
		return res.Summary.SnapshotID
	}
	dump := backupStdin("CREATE TABLE t (x int);\n", "mariadb-shop.sql", "res=site-4", "part=mariadb-shop")

	// A dump into the agent's pipe.
	pr, pw, _ := os.Pipe()
	got := make(chan string)
	go func() {
		b, _ := io.ReadAll(pr)
		got <- string(b)
	}()
	_, err = run(ResticRequest{Op: ResticDump, Snapshot: dump, Path: "/mariadb-shop.sql"}, Stdio{Out: pw})
	pw.Close()
	if s := <-got; err != nil || s != "CREATE TABLE t (x int);\n" {
		t.Fatalf("dump: %q %v", s, err)
	}

	// The agent stops a backup before the end of its input (its dump
	// failed): no snapshot is left.
	pr, pw, _ = os.Pipe()
	pw.WriteString("half a dump")
	sctx, stop := context.WithCancel(ctx)
	time.AfterFunc(500*time.Millisecond, stop)
	_, err = c.Restic(sctx, ResticRequest{Op: ResticBackupStdin, Repo: repo, Host: "basalt", Tags: []string{"res=site-4", "part=mariadb-cut"},
		StdinFilename: "mariadb-cut.sql"}, Stdio{In: pr}, nil)
	pr.Close()
	pw.Close()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped backup: %v", err)
	}
	if res, err := run(ResticRequest{Op: ResticSnapshots, Host: "basalt", Tags: []string{"part=mariadb-cut"}}, Stdio{}); err != nil || len(res.Snapshots) != 0 {
		t.Fatalf("a stopped backup left %+v (%v)", res.Snapshots, err)
	}

	// What a restore must not bring back as it was: a setuid file, a
	// pipe. A setgid folder it keeps.
	site := filepath.Join(sites, "4")
	os.WriteFile(filepath.Join(site, "public", "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.Chmod(filepath.Join(site, "public", "run.sh"), 0o755|fs.ModeSetuid)
	if mode(t, filepath.Join(site, "public", "run.sh"))&fs.ModeSetuid == 0 {
		t.Fatal("no setuid file to restore")
	}
	if err := syscall.Mkfifo(filepath.Join(site, "public", "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(site, "public", "up"), 0o750)
	os.Chmod(filepath.Join(site, "public", "up"), 0o750|fs.ModeSetgid)
	upMode := mode(t, filepath.Join(site, "public", "up"))
	os.Chmod(site, 0o751)

	// A site's files, by id: the temporary files stay out.
	res, err := run(ResticRequest{Op: ResticBackupSite, Host: "basalt", Tags: []string{"res=site-4", "part=files"}, SiteID: 4}, Stdio{})
	if err != nil || res.Summary == nil {
		t.Fatalf("backup of the site: %+v %v", res, err)
	}
	files := res.Summary.SnapshotID
	ls, err := run(ResticRequest{Op: ResticList, Snapshot: files, Path: filepath.Join(sites, "4", "tmp")}, Stdio{})
	if err != nil || ls.Node == nil || len(ls.Children) != 0 {
		t.Fatalf("tmp in the snapshot: %+v %v", ls, err)
	}

	// Restoring: only a snapshot of this site's files, over its folder.
	os.WriteFile(filepath.Join(sites, "4", "public", "index.html"), []byte("v2"), 0o644)
	os.WriteFile(filepath.Join(sites, "4", "public", "new.html"), []byte("new"), 0o644)
	for _, bad := range []ResticRequest{
		{Op: ResticRestoreSite, Snapshot: files, SiteID: 5},
		{Op: ResticRestoreSite, Snapshot: dump, SiteID: 4},
	} {
		if _, err := run(bad, Stdio{}); err == nil || !strings.Contains(err.Error(), "not of this site") {
			t.Fatalf("restore %+v: %v", bad, err)
		}
	}
	// The folder keeps its own mode, not the snapshot's; what a stopped
	// restore left aside goes.
	os.Chmod(site, 0o750)
	os.MkdirAll(filepath.Join(sites, restoreDir, "4", "public"), 0o755)
	if _, err := run(ResticRequest{Op: ResticRestoreSite, Snapshot: files, SiteID: 4}, Stdio{}); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, site); m != fs.ModeDir|0o750 {
		t.Fatalf("the site's folder after a restore: %v", m)
	}
	if m := mode(t, filepath.Join(site, "public", "run.sh")); m != 0o755 {
		t.Fatalf("a setuid file restored as %v", m)
	}
	if m := mode(t, filepath.Join(site, "public", "up")); m != upMode {
		t.Fatalf("a setgid folder restored as %v, was %v", m, upMode)
	}
	if _, err := os.Lstat(filepath.Join(site, "public", "pipe")); !os.IsNotExist(err) {
		t.Fatalf("a pipe came back: %v", err)
	}
	if left, err := os.ReadDir(filepath.Join(sites, restoreDir)); err != nil || len(left) != 0 || mode(t, filepath.Join(sites, restoreDir)) != fs.ModeDir|0o700 {
		t.Fatalf("left aside: %v %v", left, err)
	}
	if b, _ := os.ReadFile(filepath.Join(sites, "4", "public", "index.html")); string(b) != "v1" {
		t.Fatalf("restored %q", b)
	}
	if _, err := os.Stat(filepath.Join(sites, "4", "public", "new.html")); !os.IsNotExist(err) {
		t.Fatal("a file not in the snapshot survived the restore")
	}

	// A zip of a folder, into a file of the agent.
	out, _ := os.Create(filepath.Join(dir, "public.zip"))
	_, err = run(ResticRequest{Op: ResticDumpTree, Snapshot: files, Path: filepath.Join(sites, "4", "public"), Format: "zip"}, Stdio{Out: out})
	out.Close()
	if b, _ := os.ReadFile(filepath.Join(dir, "public.zip")); err != nil || !bytes.HasPrefix(b, []byte("PK")) {
		t.Fatalf("zip: %v", err)
	}

	if _, err := run(ResticRequest{Op: ResticForget, Host: "basalt", Tags: []string{"res=site-4"}, Keep: restic.Keep{Last: 1}}, Stdio{}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ResticRequest{Op: ResticCheck, ReadData: "1/52"}, Stdio{}); err != nil {
		t.Fatal(err)
	}
}

// TestResticChecksRequests: what the agent names is checked before restic
// runs.
func TestResticChecksRequests(t *testing.T) {
	dir := t.TempDir()
	sites := filepath.Join(dir, "sites")
	h, err := New(Config{SitesRoot: sites, Restic: "restic", StateDir: filepath.Join(dir, "state"), AgentDataDir: filepath.Join(dir, "agent")}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	good := agentproto.BackupRepo{Kind: agentproto.StorageLocal, Path: filepath.Join(dir, "repo"), Password: "pw"}
	in := func(p string) agentproto.BackupRepo {
		r := good
		r.Path = p
		return r
	}
	pr, pw, _ := os.Pipe()
	defer pr.Close()
	defer pw.Close()
	for name, c := range map[string]struct {
		req   ResticRequest
		stdio Stdio
		want  string
	}{
		"repo in the sites":        {ResticRequest{Op: ResticInit, Repo: in(filepath.Join(sites, "4", "repo"))}, Stdio{}, "must not contain"},
		"repo in the agent's data": {ResticRequest{Op: ResticInit, Repo: in(filepath.Join(dir, "agent", "r"))}, Stdio{}, "must not contain"},
		"repo in the helper's":     {ResticRequest{Op: ResticInit, Repo: in(filepath.Join(dir, "state"))}, Stdio{}, "must not contain"},
		"repo under /etc":          {ResticRequest{Op: ResticInit, Repo: in("/etc/basalt")}, Stdio{}, "cannot be under /etc"},
		"tag with a comma":         {ResticRequest{Op: ResticSnapshots, Repo: good, Host: "basalt", Tags: []string{"a,b"}}, Stdio{}, "invalid tag"},
		"option as a tag":          {ResticRequest{Op: ResticSnapshots, Repo: good, Host: "basalt", Tags: []string{"--password-command=id"}}, Stdio{}, "invalid tag"},
		"relative path":            {ResticRequest{Op: ResticList, Repo: good, Snapshot: "0123abcd", Path: "etc"}, Stdio{}, "invalid path"},
		"option as a snapshot":     {ResticRequest{Op: ResticList, Repo: good, Snapshot: "--help", Path: "/"}, Stdio{}, "invalid snapshot"},
		"site 0":                   {ResticRequest{Op: ResticBackupSite, Repo: good, Host: "basalt", SiteID: 0}, Stdio{}, "invalid site"},
		"file with a slash":        {ResticRequest{Op: ResticBackupStdin, Repo: good, Host: "basalt", StdinFilename: "../x"}, Stdio{In: pr}, "invalid file name"},
		"stdin without backup":     {ResticRequest{Op: ResticSnapshots, Repo: good, Host: "basalt"}, Stdio{In: pr}, "standard input"},
		"dump without output":      {ResticRequest{Op: ResticDump, Repo: good, Snapshot: "0123abcd", Path: "/x"}, Stdio{}, "standard output"},
		"unknown":                  {ResticRequest{Op: "unlock", Repo: good}, Stdio{}, "unknown restic command"},
	} {
		if _, err := h.Restic(ctx, c.req, c.stdio, nil); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sites, "4", "repo")); !os.IsNotExist(err) {
		t.Error("a refused repository was created")
	}
}

// TestUpdateThroughHelper: the helper installs only a binary matching the
// checksums signed with its own key, whatever the agent hands it.
func TestUpdateThroughHelper(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	key := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	exe := filepath.Join(t.TempDir(), "basalt")
	script := func(v string) []byte { return []byte("#!/bin/sh\necho 'basalt " + v + " (commit x, built y)'\n") }
	os.WriteFile(exe, script("v1.0.0"), 0o755)
	restarted := make(chan []string, 1)
	_, c := served(t, Config{SitesRoot: t.TempDir(), StateDir: t.TempDir(), ReleaseKey: key, UpdateExe: exe,
		Restart: func(units []string) error { restarted <- units; return nil }})
	ctx := context.Background()
	if ok, err := c.Updatable(ctx); err != nil || !ok {
		t.Fatalf("updatable: %v %v", ok, err)
	}
	name := release.BinaryName(runtime.GOARCH)
	sign := func(s ssh.Signer, bin []byte) agentproto.UpdatePrepare {
		sum := sha256.Sum256(bin)
		sums := fmt.Sprintf("%s  %s\n%s  %s\n", hex.EncodeToString(sum[:]), name, strings.Repeat("0", 64), release.VersionFile)
		sig, _ := sshsig.Sign([]byte(sums), s, sshsig.ReleaseNamespace)
		return agentproto.UpdatePrepare{Version: "v1.1.0", Sums: []byte(sums), Sig: sig}
	}
	file := func(b []byte) *os.File {
		p := filepath.Join(t.TempDir(), "bin")
		os.WriteFile(p, b, 0o600)
		f, _ := os.Open(p)
		t.Cleanup(func() { f.Close() })
		return f
	}
	evil := []byte("#!/bin/sh\necho 'basalt v1.1.0 (commit x, built y)'; id\n")
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(otherPriv)
	if err := c.StageUpdate(ctx, sign(other, evil), file(evil)); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("signed by another key: %v", err)
	}
	newBin := script("v1.1.0")
	if err := c.StageUpdate(ctx, sign(signer, newBin), file(evil)); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("another binary than signed: %v", err)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatal("a refused binary was left staged")
	}
	if err := c.ActivateUpdate(ctx, agentproto.UpdateActivate{Version: "v1.1.0"}); err == nil {
		t.Fatal("activated without staging")
	}
	if err := c.StageUpdate(ctx, sign(signer, newBin), file(newBin)); err != nil {
		t.Fatal(err)
	}
	if err := c.ActivateUpdate(ctx, agentproto.UpdateActivate{Version: "v1.1.0"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); !bytes.Equal(b, newBin) {
		t.Fatal("binary not replaced")
	}
	select {
	case u := <-restarted:
		if strings.Join(u, " ") != "basalt-proxy.service basalt-agent.service basalt-agent-helper.service" {
			t.Fatalf("restarted %v", u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no restart")
	}
}

// TestFilesOnlyWithMarker: files must come with the marker, and a
// connection without files is read from its first byte.
func TestFilesOnlyWithMarker(t *testing.T) {
	_, c := served(t, Config{SitesRoot: t.TempDir(), StateDir: t.TempDir()})
	if _, err := c.Updatable(context.Background()); err != nil {
		t.Fatalf("plain connection: %v", err)
	}
	if err := sendFiles(nil, nil); err == nil {
		t.Fatal("no files sent as files")
	}
}

// mode is a file's mode, not following a symbolic link.
func mode(t *testing.T, p string) fs.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}
