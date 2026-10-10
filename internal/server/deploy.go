package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/store"
)

// deployError is a deployment that cannot start, with the HTTP status to
// report it with.
type deployError struct {
	status int
	msg    string
	cause  error
}

func (e *deployError) Error() string { return e.msg }
func (e *deployError) Unwrap() error { return e.cause }

type deployOptions struct {
	actorID    *int64
	ip         string
	rollbackTo int64  // an earlier successful deployment to go back to
	commit     string // git apps: build this commit instead of the branch head
	trigger    string // recorded in the audit log: manual, rollback, webhook
}

// startDeployment records a deployment of app and runs it in the
// background; its output can be followed live from the moment it returns.
// A panel restart marks a deployment that was cut short as failed.
func (s *Server) startDeployment(ctx context.Context, app *store.App, o deployOptions) (*store.Deployment, error) {
	var prev *store.Deployment
	if o.rollbackTo > 0 {
		d, err := s.db.DeploymentByID(ctx, app.ID, o.rollbackTo)
		if err != nil || d.Status != "succeeded" {
			return nil, &deployError{status: http.StatusUnprocessableEntity, msg: "only a successful deployment of this app can be rolled back to"}
		}
		if app.Source == store.SourceGit && d.CommitSHA == "" {
			return nil, &deployError{status: http.StatusUnprocessableEntity, msg: "this deployment has no recorded commit"}
		}
		prev = d
	}
	if !s.agents.Connected(app.ServerID) {
		return nil, &deployError{status: http.StatusConflict, msg: "the server is offline"}
	}
	env, err := s.appEnv(app)
	if err != nil {
		return nil, fmt.Errorf("decrypt app env: %w", err)
	}
	previous, err := s.db.SucceededImages(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	spec := agentproto.AppSpec{
		AppID: app.ID, Port: app.Port, Env: env, Volumes: app.Volumes, MemoryMB: app.MemoryMB, CPUs: app.CPUs,
		PruneImages: imagesToPrune(previous, s.imagesKept),
	}
	var composeContent []byte // compose apps: the file this deployment runs
	var warnings []string
	switch app.Source {
	case store.SourceCompose:
		var pinned map[string]string
		if prev != nil {
			if composeContent, err = s.box.Open(prev.ComposeSealed, deploymentComposeAD(prev.ID)); err != nil {
				return nil, &deployError{status: http.StatusUnprocessableEntity, msg: "this deployment has no recorded compose file", cause: err}
			}
			pinned = prev.Digests
		} else if composeContent, err = s.appCompose(app); err != nil {
			return nil, fmt.Errorf("decrypt compose file: %w", err)
		}
		spec = agentproto.AppSpec{AppID: app.ID}
		if warnings, err = s.composeSpec(ctx, app, composeContent, pinned, &spec); err != nil {
			return nil, err
		}
	case store.SourceGit:
		if spec.Git, err = s.gitSource(ctx, app); err != nil {
			return nil, err
		}
		// Each build gets its own tag, so older images stay usable for rollbacks.
		var b [5]byte
		rand.Read(b[:])
		spec.Image = fmt.Sprintf("basalt/app-%d:build-%s", app.ID, hex.EncodeToString(b[:]))
		spec.Git.Commit = o.commit
		if prev != nil {
			spec.Git.Commit, spec.Git.ReuseImage = prev.CommitSHA, prev.Image
		}
	default:
		spec.Image = app.Image
		if prev != nil {
			spec.Image = prev.Image
			if docker.ValidRef(prev.ImageDigest) && strings.Contains(prev.ImageDigest, "@sha256:") {
				spec.Image = prev.ImageDigest
			}
		}
		if spec.RegistryAuth, err = s.registryAuth(ctx, spec.Image); err != nil {
			return nil, fmt.Errorf("load registry credentials: %w", err)
		}
	}

	dep, err := s.db.CreateDeployment(ctx, app.ID, spec.Image, o.actorID)
	if errors.Is(err, store.ErrDeployInProgress) {
		return nil, &deployError{http.StatusConflict, err.Error(), err}
	}
	if err != nil {
		return nil, err
	}
	spec.DeploymentID = dep.ID
	details := map[string]any{"app_id": app.ID, "deployment_id": dep.ID, "image": spec.Image, "trigger": o.trigger}
	if spec.Git != nil {
		details["commit"] = spec.Git.Commit
	}
	if err := s.db.Audit(ctx, o.actorID, "app.deploy_started", o.ip, details); err != nil {
		s.log.Error("write audit log", "err", err)
	}
	keepMessage := ""
	if prev != nil {
		keepMessage = prev.CommitMessage
	}
	live := s.live.start(dep.ID)
	notes := ""
	for _, w := range warnings {
		notes += "Warning: " + w + "\n"
	}
	live.append(notes)
	go s.runDeployment(app.ServerID, dep.ID, spec, live, deploymentExtras{
		commitMessage: keepMessage, compose: composeContent, notes: notes})
	return dep, nil
}

// deploymentExtras is what the panel adds to an agent's report.
type deploymentExtras struct {
	commitMessage string // of the commit a rollback rebuilds
	compose       []byte // the compose file deployed, kept for rollbacks
	notes         string // shown before the agent's log
}

// gitSource prepares what the agent needs to build a git app: the
// repository, the deploy key for SSH URLs and the registry credentials for
// private base images.
func (s *Server) gitSource(ctx context.Context, app *store.App) (*agentproto.GitSource, error) {
	g := &agentproto.GitSource{URL: app.Git.URL, Branch: app.Git.Branch, Dockerfile: app.Git.Dockerfile, Context: app.Git.Context}
	if agentproto.IsSSHGitURL(g.URL) && app.DeployKey != nil {
		key, err := s.box.Open(app.DeployKey, deployKeyAD(app.ID))
		if err != nil {
			return nil, fmt.Errorf("decrypt deploy key: %w", err)
		}
		g.DeployKey = string(key)
	}
	regs, err := s.db.Registries(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range regs {
		secret, err := s.box.Open(r.Secret, registryAD(r.Host))
		if err != nil {
			return nil, fmt.Errorf("decrypt registry credentials: %w", err)
		}
		g.RegistryAuths = append(g.RegistryAuths, docker.AuthConfig{
			Username: r.Username, Password: string(secret), ServerAddress: docker.ServerAddress(r.Host)})
	}
	return g, nil
}

func (s *Server) runDeployment(serverID, depID int64, spec agentproto.AppSpec, live *liveLog, x deploymentExtras) {
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()
	var resp agentproto.AppDeployResponse
	err := s.agents.Stream(ctx, serverID, agentproto.TypeAppDeploy, spec, &resp, func(raw json.RawMessage) error {
		var c agentproto.LogChunk
		if json.Unmarshal(raw, &c) == nil {
			live.append(c.Text)
		}
		return nil
	})
	if err != nil {
		resp = agentproto.AppDeployResponse{Error: "server unreachable: " + err.Error()}
	}
	if resp.CommitMessage == "" {
		resp.CommitMessage = x.commitMessage
	}
	if resp.Commit != "" && !agentproto.ValidCommit(resp.Commit) {
		resp.Commit = ""
	}
	result := store.DeploymentResult{
		OK: resp.OK, Error: resp.Error, Log: x.notes + resp.Log, ImageDigest: resp.ImageDigest,
		CommitSHA: resp.Commit, CommitMessage: truncate(resp.CommitMessage, 400),
	}
	if x.compose != nil {
		result.ComposeSealed = s.box.Seal(x.compose, deploymentComposeAD(depID))
		result.Digests = cleanDigests(resp.Digests)
	}
	// Under pendMu: a push that arrives meanwhile either sees this
	// deployment finished or is queued and started below.
	s.pendMu.Lock()
	if err := s.db.FinishDeployment(context.Background(), depID, result); err != nil {
		s.log.Error("record deployment", "err", err, "deployment_id", depID)
	}
	queued, hasQueued := s.pending[spec.AppID]
	delete(s.pending, spec.AppID)
	s.pendMu.Unlock()

	// Followers now read the outcome from the database.
	s.live.end(depID)
	action := "app.deploy_failed"
	if resp.OK {
		action = "app.deployed"
	}
	if err := s.db.Audit(context.Background(), nil, action, "", map[string]any{
		"app_id": spec.AppID, "deployment_id": depID, "error": resp.Error,
	}); err != nil {
		s.log.Error("write audit log", "err", err)
	}
	s.log.Info("deployment finished", "app_id", spec.AppID, "deployment_id", depID, "ok", resp.OK)
	if hasQueued {
		s.deployPush(spec.AppID, queued)
	}
}

// deployPush deploys a git app after a push to its branch, or queues the
// deployment while another one runs (the latest push wins).
func (s *Server) deployPush(appID int64, commit string) (queued bool, err error) {
	ctx := context.Background()
	app, err := s.db.AppByID(ctx, appID)
	if err != nil {
		return false, err
	}
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	_, err = s.startDeployment(ctx, app, deployOptions{commit: commit, trigger: "webhook"})
	if errors.Is(err, store.ErrDeployInProgress) {
		s.pending[appID] = commit
		return true, nil
	}
	return false, err
}
