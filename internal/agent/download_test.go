package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadGetRetries(t *testing.T) {
	pause, maxPause := downloadPause, downloadMaxPause
	downloadPause, downloadMaxPause = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { downloadPause, downloadMaxPause = pause, maxPause })

	// serve answers with codes in turn, then 200.
	serve := func(codes ...int) (string, *atomic.Int32) {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.UserAgent(), "Basalt/") {
				t.Errorf("User-Agent %q", r.UserAgent())
			}
			i := int(n.Add(1)) - 1
			if i < len(codes) {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(codes[i])
				return
			}
			w.Write([]byte("ok"))
		}))
		t.Cleanup(srv.Close)
		return srv.URL, &n
	}
	ctx := context.Background()
	hc := &http.Client{Timeout: 5 * time.Second}

	u, n := serve(429, 503, 429)
	resp, err := downloadGet(ctx, hc, u)
	if err != nil || n.Load() != 4 {
		t.Fatalf("after three refusals: %v, %d requests", err, n.Load())
	}
	resp.Body.Close()

	u, n = serve(429, 429, 429, 429)
	if _, err := downloadGet(ctx, hc, u); err == nil || !strings.Contains(err.Error(), "429") || n.Load() != int32(downloadAttempts) {
		t.Fatalf("refused every time: %v, %d requests", err, n.Load())
	}

	// A lasting answer is not tried again.
	u, n = serve(404)
	if _, err := downloadGet(ctx, hc, u); err == nil || n.Load() != 1 {
		t.Fatalf("404: %v, %d requests", err, n.Load())
	}

	// The pause gives way to the context.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	downloadMaxPause = time.Hour
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := downloadGet(cctx, hc, srv.URL); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("cancelled pause: %v after %v", err, time.Since(start))
	}
}
