package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/docker"
)

const (
	followSwitchTail = 1000 // history shown for a container that replaced the followed one
	followPollGap    = 2 * time.Second
	logFlushGap      = 100 * time.Millisecond
	logBatchBytes    = 32 << 10
)

// FollowLogs streams the output of the app's current container (of service,
// for a compose app) to send, first the last tail lines, then live. When a
// deployment replaces the container, or the container restarts, it follows
// the new one. It returns when ctx ends or send fails.
func (m *appsManager) FollowLogs(ctx context.Context, appID int64, service string, tail int, send func(string) error) error {
	if appID <= 0 {
		return fmt.Errorf("invalid app id")
	}
	dc, err := m.client()
	if err != nil {
		return err
	}
	tail = min(max(tail, 1), maxLogTail)
	var followed string     // container ID last followed
	var stoppedAt time.Time // when its stream ended
	notice := ""            // last notice sent, to avoid repeating it
	say := func(s string) error {
		if s == notice {
			return nil
		}
		notice = s
		return send("--- " + s + " ---\n")
	}

	for ctx.Err() == nil {
		c, err := m.current(ctx, dc, appID, service)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		var opts docker.LogsOptions
		switch {
		case c == nil:
			if err := say("no container yet; waiting for a deployment"); err != nil {
				return err
			}
		case c.ID != followed:
			opts = docker.LogsOptions{Follow: true, Tail: tail, Timestamps: true}
			if followed != "" {
				opts.Tail = followSwitchTail
				if err := say(fmt.Sprintf("now following deployment #%d", labelInt(c.Labels, labelDeployment))); err != nil {
					return err
				}
			}
		case c.State == "running":
			// The same container restarted: continue where its stream ended.
			opts = docker.LogsOptions{Follow: true, Since: stoppedAt, Timestamps: true}
			if err := say("container restarted"); err != nil {
				return err
			}
		default:
			if err := say("container " + c.State); err != nil {
				return err
			}
		}
		if opts.Follow {
			followed, notice = c.ID, ""
			if err := m.follow(ctx, dc, c.ID, opts, send); err != nil {
				if ctx.Err() != nil {
					break
				}
				return err
			}
			stoppedAt = time.Now()
		}
		select {
		case <-ctx.Done():
		case <-time.After(followPollGap):
		}
	}
	return nil
}

// follow copies one container's log stream to send until it ends.
func (m *appsManager) follow(ctx context.Context, dc *docker.Client, id string, opts docker.LogsOptions, send func(string) error) error {
	rc, err := dc.ContainerLogs(ctx, id, opts)
	if err != nil {
		return err
	}
	defer rc.Close()
	b := newLineBatcher(send)
	defer b.stop()
	if err := docker.Demux(rc, b, b); err != nil && ctx.Err() == nil {
		if berr := b.failed(); berr != nil {
			return berr
		}
		return err
	}
	return b.flush()
}

// lineBatcher groups log output into frames sent every logFlushGap or when
// logBatchBytes accumulate, so a chatty app does not cost a frame per line.
type lineBatcher struct {
	send func(string) error
	done chan struct{}
	once sync.Once

	mu  sync.Mutex
	buf []byte
	err error
}

func newLineBatcher(send func(string) error) *lineBatcher {
	b := &lineBatcher{send: send, done: make(chan struct{})}
	go func() {
		t := time.NewTicker(logFlushGap)
		defer t.Stop()
		for {
			select {
			case <-b.done:
				return
			case <-t.C:
				b.flush()
			}
		}
	}()
	return b
}

func (b *lineBatcher) Write(p []byte) (int, error) {
	b.mu.Lock()
	if b.err != nil {
		b.mu.Unlock()
		return 0, b.err
	}
	b.buf = append(b.buf, p...)
	full := len(b.buf) >= logBatchBytes
	b.mu.Unlock()
	if full {
		if err := b.flush(); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// flush sends what is buffered. The lock is held while sending so frames
// keep their order.
func (b *lineBatcher) flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil || len(b.buf) == 0 {
		return b.err
	}
	b.err = b.send(string(b.buf))
	b.buf = b.buf[:0]
	return b.err
}

func (b *lineBatcher) failed() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.err
}

func (b *lineBatcher) stop() { b.once.Do(func() { close(b.done) }) }
