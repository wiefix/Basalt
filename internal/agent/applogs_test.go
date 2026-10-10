package agent

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLineBatcher(t *testing.T) {
	var mu sync.Mutex
	var frames []string
	b := newLineBatcher(func(s string) error {
		mu.Lock()
		defer mu.Unlock()
		frames = append(frames, s)
		return nil
	})
	defer b.stop()

	// Many small writes become few frames, in order.
	var want strings.Builder
	for i := range 200 {
		line := fmt.Sprintf("line %d\n", i)
		want.WriteString(line)
		b.Write([]byte(line))
	}
	time.Sleep(3 * logFlushGap)
	mu.Lock()
	n, got := len(frames), strings.Join(frames, "")
	mu.Unlock()
	if got != want.String() {
		t.Fatal("output reordered or lost")
	}
	if n > 5 {
		t.Fatalf("%d frames for 200 small lines; want them batched", n)
	}

	// A large burst is flushed without waiting for the timer.
	b.Write([]byte(strings.Repeat("x", logBatchBytes)))
	mu.Lock()
	big := len(frames) > n
	mu.Unlock()
	if !big {
		t.Fatal("a full batch was not sent at once")
	}
}

func TestLineBatcherStopsOnSendError(t *testing.T) {
	gone := errors.New("panel went away")
	b := newLineBatcher(func(string) error { return gone })
	defer b.stop()
	b.Write([]byte("x\n"))
	if err := b.flush(); !errors.Is(err, gone) {
		t.Fatalf("flush = %v", err)
	}
	if _, err := b.Write([]byte("y\n")); !errors.Is(err, gone) {
		t.Fatalf("write after failure = %v", err)
	}
}
