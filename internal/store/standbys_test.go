package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestPromote: a standby's copy becomes the panel's with the next epoch,
// without the standby itself among its standbys.
func TestPromote(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()
	if e, err := db.PanelEpoch(ctx); err != nil || e != 0 {
		t.Fatalf("epoch of a new panel: %d %v", e, err)
	}
	self, err := db.CreateStandby(ctx, "self", 0, []byte("a"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := db.CreateStandby(ctx, "other", 0, []byte("b"), time.Now().Add(time.Hour))
	if changed, err := db.SetStandbyAgentURL(ctx, other.ID, "https://192.0.2.7:8443"); err != nil || !changed {
		t.Fatalf("agent url: %v %v", changed, err)
	}
	if changed, _ := db.SetStandbyAgentURL(ctx, other.ID, "https://192.0.2.7:8443"); changed {
		t.Fatal("the same address counted as a change")
	}
	for want := int64(1); want <= 2; want++ {
		e, err := db.Promote(ctx, self.ID, false, time.Now())
		if err != nil || e != want {
			t.Fatalf("promote: %d %v; want %d", e, err, want)
		}
		if got, _ := db.PanelEpoch(ctx); got != want {
			t.Fatalf("epoch %d; want %d", got, want)
		}
	}
	list, _ := db.Standbys(ctx)
	if len(list) != 1 || list[0].ID != other.ID || list[0].AgentURL != "https://192.0.2.7:8443" {
		t.Fatalf("standbys: %+v", list)
	}

	// A standby whose host has a server: that server becomes the panel's,
	// and the promotion waits to be announced.
	host, err := db.CreateServer(ctx, "spare", []byte("h"), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sb, _ := db.CreateStandby(ctx, "spare", host.ID, []byte("c"), time.Now().Add(time.Hour))
	if db.PanelServer(ctx) != 0 {
		t.Fatal("panel server before")
	}
	at := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if e, err := db.Promote(ctx, sb.ID, true, at); err != nil || e != 3 {
		t.Fatalf("promote: %d %v", e, err)
	}
	if db.PanelServer(ctx) != host.ID {
		t.Fatalf("panel server %d", db.PanelServer(ctx))
	}
	v, _ := db.Setting(ctx, SettingPromoted)
	var p Promotion
	if json.Unmarshal([]byte(v), &p); p != (Promotion{Epoch: 3, At: at, Auto: true}) {
		t.Fatalf("promotion: %s", v)
	}
}
