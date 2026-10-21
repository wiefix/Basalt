// Package walship keeps a standby copy of the panel's SQLite database: the
// source reads the committed frames SQLite appends to the database's WAL
// and sends them; the mirror appends them to its own copy of the WAL, so
// the copy is the database file plus a WAL, byte for byte the source's.
//
// This holds because the source alone moves the WAL back to its start:
// automatic checkpoints are off (store), and the source checkpoints while
// it holds the writer, after every follower has read the WAL to its end.
// A follower then tells its mirror to checkpoint too, which turns the same
// database file and frames into the same database file. SQLite itself
// recovers the copy from a WAL cut short.
package walship

import (
	"encoding/binary"
	"errors"
	"io"
)

const (
	walHeaderSize   = 32
	frameHeaderSize = 24
	walMagicLE      = 0x377f0682
	walMagicBE      = 0x377f0683
	walVersion      = 3007000
)

// walHeader is the header of a WAL file.
type walHeader struct {
	bigEndian    bool
	pageSize     uint32
	salt1, salt2 uint32
	s0, s1       uint32 // checksum of the header, where the frames' start
}

func (h walHeader) order() binary.ByteOrder {
	if h.bigEndian {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

// parseHeader checks and reads a WAL header (false: not a valid one).
func parseHeader(b []byte) (walHeader, bool) {
	if len(b) < walHeaderSize {
		return walHeader{}, false
	}
	be := binary.BigEndian
	var h walHeader
	switch be.Uint32(b) {
	case walMagicLE:
	case walMagicBE:
		h.bigEndian = true
	default:
		return walHeader{}, false
	}
	if be.Uint32(b[4:]) != walVersion {
		return walHeader{}, false
	}
	h.pageSize = be.Uint32(b[8:])
	if h.pageSize < 512 || h.pageSize > 65536 || h.pageSize&(h.pageSize-1) != 0 {
		return walHeader{}, false
	}
	h.salt1, h.salt2 = be.Uint32(b[16:]), be.Uint32(b[20:])
	h.s0, h.s1 = checksum(h.order(), 0, 0, b[:24])
	if h.s0 != be.Uint32(b[24:]) || h.s1 != be.Uint32(b[28:]) {
		return walHeader{}, false
	}
	return h, true
}

// checksum continues SQLite's WAL checksum over b (a multiple of 8 bytes).
func checksum(order binary.ByteOrder, s0, s1 uint32, b []byte) (uint32, uint32) {
	for i := 0; i+8 <= len(b); i += 8 {
		s0 += order.Uint32(b[i:]) + s1
		s1 += order.Uint32(b[i+4:]) + s0
	}
	return s0, s1
}

// position is how far a WAL was read: the end of its last committed frame
// and the checksum there. A zero position is before any header.
type position struct {
	hdr    walHeader
	off    int64
	s0, s1 uint32
}

func (p position) started() bool { return p.off > 0 }

// errRestarted: the WAL now belongs to another generation (it was moved
// back to its start after a checkpoint).
var errRestarted = errors.New("the WAL was restarted")

// scan reads the WAL r (of size bytes) after p and returns the position
// after its last committed frame; the bytes from p to there are what a
// mirror appends. A frame torn or not yet committed ends the scan.
func scan(r io.ReaderAt, size int64, p position) (position, error) {
	if size < walHeaderSize {
		if p.started() {
			return p, errRestarted
		}
		return p, nil
	}
	var hb [walHeaderSize]byte
	if _, err := r.ReadAt(hb[:], 0); err != nil {
		return p, err
	}
	h, ok := parseHeader(hb[:])
	switch {
	case !ok && p.started():
		return p, errRestarted
	case !ok:
		return p, nil // being written
	case p.started() && (h.salt1 != p.hdr.salt1 || h.salt2 != p.hdr.salt2):
		return p, errRestarted
	case !p.started():
		p = position{hdr: h, off: walHeaderSize, s0: h.s0, s1: h.s1}
	}
	if size < p.off {
		return p, errRestarted
	}
	frame := make([]byte, frameHeaderSize+int(h.pageSize))
	order := h.order()
	be := binary.BigEndian
	cur := p
	for off := p.off; off+int64(len(frame)) <= size; off += int64(len(frame)) {
		if _, err := r.ReadAt(frame, off); err != nil {
			return p, err
		}
		if be.Uint32(frame[8:]) != h.salt1 || be.Uint32(frame[12:]) != h.salt2 {
			break
		}
		s0, s1 := checksum(order, cur.s0, cur.s1, frame[:8])
		s0, s1 = checksum(order, s0, s1, frame[frameHeaderSize:])
		if s0 != be.Uint32(frame[16:]) || s1 != be.Uint32(frame[20:]) {
			break
		}
		cur.s0, cur.s1 = s0, s1
		cur.off = off + int64(len(frame))
		if be.Uint32(frame[4:]) != 0 { // a commit: the database's size in pages
			p = cur
		}
	}
	return p, nil
}
