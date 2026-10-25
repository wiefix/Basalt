package github_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/wiefix/basalt/internal/github"
	"github.com/wiefix/basalt/internal/github/githubtest"
)

// TestClient: the manifest code converts once into the app; the app's
// token lists installations, repositories and branches; a repository
// token reads that repository only.
func TestClient(t *testing.T) {
	ctx := context.Background()
	gh := githubtest.New(t,
		githubtest.Repo{FullName: "acme/shop", Private: true, DefaultBranch: "main", Branches: []string{"main", "dev"}, Installation: 7},
		githubtest.Repo{FullName: "acme/blog", DefaultBranch: "trunk", Branches: []string{"trunk"}, Installation: 7},
		githubtest.Repo{FullName: "other/tool", DefaultBranch: "main", Branches: []string{"main"}, Installation: 9},
	)
	c := &github.Client{API: gh.URL, HTTP: gh.Client()}
	if _, err := c.ConvertManifest(ctx, "bad/code"); err == nil {
		t.Fatal("converted a bad code")
	}
	creds, err := c.ConvertManifest(ctx, gh.Code)
	if err != nil {
		t.Fatal(err)
	}
	if creds.ID != gh.AppID || creds.Slug != gh.Slug || creds.WebhookSecret != gh.WebhookSecret || creds.Owner.Login != "acme" {
		t.Fatalf("credentials: %+v", creds)
	}
	if _, err := c.ConvertManifest(ctx, gh.Code); err == nil {
		t.Fatal("converted twice")
	}
	key, err := github.ParseKey(creds.PEM)
	if err != nil {
		t.Fatal(err)
	}
	app := github.App{ID: creds.ID, Key: key}

	insts, err := c.Installations(ctx, app)
	if err != nil || len(insts) != 2 || insts[0].Account != "acme" {
		t.Fatalf("installations: %+v %v", insts, err)
	}
	repos, err := c.Repos(ctx, app)
	if err != nil || len(repos) != 3 || repos[0].FullName != "acme/shop" || !repos[0].Private || repos[0].InstallationID != 7 || repos[2].InstallationID != 9 {
		t.Fatalf("repos: %+v %v", repos, err)
	}
	if bs, err := c.Branches(ctx, app, "acme/shop"); err != nil || !slices.Equal(bs, []string{"main", "dev"}) {
		t.Fatalf("branches: %v %v", bs, err)
	}
	if _, err := c.Branches(ctx, app, "acme/missing"); err == nil {
		t.Fatal("branches of a repository the app does not reach")
	}
	tok, err := c.RepoToken(ctx, app, "acme/blog")
	if err != nil || !github.ValidToken(tok.Token) {
		t.Fatalf("token: %+v %v", tok, err)
	}
	if got := gh.Tokens()[tok.Token]; !slices.Equal(got, []string{"blog"}) {
		t.Fatalf("the token reads %v", got)
	}
	if r, err := c.RepoInfo(ctx, app, "acme/blog"); err != nil || r.DefaultBranch != "trunk" || r.CloneURL != "https://github.example/acme/blog.git" {
		t.Fatalf("repo: %+v %v", r, err)
	}
	// Another app's id: GitHub refuses the token.
	var ge *github.Error
	bad := github.App{ID: app.ID + 1, Key: key}
	if _, err := c.Installations(ctx, bad); !errors.As(err, &ge) || ge.Status != 401 {
		t.Fatalf("another app's id: %v", err)
	}
	for _, r := range []string{"acme", "acme/shop/x", "../x", "a/..", "acme/sh op", ""} {
		if github.ValidRepo(r) {
			t.Errorf("valid repo %q", r)
		}
	}
	if m := github.Manifest("Basalt x", "https://panel.example.com"); m["redirect_url"] != "https://panel.example.com/git-sources/github/callback" ||
		m["hook_attributes"].(map[string]any)["url"] != "https://panel.example.com/api/v1/webhooks/github" {
		t.Fatalf("manifest: %v", m)
	}
}
