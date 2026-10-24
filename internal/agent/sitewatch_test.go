package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

type fakeNotifier struct {
	mu   sync.Mutex
	sent [][]int64
	err  error
}

func (n *fakeNotifier) Notify(typ string, v any) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return n.err
	}
	if typ != agentproto.TypeSiteChanged {
		return errors.New("unexpected " + typ)
	}
	n.sent = append(n.sent, v.(agentproto.SiteChanged).Sites)
	return nil
}

func (n *fakeNotifier) got() [][]int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.sent)
}

// TestSiteWatcherReports: changes of watched sites are reported once
// quiet, together; others are not; a report that fails is kept.
func TestSiteWatcherReports(t *testing.T) {
	w := newSiteWatcher(slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	if err := w.apply(agentproto.SiteWatch{Sites: []int64{3, 1, 3}}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []agentproto.SiteWatch{{Sites: []int64{0}}, {Sites: make([]int64, agentproto.MaxWatchedSites+1)}} {
		if w.apply(bad) == nil {
			t.Fatal("accepted an invalid list")
		}
	}

	// Quiet: nothing due until watchQuiet after the last change.
	now := time.Now()
	w.siteChanged(1)
	w.siteChanged(2) // not watched
	w.siteChanged(3)
	if ids, wait := w.due(now); ids != nil || wait <= 0 || wait > watchQuiet+time.Second {
		t.Fatalf("due at once: %v %v", ids, wait)
	}
	if ids, _ := w.due(now.Add(watchQuiet + time.Second)); !slices.Equal(ids, []int64{1, 3}) {
		t.Fatalf("due: %v", ids)
	}
	if ids, wait := w.due(now.Add(time.Minute)); ids != nil || wait >= 0 {
		t.Fatalf("reported twice: %v", ids)
	}
	// A site that keeps changing is reported after watchMaxDelay anyway.
	w.siteChanged(1)
	w.mu.Lock()
	w.first = w.first.Add(-watchMaxDelay)
	w.mu.Unlock()
	if ids, _ := w.due(time.Now()); !slices.Equal(ids, []int64{1}) {
		t.Fatalf("busy site not reported: %v", ids)
	}

	// Through a connection: one report for a burst.
	n := &fakeNotifier{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.report(ctx, n); close(done) }()
	for range 5 {
		w.siteChanged(3)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(n.got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := n.got(); len(got) != 1 || !slices.Equal(got[0], []int64{3}) {
		t.Fatalf("reports: %v", got)
	}
	cancel()
	<-done

	// The connection fails: the change waits for the next one.
	n = &fakeNotifier{err: errors.New("gone")}
	w.siteChanged(1)
	w.mu.Lock()
	w.last, w.first = w.last.Add(-time.Minute), w.first.Add(-time.Minute)
	w.mu.Unlock()
	w.report(context.Background(), n) // returns on the failure
	if ids, _ := w.due(time.Now().Add(time.Minute)); !slices.Equal(ids, []int64{1}) {
		t.Fatalf("lost: %v", ids)
	}

	// A site no longer watched forgets its changes.
	w.siteChanged(3)
	w.apply(agentproto.SiteWatch{Sites: []int64{1}})
	if ids, wait := w.due(time.Now().Add(time.Minute)); ids != nil || wait >= 0 {
		t.Fatalf("unwatched site reported: %v", ids)
	}
}
