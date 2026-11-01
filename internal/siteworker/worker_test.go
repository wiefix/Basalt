package siteworker

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
)

func siteDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "7")
	if err := os.MkdirAll(filepath.Join(d, "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "public", "index.html"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

// startFiles runs serveFiles on pipes, as the agent does with a process.
func startFiles(t *testing.T, dir string) *agentproto.Peer {
	t.Helper()
	toWorker, fromAgent := io.Pipe()
	toAgent, fromWorker := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serveFiles(dir, toWorker, fromWorker)
		fromWorker.Close()
	}()
	peer := agentproto.NewPeer(toAgent, fromAgent, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go peer.Run(ctx)
	t.Cleanup(func() {
		cancel()
		fromAgent.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker did not leave when the agent closed it")
		}
	})
	return peer
}

func call(t *testing.T, p *agentproto.Peer, req agentproto.FilesRequest) (*agentproto.FilesResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp := &agentproto.FilesResponse{}
	err := p.Call(ctx, MsgFiles, req, resp)
	return resp, err
}

func TestFilesOverPipes(t *testing.T) {
	dir := siteDir(t)
	p := startFiles(t, dir)

	resp, err := call(t, p, agentproto.FilesRequest{Op: agentproto.FilesList, Path: "/public"})
	if err != nil || len(resp.Entries) != 1 || resp.Entries[0].Name != "index.html" {
		t.Fatalf("list = %+v, %v", resp, err)
	}
	if _, err := call(t, p, agentproto.FilesRequest{Op: agentproto.FilesWrite, Path: "/public/a.txt", Data: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "public", "a.txt")); string(b) != "abc" {
		t.Fatalf("written %q", b)
	}

	// An upload keeps its state in the worker between requests.
	up, err := call(t, p, agentproto.FilesRequest{Op: agentproto.FilesUploadBegin, Path: "/public/big.bin"})
	if err != nil || up.UploadID == "" {
		t.Fatalf("begin = %+v, %v", up, err)
	}
	for i, part := range []string{"hello ", "world"} {
		off := int64(i * 6)
		if _, err := call(t, p, agentproto.FilesRequest{Op: agentproto.FilesUploadChunk, UploadID: up.UploadID, Offset: off, Data: []byte(part)}); err != nil {
			t.Fatal(err)
		}
	}
	if c, err := call(t, p, agentproto.FilesRequest{Op: agentproto.FilesUploadCommit, UploadID: up.UploadID}); err != nil || c.Size != 11 {
		t.Fatalf("commit = %+v, %v", c, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "public", "big.bin")); string(b) != "hello world" {
		t.Fatalf("uploaded %q", b)
	}
	if _, err := call(t, p, agentproto.FilesRequest{Op: agentproto.FilesUploadChunk, UploadID: up.UploadID, Data: []byte("x")}); err == nil || !strings.Contains(err.Error(), "unknown or expired upload") {
		t.Fatalf("chunk after commit: %v", err)
	}

	// Errors name paths in the site, never the host's.
	_, err = call(t, p, agentproto.FilesRequest{Op: agentproto.FilesRead, Path: "/public/missing", Length: 10})
	if err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("read missing: %v", err)
	}
	// A symlink out of the site leads nowhere.
	if err := os.Symlink("/etc", filepath.Join(dir, "public", "etc")); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, p, agentproto.FilesRequest{Op: agentproto.FilesList, Path: "/public/etc"}); err == nil {
		t.Fatal("listed a folder outside the site through a symlink")
	}
}

func TestFilesExpireUploads(t *testing.T) {
	dir := siteDir(t)
	f := NewFiles(func() (*files.Site, error) { return files.Open(dir) })
	if _, err := f.Handle(context.Background(), agentproto.FilesRequest{Op: agentproto.FilesUploadBegin, Path: "/public/x"}); err != nil {
		t.Fatal(err)
	}
	f.Expire(time.Hour)
	if f.Uploads() != 1 {
		t.Fatal("a fresh upload expired")
	}
	f.Expire(0)
	if f.Uploads() != 0 {
		t.Fatal("an idle upload stayed")
	}
	// The temporary is gone with it.
	entries, _ := os.ReadDir(filepath.Join(dir, "public"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".basalt-upload-") {
			t.Fatalf("temporary %s left behind", e.Name())
		}
	}
}

func TestSFTPMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a worker refuses to run as root")
	}
	dir := siteDir(t)
	toWorker, fromClient := io.Pipe()
	toClient, fromWorker := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- Main([]string{ModeSFTP, "--dir", dir}, toWorker, fromWorker, &stderr)
		fromWorker.Close()
	}()
	sc, err := sftp.NewClientPipe(toClient, fromClient)
	if err != nil {
		t.Fatal(err)
	}
	f, err := sc.Create("/public/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("via sftp"))
	f.Close()
	if b, _ := os.ReadFile(filepath.Join(dir, "public", "new.txt")); string(b) != "via sftp" {
		t.Fatalf("written %q", b)
	}
	if _, err := sc.ReadDir("/../../"); err != nil {
		t.Fatalf("root listing: %v", err)
	}
	if err := sc.Symlink("/etc/passwd", "/public/p"); err == nil {
		t.Fatal("symlink allowed")
	}
	sc.Close()
	fromClient.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("worker exit %d: %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not leave with the client")
	}
}

func TestMainArgs(t *testing.T) {
	for _, args := range [][]string{nil, {ModeFiles}, {"shell", "--dir", "/tmp"}, {ModeFiles, "--dir", "/tmp", "extra"}} {
		var stderr bytes.Buffer
		if Main(args, strings.NewReader(""), io.Discard, &stderr) == 0 {
			t.Errorf("%q accepted", args)
		}
	}
	if got := Args(ModeSFTP, "/srv/s/1"); strings.Join(got, " ") != "site-worker sftp --dir /srv/s/1" {
		t.Errorf("Args = %q", got)
	}
}
