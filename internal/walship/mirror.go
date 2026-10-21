package walship

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	_ "modernc.org/sqlite" // checkpoints the copy
)

// Mirror keeps a copy of a source's database at path (with its WAL at
// path + "-wal"). Nothing else may open the copy while it is kept.
type Mirror struct {
	path string

	// During a snapshot: the files being written.
	snapDB, snapWAL *os.File
	// After one: the WAL appended to, and its size.
	wal     *os.File
	walSize int64
	ready   bool
	// Applied is when the copy last took a change.
	Applied time.Time
}

func NewMirror(path string) *Mirror { return &Mirror{path: path} }

// Ready: the copy holds a complete snapshot and what followed it.
func (m *Mirror) Ready() bool { return m.ready }

// Close stops keeping the copy (it stays as it is).
func (m *Mirror) Close() error {
	m.abortSnapshot()
	if m.wal != nil {
		err := m.wal.Close()
		m.wal = nil
		return err
	}
	return nil
}

func (m *Mirror) abortSnapshot() {
	for _, f := range []*os.File{m.snapDB, m.snapWAL} {
		if f != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}
	m.snapDB, m.snapWAL = nil, nil
}

var errOrder = errors.New("the change does not follow what the copy holds")

// Apply takes the next message from the source. An error leaves the copy
// as it was before the message (whole snapshots and whole commits); the
// mirror then needs a new snapshot.
func (m *Mirror) Apply(ctx context.Context, msg Msg) error {
	switch msg.Kind {
	case KindSnapshotDB, KindSnapshotWAL:
		return m.snapshotPart(msg)
	case KindSnapshotEnd:
		return m.endSnapshot()
	case KindFrames:
		return m.append(msg)
	case KindCheckpoint:
		return m.checkpoint(ctx)
	}
	return fmt.Errorf("unknown change %d", msg.Kind)
}

func (m *Mirror) snapshotPart(msg Msg) error {
	if msg.Kind == KindSnapshotDB && msg.Offset == 0 && m.snapDB == nil {
		// A new snapshot: the copy is not usable until it ends.
		m.Close()
		m.ready = false
		var err error
		if m.snapDB, err = os.OpenFile(m.path+".snapshot", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
			return err
		}
		if m.snapWAL, err = os.OpenFile(m.path+"-wal.snapshot", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
			m.abortSnapshot()
			return err
		}
	}
	f := m.snapDB
	if msg.Kind == KindSnapshotWAL {
		f = m.snapWAL
	}
	if f == nil {
		return errOrder
	}
	if fi, err := f.Stat(); err != nil || fi.Size() != msg.Offset {
		m.abortSnapshot()
		return errOrder
	}
	if _, err := f.Write(msg.Data); err != nil {
		m.abortSnapshot()
		return err
	}
	return nil
}

func (m *Mirror) endSnapshot() error {
	if m.snapDB == nil {
		return errOrder
	}
	for _, f := range []*os.File{m.snapDB, m.snapWAL} {
		if err := f.Sync(); err != nil {
			m.abortSnapshot()
			return err
		}
	}
	db, wal := m.snapDB.Name(), m.snapWAL.Name()
	m.snapDB.Close()
	m.snapWAL.Close()
	m.snapDB, m.snapWAL = nil, nil
	// The WAL first: a crash in between leaves the old database with a WAL
	// SQLite ignores (other salts), or the new one with its own.
	os.Remove(m.path + "-shm")
	if err := os.Rename(wal, m.path+"-wal"); err != nil {
		os.Remove(db)
		return err
	}
	if err := os.Rename(db, m.path); err != nil {
		return err
	}
	if err := m.openWAL(); err != nil {
		return err
	}
	m.ready = true
	m.Applied = time.Now()
	return nil
}

func (m *Mirror) openWAL() error {
	f, err := os.OpenFile(m.path+"-wal", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	m.wal, m.walSize = f, fi.Size()
	return nil
}

func (m *Mirror) append(msg Msg) error {
	if !m.ready || m.wal == nil || msg.Offset != m.walSize {
		return errOrder
	}
	if _, err := m.wal.WriteAt(msg.Data, msg.Offset); err != nil {
		m.wal.Truncate(m.walSize)
		return err
	}
	if err := m.wal.Sync(); err != nil {
		return err
	}
	m.walSize += int64(len(msg.Data))
	m.Applied = time.Now()
	return nil
}

// checkpoint does what the source did: the WAL into the database file,
// then a new WAL.
func (m *Mirror) checkpoint(ctx context.Context) error {
	if !m.ready {
		return errOrder
	}
	m.wal.Close()
	m.wal = nil
	m.ready = false
	if err := Checkpoint(ctx, m.path); err != nil {
		return err
	}
	if err := m.openWAL(); err != nil {
		return err
	}
	if m.walSize != 0 {
		return errors.New("the copy's WAL was not emptied")
	}
	m.ready = true
	m.Applied = time.Now()
	return nil
}

// Checkpoint moves the WAL of the database at path into its file, checks
// the result, and leaves no WAL behind. Nothing else may have it open.
func Checkpoint(ctx context.Context, path string) error {
	os.Remove(path + "-shm")
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Opaque: path, RawQuery: q.Encode()}).String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var busy, logFrames, done int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &done); err != nil {
		return fmt.Errorf("checkpoint the copy: %w", err)
	}
	if busy != 0 {
		return errors.New("checkpoint the copy: busy")
	}
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return fmt.Errorf("check the copy: %w", err)
	}
	if check != "ok" {
		return fmt.Errorf("check the copy: %s", check)
	}
	if err := db.Close(); err != nil {
		return err
	}
	os.Remove(path + "-shm")
	return nil
}
