package server

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLiveLogFollowers(t *testing.T) {
	r := &liveLogs{m: map[int64]*liveLog{}}
	l := r.start(7)
	l.append("one\n")

	// A follower that joins late still gets everything, in order, and
	// returns when the deployment ends.
	var mu sync.Mutex
	var got strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- l.follow(context.Background(), func(s string) error {
			mu.Lock()
			defer mu.Unlock()
			got.WriteString(s)
			return nil
		})
	}()
	l.append("two\n")
	time.Sleep(20 * time.Millisecond)
	l.append("three\n")
	if r.get(7) != l {
		t.Fatal("running deployment not registered")
	}
	r.end(7)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follower did not return after the end")
	}
	if got.String() != "one\ntwo\nthree\n" {
		t.Fatalf("followed %q", got.String())
	}
	if r.get(7) != nil {
		t.Fatal("finished deployment still registered")
	}
	l.append("late\n") // ignored after the end
	var after string
	l.follow(context.Background(), func(s string) error { after += s; return nil })
	if after != "one\ntwo\nthree\n" {
		t.Fatalf("after end: %q", after)
	}

	// A cancelled follower returns at once.
	l2 := r.start(8)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l2.follow(ctx, func(string) error { return nil }); err == nil {
		t.Fatal("cancelled follower did not report it")
	}

	// Output is bounded.
	for range 2 * maxLiveLog / 1024 {
		l2.append(strings.Repeat("x", 1023) + "\n")
	}
	var size int
	r.end(8)
	l2.follow(context.Background(), func(s string) error { size += len(s); return nil })
	if size > maxLiveLog+200 {
		t.Fatalf("live log grew to %d bytes", size)
	}
}
