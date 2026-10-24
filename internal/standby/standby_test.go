package standby

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/store"
)

type fakeHub struct{ ours, theirs int64 }

func (h *fakeHub) PanelInfo(context.Context) (agentproto.PanelInfo, error) {
	return agentproto.PanelInfo{}, nil
}
func (h *fakeHub) MarkSuperseded(ours, theirs int64, _ ...any) { h.ours, h.theirs = ours, theirs }

// TestFeederRefusesNewerStandby: a panel refuses a standby that knows a
// newer panel, before telling it anything else, and learns it was
// replaced; otherwise it tells its epoch.
func TestFeederRefusesNewerStandby(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "basalt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetSetting(ctx, "panel_epoch", "2"); err != nil {
		t.Fatal(err)
	}
	f := NewFeeder(db, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	hub := &fakeHub{}
	f.UseHub(hub)
	mux := http.NewServeMux()
	f.Register(func(p string, h http.HandlerFunc) { mux.HandleFunc(p, h) })
	for _, path := range []string{PathReplicate, PathRenew} {
		for _, tc := range []struct {
			epoch string
			code  int
		}{{"3", http.StatusConflict}, {"2", http.StatusUnauthorized}, {"0", http.StatusUnauthorized}, {"", http.StatusUnauthorized}} {
			*hub = fakeHub{}
			req := httptest.NewRequest("POST", path, nil)
			if tc.epoch != "" {
				req.Header.Set(agentproto.HeaderPanelEpoch, tc.epoch)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != tc.code {
				t.Fatalf("%s with epoch %q: %d %s", path, tc.epoch, w.Code, w.Body)
			}
			if tc.code == http.StatusConflict {
				if hub.ours != 2 || hub.theirs != 3 {
					t.Fatalf("not marked superseded: %+v", hub)
				}
			} else if hub.theirs != 0 || w.Header().Get(agentproto.HeaderPanelEpoch) != "2" {
				t.Fatalf("epoch %q: marked %+v, header %q", tc.epoch, hub, w.Header().Get(agentproto.HeaderPanelEpoch))
			}
		}
	}
}

// TestClientPanels: a standby keeps the addresses and epoch the panel
// tells, tries the one that worked first and leaves itself out, and does
// not believe a panel older than it knows.
func TestClientPanels(t *testing.T) {
	dir := t.TempDir()
	c := &Client{dir: dir, agentURL: "https://self:8443", log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: config{PanelURL: "https://a:8443", StandbyID: 4}}
	info := func(epoch int64, panels ...string) []byte {
		b, _ := json.Marshal(agentproto.PanelInfo{Epoch: epoch, Panels: panels})
		return b
	}
	if err := c.setPanels(info(0, "https://a:8443", "https://self:8443", "https://b:8443")); err != nil {
		t.Fatal(err)
	}
	if got := c.panelURLs(); !slices.Equal(got, []string{"https://a:8443", "https://b:8443"}) {
		t.Fatalf("urls: %v", got)
	}
	if err := c.usePanel("https://b:8443"); err != nil {
		t.Fatal(err)
	}
	if err := c.setPanels(info(1, "https://b:8443", "https://self:8443")); err != nil {
		t.Fatal(err)
	}
	// An older panel's addresses are not taken.
	if err := c.setPanels(info(0, "https://a:8443")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{[]byte("{"), info(-1), info(2, "http://x"), info(2, "https://x/path")} {
		if c.setPanels(bad) == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if got := c.panelURLs(); !slices.Equal(got, []string{"https://b:8443"}) {
		t.Fatalf("urls: %v", got)
	}
	var saved config
	b, _ := os.ReadFile(filepath.Join(dir, FileConfig))
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.PanelURL != "https://b:8443" || saved.Epoch != 1 || saved.StandbyID != 4 || !slices.Equal(saved.Panels, []string{"https://b:8443", "https://self:8443"}) {
		t.Fatalf("standby.json: %+v", saved)
	}
	if fi, _ := os.Stat(filepath.Join(dir, FileConfig)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}

	// The panel's epoch, as it answers.
	resp := func(h string) *http.Response {
		r := &http.Response{Header: http.Header{}}
		if h != "" {
			r.Header.Set(agentproto.HeaderPanelEpoch, h)
		}
		return r
	}
	for h, ok := range map[string]bool{"1": true, "2": true, "0": false, "": false, "x": false} {
		if err := c.checkEpoch(resp(h)); (err == nil) != ok {
			t.Fatalf("panel epoch %q: %v", h, err)
		}
	}
	// A standby that knows no epoch follows a panel of any version.
	fresh := &Client{}
	if err := fresh.checkEpoch(resp("")); err != nil {
		t.Fatal(err)
	}
}
