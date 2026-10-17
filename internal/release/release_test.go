package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/sshsig"
)

func TestVerifySums(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	key := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	h := strings.Repeat("ab", 32)
	sums := []byte(h + "  basalt-linux-amd64\n" + strings.ToUpper(h) + " *version.txt\n")
	sig, _ := sshsig.Sign(sums, signer, sshsig.ReleaseNamespace)

	got, err := VerifySums(sums, sig, key)
	if err != nil || got["basalt-linux-amd64"] != h || got["version.txt"] != h || len(got) != 2 {
		t.Fatalf("sums = %v, %v", got, err)
	}
	if _, err := VerifySums(append(sums, 'x'), sig, key); err == nil {
		t.Fatal("changed checksums accepted")
	}
	catalogSig, _ := sshsig.Sign(sums, signer, "basalt-catalog")
	if _, err := VerifySums(sums, catalogSig, key); err == nil {
		t.Fatal("a catalog signature passed for a release")
	}
	if _, err := VerifySums(sums, sig, sshsig.ReleaseKey); err == nil {
		t.Fatal("another key's signature accepted")
	}
	for _, bad := range []string{"", "zz  file\n", h + "  ../evil\n", h[:10] + "  short\n"} {
		s, _ := sshsig.Sign([]byte(bad), signer, sshsig.ReleaseNamespace)
		if _, err := VerifySums([]byte(bad), s, key); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestVersions(t *testing.T) {
	for _, c := range []struct {
		a, b  string
		newer bool
	}{
		{"v0.2.0", "v0.1.9", true},
		{"v0.10.0", "v0.9.0", true},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.0", "v1.0.0-rc.1", true},
		{"v1.0.0-rc.2", "v1.0.0-rc.1", true},
		{"v1.0.0-rc.1", "v1.0.0", false},
		{"v0.1.0", "dev", true},
		{"dev", "v0.1.0", false},
		{"", "dev", false},
	} {
		if got := Newer(c.a, c.b); got != c.newer {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
	for _, v := range []string{"1.0.0", "v1.0", "v1.0.0 ", "v1.0.0-", "v1.0.0+build", "v1.0.0-$(x)"} {
		if ValidVersion(v) {
			t.Errorf("%q is valid", v)
		}
	}
}
