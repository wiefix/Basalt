package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/github"
	"github.com/wiefix/basalt/internal/store"
)

// Git sources (20.1–20.3): the panel creates a GitHub App on the user's
// account or organization (the manifest flow: the browser posts the
// manifest to GitHub, GitHub sends it back with a code the panel turns
// into the app's credentials). Apps can then be built from any repository
// the app is installed on: the panel lists them, mints a token for each
// build, and one webhook of the app deploys every push.

const (
	manifestStateTTL = time.Hour
	githubTimeout    = 30 * time.Second
)

func gitSourceAD(id int64) []byte { return fmt.Appendf(nil, "git-source:%d", id) }

// gitSourceSecrets are a source's credentials, encrypted as one.
type gitSourceSecrets struct {
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
}

// manifestStates are the manifest flows under way: state -> who started
// it and until when.
type manifestStates struct {
	mu sync.Mutex
	m  map[string]manifestState
}

type manifestState struct {
	user    int64
	expires time.Time
}

func (s *Server) githubClient() *github.Client {
	return &github.Client{API: s.githubAPI, Now: s.now,
		HTTP: outboundClient(s.githubPrivate, githubTimeout, func(*url.URL) error { return nil }, "GitHub is on the internet")}
}

func (s *Server) openGitSource(g *store.GitSource) (*gitSourceSecrets, github.App, error) {
	var sec gitSourceSecrets
	b, err := s.box.Open(g.Secrets, gitSourceAD(g.ID))
	if err != nil {
		return nil, github.App{}, fmt.Errorf("decrypt the git source: %w", err)
	}
	if err := json.Unmarshal(b, &sec); err != nil {
		return nil, github.App{}, err
	}
	key, err := github.ParseKey(sec.PEM)
	if err != nil {
		return nil, github.App{}, err
	}
	return &sec, github.App{ID: g.AppID, Key: key}, nil
}

type gitSourceView struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Owner     string    `json:"owner"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	// InstallURL installs the app on more repositories.
	InstallURL string `json:"install_url"`
}

func (s *Server) viewGitSource(g *store.GitSource) gitSourceView {
	return gitSourceView{ID: g.ID, Kind: g.Kind, Name: g.Name, Slug: g.Slug, Owner: g.Owner, HTMLURL: g.HTMLURL, CreatedAt: g.CreatedAt,
		InstallURL: s.githubWeb + "/apps/" + url.PathEscape(g.Slug) + "/installations/new"}
}

// canCreateApps: a global admin, or an admin of some project.
func (s *Server) canCreateApps(r *http.Request, p *principal) bool {
	return s.highestRole(r, p).AtLeast(store.RoleAdmin)
}

func (s *Server) handleListGitSources(w http.ResponseWriter, r *http.Request, p *principal) {
	if !s.canCreateApps(r, p) {
		writeError(w, http.StatusForbidden, "only those who can create apps see the git sources")
		return
	}
	list, err := s.db.GitSources(r.Context())
	if err != nil {
		s.internalError(w, r, "list git sources", err)
		return
	}
	out := make([]gitSourceView, len(list))
	for i, g := range list {
		out[i] = s.viewGitSource(g)
	}
	panelURL, _ := s.db.Setting(r.Context(), settingPanelURL)
	writeJSON(w, http.StatusOK, map[string]any{"sources": out, "panel_url": panelURL})
}

type installationView struct {
	ID      int64  `json:"id"`
	Account string `json:"account"`
	All     bool   `json:"all"`
	HTMLURL string `json:"html_url"`
	// Statuses: deployments are shown on the commits (the installation
	// granted the Commit statuses permission).
	Statuses bool `json:"statuses"`
	// PullRequests: pull requests reach the panel (previews).
	PullRequests bool `json:"pull_requests"`
}

func (s *Server) pathGitSource(w http.ResponseWriter, r *http.Request) (*store.GitSource, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "git source not found")
		return nil, false
	}
	g, err := s.db.GitSourceByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "git source not found")
		return nil, false
	} else if err != nil {
		s.internalError(w, r, "load git source", err)
		return nil, false
	}
	return g, true
}

// handleGitSource shows a source and where its app is installed (asked of
// GitHub).
func (s *Server) handleGitSource(w http.ResponseWriter, r *http.Request, p *principal) {
	g, ok := s.pathGitSource(w, r)
	if !ok {
		return
	}
	_, app, err := s.openGitSource(g)
	if err != nil {
		s.internalError(w, r, "open git source", err)
		return
	}
	resp := map[string]any{"source": s.viewGitSource(g), "installations": []installationView{}}
	insts, err := s.githubClient().Installations(r.Context(), app)
	if err != nil {
		resp["error"] = err.Error()
	} else {
		list := make([]installationView, len(insts))
		for i, in := range insts {
			list[i] = installationView(in)
		}
		resp["installations"] = list
	}
	writeJSON(w, http.StatusOK, resp)
}

type manifestRequest struct {
	// Name of the GitHub App (unique on GitHub).
	Name string `json:"name"`
	// Organization to create it in ("": the user's own account).
	Organization string `json:"organization"`
}

var (
	appNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,33}$`)
	orgRE     = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
)

// handleGitHubManifest starts the manifest flow: the browser posts the
// manifest to Action.
func (s *Server) handleGitHubManifest(w http.ResponseWriter, r *http.Request, p *principal) {
	var req manifestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !appNameRE.MatchString(req.Name) {
		writeError(w, http.StatusUnprocessableEntity, "the app's name: up to 34 letters, digits, spaces, dots, dashes and underscores")
		return
	}
	if req.Organization != "" && !orgRE.MatchString(req.Organization) {
		writeError(w, http.StatusUnprocessableEntity, "invalid organization")
		return
	}
	panelURL, _ := s.db.Setting(r.Context(), settingPanelURL)
	if panelURL == "" {
		writeError(w, http.StatusUnprocessableEntity, "set the panel's address first (Notifications, Links in messages): GitHub sends you back and its webhooks there")
		return
	}
	b := make([]byte, 16)
	rand.Read(b)
	state := hex.EncodeToString(b)
	s.manifests.mu.Lock()
	if s.manifests.m == nil {
		s.manifests.m = map[string]manifestState{}
	}
	now := s.now()
	for k, v := range s.manifests.m {
		if now.After(v.expires) {
			delete(s.manifests.m, k)
		}
	}
	s.manifests.m[state] = manifestState{user: p.user.ID, expires: now.Add(manifestStateTTL)}
	s.manifests.mu.Unlock()
	action := s.githubWeb + "/settings/apps/new?state=" + state
	if req.Organization != "" {
		action = s.githubWeb + "/organizations/" + url.PathEscape(req.Organization) + "/settings/apps/new?state=" + state
	}
	m, _ := json.Marshal(github.Manifest(req.Name, panelURL))
	writeJSON(w, http.StatusOK, map[string]any{"action": action, "manifest": string(m), "state": state})
}

type manifestCompleteRequest struct {
	Code  string `json:"code"`
	State string `json:"state"`
}

// handleGitHubComplete turns the code GitHub sent back into the app.
func (s *Server) handleGitHubComplete(w http.ResponseWriter, r *http.Request, p *principal) {
	var req manifestCompleteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	s.manifests.mu.Lock()
	st, ok := s.manifests.m[req.State]
	if ok {
		delete(s.manifests.m, req.State)
	}
	s.manifests.mu.Unlock()
	if !ok || st.user != p.user.ID || s.now().After(st.expires) {
		writeError(w, http.StatusUnprocessableEntity, "this GitHub App was not started here, or too long ago: start again")
		return
	}
	creds, err := s.githubClient().ConvertManifest(r.Context(), req.Code)
	if err != nil {
		writeError(w, http.StatusBadGateway, "GitHub: "+err.Error())
		return
	}
	g := &store.GitSource{Kind: "github", Name: creds.Name, AppID: creds.ID, Slug: creds.Slug, Owner: creds.Owner.Login,
		HTMLURL: creds.HTMLURL, Secrets: []byte{}}
	if g.Name == "" {
		g.Name = creds.Slug
	}
	if err := s.db.CreateGitSource(r.Context(), g); errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "a git source named "+g.Name+" exists")
		return
	} else if err != nil {
		s.internalError(w, r, "create git source", err)
		return
	}
	b, _ := json.Marshal(gitSourceSecrets{ClientID: creds.ClientID, ClientSecret: creds.ClientSecret, WebhookSecret: creds.WebhookSecret, PEM: creds.PEM})
	if err := s.db.SetGitSourceSecrets(r.Context(), g.ID, s.box.Seal(b, gitSourceAD(g.ID))); err != nil {
		s.db.DeleteGitSource(r.Context(), g.ID)
		s.internalError(w, r, "store git source", err)
		return
	}
	s.audit(r, &p.user.ID, "git_source.created", map[string]any{"source_id": g.ID, "name": g.Name, "owner": g.Owner, "github_app_id": g.AppID})
	writeJSON(w, http.StatusCreated, map[string]any{"source": s.viewGitSource(g)})
}

func (s *Server) handleDeleteGitSource(w http.ResponseWriter, r *http.Request, p *principal) {
	g, ok := s.pathGitSource(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteGitSource(r.Context(), g.ID); err != nil {
		s.internalError(w, r, "delete git source", err)
		return
	}
	s.audit(r, &p.user.ID, "git_source.deleted", map[string]any{"source_id": g.ID, "name": g.Name})
	w.WriteHeader(http.StatusNoContent)
}

type repoView struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
}

// handleGitSourceRepos lists the repositories the source's app reaches.
func (s *Server) handleGitSourceRepos(w http.ResponseWriter, r *http.Request, p *principal) {
	if !s.canCreateApps(r, p) {
		writeError(w, http.StatusForbidden, "only those who can create apps list repositories")
		return
	}
	g, ok := s.pathGitSource(w, r)
	if !ok {
		return
	}
	_, app, err := s.openGitSource(g)
	if err != nil {
		s.internalError(w, r, "open git source", err)
		return
	}
	repos, err := s.githubClient().Repos(r.Context(), app)
	if err != nil {
		writeError(w, http.StatusBadGateway, "GitHub: "+err.Error())
		return
	}
	out := make([]repoView, len(repos))
	for i, rp := range repos {
		out[i] = repoView{FullName: rp.FullName, Private: rp.Private, DefaultBranch: rp.DefaultBranch, HTMLURL: rp.HTMLURL}
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": out})
}

// handleGitSourceBranches lists a repository's branches.
func (s *Server) handleGitSourceBranches(w http.ResponseWriter, r *http.Request, p *principal) {
	if !s.canCreateApps(r, p) {
		writeError(w, http.StatusForbidden, "only those who can create apps list branches")
		return
	}
	g, ok := s.pathGitSource(w, r)
	if !ok {
		return
	}
	repo := r.URL.Query().Get("repo")
	if !github.ValidRepo(repo) {
		writeError(w, http.StatusUnprocessableEntity, "repo must be owner/name")
		return
	}
	_, app, err := s.openGitSource(g)
	if err != nil {
		s.internalError(w, r, "open git source", err)
		return
	}
	bs, err := s.githubClient().Branches(r.Context(), app, repo)
	if err != nil {
		writeError(w, http.StatusBadGateway, "GitHub: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": bs})
}

// resolveGitSource checks that an app's repository is reached through its
// source and takes its clone URL from GitHub.
func (s *Server) resolveGitSource(ctx context.Context, st *store.AppSettings) error {
	if st.Git.SourceID == 0 {
		return nil
	}
	g, err := s.db.GitSourceByID(ctx, st.Git.SourceID)
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("git source not found")
	} else if err != nil {
		return err
	}
	_, app, err := s.openGitSource(g)
	if err != nil {
		return err
	}
	info, err := s.githubClient().RepoInfo(ctx, app, st.Git.Repo)
	if err != nil {
		return fmt.Errorf("%s: %w", st.Git.Repo, err)
	}
	if !strings.HasPrefix(info.CloneURL, "https://") || agentproto.ValidateGitURL(info.CloneURL) != nil {
		return errors.New("GitHub gave an unusable clone URL")
	}
	st.Git.URL = info.CloneURL
	return nil
}

// repoToken mints a token that reads the app's repository, for one build.
func (s *Server) repoToken(ctx context.Context, a *store.App) (string, error) {
	g, err := s.db.GitSourceByID(ctx, a.Git.SourceID)
	if err != nil {
		return "", fmt.Errorf("git source: %w", err)
	}
	_, app, err := s.openGitSource(g)
	if err != nil {
		return "", err
	}
	t, err := s.githubClient().RepoToken(ctx, app, a.Git.Repo)
	if err != nil {
		return "", fmt.Errorf("GitHub: %w", err)
	}
	return t.Token, nil
}

// commitStatus tells GitHub how a deployment of a commit of an app built
// through a GitHub App went: a status on the commit ("Basalt / <app>"),
// linking to the app's page when the panel's address is set. It never
// affects the deployment: a failure (an app created before Basalt asked for
// the permission, GitHub unreachable) is logged.
func (s *Server) commitStatus(appID int64, sha, state, desc string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*githubTimeout)
	defer cancel()
	a, err := s.db.AppByID(ctx, appID)
	if err != nil || a.Source != store.SourceGit || a.Git.SourceID == 0 || a.Git.Repo == "" {
		return
	}
	g, err := s.db.GitSourceByID(ctx, a.Git.SourceID)
	if err != nil {
		return
	}
	_, app, err := s.openGitSource(g)
	if err != nil {
		s.log.Warn("commit status", "app_id", appID, "err", err)
		return
	}
	st := github.Status{State: state, Description: desc, Context: truncate("Basalt / "+a.Name, 100)}
	if panelURL, _ := s.db.Setting(ctx, settingPanelURL); panelURL != "" {
		st.TargetURL = panelURL + appPage(a)
	}
	if a.PreviewOf != 0 && state == "success" && len(a.Domains) > 0 {
		st.TargetURL = "https://" + a.Domains[0] // the preview itself
	}
	if err := s.githubClient().SetStatus(ctx, app, a.Git.Repo, sha, st); err != nil {
		s.log.Warn("set the commit status on GitHub (the GitHub App may need the Commit statuses permission)",
			"app_id", appID, "commit", sha, "err", err)
	}
}

// commitStatuses sends a deployment's commit statuses one after the other,
// in the background.
type commitStatuses struct {
	s     *Server
	appID int64
	on    bool
	last  chan struct{}
}

func (c *commitStatuses) set(sha, state, desc string) {
	if !c.on || !agentproto.ValidCommit(sha) || len(sha) != 40 {
		return
	}
	wait, done := c.last, make(chan struct{})
	c.last = done
	go func() {
		defer close(done)
		if wait != nil {
			<-wait
		}
		c.s.commitStatus(c.appID, sha, state, desc)
	}()
}

// handleGitHubWebhook deploys the apps of a repository on a push. Its
// deliveries are signed with the secret of one of the GitHub Apps.
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.hookLimiter.Allow("github:" + ip) {
		writeRateLimited(w, time.Minute)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}
	sources, err := s.db.GitSources(r.Context())
	if err != nil {
		s.internalError(w, r, "list git sources", err)
		return
	}
	var source *store.GitSource
	var event string
	for _, g := range sources {
		sec, _, err := s.openGitSource(g)
		if err != nil {
			continue
		}
		if ev, ok := verifyWebhook(r.Header, body, []byte(sec.WebhookSecret)); ok {
			source, event = g, ev
			break
		}
	}
	if source == nil {
		if !s.ipLimiter.Allow("ip:" + ip) {
			writeRateLimited(w, time.Minute)
			return
		}
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	if event == "pull_request" {
		var pr pullRequest
		if err := json.Unmarshal(body, &pr); err != nil || !github.ValidRepo(pr.Repository.FullName) || pr.Number <= 0 {
			writeError(w, http.StatusBadRequest, "invalid payload")
			return
		}
		// Removing a preview waits for its deployment to stop: GitHub does not wait.
		go func() {
			if _, err := s.pullRequestEvent(source, pr); err != nil {
				s.log.Warn("pull request", "repo", pr.Repository.FullName, "pr", pr.Number, "err", err)
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "pull request " + pr.Action})
		return
	}
	if event != "push" {
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored: " + event})
		return
	}
	var push struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &push); err != nil || !github.ValidRepo(push.Repository.FullName) {
		writeError(w, http.StatusBadRequest, "invalid payload")
		return
	}
	branch, ok := strings.CutPrefix(push.Ref, "refs/heads/")
	if !ok || strings.Trim(push.After, "0") == "" {
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored: not a branch push"})
		return
	}
	apps, err := s.db.AppsOfRepo(r.Context(), source.ID, push.Repository.FullName)
	if err != nil {
		s.internalError(w, r, "list apps", err)
		return
	}
	commit := push.After
	if !agentproto.ValidCommit(commit) {
		commit = ""
	}
	deployed := []string{}
	for _, a := range apps {
		// Previews deploy on their pull request's events.
		if a.Git.Branch != branch || !a.AutoDeploy || a.PreviewOf != 0 {
			continue
		}
		queued, err := s.deployPush(a.ID, commit)
		switch {
		case err != nil:
			s.log.Warn("deploy on push", "app_id", a.ID, "err", err)
		case queued:
			deployed = append(deployed, a.Name+" (queued)")
		default:
			deployed = append(deployed, a.Name)
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"result": "push to " + push.Repository.FullName + " " + branch, "deploying": deployed})
}
