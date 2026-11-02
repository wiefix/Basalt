package privhelper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// socketPath is short enough for a unix socket (t.TempDir is not, on macOS).
func socketPath(t *testing.T) string {
	dir, err := os.MkdirTemp("", "ph")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "h.sock")
}

// fake answers like a helper, without changing the host.
func fake(calls *atomic.Int32) func(context.Context, string, json.RawMessage) (any, error) {
	return func(_ context.Context, msgType string, payload json.RawMessage) (any, error) {
		calls.Add(1)
		var req SiteUserRequest
		json.Unmarshal(payload, &req)
		switch msgType {
		case MsgSiteUserEnsure:
			return SiteUser{UID: int(req.SiteID) * 10, GID: int(req.SiteID) * 10}, nil
		case MsgSiteUserDelete:
			return nil, errors.New("userdel: user is busy")
		}
		return struct{}{}, nil
	}
}

func serve(t *testing.T, path string, uid int, calls *atomic.Int32) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// Closed when Serve returns: stopping more than once is fine.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := Serve(ctx, path, uid, fake(calls), quiet); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if fi, err := os.Lstat(path); err == nil && fi.Mode().Perm() == 0o600 {
			return func() {
				cancel()
				<-done
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the helper did not listen")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClientServer(t *testing.T) {
	path := socketPath(t)
	var calls atomic.Int32
	serve(t, path, os.Geteuid(), &calls)
	c := NewClient(path)
	defer c.Close()
	ctx := context.Background()

	uid, gid, err := c.EnsureSiteUser(ctx, 7)
	if err != nil || uid != 70 || gid != 70 {
		t.Fatalf("ensure: %d %d %v", uid, gid, err)
	}
	if err := c.HostIdentity(ctx); err != nil {
		t.Fatal(err)
	}
	// The helper's own words reach the agent, not wrapped as remote.
	if err := c.DeleteSiteUser(ctx, 7); err == nil || err.Error() != "userdel: user is busy" {
		t.Fatalf("delete: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("%d calls, want 3 (a refused request is not repeated)", calls.Load())
	}
}

func TestClientReconnects(t *testing.T) {
	path := socketPath(t)
	var calls atomic.Int32
	stop := serve(t, path, os.Geteuid(), &calls)
	c := NewClient(path)
	defer c.Close()
	if _, _, err := c.EnsureSiteUser(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	// The helper restarts (an update): the next call finds the connection
	// broken, connects again and goes through.
	stop()
	serve(t, path, os.Geteuid(), &calls)
	if uid, _, err := c.EnsureSiteUser(context.Background(), 2); err != nil || uid != 20 {
		t.Fatalf("after restart: %d %v", uid, err)
	}
}

func TestServerRefusesOtherUsers(t *testing.T) {
	path := socketPath(t)
	var calls atomic.Int32
	// The agent is someone else: this process gets no answers.
	serve(t, path, os.Geteuid()+1, &calls)
	c := NewClient(path)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.EnsureSiteUser(ctx, 1); err == nil {
		t.Fatal("a process of another user got an answer")
	}
	if calls.Load() != 0 {
		t.Fatalf("%d requests served", calls.Load())
	}
}

func TestServeSocketFile(t *testing.T) {
	path := socketPath(t)
	// Not a socket: never removed.
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), path, os.Geteuid(), fake(new(atomic.Int32)), quiet); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("serve over a file: %v", err)
	}
	os.Remove(path)
	// A socket left by a helper that was killed is replaced.
	var calls atomic.Int32
	stop := serve(t, path, os.Geteuid(), &calls)
	stop()
	serve(t, path, os.Geteuid(), &calls)
	if _, _, err := NewClient(path).EnsureSiteUser(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
}

func TestHelperChecksRequests(t *testing.T) {
	if _, err := New(Config{SitesRoot: "sites"}, quiet); err == nil {
		t.Error("relative sites root accepted")
	}
	if _, err := New(Config{SitesRoot: "/srv/sites", WWWGroup: "www-data"}, quiet); err == nil {
		t.Error("a group outside basalt- accepted")
	}
	h, err := New(Config{SitesRoot: "/srv/sites"}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []string{`{"site_id":0}`, `{"site_id":-3}`} {
		for _, msg := range []string{MsgSiteUserEnsure, MsgSiteUserDelete} {
			if _, err := h.Handle(ctx, msg, json.RawMessage(id)); !errors.Is(err, errSiteID) {
				t.Errorf("%s %s: %v", msg, id, err)
			}
		}
	}
	if _, err := h.Handle(ctx, "run", json.RawMessage(`{"cmd":"id"}`)); err == nil || !strings.Contains(err.Error(), "unknown request") {
		t.Errorf("unknown request: %v", err)
	}
}
