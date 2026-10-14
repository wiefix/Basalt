package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/proxy"
)

// The proxy renews a certificate 30 days before it expires; one with less
// than certWarnBefore left is not being renewed.
const certWarnBefore = 14 * 24 * time.Hour

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
		for _, d := range r.Domains {
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
		out[i] = certView{Name: c.Name, Issuer: c.Issuer}
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
	t := time.NewTicker(s.certCheckEvery)
	defer t.Stop()
	for {
		s.checkCertificates(ctx, notified)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) checkCertificates(ctx context.Context, notified map[string]bool) {
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
		for _, c := range certs {
			left := c.NotAfter.Sub(now)
			if c.NotAfter.IsZero() || left >= certWarnBefore {
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
	}
}
