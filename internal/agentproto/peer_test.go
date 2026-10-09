package agentproto

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func pair(t *testing.T, ha, hb Handler) (*Peer, *Peer) {
	t.Helper()
	ca, cb := net.Pipe()
	t.Cleanup(func() { ca.Close(); cb.Close() })
	return NewPeer(ca, ca, nil, ha), NewPeer(cb, cb, nil, hb)
}

func echo(ctx context.Context, typ string, p json.RawMessage) (any, error) {
	if typ == "fail" {
		return nil, errors.New("boom")
	}
	return map[string]string{"type": typ, "got": string(p)}, nil
}

func TestCallBothDirections(t *testing.T) {
	a, b := pair(t, echo, echo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	go b.Run(ctx)

	var out map[string]string
	if err := a.Call(ctx, "hi", 1, &out); err != nil || out["type"] != "hi" || out["got"] != "1" {
		t.Fatalf("a->b: %v %v", out, err)
	}
	if err := b.Call(ctx, "yo", "x", &out); err != nil || out["got"] != `"x"` {
		t.Fatalf("b->a: %v %v", out, err)
	}
	var re *RemoteError
	if err := a.Call(ctx, "fail", nil, nil); !errors.As(err, &re) || re.Msg != "boom" {
		t.Fatalf("remote error = %v", err)
	}
}

func TestConcurrentCalls(t *testing.T) {
	a, b := pair(t, echo, echo)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	go b.Run(ctx)

	errs := make(chan error, 50)
	for i := range 50 {
		go func() {
			var out map[string]string
			err := a.Call(ctx, "n", i, &out)
			if err == nil && out["got"] != string(must(json.Marshal(i))) {
				err = errors.New("reply routed to wrong caller")
			}
			errs <- err
		}()
	}
	for range 50 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestNotification(t *testing.T) {
	got := make(chan string, 1)
	a, b := pair(t, echo, func(ctx context.Context, typ string, p json.RawMessage) (any, error) {
		got <- typ + string(p)
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)
	go b.Run(ctx)
	if err := a.Notify("metrics", 42); err != nil {
		t.Fatal(err)
	}
	if s := <-got; s != "metrics42" {
		t.Fatalf("got %q", s)
	}
}

func TestIdleTimeoutAndHeartbeat(t *testing.T) {
	a, b := pair(t, echo, echo)
	for _, p := range []*Peer{a, b} {
		p.Heartbeat = 20 * time.Millisecond
		p.IdleTimeout = 100 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errA := make(chan error, 1)
	go func() { errA <- a.Run(ctx) }()
	go b.Run(ctx)

	// Heartbeats keep the connection alive well beyond the idle timeout.
	select {
	case err := <-errA:
		t.Fatalf("peer dropped despite heartbeats: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// A silent peer is dropped.
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go io.Copy(io.Discard, c2)
	silent := NewPeer(c1, c1, nil, echo)
	silent.Heartbeat = time.Hour
	silent.IdleTimeout = 50 * time.Millisecond
	if err := silent.Run(ctx); !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("Run = %v; want ErrIdleTimeout", err)
	}
}

func TestCallFailsWhenPeerCloses(t *testing.T) {
	ca, cb := net.Pipe()
	a := NewPeer(ca, ca, nil, echo)
	go io.Copy(io.Discard, cb) // swallow requests, never reply
	done := make(chan error, 1)
	go func() { done <- a.Run(context.Background()) }()

	callErr := make(chan error, 1)
	go func() { callErr <- a.Call(context.Background(), "x", nil, nil) }()
	time.Sleep(20 * time.Millisecond)
	cb.Close()
	if err := <-callErr; !errors.Is(err, ErrClosed) {
		t.Fatalf("Call = %v; want ErrClosed", err)
	}
	<-done
	if err := a.Notify("x", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close = %v; want ErrClosed", err)
	}
}

func TestOversizedFrameRejected(t *testing.T) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, uint32(MaxFrameSize+1))
	if _, err := ReadFrame(&buf); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("ReadFrame = %v; want ErrFrameTooLarge", err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
