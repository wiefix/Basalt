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
	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

// TestReplAlert: failing copying is notified once it has failed for a
// while, once, and its recovery only after that; an offline source changes
// nothing (the server's own notification covers it).
func TestReplAlert(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	ok := agentproto.DBReplState{Running: true}
	bad := agentproto.DBReplState{Error: "Duplicate entry"}
	stopped := agentproto.DBReplState{}
	offline := agentproto.DBReplState{Error: errReplSourceOffline}
	type rec = store.DBReplication
	for _, c := range []struct {
		name    string
		prev    rec
		st      agentproto.DBReplState
		now     time.Time
		since   time.Time
		alerted bool
		change  replChange
	}{
		{"works", rec{}, ok, t0, time.Time{}, false, replNoChange},
		{"first failure", rec{}, bad, t0, t0, false, replNoChange},
		{"not running", rec{}, stopped, t0, t0, false, replNoChange},
		{"still failing", rec{FailingSince: t0}, bad, t0.Add(4 * time.Minute), t0, false, replNoChange},
		{"failing long", rec{FailingSince: t0}, bad, t0.Add(5 * time.Minute), t0, true, replStopped},
		{"notified", rec{FailingSince: t0, Alerted: true}, bad, t0.Add(time.Hour), t0, true, replNoChange},
		{"recovered", rec{FailingSince: t0, Alerted: true}, ok, t0.Add(time.Hour), time.Time{}, false, replResumed},
		{"brief failure over", rec{FailingSince: t0}, ok, t0.Add(time.Minute), time.Time{}, false, replNoChange},
		{"source offline", rec{FailingSince: t0}, offline, t0.Add(time.Hour), t0, false, replNoChange},
	} {
		since, alerted, change := replAlert(c.prev, c.st, c.now)
		if !since.Equal(c.since) || alerted != c.alerted || change != c.change {
			t.Errorf("%s: %v %v %v, want %v %v %v", c.name, since, alerted, change, c.since, c.alerted, c.change)
		}
	}
}

// replHub answers replication requests with the state set in state.
type replHub struct {
	switchHub
	state agentproto.DBReplState
}

func (h *replHub) Call(ctx context.Context, id int64, typ string, req, resp any) error {
	h.switchHub.Call(ctx, id, typ, req, resp)
	if typ == agentproto.TypeDBUserHashes {
		hashes := map[string]string{}
		for _, u := range req.(agentproto.DBUserHashes).Users {
			hashes[u] = "*" + strings.Repeat("A", 40)
		}
		return fill(resp, agentproto.DBUserHashesResponse{Hashes: hashes})
	}
	return nil
}

func (h *replHub) Stream(ctx context.Context, id int64, typ string, req, resp any, fn func(json.RawMessage) error) error {
	h.switchHub.Stream(ctx, id, typ, req, resp, fn)
	if typ == agentproto.TypeDBReplApply {
		h.mu.Lock()
		st := h.state
		h.mu.Unlock()
		var out agentproto.DBReplApplyResponse
		for _, s := range req.(agentproto.DBReplApply).Sources {
			st.ServerID = s.ServerID
			out.Sources = append(out.Sources, st)
		}
		return fill(resp, out)
	}
	return nil
}

func fill(dst, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// TestReplicationNotifications: when copying a site's databases to a
// replica keeps failing, the site's channels hear it once, and again when
// it works.
func TestReplicationNotifications(t *testing.T) {
	var mu sync.Mutex
	var got []notify.Message
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m notify.Message
		json.NewDecoder(r.Body).Decode(&m)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	}))
	t.Cleanup(receiver.Close)
	hub := &replHub{switchHub: switchHub{offline: map[int64]bool{}}}
	clock := &testClock{t: time.Unix(1_800_000_000, 0)}
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, Now: clock.Now, Agents: hub, NotifyWindow: -1})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, srv: srv, ts: ts, clock: clock}
	o := e.setupOwner()
	o.want(http.StatusCreated, "POST", "/api/v1/notifications", map[string]any{
		"name": "hook", "kind": "webhook", "url": receiver.URL, "allow_private": true, "events": []string{EventReplication}})
	o.want(http.StatusOK, "PUT", "/api/v1/notifications/panel-url", map[string]any{"url": "https://panel.example.com"})
	ctx := context.Background()
	db := srv.db
	s1, _ := db.CreateServer(ctx, "one", []byte("t1"), time.Now().Add(time.Hour))
	s2, _ := db.CreateServer(ctx, "two", []byte("t2"), time.Now().Add(time.Hour))
	db.W.Exec(`UPDATE servers SET peer_port = 8444, last_ip = '192.0.2.1', mariadb = '11.8'`)
	site, _ := db.CreateSite(ctx, 0, s1.ID, "shop", []string{"shop.example.com"}, proxy.TLSAuto, "8.4")
	if _, err := db.CreateDatabaseRecord(ctx, site.ID, s1.ID, "s1_app", "s1_app"); err != nil {
		t.Fatal(err)
	}
	hc := func(n string) *store.HealthCheck {
		return &store.HealthCheck{Name: n, Kind: "https", Host: "192.0.2.1", Port: 443, Path: "/", Interval: 30, Timeout: 5}
	}
	if err := db.AddSiteReplica(ctx, site.ID, s2.ID, hc("r"), hc("p")); err != nil {
		t.Fatal(err)
	}
	set := func(st agentproto.DBReplState) {
		hub.mu.Lock()
		hub.state = st
		hub.mu.Unlock()
	}
	messages := func() []notify.Message {
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		out := got
		got = nil
		return out
	}
	round := func(after time.Duration) {
		clock.Advance(after)
		srv.syncDBReplication(ctx)
	}

	set(agentproto.DBReplState{Running: true, Databases: []string{"s1_app"}})
	round(0)
	set(agentproto.DBReplState{Error: "Could not execute Write_rows_v1 event: Duplicate entry", Databases: []string{"s1_app"}})
	round(time.Minute)
	round(3 * time.Minute)
	if m := messages(); len(m) != 0 {
		t.Fatalf("notified too early: %+v", m)
	}
	round(2 * time.Minute)
	m := messages()
	if len(m) != 1 || m[0].Title != "The databases of shop are not copied to two" ||
		!strings.Contains(m[0].Text, "Copying from one has not worked since") || !strings.Contains(m[0].Text, "Duplicate entry") ||
		m[0].Link != "https://panel.example.com/sites/"+num(float64(site.ID)) {
		t.Fatalf("stopped: %+v", m)
	}
	round(time.Minute)
	if m := messages(); len(m) != 0 {
		t.Fatalf("notified twice: %+v", m)
	}
	// The source going offline neither ends nor repeats the episode.
	hub.mu.Lock()
	hub.offline[s1.ID] = true
	hub.mu.Unlock()
	round(time.Minute)
	hub.mu.Lock()
	hub.offline[s1.ID] = false
	hub.mu.Unlock()
	set(agentproto.DBReplState{Running: true, Databases: []string{"s1_app"}})
	round(time.Minute)
	if m := messages(); len(m) != 1 || m[0].Title != "The databases of shop are copied to two again" {
		t.Fatalf("resumed: %+v", m)
	}
}
