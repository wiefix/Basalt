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

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/github/githubtest"
)

// TestPreviews: a pull request into a GitHub app's branch gets a preview
// at pr-<n>.<domain>, with the app's environment, deployed at each push to
// it and deleted with its data when it closes; pull requests from forks,
// into other branches, or of an app without a preview domain get none.
func TestPreviews(t *testing.T) {
	gh := githubtest.New(t, githubtest.Repo{FullName: "acme/shop", DefaultBranch: "main", Branches: []string{"main"}, Installation: 7})
	hub := &fakeHub{}
	clock := &testClock{t: time.Now()}
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, Now: clock.Now, Agents: hub, GitHubAPI: gh.URL, GitHubWeb: "https://github.example"})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, srv: srv, ts: ts, clock: clock}
	c := e.setupOwner()
	c.want(http.StatusOK, "PUT", "/api/v1/notifications/panel-url", map[string]any{"url": "https://panel.example.com"})
	state := c.want(http.StatusOK, "POST", "/api/v1/git-sources/github/manifest", map[string]any{"name": "t"}).body["state"].(string)
	sid := c.want(http.StatusCreated, "POST", "/api/v1/git-sources/github/complete", map[string]any{"code": gh.Code, "state": state}).body["source"].(map[string]any)["id"]
	s := c.want(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "s1"}).body["server"].(map[string]any)
	srv.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, s["id"])
	img := c.want(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": s["id"], "name": "img", "image": "nginx:1.27", "port": 80}).body["app"].(map[string]any)
	c.want(http.StatusUnprocessableEntity, "PUT", "/api/v1/apps/"+strconv.Itoa(int(img["id"].(float64)))+"/previews", map[string]any{"domain": "preview.example.com"})
	a := c.want(http.StatusCreated, "POST", "/api/v1/apps", map[string]any{"server_id": s["id"], "name": "shop", "source": "git", "port": 80,
		"git": map[string]any{"source_id": sid, "repo": "acme/shop", "branch": "main"}}).body["app"].(map[string]any)
	appID := strconv.Itoa(int(a["id"].(float64)))
	base := "/api/v1/apps/" + appID + "/previews"
	c.want(http.StatusOK, "PUT", "/api/v1/apps/"+appID+"/env", map[string]any{"env": map[string]string{"API_KEY": "k1"}})

	sha := func(b string) string { return strings.Repeat(b, 40) }
	pull := func(action string, number int, head, headSHA, headRepo, baseRef string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"action": action, "number": number, "repository": map[string]any{"full_name": "acme/shop"},
			"pull_request": map[string]any{"head": map[string]any{"ref": head, "sha": headSHA, "repo": map[string]any{"full_name": headRepo}},
				"base": map[string]any{"ref": baseRef}}})
		mac := hmac.New(sha256.New, []byte(gh.WebhookSecret))
		mac.Write(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s #%d: %d", action, number, resp.StatusCode)
		}
	}
	previews := func(n int) []any {
		t.Helper()
		var l []any
		for range 300 {
			l = c.want(http.StatusOK, "GET", base, nil).body["previews"].([]any)
			var running int
			srv.db.R.QueryRow(`SELECT COUNT(*) FROM deployments WHERE status = 'running'`).Scan(&running)
			if len(l) == n && running == 0 {
				return l
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("previews: %v", l)
		return nil
	}

	// No preview domain yet: nothing.
	pull("opened", 7, "feature", sha("a"), "acme/shop", "main")
	time.Sleep(100 * time.Millisecond)
	previews(0)
	c.want(http.StatusUnprocessableEntity, "PUT", base, map[string]any{"domain": "*.preview.example.com"})
	c.want(http.StatusUnprocessableEntity, "PUT", base, map[string]any{"domain": "bad domain"})
	if d := c.want(http.StatusOK, "PUT", base, map[string]any{"domain": "Preview.Example.com"}).body["domain"]; d != "preview.example.com" {
		t.Fatalf("domain: %v", d)
	}

	pull("opened", 7, "feature", sha("a"), "acme/shop", "main")
	pv := previews(1)[0].(map[string]any)
	pvID := int64(pv["id"].(float64))
	if pv["name"] != "shop-pr-7" || pv["pr_number"] != float64(7) || pv["preview_of"] != a["id"] ||
		pv["git"].(map[string]any)["branch"] != "feature" || pv["domains"].([]any)[0] != "pr-7.preview.example.com" {
		t.Fatalf("preview: %v", pv)
	}
	last := func() agentproto.AppSpec { d := hub.deployed(); return d[len(d)-1] }
	waitDeploys := func(n int) {
		t.Helper()
		for range 300 {
			if len(hub.deployed()) >= n {
				previews(len(c.want(http.StatusOK, "GET", base, nil).body["previews"].([]any))) // and finished
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("deployments: %d, want %d", len(hub.deployed()), n)
	}
	waitDeploys(1)
	if d := last(); d.AppID != pvID || d.Git.Commit != sha("a") || d.Env["API_KEY"] != "k1" || d.Git.Token == "" {
		t.Fatalf("preview deployment: %+v", d)
	}
	// A fork's pull request, one into another branch: not built.
	n := len(hub.deployed())
	pull("opened", 8, "evil", sha("b"), "mallory/shop", "main")
	pull("opened", 9, "x", sha("c"), "acme/shop", "dev")
	time.Sleep(100 * time.Millisecond)
	previews(1)
	// A push to the pull request: the preview deploys it, with the app's
	// environment as it is now; a push to its branch alone does not.
	c.want(http.StatusOK, "PUT", "/api/v1/apps/"+appID+"/env", map[string]any{"env": map[string]string{"API_KEY": "k2"}})
	pull("synchronize", 7, "feature", sha("d"), "acme/shop", "main")
	waitDeploys(n + 1)
	if d := last(); len(hub.deployed()) != n+1 || d.AppID != pvID || d.Git.Commit != sha("d") || d.Env["API_KEY"] != "k2" {
		t.Fatalf("after a push to the pull request: %d %+v", len(hub.deployed())-n, d)
	}
	// Its commit status links to the preview.
	for range 300 {
		if st := gh.Statuses(); len(st) > 0 && st[len(st)-1].State == "success" && st[len(st)-1].SHA == sha("d") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := gh.Statuses(); st[len(st)-1].TargetURL != "https://pr-7.preview.example.com" || st[len(st)-1].Context != "Basalt / shop-pr-7" {
		t.Fatalf("statuses: %+v", st)
	}

	// Closed: the preview goes, with its volumes.
	pull("closed", 7, "feature", sha("d"), "acme/shop", "main")
	previews(0)
	var deleted bool
	for range 300 {
		for _, r := range hub.called(agentproto.TypeAppDelete) {
			if q := r.(agentproto.AppRequest); q.AppID == pvID && q.DeleteVolumes {
				deleted = true
			}
		}
		if deleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !deleted {
		t.Fatal("the preview's containers and volumes were not removed")
	}
	// Deleting the app deletes its previews.
	pull("opened", 10, "f2", sha("e"), "acme/shop", "main")
	pv2 := int64(previews(1)[0].(map[string]any)["id"].(float64))
	c.want(http.StatusOK, "DELETE", "/api/v1/apps/"+appID, nil)
	c.want(http.StatusNotFound, "GET", "/api/v1/apps/"+strconv.FormatInt(pv2, 10), nil)
}
