package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/forge/forgetest"
)

// TestForgeSources: a GitLab or Gitea instance is connected with a token
// (checked first); an app from one of its repositories gets its deploy key
// and push webhook added there, builds over SSH with that key (the token
// never reaches the server), deploys pushes the webhook reports, shows them
// on the commits, and takes its key and webhook away when deleted.
func TestForgeSources(t *testing.T) {
	for _, kind := range []string{"gitlab", "gitea"} {
		t.Run(kind, func(t *testing.T) {
			f := forgetest.New(t, kind, forgetest.Repo{FullName: "acme/shop", Private: true, DefaultBranch: "main", Branches: []string{"main", "dev"}})
			hub := &fakeHub{}
			clock := &testClock{t: time.Now()}
			srv := newTestServerWith(t, Options{SetupToken: testSetupToken, Now: clock.Now, Agents: hub})
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)
			e := &env{t: t, srv: srv, ts: ts, clock: clock}
			c := e.setupOwner()

			src := map[string]any{"kind": kind, "name": "work", "url": f.URL, "token": f.Token, "allow_private": true}
			c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/git-sources", src) // no panel address yet
			c.want(http.StatusOK, "PUT", "/api/v1/notifications/panel-url", map[string]any{"url": "https://panel.example.com"})
			for k, v := range map[string]any{"kind": "svn", "url": "ftp://x", "token": "a b", "name": "bad/name"} {
				bad := map[string]any{}
				for kk, vv := range src {
					bad[kk] = vv
				}
				bad[k] = v
				c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/git-sources", bad)
			}
			noPrivate := map[string]any{"kind": kind, "name": "work", "url": f.URL, "token": f.Token}
			c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/git-sources", noPrivate) // http without a private network
			wrong := map[string]any{"kind": kind, "name": "work", "url": f.URL, "token": "wrong-token-123", "allow_private": true}
			c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/git-sources", wrong)
			s := c.want(http.StatusCreated, "POST", "/api/v1/git-sources", src).body["source"].(map[string]any)
			if s["kind"] != kind || s["owner"] != "alice" || s["base_url"] != f.URL || s["install_url"] != "" {
				t.Fatalf("source: %v", s)
			}
			var blob []byte
			srv.db.R.QueryRow(`SELECT secrets FROM git_sources WHERE id = ?`, s["id"]).Scan(&blob)
			if bytes.Contains(blob, []byte(f.Token)) {
				t.Fatal("the token is stored in the clear")
			}
			base := "/api/v1/git-sources/" + strconv.Itoa(int(s["id"].(float64)))
			if d := c.want(http.StatusOK, "GET", base, nil).body; d["error"] != nil {
				t.Fatalf("source: %v", d)
			}
			if r := c.want(http.StatusOK, "GET", base+"/repos", nil).body["repos"].([]any); len(r) != 1 || r[0].(map[string]any)["full_name"] != "acme/shop" {
				t.Fatalf("repos: %v", r)
			}
			if b := c.want(http.StatusOK, "GET", base+"/branches?repo=acme/shop", nil).body["branches"].([]any); len(b) != 2 {
				t.Fatalf("branches: %v", b)
			}

			srvID := c.want(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "s1"}).body["server"].(map[string]any)["id"]
			srv.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, srvID)
			c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/apps", map[string]any{"server_id": srvID, "name": "x", "source": "git", "port": 80,
				"git": map[string]any{"source_id": s["id"], "repo": "acme/missing"}})
			a := c.want(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": srvID, "name": "shop", "source": "git", "port": 80,
				"git": map[string]any{"source_id": s["id"], "repo": "acme/shop", "branch": "main"}}).body["app"].(map[string]any)
			appID := strconv.Itoa(int(a["id"].(float64)))
			if g := a["git"].(map[string]any); !strings.HasPrefix(g["url"].(string), "git@") || g["repo"] != "acme/shop" {
				t.Fatalf("app git: %v", g)
			}
			hook := c.want(http.StatusOK, "GET", "/api/v1/apps/"+appID+"/webhook", nil).body["webhook"].(map[string]any)
			keys, hooks := f.Keys(), f.Hooks()
			if len(keys) != 1 || !keys[0].ReadOnly || keys[0].Key != a["deploy_key"] || len(hooks) != 1 ||
				hooks[0].URL != "https://panel.example.com/api/v1/webhooks/apps/"+appID || hooks[0].Secret != hook["secret"] || !hooks[0].PushEvents {
				t.Fatalf("on the forge: keys %+v hooks %+v", keys, hooks)
			}

			// A push the forge reports deploys that commit, over SSH with the
			// app's key; the token stays in the panel.
			sha := strings.Repeat("1e", 20)
			body, _ := json.Marshal(map[string]any{"ref": "refs/heads/main", "after": sha})
			req, _ := http.NewRequest("POST", ts.URL+"/api/v1/webhooks/apps/"+appID, bytes.NewReader(body))
			if kind == "gitlab" {
				req.Header.Set("X-Gitlab-Event", "Push Hook")
				req.Header.Set("X-Gitlab-Token", hook["secret"].(string))
			} else {
				mac := hmac.New(sha256.New, []byte(hook["secret"].(string)))
				mac.Write(body)
				req.Header.Set("X-Gitea-Event", "push")
				req.Header.Set("X-Gitea-Signature", hex.EncodeToString(mac.Sum(nil)))
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("push: %d", resp.StatusCode)
			}
			for range 300 {
				if st := f.Statuses(); len(st) == 2 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			d := hub.deployed()
			if len(d) != 1 || d[0].Git.Commit != sha || d[0].Git.Token != "" || d[0].Git.DeployKey == "" || strings.Contains(d[0].Git.DeployKey, f.Token) {
				t.Fatalf("deployment: %+v", d)
			}
			done := "success"
			if st := f.Statuses(); len(st) != 2 || st[1].State != done || st[1].SHA != sha || st[1].Context != "Basalt / shop" ||
				st[1].TargetURL != "https://panel.example.com/apps/"+appID {
				t.Fatalf("statuses: %+v", st)
			}

			// A new deploy key or webhook secret replaces the old one there.
			k2 := c.want(http.StatusOK, "POST", "/api/v1/apps/"+appID+"/deploy-key", nil).body["app"].(map[string]any)["deploy_key"]
			c.want(http.StatusOK, "PUT", "/api/v1/apps/"+appID+"/webhook", map[string]any{"auto_deploy": true, "regenerate": true})
			h2 := c.want(http.StatusOK, "GET", "/api/v1/apps/"+appID+"/webhook", nil).body["webhook"].(map[string]any)["secret"]
			if keys, hooks := f.Keys(), f.Hooks(); len(keys) != 1 || keys[0].Key != k2 || len(hooks) != 1 || hooks[0].Secret != h2 {
				t.Fatalf("after replacing: keys %+v hooks %+v", keys, hooks)
			}
			// Deleted: its key and webhook go.
			c.want(http.StatusOK, "DELETE", "/api/v1/apps/"+appID, nil)
			if len(f.Keys()) != 0 || len(f.Hooks()) != 0 {
				t.Fatalf("after deleting: keys %+v hooks %+v", f.Keys(), f.Hooks())
			}
		})
	}
}
