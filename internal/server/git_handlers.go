package server

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/store"
)

const maxWebhookBody = 5 << 20

func deployKeyAD(appID int64) []byte { return fmt.Appendf(nil, "app-deploy-key:%d", appID) }
func webhookAD(appID int64) []byte   { return fmt.Appendf(nil, "app-webhook:%d", appID) }

// newDeployKey gives a git app a fresh ed25519 deploy key. The private key
// is stored encrypted and only ever sent to the app's server for clones.
func (s *Server) newDeployKey(ctx context.Context, app *store.App) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "basalt app "+app.Name)
	if err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " basalt-" + app.Name
	sealed := s.box.Seal(pem.EncodeToMemory(blk), deployKeyAD(app.ID))
	if err := s.db.SetAppDeployKey(ctx, app.ID, sealed, line); err != nil {
		return err
	}
	app.DeployKey, app.DeployKeyPublic = sealed, line
	return nil
}

// newWebhookSecret replaces the secret webhooks are verified with.
func (s *Server) newWebhookSecret(ctx context.Context, app *store.App, autoDeploy bool) error {
	var b [32]byte
	rand.Read(b[:])
	sealed := s.box.Seal([]byte(hex.EncodeToString(b[:])), webhookAD(app.ID))
	if err := s.db.SetAppWebhook(ctx, app.ID, sealed, autoDeploy); err != nil {
		return err
	}
	app.WebhookSecret, app.AutoDeploy = sealed, autoDeploy
	return nil
}

func (s *Server) gitApp(w http.ResponseWriter, r *http.Request) (*store.App, bool) {
	app, ok := s.pathApp(w, r)
	if ok && app.Source != store.SourceGit {
		writeError(w, http.StatusNotFound, "this app is not built from a git repository")
		return nil, false
	}
	return app, ok
}

// handleRegenerateDeployKey replaces the deploy key; the old one stops
// working once removed from the repository.
func (s *Server) handleRegenerateDeployKey(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.gitApp(w, r)
	if !ok {
		return
	}
	if err := s.newDeployKey(r.Context(), app); err != nil {
		s.internalError(w, r, "create deploy key", err)
		return
	}
	s.audit(r, &p.user.ID, "app.deploy_key_regenerated", map[string]any{"app_id": app.ID})
	if err := s.connectForge(r.Context(), app); err != nil {
		writeError(w, http.StatusBadGateway, "new key made, but "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"app": viewApp(app)})
}

type webhookView struct {
	Path       string `json:"path"` // appended to the panel's address
	Secret     string `json:"secret"`
	AutoDeploy bool   `json:"auto_deploy"`
}

func (s *Server) webhookView(app *store.App) (webhookView, error) {
	v := webhookView{Path: "/api/v1/webhooks/apps/" + strconv.FormatInt(app.ID, 10), AutoDeploy: app.AutoDeploy}
	if app.WebhookSecret != nil {
		b, err := s.box.Open(app.WebhookSecret, webhookAD(app.ID))
		if err != nil {
			return v, err
		}
		v.Secret = string(b)
	}
	return v, nil
}

// handleGetWebhook shows the webhook URL path and secret (admins only).
func (s *Server) handleGetWebhook(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.gitApp(w, r)
	if !ok {
		return
	}
	v, err := s.webhookView(app)
	if err != nil {
		s.internalError(w, r, "decrypt webhook secret", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhook": v})
}

type webhookRequest struct {
	AutoDeploy bool `json:"auto_deploy"`
	// Regenerate replaces the secret.
	Regenerate bool `json:"regenerate"`
}

func (s *Server) handleSetWebhook(w http.ResponseWriter, r *http.Request, p *principal) {
	app, ok := s.gitApp(w, r)
	if !ok {
		return
	}
	var req webhookRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var err error
	if req.Regenerate || app.WebhookSecret == nil {
		err = s.newWebhookSecret(r.Context(), app, req.AutoDeploy)
	} else {
		err = s.db.SetAppWebhook(r.Context(), app.ID, app.WebhookSecret, req.AutoDeploy)
		app.AutoDeploy = req.AutoDeploy
	}
	if err != nil {
		s.internalError(w, r, "update webhook", err)
		return
	}
	if req.Regenerate {
		if err := s.connectForge(r.Context(), app); err != nil {
			writeError(w, http.StatusBadGateway, "new secret made, but "+err.Error())
			return
		}
	}
	s.audit(r, &p.user.ID, "app.webhook_changed", map[string]any{
		"app_id": app.ID, "auto_deploy": req.AutoDeploy, "regenerated": req.Regenerate})
	v, err := s.webhookView(app)
	if err != nil {
		s.internalError(w, r, "decrypt webhook secret", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"webhook": v})
}

// verifyWebhook checks a delivery against the secret, as GitHub
// (X-Hub-Signature-256), Gitea, Forgejo and Gogs (X-Gitea-Signature,
// X-Gogs-Signature: hex HMAC-SHA256) and GitLab (X-Gitlab-Token) sign them.
// It returns the event name.
func verifyWebhook(h http.Header, body, secret []byte) (event string, ok bool) {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	match := func(got string) bool { return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 }
	switch {
	case h.Get("X-Hub-Signature-256") != "":
		got, _ := strings.CutPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
		ok = match(got)
	case h.Get("X-Gitea-Signature") != "":
		ok = match(h.Get("X-Gitea-Signature"))
	case h.Get("X-Gogs-Signature") != "":
		ok = match(h.Get("X-Gogs-Signature"))
	case h.Get("X-Gitlab-Token") != "":
		ok = subtle.ConstantTimeCompare([]byte(h.Get("X-Gitlab-Token")), secret) == 1
	}
	for _, k := range []string{"X-GitHub-Event", "X-Gitea-Event", "X-Gogs-Event", "X-Gitlab-Event"} {
		if v := h.Get(k); v != "" {
			event = strings.ToLower(v)
			break
		}
	}
	return event, ok
}

// handleWebhook deploys a git app when its branch receives a push. It needs
// no session: deliveries are authenticated by their signature.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if !s.hookLimiter.Allow(fmt.Sprint("app:", id)) {
		writeRateLimited(w, time.Minute)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
		return
	}
	app, err := s.db.AppByID(r.Context(), id)
	if err != nil || app.Source != store.SourceGit || app.WebhookSecret == nil {
		// The same answer as a bad signature: do not reveal which apps exist.
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	secret, err := s.box.Open(app.WebhookSecret, webhookAD(app.ID))
	if err != nil {
		s.internalError(w, r, "decrypt webhook secret", err)
		return
	}
	event, ok := verifyWebhook(r.Header, body, secret)
	if !ok {
		if !s.ipLimiter.Allow("ip:" + ip) {
			writeRateLimited(w, time.Minute)
			return
		}
		s.audit(r, nil, "app.webhook_rejected", map[string]any{"app_id": app.ID})
		writeError(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	if event == "ping" {
		writeJSON(w, http.StatusOK, map[string]string{"result": "pong"})
		return
	}
	if event != "push" && event != "push hook" {
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored: not a push"})
		return
	}
	var push struct {
		Ref   string `json:"ref"`
		After string `json:"after"`
	}
	if err := json.Unmarshal(body, &push); err != nil {
		writeError(w, http.StatusBadRequest, "invalid payload")
		return
	}
	switch {
	case push.Ref != "refs/heads/"+app.Git.Branch:
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored: another branch"})
		return
	case !app.AutoDeploy:
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored: automatic deployment is off"})
		return
	case strings.Trim(push.After, "0") == "":
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored: branch deleted"})
		return
	}
	commit := push.After
	if !agentproto.ValidCommit(commit) {
		commit = "" // build the branch head
	}
	queued, err := s.deployPush(app.ID, commit)
	var de *deployError
	if errors.As(err, &de) {
		writeError(w, de.status, de.msg)
		return
	}
	if err != nil {
		s.internalError(w, r, "start deployment", err)
		return
	}
	result := "deploying"
	if queued {
		result = "queued after the running deployment"
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"result": result, "commit": commit})
}
