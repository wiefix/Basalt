package store

import (
	"context"
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
		e, err := db.Promote(ctx, self.ID)
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
}
