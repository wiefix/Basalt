package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	b, _ := NewBox(bytes.Repeat([]byte{7}, KeySize))
	ct := b.Seal([]byte("hello"), []byte("ad"))
	pt, err := b.Open(ct, []byte("ad"))
	if err != nil || string(pt) != "hello" {
		t.Fatalf("Open = %q, %v", pt, err)
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	b, _ := NewBox(bytes.Repeat([]byte{7}, KeySize))
	ct := b.Seal([]byte("hello"), []byte("ad"))

	if _, err := b.Open(ct, []byte("other")); err == nil {
		t.Error("wrong associated data accepted")
	}
	flipped := bytes.Clone(ct)
	flipped[len(flipped)-1] ^= 1
	if _, err := b.Open(flipped, []byte("ad")); err == nil {
		t.Error("modified ciphertext accepted")
	}
	other, _ := NewBox(bytes.Repeat([]byte{8}, KeySize))
	if _, err := other.Open(ct, []byte("ad")); err == nil {
		t.Error("wrong key accepted")
	}
	if _, err := b.Open(ct[:5], []byte("ad")); err == nil {
		t.Error("truncated ciphertext accepted")
	}
}

func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	a, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v; want 0600", info.Mode().Perm())
	}
	b, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(a.Seal([]byte("x"), nil), nil); err != nil {
		t.Fatal("reloaded key differs from generated key")
	}

	os.Chmod(path, 0o644)
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("world-readable key accepted")
	}
}

func TestRecoveryFile(t *testing.T) {
	key := make([]byte, KeySize)
	key[0] = 7
	b, _ := NewBox(key)
	if _, err := b.WrapKey("short"); err == nil {
		t.Fatal("short passphrase accepted")
	}
	f, err := b.WrapKey("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(f, key) {
		t.Fatal("key in clear")
	}
	got, err := UnwrapKey(f, "correct horse battery staple")
	if err != nil || !b.SameKey(got) {
		t.Fatalf("unwrap: %v", err)
	}
	if _, err := UnwrapKey(f, "wrong horse battery staple"); !errors.Is(err, ErrRecoveryPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	// A crafted file cannot ask for absurd KDF costs.
	bomb := bytes.Replace(f, []byte(`"memory_kib": 65536`), []byte(`"memory_kib": 4000000000`), 1)
	if _, err := UnwrapKey(bomb, "correct horse battery staple"); err == nil || errors.Is(err, ErrRecoveryPassphrase) {
		t.Fatalf("memory bomb: %v", err)
	}
	if _, err := UnwrapKey([]byte("{}"), "x"); err == nil {
		t.Fatal("empty file accepted")
	}
}

func TestDeriveKey(t *testing.T) {
	a, _ := NewBox(bytes.Repeat([]byte{1}, KeySize))
	b, _ := NewBox(bytes.Repeat([]byte{2}, KeySize))
	k := a.DeriveKey("audit-log")
	if len(k) != 32 || bytes.Equal(k, a.DeriveKey("other")) || bytes.Equal(k, b.DeriveKey("audit-log")) ||
		!bytes.Equal(k, a.DeriveKey("audit-log")) || bytes.Contains(k, a.key[:8]) {
		t.Fatal("derived keys must depend on the label and the master key only")
	}
}
