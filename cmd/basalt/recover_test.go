package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agenthub"
	"github.com/wiefix/basalt/internal/secrets"
	"github.com/wiefix/basalt/internal/store"
)

func TestRecover(t *testing.T) {
	ctx := context.Background()
	const phrase = "violet tractor sings at dawn"

	// A panel with a CA (its key encrypted with the master key) and its
	// backup as restic would restore it: nested folders.
	orig := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(orig, "basalt.db"))
	if err != nil {
		t.Fatal(err)
	}
	box, _ := secrets.LoadOrCreateKey(filepath.Join(orig, "master.key"))
	if _, err := agenthub.LoadOrCreateCA(ctx, db, box, "x", time.Now()); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "var", "lib", "basalt", "server", "panel-backup")
	os.MkdirAll(restored, 0o700)
	if err := db.SnapshotTo(ctx, filepath.Join(restored, "basalt.db")); err != nil {
		t.Fatal(err)
	}
	db.Close()
	wrapped, _ := box.WrapKey(phrase)
	os.WriteFile(filepath.Join(restored, "master-key.json"), wrapped, 0o600)

	recover := func(dataDir, pass string, extra ...string) (string, error) {
		t.Setenv("BASALT_RECOVERY_PASSPHRASE", pass)
		var out, errOut bytes.Buffer
		err := runRecover(ctx, append([]string{"--data-dir", dataDir, "--from", filepath.Dir(filepath.Dir(restored))}, extra...),
			os.Stdin, &out, &errOut)
		return out.String(), err
	}
	fresh := t.TempDir()
	if _, err := recover(fresh, "wrong passphrase entirely"); err == nil || !mentions(err, "wrong recovery passphrase") {
		t.Fatalf("wrong passphrase: %v", err)
	}
	out, err := recover(fresh, phrase)
	if err != nil || !strings.Contains(out, "Recovered the panel") {
		t.Fatalf("recover: %v %s", err, out)
	}
	origKey, _ := os.ReadFile(filepath.Join(orig, "master.key"))
	key, _ := os.ReadFile(filepath.Join(fresh, "master.key"))
	if !bytes.Equal(origKey, key) {
		t.Fatal("master key not restored")
	}
	if fi, _ := os.Stat(filepath.Join(fresh, "master.key")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode())
	}
	// An existing panel is replaced only on request, and kept.
	if _, err := recover(fresh, phrase); err == nil || !mentions(err, "--force") {
		t.Fatalf("over an existing database: %v", err)
	}
	if _, err := recover(fresh, phrase, "--force"); err != nil {
		t.Fatal(err)
	}
	if baks, _ := filepath.Glob(filepath.Join(fresh, "basalt.db.bak-*")); len(baks) != 1 {
		t.Fatalf("backups of the replaced files: %v", baks)
	}
}

func mentions(err error, s string) bool { return strings.Contains(err.Error(), s) }
