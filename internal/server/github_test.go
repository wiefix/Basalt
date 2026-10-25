package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/github"
	"github.com/wiefix/basalt/internal/github/githubtest"
)

// TestGitHubSource: the panel creates a GitHub App by the manifest flow,
// lists the repositories it is installed on and their branches, builds an
// app from one with a token that reads only that repository, and deploys
// it when GitHub reports a push to its branch.
func TestGitHubSource(t *testing.T) {
	gh := githubtest.New(t,
		githubtest.Repo{FullName: "acme/shop", Private: true, DefaultBranch: "main", Branches: []string{"main", "dev"}, Installation: 7},
		githubtest.Repo{FullName: "acme/blog", DefaultBranch: "main", Branches: []string{"main"}, Installation: 7},
	)
	hub := &fakeHub{}
	clock := &testClock{t: time.Now()}
	srv := newTestServerWith(t, Options{SetupToken: testSetupToken, Now: clock.Now, Agents: hub, GitHubAPI: gh.URL, GitHubWeb: "https://github.example"})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	e := &env{t: t, srv: srv, ts: ts, clock: clock}
	c := e.setupOwner()

	// The manifest needs the panel's address; then the browser posts it.
	start := map[string]any{"name": "Basalt test"}
	c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/git-sources/github/manifest", start)
	c.want(http.StatusOK, "PUT", "/api/v1/notifications/panel-url", map[string]any{"url": "https://panel.example.com"})
	c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/git-sources/github/manifest", map[string]any{"name": "bad/name"})
	c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/git-sources/github/manifest", map[string]any{"name": "ok", "organization": "-x"})
	org := c.want(http.StatusOK, "POST", "/api/v1/git-sources/github/manifest", map[string]any{"name": "ok", "organization": "acme"}).body
	if !strings.HasPrefix(org["action"].(string), "https://github.example/organizations/acme/settings/apps/new?state=") {
		t.Fatalf("org action: %v", org["action"])
	}
	m := c.want(http.StatusOK, "POST", "/api/v1/git-sources/github/manifest", start).body
	state := m["state"].(string)
	if m["action"] != "https://github.example/settings/apps/new?state="+state ||
		!strings.Contains(m["manifest"].(string), `"url":"https://panel.example.com/api/v1/webhooks/github"`) {
		t.Fatalf("manifest: %v", m)
	}
	c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/git-sources/github/complete", map[string]any{"code": gh.Code, "state": "nope"})
	src := c.want(http.StatusCreated, "POST", "/api/v1/git-sources/github/complete", map[string]any{"code": gh.Code, "state": state}).body["source"].(map[string]any)
	sid := int64(src["id"].(float64))
	if src["slug"] != gh.Slug || src["owner"] != "acme" || src["install_url"] != "https://github.example/apps/"+gh.Slug+"/installations/new" {
		t.Fatalf("source: %v", src)
	}
	c.want(http.StatusUnprocessableEntity, "POST", "/api/v1/git-sources/github/complete", map[string]any{"code": gh.Code, "state": state})
	var blob []byte
	srv.db.R.QueryRow(`SELECT secrets FROM git_sources WHERE id = ?`, sid).Scan(&blob)
	if bytes.Contains(blob, []byte(gh.WebhookSecret)) || bytes.Contains(blob, []byte("PRIVATE KEY")) {
		t.Fatal("the source's secrets are stored in the clear")
	}
	base := "/api/v1/git-sources/" + strconv.FormatInt(sid, 10)
	if l := c.want(http.StatusOK, "GET", "/api/v1/git-sources", nil).body["sources"].([]any); len(l) != 1 {
		t.Fatalf("sources: %v", l)
	}
	if in := c.want(http.StatusOK, "GET", base, nil).body["installations"].([]any); len(in) != 1 || in[0].(map[string]any)["account"] != "acme" || in[0].(map[string]any)["statuses"] != true {
		t.Fatalf("installations: %v", in)
	}
	repos := c.want(http.StatusOK, "GET", base+"/repos", nil).body["repos"].([]any)
	if len(repos) != 2 || repos[0].(map[string]any)["full_name"] != "acme/shop" || repos[0].(map[string]any)["private"] != true {
		t.Fatalf("repos: %v", repos)
	}
	if bs := c.want(http.StatusOK, "GET", base+"/branches?repo=acme/shop", nil).body["branches"].([]any); len(bs) != 2 || bs[1] != "dev" {
		t.Fatalf("branches: %v", bs)
	}
	c.want(http.StatusUnprocessableEntity, "GET", base+"/branches?repo=../x", nil)
	c.want(http.StatusNotFound, "GET", "/api/v1/git-sources/999/repos", nil)

	// An app from a repository of the source.
	s := c.want(http.StatusCreated, "POST", "/api/v1/servers", map[string]string{"name": "s1"}).body["server"].(map[string]any)
	srv.db.W.Exec(`UPDATE servers SET docker = '28.0.0' WHERE id = ?`, s["id"])
	app := func(name, repo, branch string, code int) map[string]any {
		return c.want(code, "POST", "/api/v1/apps", map[string]any{"server_id": s["id"], "name": name, "source": "git", "port": 80,
			"git": map[string]any{"source_id": sid, "repo": repo, "branch": branch}}).body
	}
	app("nope", "acme/missing", "main", http.StatusUnprocessableEntity)
	app("bad", "acme/shop/x", "main", http.StatusUnprocessableEntity)
	a := app("shop", "acme/shop", "dev", http.StatusCreated)["app"].(map[string]any)
	if g := a["git"].(map[string]any); g["url"] != "https://github.example/acme/shop.git" || g["repo"] != "acme/shop" || g["source_id"] != float64(sid) {
		t.Fatalf("app git: %v", g)
	}
	app("blog", "acme/blog", "main", http.StatusCreated)
	appID := strconv.Itoa(int(a["id"].(float64)))
	waitIdle := func() {
		t.Helper()
		for range 300 {
			var n int
			srv.db.R.QueryRow(`SELECT COUNT(*) FROM deployments WHERE status = 'running'`).Scan(&n)
			if n == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("deployments still running")
	}
	c.want(http.StatusAccepted, "POST", "/api/v1/apps/"+appID+"/deploy", map[string]any{})
	waitIdle()
	specs := hub.deployed()
	if len(specs) != 1 || specs[0].Git == nil || !github.ValidToken(specs[0].Git.Token) {
		t.Fatalf("deployed: %+v", specs)
	}
	if reads := gh.Tokens()[specs[0].Git.Token]; !slices.Equal(reads, []string{"shop"}) {
		t.Fatalf("the build's token reads %v", reads)
	}

	// GitHub reports pushes, signed with the app's secret.
	push := func(secret, repo, ref string, code int) map[string]any {
		body, _ := json.Marshal(map[string]any{"ref": ref, "after": strings.Repeat("ab", 20), "repository": map[string]any{"full_name": repo}})
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/webhooks/github", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != code {
			t.Fatalf("push %s %s: %d %v", repo, ref, resp.StatusCode, out)
		}
		return out
	}
	push("wrong", "acme/shop", "refs/heads/dev", http.StatusUnauthorized)
	if r := push(gh.WebhookSecret, "acme/shop", "refs/heads/main", http.StatusAccepted); len(r["deploying"].([]any)) != 0 {
		t.Fatalf("another branch deployed: %v", r)
	}
	r := push(gh.WebhookSecret, "acme/shop", "refs/heads/dev", http.StatusAccepted)
	if d := r["deploying"].([]any); len(d) != 1 || d[0] != "shop" {
		t.Fatalf("push: %v", r)
	}
	waitIdle()
	specs = hub.deployed()
	if len(specs) != 2 || specs[1].Git.Commit != strings.Repeat("ab", 20) {
		t.Fatalf("pushed commit: %+v", specs[len(specs)-1].Git)
	}
	// The deployment shows on the commit: deploying, then deployed (the
	// manual one built an unknown commit: nothing to show).
	waitStatuses := func(n int) []githubtest.SetStatus {
		t.Helper()
		for range 300 {
			if st := gh.Statuses(); len(st) >= n {
				return st
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("statuses: %+v", gh.Statuses())
		return nil
	}
	st := waitStatuses(2)
	if len(st) != 2 || st[0].State != "pending" || st[1].State != "success" || st[1].SHA != strings.Repeat("ab", 20) ||
		st[1].Repo != "acme/shop" || st[1].Context != "Basalt / shop" || st[1].TargetURL != "https://panel.example.com/apps/"+appID ||
		!strings.HasPrefix(st[1].Description, "Deployed (#") {
		t.Fatalf("statuses: %+v", st)
	}
	// An installation without the permission: deployments go on, no status.
	gh.GrantStatuses(false)
	if in := c.want(http.StatusOK, "GET", base, nil).body["installations"].([]any); in[0].(map[string]any)["statuses"] != false {
		t.Fatalf("installations: %v", in)
	}
	push(gh.WebhookSecret, "acme/shop", "refs/heads/dev", http.StatusAccepted)
	waitIdle()
	if specs = hub.deployed(); len(specs) != 3 {
		t.Fatalf("deployed: %d", len(specs))
	}
	var ok string
	srv.db.R.QueryRow(`SELECT status FROM deployments ORDER BY id DESC LIMIT 1`).Scan(&ok)
	time.Sleep(100 * time.Millisecond)
	if ok != "succeeded" || len(gh.Statuses()) != 2 {
		t.Fatalf("without the permission: %s %+v", ok, gh.Statuses())
	}

	// The source goes: its apps keep their URL, without it.
	c.want(http.StatusNoContent, "DELETE", base, nil)
	if g := c.want(http.StatusOK, "GET", "/api/v1/apps/"+appID, nil).body["app"].(map[string]any)["git"].(map[string]any); g["source_id"] != nil || g["url"] == "" {
		t.Fatalf("after the source went: %v", g)
	}
}
