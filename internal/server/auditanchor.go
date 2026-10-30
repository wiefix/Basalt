package server

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/store"
)

// Anchors of the audit log outside its database: every agent keeps the
// newest entry (number and hash) the panel gave it, and answers with what
// it held before (agentproto.AuditAnchor). The hash chain shows a changed
// or removed entry inside the log; an anchor shows a log that lost its
// end, or was replaced by an older copy, which the chain alone cannot.

// auditAnchorEvery is how often new entries are given to the agents.
const auditAnchorEvery = time.Minute

// auditAnchorView is what a server's agent holds of the log, as checked
// last.
type auditAnchorView struct {
	ServerID  int64     `json:"server_id"`
	Server    string    `json:"server"`
	Entry     int64     `json:"entry"`       // the newest entry it holds
	At        time.Time `json:"at,omitzero"` // when it was given that entry
	OK        bool      `json:"ok"`          // the log has that entry, unchanged
	Problem   string    `json:"problem,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// auditAnchors is the last check of each server's anchor.
type auditAnchors struct {
	mu       sync.Mutex
	byServer map[int64]auditAnchorView
	reported map[string]bool // problems notified (server/entry)
}

// AnchorAudit gives a server's agent the newest entry of the audit log
// and checks what it held (run when an agent connects).
func (s *Server) AnchorAudit(ctx context.Context, serverID int64) {
	if _, err := s.anchorAudit(ctx, serverID, false); err != nil && ctx.Err() == nil {
		s.log.Warn("anchor audit log", "server_id", serverID, "err", err)
	}
}

func (s *Server) anchorAudit(ctx context.Context, serverID int64, replace bool) (*auditAnchorView, error) {
	id, hash, err := s.db.AuditHead(ctx)
	if err != nil || id == 0 {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var resp agentproto.AuditAnchorResponse
	req := agentproto.AuditAnchor{ID: id, Hash: hex.EncodeToString(hash), Replace: replace}
	if err := s.agents.Call(ctx, serverID, agentproto.TypeAuditAnchor, req, &resp); err != nil {
		return nil, err
	}
	now := s.now()
	v := auditAnchorView{ServerID: serverID, Server: fmt.Sprintf("server #%d", serverID), Entry: id, At: now, OK: true, CheckedAt: now}
	if srv, err := s.db.ServerByID(ctx, serverID); err == nil {
		v.Server = srv.Name
	}
	if b := resp.Before; b.ID != 0 && !replace {
		have, err := s.db.AuditHash(ctx, b.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			v.OK = false
			v.Problem = fmt.Sprintf("the log ends at entry #%d, but this server was given entry #%d on %s: the entries after #%d were removed "+
				"(or the panel's database was restored from an older copy)", id, b.ID, resp.At.Format(time.DateOnly), id)
		case err != nil:
			return nil, err
		case hex.EncodeToString(have) != b.Hash:
			v.OK = false
			v.Problem = fmt.Sprintf("entry #%d differs from the one this server was given on %s: the log was replaced", b.ID, resp.At.Format(time.DateOnly))
		}
		if resp.Kept {
			v.Entry, v.At = b.ID, resp.At
		}
	}
	s.anchors.mu.Lock()
	s.anchors.byServer[serverID] = v
	report := !v.OK && !s.anchors.reported[fmt.Sprintf("%d/%d", serverID, v.Entry)]
	if report {
		s.anchors.reported[fmt.Sprintf("%d/%d", serverID, v.Entry)] = true
	}
	s.anchors.mu.Unlock()
	if report {
		s.log.Error("audit log does not match its anchor", "server", v.Server, "problem", v.Problem)
		s.notify(notify.Message{Event: EventAuditBroken, At: now, Link: "/audit",
			Title: "The audit log lost entries " + v.Server + " knows of",
			Text:  "Server " + v.Server + " keeps the newest audit log entry it was given. Now " + v.Problem + ". Find out who has access to the panel's database."})
	}
	return &v, nil
}

// anchorsView lists the last check of each server's anchor.
func (s *Server) anchorsView() []auditAnchorView {
	s.anchors.mu.Lock()
	defer s.anchors.mu.Unlock()
	out := make([]auditAnchorView, 0, len(s.anchors.byServer))
	for _, v := range s.anchors.byServer {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b auditAnchorView) int { return int(a.ServerID - b.ServerID) })
	return out
}

// anchorAll gives every connected agent the newest entry (if it is newer
// than what they were given: all, when force).
func (s *Server) anchorAll(ctx context.Context, sent map[int64]int64, force, replace bool) {
	id, _, err := s.db.AuditHead(ctx)
	if err != nil || id == 0 {
		return
	}
	servers, err := s.db.ListServers(ctx)
	if err != nil {
		s.log.Warn("anchor audit log", "err", err)
		return
	}
	for _, srv := range servers {
		if !s.agents.Connected(srv.ID) || (!force && sent[srv.ID] == id) {
			continue
		}
		v, err := s.anchorAudit(ctx, srv.ID, replace)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("anchor audit log", "server_id", srv.ID, "err", err)
			}
			continue
		}
		if sent != nil && v != nil && v.OK {
			sent[srv.ID] = id
		}
	}
}

// auditAnchorLoop gives the agents new entries every auditAnchorEvery.
func (s *Server) auditAnchorLoop(ctx context.Context) {
	sent := map[int64]int64{}
	t := time.NewTicker(auditAnchorEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.anchorAll(ctx, sent, false, false)
	}
}

// handleResetAuditAnchors accepts the log as it is now: the agents take
// its newest entry even if it is older than theirs (after restoring the
// panel from a backup, say). Each agent logs what it held.
func (s *Server) handleResetAuditAnchors(w http.ResponseWriter, r *http.Request, p *principal) {
	before := s.anchorsView()
	var problems []string
	for _, v := range before {
		if !v.OK {
			problems = append(problems, v.Server+": "+v.Problem)
		}
	}
	s.audit(r, &p.user.ID, "audit.anchors_reset", map[string]any{"problems": problems})
	s.anchorAll(r.Context(), nil, true, true)
	s.anchors.mu.Lock()
	clear(s.anchors.reported)
	s.anchors.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"anchors": s.anchorsView()})
}
