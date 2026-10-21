// Package store owns the control-plane SQLite database.
//
// SQLite allows a single writer at a time, so the database is opened twice:
// a one-connection write pool (transactions start IMMEDIATE to avoid
// SQLITE_BUSY upgrades) and a multi-connection read-only pool.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

type DB struct {
	// W is the single-connection write pool. Use it for anything that mutates.
	W *sql.DB
	// R is the read-only pool.
	R *sql.DB
	// auditKey keys the audit log's hash chain (see SetAuditKey).
	auditKey atomic.Pointer[[]byte]
}

// Open opens (creating if needed) the database file at path and applies
// pending migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	// Create the file ourselves so it is never world-readable.
	// SQLite gives the -wal and -shm files the same mode as the main file.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	f.Close()

	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, fmt.Errorf("open write pool: %w", err)
	}
	w.SetMaxOpenConns(1)

	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("open read pool: %w", err)
	}
	r.SetMaxOpenConns(max(4, runtime.NumCPU()))

	db := &DB{W: w, R: r}
	if err := w.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if err := db.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	// Only walship.Source checkpoints the WAL (it keeps standby copies of
	// the database byte for byte, which needs to know when that happens).
	q.Add("_pragma", "wal_autocheckpoint(0)")
	if readOnly {
		q.Add("_pragma", "query_only(ON)")
	} else {
		q.Set("_txlock", "immediate")
	}
	return (&url.URL{Scheme: "file", Opaque: path, RawQuery: q.Encode()}).String()
}

// WithWriter runs fn on the write pool's only connection: nothing commits
// until it returns.
func (db *DB) WithWriter(ctx context.Context, fn func(ctx context.Context, conn *sql.Conn) error) error {
	conn, err := db.W.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(ctx, conn)
}

func (db *DB) Ping(ctx context.Context) error {
	return db.R.PingContext(ctx)
}

func (db *DB) Close() error {
	rerr := db.R.Close()
	werr := db.W.Close()
	if werr != nil {
		return werr
	}
	return rerr
}
