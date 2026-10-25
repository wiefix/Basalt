package forge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/forge"
	"github.com/wiefix/basalt/internal/forge/forgetest"
)

// TestClient: on GitLab and Gitea, the token is checked, repositories and
// branches are listed, a read-only deploy key and a push webhook are added
// and removed, and commit statuses are set (GitLab's own states).
func TestClient(t *testing.T) {
	for _, kind := range []string{forge.GitLab, forge.Gitea} {
		t.Run(kind, func(t *testing.T) {
			full := "acme/shop"
			if kind == forge.GitLab {
				full = "acme/web/shop" // a subgroup
			}
			f := forgetest.New(t, kind, forgetest.Repo{FullName: full, Private: true, DefaultBranch: "main", Branches: []string{"main", "dev"}})
			ctx := context.Background()
			bad := &forge.Client{Kind: kind, BaseURL: f.URL, Token: "wrong-token"}
			if _, err := bad.User(ctx); err == nil || !strings.Contains(err.Error(), "401") {
				t.Fatalf("wrong token: %v", err)
			}
			c := &forge.Client{Kind: kind, BaseURL: f.URL + "/", Token: f.Token}
			if u, err := c.User(ctx); err != nil || u != "alice" {
				t.Fatalf("user: %q %v", u, err)
			}
			repos, err := c.Repos(ctx)
			if err != nil || len(repos) != 1 || repos[0].FullName != full || !repos[0].Private || !strings.HasPrefix(repos[0].SSHURL, "git@") {
				t.Fatalf("repos: %+v %v", repos, err)
			}
			if r, err := c.Repo(ctx, full); err != nil || r.DefaultBranch != "main" {
				t.Fatalf("repo: %+v %v", r, err)
			}
			if _, err := c.Repo(ctx, "acme/missing"); err == nil {
				t.Fatal("a missing repository found")
			}
			for _, r := range []string{"acme", "../x/y", "a/b c", "a/./b"} {
				if _, err := c.Repo(ctx, r); err == nil {
					t.Errorf("%q accepted", r)
				}
			}
			if bs, err := c.Branches(ctx, full); err != nil || len(bs) != 2 || bs[1] != "dev" {
				t.Fatalf("branches: %v %v", bs, err)
			}
			kid, err := c.AddDeployKey(ctx, full, "basalt app 1", "ssh-ed25519 AAAAkey basalt")
			if err != nil || len(f.Keys()) != 1 || !f.Keys()[0].ReadOnly || f.Keys()[0].Key != "ssh-ed25519 AAAAkey basalt" {
				t.Fatalf("key: %d %v %+v", kid, err, f.Keys())
			}
			hid, err := c.AddHook(ctx, full, "https://panel.example.com/api/v1/webhooks/apps/1", "s3cret")
			if h := f.Hooks(); err != nil || len(h) != 1 || h[0].Secret != "s3cret" || !h[0].PushEvents || h[0].URL != "https://panel.example.com/api/v1/webhooks/apps/1" {
				t.Fatalf("hook: %d %v %+v", hid, err, h)
			}
			sha := strings.Repeat("0f", 20)
			for _, st := range []string{"pending", "success", "failure", "error"} {
				if err := c.SetStatus(ctx, full, sha, st, "https://panel.example.com/apps/1", strings.Repeat("d", 200), "Basalt / shop"); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.SetStatus(ctx, full, "abc", "success", "", "", "x"); err == nil {
				t.Error("short sha accepted")
			}
			if err := c.SetStatus(ctx, full, sha, "done", "", "", "x"); err == nil {
				t.Error("unknown state accepted")
			}
			want := []string{"pending", "success", "failure", "error"}
			if kind == forge.GitLab {
				want = []string{"running", "success", "failed", "canceled"}
			}
			sts := f.Statuses()
			for i, st := range sts {
				if st.State != want[i] || st.Context != "Basalt / shop" || len(st.Description) != 140 || st.SHA != sha {
					t.Errorf("status %d: %+v", i, st)
				}
			}
			if err := c.DeleteDeployKey(ctx, full, kid); err != nil || len(f.Keys()) != 0 {
				t.Fatalf("delete key: %v", err)
			}
			if err := c.DeleteHook(ctx, full, hid); err != nil || len(f.Hooks()) != 0 {
				t.Fatalf("delete hook: %v", err)
			}
			// Already gone: fine.
			if err := c.DeleteHook(ctx, full, hid); err != nil {
				t.Fatalf("delete again: %v", err)
			}
		})
	}
	for _, u := range []string{"ftp://x", "https://", "https://u:p@x", "https://x?a=1", "gitlab.com"} {
		if forge.ValidBaseURL(u) == nil {
			t.Errorf("%q accepted", u)
		}
	}
}
