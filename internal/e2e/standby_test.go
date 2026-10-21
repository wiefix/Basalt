package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/standby"
)

// TestStandbyPanel: an owner adds a standby panel; it enrolls once with its
// token, gets the master key and a copy of the database that follows the
// panel (also after it reconnects), and stops when the panel removes it.
func TestStandbyPanel(t *testing.T) {
	p := startPanel(t)
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/standbys", map[string]any{"name": "bad name"})
	created := p.call(http.StatusCreated, "POST", "/api/v1/standbys", map[string]any{"name": "spare"})
	enr := created["enrollment"].(map[string]any)
	cmd := enr["install_command"].(string)
	if !strings.Contains(cmd, "| sh -s -- standby --panel "+p.agentURL+" --token "+enr["token"].(string)+" --agent-token ") {
		t.Fatalf("install command: %s", cmd)
	}
	if created["server"].(map[string]any)["name"] != "spare" {
		t.Fatalf("no server for its agent: %v", created["server"])
	}
	p.call(http.StatusConflict, "POST", "/api/v1/standbys", map[string]any{"name": "spare"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	o := standby.EnrollOptions{DataDir: dir, PanelURL: p.agentURL, Token: enr["token"].(string), CASHA256: p.hub.CAFingerprint()}
	if _, err := standby.Enroll(ctx, standby.EnrollOptions{DataDir: t.TempDir(), PanelURL: p.agentURL, Token: "wrong", CASHA256: o.CASHA256}); err == nil {
		t.Fatal("enrolled with a wrong token")
	}
	id, err := standby.Enroll(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := standby.Enroll(ctx, standby.EnrollOptions{DataDir: t.TempDir(), PanelURL: p.agentURL, Token: o.Token, CASHA256: o.CASHA256}); err == nil {
		t.Fatal("the token enrolled twice")
	}
	if _, err := standby.Enroll(ctx, o); err == nil {
		t.Fatal("enrolled twice into one directory")
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	follow := func() (*standby.Client, context.CancelFunc, chan struct{}) {
		c, err := standby.NewClient(dir, log)
		if err != nil {
			t.Fatal(err)
		}
		cctx, stop := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { c.Run(cctx); close(done) }()
		return c, stop, done
	}
	c, stop, done := follow()
	listed := func() map[string]any {
		l := p.call(200, "GET", "/api/v1/standbys", nil)["standbys"].([]any)
		if len(l) == 0 {
			return nil
		}
		return l[0].(map[string]any)
	}
	eventually(t, "the standby's copy is complete", func() bool {
		sb := listed()
		return sb != nil && sb["connected"] == true && sb["ready"] == true && sb["enrolled"] == true
	})
	if int64(listed()["id"].(float64)) != id {
		t.Fatal("another standby")
	}
	// A change on the panel reaches the copy.
	zone := p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]any{"name": "standby.test"})["zone"].(map[string]any)
	after := time.Now()
	eventually(t, "the copy takes the change", func() bool { return c.Status().Applied.After(after) })
	time.Sleep(300 * time.Millisecond)
	stop()
	<-done
	key, _ := os.ReadFile(filepath.Join(p.dataDir, "master.key"))
	if got, _ := os.ReadFile(filepath.Join(dir, "master.key")); !bytes.Equal(got, key) {
		t.Fatal("the master key was not copied")
	}
	copyHas := func(name string) bool {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "basalt.db")+"?_pragma=query_only(ON)")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		db.QueryRow(`SELECT COUNT(*) FROM dns_zones WHERE name = ?`, name).Scan(&n)
		return n == 1
	}
	if !copyHas(zone["name"].(string)) {
		t.Fatal("the copy lacks the new zone")
	}

	// Following again starts from a new snapshot.
	p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]any{"name": "later.test"})
	c, stop, done = follow()
	eventually(t, "the standby follows again", func() bool { st := c.Status(); return st.Ready && st.Connected })
	time.Sleep(300 * time.Millisecond)

	// Removed: the stream ends and the standby stops following.
	p.call(http.StatusNoContent, "DELETE", "/api/v1/standbys/"+strconv.FormatInt(id, 10), nil)
	eventually(t, "the standby learns it was removed", func() bool { return c.Status().Removed })
	stop()
	<-done
	if !copyHas("later.test") {
		t.Fatal("the copy lacks what came before it reconnected")
	}
	if l := p.call(200, "GET", "/api/v1/standbys", nil)["standbys"].([]any); len(l) != 0 {
		t.Fatalf("still listed: %v", l)
	}
	p.call(http.StatusNotFound, "DELETE", "/api/v1/standbys/"+strconv.FormatInt(id, 10), nil)
	var audit string
	p.db.R.QueryRow(`SELECT group_concat(action) FROM (SELECT action FROM audit_log WHERE action LIKE 'standby.%' ORDER BY id)`).Scan(&audit)
	if audit != "standby.created,standby.enrolled,standby.removed" {
		t.Fatalf("audit: %s", audit)
	}
}
