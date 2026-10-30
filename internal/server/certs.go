package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/proxy"
)

// The proxy renews a certificate 30 days before it expires; one with less
// than certWarnBefore left is not being renewed. A name still without a
// certificate after certMissingAfter is reported too.
const (
	certWarnBefore   = 14 * 24 * time.Hour
	certMissingAfter = 24 * time.Hour
)

// publicNames lists the names a server's proxy gets public certificates
// for: domains of its routes with automatic TLS.
func (s *Server) publicNames(ctx context.Context, serverID int64) ([]string, error) {
	routes, err := s.db.Routes(ctx, serverID)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, r := range routes {
		if r.TLS != proxy.TLSAuto {
			continue
		}
		for _, d := range r.AllDomains() {
			if proxy.PublicCertEligible(d) {
				names = append(names, d)
			}
		}
	}
	return names, nil
}

func (s *Server) certStatus(ctx context.Context, serverID int64) ([]agentproto.CertInfo, error) {
	names, err := s.publicNames(ctx, serverID)
	if err != nil || len(names) == 0 {
		return []agentproto.CertInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, appCallTimeout)
	defer cancel()
	var resp agentproto.CertStatusResponse
	if err := s.agents.Call(ctx, serverID, agentproto.TypeCertStatus, agentproto.CertStatus{Names: names}, &resp); err != nil {
		return nil, err
	}
	return resp.Certs, nil
}

type certView struct {
	Name     string     `json:"name"`
	NotAfter *time.Time `json:"not_after"` // nil: none yet
	Issuer   string     `json:"issuer,omitempty"`
	DaysLeft *int       `json:"days_left,omitempty"`
	Expiring bool       `json:"expiring"` // not renewed in time
	Error    string     `json:"error,omitempty"`
	ErrorAt  *time.Time `json:"error_at,omitempty"`
}

// handleServerCerts lists the public certificates of a server's domains.
func (s *Server) handleServerCerts(w http.ResponseWriter, r *http.Request, p *principal) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	if _, err := s.db.ServerByID(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	certs, err := s.certStatus(r.Context(), id)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	now := s.now()
	out := make([]certView, len(certs))
	for i, c := range certs {
		out[i] = certView{Name: c.Name, Issuer: c.Issuer, Error: c.Error}
		if !c.ErrorAt.IsZero() {
			at := c.ErrorAt
			out[i].ErrorAt = &at
		}
		if !c.NotAfter.IsZero() {
			na := c.NotAfter
			days := int(math.Floor(na.Sub(now).Hours() / 24))
			out[i].NotAfter, out[i].DaysLeft, out[i].Expiring = &na, &days, na.Sub(now) < certWarnBefore
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"certificates": out})
}

// WatchCertificates checks the certificates of every connected server
// every s.certCheckEvery and notifies about each one that is close to
// expiring (once per certificate). It runs until ctx ends.
func (s *Server) WatchCertificates(ctx context.Context) {
	notified := map[string]bool{}
	missing := map[string]time.Time{} // server|name -> when first seen without a certificate
	t := time.NewTicker(s.certCheckEvery)
	defer t.Stop()
	for {
		s.checkCertificates(ctx, notified, missing)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) checkCertificates(ctx context.Context, notified map[string]bool, missing map[string]time.Time) {
	servers, err := s.db.ListServers(ctx)
	if err != nil {
		return
	}
	now := s.now()
	for _, srv := range servers {
		if !srv.Enrolled || !s.agents.Connected(srv.ID) {
			continue
		}
		certs, err := s.certStatus(ctx, srv.ID)
		if err != nil {
			s.log.Warn("certificate status", "server_id", srv.ID, "err", err)
			continue
		}
		seen := map[string]bool{}
		for _, c := range certs {
			mkey := fmt.Sprintf("%d|%s", srv.ID, c.Name)
			seen[mkey] = true
			if c.NotAfter.IsZero() {
				since, ok := missing[mkey]
				if !ok {
					missing[mkey] = now
					continue
				}
				if now.Sub(since) < certMissingAfter || notified[mkey+"|missing"] {
					continue
				}
				notified[mkey+"|missing"] = true
				text := fmt.Sprintf("%s has been on %s for over a day without a certificate. Check that the domain points to "+
					"the server and that ports 80 and 443 are reachable (for a wildcard or an app on several servers: "+
					"that its zone is under DNS here).", c.Name, srv.Name)
				if c.Error != "" {
					text += "\nThe last error: " + c.Error
				}
				s.notify(notify.Message{Event: EventCertMissing, Title: "No certificate for " + c.Name, At: now,
					Link: fmt.Sprintf("/servers/%d", srv.ID), Text: text})
				continue
			}
			delete(missing, mkey)
			left := c.NotAfter.Sub(now)
			if left >= certWarnBefore {
				continue
			}
			key := fmt.Sprintf("%d|%s|%d", srv.ID, c.Name, c.NotAfter.Unix())
			if notified[key] {
				continue
			}
			notified[key] = true
			title := fmt.Sprintf("The certificate for %s expires in %d days", c.Name, int(left.Hours()/24))
			if left <= 0 {
				title = "The certificate for " + c.Name + " has expired"
			}
			s.notify(notify.Message{Event: EventCertExpiring, Title: title, At: now, Link: fmt.Sprintf("/servers/%d", srv.ID),
				Text: fmt.Sprintf("On %s, valid until %s (%s). Certificates are renewed 30 days before they expire: "+
					"check that the domain still points to the server and that ports 80 and 443 are reachable "+
					"(for a wildcard or an app on several servers: that the zone is under DNS here).",
					srv.Name, c.NotAfter.UTC().Format("2006-01-02"), c.Issuer)})
		}
		// Names gone from the server's routes are forgotten.
		for k := range missing {
			if strings.HasPrefix(k, fmt.Sprintf("%d|", srv.ID)) && !seen[k] {
				delete(missing, k)
			}
		}
	}
}
