package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/forge"
	"github.com/wiefix/basalt/internal/store"
)

// Git sources on GitLab and Gitea or Forgejo (20.6): an instance and an
// access token, kept encrypted in the panel and never sent to servers. With
// it the panel lists repositories and branches, adds each app's read-only
// deploy key and push webhook to its repository (and removes them with
// the app), and sets the status of deployed commits. Servers clone over
// SSH with the app's deploy key, as for any repository.

// forgeSecrets are a GitLab or Gitea source's credentials.
type forgeSecrets struct {
	Token string `json:"token"`
	// AllowPrivate: the instance may be on a private network.
	AllowPrivate bool `json:"allow_private"`
}

func isForge(kind string) bool { return kind == forge.GitLab || kind == forge.Gitea }

// forgeClient opens a GitLab or Gitea source.
func (s *Server) forgeClient(g *store.GitSource) (*forge.Client, error) {
	b, err := s.box.Open(g.Secrets, gitSourceAD(g.ID))
	if err != nil {
		return nil, fmt.Errorf("decrypt the git source: %w", err)
	}
	var sec forgeSecrets
	if err := json.Unmarshal(b, &sec); err != nil {
		return nil, err
	}
	return s.newForgeClient(g.Kind, g.BaseURL, sec), nil
}

func (s *Server) newForgeClient(kind, base string, sec forgeSecrets) *forge.Client {
	return &forge.Client{Kind: kind, BaseURL: base, Token: sec.Token,
		HTTP: outboundClient(sec.AllowPrivate, githubTimeout, func(*url.URL) error { return nil }, "set the source to allow a private network")}
}

type forgeSourceRequest struct {
	// Kind: "gitlab" or "gitea" (Gitea and Forgejo).
	Kind string `json:"kind"`
	Name string `json:"name"`
	// URL of the instance, e.g. https://gitlab.com or https://git.example.com.
	URL string `json:"url"`
	// Token: a GitLab access token with the api scope (Maintainer of the
	// repositories), or a Gitea token with write:repository and read:user.
	Token string `json:"token"`
	// AllowPrivate lets the instance be on a private network (plain http too).
	AllowPrivate bool `json:"allow_private"`
}

// handleCreateForgeSource connects a GitLab or Gitea instance; the token
// is checked first.
func (s *Server) handleCreateForgeSource(w http.ResponseWriter, r *http.Request, p *principal) {
	var req forgeSourceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name, req.URL, req.Token = strings.TrimSpace(req.Name), strings.TrimRight(strings.TrimSpace(req.URL), "/"), strings.TrimSpace(req.Token)
	switch {
	case !isForge(req.Kind):
		writeError(w, http.StatusUnprocessableEntity, `kind must be "gitlab" or "gitea"`)
		return
	case !appNameRE.MatchString(req.Name):
		writeError(w, http.StatusUnprocessableEntity, "name: up to 34 letters, digits, spaces, dots, dashes and underscores")
		return
	case forge.ValidBaseURL(req.URL) != nil:
		writeError(w, http.StatusUnprocessableEntity, forge.ValidBaseURL(req.URL).Error())
		return
	case strings.HasPrefix(req.URL, "http://") && !req.AllowPrivate:
		writeError(w, http.StatusUnprocessableEntity, "the address must use https (http only on a private network)")
		return
	case !forge.ValidToken(req.Token):
		writeError(w, http.StatusUnprocessableEntity, "invalid token")
		return
	}
	panelURL, _ := s.db.Setting(r.Context(), settingPanelURL)
	if panelURL == "" {
		writeError(w, http.StatusUnprocessableEntity, "set the panel's address first (Notifications, Links in messages): the repositories' webhooks call it")
		return
	}
	sec := forgeSecrets{Token: req.Token, AllowPrivate: req.AllowPrivate}
	user, err := s.newForgeClient(req.Kind, req.URL, sec).User(r.Context())
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "the token does not work: "+err.Error())
		return
	}
	g := &store.GitSource{Kind: req.Kind, Name: req.Name, Slug: req.Kind, Owner: user, HTMLURL: req.URL, BaseURL: req.URL, Secrets: []byte{}}
	if err := s.db.CreateGitSource(r.Context(), g); errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "a git source named "+g.Name+" exists")
		return
	} else if err != nil {
		s.internalError(w, r, "create git source", err)
		return
	}
	b, _ := json.Marshal(sec)
	if err := s.db.SetGitSourceSecrets(r.Context(), g.ID, s.box.Seal(b, gitSourceAD(g.ID))); err != nil {
		s.db.DeleteGitSource(r.Context(), g.ID)
		s.internalError(w, r, "store git source", err)
		return
	}
	s.audit(r, &p.user.ID, "git_source.created", map[string]any{"source_id": g.ID, "name": g.Name, "kind": g.Kind, "url": g.BaseURL, "user": user})
	writeJSON(w, http.StatusCreated, map[string]any{"source": s.viewGitSource(g)})
}

// forgeOf returns the forge an app's repository is on (nil: not on one).
func (s *Server) forgeOf(ctx context.Context, app *store.App) (*forge.Client, error) {
	if app.Source != store.SourceGit || app.Git.SourceID == 0 || app.Git.Repo == "" {
		return nil, nil
	}
	g, err := s.db.GitSourceByID(ctx, app.Git.SourceID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if !isForge(g.Kind) {
		return nil, nil
	}
	return s.forgeClient(g)
}

// connectForge (re)adds the app's deploy key and push webhook to its
// repository on GitLab or Gitea, replacing those it added before.
func (s *Server) connectForge(ctx context.Context, app *store.App) error {
	c, err := s.forgeOf(ctx, app)
	if err != nil || c == nil {
		return err
	}
	s.disconnectForge(ctx, app, c)
	panelURL, _ := s.db.Setting(ctx, settingPanelURL)
	if panelURL == "" {
		return errors.New("set the panel's address first (Notifications, Links in messages): the webhook calls it")
	}
	secret, err := s.box.Open(app.WebhookSecret, webhookAD(app.ID))
	if err != nil {
		return fmt.Errorf("decrypt webhook secret: %w", err)
	}
	keyID, err := c.AddDeployKey(ctx, app.Git.Repo, "Basalt app "+app.Name, app.DeployKeyPublic)
	if err != nil {
		return fmt.Errorf("add the deploy key to %s: %w", app.Git.Repo, err)
	}
	hookID, err := c.AddHook(ctx, app.Git.Repo, panelURL+"/api/v1/webhooks/apps/"+strconv.FormatInt(app.ID, 10), string(secret))
	if err != nil {
		c.DeleteDeployKey(ctx, app.Git.Repo, keyID)
		return fmt.Errorf("add the webhook to %s: %w", app.Git.Repo, err)
	}
	app.ForgeKeyID, app.ForgeHookID = keyID, hookID
	return s.db.SetForgeHooks(ctx, app.ID, keyID, hookID)
}

// disconnectForge removes the deploy key and webhook the panel added to the
// app's repository (as far as it can: they may be gone already).
func (s *Server) disconnectForge(ctx context.Context, app *store.App, c *forge.Client) {
	if app.ForgeKeyID == 0 && app.ForgeHookID == 0 {
		return
	}
	if c == nil {
		var err error
		if c, err = s.forgeOf(ctx, app); err != nil || c == nil {
			return
		}
	}
	if app.ForgeKeyID != 0 {
		if err := c.DeleteDeployKey(ctx, app.Git.Repo, app.ForgeKeyID); err != nil {
			s.log.Warn("remove the deploy key from the repository", "app_id", app.ID, "err", err)
		}
	}
	if app.ForgeHookID != 0 {
		if err := c.DeleteHook(ctx, app.Git.Repo, app.ForgeHookID); err != nil {
			s.log.Warn("remove the webhook from the repository", "app_id", app.ID, "err", err)
		}
	}
	app.ForgeKeyID, app.ForgeHookID = 0, 0
	s.db.SetForgeHooks(ctx, app.ID, 0, 0)
}

// forgeRepos lists the repositories a GitLab or Gitea source administers.
func (s *Server) forgeRepos(ctx context.Context, g *store.GitSource) ([]repoView, error) {
	c, err := s.forgeClient(g)
	if err != nil {
		return nil, err
	}
	repos, err := c.Repos(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]repoView, len(repos))
	for i, rp := range repos {
		out[i] = repoView{FullName: rp.FullName, Private: rp.Private, DefaultBranch: rp.DefaultBranch, HTMLURL: rp.HTMLURL}
	}
	return out, nil
}

// resolveForgeRepo takes the SSH clone URL of repo from its forge.
func (s *Server) resolveForgeRepo(ctx context.Context, g *store.GitSource, repo string) (string, error) {
	c, err := s.forgeClient(g)
	if err != nil {
		return "", err
	}
	info, err := c.Repo(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("%s: %w", repo, err)
	}
	if !agentproto.IsSSHGitURL(info.SSHURL) || agentproto.ValidateGitURL(info.SSHURL) != nil {
		return "", errors.New("the forge gave an unusable SSH clone URL")
	}
	return info.SSHURL, nil
}
