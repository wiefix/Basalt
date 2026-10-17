package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/buildinfo"
	"github.com/wiefix/basalt/internal/release"
	"github.com/wiefix/basalt/internal/sshsig"
	"github.com/wiefix/basalt/internal/store"
)

// Updates: the panel looks for a newer signed release, and on request has
// every server's agent install it. Agents download the binary from the
// panel and check it against the checksums signed with the release key
// built into them; all are prepared before any restarts.

const (
	updateCheckEvery = 6 * time.Hour
	releaseFileMax   = 64 << 10
	releaseBinMax    = 512 << 20
	updatePrepareMax = 15 * time.Minute
)

var errNoRelease = errors.New("no release")

// releaseInfo is the newest release found, with its signed checksums.
type releaseInfo struct {
	Version   string    `json:"version,omitempty"`
	Sums      []byte    `json:"sums,omitempty"`
	Sig       []byte    `json:"sig,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitzero"`
	Error     string    `json:"error,omitempty"`
}

// updateRun is the last update started from the panel.
type updateRun struct {
	Version    string            `json:"version"`
	State      string            `json:"state"` // downloading, preparing, restarting, done, failed
	Error      string            `json:"error,omitempty"`
	StartedAt  time.Time         `json:"started_at"`
	FinishedAt time.Time         `json:"finished_at,omitzero"`
	By         string            `json:"by"`
	Servers    []updateRunServer `json:"servers"`
}

type updateRunServer struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // pending, prepared, failed, restarting
	Error  string `json:"error,omitempty"`
}

// releaseOptions say where releases come from.
type releaseOptions struct {
	URL          string // GitHub releases page or a mirror of its layout; "" disables checks
	Key          string // signing key (authorized_keys line); "" is the Basalt release key
	AllowPrivate bool   // tests: a release server on a private address
	Version      string // tests: the panel's own version
}

const defaultReleaseKey = sshsig.ReleaseKey

func currentVersion() string { return buildinfo.Version }

type updater struct {
	mu      sync.Mutex
	running bool
}

func (s *Server) releaseURL(path string) string {
	return strings.TrimSuffix(s.rel.URL, "/") + "/" + path
}

func (s *Server) releaseClient(timeout time.Duration) *http.Client {
	return outboundClient(s.rel.AllowPrivate, timeout, func(u *url.URL) error {
		if u.Scheme != "https" && !s.rel.AllowPrivate {
			return errors.New("releases are fetched over https only")
		}
		return nil
	}, "releases come from public addresses")
}

func fetchLimited(ctx context.Context, hc *http.Client, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is too large", u)
	}
	return b, nil
}

// checkRelease looks up the latest release and records it.
func (s *Server) checkRelease(ctx context.Context) releaseInfo {
	info := releaseInfo{CheckedAt: s.now()}
	err := func() error {
		hc := s.releaseClient(time.Minute)
		sums, err := fetchLimited(ctx, hc, s.releaseURL("latest/download/"+release.SumsFile), releaseFileMax)
		if err != nil {
			return err
		}
		sig, err := fetchLimited(ctx, hc, s.releaseURL("latest/download/"+release.SigFile), releaseFileMax)
		if err != nil {
			return err
		}
		list, err := release.VerifySums(sums, sig, s.releaseKey())
		if err != nil {
			return err
		}
		ver, err := fetchLimited(ctx, hc, s.releaseURL("latest/download/"+release.VersionFile), 256)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(ver)
		if want, ok := list[release.VersionFile]; !ok || want != hex.EncodeToString(sum[:]) {
			return errors.New("the version file does not match the signed checksums (a release may be in progress; try again)")
		}
		v := strings.TrimSpace(string(ver))
		if !release.ValidVersion(v) {
			return fmt.Errorf("invalid release version %q", truncate(v, 40))
		}
		info.Version, info.Sums, info.Sig = v, sums, sig
		return nil
	}()
	if errors.Is(err, errNoRelease) {
		err = fmt.Errorf("no release published at %s yet (or the repository is not public)", s.rel.URL)
	}
	if err != nil {
		info.Error = truncate(err.Error(), 300)
		// Keep what an earlier check found.
		if prev := s.loadRelease(ctx); prev.Version != "" {
			info.Version, info.Sums, info.Sig = prev.Version, prev.Sums, prev.Sig
		}
	}
	b, _ := json.Marshal(info)
	if err := s.db.SetSetting(ctx, "update_check", string(b)); err != nil {
		s.log.Error("save update check", "err", err)
	}
	return info
}

func (s *Server) loadRelease(ctx context.Context) releaseInfo {
	var info releaseInfo
	if v, err := s.db.Setting(ctx, "update_check"); err == nil && v != "" {
		json.Unmarshal([]byte(v), &info)
	}
	return info
}

func (s *Server) loadUpdateRun(ctx context.Context) *updateRun {
	v, err := s.db.Setting(ctx, "update_run")
	if err != nil || v == "" {
		return nil
	}
	var run updateRun
	if json.Unmarshal([]byte(v), &run) != nil {
		return nil
	}
	return &run
}

func (s *Server) saveUpdateRun(run *updateRun) {
	b, _ := json.Marshal(run)
	if err := s.db.SetSetting(context.Background(), "update_run", string(b)); err != nil {
		s.log.Error("save update run", "err", err)
	}
}

func (s *Server) releaseKey() string {
	if s.rel.Key != "" {
		return s.rel.Key
	}
	return defaultReleaseKey
}

func (s *Server) version() string {
	if s.rel.Version != "" {
		return s.rel.Version
	}
	return currentVersion()
}

func (s *Server) updateLoop(ctx context.Context) {
	if s.rel.URL == "" {
		return
	}
	// A run interrupted by the panel's own restart: the restart was the
	// last step.
	if run := s.loadUpdateRun(ctx); run != nil && run.FinishedAt.IsZero() {
		run.FinishedAt = s.now()
		if run.State == "restarting" {
			run.State = "done"
		} else {
			run.State, run.Error = "failed", "interrupted by a restart of the panel"
		}
		s.saveUpdateRun(run)
	}
	t := time.NewTimer(2 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			t.Reset(updateCheckEvery)
		}
		if info := s.checkRelease(ctx); info.Error != "" && ctx.Err() == nil {
			s.log.Warn("update check", "err", info.Error)
		}
	}
}

type updateServerView struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Online    bool   `json:"online"`
	Version   string `json:"version"`
	Arch      string `json:"arch"`
	Updatable bool   `json:"updatable"`
}

func (s *Server) updatesView(ctx context.Context, info releaseInfo) (map[string]any, error) {
	servers, err := s.db.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	list := []updateServerView{}
	for _, sv := range servers {
		if !sv.Enrolled {
			continue
		}
		list = append(list, updateServerView{ID: sv.ID, Name: sv.Name, Online: s.agents.Connected(sv.ID),
			Version: sv.AgentVersion, Arch: sv.Arch, Updatable: sv.Updatable})
	}
	s.updates.mu.Lock()
	running := s.updates.running
	s.updates.mu.Unlock()
	return map[string]any{
		"enabled": s.rel.URL != "",
		"current": s.version(),
		"latest":  info.Version,
		"available": release.Newer(info.Version, s.version()) || slices.ContainsFunc(list, func(v updateServerView) bool {
			return v.Online && v.Updatable && release.Newer(info.Version, v.Version)
		}),
		"checked_at": info.CheckedAt,
		"error":      info.Error,
		"servers":    list,
		"running":    running,
		"run":        s.loadUpdateRun(ctx),
	}, nil
}

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request, p *principal) {
	v, err := s.updatesView(r.Context(), s.loadRelease(r.Context()))
	if err != nil {
		s.internalError(w, r, "list servers", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleCheckUpdates(w http.ResponseWriter, r *http.Request, p *principal) {
	if s.rel.URL == "" {
		writeError(w, http.StatusConflict, "update checks are turned off (--release-url)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	v, err := s.updatesView(r.Context(), s.checkRelease(ctx))
	if err != nil {
		s.internalError(w, r, "list servers", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleApplyUpdate updates every online server whose agent can update
// itself and runs an older version.
func (s *Server) handleApplyUpdate(w http.ResponseWriter, r *http.Request, p *principal) {
	info := s.loadRelease(r.Context())
	if info.Version == "" {
		writeError(w, http.StatusConflict, "no release found yet: check for updates first")
		return
	}
	servers, err := s.db.ListServers(r.Context())
	if err != nil {
		s.internalError(w, r, "list servers", err)
		return
	}
	var targets []*store.Server
	for _, sv := range servers {
		if sv.Enrolled && sv.Updatable && s.agents.Connected(sv.ID) && release.Newer(info.Version, sv.AgentVersion) {
			targets = append(targets, sv)
		}
	}
	if len(targets) == 0 {
		writeError(w, http.StatusConflict, "no online server can be updated to "+info.Version)
		return
	}
	s.updates.mu.Lock()
	if s.updates.running {
		s.updates.mu.Unlock()
		writeError(w, http.StatusConflict, "an update is already running")
		return
	}
	s.updates.running = true
	s.updates.mu.Unlock()

	run := &updateRun{Version: info.Version, State: "downloading", StartedAt: s.now(), By: p.user.Name}
	for _, sv := range targets {
		run.Servers = append(run.Servers, updateRunServer{ID: sv.ID, Name: sv.Name, Status: "pending"})
	}
	s.saveUpdateRun(run)
	s.audit(r, &p.user.ID, "update.started", map[string]any{"version": info.Version, "servers": len(targets)})
	go s.runUpdate(info, targets, run)
	writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
}

func (s *Server) runUpdate(info releaseInfo, targets []*store.Server, run *updateRun) {
	defer func() {
		s.updates.mu.Lock()
		s.updates.running = false
		s.updates.mu.Unlock()
	}()
	fail := func(err error) {
		run.State, run.Error, run.FinishedAt = "failed", truncate(err.Error(), 500), s.now()
		s.saveUpdateRun(run)
		s.log.Error("update failed", "version", info.Version, "err", err)
		if aerr := s.db.Audit(context.Background(), nil, "update.failed", "", map[string]any{"version": info.Version, "error": run.Error}); aerr != nil {
			s.log.Error("write audit log", "err", aerr)
		}
	}
	sums, err := release.VerifySums(info.Sums, info.Sig, s.releaseKey())
	if err != nil {
		fail(err)
		return
	}
	// The binaries the servers need, downloaded once into the directory
	// the agents fetch them from.
	archs := map[string]bool{}
	for _, sv := range targets {
		archs[sv.Arch] = true
	}
	for arch := range archs {
		if err := s.stageBinary(info.Version, release.BinaryName(arch), sums); err != nil {
			fail(err)
			return
		}
	}

	run.State = "preparing"
	s.saveUpdateRun(run)
	var wg sync.WaitGroup
	var mu sync.Mutex
	req := agentproto.UpdatePrepare{Version: info.Version, Sums: info.Sums, Sig: info.Sig}
	for i, sv := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), updatePrepareMax)
			defer cancel()
			err := s.agents.Stream(ctx, sv.ID, agentproto.TypeUpdatePrepare, req, nil, func(json.RawMessage) error { return nil })
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				run.Servers[i].Status, run.Servers[i].Error = "failed", truncate(agentErrorText(err), 300)
			} else {
				run.Servers[i].Status = "prepared"
			}
		}()
	}
	wg.Wait()
	prepared := 0
	for _, sv := range run.Servers {
		if sv.Status == "prepared" {
			prepared++
		}
	}
	if prepared == 0 {
		fail(errors.New("no server could prepare the update"))
		return
	}

	// Switch them all at once: each restarts a moment after answering, the
	// panel's own host restarts the panel too.
	run.State = "restarting"
	for i := range run.Servers {
		if run.Servers[i].Status == "prepared" {
			run.Servers[i].Status = "restarting"
		}
	}
	s.saveUpdateRun(run)
	for i, sv := range targets {
		if run.Servers[i].Status != "restarting" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := s.agents.Call(ctx, sv.ID, agentproto.TypeUpdateActivate, agentproto.UpdateActivate{Version: info.Version}, nil); err != nil {
				mu.Lock()
				run.Servers[i].Status, run.Servers[i].Error = "failed", truncate(agentErrorText(err), 300)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	run.State, run.FinishedAt = "done", s.now()
	if prepared < len(targets) {
		run.Error = fmt.Sprintf("%d of %d servers updated", prepared, len(targets))
	}
	s.saveUpdateRun(run)
	if err := s.db.Audit(context.Background(), nil, "update.finished", "", map[string]any{"version": info.Version, "servers": prepared, "of": len(targets)}); err != nil {
		s.log.Error("write audit log", "err", err)
	}
}

// stageBinary downloads a release binary into the releases directory,
// unless a copy matching the signed checksum is already there.
func (s *Server) stageBinary(version, name string, sums release.Sums) error {
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("the release has no %s", name)
	}
	dir := filepath.Join(s.dataDir, "releases", version)
	path := filepath.Join(dir, name)
	if got, err := fileSHA256(path); err == nil && got == want {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.releaseURL("download/"+version+"/"+name), nil)
	if err != nil {
		return err
	}
	resp, err := s.releaseClient(0).Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", name, resp.StatusCode)
	}
	f, err := os.CreateTemp(dir, name+".part-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, releaseBinMax+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	if n > releaseBinMax || hex.EncodeToString(h.Sum(nil)) != want {
		return fmt.Errorf("%s does not match the signed checksum", name)
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
