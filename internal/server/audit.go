package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/store"
)

const (
	auditPageDefault = 50
	auditPageMax     = 200
)

type auditActor struct {
	ID    int64  `json:"id"`
	Name  string `json:"name,omitempty"` // empty: the user was deleted
	Email string `json:"email,omitempty"`
}

type auditView struct {
	ID      int64          `json:"id"`
	At      time.Time      `json:"at"`
	Actor   *auditActor    `json:"actor"` // null: the panel itself (schedules, webhooks)
	Action  string         `json:"action"`
	IP      string         `json:"ip,omitempty"`
	Details map[string]any `json:"details"`
}

// handleAudit lists the audit log, newest first: ?before=<id> pages,
// ?actor=<user id>|system and ?action=<prefix> filter.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, p *principal) {
	s.auditPage(w, r, store.AuditQuery{Limit: auditPageDefault})
}

// handleProjectAudit is the audit log of a project's resources, for its
// admins.
func (s *Server) handleProjectAudit(w http.ResponseWriter, r *http.Request, p *principal) {
	pr, ok := s.pathProject(w, r, p, store.RoleAdmin)
	if !ok {
		return
	}
	s.auditPage(w, r, store.AuditQuery{Limit: auditPageDefault, Project: pr.ID})
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request, q store.AuditQuery) {
	v := r.URL.Query()
	num := func(name string) (int64, bool) {
		if v.Get(name) == "" {
			return 0, true
		}
		n, err := strconv.ParseInt(v.Get(name), 10, 64)
		return n, err == nil && n > 0
	}
	var ok bool
	if q.Before, ok = num("before"); !ok {
		writeError(w, http.StatusUnprocessableEntity, "before must be an entry ID")
		return
	}
	if l, ok := num("limit"); !ok || l > auditPageMax {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("limit must be 1 to %d", auditPageMax))
		return
	} else if l > 0 {
		q.Limit = int(l)
	}
	if v.Get("actor") == "system" {
		q.System = true
	} else if q.ActorID, ok = num("actor"); !ok {
		writeError(w, http.StatusUnprocessableEntity, `actor must be a user ID or "system"`)
		return
	}
	if q.Action = v.Get("action"); len(q.Action) > 100 {
		writeError(w, http.StatusUnprocessableEntity, "action is too long")
		return
	}
	entries, err := s.db.AuditEntries(r.Context(), q)
	if err != nil {
		s.internalError(w, r, "list audit log", err)
		return
	}
	actions, err := s.db.AuditActions(r.Context(), q.Project)
	if err != nil {
		s.internalError(w, r, "list audit actions", err)
		return
	}
	out := make([]auditView, len(entries))
	for i, e := range entries {
		out[i] = auditView{ID: e.ID, At: e.At, Action: e.Action, IP: e.IP, Details: e.Details}
		if e.ActorID != nil {
			out[i].Actor = &auditActor{ID: *e.ActorID, Name: e.ActorName, Email: e.ActorEmail}
		}
	}
	// A full page may be followed by more.
	more := len(entries) == q.Limit
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "actions": actions, "more": more})
}

type auditCheckView struct {
	Entries  int       `json:"entries"`
	Legacy   int       `json:"legacy"` // written before the chain existed: not protected
	Intact   bool      `json:"intact"`
	BrokenAt int64     `json:"broken_at,omitempty"`
	At       time.Time `json:"at"`
}

// handleVerifyAudit recomputes the audit log's hash chain.
func (s *Server) handleVerifyAudit(w http.ResponseWriter, r *http.Request, p *principal) {
	c, err := s.verifyAudit(r.Context())
	if err != nil {
		s.internalError(w, r, "verify audit log", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// auditWatch remembers the last broken entry reported, so a broken chain
// is notified once.
type auditWatch struct {
	mu       sync.Mutex
	reported int64
}

func (s *Server) verifyAudit(ctx context.Context) (auditCheckView, error) {
	c, err := s.db.VerifyAudit(ctx)
	if err != nil {
		return auditCheckView{}, err
	}
	v := auditCheckView{Entries: c.Entries, Legacy: c.Legacy, Intact: c.BrokenAt == 0, BrokenAt: c.BrokenAt, At: s.now()}
	if c.BrokenAt != 0 {
		s.reportBrokenAudit(c.BrokenAt)
	}
	return v, nil
}

func (s *Server) reportBrokenAudit(id int64) {
	s.auditWatch.mu.Lock()
	seen := s.auditWatch.reported == id
	s.auditWatch.reported = id
	s.auditWatch.mu.Unlock()
	if seen {
		return
	}
	s.log.Error("audit log tampered with", "entry", id)
	s.notify(notify.Message{Event: EventAuditBroken, At: s.now(), Link: "/audit",
		Title: fmt.Sprintf("The audit log was changed at entry #%d", id),
		Text: fmt.Sprintf("Entry #%d of the audit log does not match its signature: it, or an entry before it, "+
			"was changed or removed outside the panel. Find out who has access to the panel's database.", id)})
}

// WatchAuditLog verifies the audit log's chain every s.auditCheckEvery and
// notifies when it is broken. It runs until ctx ends.
func (s *Server) WatchAuditLog(ctx context.Context) {
	t := time.NewTicker(s.auditCheckEvery)
	defer t.Stop()
	for {
		if _, err := s.verifyAudit(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("verify audit log", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
