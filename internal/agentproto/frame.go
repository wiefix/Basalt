// Package agentproto defines the wire protocol between the control plane and
// agents: length-prefixed JSON frames carried over one long-lived,
// bidirectional HTTP/2 stream secured with mutual TLS.
//
// Either side may send requests (id > 0), replies (reply = true, same id) and
// notifications (id = 0). A request may be answered with stream frames
// (type "stream", same id) before its reply, and the caller may cancel it
// (type "cancel", same id). Both sides send heartbeats; a silent peer is
// dropped.
package agentproto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxFrameSize bounds a single message so a misbehaving peer cannot make the
// other side allocate unbounded memory.
const MaxFrameSize = 4 << 20

var ErrFrameTooLarge = errors.New("agentproto: frame too large")

type Envelope struct {
	ID      uint64          `json:"id,omitempty"`
	Type    string          `json:"type,omitempty"`
	Reply   bool            `json:"reply,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   string          `json:"error,omitempty"`
	// Stream, on a request, says the caller accepts stream frames (type
	// "stream", same id) before the reply.
	Stream bool `json:"stream,omitempty"`
}

func WriteFrame(w io.Writer, e *Envelope) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

func ReadFrame(r io.Reader) (*Envelope, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, fmt.Errorf("read frame body: %w", err)
	}
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	return &e, nil
}
