package privhelper

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wiefix/basalt/internal/agentproto"
)

// fakeIP is iproute2's ip over a table of interfaces and their addresses.
type fakeIP struct {
	mu     sync.Mutex
	dev    string // of the default route
	addrs  map[string]map[string]int
	calls  []string
	failOn string
}

func (f *fakeIP) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	switch {
	case cmd == "-4 -j route show default":
		return fmt.Appendf(nil, `[{"dst":"default","gateway":"172.31.1.1","dev":%q}]`, f.dev), nil
	case len(args) == 5 && args[1] == "addr" && args[2] == "show":
		var info []map[string]any
		for a, bits := range f.addrs[args[4]] {
			info = append(info, map[string]any{"local": a, "prefixlen": bits})
		}
		return json.Marshal([]any{map[string]any{"ifname": args[4], "addr_info": info}})
	case args[0] == "addr" && (args[1] == "add" || args[1] == "del"):
		if f.failOn != "" && strings.Contains(cmd, f.failOn) {
			return nil, fmt.Errorf("ip %s: RTNETLINK answers: Operation not permitted", cmd)
		}
		a, bits, _ := strings.Cut(args[2], "/")
		n, _ := strconv.Atoi(bits)
		if f.addrs[args[4]] == nil {
			f.addrs[args[4]] = map[string]int{}
		}
		if args[1] == "add" {
			f.addrs[args[4]][a] = n
		} else {
			delete(f.addrs[args[4]], a)
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected ip %s", cmd)
}

func (f *fakeIP) on(dev string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.addrs[dev]))
}

func TestFloatingIPs(t *testing.T) {
	h, err := New(Config{SitesRoot: "/srv/basalt/sites", StateDir: t.TempDir()}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ip := &fakeIP{dev: "eth0", addrs: map[string]map[string]int{"eth0": {"198.51.100.1": 32, "203.0.113.99": 32}}}
	h.ip = ip.run
	ctx := context.Background()

	for _, bad := range [][]string{{"10.0.0.1"}, {"127.0.0.1"}, {"fe80::1"}, {"::ffff:203.0.113.1"}, {"203.0.113.1", "203.0.113.1"}, {"2001:DB8::1"}} {
		if err := h.SetFloatingIPs(ctx, agentproto.FloatingIPs{Addresses: bad}); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}

	// 203.0.113.99 is the host's own: kept, never removed.
	if err := h.SetFloatingIPs(ctx, agentproto.FloatingIPs{Addresses: []string{"203.0.113.10", "2001:db8:f::1", "203.0.113.99"}}); err != nil {
		t.Fatal(err)
	}
	if got := ip.on("eth0"); !slices.Equal(got, []string{"198.51.100.1", "2001:db8:f::1", "203.0.113.10", "203.0.113.99"}) {
		t.Fatalf("eth0: %v", got)
	}
	if !slices.Contains(ip.calls, "addr add 2001:db8:f::1/128 dev eth0 nodad") || !slices.Contains(ip.calls, "addr add 203.0.113.10/32 dev eth0") {
		t.Fatalf("calls: %v", ip.calls)
	}
	// Again: nothing to do.
	ip.calls = nil
	if err := h.SetFloatingIPs(ctx, agentproto.FloatingIPs{Addresses: []string{"203.0.113.10", "2001:db8:f::1", "203.0.113.99"}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range ip.calls {
		if strings.HasPrefix(c, "addr add") || strings.HasPrefix(c, "addr del") {
			t.Fatalf("changed again: %v", ip.calls)
		}
	}
	// Gone from the set: removed, except the host's own.
	if err := h.SetFloatingIPs(ctx, agentproto.FloatingIPs{Addresses: []string{"2001:db8:f::1"}}); err != nil {
		t.Fatal(err)
	}
	if got := ip.on("eth0"); !slices.Equal(got, []string{"198.51.100.1", "2001:db8:f::1", "203.0.113.99"}) {
		t.Fatalf("eth0 after removal: %v", got)
	}
	// The default route moved: the address follows it.
	ip.dev = "ens3"
	if err := h.SetFloatingIPs(ctx, agentproto.FloatingIPs{Addresses: []string{"2001:db8:f::1"}}); err != nil {
		t.Fatal(err)
	}
	if got := ip.on("eth0"); slices.Contains(got, "2001:db8:f::1") || !slices.Equal(ip.on("ens3"), []string{"2001:db8:f::1"}) {
		t.Fatalf("after the route moved: eth0 %v, ens3 %v", got, ip.on("ens3"))
	}
	// One that cannot be added: the others are, and the error tells.
	ip.failOn = "203.0.113.20"
	err = h.SetFloatingIPs(ctx, agentproto.FloatingIPs{Addresses: []string{"2001:db8:f::1", "203.0.113.20", "203.0.113.21"}})
	if err == nil || !strings.Contains(err.Error(), "Operation not permitted") || !slices.Contains(ip.on("ens3"), "203.0.113.21") {
		t.Fatalf("partial failure: %v %v", err, ip.on("ens3"))
	}
	// Removing all, through the request.
	ip.failOn = ""
	if _, err := h.Handle(ctx, MsgFloatingIPs, json.RawMessage(`{"addresses":[]}`)); err != nil {
		t.Fatal(err)
	}
	if got := ip.on("ens3"); len(got) != 0 {
		t.Fatalf("ens3 after all were removed: %v", got)
	}
}
