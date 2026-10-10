package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nested", "basalt.db")
	db, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestOpenAppliesMigrationsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, path := openTemp(t)

	v, err := db.SchemaVersion(ctx)
	if err != nil || v < 1 {
		t.Fatalf("SchemaVersion = %d, %v; want >= 1", v, err)
	}
	db.Close()

	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	v2, err := db2.SchemaVersion(ctx)
	if err != nil || v2 != v {
		t.Fatalf("SchemaVersion after reopen = %d, %v; want %d", v2, err, v)
	}
}

func TestDatabaseFileIsPrivate(t *testing.T) {
	_, path := openTemp(t)
	for _, p := range []string{path, filepath.Dir(path)} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %v; must not be accessible by group or others", p, perm)
		}
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	db, path := openTemp(t)
	if _, err := db.W.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES (999999, 'future.sql')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if db2, err := Open(ctx, path); err == nil {
		db2.Close()
		t.Fatal("Open succeeded on a database with a newer schema; want error")
	}
}

func TestReadPoolIsReadOnly(t *testing.T) {
	db, _ := openTemp(t)
	_, err := db.R.ExecContext(context.Background(),
		`INSERT INTO settings (key, value) VALUES ('x', 'y')`)
	if err == nil {
		t.Fatal("write through read pool succeeded; want error")
	}
}

func TestInstanceIDIsStable(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	a, err := db.InstanceID(ctx)
	if err != nil || a == "" {
		t.Fatalf("InstanceID = %q, %v", a, err)
	}
	b, err := db.InstanceID(ctx)
	if err != nil || b != a {
		t.Fatalf("second InstanceID = %q, %v; want %q", b, err, a)
	}
}

func TestSettingNotFound(t *testing.T) {
	db, _ := openTemp(t)
	if _, err := db.Setting(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; want ErrNotFound", err)
	}
}

// TestComposeMigrationKeepsData rebuilds the apps table under existing
// deployments and routes: nothing may be lost to ON DELETE CASCADE.
func TestComposeMigrationKeepsData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "basalt.db")
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	f.Close()
	w, _ := sql.Open("sqlite", dsn(path, false))
	w.SetMaxOpenConns(1)
	r, _ := sql.Open("sqlite", dsn(path, true))
	db := &DB{W: w, R: r}
	defer db.Close()
	if err := db.migrateTo(ctx, 12); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO servers (id, name) VALUES (1, 's')`,
		`INSERT INTO apps (id, server_id, name, image, port, source, git_url, auto_deploy) VALUES (7, 1, 'web', '', 80, 'git', 'https://x/y.git', 1)`,
		`INSERT INTO deployments (app_id, image, status) VALUES (7, 'basalt/app-7:build-1', 'succeeded')`,
		`INSERT INTO proxy_routes (id, server_id, kind, tls, app_id) VALUES (3, 1, 'reverse_proxy', 'auto', 7)`,
		`INSERT INTO proxy_route_domains (server_id, domain, route_id) VALUES (1, 'web.example.com', 3)`,
	} {
		if _, err := db.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := db.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	a, err := db.AppByID(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if a.Source != "git" || a.Git.URL != "https://x/y.git" || !a.AutoDeploy || a.Port != 80 || len(a.Domains) != 1 {
		t.Fatalf("app after migration = %+v", a)
	}
	if list, _ := db.Deployments(ctx, 7, 10); len(list) != 1 {
		t.Fatal("deployments lost in the migration")
	}
	var fk int
	db.W.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Fatal("foreign keys left off")
	}
	// Cascades work against the rebuilt table.
	if err := db.DeleteApp(ctx, 7); err != nil {
		t.Fatal(err)
	}
	var n int
	db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployments`).Scan(&n)
	if n != 0 {
		t.Fatal("deployments not removed with their app after the rebuild")
	}
}
