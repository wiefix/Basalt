package e2e

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/cloud/cloudtest"
	"github.com/wiefix/basalt/internal/server"
)

// floatingHost records the floating IPs its agent was told to configure.
type floatingHost struct {
	mu   sync.Mutex
	got  []string
	n    int
	fail bool
}

func (f *floatingHost) set(_ context.Context, ips agentproto.FloatingIPs) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	if f.fail {
		return errors.New("RTNETLINK answers: Operation not permitted")
	}
	f.got = slices.Clone(ips.Addresses)
	return nil
}

func (f *floatingHost) addrs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got
}

// TestFloatingIPs: a Hetzner account, one of its floating IPs added with a
// group of two servers (found at the provider by their addresses), which
// both configure it; moving it between them; changing the group and
// removing it.
func TestFloatingIPs(t *testing.T) {
	fake := cloudtest.NewHetzner(t, "hcloud-token")
	fake.AddServer(cloudtest.Server{ID: 1, Name: "fsn-1", IPv4: "198.51.100.1", IPv6: "2001:db8:1::/64"})
	fake.AddServer(cloudtest.Server{ID: 2, Name: "fsn-2", IPv4: "198.51.100.2"})
	fake.AddServer(cloudtest.Server{ID: 3, Name: "not-basalt", IPv4: "198.51.100.3"})
	fake.AddFloatingIP(cloudtest.FloatingIP{ID: 10, Name: "web", IP: "203.0.113.10", Location: "fsn1", Server: 1})
	fake.AddFloatingIP(cloudtest.FloatingIP{ID: 11, Name: "web6", IP: "2001:db8:f::/64", Location: "fsn1"})

	p := startPanelWith(t, server.Options{HetznerAPI: fake.URL})
	var f1, f2 floatingHost
	h1 := startHostWith(t, p, "", func(c *agent.Config) { c.FloatingIPs = f1.set })
	h2 := startHostWith(t, p, "", func(c *agent.Config) { c.FloatingIPs = f2.set })
	h3 := startHost(t, p)
	p.call(200, "PUT", "/api/v1/servers/"+h1.id+"/public-ips", map[string]any{"ips": []string{"2001:db8:1::5"}})
	p.call(200, "PUT", "/api/v1/servers/"+h2.id+"/public-ips", map[string]any{"ips": []string{"198.51.100.2"}})
	p.call(200, "PUT", "/api/v1/servers/"+h3.id+"/public-ips", map[string]any{"ips": []string{"192.0.2.99"}})
	sid := func(h *host) int64 { return mustAtoi(h.id) }

	// The account: the provider must take the token.
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/cloud-accounts", map[string]string{"provider": "hetzner", "name": "main", "token": "wrong"})
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/cloud-accounts", map[string]string{"provider": "aws", "name": "main", "token": "hcloud-token"})
	acc := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/cloud-accounts",
		map[string]string{"provider": "hetzner", "name": "main", "token": "hcloud-token"})["account"].(map[string]any)["id"])
	p.call(http.StatusConflict, "POST", "/api/v1/cloud-accounts", map[string]string{"provider": "hetzner", "name": "main", "token": "hcloud-token"})
	if body := jsonString(p.call(200, "GET", "/api/v1/cloud-accounts", nil)); strings.Contains(body, "hcloud-token") {
		t.Fatalf("the token is shown: %s", body)
	}

	// What the account has, with the panel's servers it is.
	at := p.call(200, "GET", "/api/v1/cloud-accounts/"+acc+"/floating-ips", nil)
	ips, servers := at["floating_ips"].([]any), at["servers"].([]any)
	if len(ips) != 2 || ips[1].(map[string]any)["address"] != "2001:db8:f::1" || ips[0].(map[string]any)["assigned_to"] != "1" {
		t.Fatalf("floating IPs: %v", ips)
	}
	if len(servers) != 3 || jsonID(servers[0].(map[string]any)["server_id"]) != h1.id ||
		jsonID(servers[1].(map[string]any)["server_id"]) != h2.id || jsonID(servers[2].(map[string]any)["server_id"]) != "0" {
		t.Fatalf("servers: %v", servers)
	}

	// A server that is not the account's cannot be in the group.
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/floating-ips", map[string]any{"account_id": mustAtoi(acc), "provider_id": "10",
		"server_ids": []int64{sid(h1), sid(h3)}})
	p.call(http.StatusNotFound, "POST", "/api/v1/floating-ips", map[string]any{"account_id": mustAtoi(acc), "provider_id": "99",
		"server_ids": []int64{sid(h1)}})
	f := p.call(http.StatusCreated, "POST", "/api/v1/floating-ips", map[string]any{"account_id": mustAtoi(acc), "provider_id": "10",
		"server_ids": []int64{sid(h1), sid(h2)}})["floating_ip"].(map[string]any)
	id := jsonID(f["id"])
	if f["address"] != "203.0.113.10" || jsonID(f["server_id"]) != h1.id || len(f["servers"].([]any)) != 2 ||
		f["servers"].([]any)[1].(map[string]any)["provider_server_id"] != "2" {
		t.Fatalf("added: %v", f)
	}
	p.call(http.StatusConflict, "POST", "/api/v1/floating-ips", map[string]any{"account_id": mustAtoi(acc), "provider_id": "10",
		"server_ids": []int64{sid(h1)}})
	for i, fh := range []*floatingHost{&f1, &f2} {
		if got := fh.addrs(); !slices.Equal(got, []string{"203.0.113.10"}) {
			t.Fatalf("server %d configures %v", i+1, got)
		}
	}
	// The agent keeps the set for its restarts.
	if b, err := os.ReadFile(filepath.Join(h1.dataDir, "floating-ips.json")); err != nil || !strings.Contains(string(b), "203.0.113.10") {
		t.Fatalf("saved set: %s %v", b, err)
	}

	// Moved to the other server of the group.
	assign := "/api/v1/floating-ips/" + id + "/assign"
	p.call(http.StatusUnprocessableEntity, "POST", assign, map[string]any{"server_id": sid(h3)})
	f = p.call(200, "POST", assign, map[string]any{"server_id": sid(h2)})["floating_ip"].(map[string]any)
	if jsonID(f["server_id"]) != h2.id || fake.AssignedTo(10) != 2 {
		t.Fatalf("assigned: %v, at the provider %d", f, fake.AssignedTo(10))
	}
	fake.FailAssign(true)
	p.call(http.StatusBadGateway, "POST", assign, map[string]any{"server_id": sid(h1)})
	fake.FailAssign(false)
	if f := p.call(200, "POST", "/api/v1/floating-ips/"+id+"/refresh", nil)["floating_ip"].(map[string]any); jsonID(f["server_id"]) != h2.id {
		t.Fatalf("after a failed assignment: %v", f)
	}

	// A server that cannot configure it: the error shows.
	f1.mu.Lock()
	f1.fail = true
	f1.mu.Unlock()
	p.call(200, "PUT", "/api/v1/floating-ips/"+id+"/servers", map[string]any{"server_ids": []int64{sid(h1), sid(h2)}})
	list := p.call(200, "GET", "/api/v1/floating-ips", nil)["floating_ips"].([]any)
	members := list[0].(map[string]any)["servers"].([]any)
	if e := members[0].(map[string]any)["error"].(string); !strings.Contains(e, "Operation not permitted") || members[1].(map[string]any)["error"] != "" {
		t.Fatalf("errors: %v", members)
	}
	f1.mu.Lock()
	f1.fail = false
	f1.mu.Unlock()

	// The server it is routed to leaves the group: it no longer counts,
	// and takes the address off.
	f = p.call(200, "PUT", "/api/v1/floating-ips/"+id+"/servers", map[string]any{"server_ids": []int64{sid(h1)}})["floating_ip"].(map[string]any)
	if jsonID(f["server_id"]) != "0" || len(f2.addrs()) != 0 || !slices.Equal(f1.addrs(), []string{"203.0.113.10"}) {
		t.Fatalf("group of one: %v; server 2 configures %v", f, f2.addrs())
	}
	if e := f["servers"].([]any)[0].(map[string]any)["error"]; e != "" {
		t.Fatalf("error after it worked: %v", e)
	}

	p.call(http.StatusConflict, "DELETE", "/api/v1/cloud-accounts/"+acc, nil)
	p.call(http.StatusNoContent, "DELETE", "/api/v1/floating-ips/"+id, nil)
	if got := f1.addrs(); len(got) != 0 {
		t.Fatalf("still configured: %v", got)
	}
	p.call(200, "PUT", "/api/v1/cloud-accounts/"+acc, map[string]string{"name": "renamed"})
	p.call(http.StatusNoContent, "DELETE", "/api/v1/cloud-accounts/"+acc, nil)

	audit := jsonString(p.call(200, "GET", "/api/v1/audit", nil))
	for _, ev := range []string{"cloud_account.created", "floating_ip.added", "floating_ip.assigned", "floating_ip.servers_changed", "floating_ip.removed", "cloud_account.deleted"} {
		if !strings.Contains(audit, ev) {
			t.Errorf("no %s in the audit log", ev)
		}
	}
}
