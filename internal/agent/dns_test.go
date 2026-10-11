package agent

import (
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/dns"
)

func TestDNSManager(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	zone := func(ip string) []dns.Zone {
		return []dns.Zone{{Name: "example.test", Serial: 1, Records: []dns.Record{{Name: "@", Type: dns.TypeA, TTL: 60, Value: ip}}}}
	}

	// Without a listener the zones are still kept.
	off := newDNSManager(Config{DataDir: dir}, quiet)
	if port, _ := off.Status(); port != 0 {
		t.Fatal("port without listener")
	}
	if err := off.Apply(agentproto.DNSApply{Revision: 2, Zones: zone("192.0.2.1")}); err != nil {
		t.Fatal(err)
	}
	bad := zone("not-an-ip")
	if err := off.Apply(agentproto.DNSApply{Revision: 3, Zones: bad}); err == nil {
		t.Fatal("invalid zone accepted")
	}

	// A listener serves the saved zones at once, before the panel calls.
	on := newDNSManager(Config{DataDir: dir, DNSListen: "127.0.0.1:0"}, quiet)
	port, listenErr := on.Status()
	if port == 0 || listenErr != "" {
		t.Fatalf("status %d %q", port, listenErr)
	}
	ctx := t.Context()
	go on.run(ctx)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ask := func() string {
		q := new(mdns.Msg)
		q.SetQuestion("example.test.", mdns.TypeA)
		r, _, err := (&mdns.Client{Timeout: 2 * time.Second}).Exchange(q, addr)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Answer) != 1 {
			return mdns.RcodeToString[r.Rcode]
		}
		return r.Answer[0].(*mdns.A).A.String()
	}
	if got := ask(); got != "192.0.2.1" {
		t.Fatalf("saved zone: %s", got)
	}
	// Outdated deliveries are ignored, newer ones served.
	on.Apply(agentproto.DNSApply{Revision: 1, Zones: zone("192.0.2.9")})
	if got := ask(); got != "192.0.2.1" {
		t.Fatalf("outdated revision applied: %s", got)
	}
	if err := on.Apply(agentproto.DNSApply{Revision: 4, Zones: zone("192.0.2.4")}); err != nil {
		t.Fatal(err)
	}
	if got := ask(); got != "192.0.2.4" {
		t.Fatalf("new revision: %s", got)
	}

	// A busy port is reported, not fatal.
	busy := newDNSManager(Config{DataDir: t.TempDir(), DNSListen: addr}, quiet)
	if port, listenErr := busy.Status(); port != 0 || listenErr == "" {
		t.Fatalf("busy port: %d %q", port, listenErr)
	}
}
