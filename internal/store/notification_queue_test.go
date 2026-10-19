package store

import (
	"context"
	"testing"
	"time"
)

// TestNotificationQueue: messages are stored per channel up to a limit,
// claimed once when due (held while an attempt runs), rescheduled, dropped,
// and deleted with their channel.
func TestNotificationQueue(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	a := &NotificationChannel{Name: "a", Kind: "webhook", Config: []byte("x"), Events: []string{"x"}}
	b := &NotificationChannel{Name: "b", Kind: "webhook", Config: []byte("x"), Events: []string{"x"}}
	for _, c := range []*NotificationChannel{a, b} {
		if err := db.CreateNotificationChannel(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Unix(1_800_000_000, 0)
	q, err := db.QueueNotification(ctx, []int64{a.ID, b.ID}, []byte(`{"title":"one"}`), now.Add(time.Minute), 2)
	if err != nil || len(q) != 2 || q[0].ChannelID != a.ID || string(q[1].Message) != `{"title":"one"}` {
		t.Fatalf("queued %+v, %v", q, err)
	}
	db.QueueNotification(ctx, []int64{a.ID}, []byte(`{"title":"two"}`), now.Add(time.Minute), 2)
	// a has 2 waiting: a third is not stored; b still takes one.
	if q, _ := db.QueueNotification(ctx, []int64{a.ID, b.ID}, []byte(`{}`), now.Add(time.Minute), 2); len(q) != 1 || q[0].ChannelID != b.ID {
		t.Fatalf("over the limit: %+v", q)
	}
	if n, _ := db.QueuedNotifications(ctx); n[a.ID] != 2 || n[b.ID] != 2 {
		t.Fatalf("counts %v", n)
	}
	// Held until a minute from now: nothing is due yet.
	if due, _ := db.ClaimDueNotifications(ctx, now, now.Add(time.Minute), 10); len(due) != 0 {
		t.Fatalf("claimed while held: %+v", due)
	}
	later := now.Add(2 * time.Minute)
	due, err := db.ClaimDueNotifications(ctx, later, later.Add(time.Minute), 3)
	if err != nil || len(due) != 3 {
		t.Fatalf("claimed %+v, %v", due, err)
	}
	// Claimed ones are held: a second claim gets only the fourth.
	if again, _ := db.ClaimDueNotifications(ctx, later, later.Add(time.Minute), 10); len(again) != 1 {
		t.Fatalf("claimed twice: %+v", again)
	}
	if err := db.RetryNotification(ctx, due[0].ID, 1, later.Add(time.Hour), "refused"); err != nil {
		t.Fatal(err)
	}
	if err := db.DropNotification(ctx, due[1].ID); err != nil {
		t.Fatal(err)
	}
	r, _ := db.ClaimDueNotifications(ctx, later.Add(2*time.Hour), later.Add(3*time.Hour), 10)
	var retried *QueuedNotification
	for i := range r {
		if r[i].ID == due[0].ID {
			retried = &r[i]
		}
		if r[i].ID == due[1].ID {
			t.Fatal("a dropped message came back")
		}
	}
	if len(r) != 3 || retried == nil || retried.Attempts != 1 || retried.LastError != "refused" {
		t.Fatalf("after retry and drop: %+v", r)
	}
	if err := db.DeleteNotificationChannel(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.QueuedNotifications(ctx); n[a.ID] != 0 || n[b.ID] == 0 {
		t.Fatalf("after deleting channel a: %v", n)
	}
}
