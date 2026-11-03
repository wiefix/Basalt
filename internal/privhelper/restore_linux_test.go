package privhelper

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/wiefix/basalt/internal/agentproto"
)

// TestRestoreSiteAsRoot: a snapshot restored by root gives the site
// nothing of anyone else's: owners become the site's, devices go, setuid
// and extended attributes too (make test-linux runs it as root).
func TestRestoreSiteAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	bin, err := exec.LookPath("restic")
	if err != nil {
		t.Skip("restic is not installed")
	}
	dir := t.TempDir()
	sites := filepath.Join(dir, "sites")
	site := filepath.Join(sites, "7")
	os.MkdirAll(filepath.Join(site, "public"), 0o755)
	const owner = 54321 // no site user: the folder's owner gets the files
	os.Lchown(site, owner, owner)
	os.Chmod(site, 0o750|fs.ModeSetgid)

	evil := filepath.Join(site, "public", "evil")
	os.WriteFile(evil, []byte("#!/bin/sh\n"), 0o755)
	os.Chown(evil, 0, 0)
	os.Chmod(evil, 0o755|fs.ModeSetuid|fs.ModeSetgid)
	if err := unix.Lsetxattr(evil, "user.basalt", []byte("x"), 0); err != nil {
		t.Logf("no extended attributes here: %v", err)
	}
	if err := unix.Mknod(filepath.Join(site, "public", "null"), unix.S_IFCHR|0o666, int(unix.Mkdev(1, 3))); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(site, "public", "link")
	os.Symlink("/etc/passwd", link)

	h, err := New(Config{SitesRoot: sites, Restic: bin, StateDir: filepath.Join(dir, "state"), AgentDataDir: filepath.Join(dir, "agent")}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	repo := agentproto.BackupRepo{Kind: agentproto.StorageLocal, Path: filepath.Join(dir, "repo"), Password: "pw"}
	run := func(req ResticRequest) ResticResult {
		t.Helper()
		req.Repo = repo
		res, err := h.Restic(ctx, req, Stdio{}, nil)
		if err != nil {
			t.Fatalf("%s: %v", req.Op, err)
		}
		return res
	}
	run(ResticRequest{Op: ResticInit})
	snap := run(ResticRequest{Op: ResticBackupSite, Host: "basalt", Tags: []string{"res=site-7", "part=files"}, SiteID: 7}).Summary.SnapshotID
	run(ResticRequest{Op: ResticRestoreSite, Snapshot: snap, SiteID: 7})

	for _, p := range []string{site, filepath.Join(site, "public"), evil, link} {
		var st syscall.Stat_t
		if err := syscall.Lstat(p, &st); err != nil {
			t.Fatal(err)
		}
		if st.Uid != owner || st.Gid != owner {
			t.Errorf("%s belongs to %d:%d", p, st.Uid, st.Gid)
		}
	}
	if m := mode(t, site); m != fs.ModeDir|fs.ModeSetgid|0o750 {
		t.Errorf("the site's folder: %v", m)
	}
	if m := mode(t, evil); m != 0o755 {
		t.Errorf("the setuid file: %v", m)
	}
	if n, _ := unix.Llistxattr(evil, nil); n != 0 {
		t.Errorf("extended attributes came back (%d bytes)", n)
	}
	if _, err := os.Lstat(filepath.Join(site, "public", "null")); !os.IsNotExist(err) {
		t.Errorf("the device came back: %v", err)
	}
	if target, err := os.Readlink(link); err != nil || target != "/etc/passwd" {
		t.Errorf("the link: %q %v", target, err)
	}
	var st syscall.Stat_t
	if syscall.Stat("/etc/passwd", &st); st.Uid != 0 {
		t.Error("the restore followed a link")
	}
}
