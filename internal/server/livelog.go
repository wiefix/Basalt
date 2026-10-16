package server

import (
	"context"
	"sync"
)

const maxLiveLog = 256 << 10

// liveLog holds the output of a deployment in progress so any number of
// browsers can follow it from the start. It lives only until the
// deployment's outcome is in the database.
type liveLog struct {
	mu        sync.Mutex
	buf       []byte
	changed   chan struct{} // closed and replaced on every change
	done      bool
	truncated bool
}

func newLiveLog() *liveLog { return &liveLog{changed: make(chan struct{})} }

func (l *liveLog) append(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done || l.truncated {
		return
	}
	if len(l.buf)+len(s) > maxLiveLog {
		s = "… (live output truncated; the full log is shown when the deployment ends)\n"
		l.truncated = true
	}
	l.buf = append(l.buf, s...)
	l.notify()
}

// text is everything written so far.
func (l *liveLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.buf)
}

func (l *liveLog) finish() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.done {
		l.done = true
		l.notify()
	}
}

func (l *liveLog) notify() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// follow passes everything written so far, then each addition, to fn until
// the log is finished or ctx ends.
func (l *liveLog) follow(ctx context.Context, fn func(string) error) error {
	off := 0
	for {
		l.mu.Lock()
		chunk := string(l.buf[off:])
		off = len(l.buf)
		done, changed := l.done, l.changed
		l.mu.Unlock()
		if chunk != "" {
			if err := fn(chunk); err != nil {
				return err
			}
		}
		if done {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// liveLogs indexes the logs of running deployments by deployment ID.
type liveLogs struct {
	mu sync.Mutex
	m  map[int64]*liveLog
}

func (r *liveLogs) start(id int64) *liveLog {
	l := newLiveLog()
	r.mu.Lock()
	r.m[id] = l
	r.mu.Unlock()
	return l
}

func (r *liveLogs) get(id int64) *liveLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[id]
}

// end finishes the log and forgets it: from now on the database has it.
func (r *liveLogs) end(id int64) {
	r.mu.Lock()
	l := r.m[id]
	delete(r.m, id)
	r.mu.Unlock()
	if l != nil {
		l.finish()
	}
}
