package agent

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// The primary server of a site with replicas tells the panel when the
// site's files change (agentproto.TypeSiteChanged), so they are copied
// within seconds rather than at the panel's next round. Where the system
// cannot watch folders (not Linux, or too many of them), the round every
// minute still copies them.

const (
	// watchQuiet: changes are reported once the site was quiet this long
	// (an upload of many files is one report)...
	watchQuiet = 2 * time.Second
	// ...or this long after the first change, while it keeps changing.
	watchMaxDelay = 10 * time.Second
	// maxWatchDirs bounds the folders watched in one site: a larger one is
	// copied by the panel's round only.
	maxWatchDirs = 20_000
)

// watchBackend watches the folders of sites (inotify on Linux).
type watchBackend interface {
	// set watches the sites in ids and no others.
	set(ids []int64)
	close()
}

// siteWatcher collects the sites whose files changed and reports them to
// the panel.
type siteWatcher struct {
	log     *slog.Logger
	root    string
	backend watchBackend // nil: the system cannot watch

	mu      sync.Mutex
	sites   []int64
	changed map[int64]bool
	first   time.Time // of the changes not reported yet
	last    time.Time
	wake    chan struct{}
}

func newSiteWatcher(log *slog.Logger, root string) *siteWatcher {
	return &siteWatcher{log: log, root: root, changed: map[int64]bool{}, wake: make(chan struct{}, 1)}
}

// start begins watching (Run): the sites named so far and later.
func (w *siteWatcher) start() {
	b := newWatchBackend(w)
	w.mu.Lock()
	w.backend = b
	ids := slices.Clone(w.sites)
	w.mu.Unlock()
	if b != nil && len(ids) > 0 {
		b.set(ids)
	}
}

// apply sets the sites to watch.
func (w *siteWatcher) apply(req agentproto.SiteWatch) error {
	if err := req.Validate(); err != nil {
		return err
	}
	ids := slices.Clone(req.Sites)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	w.mu.Lock()
	same := slices.Equal(ids, w.sites)
	w.sites = ids
	b := w.backend
	for id := range w.changed {
		if !slices.Contains(ids, id) {
			delete(w.changed, id)
		}
	}
	w.mu.Unlock()
	if !same && b != nil {
		b.set(ids)
	}
	return nil
}

// siteChanged records a change in site id's files.
func (w *siteWatcher) siteChanged(id int64) {
	now := time.Now()
	w.mu.Lock()
	if !slices.Contains(w.sites, id) {
		w.mu.Unlock()
		return
	}
	if len(w.changed) == 0 {
		w.first = now
	}
	w.changed[id] = true
	w.last = now
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// due returns the changed sites once they are quiet (or waited long
// enough), else how long to wait.
func (w *siteWatcher) due(now time.Time) ([]int64, time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.changed) == 0 {
		return nil, -1
	}
	wait := min(w.last.Add(watchQuiet).Sub(now), w.first.Add(watchMaxDelay).Sub(now))
	if wait > 0 {
		return nil, wait
	}
	ids := make([]int64, 0, len(w.changed))
	for id := range w.changed {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	clear(w.changed)
	return ids, 0
}

// putBack keeps changes that could not be reported.
func (w *siteWatcher) putBack(ids []int64) {
	for _, id := range ids {
		w.siteChanged(id)
	}
}

type notifier interface {
	Notify(msgType string, v any) error
}

// report tells the panel of changes while connected.
func (w *siteWatcher) report(ctx context.Context, peer notifier) {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		ids, wait := w.due(time.Now())
		if len(ids) > 0 {
			if err := peer.Notify(agentproto.TypeSiteChanged, agentproto.SiteChanged{Sites: ids}); err != nil {
				w.putBack(ids)
				return
			}
			continue
		}
		if wait < 0 {
			wait = time.Hour
		}
		t.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-t.C:
		}
	}
}

func (w *siteWatcher) close() {
	w.mu.Lock()
	b := w.backend
	w.backend = nil
	w.mu.Unlock()
	if b != nil {
		b.close()
	}
}

// watchIgnored: a name in a site's folder whose changes are not copied
// (temporary uploads), or, at the top, the site's own logs and tmp.
func watchIgnored(name string, top bool) bool {
	return strings.HasPrefix(name, ".basalt-upload-") || top && (name == "logs" || name == "tmp")
}
