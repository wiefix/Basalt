package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Live output (container logs, deployments) reaches the browser as
// server-sent events: one-way, plain HTTP through any proxy, and the
// browser's EventSource reconnects by itself.

const (
	sseWriteTimeout   = 30 * time.Second
	sseKeepAlive      = 15 * time.Second
	maxStreamsPerUser = 16
)

type sse struct {
	mu sync.Mutex
	w  http.ResponseWriter
	rc *http.ResponseController
}

// startSSE sends the response headers of an event stream. The server's read
// and write timeouts do not fit a response that lasts hours; a deadline on
// every write replaces them.
func startSSE(w http.ResponseWriter) (*sse, error) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	s := &sse{w: w, rc: rc}
	return s, s.write(": stream\n\n")
}

// send writes one event whose data is v as JSON.
func (s *sse) send(event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.write(fmt.Sprintf("event: %s\ndata: %s\n\n", event, b))
}

func (s *sse) write(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	// An armed HTTP/2 deadline resets the stream when it passes, even when
	// idle: keep it only for the duration of the write.
	defer s.rc.SetWriteDeadline(time.Time{})
	if _, err := fmt.Fprint(s.w, text); err != nil {
		return err
	}
	return s.rc.Flush()
}

// keepAlive sends a comment periodically so idle proxies keep the
// connection open. The returned function stops it and waits.
func (s *sse) keepAlive(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(sseKeepAlive)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if s.write(": keep-alive\n\n") != nil {
					return
				}
			}
		}
	})
	return func() { cancel(); wg.Wait() }
}

// acquireStream bounds the event streams one user holds open.
func (s *Server) acquireStream(userID int64) bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.streams[userID] >= maxStreamsPerUser {
		return false
	}
	s.streams[userID]++
	return true
}

func (s *Server) releaseStream(userID int64) {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.streams[userID]--; s.streams[userID] <= 0 {
		delete(s.streams, userID)
	}
}
