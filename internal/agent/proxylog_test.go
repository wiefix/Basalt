package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/proxy"
)

func TestFormatProxyLine(t *testing.T) {
	for _, c := range []struct {
		line, want string
		min        int
	}{
		{`{"level":"error","ts":1792250000.5,"logger":"tls.obtain","msg":"could not get certificate from issuer","identifier":"a.example","attempt":2,"error":"HTTP 429 urn:ietf:params:acme:error:rateLimited - too many"}`,
			`2026-10-17T15:13:20.500Z ERROR tls.obtain: could not get certificate from issuer identifier=a.example attempt=2 error="HTTP 429 urn:ietf:params:acme:error:rateLimited - too many"`, 0},
		{`{"level":"info","ts":1792250000,"msg":"redirected default logger","from":"stderr","to":""}`,
			`2026-10-17T15:13:20.000Z INFO redirected default logger from=stderr to=""`, 0},
		// Headers never leave, at any depth.
		{`{"level":"error","ts":1792250000,"logger":"http.log.error","msg":"dial tcp: refused","request":{"method":"GET","uri":"/x","headers":{"Cookie":["s=1"]}},"resp_headers":{"A":["b"]},"status":502}`,
			`2026-10-17T15:13:20.000Z ERROR http.log.error: dial tcp: refused request={"method":"GET","uri":"/x"} status=502`, 0},
		{`not json`, `not json`, 0},
		{`{"level":"info","ts":1,"logger":"tls","msg":"x"}`, "", 2},
		{`{"level":"warn","ts":1,"logger":"tls","msg":"x"}`, "1970-01-01T00:00:01.000Z WARN tls: x", 2},
		{`{"level":"info","ts":1,"logger":"admin.api","msg":"received request","uri":"/id/x"}`, "", 0},
		{`{"level":"warn","ts":1,"logger":"admin","msg":"admin endpoint on open interface; host checking disabled"}`, "", 0},
		{`{"level":"error","ts":1,"logger":"admin.api","msg":"request error","error":"loading config: bad"}`,
			`1970-01-01T00:00:01.000Z ERROR admin.api: request error error="loading config: bad"`, 0},
		{`{"level":"error","ts":1,"logger":"admin.api","msg":"request error","error":"unknown object ID 'basalt-1'","status_code":404}`, "", 0},
		{`{"level":"warn","ts":1,"logger":"http","msg":"HTTP/3 skipped because it requires TLS","network":"tcp","addr":":80"}`, "", 0},
		{"  ", "", 0},
	} {
		got, ok := formatProxyLine([]byte(c.line), c.min)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s:\n got %q %v\nwant %q", c.line, got, ok, c.want)
		}
	}
	long := fmt.Sprintf(`{"level":"error","ts":1,"msg":"m","error":"%s"}`, strings.Repeat("x", 10000))
	if got, _ := formatProxyLine([]byte(long), 0); len(got) > proxyLogMaxLine+10 || !strings.HasSuffix(got, "…") {
		t.Errorf("long value not cut: %d bytes", len(got))
	}
}

func testProxyLogAgent(t *testing.T) (*Agent, string) {
	dir := t.TempDir()
	a := &Agent{
		proxy: &proxyManager{opts: proxy.BuildOptions{Dir: dir}},
		paths: newHostPaths(Config{ProxyDir: dir}),
	}
	os.MkdirAll(filepath.Join(dir, "logs"), 0o755)
	return a, dir
}

func TestProxyLog(t *testing.T) {
	a, dir := testProxyLogAgent(t)
	if l, err := a.proxyLog(agentproto.ProxyLogRequest{Tail: 10}); err != nil || l.Log != "" {
		t.Fatalf("no log yet: %+v %v", l, err)
	}
	if _, err := a.proxyLog(agentproto.ProxyLogRequest{Tail: 10, Level: "debug"}); err == nil {
		t.Fatal("unknown level accepted")
	}
	var b strings.Builder
	for i := range 50000 { // more than proxyLogScan: only the end is read
		level := "info"
		if i%10 == 0 {
			level = "error"
		}
		fmt.Fprintf(&b, `{"level":%q,"ts":1792250000,"logger":"tls","msg":"line %d","storage":"FileStorage:%s/storage"}`+"\n", level, i, dir)
		fmt.Fprintf(&b, `{"level":"info","ts":1792250000,"logger":"admin.api","msg":"received request"}`+"\n")
	}
	os.WriteFile(proxy.OwnLogPath(dir), []byte(b.String()), 0o600)

	l, err := a.proxyLog(agentproto.ProxyLogRequest{Tail: 3})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(l.Log, "\n"), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], "line 49997 ") || !strings.Contains(lines[2], "line 49999 ") {
		t.Fatalf("tail: %q", l.Log)
	}
	if strings.Contains(l.Log, dir) || !strings.Contains(l.Log, "FileStorage:<proxy-data>/storage") {
		t.Fatalf("host path not replaced: %q", l.Log)
	}
	l, _ = a.proxyLog(agentproto.ProxyLogRequest{Tail: 2, Level: "error"})
	if l.Log != strings.Join([]string{
		"2026-10-17T15:13:20.000Z ERROR tls: line 49980 storage=FileStorage:<proxy-data>/storage",
		"2026-10-17T15:13:20.000Z ERROR tls: line 49990 storage=FileStorage:<proxy-data>/storage", ""}, "\n") {
		t.Fatalf("errors only: %q", l.Log)
	}
}

func TestFollowProxyLog(t *testing.T) {
	a, dir := testProxyLogAgent(t)
	path := proxy.OwnLogPath(dir)
	line := func(msg string) string {
		return fmt.Sprintf(`{"level":"error","ts":1,"logger":"tls","msg":%q,"path":"%s/x"}`+"\n", msg, dir)
	}
	appendTo := func(p, s string) {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(s)
		f.Close()
	}
	appendTo(path, line("old1")+line("old2"))

	var mu sync.Mutex
	var got strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.followProxyLog(ctx, agentproto.ProxyLogRequest{Tail: 1}, func(s string) error {
			mu.Lock()
			defer mu.Unlock()
			got.WriteString(s)
			return nil
		})
	}()
	wait := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			mu.Lock()
			s := got.String()
			mu.Unlock()
			if strings.Contains(s, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("waiting for %q, have %q", want, s)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	wait("old2")
	// A line written in two parts arrives once, whole.
	l := line("new1")
	appendTo(path, l[:20])
	time.Sleep(proxyLogPoll + 200*time.Millisecond)
	appendTo(path, l[20:])
	wait("new1")
	// Rotation: the rest of the old file, then the new one.
	appendTo(path, line("before-rotate"))
	os.Rename(path, filepath.Join(dir, "logs", "proxy-1.log"))
	appendTo(path, line("after-rotate"))
	wait("after-rotate")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s := got.String()
	if strings.Contains(s, "old1") || strings.Count(s, "new1") != 1 || !strings.Contains(s, "before-rotate") || strings.Contains(s, dir) {
		t.Fatalf("followed: %q", s)
	}
	if !strings.Contains(s, "ERROR tls: new1 path=<proxy-data>/x\n") {
		t.Fatalf("line format: %q", s)
	}
}
