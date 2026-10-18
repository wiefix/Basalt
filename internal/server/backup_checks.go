package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/restic"
	"github.com/wiefix/basalt/internal/store"
)

// Integrity checks of backup repositories (restic check). Each repository
// that holds backups is checked weekly: its structure, and one of
// checkParts parts of its data is read back and decrypted, a different part
// each week, so all of it is read within a year. A failed check notifies
// like a failed backup and is retried after a day.

const (
	checkEvery      = 7 * 24 * time.Hour
	checkRetryAfter = 24 * time.Hour
	checkParts      = 52
	checksKept      = 180 * 24 * time.Hour
)

// checkSubset is the part of the data a check started at now reads.
func checkSubset(now time.Time) string {
	week := now.Unix() / int64(7*24*time.Hour/time.Second)
	return fmt.Sprintf("%d/%d", week%checkParts+1, checkParts)
}

// checkDue reports whether a repository should be checked now.
func checkDue(t store.CheckTarget, now time.Time) bool {
	switch {
	case t.Last == nil:
		return true
	case t.Last.Status == "running":
		return false
	case t.Last.Status == "failed":
		return now.Sub(t.Last.CreatedAt) >= checkRetryAfter
	}
	return now.Sub(t.Last.CreatedAt) >= checkEvery
}

// nextCheck is when a repository's next scheduled check is due.
func nextCheck(t store.CheckTarget, now time.Time) time.Time {
	switch {
	case t.Last == nil:
		return now
	case t.Last.Status == "running":
		return time.Time{}
	case t.Last.Status == "failed":
		return t.Last.CreatedAt.Add(checkRetryAfter)
	}
	return t.Last.CreatedAt.Add(checkEvery)
}

type checkView struct {
	ID         int64      `json:"id"`
	StorageID  int64      `json:"storage_id"`
	ServerID   int64      `json:"server_id,omitempty"` // absent: the panel's repository
	Trigger    string     `json:"trigger"`
	Status     string     `json:"status"`
	ReadData   string     `json:"read_data,omitempty"`
	Error      string     `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Log        *string    `json:"log,omitempty"`
}

func viewCheck(c *store.BackupCheck) *checkView {
	if c == nil {
		return nil
	}
	return &checkView{ID: c.ID, StorageID: c.StorageID, ServerID: c.ServerID, Trigger: c.Trigger, Status: c.Status,
		ReadData: c.ReadData, Error: c.Error, CreatedAt: c.CreatedAt, FinishedAt: c.FinishedAt}
}

// handleBackupChecks lists the repositories holding backups with their
// latest check.
func (s *Server) handleBackupChecks(w http.ResponseWriter, r *http.Request, p *principal) {
	targets, err := s.db.BackupCheckTargets(r.Context())
	if err != nil {
		s.internalError(w, r, "list backup repositories", err)
		return
	}
	now := s.now()
	out := make([]checkRepoView, len(targets))
	for i, t := range targets {
		out[i] = checkRepoView{StorageID: t.StorageID, ServerID: t.ServerID, Server: t.ServerName, Last: viewCheck(t.Last),
			NextAt: nextCheck(t, now)}
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": out})
}

// checkRepoView is a repository holding backups and its latest check.
type checkRepoView struct {
	StorageID int64      `json:"storage_id"`
	ServerID  int64      `json:"server_id"` // 0: the panel's repository
	Server    string     `json:"server"`
	Last      *checkView `json:"last"` // null: never checked
	NextAt    time.Time  `json:"next_at,omitzero"`
}

type startCheckRequest struct {
	StorageID int64 `json:"storage_id"`
	ServerID  int64 `json:"server_id"` // 0: the panel's repository
}

func (s *Server) handleStartCheck(w http.ResponseWriter, r *http.Request, p *principal) {
	var req startCheckRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	targets, err := s.db.BackupCheckTargets(r.Context())
	if err != nil {
		s.internalError(w, r, "list backup repositories", err)
		return
	}
	found := false
	for _, t := range targets {
		found = found || (t.StorageID == req.StorageID && t.ServerID == req.ServerID)
	}
	if !found {
		writeError(w, http.StatusUnprocessableEntity, "no backups were made to this storage from there yet: nothing to check")
		return
	}
	if req.ServerID != 0 && !s.backupsAvailable(w, r, req.ServerID) {
		return
	}
	c, err := s.startCheck(r.Context(), req.StorageID, req.ServerID, "manual", &p.user.ID)
	var de *deployError
	switch {
	case errors.As(err, &de):
		writeError(w, de.status, de.msg)
		return
	case errors.Is(err, store.ErrCheckInProgress):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		s.internalError(w, r, "start backup check", err)
		return
	}
	s.audit(r, &p.user.ID, "backup.check_started", map[string]any{"check_id": c.ID, "storage_id": c.StorageID, "server_id": c.ServerID})
	writeJSON(w, http.StatusAccepted, map[string]any{"check": viewCheck(c)})
}

func (s *Server) handleGetCheck(w http.ResponseWriter, r *http.Request, p *principal) {
	id, _ := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	c, err := s.db.BackupCheck(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "check not found")
		return
	}
	if err != nil {
		s.internalError(w, r, "load backup check", err)
		return
	}
	v := viewCheck(c)
	v.Log = &c.Log
	if live := s.checkLive.get(c.ID); live != nil && c.Status == "running" {
		text := live.text()
		v.Log = &text
	}
	writeJSON(w, http.StatusOK, map[string]any{"check": v})
}

// startCheck records a check of the repository serverID keeps in a storage
// (0: the panel's) and runs it in the background.
func (s *Server) startCheck(ctx context.Context, storageID, serverID int64, trigger string, actor *int64) (*store.BackupCheck, error) {
	st, err := s.db.BackupStorageByID(ctx, storageID)
	if err != nil {
		return nil, err
	}
	var spec agentproto.BackupRepo
	var repo restic.Repo
	if serverID != 0 {
		if spec, err = s.repoFor(st, serverID); err != nil {
			return nil, err
		}
	} else {
		if _, err := s.panelRunner(nil).Version(ctx); err != nil {
			return nil, &deployError{status: http.StatusConflict, msg: err.Error()}
		}
		if repo, err = s.panelRepo(st); err != nil {
			return nil, err
		}
	}
	c := &store.BackupCheck{StorageID: st.ID, ServerID: serverID, Trigger: trigger, ReadData: checkSubset(s.now()), ActorID: actor}
	if err := s.db.CreateBackupCheck(ctx, c); err != nil {
		return nil, err
	}
	live := s.checkLive.start(c.ID)
	go s.runCheck(c, st.Name, spec, repo, live)
	return c, nil
}

func (s *Server) runCheck(c *store.BackupCheck, storage string, spec agentproto.BackupRepo, repo restic.Repo, live *liveLog) {
	ctx, cancel := context.WithTimeout(context.Background(), backupTimeout)
	defer cancel()
	var resp agentproto.VerifyResponse
	where := "the panel's backups"
	if c.ServerID != 0 {
		where = "the backups of server #" + strconv.FormatInt(c.ServerID, 10)
		if srv, err := s.db.ServerByID(ctx, c.ServerID); err == nil {
			where = "the backups of " + srv.Name
		}
		err := s.agents.Stream(ctx, c.ServerID, agentproto.TypeBackupVerify,
			agentproto.VerifyRequest{Repo: spec, ReadData: c.ReadData}, &resp, func(raw json.RawMessage) error {
				var chunk agentproto.LogChunk
				if json.Unmarshal(raw, &chunk) == nil {
					live.append(chunk.Text)
				}
				return nil
			})
		if err != nil {
			resp = agentproto.VerifyResponse{Error: "server unreachable: " + err.Error()}
		}
	} else {
		resp = s.checkPanelRepo(ctx, repo, c.ReadData, live)
	}
	if resp.OK && resp.Empty {
		// Backups were recorded here: the repository should exist.
		resp.OK = false
		resp.Error = "the repository is missing or incomplete (restic finds no config file): the backups made to it cannot be read" +
			" (if the storage was moved, they stay at the old location)"
	}
	if err := s.db.FinishBackupCheck(context.Background(), c.ID, resp.OK, truncate(resp.Error, 2000), resp.Log); err != nil {
		s.log.Error("record backup check", "err", err, "check_id", c.ID)
	}
	s.checkLive.end(c.ID)
	action := "backup.check_succeeded"
	if !resp.OK {
		action = "backup.check_failed"
		s.notify(notify.Message{Event: EventBackupFailed, Title: "Integrity check of " + where + " failed",
			Text: "Storage " + storage + ": " + truncate(resp.Error, 1000), Link: "/backups"})
	}
	if err := s.db.Audit(context.Background(), nil, action, "", map[string]any{"check_id": c.ID, "storage_id": c.StorageID,
		"server_id": c.ServerID, "error": truncate(resp.Error, 500)}); err != nil {
		s.log.Error("write audit log", "err", err)
	}
	s.log.Info("backup check finished", "check_id", c.ID, "storage_id", c.StorageID, "server_id", c.ServerID, "ok", resp.OK)
}

// checkPanelRepo checks the panel's own repository with the panel's restic.
func (s *Server) checkPanelRepo(ctx context.Context, repo restic.Repo, readData string, live *liveLog) agentproto.VerifyResponse {
	lg := &deployLogLines{}
	printf := func(format string, a ...any) {
		before := lg.Len()
		lg.printf(format, a...)
		live.append(lg.String()[before:])
	}
	s.panelRepoMu.Lock()
	defer s.panelRepoMu.Unlock()
	printf("Checking the repository and reading part %s of its data", readData)
	err := s.panelRunner(func(l string) { printf("restic: %s", l) }).Check(ctx, repo, readData)
	switch {
	case errors.Is(err, restic.ErrNoRepository):
		printf("restic finds no repository at this location")
		return agentproto.VerifyResponse{OK: true, Empty: true, Log: lg.String()}
	case err != nil:
		printf("Check failed: %v", err)
		return agentproto.VerifyResponse{Error: err.Error(), Log: lg.String()}
	}
	printf("Check finished: no errors")
	return agentproto.VerifyResponse{OK: true, Log: lg.String()}
}

// runDueChecks starts the scheduled checks of repositories whose server
// is online.
func (s *Server) runDueChecks(ctx context.Context) {
	targets, err := s.db.BackupCheckTargets(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("list backup repositories", "err", err)
		}
		return
	}
	now := s.now()
	for _, t := range targets {
		if !checkDue(t, now) {
			continue
		}
		if t.ServerID != 0 {
			if !s.agents.Connected(t.ServerID) {
				continue
			}
			if v, err := s.db.ServerRestic(ctx, t.ServerID); err != nil || v == "" {
				continue
			}
		}
		_, err := s.startCheck(ctx, t.StorageID, t.ServerID, "schedule", nil)
		var de *deployError
		if err != nil && !errors.Is(err, store.ErrCheckInProgress) && !errors.As(err, &de) {
			s.log.Error("start scheduled backup check", "err", err, "storage_id", t.StorageID, "server_id", t.ServerID)
		}
	}
	if err := s.db.DeleteBackupChecksBefore(ctx, now.Add(-checksKept)); err != nil && ctx.Err() == nil {
		s.log.Error("delete old backup checks", "err", err)
	}
}
