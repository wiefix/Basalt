package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/proxy"
)

func TestAccessLog(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{proxy: &proxyManager{opts: proxy.BuildOptions{Dir: dir}}}
	if l, err := a.accessLog(agentproto.AccessLogRequest{SiteID: 7, Limit: 10}); err != nil || len(l.Entries) != 0 {
		t.Fatalf("no log yet: %+v %v", l, err)
	}
	var b strings.Builder
	// Enough lines that only the end of the file is read.
	for i := range 20000 {
		fmt.Fprintf(&b, `{"level":"info","ts":1792250000.%03d,"logger":"http.log.access.site-7","request":{"remote_ip":"192.0.2.1","client_ip":"198.51.100.%d","proto":"HTTP/2.0","method":"GET","host":"blog.example","uri":"/p/%d","headers":{"User-Agent":["curl/8"]}},"duration":0.0015,"size":512,"status":200}`+"\n", i%1000, i%250, i)
	}
	b.WriteString("not json\n")
	os.MkdirAll(filepath.Join(dir, "logs"), 0o755)
	os.WriteFile(proxy.AccessLogPath(dir, "site-7"), []byte(b.String()), 0o644)

	l, err := a.accessLog(agentproto.AccessLogRequest{SiteID: 7, Limit: 3})
	if err != nil || len(l.Entries) != 3 {
		t.Fatalf("entries = %+v, %v", l, err)
	}
	e := l.Entries[0]
	if e.URI != "/p/19999" || e.IP != "198.51.100.249" || e.Method != "GET" || e.Status != 200 || e.Bytes != 512 ||
		e.DurationMS != 1.5 || e.UserAgent != "curl/8" || e.Proto != "HTTP/2.0" || e.At.Unix() != 1792250000 {
		t.Fatalf("newest = %+v", e)
	}
	if l.Entries[2].URI != "/p/19997" {
		t.Fatalf("order: %+v", l.Entries)
	}
	if _, err := a.accessLog(agentproto.AccessLogRequest{}); err == nil {
		t.Fatal("no site or app accepted")
	}
	if _, err := a.accessLog(agentproto.AccessLogRequest{SiteID: 1, AppID: 2}); err == nil {
		t.Fatal("site and app at once accepted")
	}
}
