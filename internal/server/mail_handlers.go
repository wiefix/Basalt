package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/dns"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/mailserver"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

const (
	mailAppName = "mail"
	// mailDNSRefresh is how often the panel picks up the records the mail
	// server asks for (DKIM keys rotate; new RSA keys appear a few seconds
	// after a domain is added, so there is also one pass soon after).
	mailDNSRefresh = 10 * time.Minute
	mailDNSSoon    = 15 * time.Second
	mailTimeout    = 2 * time.Minute
)

var (
	localPartRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._+-]{0,62}[a-z0-9])?$`)
	// Names the mail server serves for each domain over HTTPS.
	mailSubdomains = []string{"mta-sts", "autoconfig", "autodiscover"}
)

// mailSpec prepares the deployment of the mail server; pinned are the
// images of the deployment rolled back to.
func (s *Server) mailSpec(app *store.App, pinned map[string]string, spec *agentproto.AppSpec) error {
	env, err := s.appEnv(app)
	if err != nil {
		return err
	}
	svc, err := mailserver.ServiceSpec(env, app.Hostname, s.mailPorts, mailserver.Limits{MemoryMB: app.MemoryMB, CPUs: app.CPUs})
	if err != nil {
		return err
	}
	if d := pinned[svc.Name]; strings.Contains(d, "@sha256:") && docker.ValidRef(d) {
		svc.Image = d
	}
	spec.Services = []agentproto.ServiceSpec{svc}
	spec.ComposeVolumes = mailserver.Volumes
	spec.Mail = true
	return nil
}

// mailClient talks to the mail server through its agent.
func (s *Server) mailClient(app *store.App) (*mailserver.Client, error) {
	env, err := s.appEnv(app)
	if err != nil {
		return nil, err
	}
	user, pass, err := mailserver.Credentials(env)
	if err != nil {
		return nil, err
	}
	return mailserver.NewClient(func(ctx context.Context, body []byte) (int, []byte, error) {
		var resp agentproto.MailAPIResponse
		err := s.agents.Call(ctx, app.ServerID, agentproto.TypeMailAPI,
			agentproto.MailAPI{AppID: app.ID, User: user, Password: pass, Body: body}, &resp)
		return resp.Status, resp.Body, err
	}), nil
}

// configureMail runs after each deployment of the mail server: settings,
// then the domains Basalt knows (recreated if the mail server lost them),
// their DNS records and the host names the proxy serves.
func (s *Server) configureMail(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, mailTimeout)
	defer cancel()
	s.mailMu.Lock()
	defer s.mailMu.Unlock()
	app, err := s.db.MailApp(ctx)
	if err != nil {
		return
	}
	c, err := s.mailClient(app)
	if err == nil {
		err = s.reconcileMailDomains(ctx, c, app)
	}
	if err == nil {
		var m *mailSettings
		if m, err = s.mailSettings(ctx); err == nil {
			err = s.applyMailSettings(ctx, c, m)
		}
	}
	if err != nil {
		s.log.Warn("configure mail server", "err", err)
		return
	}
	if err := s.refreshMailDNS(ctx, c); err != nil {
		s.log.Warn("mail dns records", "err", err)
	}
	s.syncMailRoutes(ctx, app)
	s.syncMailCert(ctx, app)
	s.refreshMailDNSSoon()
}

// mailTLSStatus is the outcome of the last certificate sync.
type mailTLSStatus struct {
	NotAfter *time.Time `json:"not_after,omitempty"`
	Error    string     `json:"error,omitempty"`
}

// syncMailCert has the agent load the proxy's certificate for the mail
// host name into the mail server (renewals included).
func (s *Server) syncMailCert(ctx context.Context, app *store.App) {
	env, err := s.appEnv(app)
	if err != nil {
		return
	}
	user, pass, err := mailserver.Credentials(env)
	if err != nil {
		return
	}
	var resp agentproto.MailCertResponse
	err = s.agents.Call(ctx, app.ServerID, agentproto.TypeMailCert,
		agentproto.MailCert{AppID: app.ID, Hostname: app.Hostname, User: user, Password: pass}, &resp)
	st := mailTLSStatus{}
	if err != nil {
		st.Error = agentErrorText(err)
	} else {
		st.NotAfter = &resp.NotAfter
		if resp.Installed {
			s.log.Info("mail certificate installed", "hostname", app.Hostname, "not_after", resp.NotAfter)
		}
	}
	s.mailTLSMu.Lock()
	s.mailTLS = st
	s.mailTLSMu.Unlock()
}

// refreshMailDNSSoon picks up records the mail server adds shortly after a
// change (keys it generates in the background).
func (s *Server) refreshMailDNSSoon() {
	time.AfterFunc(mailDNSSoon, func() {
		ctx, cancel := context.WithTimeout(context.Background(), mailTimeout)
		defer cancel()
		app, err := s.db.MailApp(ctx)
		if err != nil {
			return
		}
		if c, err := s.mailClient(app); err == nil {
			if err := s.refreshMailDNS(ctx, c); err != nil {
				s.log.Warn("mail dns records", "err", err)
			}
		}
		// The proxy may have got the certificate meanwhile.
		s.syncMailCert(ctx, app)
	})
}

// reconcileMailDomains creates domains the mail server does not have (a
// new server, or one restored without data), records their IDs and
// applies the server settings (the first domain is the default).
func (s *Server) reconcileMailDomains(ctx context.Context, c *mailserver.Client, app *store.App) error {
	remote, err := c.Domains(ctx)
	if err != nil {
		return err
	}
	ids := map[string]string{}
	for _, d := range remote {
		ids[d.Name] = d.ID
	}
	local, err := s.db.MailDomains(ctx)
	if err != nil {
		return err
	}
	defaultID := ""
	for i, d := range local {
		id := ids[d.Name]
		if id == "" {
			if id, err = c.CreateDomain(ctx, d.Name); err != nil {
				return err
			}
		} else if err := c.EnableDomain(ctx, id, true); err != nil {
			return err // it may have been turned off as the last domain
		}
		if err := c.SetCatchAll(ctx, id, catchAllAddress(d.CatchAll, d.Name)); err != nil {
			return err
		}
		if id != d.RemoteID {
			if err := s.db.SetMailDomainRemote(ctx, d.ID, id); err != nil {
				return err
			}
		}
		if i == 0 {
			defaultID = id
		}
	}
	return c.Configure(ctx, app.Hostname, defaultID)
}

// refreshMailDNS stores the records the mail server asks for and updates
// the zones.
func (s *Server) refreshMailDNS(ctx context.Context, c *mailserver.Client) error {
	remote, err := c.Domains(ctx)
	if err != nil {
		return err
	}
	local, err := s.db.MailDomains(ctx)
	if err != nil {
		return err
	}
	for _, d := range local {
		i := slices.IndexFunc(remote, func(r mailserver.Domain) bool { return r.Name == d.Name })
		if i < 0 {
			continue
		}
		if err := s.db.SetMailDomainDNS(ctx, d.ID, mailserver.ParseZone(remote[i].DNSZoneFile)); err != nil {
			return err
		}
	}
	s.syncAutoDNS(ctx)
	return nil
}

// refreshMailLoop picks up DKIM key rotations.
func (s *Server) refreshMailLoop(ctx context.Context) {
	// The first pass soon after start: agents have reconnected by then.
	t := time.NewTimer(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			t.Reset(mailDNSRefresh)
		}
		app, err := s.db.MailApp(ctx)
		if err != nil || !s.agents.Connected(app.ServerID) {
			continue
		}
		if c, err := s.mailClient(app); err == nil {
			cctx, cancel := context.WithTimeout(ctx, mailTimeout)
			if err := s.refreshMailDNS(cctx, c); err != nil {
				s.log.Warn("mail dns records", "err", err)
			}
			s.syncMailCert(cctx, app)
			cancel()
		}
		if s.mailCheckEvery > 0 {
			if last := s.lastMailChecks(ctx); last == nil || s.now().Sub(last.At) >= s.mailCheckEvery {
				cctx, cancel := context.WithTimeout(ctx, mailTimeout)
				s.runMailChecks(cctx, app)
				cancel()
			}
		}
	}
}

// settingMailChecks holds the outcome of the last deliverability check.
const settingMailChecks = "mail_checks"

type mailChecks struct {
	At      time.Time                    `json:"at"`
	Results []agentproto.MailCheckResult `json:"results"`
	Error   string                       `json:"error,omitempty"`
}

func (s *Server) lastMailChecks(ctx context.Context) *mailChecks {
	v, err := s.db.Setting(ctx, settingMailChecks)
	if err != nil || v == "" {
		return nil
	}
	var c mailChecks
	if json.Unmarshal([]byte(v), &c) != nil {
		return nil
	}
	return &c
}

// runMailChecks has the mail server's agent check deliverability (see
// mailcheck) and stores the outcome. It reports false if a check is
// already running.
func (s *Server) runMailChecks(ctx context.Context, app *store.App) (*mailChecks, bool) {
	if !s.mailCheckMu.TryLock() {
		return nil, false
	}
	defer s.mailCheckMu.Unlock()
	out := &mailChecks{At: s.now(), Results: []agentproto.MailCheckResult{}}
	req := agentproto.MailCheck{Hostname: app.Hostname, IPs: []string{}}
	srv, err := s.db.ServerByID(ctx, app.ServerID)
	if err == nil {
		for _, a := range srv.DNSAddrs() {
			req.IPs = append(req.IPs, a.String())
		}
		var domains []*store.MailDomain
		if domains, err = s.db.MailDomains(ctx); err == nil {
			for _, d := range domains {
				req.Domains = append(req.Domains, agentproto.MailCheckDomain{Name: d.Name, Records: d.DNSRecords})
			}
			var resp agentproto.MailCheckResponse
			if err = s.agents.Call(ctx, app.ServerID, agentproto.TypeMailCheck, req, &resp); err == nil {
				out.Results = nonNil(resp.Results)
			}
		}
	}
	if err != nil {
		out.Error = agentErrorText(err)
		s.log.Warn("mail deliverability check", "err", err)
	}
	b, _ := json.Marshal(out)
	if err := s.db.SetSetting(context.WithoutCancel(ctx), settingMailChecks, string(b)); err != nil {
		s.log.Warn("store mail checks", "err", err)
	}
	return out, true
}

// handleMailChecks runs the deliverability checks now.
func (s *Server) handleMailChecks(w http.ResponseWriter, r *http.Request, p *principal) {
	app, _, ok := s.mailServer(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	checks, ok := s.runMailChecks(ctx, app)
	if !ok {
		writeError(w, http.StatusConflict, "a check is already running")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": checks})
}

// syncMailRoutes points the mail server's route at its host name and, for
// each mail domain in a zone Basalt serves, at mta-sts, autoconfig and
// autodiscover (served over HTTPS; the zone gets their addresses).
func (s *Server) syncMailRoutes(ctx context.Context, app *store.App) {
	zones, err := s.db.DNSZones(ctx)
	if err != nil {
		return
	}
	names := make([]string, len(zones))
	for i, z := range zones {
		names[i] = z.Name
	}
	domains, err := s.db.MailDomains(ctx)
	if err != nil {
		return
	}
	want := []string{app.Hostname}
	for _, d := range domains {
		for _, sub := range mailSubdomains {
			if n := sub + "." + d.Name; dns.ZoneOf(n, names) != "" && !slices.Contains(want, n) {
				want = append(want, n)
			}
		}
	}
	slices.Sort(want)
	have := slices.Sorted(slices.Values(app.Domains))
	if slices.Equal(want, have) {
		return
	}
	if err := s.db.SetAppDomains(ctx, app.ID, want); err != nil {
		s.log.Warn("mail host names", "err", err)
		return
	}
	s.PushProxy(ctx, app.ServerID)
}

type mailDomainView struct {
	ID         int64        `json:"id"`
	Name       string       `json:"name"`
	DNSRecords []dns.Record `json:"dns_records"`
	// ManagedDNS: a Basalt zone holds the domain and gets the records
	// automatically; otherwise they go to the domain's DNS provider.
	ManagedDNS bool      `json:"managed_dns"`
	CatchAll   string    `json:"catch_all"`
	CreatedAt  time.Time `json:"created_at"`
}

// viewMailDomains describes mail domains with whether Basalt serves their DNS.
func (s *Server) viewMailDomains(ctx context.Context, domains []*store.MailDomain) ([]mailDomainView, error) {
	zones, err := s.db.DNSZones(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(zones))
	for i, z := range zones {
		names[i] = z.Name
	}
	out := make([]mailDomainView, len(domains))
	for i, d := range domains {
		out[i] = mailDomainView{ID: d.ID, Name: d.Name, DNSRecords: nonNil(d.DNSRecords),
			ManagedDNS: dns.ZoneOf(d.Name, names) != "", CatchAll: d.CatchAll, CreatedAt: d.CreatedAt}
	}
	return out, nil
}

func (s *Server) handleMailOverview(w http.ResponseWriter, r *http.Request, p *principal) {
	app, err := s.db.MailApp(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"server": nil, "domains": []any{}, "ports": agentproto.MailPorts})
		return
	}
	if err != nil {
		s.internalError(w, r, "load mail server", err)
		return
	}
	domains, err := s.db.MailDomains(r.Context())
	if err != nil {
		s.internalError(w, r, "list mail domains", err)
		return
	}
	out, err := s.viewMailDomains(r.Context(), domains)
	if err != nil {
		s.internalError(w, r, "list mail domains", err)
		return
	}
	settings, err := s.mailSettings(r.Context())
	if err != nil {
		s.internalError(w, r, "load mail settings", err)
		return
	}
	s.mailTLSMu.Lock()
	tls := s.mailTLS
	s.mailTLSMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"server": viewApp(app), "hostname": app.Hostname,
		"domains": out, "ports": agentproto.MailPorts, "tls": tls, "checks": s.lastMailChecks(r.Context()),
		"settings": viewMailSettings(settings)})
}

type createMailRequest struct {
	ServerID int64  `json:"server_id"`
	Hostname string `json:"hostname"`
	MemoryMB int    `json:"memory_mb"`
}

// handleCreateMail sets up the mail server on a server and deploys it.
func (s *Server) handleCreateMail(w http.ResponseWriter, r *http.Request, p *principal) {
	var req createMailRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.Hostname)), ".")
	if err := mailserver.ValidHostname(host); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if req.MemoryMB != 0 && (req.MemoryMB < 512 || req.MemoryMB > 1<<20) {
		writeError(w, http.StatusUnprocessableEntity, "give the mail server at least 512 MB")
		return
	}
	srv, err := s.db.ServerByID(r.Context(), req.ServerID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "server not found")
		return
	} else if err != nil {
		s.internalError(w, r, "load server", err)
		return
	}
	if srv.Docker == "" {
		writeError(w, http.StatusUnprocessableEntity, "Docker is not installed on this server")
		return
	}
	st := store.AppSettings{Source: store.SourceMail, Image: mailserver.Image, Port: mailserver.HTTPPort,
		Hostname: host, MemoryMB: req.MemoryMB}
	app, err := s.db.CreateApp(r.Context(), srv.ID, mailAppName, st, []string{host}, proxy.TLSAuto)
	var taken *store.ErrDomainTaken
	switch {
	case errors.Is(err, store.ErrMailServerExists):
		writeError(w, http.StatusConflict, "the mail server is already set up")
		return
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, `an app named "mail" already exists; rename it first`)
		return
	case errors.As(err, &taken):
		writeError(w, http.StatusConflict, taken.Error())
		return
	case err != nil:
		s.internalError(w, r, "create mail server", err)
		return
	}
	b, _ := json.Marshal(mailserver.NewEnv())
	sealed := s.box.Seal(b, envAD(app.ID))
	if err := s.db.SetAppEnv(r.Context(), app.ID, sealed); err != nil {
		s.internalError(w, r, "create mail server credentials", err)
		return
	}
	app.EnvSealed = sealed
	// Checks of an earlier mail server no longer apply.
	if err := s.db.SetSetting(r.Context(), settingMailChecks, ""); err != nil {
		s.log.Warn("clear mail checks", "err", err)
	}
	s.audit(r, &p.user.ID, "mail.server_created", map[string]any{"app_id": app.ID, "server_id": srv.ID, "hostname": host})
	s.PushProxy(r.Context(), app.ServerID)
	resp := map[string]any{"server": viewApp(app)}
	dep, err := s.startDeployment(r.Context(), app, deployOptions{actorID: &p.user.ID, ip: clientIP(r), trigger: "manual"})
	if err != nil {
		resp["deploy_error"] = err.Error()
	} else {
		resp["deployment"] = viewDeployment(dep, false)
	}
	writeJSON(w, http.StatusCreated, resp)
}

// mailServer loads the mail server for a request that needs it running.
func (s *Server) mailServer(w http.ResponseWriter, r *http.Request) (*store.App, *mailserver.Client, bool) {
	app, err := s.db.MailApp(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "set up the mail server first")
		return nil, nil, false
	}
	if err != nil {
		s.internalError(w, r, "load mail server", err)
		return nil, nil, false
	}
	if !s.agents.Connected(app.ServerID) {
		writeError(w, http.StatusConflict, "the mail server's server is offline")
		return nil, nil, false
	}
	c, err := s.mailClient(app)
	if err != nil {
		s.internalError(w, r, "mail server credentials", err)
		return nil, nil, false
	}
	return app, c, true
}

// mailError answers a failed mail server call: refusals are the user's to
// fix (422, 409 for duplicates), the rest is the server's (502).
func (s *Server) mailError(w http.ResponseWriter, err error) {
	var se *mailserver.SetError
	switch {
	case errors.As(err, &se) && (se.Type == "alreadyExists" || se.Type == "primaryKeyViolation"):
		writeError(w, http.StatusConflict, "already exists")
	case errors.As(err, &se):
		writeError(w, http.StatusUnprocessableEntity, se.Error())
	default:
		writeError(w, http.StatusBadGateway, agentErrorText(err))
	}
}

type mailDomainRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleCreateMailDomain(w http.ResponseWriter, r *http.Request, p *principal) {
	var req mailDomainRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name, err := proxy.NormalizeDomain(req.Name)
	if err == nil && (!strings.Contains(name, ".") || dns.ValidateZoneName(name) != nil) {
		err = errors.New("use a domain such as example.com")
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	app, c, ok := s.mailServer(w, r)
	if !ok {
		return
	}
	s.mailMu.Lock()
	defer s.mailMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	d, err := s.db.CreateMailDomain(ctx, app.ID, name, "")
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "this mail domain already exists")
		return
	} else if err != nil {
		s.internalError(w, r, "create mail domain", err)
		return
	}
	if err := s.reconcileMailDomains(ctx, c, app); err != nil {
		s.db.DeleteMailDomain(context.WithoutCancel(ctx), d.ID)
		s.mailError(w, err)
		return
	}
	if err := s.refreshMailDNS(ctx, c); err != nil {
		s.log.Warn("mail dns records", "err", err)
	}
	s.syncMailRoutes(ctx, app)
	s.refreshMailDNSSoon()
	s.audit(r, &p.user.ID, "mail.domain_created", map[string]any{"mail_domain_id": d.ID, "name": name})
	d, _ = s.db.MailDomain(ctx, d.ID)
	views, err := s.viewMailDomains(ctx, []*store.MailDomain{d})
	if err != nil {
		s.internalError(w, r, "describe mail domain", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"domain": views[0]})
}

func (s *Server) pathMailDomain(w http.ResponseWriter, r *http.Request) (*store.MailDomain, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		d, err := s.db.MailDomain(r.Context(), id)
		if err == nil {
			return d, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "load mail domain", err)
			return nil, false
		}
	}
	writeError(w, http.StatusNotFound, "mail domain not found")
	return nil, false
}

func (s *Server) handleDeleteMailDomain(w http.ResponseWriter, r *http.Request, p *principal) {
	d, ok := s.pathMailDomain(w, r)
	if !ok {
		return
	}
	app, c, ok := s.mailServer(w, r)
	if !ok {
		return
	}
	s.mailMu.Lock()
	defer s.mailMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	boxes, err := c.Mailboxes(ctx, d.RemoteID)
	if err != nil {
		s.mailError(w, err)
		return
	}
	fwds, err := c.Forwarders(ctx, d.RemoteID)
	if err != nil {
		s.mailError(w, err)
		return
	}
	if len(boxes) > 0 || len(fwds) > 0 {
		writeError(w, http.StatusConflict, "delete the domain's mailboxes and forwarders first")
		return
	}
	if d.RemoteID != "" {
		if err := s.removeMailDomain(ctx, c, app, d); err != nil {
			s.mailError(w, err)
			return
		}
	}
	if err := s.db.DeleteMailDomain(ctx, d.ID); err != nil {
		s.internalError(w, r, "delete mail domain", err)
		return
	}
	s.audit(r, &p.user.ID, "mail.domain_deleted", map[string]any{"mail_domain_id": d.ID, "name": d.Name})
	if app, err := s.db.AppByID(ctx, app.ID); err == nil {
		s.syncMailRoutes(ctx, app)
	}
	s.syncAutoDNS(ctx)
	w.WriteHeader(http.StatusNoContent)
}

// removeMailDomain deletes a domain from the mail server. The default
// domain cannot go: another one becomes the default first, and the last
// one is only turned off (Stalwart always needs a default domain).
func (s *Server) removeMailDomain(ctx context.Context, c *mailserver.Client, app *store.App, d *store.MailDomain) error {
	all, err := s.db.MailDomains(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(all, func(o *store.MailDomain) bool { return o.ID != d.ID && o.RemoteID != "" })
	if i < 0 {
		return c.EnableDomain(ctx, d.RemoteID, false)
	}
	if err := c.Configure(ctx, app.Hostname, all[i].RemoteID); err != nil {
		return err
	}
	return c.DeleteDomain(ctx, d.RemoteID)
}

func (s *Server) handleListMailboxes(w http.ResponseWriter, r *http.Request, p *principal) {
	d, ok := s.pathMailDomain(w, r)
	if !ok {
		return
	}
	_, c, ok := s.mailServer(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	boxes, err := c.Mailboxes(ctx, d.RemoteID)
	if err != nil {
		s.mailError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"mailboxes": boxes})
}

type mailboxRequest struct {
	Local    string   `json:"local"`
	Name     *string  `json:"name"`
	Password *string  `json:"password"`
	QuotaMB  *int64   `json:"quota_mb"`
	Aliases  []string `json:"aliases"`
}

// change validates a mailbox request; creating needs a password.
func (req *mailboxRequest) change(create bool) (mailserver.MailboxChange, error) {
	ch := mailserver.MailboxChange{Name: req.Name, QuotaMB: req.QuotaMB, Password: req.Password}
	if req.Password != nil && (len(*req.Password) < 10 || len(*req.Password) > 256) {
		return ch, errors.New("the password needs 10 to 256 characters")
	}
	if create && req.Password == nil {
		return ch, errors.New("a password is required")
	}
	if req.Name != nil && (len(*req.Name) > 200 || strings.ContainsAny(*req.Name, "\x00\r\n")) {
		return ch, errors.New("invalid display name")
	}
	if req.QuotaMB != nil && (*req.QuotaMB < 0 || *req.QuotaMB > 1<<24) {
		return ch, errors.New("invalid quota")
	}
	if req.Aliases != nil {
		if len(req.Aliases) > 50 {
			return ch, errors.New("at most 50 aliases")
		}
		ch.Aliases = []string{}
		for _, a := range req.Aliases {
			a = strings.ToLower(strings.TrimSpace(a))
			if !localPartRE.MatchString(a) {
				return ch, errors.New("invalid alias " + strconv.Quote(a) + ": use the part before @")
			}
			if !slices.Contains(ch.Aliases, a) {
				ch.Aliases = append(ch.Aliases, a)
			}
		}
	}
	return ch, nil
}

func (s *Server) handleCreateMailbox(w http.ResponseWriter, r *http.Request, p *principal) {
	d, ok := s.pathMailDomain(w, r)
	if !ok {
		return
	}
	var req mailboxRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	local := strings.ToLower(strings.TrimSpace(req.Local))
	if !localPartRE.MatchString(local) {
		writeError(w, http.StatusUnprocessableEntity, "the address is the part before @: letters, digits, . _ + -")
		return
	}
	ch, err := req.change(true)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	_, c, ok := s.mailServer(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	id, err := c.CreateMailbox(ctx, d.RemoteID, local, ch)
	if err != nil {
		s.mailError(w, err)
		return
	}
	s.audit(r, &p.user.ID, "mail.mailbox_created", map[string]any{"mail_domain_id": d.ID, "address": local + "@" + d.Name})
	writeJSON(w, http.StatusCreated, map[string]any{"mailbox": map[string]any{"id": id, "address": local + "@" + d.Name}})
}

func (s *Server) handleUpdateMailbox(w http.ResponseWriter, r *http.Request, p *principal) {
	d, ok := s.pathMailDomain(w, r)
	if !ok {
		return
	}
	var req mailboxRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Local != "" {
		writeError(w, http.StatusUnprocessableEntity, "the address cannot be changed")
		return
	}
	ch, err := req.change(false)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	box, c, ok := s.mailbox(w, r, d)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	if err := c.UpdateMailbox(ctx, box.ID, d.RemoteID, ch); err != nil {
		s.mailError(w, err)
		return
	}
	s.audit(r, &p.user.ID, "mail.mailbox_updated", map[string]any{"address": box.Address,
		"password": req.Password != nil, "aliases": req.Aliases, "quota_mb": req.QuotaMB})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteMailbox(w http.ResponseWriter, r *http.Request, p *principal) {
	d, ok := s.pathMailDomain(w, r)
	if !ok {
		return
	}
	box, c, ok := s.mailbox(w, r, d)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	if err := c.DeleteMailbox(ctx, box.ID); err != nil {
		s.mailError(w, err)
		return
	}
	s.audit(r, &p.user.ID, "mail.mailbox_deleted", map[string]any{"address": box.Address})
	w.WriteHeader(http.StatusNoContent)
}

// mailbox finds the mailbox {mid} of domain d (only of that domain).
func (s *Server) mailbox(w http.ResponseWriter, r *http.Request, d *store.MailDomain) (*mailserver.Mailbox, *mailserver.Client, bool) {
	_, c, ok := s.mailServer(w, r)
	if !ok {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	boxes, err := c.Mailboxes(ctx, d.RemoteID)
	if err != nil {
		s.mailError(w, err)
		return nil, nil, false
	}
	i := slices.IndexFunc(boxes, func(b mailserver.Mailbox) bool { return b.ID == r.PathValue("mid") })
	if i < 0 {
		writeError(w, http.StatusNotFound, "mailbox not found")
		return nil, nil, false
	}
	return &boxes[i], c, true
}
