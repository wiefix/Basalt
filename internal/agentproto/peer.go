package agentproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	TypeHeartbeat = "heartbeat"
	// TypeStream carries one frame of a streamed answer; its id is the
	// request's id.
	TypeStream = "stream"
	// TypeCancel asks the other side to stop serving the request with the
	// same id.
	TypeCancel = "cancel"

	DefaultHeartbeat   = 15 * time.Second
	DefaultIdleTimeout = 45 * time.Second
	maxConcurrent      = 16
	// Streaming requests (log follows, deployments) can run for a long
	// time. They have their own limit and are refused, never queued, when
	// it is reached, so they cannot starve ordinary requests.
	maxStreams = 64
	// streamBuffer frames are held for a slow consumer before its stream
	// is dropped; the read loop never waits for a consumer.
	streamBuffer = 256
)

var (
	ErrClosed         = errors.New("agentproto: connection closed")
	ErrIdleTimeout    = errors.New("agentproto: peer silent for too long")
	ErrStreamOverflow = errors.New("agentproto: stream consumer too slow")
	ErrTooManyStreams = errors.New("too many concurrent streams")
)

// RemoteError is an error returned by the other side's handler.
type RemoteError struct{ Msg string }

func (e *RemoteError) Error() string { return "remote: " + e.Msg }

// Handler serves requests and notifications from the other side. For
// notifications the returned value is ignored. The context is cancelled
// when the caller cancels the request or the connection ends; for a
// streaming request, StreamSender returns the function that sends frames.
type Handler func(ctx context.Context, msgType string, payload json.RawMessage) (any, error)

// Peer multiplexes requests, replies and notifications over one stream.
type Peer struct {
	r       io.Reader
	w       io.Writer
	flush   func() error
	handler Handler

	Heartbeat   time.Duration
	IdleTimeout time.Duration

	wmu    sync.Mutex
	closed bool

	nextID  atomic.Uint64
	pmu     sync.Mutex
	pending map[uint64]chan *Envelope
	streams map[uint64]*streamSink

	// imu guards incoming: the cancel functions of requests being served,
	// by the other side's request id.
	imu      sync.Mutex
	incoming map[uint64]context.CancelFunc

	done     chan struct{}
	doneOnce sync.Once
	sem      chan struct{}
	ssem     chan struct{}
}

type streamSink struct {
	frames   chan json.RawMessage
	overflow chan struct{} // closed when frames was full
}

// NewPeer wraps a stream. flush (may be nil) is called after every frame so
// messages are not held in buffers.
func NewPeer(r io.Reader, w io.Writer, flush func() error, h Handler) *Peer {
	return &Peer{
		r: r, w: w, flush: flush, handler: h,
		Heartbeat:   DefaultHeartbeat,
		IdleTimeout: DefaultIdleTimeout,
		pending:     map[uint64]chan *Envelope{},
		streams:     map[uint64]*streamSink{},
		incoming:    map[uint64]context.CancelFunc{},
		done:        make(chan struct{}),
		sem:         make(chan struct{}, maxConcurrent),
		ssem:        make(chan struct{}, maxStreams),
	}
}

// Done is closed when Run returns.
func (p *Peer) Done() <-chan struct{} { return p.done }

// Run reads frames until the stream fails, the peer goes silent or ctx ends.
// After Run returns the Peer never writes to the stream again.
func (p *Peer) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer p.shutdown()
	defer cancel(nil)

	frames := make(chan *Envelope)
	go func() {
		for {
			e, err := ReadFrame(p.r)
			if err != nil {
				cancel(err)
				return
			}
			select {
			case frames <- e:
			case <-ctx.Done():
				return
			}
		}
	}()

	hb := time.NewTicker(p.Heartbeat)
	defer hb.Stop()
	idle := time.NewTimer(p.IdleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			err := context.Cause(ctx)
			if errors.Is(err, io.EOF) {
				return ErrClosed
			}
			return err
		case <-idle.C:
			return ErrIdleTimeout
		case <-hb.C:
			if err := p.write(&Envelope{Type: TypeHeartbeat}); err != nil {
				return err
			}
		case e := <-frames:
			idle.Reset(p.IdleTimeout)
			if err := p.dispatch(ctx, e); err != nil {
				return err
			}
		}
	}
}

func (p *Peer) dispatch(ctx context.Context, e *Envelope) error {
	switch {
	case e.Reply:
		p.pmu.Lock()
		ch, ok := p.pending[e.ID]
		delete(p.pending, e.ID)
		p.pmu.Unlock()
		if ok {
			ch <- e
		}
		return nil
	case e.Type == TypeHeartbeat:
		return nil
	case e.Type == TypeStream:
		p.deliver(e)
		return nil
	case e.Type == TypeCancel:
		p.imu.Lock()
		if cancel := p.incoming[e.ID]; cancel != nil {
			cancel()
		}
		p.imu.Unlock()
		return nil
	case e.Type == "":
		return fmt.Errorf("agentproto: frame without type")
	}

	if e.ID != 0 && e.Stream {
		select {
		case p.ssem <- struct{}{}:
		default:
			go p.write(&Envelope{ID: e.ID, Reply: true, Error: ErrTooManyStreams.Error()})
			return nil
		}
		go func() {
			defer func() { <-p.ssem }()
			p.serve(ctx, e)
		}()
		return nil
	}
	// Bound concurrent handlers; a flood of requests applies back-pressure
	// to the reader instead of spawning unbounded goroutines.
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil
	}
	go func() {
		defer func() { <-p.sem }()
		p.serve(ctx, e)
	}()
	return nil
}

// deliver hands a stream frame to the waiting caller without blocking the
// read loop: a consumer that falls streamBuffer frames behind loses its stream.
func (p *Peer) deliver(e *Envelope) {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	s := p.streams[e.ID]
	if s == nil {
		return // cancelled meanwhile
	}
	select {
	case s.frames <- e.Payload:
	default:
		delete(p.streams, e.ID)
		close(s.overflow)
	}
}

type streamKey struct{}

// StreamSender returns the function a handler uses to send frames of a
// streamed answer, or nil when the caller did not ask for a stream.
func StreamSender(ctx context.Context) func(v any) error {
	f, _ := ctx.Value(streamKey{}).(func(any) error)
	return f
}

// serve runs the handler for one request or notification.
func (p *Peer) serve(ctx context.Context, e *Envelope) {
	if e.ID == 0 {
		p.handler(ctx, e.Type, e.Payload) // notification
		return
	}
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.imu.Lock()
	p.incoming[e.ID] = cancel
	p.imu.Unlock()
	defer func() {
		p.imu.Lock()
		delete(p.incoming, e.ID)
		p.imu.Unlock()
	}()
	hctx := reqCtx
	if e.Stream {
		hctx = context.WithValue(reqCtx, streamKey{}, func(v any) error {
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			if err := reqCtx.Err(); err != nil {
				return err
			}
			return p.write(&Envelope{ID: e.ID, Type: TypeStream, Payload: b})
		})
	}

	res, err := p.handler(hctx, e.Type, e.Payload)
	reply := &Envelope{ID: e.ID, Reply: true}
	if err != nil {
		reply.Error = err.Error()
	} else if res != nil {
		b, merr := json.Marshal(res)
		if merr != nil {
			reply.Error = "marshal reply: " + merr.Error()
		} else {
			reply.Payload = b
		}
	}
	_ = p.write(reply)
}

func (p *Peer) shutdown() {
	p.wmu.Lock()
	p.closed = true
	p.wmu.Unlock()
	p.doneOnce.Do(func() { close(p.done) })
}

func (p *Peer) write(e *Envelope) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if err := WriteFrame(p.w, e); err != nil {
		return err
	}
	if p.flush != nil {
		return p.flush()
	}
	return nil
}

// Notify sends a one-way message.
func (p *Peer) Notify(msgType string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return p.write(&Envelope{Type: msgType, Payload: b})
}

// Call sends a request and decodes the reply into resp (may be nil). If ctx
// ends first, the other side is asked to cancel the request.
func (p *Peer) Call(ctx context.Context, msgType string, req, resp any) error {
	return p.Stream(ctx, msgType, req, resp, nil)
}

// Stream is Call for requests answered with a stream of frames before the
// reply. fn (nil for a plain call) receives the frames in order from a
// goroutine of its own; an error from fn cancels the request and is returned.
// All frames are passed to fn before Stream returns.
func (p *Peer) Stream(ctx context.Context, msgType string, req, resp any, fn func(json.RawMessage) error) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	id := p.nextID.Add(1)
	ch := make(chan *Envelope, 1)
	var sink *streamSink
	if fn != nil {
		sink = &streamSink{frames: make(chan json.RawMessage, streamBuffer), overflow: make(chan struct{})}
	}
	p.pmu.Lock()
	p.pending[id] = ch
	if sink != nil {
		p.streams[id] = sink
	}
	p.pmu.Unlock()
	defer func() {
		p.pmu.Lock()
		delete(p.pending, id)
		delete(p.streams, id)
		p.pmu.Unlock()
	}()

	if err := p.write(&Envelope{ID: id, Type: msgType, Payload: b, Stream: fn != nil}); err != nil {
		return err
	}
	var frames <-chan json.RawMessage
	var overflow <-chan struct{}
	if sink != nil {
		frames, overflow = sink.frames, sink.overflow
	}
	interrupt, _ := ctx.Value(interruptKey{}).(<-chan struct{})
	for {
		select {
		case <-interrupt:
			// Ask the other side to stop, and keep waiting for its reply:
			// it reports what it did (and undid).
			p.cancel(id)
			interrupt = nil
		case f := <-frames:
			if err := fn(f); err != nil {
				p.cancel(id)
				return err
			}
		case e := <-ch:
			// Frames precede the reply on the wire, so all of them are queued.
			for drained := false; !drained && frames != nil; {
				select {
				case f := <-frames:
					if err := fn(f); err != nil {
						return err
					}
				default:
					drained = true
				}
			}
			if e.Error != "" {
				return &RemoteError{Msg: e.Error}
			}
			if resp != nil && len(e.Payload) > 0 {
				return json.Unmarshal(e.Payload, resp)
			}
			return nil
		case <-overflow:
			p.cancel(id)
			return ErrStreamOverflow
		case <-ctx.Done():
			p.cancel(id)
			return ctx.Err()
		case <-p.done:
			return ErrClosed
		}
	}
}

type interruptKey struct{}

// WithInterrupt returns a context whose requests (Stream, Call) are
// interrupted when ch closes: the other side is asked to stop, and the
// caller still receives its reply. Cancelling ctx itself abandons the reply.
func WithInterrupt(ctx context.Context, ch <-chan struct{}) context.Context {
	return context.WithValue(ctx, interruptKey{}, ch)
}

// cancel tells the other side to stop serving request id (best effort).
func (p *Peer) cancel(id uint64) {
	go p.write(&Envelope{ID: id, Type: TypeCancel})
}
