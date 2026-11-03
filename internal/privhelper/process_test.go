package privhelper

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSiteProcess: the site's "PHP" (cat here) runs in the site's folder
// on the socket it is handed and answers with its exit code; requests for
// other programs, folders out of the site or missing sites are refused;
// stopping the request kills it.
func TestSiteProcess(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "1", "app"), 0o755)
	h, err := New(Config{SitesRoot: root, PHPBinary: func(v string) string {
		if v == "8.4" {
			return "/bin/sh"
		}
		return "/nonexistent"
	}}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context, req SiteProcess, stdin string) (string, SiteProcessResult, error) {
		mine, theirs, err := SocketPair()
		if err != nil {
			t.Fatal(err)
		}
		defer theirs.Close()
		c, _ := net.FileConn(mine)
		mine.Close()
		conn := c.(*net.UnixConn)
		defer conn.Close()
		go func() { io.WriteString(conn, stdin); conn.CloseWrite() }()
		out := make(chan string, 1)
		go func() { b, _ := io.ReadAll(conn); out <- string(b) }()
		res, err := h.SiteProcess(ctx, req, theirs, nil)
		theirs.Close()
		return <-out, res, err
	}
	out, res, err := run(context.Background(), SiteProcess{SiteID: 1, PHP: "8.4", Dir: "app", Env: []string{"X=1"},
		Args: []string{"-c", `cat; pwd; echo "$X"; exit 3`}}, "in\n")
	if err != nil || res.Exit != 3 || !strings.HasPrefix(out, "in\n") || !strings.Contains(out, "/1/app\n1\n") {
		t.Fatalf("out %q, %+v, %v", out, res, err)
	}
	for _, bad := range []SiteProcess{
		{SiteID: 2, PHP: "8.4"},
		{SiteID: 1, PHP: "8"},
		{SiteID: 1, PHP: "8.4", Dir: "../2"},
		{SiteID: 1, PHP: "8.4", Dir: "/etc"},
		{SiteID: 1, PHP: "8.4", Env: []string{"novalue"}},
		{SiteID: 1, Worker: "shell"},
		{SiteID: 1, Worker: "files", Args: []string{"x"}},
		{SiteID: 0, PHP: "8.4"},
	} {
		if _, _, err := run(context.Background(), bad, ""); err == nil {
			t.Errorf("ran %+v", bad)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := run(ctx, SiteProcess{SiteID: 1, PHP: "8.4", Args: []string{"-c", "sleep 30"}}, ""); err == nil || time.Since(start) > 10*time.Second {
		t.Errorf("a stopped request: %v after %v", err, time.Since(start))
	}
}
