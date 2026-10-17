package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/release"
	"github.com/wiefix/basalt/internal/server"
	"github.com/wiefix/basalt/internal/sshsig"
)

// TestUpdates: the panel finds a signed release, downloads the binary and
// has the agent check, stage and swap it in, then restart.
func TestUpdates(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	key := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))

	name := release.BinaryName(runtime.GOARCH)
	newBin := []byte("#!/bin/sh\necho 'basalt v1.1.0 (commit x, built y)'\n")
	files := map[string][]byte{name: newBin, release.VersionFile: []byte("v1.1.0\n")}
	var sums strings.Builder
	for n, b := range files {
		h := sha256.Sum256(b)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h[:]), n)
	}
	sig, _ := sshsig.Sign([]byte(sums.String()), signer, sshsig.ReleaseNamespace)
	var rmu sync.Mutex
	served := map[string][]byte{
		"/latest/download/" + release.SumsFile:    []byte(sums.String()),
		"/latest/download/" + release.SigFile:     sig,
		"/latest/download/" + release.VersionFile: files[release.VersionFile],
		"/download/v1.1.0/" + name:                newBin,
	}
	releases := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rmu.Lock()
		b, ok := served[r.URL.Path]
		rmu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer releases.Close()

	p := startPanelWith(t, server.Options{ReleaseURL: releases.URL, ReleaseKey: key, ReleaseAllowPrivate: true, Version: "v1.0.0"})
	exe := filepath.Join(t.TempDir(), "basalt")
	os.WriteFile(exe, []byte("old"), 0o755)
	var mu sync.Mutex
	var restarted []string
	h := startHostWith(t, p, "", func(c *agent.Config) {
		c.UpdateExe, c.ReleaseKey = exe, key
		c.Restart = func(units []string) error {
			mu.Lock()
			defer mu.Unlock()
			restarted = units
			return nil
		}
	})

	u := p.call(http.StatusOK, "GET", "/api/v1/updates", nil)
	srv := u["servers"].([]any)[0].(map[string]any)
	if u["enabled"] != true || u["current"] != "v1.0.0" || u["latest"] != "" || srv["updatable"] != true || jsonID(srv["id"]) != h.id {
		t.Fatalf("updates = %v", u)
	}
	p.call(http.StatusConflict, "POST", "/api/v1/updates/apply", map[string]any{})
	u = p.call(http.StatusOK, "POST", "/api/v1/updates/check", map[string]any{})
	if u["latest"] != "v1.1.0" || u["available"] != true || u["error"] != "" {
		t.Fatalf("after check = %v", u)
	}

	p.call(http.StatusAccepted, "POST", "/api/v1/updates/apply", map[string]any{})
	var run map[string]any
	eventually(t, "update finished", func() bool {
		run, _ = p.call(http.StatusOK, "GET", "/api/v1/updates", nil)["run"].(map[string]any)
		return run != nil && (run["state"] == "done" || run["state"] == "failed")
	})
	if s := run["servers"].([]any)[0].(map[string]any); run["state"] != "done" || s["status"] != "restarting" {
		t.Fatalf("run = %v", run)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(newBin) {
		t.Fatalf("binary on the server: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(p.dataDir, "releases", "v1.1.0", name)); string(b) != string(newBin) {
		t.Fatal("release binary not kept on the panel")
	}
	eventually(t, "services restarted", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(restarted) > 0
	})

	// A changed checksum list (not signed) is refused; the release found
	// before stays.
	rmu.Lock()
	served["/latest/download/"+release.SumsFile] = []byte(strings.Repeat("0", 64) + "  " + name + "\n")
	rmu.Unlock()
	u = p.call(http.StatusOK, "POST", "/api/v1/updates/check", map[string]any{})
	if !strings.Contains(u["error"].(string), "not signed") || u["latest"] != "v1.1.0" {
		t.Fatalf("tampered release = %v", u)
	}

	// Members cannot see or start updates.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{"email": "m@example.com", "name": "M", "password": "a long password 1", "role": "member"})
	m := *p
	m.hc = &http.Client{Jar: newJar()}
	m.call(http.StatusOK, "POST", "/api/v1/auth/login", map[string]string{"email": "m@example.com", "password": "a long password 1"})
	m.call(http.StatusForbidden, "GET", "/api/v1/updates", nil)
	m.call(http.StatusForbidden, "POST", "/api/v1/updates/apply", map[string]any{})

	var started, finished int
	p.db.R.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'update.started'`).Scan(&started)
	p.db.R.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'update.finished'`).Scan(&finished)
	if started != 1 || finished != 1 {
		t.Fatalf("audit: %d started, %d finished", started, finished)
	}
}
