package agentproto

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
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

func run(t *testing.T, ha, hb Handler) (*Peer, *Peer, context.Context) {
	t.Helper()
	a, b := pair(t, ha, hb)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Run(ctx)
	go b.Run(ctx)
	return a, b, ctx
}

func TestStreamFramesArriveInOrderBeforeReply(t *testing.T) {
	_, b, ctx := run(t, func(ctx context.Context, typ string, _ json.RawMessage) (any, error) {
		send := StreamSender(ctx)
		if send == nil {
			return nil, errors.New("no stream requested")
		}
		for i := range 1000 {
			if err := send(i); err != nil {
				return nil, err
			}
		}
		return map[string]int{"n": 1000}, nil
	}, echo)

	var got []int
	var resp map[string]int
	err := b.Stream(ctx, "count", nil, &resp, func(f json.RawMessage) error {
		var n int
		json.Unmarshal(f, &n)
		got = append(got, n)
		return nil
	})
	if err != nil || resp["n"] != 1000 || len(got) != 1000 {
		t.Fatalf("err=%v resp=%v frames=%d", err, resp, len(got))
	}
	for i, n := range got {
		if n != i {
			t.Fatalf("frame %d = %d: out of order", i, n)
		}
	}
	// A plain call gets no stream sender.
	var out map[string]string
	if err := b.Call(ctx, "count", nil, &out); err == nil || !strings.Contains(err.Error(), "no stream requested") {
		t.Fatalf("plain call: %v", err)
	}
}

func TestCancelReachesHandler(t *testing.T) {
	cancelled := make(chan struct{})
	started := make(chan struct{})
	_, b, ctx := run(t, func(ctx context.Context, typ string, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	}, echo)

	cctx, cancel := context.WithCancel(ctx)
	go func() { <-started; cancel() }()
	if err := b.Call(cctx, "block", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Call = %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler not cancelled")
	}
}

func TestStreamConsumerErrorStopsProducer(t *testing.T) {
	stopped := make(chan error, 1)
	_, b, ctx := run(t, func(ctx context.Context, typ string, _ json.RawMessage) (any, error) {
		send := StreamSender(ctx)
		for {
			if err := send("x"); err != nil {
				stopped <- err
				return nil, err
			}
			time.Sleep(time.Millisecond)
		}
	}, echo)
	boom := errors.New("client went away")
	if err := b.Stream(ctx, "tail", nil, nil, func(json.RawMessage) error { return boom }); err != boom {
		t.Fatalf("Stream = %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("producer kept sending after the consumer gave up")
	}
}

func TestSlowStreamConsumerIsDropped(t *testing.T) {
	sent := make(chan struct{})
	_, b, ctx := run(t, func(ctx context.Context, typ string, p json.RawMessage) (any, error) {
		if typ == "fail" {
			return echo(ctx, typ, p)
		}
		send := StreamSender(ctx)
		for range streamBuffer * 4 {
			if send("x") != nil {
				break
			}
		}
		close(sent)
		<-ctx.Done()
		return nil, nil
	}, echo)
	first := true
	err := b.Stream(ctx, "flood", nil, nil, func(json.RawMessage) error {
		if first {
			first = false
			<-sent // stall while the producer floods
		}
		return nil
	})
	if !errors.Is(err, ErrStreamOverflow) {
		t.Fatalf("Stream = %v; want ErrStreamOverflow", err)
	}
	// The connection itself is unaffected.
	var out map[string]string
	if err := b.Call(ctx, "fail", nil, &out); err == nil {
		t.Fatal("expected remote error")
	}
}

func TestStreamLimitRefusesInsteadOfBlocking(t *testing.T) {
	var running atomic.Int64
	_, b, ctx := run(t, func(ctx context.Context, typ string, _ json.RawMessage) (any, error) {
		if typ == "ping" {
			return "pong", nil
		}
		running.Add(1)
		<-ctx.Done()
		return nil, nil
	}, echo)
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	for range maxStreams {
		go b.Stream(sctx, "hold", nil, nil, func(json.RawMessage) error { return nil })
	}
	deadline := time.Now().Add(5 * time.Second)
	for running.Load() < maxStreams && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	var re *RemoteError
	if err := b.Stream(ctx, "hold", nil, nil, func(json.RawMessage) error { return nil }); !errors.As(err, &re) ||
		!strings.Contains(re.Msg, "too many") {
		t.Fatalf("stream over the limit: %v", err)
	}
	// Ordinary requests still go through.
	var pong string
	if err := b.Call(ctx, "ping", nil, &pong); err != nil || pong != "pong" {
		t.Fatalf("ping = %q, %v", pong, err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestInterruptKeepsReply: an interrupted request still returns the other
// side's reply (what it did on being stopped).
func TestInterruptKeepsReply(t *testing.T) {
	started := make(chan struct{})
	_, b, ctx := run(t, func(ctx context.Context, typ string, p json.RawMessage) (any, error) {
		if typ != "work" {
			return echo(ctx, typ, p)
		}
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // cleaning up
		return map[string]string{"outcome": "rolled back"}, nil
	}, echo)

	stop := make(chan struct{})
	go func() { <-started; close(stop) }()
	var resp map[string]string
	if err := b.Call(WithInterrupt(ctx, stop), "work", nil, &resp); err != nil || resp["outcome"] != "rolled back" {
		t.Fatalf("Call = %v, %v", resp, err)
	}
	// Not interrupted: a normal reply.
	resp = nil
	if err := b.Call(WithInterrupt(ctx, make(chan struct{})), "plain", nil, &resp); err != nil || resp["type"] != "plain" {
		t.Fatalf("plain: %v %v", resp, err)
	}
}
