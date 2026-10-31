package server

import (
	"context"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/store"
)

// Retention and export of the audit log. Old entries are removed after the
// retention period (store.PruneAudit keeps the chain verifiable from the
// oldest entry kept); exports let them be archived elsewhere first.

const (
	auditRetentionMin = 90   // days: shorter would let a log be wiped quickly
	auditRetentionMax = 3650 // days
	auditExportPage   = 500
)

type auditRetentionView struct {
	Days     int       `json:"days"`   // 0: kept forever
	Pruned   int       `json:"pruned"` // entries removed so far
	PrunedAt time.Time `json:"pruned_at,omitzero"`
	FirstID  int64     `json:"first_id,omitempty"` // the oldest entry kept, after pruning
}

type auditRetentionRequest struct {
	Days int `json:"days"`
}

func (s *Server) auditRetentionView(ctx context.Context) (auditRetentionView, error) {
	days, err := s.db.AuditRetention(ctx)
	if err != nil {
		return auditRetentionView{}, err
	}
	c, err := s.db.VerifyAudit(ctx)
	if err != nil {
		return auditRetentionView{}, err
	}
	return auditRetentionView{Days: days, Pruned: c.Pruned, PrunedAt: c.PrunedAt, FirstID: c.FirstID}, nil
}

// handleAuditRetention shows how long audit entries are kept.
func (s *Server) handleAuditRetention(w http.ResponseWriter, r *http.Request, p *principal) {
	v, err := s.auditRetentionView(r.Context())
	if err != nil {
		s.internalError(w, r, "audit retention", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// handleSetAuditRetention sets how long audit entries are kept and removes
// the ones older right away (owners, with a session).
func (s *Server) handleSetAuditRetention(w http.ResponseWriter, r *http.Request, p *principal) {
	var req auditRetentionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Days != 0 && (req.Days < auditRetentionMin || req.Days > auditRetentionMax) {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("days must be 0 (keep forever) or %d to %d", auditRetentionMin, auditRetentionMax))
		return
	}
	before, err := s.db.AuditRetention(r.Context())
	if err != nil {
		s.internalError(w, r, "audit retention", err)
		return
	}
	if err := s.db.SetAuditRetention(r.Context(), req.Days); err != nil {
		s.internalError(w, r, "set audit retention", err)
		return
	}
	s.audit(r, &p.user.ID, "audit.retention_set", map[string]any{"days": req.Days, "before": before})
	if err := s.pruneAudit(r.Context()); err != nil {
		s.internalError(w, r, "prune audit log", err)
		return
	}
	s.handleAuditRetention(w, r, p)
}

// pruneAudit removes the entries older than the retention period, and
// records how many went.
func (s *Server) pruneAudit(ctx context.Context) error {
	days, err := s.db.AuditRetention(ctx)
	if err != nil || days == 0 {
		return err
	}
	cutoff := s.now().AddDate(0, 0, -days)
	n, through, err := s.db.PruneAudit(ctx, cutoff)
	if n > 0 {
		s.log.Info("pruned audit log", "entries", n, "through", through)
		if aerr := s.db.Audit(ctx, nil, "audit.pruned", "", map[string]any{
			"entries": n, "through": through, "before": cutoff.UTC().Format(time.DateOnly), "days": days,
		}); aerr != nil && err == nil {
			err = aerr
		}
	}
	return err
}

// handleAuditExport downloads the audit log (with the list's filters),
// oldest first: ?format=jsonl (default) or csv.
func (s *Server) handleAuditExport(w http.ResponseWriter, r *http.Request, p *principal) {
	s.auditExport(w, r, p, store.AuditQuery{}, "basalt-audit")
}

// handleProjectAuditExport downloads the audit log of a project's
// resources (its admins).
func (s *Server) handleProjectAuditExport(w http.ResponseWriter, r *http.Request, p *principal) {
	pr, ok := s.pathProject(w, r, p, store.RoleAdmin)
	if !ok {
		return
	}
	s.auditExport(w, r, p, store.AuditQuery{Project: pr.ID}, fmt.Sprintf("basalt-audit-project-%d", pr.ID))
}

// auditExportEntry is a line of a JSON Lines export: an entry with its
// signature (hex), so an archived copy can be compared with the log.
type auditExportEntry struct {
	auditView
	Hash string `json:"hash,omitempty"`
}

func (s *Server) auditExport(w http.ResponseWriter, r *http.Request, p *principal, q store.AuditQuery, name string) {
	format := r.URL.Query().Get("format")
	switch format {
	case "":
		format = "jsonl"
	case "jsonl", "csv":
	default:
		writeError(w, http.StatusUnprocessableEntity, "format must be jsonl or csv")
		return
	}
	q.Limit = auditExportPage
	if !s.auditFilter(w, r, &q) {
		return
	}
	if q.Before > 0 || r.URL.Query().Get("limit") != "" {
		writeError(w, http.StatusUnprocessableEntity, "an export has no paging: it holds every matching entry")
		return
	}
	q.Limit, q.Oldest = auditExportPage, true
	details := map[string]any{"format": format}
	for _, k := range []string{"actor", "action"} {
		if v := r.URL.Query().Get(k); v != "" {
			details[k] = v
		}
	}
	s.audit(r, &p.user.ID, "audit.exported", details)
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(transferDeadline))

	h := w.Header()
	if format == "csv" {
		h.Set("Content-Type", "text/csv; charset=utf-8")
	} else {
		h.Set("Content-Type", "application/x-ndjson")
	}
	file := name + "-" + s.now().UTC().Format("20060102") + "." + format
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file}))
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	cw := csv.NewWriter(w)
	enc := json.NewEncoder(w)
	if format == "csv" {
		cw.Write([]string{"id", "at", "actor_id", "actor_name", "actor_email", "action", "ip", "details", "hash"})
	}
	for {
		entries, err := s.db.AuditEntries(r.Context(), q)
		if err != nil {
			// Headers are sent; the short body tells the client it failed.
			s.log.Warn("audit export interrupted", "err", err)
			return
		}
		for _, e := range entries {
			if format == "jsonl" {
				if err := enc.Encode(auditExportEntry{viewAudit(e), hex.EncodeToString(e.Hash)}); err != nil {
					return
				}
				continue
			}
			actor := ""
			if e.ActorID != nil {
				actor = strconv.FormatInt(*e.ActorID, 10)
			}
			d, _ := json.Marshal(e.Details)
			cw.Write([]string{strconv.FormatInt(e.ID, 10), e.At.UTC().Format(time.RFC3339), actor,
				csvCell(e.ActorName), csvCell(e.ActorEmail), csvCell(e.Action), csvCell(e.IP), csvCell(string(d)),
				hex.EncodeToString(e.Hash)})
		}
		cw.Flush()
		if cw.Error() != nil || len(entries) < q.Limit {
			return
		}
		q.After = entries[len(entries)-1].ID
	}
}

// csvCell keeps a spreadsheet from running a cell as a formula.
func csvCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
