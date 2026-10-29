package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/mailserver"
	"github.com/wiefix/basalt/internal/notify"
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
		if (app.Source == store.SourceDatabase && d.Digests[app.Name] == "") ||
			(app.Source == store.SourceMail && d.Digests[mailserver.Service] == "") {
			return nil, &deployError{status: http.StatusUnprocessableEntity, msg: "this deployment has no recorded image"}
		}
		if app.Source == store.SourceGit && d.CommitSHA == "" {
			return nil, &deployError{status: http.StatusUnprocessableEntity, msg: "this deployment has no recorded commit"}
		}
		prev = d
	}
	if !s.agents.Connected(app.ServerID) {
		return nil, &deployError{status: http.StatusConflict, msg: "the server is offline"}
	}
	// The primary server first, then each replica that is online; an
	// offline replica keeps running what it ran.
	targets := []deployTarget{{id: app.ServerID}}
	var skipped []string
	for _, id := range app.Replicas {
		name := fmt.Sprintf("server #%d", id)
		if srv, err := s.db.ServerByID(ctx, id); err == nil {
			name = srv.Name
		}
		if s.agents.Connected(id) {
			targets = append(targets, deployTarget{id: id, name: name})
		} else {
			skipped = append(skipped, name)
		}
	}
	if len(app.Replicas) > 0 {
		if srv, err := s.db.ServerByID(ctx, app.ServerID); err == nil {
			targets[0].name = srv.Name
		}
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
		ReadOnly:    app.ReadOnly,
		PruneImages: imagesToPrune(previous, s.imagesKept),
	}
	var composeContent []byte // compose apps: the file this deployment runs
	var warnings []string
	fetchCompose := false // a git app's compose file is to be read first
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
		cf, err := s.composeSpec(ctx, app, composeContent, pinned, &spec)
		if err != nil {
			return nil, err
		}
		warnings = cf.project.Warnings
		if len(app.Replicas) > 0 && len(spec.ComposeVolumes) > 0 {
			// An earlier file (a rollback) may have had volumes.
			return nil, &deployError{status: http.StatusUnprocessableEntity,
				msg: "the app runs on several servers: this compose file keeps data in volumes (remove the replicas first)"}
		}
	case store.SourceDatabase:
		var pinned map[string]string
		if prev != nil {
			pinned = prev.Digests
		}
		spec = agentproto.AppSpec{AppID: app.ID}
		if err := s.databaseSpec(app, pinned, &spec); err != nil {
			return nil, &deployError{status: http.StatusUnprocessableEntity, msg: err.Error(), cause: err}
		}
	case store.SourceMail:
		var pinned map[string]string
		if prev != nil {
			pinned = prev.Digests
		}
		spec = agentproto.AppSpec{AppID: app.ID}
		if err := s.mailSpec(app, pinned, &spec); err != nil {
			return nil, &deployError{status: http.StatusUnprocessableEntity, msg: err.Error(), cause: err}
		}
	case store.SourceGit:
		if spec.Git, err = s.gitSource(ctx, app); err != nil {
			return nil, err
		}
		if app.ComposeFromGit() {
			spec = agentproto.AppSpec{AppID: app.ID, Git: spec.Git}
			spec.Git.Commit = o.commit
			if prev == nil {
				// The file is read from the repository first (fetchCompose).
				fetchCompose = true
				break
			}
			if composeContent, err = s.box.Open(prev.ComposeSealed, deploymentComposeAD(prev.ID)); err != nil {
				return nil, &deployError{status: http.StatusUnprocessableEntity, msg: "this deployment has no recorded compose file", cause: err}
			}
			spec.Git.Commit = prev.CommitSHA
			cf, err := s.composeSpec(ctx, app, composeContent, prev.Digests, &spec)
			if err != nil {
				return nil, err
			}
			warnings = cf.project.Warnings
			if len(app.Replicas) > 0 && len(spec.ComposeVolumes) > 0 {
				return nil, &deployError{status: http.StatusUnprocessableEntity,
					msg: "the app runs on several servers: this compose file keeps data in volumes (remove the replicas first)"}
			}
			break
		}
		// Each build gets its own tag, so older images stay usable for rollbacks.
		var b [5]byte
		rand.Read(b[:])
		spec.Image = fmt.Sprintf("basalt/app-%d:build-%s", app.ID, hex.EncodeToString(b[:]))
		spec.Git.Commit = o.commit
		// Apps Railpack builds listen on $PORT, as on Railway.
		if app.Git.Builder == agentproto.BuilderRailpack && app.Port > 0 && spec.Env["PORT"] == "" {
			if spec.Env == nil {
				spec.Env = map[string]string{}
			}
			spec.Env["PORT"] = strconv.Itoa(app.Port)
		}
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

	if !fetchCompose {
		if err := s.checkProjectResources(ctx, app, &spec); err != nil {
			return nil, err
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
	project := app.ProjectID
	if app.Source == store.SourceMail {
		project = 0 // the mail server is in no project
	}
	if project != 0 {
		details["project_id"] = project
	}
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
	for _, name := range skipped {
		notes += "Warning: " + name + " is offline; it keeps running the previous deployment.\n"
	}
	live.append(notes)
	stop := s.trackDeployment(dep.ID)
	go s.runDeployment(targets, dep.ID, spec, live, stop, deploymentExtras{
		commitMessage: keepMessage, compose: composeContent, notes: notes, projectID: project, fetchCompose: fetchCompose})
	return dep, nil
}

// deployStop lets a running deployment be canceled.
type deployStop struct {
	ch   chan struct{}
	once sync.Once
	by   string // who canceled it
}

func (s *Server) trackDeployment(id int64) *deployStop {
	stop := &deployStop{ch: make(chan struct{})}
	s.stopsMu.Lock()
	s.stops[id] = stop
	s.stopsMu.Unlock()
	return stop
}

// stopDeployment asks a running deployment to stop; false if it is not
// running (here).
func (s *Server) stopDeployment(id int64, by string) bool {
	s.stopsMu.Lock()
	stop := s.stops[id]
	s.stopsMu.Unlock()
	if stop == nil {
		return false
	}
	stop.once.Do(func() {
		stop.by = by
		close(stop.ch)
	})
	return true
}

func (st *deployStop) stopped() bool {
	select {
	case <-st.ch:
		return true
	default:
		return false
	}
}

// deploymentExtras is what the panel adds to an agent's report.
type deploymentExtras struct {
	commitMessage string // of the commit a rollback rebuilds
	compose       []byte // the compose file deployed, kept for rollbacks
	notes         string // shown before the agent's log
	projectID     int64  // of the app, for the audit log (0: none)
	// fetchCompose: the compose file of a git app is read from its
	// repository before the deployment starts (fetchCompose).
	fetchCompose bool
}

// gitSource prepares what the agent needs to build a git app: the
// repository, the deploy key for SSH URLs and the registry credentials for
// private base images.
func (s *Server) gitSource(ctx context.Context, app *store.App) (*agentproto.GitSource, error) {
	g := &agentproto.GitSource{URL: app.Git.URL, Branch: app.Git.Branch, Dockerfile: app.Git.Dockerfile, Context: app.Git.Context,
		Builder: app.Git.Builder, ComposeFile: app.Git.ComposeFile}
	if app.Git.SourceID != 0 && !agentproto.IsSSHGitURL(g.URL) {
		// A token for this build only: it reads this repository and
		// expires within the hour.
		tok, err := s.repoToken(ctx, app)
		if err != nil {
			return nil, err
		}
		g.Token = tok
	}
	if len(app.Git.BuildEnv) > 0 {
		env, err := s.appEnv(app)
		if err != nil {
			return nil, fmt.Errorf("decrypt app env: %w", err)
		}
		g.BuildEnv = map[string]string{}
		for _, k := range app.Git.BuildEnv {
			if v, ok := env[k]; ok {
				g.BuildEnv[k] = v
			}
		}
	}
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

// fetchCompose reads the compose file of a git app from its repository,
// at the commit asked for or the head of its branch (on the app's server,
// where the deployment starts), and completes spec with it: the services,
// with the commit they are built from. The file becomes the app's (its
// Compose tab shows what the repository has, even if the deployment fails)
// and the deployment's.
func (s *Server) fetchCompose(ctx context.Context, spec *agentproto.AppSpec, x *deploymentExtras, live *liveLog) error {
	note := func(format string, a ...any) {
		line := time.Now().UTC().Format("15:04:05") + " " + fmt.Sprintf(format, a...) + "\n"
		live.append(line)
		x.notes += line
	}
	app, err := s.db.AppByID(ctx, spec.AppID)
	if err != nil {
		return fmt.Errorf("load app: %w", err)
	}
	g := spec.Git
	at := g.Branch
	if g.Commit != "" {
		at = g.Commit
	}
	note("Reading %s at %s", g.ComposeFile, at)
	resp, err := s.readComposeFile(ctx, app, g)
	if err != nil {
		return err
	}
	g.Commit = resp.Commit
	x.commitMessage = resp.CommitMessage
	cf, err := s.composeSpec(ctx, app, []byte(resp.Compose), nil, spec)
	if err != nil {
		return err
	}
	for _, w := range cf.project.Warnings {
		note("Warning: %s", w)
	}
	if err := s.checkComposeFile(ctx, app, cf); err != nil {
		return err
	}
	if err := s.saveCompose(ctx, app, cf); err != nil {
		return fmt.Errorf("store compose file: %w", err)
	}
	x.compose = []byte(resp.Compose)
	return s.checkProjectResources(ctx, app, spec)
}

// deployTarget is a server a deployment runs on; name is set when the app
// runs on several (it heads that server's part of the log).
type deployTarget struct {
	id   int64
	name string
}

func (s *Server) runDeployment(targets []deployTarget, depID int64, spec agentproto.AppSpec, live *liveLog, stop *deployStop, x deploymentExtras) {
	ctx, cancel := context.WithTimeout(context.Background(), deployTimeout)
	defer cancel()
	defer func() {
		s.stopsMu.Lock()
		delete(s.stops, depID)
		s.stopsMu.Unlock()
	}()
	// A cancellation is passed on to the agent, which undoes what it
	// started and still reports (once traffic has switched, it completes).
	ctx = agentproto.WithInterrupt(ctx, stop.ch)
	go func() {
		select {
		case <-stop.ch:
			live.append("--- Cancel requested by " + stop.by + " ---\n")
		case <-ctx.Done():
		}
	}()
	// Apps built through a git source show the deployment on the commit
	// (commitStatus finds out whether the app's repository is on one).
	statuses := &commitStatuses{s: s, appID: spec.AppID, on: spec.Git != nil}
	var resp agentproto.AppDeployResponse
	var prepErr error
	if x.fetchCompose {
		prepErr = s.fetchCompose(ctx, &spec, &x, live)
	}
	if spec.Git != nil {
		statuses.set(spec.Git.Commit, "pending", fmt.Sprintf("Deploying (#%d)", depID))
	}
	if prepErr != nil {
		resp.Error, resp.Commit = prepErr.Error(), spec.Git.Commit
		line := time.Now().UTC().Format("15:04:05") + " Deployment failed: " + resp.Error + "\n"
		live.append(line)
		x.notes += line
		targets = nil
	}
	// One server after the other: the first that fails stops the rest, so
	// they keep the previous version rather than a half-tested new one.
	var log strings.Builder
	for i, t := range targets {
		if i > 0 && stop.stopped() {
			resp.OK = false
			resp.Error = fmt.Sprintf("canceled by %s: %s and the servers after it keep the previous deployment", stop.by, t.name)
			break
		}
		if t.name != "" {
			head := "=== " + t.name + " ===\n"
			live.append(head)
			log.WriteString(head)
		}
		var r agentproto.AppDeployResponse
		err := s.agents.Stream(ctx, t.id, agentproto.TypeAppDeploy, spec, &r, func(raw json.RawMessage) error {
			var c agentproto.LogChunk
			if json.Unmarshal(raw, &c) == nil {
				live.append(c.Text)
			}
			return nil
		})
		if err != nil {
			r = agentproto.AppDeployResponse{Error: "server unreachable: " + err.Error()}
		}
		log.WriteString(r.Log)
		if i == 0 {
			resp = r
			// Replicas build exactly the commit the first server built.
			if spec.Git != nil && agentproto.ValidCommit(r.Commit) {
				g := *spec.Git
				g.Commit = r.Commit
				spec.Git = &g
			}
		}
		if !r.OK {
			resp.OK = false
			if stop.stopped() {
				r.Error = "canceled by " + stop.by
			}
			if t.name != "" {
				resp.Error = t.name + ": " + r.Error
			} else {
				resp.Error = r.Error
			}
			break
		}
	}
	resp.Log = log.String()
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
	}
	if len(spec.Services) > 0 {
		result.Digests = cleanDigests(resp.Digests)
	}
	if spec.Git != nil {
		sha := resp.Commit
		if sha == "" {
			sha = spec.Git.Commit
		}
		switch {
		case resp.OK:
			statuses.set(sha, "success", fmt.Sprintf("Deployed (#%d)", depID))
		case stop.stopped():
			statuses.set(sha, "error", fmt.Sprintf("Deployment #%d canceled by %s", depID, stop.by))
		default:
			statuses.set(sha, "failure", fmt.Sprintf("#%d failed: %s", depID, resp.Error))
		}
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
	details := map[string]any{"app_id": spec.AppID, "deployment_id": depID, "error": resp.Error}
	if x.projectID != 0 {
		details["project_id"] = x.projectID
	}
	if err := s.db.Audit(context.Background(), nil, action, "", details); err != nil {
		s.log.Error("write audit log", "err", err)
	}
	s.log.Info("deployment finished", "app_id", spec.AppID, "deployment_id", depID, "ok", resp.OK)
	if !resp.OK && !stop.stopped() {
		name, link := fmt.Sprintf("app #%d", spec.AppID), fmt.Sprintf("/apps/%d", spec.AppID)
		if app, err := s.db.AppByID(context.Background(), spec.AppID); err == nil {
			name, link = app.Name, appPage(app)
		}
		s.notify(notify.Message{Event: EventDeployFailed, Title: fmt.Sprintf("Deployment #%d of %s failed", depID, name),
			Text: truncate(resp.Error, 1000), Link: link, Project: x.projectID})
	}
	if resp.OK && spec.Mail {
		s.configureMail(context.Background())
	}
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

// checkProjectResources refuses a deployment whose containers would take
// the app's project over its memory or CPU limit (with the other apps'
// limits), or would run without a limit of their own while the project
// has one. Each server the app runs on counts.
func (s *Server) checkProjectResources(ctx context.Context, app *store.App, spec *agentproto.AppSpec) error {
	if app.Source == store.SourceMail {
		return nil // in no project
	}
	pr, err := s.db.Project(ctx, app.ProjectID)
	if err != nil {
		return err
	}
	l := pr.Limits
	if l.MemoryMB == 0 && l.CPUs == 0 {
		return nil
	}
	mem, cpus := spec.MemoryMB, spec.CPUs
	if len(spec.Services) > 0 {
		mem, cpus = 0, 0
		noMem, noCPU := false, false
		for _, svc := range spec.Services {
			mem, cpus = mem+svc.MemoryMB, cpus+svc.CPUs
			noMem, noCPU = noMem || svc.MemoryMB <= 0, noCPU || svc.CPUs <= 0
		}
		// One service without a limit: the app has none.
		if noMem {
			mem = 0
		}
		if noCPU {
			cpus = 0
		}
	}
	n := 1 + len(app.Replicas)
	usedMem, usedCPUs, err := s.db.ProjectResources(ctx, app.ProjectID, app.ID)
	if err != nil {
		return err
	}
	if err := store.CheckResources(l, usedMem, usedCPUs, mem*n, cpus*float64(n)); err != nil {
		return &deployError{status: http.StatusUnprocessableEntity, msg: err.Error(), cause: err}
	}
	return nil
}
