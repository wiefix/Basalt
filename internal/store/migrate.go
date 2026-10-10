package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads migrations named NNNN_description.sql, sorted by version.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		prefix, _, ok := strings.Cut(name, "_")
		if !ok || !strings.HasSuffix(name, ".sql") {
			return nil, fmt.Errorf("migration %q: want NNNN_description.sql", name)
		}
		v, err := strconv.Atoi(prefix)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %q: invalid version prefix", name)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", prev, name, v)
		}
		seen[v] = name
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		out = append(out, migration{version: v, name: name, sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func (db *DB) migrate(ctx context.Context) error { return db.migrateTo(ctx, 0) }

// migrateTo applies pending migrations up to version upTo (0: all).
func (db *DB) migrateTo(ctx context.Context, upTo int) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	if _, err := db.W.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT    NOT NULL,
			applied_at INTEGER NOT NULL DEFAULT (unixepoch())
		) STRICT`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	if err := db.W.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if latest := migrations[len(migrations)-1].version; current > latest {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d); refusing to start", current, latest)
	}

	for _, m := range migrations {
		if m.version <= current || upTo > 0 && m.version > upTo {
			continue
		}
		if err := db.apply(ctx, m); err != nil {
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
	}
	return nil
}

// noForeignKeys marks a migration that rebuilds a table: foreign keys are
// switched off around it (SQLite ignores the pragma inside a transaction)
// and checked before it commits.
const noForeignKeys = "-- basalt:foreign-keys-off"

func (db *DB) apply(ctx context.Context, m migration) error {
	// The write pool has one connection; pin it so the pragma and the
	// transaction share it.
	conn, err := db.W.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	fkOff := strings.HasPrefix(m.sql, noForeignKeys)
	if fkOff {
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return err
		}
		defer conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if fkOff {
		rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			return err
		}
		broken := rows.Next()
		rows.Close()
		if broken {
			return fmt.Errorf("foreign key violations after migration")
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.version, m.name); err != nil {
		return err
	}
	return tx.Commit()
}

// SchemaVersion returns the highest applied migration version.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := db.R.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	return int(v.Int64), err
}
