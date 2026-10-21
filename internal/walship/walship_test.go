package walship_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/store"
	"github.com/wiefix/basalt/internal/walship"
)

// feed runs a follower into a mirror until ctx ends; it returns the
// follower's end.
func feed(ctx context.Context, t *testing.T, src *walship.Source, m *walship.Mirror) <-chan error {
	done := make(chan error, 1)
	var mu sync.Mutex
	go func() {
		done <- src.Follow(ctx, func(msg walship.Msg) error {
			mu.Lock()
			defer mu.Unlock()
			if err := m.Apply(context.WithoutCancel(ctx), msg); err != nil {
				t.Errorf("apply %d at %d: %v", msg.Kind, msg.Offset, err)
				return err
			}
			return nil
		})
	}()
	return done
}

// content is the whole of the test table, in order.
func content(t *testing.T, db *sql.DB) string {
	t.Helper()
	var n int
	var sum string
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(group_concat(id || ':' || v, ','), '') FROM (SELECT id, v FROM t ORDER BY id)`).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d %x", n, len(sum)) + sum
}

func openCopy(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=query_only(ON)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestMirror: mirrors fed while the database takes writes, checkpoints
// (also one a long read blocks, after which SQLite restarts the WAL in
// place) and a second follower starting halfway end up with the same
// database, and the copy survives a torn tail.
func TestMirror(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, "basalt.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.W.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	src := walship.NewSource(path, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go src.Run(ctx)

	var stop sync.WaitGroup
	writing, endWrites := context.WithCancel(ctx)
	write := func(seed uint64) {
		defer stop.Done()
		r := rand.New(rand.NewPCG(seed, 1))
		for writing.Err() == nil {
			switch r.IntN(4) {
			case 0:
				db.W.Exec(`UPDATE t SET v = ? WHERE id = ?`, strings.Repeat("u", r.IntN(3000)), r.IntN(500))
			case 1:
				db.W.Exec(`DELETE FROM t WHERE id = ?`, r.IntN(500))
			default:
				db.W.Exec(`INSERT INTO t (v) VALUES (?)`, strings.Repeat("x", r.IntN(5000)))
			}
		}
	}
	stop.Add(2)
	go write(1)
	go write(2)

	m1 := walship.NewMirror(filepath.Join(dir, "one.db"))
	ctx1, stop1 := context.WithCancel(ctx)
	done1 := feed(ctx1, t, src, m1)
	time.Sleep(300 * time.Millisecond)
	if err := src.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	// A read holds the WAL: the checkpoint cannot truncate it.
	tx, err := db.R.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	tx.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n)
	if err := src.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()

	m2 := walship.NewMirror(filepath.Join(dir, "two.db"))
	ctx2, stop2 := context.WithCancel(ctx)
	done2 := feed(ctx2, t, src, m2)
	time.Sleep(300 * time.Millisecond)
	if err := src.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	endWrites()
	stop.Wait()
	db.W.Exec(`INSERT INTO t (v) VALUES ('last')`)
	want := content(t, db.R)

	// Both mirrors catch up.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var walSize int64
		if fi, err := os.Stat(path + "-wal"); err == nil {
			walSize = fi.Size()
		}
		a, _ := os.Stat(filepath.Join(dir, "one.db-wal"))
		b, _ := os.Stat(filepath.Join(dir, "two.db-wal"))
		if a != nil && b != nil && a.Size() == walSize && b.Size() == walSize {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop1()
	stop2()
	<-done1
	<-done2
	m1.Close()
	m2.Close()
	for _, name := range []string{"one.db", "two.db"} {
		p := filepath.Join(dir, name)
		if name == "two.db" {
			// A frame cut short at the end (the standby stopped mid-write).
			f, _ := os.OpenFile(p+"-wal", os.O_WRONLY|os.O_APPEND, 0)
			f.Write([]byte("torn frame"))
			f.Close()
		}
		if got := content(t, openCopy(t, p)); got != want {
			t.Errorf("%s differs: %.60s… want %.60s…", name, got, want)
		}
		if err := walship.Checkpoint(ctx, p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := content(t, openCopy(t, p)); got != want {
			t.Errorf("%s differs after its checkpoint", name)
		}
	}
	if want == "0 0" {
		t.Fatal("nothing was written")
	}
	t.Logf("rows: %s", want[:strings.IndexByte(want, ' ')])
}

// TestMirrorOrder: a change that does not follow what the copy holds is
// refused.
func TestMirrorOrder(t *testing.T) {
	ctx := context.Background()
	m := walship.NewMirror(filepath.Join(t.TempDir(), "x.db"))
	defer m.Close()
	for _, msg := range []walship.Msg{
		{Kind: walship.KindFrames, Data: []byte("x")},
		{Kind: walship.KindSnapshotEnd},
		{Kind: walship.KindSnapshotWAL, Data: []byte("x")},
		{Kind: walship.KindSnapshotDB, Offset: 5, Data: []byte("x")},
		{Kind: 99},
	} {
		if err := m.Apply(ctx, msg); err == nil {
			t.Errorf("%+v accepted", msg)
		}
	}
	if m.Ready() {
		t.Fatal("ready without a snapshot")
	}
}
