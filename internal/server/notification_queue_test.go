package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
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

// TestSummarize: a burst of one event becomes one message listing them.
func TestSummarize(t *testing.T) {
	msg := func(title, link string) notify.Message {
		return notify.Message{Event: EventHealthDown, Title: title, Link: link}
	}
	two := []notify.Message{msg("a is down", "/monitoring"), msg("b is down", "/monitoring")}
	if got := summarize(two); len(got) != 2 || got[1].Title != "b is down" {
		t.Fatalf("two messages: %+v", got)
	}
	got := summarize(append(two, msg("c is down", "/monitoring")))
	if len(got) != 1 || got[0].Title != "a is down and 2 more" || got[0].Event != EventHealthDown ||
		got[0].Text != "• a is down\n• b is down\n• c is down" || got[0].Link != "/monitoring" {
		t.Fatalf("summary: %+v", got)
	}
	if got := summarize(append(two, msg("c", "/servers/2"))); got[0].Link != "" {
		t.Fatalf("a link to one of several pages: %q", got[0].Link)
	}
	var many []notify.Message
	for i := range 25 {
		many = append(many, msg(fmt.Sprintf("check %d is down", i), ""))
	}
	got = summarize(many)
	if lines := strings.Split(got[0].Text, "\n"); len(lines) != 21 || lines[20] != "… and 5 more" || got[0].Title != "check 0 is down and 24 more" {
		t.Fatalf("long summary: %q", got[0].Text)
	}
}

// TestNotificationBurst: messages of one event within the window reach a
// channel as one; another event's message goes out on its own.
func TestNotificationBurst(t *testing.T) {
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
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, NotifyWindow: 200 * time.Millisecond})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, srv: srv, ts: ts, clock: &testClock{}}
	owner := e.setupOwner()
	owner.want(http.StatusCreated, "POST", "/api/v1/notifications", map[string]any{
		"name": "hook", "kind": "webhook", "url": receiver.URL, "allow_private": true,
		"events": []string{EventHealthDown, EventDeployFailed}})
	for i := range 5 {
		srv.notify(notify.Message{Event: EventHealthDown, Title: fmt.Sprintf("check %d is down", i), Link: "/monitoring"})
	}
	srv.notify(notify.Message{Event: EventDeployFailed, Title: "Deployment #3 of web failed"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // nothing else may follow
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("%d messages delivered: %+v", len(got), got)
	}
	slices.SortFunc(got, func(a, b notify.Message) int { return strings.Compare(a.Event, b.Event) })
	if got[0].Title != "Deployment #3 of web failed" || got[1].Title != "check 0 is down and 4 more" ||
		strings.Count(got[1].Text, "•") != 5 {
		t.Fatalf("messages: %+v", got)
	}
}
