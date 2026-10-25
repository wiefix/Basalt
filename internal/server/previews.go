package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

// Preview deployments (20.5): an app built through a GitHub App, with a
// preview domain, gets a copy of itself for each pull request into its
// branch, at pr-<number>.<domain>. GitHub reports the pull request
// (webhook of the GitHub App, event pull_request): opened, reopened or
// pushed to — the preview is created if needed and deploys the pull
// request's head commit; closed — it is deleted with its data. Pull
// requests from forks are never built: their code is anyone's, and it
// would run on the server.

const maxPreviews = 10

type previewsRequest struct {
	// Domain under which pull requests are deployed, as pr-<number>.<domain>
	// ("": no previews). Point *.<domain> at the app's server.
	Domain string `json:"domain"`
}

// previewParent tells why pull requests of app cannot be previewed (nil: they can).
func previewParent(app *store.App) error {
	if app.Source != store.SourceGit || app.Git.SourceID == 0 || app.Git.Repo == "" {
		return errors.New("previews need an app built from a GitHub repository through a git source")
	}
	if app.PreviewOf != 0 {
		return errors.New("this app is a preview")
	}
	return nil
}

func (s *Server) handlePreviews(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	s.writePreviews(w, r, app)
}

func (s *Server) writePreviews(w http.ResponseWriter, r *http.Request, app *store.App) {
	list, err := s.db.Previews(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, "list previews", err)
		return
	}
	out := make([]appView, len(list))
	for i, pv := range list {
		out[i] = s.viewAppFull(pv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"domain": app.PreviewDomain, "previews": out, "max": maxPreviews,
		"supported": previewParent(app) == nil && s.onGitHub(r.Context(), app)})
}

// onGitHub reports whether an app is built through a GitHub App.
func (s *Server) onGitHub(ctx context.Context, app *store.App) bool {
	if app.Git.SourceID == 0 {
		return false
	}
	g, err := s.db.GitSourceByID(ctx, app.Git.SourceID)
	return err == nil && g.Kind == "github"
}

func (s *Server) handleSetPreviews(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.pathApp(w, r)
	if !ok {
		return
	}
	var req previewsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := previewParent(app); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !s.onGitHub(r.Context(), app) {
		writeError(w, http.StatusUnprocessableEntity, "previews need a repository on GitHub (pull requests reach the panel through the GitHub App)")
		return
	}
	domain := strings.TrimSpace(req.Domain)
	if domain != "" {
		n, err := proxy.NormalizeDomain(domain)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "domain: "+err.Error())
			return
		}
		domain = n
		sample := []string{"pr-1." + domain}
		if _, _, err := normalizeAppDomains(sample, app.TLS, app.Port); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if s.domainError(w, r, s.checkDomainProjects(r.Context(), sample, app.ProjectID)) {
			return
		}
	}
	if err := s.db.SetPreviewDomain(r.Context(), app.ID, domain); err != nil {
		s.internalError(w, r, "set preview domain", err)
		return
	}
	app.PreviewDomain = domain
	s.audit(r, &p.user.ID, "app.previews_set", map[string]any{"app_id": app.ID, "domain": domain})
	s.writePreviews(w, r, app)
}

// pullRequest is what the panel reads of a pull_request event.
type pullRequest struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// pullRequestEvent deploys or removes the previews of a pull request; it
// returns the apps concerned.
func (s *Server) pullRequestEvent(source *store.GitSource, pr pullRequest) ([]string, error) {
	ctx := context.Background()
	apps, err := s.db.AppsOfRepo(ctx, source.ID, pr.Repository.FullName)
	if err != nil {
		return nil, err
	}
	head := pr.PullRequest.Head
	fork := head.Repo == nil || !strings.EqualFold(head.Repo.FullName, pr.Repository.FullName)
	var out []string
	for _, a := range apps {
		if a.PreviewOf != 0 || a.PreviewDomain == "" || a.Git.Branch != pr.PullRequest.Base.Ref {
			continue
		}
		switch pr.Action {
		case "opened", "reopened", "synchronize":
			if fork {
				out = append(out, a.Name+" (not built: the pull request comes from a fork)")
				continue
			}
			pv, err := s.ensurePreview(ctx, a, pr.Number, head.Ref)
			if err != nil {
				s.log.Warn("create preview", "app_id", a.ID, "pr", pr.Number, "err", err)
				out = append(out, a.Name+" (no preview: "+err.Error()+")")
				continue
			}
			commit := head.SHA
			if !agentproto.ValidCommit(commit) {
				commit = ""
			}
			queued, err := s.deployPush(pv.ID, commit)
			switch {
			case err != nil:
				s.log.Warn("deploy preview", "app_id", pv.ID, "err", err)
				out = append(out, pv.Name+" ("+err.Error()+")")
			case queued:
				out = append(out, pv.Name+" (queued)")
			default:
				out = append(out, pv.Name)
			}
		case "closed":
			if name, ok := s.removePreview(ctx, a, pr.Number); ok {
				out = append(out, name+" (removed)")
			}
		}
	}
	return out, nil
}

// ensurePreview returns the preview of pull request number of parent,
// created if it has none, built from branch with parent's current
// settings and environment.
func (s *Server) ensurePreview(ctx context.Context, parent *store.App, number int, branch string) (*store.App, error) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	if number <= 0 {
		return nil, errors.New("invalid pull request")
	}
	g := agentproto.GitSource{URL: parent.Git.URL, Branch: branch, Dockerfile: parent.Git.Dockerfile, Context: parent.Git.Context}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	list, err := s.db.Previews(ctx, parent.ID)
	if err != nil {
		return nil, err
	}
	st := store.AppSettings{ProjectID: parent.ProjectID, Source: store.SourceGit, Git: parent.Git, Port: parent.Port,
		Volumes: parent.Volumes, MemoryMB: parent.MemoryMB, CPUs: parent.CPUs, ReadOnly: parent.ReadOnly,
		PreviewOf: parent.ID, PRNumber: number}
	st.Git.Branch = branch
	var pv *store.App
	for _, x := range list {
		if x.PRNumber == number {
			pv = x
		}
	}
	if pv == nil {
		if len(list) >= maxPreviews {
			return nil, fmt.Errorf("the app has %d previews already (close a pull request first)", maxPreviews)
		}
		suffix := fmt.Sprintf("-pr-%d", number)
		name := truncate(parent.Name, 63-len(suffix)) + suffix
		domain := fmt.Sprintf("pr-%d.%s", number, parent.PreviewDomain)
		if pv, err = s.db.CreateApp(ctx, parent.ServerID, name, st, []string{domain}, parent.TLS); err != nil {
			return nil, err
		}
		if err := s.db.Audit(ctx, nil, "app.preview_created", "", map[string]any{"app_id": pv.ID, "of": parent.ID, "pr": number,
			"domain": domain, "project_id": parent.ProjectID}); err != nil {
			s.log.Error("write audit log", "err", err)
		}
	} else if err := s.db.UpdateAppSettings(ctx, pv.ID, st); err != nil {
		return nil, err
	}
	// The parent's environment, as it is now.
	env, err := s.appEnv(parent)
	if err != nil {
		return nil, err
	}
	var sealed []byte
	if len(env) > 0 {
		b, _ := json.Marshal(env)
		sealed = s.box.Seal(b, envAD(pv.ID))
	}
	if err := s.db.SetAppEnv(ctx, pv.ID, sealed); err != nil {
		return nil, err
	}
	s.PushProxy(ctx, pv.ServerID)
	return pv, nil
}

// removePreview deletes the preview of pull request number, with its data,
// once a deployment it runs is canceled.
func (s *Server) removePreview(ctx context.Context, parent *store.App, number int) (string, bool) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	list, err := s.db.Previews(ctx, parent.ID)
	if err != nil {
		return "", false
	}
	for _, pv := range list {
		if pv.PRNumber != number {
			continue
		}
		s.pendMu.Lock()
		delete(s.pending, pv.ID)
		s.pendMu.Unlock()
		for range 600 { // up to 10 minutes
			deps, err := s.db.Deployments(ctx, pv.ID, 1)
			if err != nil || len(deps) == 0 || deps[0].Status != "running" {
				break
			}
			s.stopDeployment(deps[0].ID, "the pull request closing")
			time.Sleep(time.Second)
		}
		cleanup, err := s.deleteApp(ctx, pv, true)
		if err != nil {
			s.log.Warn("remove preview", "app_id", pv.ID, "err", err)
			return "", false
		}
		if len(cleanup) > 0 {
			s.log.Warn("remove preview", "app_id", pv.ID, "cleanup", strings.Join(cleanup, "; "))
		}
		if err := s.db.Audit(ctx, nil, "app.preview_removed", "", map[string]any{"app_id": pv.ID, "of": parent.ID, "pr": number,
			"project_id": parent.ProjectID}); err != nil {
			s.log.Error("write audit log", "err", err)
		}
		return pv.Name, true
	}
	return "", false
}
