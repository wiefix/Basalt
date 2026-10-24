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

// TestDecide: a standby takes over only if most of all its witnesses
// confirm the panel has been gone long enough, none reaches it and none
// knows a newer panel.
func TestDecide(t *testing.T) {
	ws := func(n int) []Witness {
		out := make([]Witness, n)
		for i := range out {
			out[i] = Witness{ServerID: int64(i + 1), Addr: "192.0.2.1:8444"}
		}
		return out
	}
	gone := func(id int64, s int64) witnessAnswer {
		return witnessAnswer{ServerID: id, Status: agentproto.PanelStatus{For: s}}
	}
	unreachable := witnessAnswer{ServerID: 9, Err: io.EOF}
	for _, tc := range []struct {
		name    string
		n       int
		answers []witnessAnswer
		epoch   int64
		want    bool
	}{
		{"one of one", 1, []witnessAnswer{gone(1, 300)}, 0, true},
		{"just lost it", 1, []witnessAnswer{gone(1, 100)}, 0, false},
		{"half the delay is enough", 1, []witnessAnswer{gone(1, 150)}, 0, true},
		{"two of three", 3, []witnessAnswer{gone(1, 300), gone(2, 300), unreachable}, 0, true},
		{"one of two is not most", 2, []witnessAnswer{gone(1, 300), unreachable}, 0, false},
		{"nobody answers (the standby is cut off)", 3, []witnessAnswer{unreachable, unreachable, unreachable}, 0, false},
		{"one reaches the panel", 3, []witnessAnswer{gone(1, 300), gone(2, 300), {ServerID: 3, Status: agentproto.PanelStatus{Connected: true, For: 5}}}, 0, false},
		{"one knows a newer panel", 2, []witnessAnswer{gone(1, 300), {ServerID: 2, Status: agentproto.PanelStatus{For: 300, Epoch: 2}}}, 1, false},
		{"same epoch", 1, []witnessAnswer{{ServerID: 1, Status: agentproto.PanelStatus{For: 300, Epoch: 1}}}, 1, true},
	} {
		f := &Failover{Auto: true, After: 300, Witnesses: ws(tc.n)}
		if got, why := decide(f, tc.answers, tc.epoch); got != tc.want {
			t.Errorf("%s: %v (%s)", tc.name, got, why)
		}
	}

	for _, bad := range []Failover{
		{Auto: true, After: 0, Witnesses: ws(1)},
		{Auto: true, After: 300},
		{Auto: true, After: 300, Witnesses: []Witness{{ServerID: 0, Addr: "a:1"}}},
		{Auto: true, After: 300, Witnesses: []Witness{{ServerID: 1, Addr: "no-port"}}},
		{Auto: true, After: 300, Witnesses: ws(MaxWitnesses + 1)},
	} {
		if bad.Validate() == nil {
			t.Errorf("valid: %+v", bad)
		}
	}
	if (Failover{}).Validate() != nil {
		t.Error("off is invalid")
	}

	// The setting is kept in standby.json; off forgets it.
	c := &Client{dir: t.TempDir(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	on, _ := json.Marshal(Failover{Auto: true, After: 60, Witnesses: ws(2)})
	if err := c.setFailover(on); err != nil || c.cfg.Failover == nil || c.Status().AfterS != 60 {
		t.Fatalf("on: %v %+v", err, c.cfg.Failover)
	}
	if err := c.setFailover([]byte(`{"auto":true,"after_s":60}`)); err == nil {
		t.Fatal("accepted no witnesses")
	}
	if err := c.setFailover([]byte(`{"auto":false}`)); err != nil || c.cfg.Failover != nil || c.Status().AutoFailover {
		t.Fatalf("off: %v", err)
	}
}
