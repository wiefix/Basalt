package webauthn

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// A minimal CBOR (RFC 8949) decoder for what authenticators send: integers,
// byte and text strings, arrays, maps and simple values, definite lengths
// only. Maps decode to map[any]any with int64 or string keys.

const (
	maxDepth = 8
	maxItems = 1024
)

var errCBOR = errors.New("malformed CBOR")

type decoder struct {
	b     []byte
	off   int
	items int
}

// decodeCBOR decodes one item at the start of b and returns it with the
// number of bytes it took.
func decodeCBOR(b []byte) (any, int, error) {
	d := &decoder{b: b}
	v, err := d.item(0)
	if err != nil {
		return nil, 0, err
	}
	return v, d.off, nil
}

func (d *decoder) head() (major byte, arg uint64, err error) {
	if d.off >= len(d.b) {
		return 0, 0, errCBOR
	}
	c := d.b[d.off]
	d.off++
	major, info := c>>5, c&0x1f
	switch {
	case info < 24:
		return major, uint64(info), nil
	case info <= 27:
		n := 1 << (info - 24)
		if d.off+n > len(d.b) {
			return 0, 0, errCBOR
		}
		var v uint64
		switch n {
		case 1:
			v = uint64(d.b[d.off])
		case 2:
			v = uint64(binary.BigEndian.Uint16(d.b[d.off:]))
		case 4:
			v = uint64(binary.BigEndian.Uint32(d.b[d.off:]))
		case 8:
			v = binary.BigEndian.Uint64(d.b[d.off:])
		}
		d.off += n
		return major, v, nil
	}
	return 0, 0, fmt.Errorf("%w: unsupported length encoding", errCBOR)
}

func (d *decoder) item(depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: nested too deeply", errCBOR)
	}
	if d.items++; d.items > maxItems {
		return nil, fmt.Errorf("%w: too many items", errCBOR)
	}
	major, arg, err := d.head()
	if err != nil {
		return nil, err
	}
	switch major {
	case 0:
		if arg > 1<<63-1 {
			return nil, errCBOR
		}
		return int64(arg), nil
	case 1:
		if arg > 1<<63-1 {
			return nil, errCBOR
		}
		return -1 - int64(arg), nil
	case 2, 3:
		if arg > uint64(len(d.b)-d.off) {
			return nil, errCBOR
		}
		s := d.b[d.off : d.off+int(arg)]
		d.off += int(arg)
		if major == 3 {
			return string(s), nil
		}
		return append([]byte(nil), s...), nil
	case 4:
		if arg > maxItems {
			return nil, errCBOR
		}
		out := make([]any, 0, arg)
		for range arg {
			v, err := d.item(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case 5:
		if arg > maxItems {
			return nil, errCBOR
		}
		out := make(map[any]any, arg)
		for range arg {
			k, err := d.item(depth + 1)
			if err != nil {
				return nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, fmt.Errorf("%w: unsupported map key", errCBOR)
			}
			if _, dup := out[k]; dup {
				return nil, fmt.Errorf("%w: duplicate map key", errCBOR)
			}
			v, err := d.item(depth + 1)
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	case 7:
		switch arg {
		case 20:
			return false, nil
		case 21:
			return true, nil
		case 22:
			return nil, nil
		}
	}
	return nil, fmt.Errorf("%w: unsupported item (major type %d)", errCBOR, major)
}
