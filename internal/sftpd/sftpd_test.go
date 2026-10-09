package sftpd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/auth"
	"github.com/wiefix/basalt/internal/files"
)

type fixture struct {
	addr    string
	base    string
	hostKey ssh.PublicKey
	userKey ssh.Signer
}

func start(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	site := filepath.Join(base, "1")
	os.MkdirAll(filepath.Join(site, "public"), 0o755)
	os.WriteFile(filepath.Join(site, "public", "index.html"), []byte("hello"), 0o644)
	os.MkdirAll(filepath.Join(base, "2"), 0o755)
	os.WriteFile(filepath.Join(base, "2", "config.php"), []byte("SECRET"), 0o600)
	os.Symlink(filepath.Join(base, "2", "config.php"), filepath.Join(site, "public", "leak"))

	hostKey, err := LoadOrCreateHostKey(filepath.Join(t.TempDir(), "host_key"))
	if err != nil {
		t.Fatal(err)
	}
	hasher := auth.NewHasher(auth.Argon2Params{Memory: 1024, Time: 1, Threads: 1}, 2)
	hash, _ := hasher.Hash(context.Background(), "site password 1")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	userKey, _ := ssh.NewSignerFromKey(priv)

	srv := New(slog.New(slog.NewTextHandler(io.Discard, nil)), hostKey, hasher, func(id int64) (*files.Site, error) {
		return files.Open(filepath.Join(base, strconv.FormatInt(id, 10)))
	})
	srv.SetUsers([]User{
		{Name: "blog", SiteID: 1, PasswordHash: hash},
		{Name: "deploy", SiteID: 1, PublicKeys: []string{string(ssh.MarshalAuthorizedKey(userKey.PublicKey())), "garbage"}},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx, ln)
	return &fixture{addr: ln.Addr().String(), base: base, hostKey: hostKey.PublicKey(), userKey: userKey}
}

func (f *fixture) dial(user string, auths ...ssh.AuthMethod) (*ssh.Client, error) {
	return ssh.Dial("tcp", f.addr, &ssh.ClientConfig{
		User: user, Auth: auths, HostKeyCallback: ssh.FixedHostKey(f.hostKey),
	})
}

func (f *fixture) sftp(t *testing.T, user string, a ssh.AuthMethod) *sftp.Client {
	t.Helper()
	c, err := f.dial(user, a)
	if err != nil {
		t.Fatalf("dial %s: %v", user, err)
	}
	sc, err := sftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sc.Close(); c.Close() })
	return sc
}

func TestAuthentication(t *testing.T) {
	f := start(t)
	if _, err := f.dial("blog", ssh.Password("wrong")); err == nil {
		t.Fatal("wrong password accepted")
	}
	if _, err := f.dial("nobody", ssh.Password("site password 1")); err == nil {
		t.Fatal("unknown user accepted")
	}
	if _, err := f.dial("deploy", ssh.Password("")); err == nil {
		t.Fatal("key-only account accepted an empty password")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherKey, _ := ssh.NewSignerFromKey(other)
	if _, err := f.dial("deploy", ssh.PublicKeys(otherKey)); err == nil {
		t.Fatal("unknown key accepted")
	}
	f.sftp(t, "blog", ssh.Password("site password 1"))
	f.sftp(t, "deploy", ssh.PublicKeys(f.userKey))
}

func TestJailAndOperations(t *testing.T) {
	f := start(t)
	c := f.sftp(t, "blog", ssh.Password("site password 1"))

	if wd, _ := c.Getwd(); wd != "/" {
		t.Fatalf("cwd = %q; want /", wd)
	}
	if rp, _ := c.RealPath("../../.."); rp != "/" {
		t.Fatalf("realpath(../../..) = %q", rp)
	}
	entries, err := c.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "public" {
		t.Fatalf("root = %v", entries)
	}

	// Upload, read back, rename, chmod.
	w, err := c.Create("/public/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "uploaded")
	w.Close()
	if b, _ := os.ReadFile(filepath.Join(f.base, "1", "public", "new.txt")); string(b) != "uploaded" {
		t.Fatalf("on disk: %q", b)
	}
	if err := c.PosixRename("/public/new.txt", "/public/index.html"); err != nil {
		t.Fatal(err)
	}
	r, _ := c.Open("/public/index.html")
	b, _ := io.ReadAll(r)
	r.Close()
	if string(b) != "uploaded" {
		t.Fatalf("after posix-rename: %q", b)
	}
	if err := c.Chmod("/public/index.html", 0o4755); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(f.base, "1", "public", "index.html")); fi.Mode()&os.ModeSetuid != 0 || fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v; setuid must be stripped", fi.Mode())
	}
	if err := c.Mkdir("/public/dir"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDirectory("/public/dir"); err != nil {
		t.Fatal(err)
	}

	// Escapes.
	for _, p := range []string{"/public/leak", "../2/config.php", "/../../2/config.php"} {
		if r, err := c.Open(p); err == nil {
			b, _ := io.ReadAll(r)
			r.Close()
			if strings.Contains(string(b), "SECRET") {
				t.Fatalf("read %s escaped the jail", p)
			}
		}
	}
	if err := c.Symlink("/etc/passwd", "/public/pw"); err == nil {
		t.Fatal("symlink creation allowed")
	}
	if err := c.Link("/public/index.html", "/public/hard"); err == nil {
		t.Fatal("hard link allowed")
	}
	if err := c.Chown("/public/index.html", 0, 0); err == nil {
		t.Fatal("chown allowed")
	}
	if err := c.Remove("/"); err == nil {
		t.Fatal("removed the site root")
	}
	if w, err := c.Create("/../2/config.php"); err == nil {
		io.WriteString(w, "pwned")
		w.Close()
	}
	if b, _ := os.ReadFile(filepath.Join(f.base, "2", "config.php")); string(b) != "SECRET" {
		t.Fatalf("other site modified: %q", b)
	}

	names := []string{}
	es, _ := c.ReadDir("/public")
	for _, e := range es {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "index.html,leak" {
		t.Fatalf("public = %v", names)
	}
}

func TestOnlySFTPIsOffered(t *testing.T) {
	f := start(t)
	c, err := f.dial("blog", ssh.Password("site password 1"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s, _ := c.NewSession()
	if err := s.Shell(); err == nil {
		t.Fatal("shell allowed")
	}
	s.Close()
	s, _ = c.NewSession()
	if _, err := s.Output("id"); err == nil {
		t.Fatal("exec allowed")
	}
	s.Close()
	if _, err := c.Dial("tcp", "127.0.0.1:22"); err == nil {
		t.Fatal("port forwarding allowed")
	}
	if _, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Fatal("remote forwarding allowed")
	}
}

func TestHostKeyPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	a, _ := LoadOrCreateHostKey(p)
	b, _ := LoadOrCreateHostKey(p)
	if string(a.PublicKey().Marshal()) != string(b.PublicKey().Marshal()) {
		t.Fatal("host key changed between loads")
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("host key mode %v", fi.Mode().Perm())
	}
}
