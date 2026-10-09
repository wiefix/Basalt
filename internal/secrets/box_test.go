package secrets

import (
	"bytes"
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
