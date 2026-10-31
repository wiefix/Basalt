package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// echoServer stands in for a database container.
func echoServer(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func freeTCPPort(t *testing.T) int {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// echoes reports whether a connection to port is forwarded: it says
// "ping" and waits for the echo.
func echoes(port int) error {
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	return echoesOn(c)
}

func echoesOn(c net.Conn) error {
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		return err
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil {
		return err
	}
	if string(b) != "ping" {
		return fmt.Errorf("read %q", b)
	}
	return nil
}

func TestDBExpose(t *testing.T) {
	dir := t.TempDir()
	target := echoServer(t)
	asked := 0
	resolve := func(ctx context.Context, appID int64, service string, port int) (string, error) {
		asked++
		if appID != 7 || service != "pg" || port != 5432 {
			return "", errors.New("unknown database")
		}
		return target, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := newDBExposeManager(Config{DataDir: dir}, resolve, slog.New(slog.DiscardHandler))
	m.start(ctx)

	port := freeTCPPort(t)
	entry := agentproto.DBExpose{AppID: 7, Service: "pg", TargetPort: 5432, Port: port, Allow: []string{"127.0.0.1"}}
	apply := func(entries ...agentproto.DBExpose) agentproto.DBExposeResponse {
		t.Helper()
		resp, err := m.Apply(agentproto.DBExposeApply{Entries: entries})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := apply(entry); len(resp.Entries) != 1 || resp.Entries[0].Error != "" {
		t.Fatalf("apply = %+v", resp)
	}
	if err := echoes(port); err != nil {
		t.Fatalf("forwarding: %v", err)
	}
	if err := echoes(port); err != nil || asked != 1 {
		t.Fatalf("second connection: %v (target asked %d times)", err, asked)
	}

	// Connections from an address no longer allowed are closed, new ones
	// refused.
	open, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	if err := echoesOn(open); err != nil {
		t.Fatal(err)
	}
	entry.Allow = []string{"192.0.2.0/24"}
	apply(entry)
	if err := echoesOn(open); err == nil {
		t.Fatal("a connection from an address no longer allowed stays open")
	}
	if err := echoes(port); err == nil {
		t.Fatal("an address not allowed is forwarded")
	}

	// Limit per address.
	entry.Allow = []string{"127.0.0.0/8", "::1"}
	apply(entry)
	var conns []net.Conn
	for range dbExposeMaxConnsPerIP {
		c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err := echoesOn(c); err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	if err := echoes(port); err == nil {
		t.Fatal("more connections than the limit per address")
	}
	conns[0].Close()
	time.Sleep(100 * time.Millisecond)
	if err := echoes(port); err != nil {
		t.Fatalf("a connection after one closed: %v", err)
	}
	for _, c := range conns {
		c.Close()
	}

	// A port Basalt cannot listen on is reported; the others still apply.
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	other := agentproto.DBExpose{AppID: 8, Service: "cache", TargetPort: 6379, Port: busy.Addr().(*net.TCPAddr).Port, Allow: []string{"127.0.0.1"}}
	resp := apply(entry, other)
	if len(resp.Entries) != 2 || resp.Entries[0].Error != "" || resp.Entries[1].Error == "" {
		t.Fatalf("busy port: %+v", resp)
	}

	// Saved: a restarted agent listens again without the panel.
	cancel()
	eventuallyTrue(t, func() bool { return echoes(port) != nil })
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	m2 := newDBExposeManager(Config{DataDir: dir}, resolve, slog.New(slog.DiscardHandler))
	m2.start(ctx2)
	if err := echoes(port); err != nil {
		t.Fatalf("after a restart: %v", err)
	}

	// A new port frees the old one; an empty list closes everything.
	port2 := freeTCPPort(t)
	entry.Port = port2
	if _, err := m2.Apply(agentproto.DBExposeApply{Entries: []agentproto.DBExpose{entry}}); err != nil {
		t.Fatal(err)
	}
	if echoes(port) == nil || echoes(port2) != nil {
		t.Fatal("port change")
	}
	if _, err := m2.Apply(agentproto.DBExposeApply{}); err != nil {
		t.Fatal(err)
	}
	if echoes(port2) == nil {
		t.Fatal("still listening after removal")
	}
	if _, err := m2.Apply(agentproto.DBExposeApply{Entries: []agentproto.DBExpose{{AppID: 1, Service: "pg", TargetPort: 5432, Port: 80, Allow: []string{"127.0.0.1"}}}}); err == nil {
		t.Fatal("a privileged port accepted")
	}
}

func eventuallyTrue(t *testing.T, cond func() bool) {
	t.Helper()
	for range 100 {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
