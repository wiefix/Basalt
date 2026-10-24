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

	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/store"
)

// TestFailoverWitnesses: the standby that takes over by itself asks every
// server with a tunnel but the panel's own and its own; without anyone to
// ask it is refused.
func TestFailoverWitnesses(t *testing.T) {
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, srv: srv, ts: ts}
	o := e.setupOwner()
	ctx := context.Background()
	db := srv.db
	mk := func(name string) *store.Server {
		s, err := db.CreateServer(ctx, name, []byte(name), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	panelHost, spareHost, web, quiet := mk("panel"), mk("spare"), mk("web"), mk("quiet")
	db.W.Exec(`UPDATE servers SET peer_port = 8444, last_ip = '192.0.2.' || id WHERE id <> ?`, quiet.ID)
	db.SetSetting(ctx, store.SettingPanelServer, jsonNum(panelHost.ID))
	sb, _ := db.CreateStandby(ctx, "spare", spareHost.ID, []byte("t"), time.Now().Add(time.Hour))
	set := func(code int, id int64, after int) map[string]any {
		return o.want(code, "PUT", "/api/v1/standbys/failover", map[string]any{"standby_id": id, "after_s": after}).body
	}
	set(http.StatusUnprocessableEntity, sb.ID, 300) // not installed yet
	db.SetStandbyCert(ctx, sb.ID, "fp", time.Now().Add(time.Hour), time.Now())
	set(http.StatusUnprocessableEntity, sb.ID, 30)   // too short
	set(http.StatusUnprocessableEntity, sb.ID, 7200) // too long
	fo := set(http.StatusOK, sb.ID, 300)["failover"].(map[string]any)
	ws := fo["witnesses"].([]any)
	if len(ws) != 1 || ws[0].(map[string]any)["name"] != "web" || ws[0].(map[string]any)["addr"] != "192.0.2."+jsonNum(web.ID)+":8444" {
		t.Fatalf("witnesses: %v", ws)
	}
	f, err := srv.FailoverFor(ctx, sb.ID)
	if err != nil || !f.Auto || f.After != 300 || len(f.Witnesses) != 1 || f.Witnesses[0].ServerID != web.ID {
		t.Fatalf("for the standby: %+v %v", f, err)
	}
	if f, _ := srv.FailoverFor(ctx, sb.ID+1); f.Auto {
		t.Fatal("another standby takes over too")
	}
	// Nobody left to ask.
	db.W.Exec(`UPDATE servers SET peer_port = 0 WHERE id = ?`, web.ID)
	if f, _ := srv.FailoverFor(ctx, sb.ID); f.Auto {
		t.Fatal("takes over without anyone to ask")
	}
	set(http.StatusUnprocessableEntity, sb.ID, 300)
	if fo := set(http.StatusOK, 0, 0)["failover"].(map[string]any); fo["standby_id"] != 0.0 {
		t.Fatalf("off: %v", fo)
	}
}

// TestAnnouncePromotion: a panel that took over says so once, how it
// happened.
func TestAnnouncePromotion(t *testing.T) {
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
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, NotifyWindow: -1})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, srv: srv, ts: ts}
	o := e.setupOwner()
	o.want(http.StatusCreated, "POST", "/api/v1/notifications", map[string]any{
		"name": "hook", "kind": "webhook", "url": receiver.URL, "allow_private": true, "events": []string{EventPanelFailover}})
	ctx := context.Background()
	sb, _ := srv.db.CreateStandby(ctx, "spare", 0, []byte("t"), time.Now().Add(time.Hour))
	if _, err := srv.db.Promote(ctx, sb.ID, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	srv.announcePromotion(ctx)
	srv.announcePromotion(ctx) // once
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Event != EventPanelFailover || !strings.Contains(got[0].Text, "by itself") || !strings.Contains(got[0].Text, "epoch 1") {
		t.Fatalf("messages: %+v", got)
	}
}

func jsonNum(id int64) string {
	b, _ := json.Marshal(id)
	return string(b)
}
