//go:build linux_integration

package dns

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
)

// TestLinuxDNSListenBesideStub binds :53 while a stub resolver holds
// 127.0.0.53:53 the way systemd-resolved does (Ubuntu's default).
func TestLinuxDNSListenBesideStub(t *testing.T) {
	ctx := t.Context()
	lc := net.ListenConfig{Control: reuseAddr}
	stubUDP, err := lc.ListenPacket(ctx, "udp", "127.0.0.53:53")
	if err != nil {
		t.Fatal(err)
	}
	defer stubUDP.Close()
	stubTCP, err := lc.Listen(ctx, "tcp", "127.0.0.53:53")
	if err != nil {
		t.Fatal(err)
	}
	defer stubTCP.Close()

	k, err := Listen(ctx, ":53")
	if err != nil {
		t.Fatalf("listen beside the stub: %v", err)
	}
	t.Logf("bound %v", k.Addrs())
	s := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := s.SetZones([]Zone{{Name: "example.test", Serial: 1, Records: []Record{{Name: "@", Type: TypeA, TTL: 60, Value: "192.0.2.1"}}}}); err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Serve(sctx, k) }()

	// Answered on loopback and on the container's own address, over UDP and TCP.
	var targets []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			targets = append(targets, ipn.IP.String())
		}
	}
	if len(targets) < 2 {
		t.Fatalf("expected loopback and an external address, got %v", targets)
	}
	for _, ip := range targets {
		for _, proto := range []string{"udp", "tcp"} {
			q := new(mdns.Msg)
			q.SetQuestion("example.test.", mdns.TypeA)
			r, _, err := (&mdns.Client{Net: proto, Timeout: 2 * time.Second}).Exchange(q, net.JoinHostPort(ip, "53"))
			if err != nil || len(r.Answer) != 1 {
				t.Fatalf("%s %s: %v %v", proto, ip, r, err)
			}
		}
	}
	// The stub keeps its address.
	go func() {
		if c, err := stubTCP.Accept(); err == nil {
			c.Close()
		}
	}()
	c, err := net.DialTimeout("tcp", "127.0.0.53:53", 2*time.Second)
	if err != nil {
		t.Fatalf("stub lost its address: %v", err)
	}
	c.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// Without the stub the wildcard address is bound as one socket.
	stubUDP.Close()
	stubTCP.Close()
	k, err = Listen(ctx, ":53")
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if len(k.Packet) != 1 || len(k.Stream) != 1 {
		t.Fatalf("wildcard: %v", k.Addrs())
	}
}
