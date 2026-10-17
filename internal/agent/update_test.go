package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/release"
	"github.com/wiefix/basalt/internal/sshsig"
)

// TestUpdater: an update stages only a binary matching the signed
// checksums that runs and reports the announced version, then swaps it in
// and restarts the services.
func TestUpdater(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	key := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))

	script := func(v string) []byte { return []byte("#!/bin/sh\necho 'basalt " + v + " (commit x, built y)'\n") }
	served := map[string][]byte{}
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := served[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer panel.Close()
	name := release.BinaryName(runtime.GOARCH)
	sign := func(files map[string][]byte) agentproto.UpdatePrepare {
		var sums strings.Builder
		for n, b := range files {
			h := sha256.Sum256(b)
			fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h[:]), n)
		}
		sig, _ := sshsig.Sign([]byte(sums.String()), signer, sshsig.ReleaseNamespace)
		return agentproto.UpdatePrepare{Sums: []byte(sums.String()), Sig: sig}
	}

	exe := filepath.Join(t.TempDir(), "basalt")
	os.WriteFile(exe, script("v1.0.0"), 0o755)
	var mu sync.Mutex
	var restarted []string
	u := newUpdater(Config{ReleaseKey: key, UpdateExe: exe, Restart: func(units []string) error {
		mu.Lock()
		defer mu.Unlock()
		restarted = units
		return nil
	}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !u.updatable() {
		t.Fatal("not updatable")
	}
	ctx := context.Background()
	prepare := func(v string, req agentproto.UpdatePrepare) error {
		req.Version = v
		return u.prepare(ctx, panel.Client(), panel.URL, req)
	}

	newBin := script("v1.1.0")
	served[agentproto.PathRelease+"v1.1.0/"+name] = newBin
	good := sign(map[string][]byte{name: newBin, release.VersionFile: []byte("v1.1.0\n")})

	// Signed by another key.
	other := good
	other.Sig, _ = sshsig.Sign(good.Sums, mustSigner(t), sshsig.ReleaseNamespace)
	if err := prepare("v1.1.0", other); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("foreign signature: %v", err)
	}
	// The panel serves something else than what was signed.
	served[agentproto.PathRelease+"v1.2.0/"+name] = script("v1.2.0")
	if err := prepare("v1.2.0", sign(map[string][]byte{name: []byte("other"), release.VersionFile: nil})); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatching binary: %v", err)
	}
	// A signed binary that is not the announced version.
	if err := prepare("v1.3.0", good); err == nil {
		t.Fatal("binary of another version staged")
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatal("a refused binary was left staged")
	}
	if err := u.activate(agentproto.UpdateActivate{Version: "v1.1.0"}); err == nil {
		t.Fatal("activated without preparing")
	}

	if err := prepare("v1.1.0", good); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(script("v1.0.0")) {
		t.Fatal("preparing replaced the running binary")
	}
	if err := u.activate(agentproto.UpdateActivate{Version: "v1.2.0"}); err == nil {
		t.Fatal("activated another version")
	}
	if err := u.activate(agentproto.UpdateActivate{Version: "v1.1.0"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(newBin) {
		t.Fatal("binary not replaced")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := strings.Join(restarted, " ")
		mu.Unlock()
		if got == "basalt-proxy.service basalt-agent.service" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restarted %q", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mustSigner(t *testing.T) ssh.Signer {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
