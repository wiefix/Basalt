package catalog

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func newSigner(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s, string(ssh.MarshalAuthorizedKey(s.PublicKey()))
}

func TestSSHSig(t *testing.T) {
	signer, pub := newSigner(t)
	_, other := newSigner(t)
	msg := []byte("catalog index")
	sig, err := SignSSHSig(msg, signer, Namespace)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySSHSig(msg, sig, pub, Namespace); err != nil {
		t.Fatal(err)
	}
	if VerifySSHSig([]byte("catalog indeX"), sig, pub, Namespace) == nil {
		t.Error("changed message accepted")
	}
	if VerifySSHSig(msg, sig, other, Namespace) == nil {
		t.Error("another key accepted")
	}
	if VerifySSHSig(msg, sig, pub, "basalt-release") == nil {
		t.Error("another namespace accepted")
	}
	if VerifySSHSig(msg, []byte("garbage"), pub, Namespace) == nil || VerifySSHSig(msg, sig, "not a key", Namespace) == nil {
		t.Error("garbage accepted")
	}
	// A signature blob with a flipped byte of the signature.
	block, _ := pem.Decode(sig)
	block.Bytes[len(block.Bytes)-3] ^= 1
	if VerifySSHSig(msg, pem.EncodeToMemory(block), pub, Namespace) == nil {
		t.Error("damaged signature accepted")
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(BasaltKey)); err != nil {
		t.Fatalf("BasaltKey: %v", err)
	}
}

// TestSSHSigOpenSSH checks the format against ssh-keygen both ways.
func TestSSHSigOpenSSH(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "key")
	if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-C", "test", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	pub, _ := os.ReadFile(key + ".pub")
	msgFile := filepath.Join(dir, "catalog.json")
	msg := []byte(`{"version":1}` + "\n")
	os.WriteFile(msgFile, msg, 0o600)
	if out, err := exec.Command(keygen, "-Y", "sign", "-f", key, "-n", Namespace, msgFile).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	sig, _ := os.ReadFile(msgFile + ".sig")
	if err := VerifySSHSig(msg, sig, string(pub), Namespace); err != nil {
		t.Fatalf("ssh-keygen signature: %v", err)
	}
	// And ssh-keygen accepts ours.
	raw, _ := os.ReadFile(key)
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	ours, _ := SignSSHSig(msg, signer, Namespace)
	os.WriteFile(msgFile+".sig", ours, 0o600)
	allowed := filepath.Join(dir, "allowed")
	os.WriteFile(allowed, []byte("test "+string(pub)), 0o600)
	cmd := exec.Command(keygen, "-Y", "verify", "-f", allowed, "-I", "test", "-n", Namespace, "-s", msgFile+".sig")
	cmd.Stdin = bytes.NewReader(msg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen rejects ours: %v %s", err, out)
	}
}

func TestIndex(t *testing.T) {
	ctx := context.Background()
	signer, pub := newSigner(t)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	data, err := BuildIndex(ctx, builtinFS, "templates", now)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := SignSSHSig(data, signer, Namespace)
	r, err := ParseIndex(ctx, data, sig, pub)
	if err != nil {
		t.Fatal(err)
	}
	builtin, _ := Builtin()
	if len(r.Templates) != len(builtin) || len(r.Skipped) != 0 || !r.Generated.Equal(now) {
		t.Fatalf("%d templates, skipped %v, %v", len(r.Templates), r.Skipped, r.Generated)
	}
	if _, err := ParseIndex(ctx, append(data, ' '), sig, pub); err == nil {
		t.Fatal("changed index accepted")
	}
	sign := func(idx Index) ([]byte, []byte) {
		b, _ := json.Marshal(idx)
		s, _ := SignSSHSig(b, signer, Namespace)
		return b, s
	}
	var idx Index
	json.Unmarshal(data, &idx)
	// A template this panel cannot read is skipped; the rest is kept.
	bad := idx
	bad.Templates = append([]IndexEntry{{Template: "id: future\nnew_field: 1\n", Compose: "services: {}"}}, idx.Templates...)
	bd, bs := sign(bad)
	r, err = ParseIndex(ctx, bd, bs, pub)
	if err != nil || len(r.Skipped) != 1 || len(r.Templates) != len(builtin) {
		t.Fatalf("%v %v", r, err)
	}
	for name, change := range map[string]func(*Index){
		"version":   func(i *Index) { i.Version = 2 },
		"no date":   func(i *Index) { i.Generated = time.Time{} },
		"duplicate": func(i *Index) { i.Templates = append(i.Templates, i.Templates[0]) },
	} {
		c := idx
		c.Templates = append([]IndexEntry(nil), idx.Templates...)
		change(&c)
		cd, cs := sign(c)
		if _, err := ParseIndex(ctx, cd, cs, pub); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// Merge: a higher remote revision wins, a lower one does not.
	newer := *Find(r.Templates, "wordpress")
	newer.Revision = 99
	older := *Find(r.Templates, "ghost")
	older.Revision = 0
	extra := *Find(r.Templates, "n8n")
	extra.ID, extra.Name = "zzz", "Zzz"
	m := Merge(builtin, []*Template{&newer, &older, &extra}, "catalog.example.com")
	if len(m) != len(builtin)+1 || Find(m, "wordpress").Revision != 99 || Find(m, "wordpress").Source != "catalog.example.com" ||
		Find(m, "ghost").Source != "builtin" || Find(m, "zzz").Source != "catalog.example.com" {
		t.Fatalf("merge: %+v", m)
	}
	if builtin[0].Source != "" {
		t.Fatal("merge changed the built-in templates")
	}
}
