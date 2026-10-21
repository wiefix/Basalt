package walship

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Kind is what a message carries.
type Kind uint8

const (
	// KindSnapshotDB is a piece of the database file of a snapshot (they
	// come in order, the first at offset 0).
	KindSnapshotDB Kind = iota + 1
	// KindSnapshotWAL is a piece of the WAL of a snapshot.
	KindSnapshotWAL
	// KindSnapshotEnd ends a snapshot: the mirror is now the source.
	KindSnapshotEnd
	// KindFrames are committed frames to append to the WAL.
	KindFrames
	// KindCheckpoint: the source checkpointed; the mirror does the same and
	// starts a new WAL.
	KindCheckpoint
)

// Msg is one step of keeping a mirror; Offset is where Data goes in its
// file.
type Msg struct {
	Kind   Kind
	Offset int64
	Data   []byte
}

// Writer runs fn holding the database's only writer, so nothing commits
// meanwhile.
type Writer interface {
	WithWriter(ctx context.Context, fn func(ctx context.Context, conn *sql.Conn) error) error
}

const (
	pollEvery       = 200 * time.Millisecond
	checkpointEvery = time.Minute
	checkpointSize  = 4 << 20  // a WAL bigger than this is checkpointed soon
	followerQueue   = 64 << 20 // most bytes waiting for a slow follower
	chunk           = 1 << 20
)

// Source checkpoints the database and feeds followers. Run must be running
// for the database's WAL not to grow without end.
type Source struct {
	path string // the database file; the WAL is path + "-wal"
	w    Writer
	log  *slog.Logger

	mu        sync.Mutex
	followers map[*follower]bool
}

func NewSource(path string, w Writer, log *slog.Logger) *Source {
	return &Source{path: path, w: w, log: log, followers: map[*follower]bool{}}
}

// follower is one mirror being fed: what was read from the WAL waits in
// queue until it is sent.
type follower struct {
	pos    position
	mu     sync.Mutex
	queue  []Msg
	queued int
	broken error
	wake   chan struct{}
}

func (f *follower) push(m Msg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken != nil {
		return
	}
	if f.queued+len(m.Data) > followerQueue {
		f.broken, f.queue, f.queued = errors.New("the standby falls too far behind"), nil, 0
	} else {
		f.queue = append(f.queue, m)
		f.queued += len(m.Data)
	}
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *follower) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken == nil {
		f.broken, f.queue, f.queued = err, nil, 0
	}
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *follower) take() ([]Msg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q, err := f.queue, f.broken
	f.queue, f.queued = nil, 0
	return q, err
}

// read queues what was committed to the WAL since the follower last read
// it; a new generation of the WAL starts with a checkpoint.
func (s *Source) read(f *follower, wal *os.File) {
	if wal == nil {
		return
	}
	fi, err := wal.Stat()
	if err != nil {
		return
	}
	next, err := scan(wal, fi.Size(), f.pos)
	if errors.Is(err, errRestarted) {
		// The source checkpointed everything this follower had read (only
		// the source's checkpoints, with every follower read to the end,
		// let SQLite move the WAL back).
		f.push(Msg{Kind: KindCheckpoint})
		f.pos = position{}
		next, err = scan(wal, fi.Size(), f.pos)
	}
	if err != nil {
		f.fail(fmt.Errorf("read the WAL: %w", err))
		return
	}
	for off := f.pos.off; off < next.off; {
		n := min(int64(chunk), next.off-off)
		b := make([]byte, n)
		if _, err := wal.ReadAt(b, off); err != nil {
			f.fail(fmt.Errorf("read the WAL: %w", err))
			return
		}
		f.push(Msg{Kind: KindFrames, Offset: off, Data: b})
		off += n
	}
	f.pos = next
}

func (s *Source) openWAL() *os.File {
	f, err := os.Open(s.path + "-wal")
	if err != nil {
		return nil
	}
	return f
}

// readAll lets every follower read the WAL.
func (s *Source) readAll() {
	wal := s.openWAL()
	if wal != nil {
		defer wal.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for f := range s.followers {
		s.read(f, wal)
	}
}

// Run polls the WAL for followers and checkpoints it, until ctx ends.
func (s *Source) Run(ctx context.Context) {
	poll := time.NewTicker(pollEvery)
	defer poll.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
		s.readAll()
		size := int64(0)
		if fi, err := os.Stat(s.path + "-wal"); err == nil {
			size = fi.Size()
		}
		if size > 0 && (size > checkpointSize || time.Since(last) >= checkpointEvery) {
			if err := s.Checkpoint(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("checkpoint the database", "err", err)
			}
			last = time.Now()
		}
	}
}

// Checkpoint moves the WAL into the database file, holding the writer
// after every follower read the WAL to its end.
func (s *Source) Checkpoint(ctx context.Context) error {
	return s.w.WithWriter(ctx, func(ctx context.Context, conn *sql.Conn) error {
		s.readAll()
		// A long read must not hold every writer up: a checkpoint it blocks
		// is tried again later.
		if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout = 100`); err != nil {
			return err
		}
		var busy, logFrames, done int
		err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done)
		if _, rerr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA busy_timeout = 5000`); err == nil {
			err = rerr
		}
		if err != nil {
			return err
		}
		// Truncated: followers learn of it now. Otherwise the WAL goes on, or,
		// if all of it was checkpointed, the next commit restarts it and
		// followers see that (read).
		if fi, err := os.Stat(s.path + "-wal"); err == nil && fi.Size() == 0 {
			s.mu.Lock()
			for f := range s.followers {
				if f.pos.started() {
					f.push(Msg{Kind: KindCheckpoint})
					f.pos = position{}
				}
			}
			s.mu.Unlock()
		}
		return nil
	})
}

// Follow feeds a mirror until ctx ends or send fails: a snapshot of the
// database, then what is committed. A follower that falls too far behind
// is dropped (its mirror starts over with a new snapshot).
func (s *Source) Follow(ctx context.Context, send func(Msg) error) error {
	f := &follower{wake: make(chan struct{}, 1)}
	dbCopy, walCopy, err := s.snapshot(ctx, f)
	if err != nil {
		return err
	}
	defer func() {
		s.mu.Lock()
		delete(s.followers, f)
		s.mu.Unlock()
	}()
	err = sendFile(dbCopy, KindSnapshotDB, send)
	if err == nil {
		err = sendFile(walCopy, KindSnapshotWAL, send)
	}
	os.Remove(dbCopy)
	os.Remove(walCopy)
	if err != nil {
		return err
	}
	if err := send(Msg{Kind: KindSnapshotEnd}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.wake:
		}
		q, err := f.take()
		for _, m := range q {
			if err := send(m); err != nil {
				return err
			}
		}
		if err != nil {
			return err
		}
	}
}

// snapshot copies the database file and the committed part of its WAL
// while holding the writer, and starts f where the copy ends.
func (s *Source) snapshot(ctx context.Context, f *follower) (dbCopy, walCopy string, err error) {
	err = s.w.WithWriter(ctx, func(ctx context.Context, _ *sql.Conn) error {
		dbCopy, err = copyFile(s.path, -1)
		if err != nil {
			return err
		}
		var end int64
		if wal := s.openWAL(); wal != nil {
			fi, err := wal.Stat()
			if err == nil {
				f.pos, err = scan(wal, fi.Size(), position{})
			}
			wal.Close()
			if err != nil {
				return err
			}
			end = f.pos.off
		}
		walCopy, err = copyFile(s.path+"-wal", end)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.followers[f] = true
		s.mu.Unlock()
		return nil
	})
	if err != nil {
		if dbCopy != "" {
			os.Remove(dbCopy)
		}
		if walCopy != "" {
			os.Remove(walCopy)
		}
	}
	return dbCopy, walCopy, err
}

// copyFile copies the first n bytes of src (n < 0: all of it; a missing
// file is empty) into a new private file next to it.
func copyFile(src string, n int64) (string, error) {
	out, err := os.CreateTemp(filepath.Dir(src), ".basalt-standby-*")
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		out.Close()
		os.Remove(out.Name())
		return "", err
	}
	in, err := os.Open(src)
	if errors.Is(err, os.ErrNotExist) {
		return out.Name(), out.Close()
	} else if err != nil {
		return fail(err)
	}
	defer in.Close()
	var r io.Reader = in
	if n >= 0 {
		r = io.LimitReader(in, n)
	}
	if _, err := io.Copy(out, r); err != nil {
		return fail(err)
	}
	return out.Name(), out.Close()
}

// sendFile sends a file in pieces of kind.
func sendFile(path string, kind Kind, send func(Msg) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var off int64
	for {
		b := make([]byte, chunk)
		n, err := io.ReadFull(f, b)
		if n > 0 {
			if err := send(Msg{Kind: kind, Offset: off, Data: b[:n]}); err != nil {
				return err
			}
			off += int64(n)
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
