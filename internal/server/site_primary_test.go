package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

// switchHub is an agent channel where some servers are offline; it
// records the calls.
type switchHub struct {
	mu      sync.Mutex
	offline map[int64]bool
	calls   []string // "<server> <type>"
	reqs    map[string]any
}

func (h *switchHub) Connected(id int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.offline[id]
}

func (h *switchHub) record(id int64, typ string, req any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := strings.TrimSpace(num(float64(id)) + " " + typ)
	h.calls = append(h.calls, k)
	if h.reqs == nil {
		h.reqs = map[string]any{}
	}
	h.reqs[k] = req
}

func (h *switchHub) Call(_ context.Context, id int64, typ string, req, _ any) error {
	h.record(id, typ, req)
	return nil
}

func (h *switchHub) Stream(_ context.Context, id int64, typ string, req, _ any, _ func(json.RawMessage) error) error {
	h.record(id, typ, req)
	return nil
}
func (h *switchHub) Disconnect(int64)      {}
func (h *switchHub) CAFingerprint() string { return "" }

func (h *switchHub) called(k string) (any, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.reqs[k]
	return v, ok
}

// TestSitePrimaryForced: with its primary offline, a site switches to a
// copy only when asked to explicitly, and only with complete copies of its
// databases: the copy takes over as it is, nothing waits for the primary.
func TestSitePrimaryForced(t *testing.T) {
	hub := &switchHub{offline: map[int64]bool{}}
	clock := &testClock{t: time.Unix(1_800_000_000, 0)}
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, Now: clock.Now, Agents: hub})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, srv: srv, ts: ts, clock: clock}
	o := e.setupOwner()
	ctx := context.Background()
	db := srv.db
	s1, _ := db.CreateServer(ctx, "one", []byte("t1"), time.Now().Add(time.Hour))
	s2, _ := db.CreateServer(ctx, "two", []byte("t2"), time.Now().Add(time.Hour))
	site, err := db.CreateSite(ctx, 0, s1.ID, "shop", []string{"shop.example.com"}, proxy.TLSAuto, "8.4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateDatabaseRecord(ctx, site.ID, s1.ID, "s1_app", "s1_app"); err != nil {
		t.Fatal(err)
	}
	hc := func(n string) *store.HealthCheck {
		return &store.HealthCheck{Name: n, Kind: "https", Host: "192.0.2.1", Port: 443, Path: "/", Interval: 30, Timeout: 5}
	}
	if err := db.AddSiteReplica(ctx, site.ID, s2.ID, hc("r"), hc("p")); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/sites/" + num(float64(site.ID)) + "/primary"
	two := map[string]any{"server_id": s2.ID}
	o.want(http.StatusUnprocessableEntity, "PUT", path, map[string]any{"server_id": s1.ID}) // the primary itself
	o.want(http.StatusUnprocessableEntity, "PUT", path, map[string]any{"server_id": 999})

	hub.mu.Lock()
	hub.offline[s1.ID] = true
	hub.mu.Unlock()
	// No copy of the databases yet: never.
	o.want(http.StatusConflict, "PUT", path, map[string]any{"server_id": s2.ID, "force": true})
	db.SetDBReplication(ctx, store.DBReplication{ReplicaID: s2.ID, SourceID: s1.ID, Running: false, Lag: 42,
		Databases: []string{"s1_app"}, UpdatedAt: time.Now()})
	// Offline primary: only when confirmed, saying what is at stake.
	if msg := o.want(http.StatusConflict, "PUT", path, two).body["error"].(string); !strings.Contains(msg, "offline") || !strings.Contains(msg, "42 s behind") {
		t.Fatalf("offline: %q", msg)
	}
	if _, ok := hub.called(num(float64(s2.ID)) + " " + agentproto.TypeDBPromote); ok {
		t.Fatal("promoted without confirmation")
	}
	r := o.want(http.StatusOK, "PUT", path, map[string]any{"server_id": s2.ID, "force": true}).body
	if r["site"].(map[string]any)["server_id"] != float64(s2.ID) {
		t.Fatalf("switch: %v", r)
	}
	req, ok := hub.called(num(float64(s2.ID)) + " " + agentproto.TypeDBPromote)
	if !ok {
		t.Fatal("not promoted")
	}
	if h := req.(agentproto.DBHandover); h.WaitPos != "" || h.Source != s1.ID || len(h.Databases) != 1 || h.Databases[0].User != "s1_app" {
		t.Fatalf("promote: %+v", h)
	}
	if _, ok := hub.called(num(float64(s1.ID)) + " " + agentproto.TypeDBDemote); ok {
		t.Fatal("the offline primary was asked to hand over")
	}
	ensure, ok := hub.called(num(float64(s2.ID)) + " " + agentproto.TypeSiteEnsure)
	if !ok || ensure.(agentproto.SiteRequest).DB != nil {
		t.Fatalf("the new primary uses its own databases: %+v", ensure)
	}
	got, _ := db.SiteByID(ctx, site.ID)
	dbs, _ := db.Databases(ctx, site.ID)
	if got.ServerID != s2.ID || len(got.Replicas) != 1 || got.Replicas[0] != s1.ID || dbs[0].ServerID != s2.ID {
		t.Fatalf("after: %+v %+v", got, dbs[0])
	}
	var n int
	db.R.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'site.primary_changed'`).Scan(&n)
	if n != 1 {
		t.Fatalf("audit entries: %d", n)
	}
}
