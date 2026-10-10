package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

// defaultImagesKept is how many images of recent successful deployments an
// app keeps on its server for fast rollbacks; older ones are removed.
const defaultImagesKept = 3

type registryView struct {
	ID        int64     `json:"id"`
	Host      string    `json:"host"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func viewRegistry(r *store.Registry) registryView {
	return registryView{ID: r.ID, Host: r.Host, Username: r.Username, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

func registryAD(host string) []byte { return []byte("registry:" + host) }

// normalizeRegistryHost accepts "ghcr.io", "https://ghcr.io/", "host:5000"
// and Docker Hub's aliases, and returns the host as image references name it.
func normalizeRegistryHost(s string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(s))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h = strings.TrimSuffix(h, "/")
	if strings.HasSuffix(h, "/v1") || strings.HasSuffix(h, "/v2") {
		h = h[:len(h)-3]
	}
	switch h {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "hub.docker.com":
		return docker.DockerHub, nil
	}
	name, port, hasPort := strings.Cut(h, ":")
	if hasPort {
		if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
			return "", errors.New("registry port is invalid")
		}
	}
	if name != "localhost" {
		if _, err := proxy.NormalizeDomain(name); err != nil {
			return "", errors.New("registry must be a host name such as ghcr.io or registry.example.com:5000")
		}
	}
	// Must be how an image reference names it.
	if docker.RegistryHost(h+"/x") != h {
		return "", errors.New("registry must be a host name such as ghcr.io or registry.example.com:5000")
	}
	return h, nil
}

func validateRegistryCredentials(username, secret string) error {
	if username == "" || len(username) > 256 || strings.ContainsAny(username, "\x00\r\n") {
		return errors.New("username is required (at most 256 characters)")
	}
	if secret == "" || len(secret) > 4096 || strings.ContainsAny(secret, "\x00\r\n") {
		return errors.New("password or token is required (at most 4096 characters)")
	}
	return nil
}

func (s *Server) handleListRegistries(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.db.Registries(r.Context())
	if err != nil {
		s.internalError(w, r, "list registries", err)
		return
	}
	out := make([]registryView, len(list))
	for i, reg := range list {
		out[i] = viewRegistry(reg)
	}
	writeJSON(w, http.StatusOK, map[string]any{"registries": out})
}

type registryRequest struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleCreateRegistry(w http.ResponseWriter, r *http.Request, p *principal) {
	var req registryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	host, err := normalizeRegistryHost(req.Host)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	username := strings.TrimSpace(req.Username)
	if err := validateRegistryCredentials(username, req.Password); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	reg, err := s.db.CreateRegistry(r.Context(), host, username, s.box.Seal([]byte(req.Password), registryAD(host)))
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "credentials for this registry already exist; edit them instead")
		return
	}
	if err != nil {
		s.internalError(w, r, "create registry", err)
		return
	}
	s.audit(r, &p.user.ID, "registry.created", map[string]any{"registry_id": reg.ID, "host": host, "username": username})
	writeJSON(w, http.StatusCreated, map[string]any{"registry": viewRegistry(reg)})
}

func (s *Server) pathRegistry(w http.ResponseWriter, r *http.Request) (*store.Registry, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		reg, err := s.db.RegistryByID(r.Context(), id)
		if err == nil {
			return reg, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "load registry", err)
			return nil, false
		}
	}
	writeError(w, http.StatusNotFound, "registry not found")
	return nil, false
}

// handleUpdateRegistry replaces the credentials; the host cannot change.
func (s *Server) handleUpdateRegistry(w http.ResponseWriter, r *http.Request, p *principal) {
	reg, ok := s.pathRegistry(w, r)
	if !ok {
		return
	}
	var req registryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Host != "" {
		if h, err := normalizeRegistryHost(req.Host); err != nil || h != reg.Host {
			writeError(w, http.StatusUnprocessableEntity, "the registry host cannot be changed; add a new entry instead")
			return
		}
	}
	username := strings.TrimSpace(req.Username)
	if err := validateRegistryCredentials(username, req.Password); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := s.db.UpdateRegistry(r.Context(), reg.ID, username, s.box.Seal([]byte(req.Password), registryAD(reg.Host))); err != nil {
		s.internalError(w, r, "update registry", err)
		return
	}
	s.audit(r, &p.user.ID, "registry.updated", map[string]any{"registry_id": reg.ID, "host": reg.Host, "username": username})
	reg.Username = username
	writeJSON(w, http.StatusOK, map[string]any{"registry": viewRegistry(reg)})
}

func (s *Server) handleDeleteRegistry(w http.ResponseWriter, r *http.Request, p *principal) {
	reg, ok := s.pathRegistry(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteRegistry(r.Context(), reg.ID); err != nil {
		s.internalError(w, r, "delete registry", err)
		return
	}
	s.audit(r, &p.user.ID, "registry.deleted", map[string]any{"registry_id": reg.ID, "host": reg.Host})
	w.WriteHeader(http.StatusNoContent)
}

// registryAuth returns the stored credentials for the registry of image,
// or nil when there are none.
func (s *Server) registryAuth(ctx context.Context, image string) (*docker.AuthConfig, error) {
	host := docker.RegistryHost(image)
	reg, err := s.db.RegistryByHost(ctx, host)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	secret, err := s.box.Open(reg.Secret, registryAD(host))
	if err != nil {
		return nil, err
	}
	return &docker.AuthConfig{Username: reg.Username, Password: string(secret), ServerAddress: docker.ServerAddress(host)}, nil
}

// imagesToPrune picks, from an app's successful deployments (newest
// first), the images to remove after a new deployment: all but the keep
// most recent, the new one counting as one of them. An image is identified
// by its digest (a moving tag such as "latest" names different images over
// time), or by its reference for local images without one.
func imagesToPrune(succeeded []agentproto.ImageRef, keep int) []agentproto.ImageRef {
	key := func(r agentproto.ImageRef) string {
		if r.Digest != "" {
			return r.Digest
		}
		return r.Ref
	}
	for i := range succeeded {
		// Local image IDs (no registry digest) are pruned by reference.
		if !strings.Contains(succeeded[i].Digest, "@sha256:") {
			succeeded[i].Digest = ""
		}
	}
	keep = max(keep-1, 0)
	if len(succeeded) <= keep {
		return nil
	}
	seen := map[string]bool{}
	for _, r := range succeeded[:keep] {
		seen[key(r)] = true
	}
	var out []agentproto.ImageRef
	for _, r := range succeeded[keep:] {
		if seen[key(r)] || len(out) == agentproto.MaxPruneImages {
			continue
		}
		seen[key(r)] = true
		out = append(out, r)
	}
	return out
}
