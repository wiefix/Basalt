package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/notify"
)

// TestNotificationQueue: a failed delivery waits in the database; a long
// pause is left to the notification loop, which delivers once it is due.
// A message an earlier run of the panel left behind is delivered once,
// however often the loop looks.
func TestNotificationQueue(t *testing.T) {
	var hits, fail atomic.Int32
	fail.Store(1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Add(-1) >= 0 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(receiver.Close)

	clock := &testClock{t: time.Unix(1_800_000_000, 0)}
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, Now: clock.Now,
		NotifyRetries: []time.Duration{0, 10 * time.Minute}})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, srv: srv, ts: ts, clock: clock}
	owner := e.setupOwner()
	owner.want(http.StatusCreated, "POST", "/api/v1/notifications", map[string]any{
		"name": "hook", "kind": "webhook", "url": receiver.URL, "allow_private": true, "events": []string{EventDeployFailed}})
	ctx := context.Background()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	queued := func() int {
		n, _ := srv.db.QueuedNotifications(ctx)
		total := 0
		for _, c := range n {
			total += c
		}
		return total
	}

	srv.notify(notify.Message{Event: EventDeployFailed, Title: "Deployment #1 of web failed"})
	waitFor("the first attempt", func() bool { return hits.Load() == 1 })
	waitFor("the failure recorded", func() bool {
		c := owner.want(http.StatusOK, "GET", "/api/v1/notifications", nil).body["channels"].([]any)[0].(map[string]any)
		return c["queued"] == float64(1) && c["last_error"] != nil
	})
	// Not due for 10 minutes: the loop leaves it.
	srv.resumeNotifications(ctx)
	time.Sleep(100 * time.Millisecond)
	if hits.Load() != 1 {
		t.Fatalf("retried before the pause: %d attempts", hits.Load())
	}
	clock.Advance(11 * time.Minute)
	srv.resumeNotifications(ctx)
	waitFor("the retry", func() bool { return hits.Load() == 2 && queued() == 0 })
	c := owner.want(http.StatusOK, "GET", "/api/v1/notifications", nil).body["channels"].([]any)[0].(map[string]any)
	if c["last_sent_at"] == nil || c["last_error"] != nil || c["queued"] != float64(0) {
		t.Fatalf("after delivery: %v", c)
	}

	// Left by an earlier run: due at once, delivered once.
	ch, _ := srv.db.NotificationChannels(ctx)
	if _, err := srv.db.QueueNotification(ctx, []int64{ch[0].ID}, []byte(`{"event":"deploy.failed","title":"left over"}`),
		clock.Now().Add(-time.Second), notifyQueued); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		srv.resumeNotifications(ctx)
	}
	waitFor("the left-over message", func() bool { return hits.Load() == 3 && queued() == 0 })
	time.Sleep(100 * time.Millisecond)
	if hits.Load() != 3 {
		t.Fatalf("delivered more than once: %d", hits.Load())
	}

	// A channel that never answers: given up after the attempts, nothing waits.
	fail.Store(100)
	srv.notifyRetries = []time.Duration{0, 10 * time.Millisecond}
	srv.notify(notify.Message{Event: EventDeployFailed, Title: "again"})
	waitFor("giving up", func() bool { return hits.Load() == 5 && queued() == 0 })
}
