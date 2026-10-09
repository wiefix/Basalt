package store

import (
	"context"
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
