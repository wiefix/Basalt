package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/dns"
	"github.com/wiefix/basalt/internal/proxy"
	"github.com/wiefix/basalt/internal/store"
)

type dnsZoneView struct {
	ID        int64       `json:"id"`
	Name      string      `json:"name"`
	Serial    uint32      `json:"serial"`
	Records   int         `json:"records"`
	DNSSEC    *dnssecView `json:"dnssec,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// dnssecView is what the registrar needs: the DS record (or the key).
type dnssecView struct {
	KeyTag    uint16   `json:"key_tag"`
	Algorithm int      `json:"algorithm"`
	DS        []string `json:"ds"`
	DNSKEY    string   `json:"dnskey"`
}

func viewDNSZone(z *store.DNSZone) dnsZoneView {
	v := dnsZoneView{ID: z.ID, Name: z.Name, Serial: z.Serial, Records: z.Records, CreatedAt: z.CreatedAt, UpdatedAt: z.UpdatedAt}
	if z.DNSKEY != "" {
		tag, ds, rdata, err := dns.PublicKeyInfo(z.Name, z.DNSKEY)
		if err == nil {
			v.DNSSEC = &dnssecView{KeyTag: tag, Algorithm: 13, DS: ds, DNSKEY: rdata}
		}
	}
	return v
}

func dnssecAD(zoneID int64) []byte { return []byte("dns-key:" + strconv.FormatInt(zoneID, 10)) }

func (s *Server) unsealZoneKey(zoneID int64, sealed []byte) (string, error) {
	b, err := s.box.Open(sealed, dnssecAD(zoneID))
	return string(b), err
}

type dnssecRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSetDNSSEC signs a zone (a new key) or stops signing it. A zone
// already signed keeps its key, so the DS at the registrar stays valid.
func (s *Server) handleSetDNSSEC(w http.ResponseWriter, r *http.Request, p *principal) {
	z, ok := s.pathZone(w, r)
	if !ok {
		return
	}
	var req dnssecRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Enabled == (z.DNSKEY != "") {
		writeJSON(w, http.StatusOK, map[string]any{"zone": viewDNSZone(z)})
		return
	}
	dnskey, sealed := "", []byte(nil)
	if req.Enabled {
		k, err := dns.GenerateKey(z.Name)
		if err != nil {
			s.internalError(w, r, "generate dnssec key", err)
			return
		}
		dnskey, sealed = k.DNSKEY, s.box.Seal([]byte(k.PrivateKey), dnssecAD(z.ID))
	}
	if err := s.db.SetZoneDNSSEC(r.Context(), z.ID, dnskey, sealed, s.now()); err != nil {
		s.internalError(w, r, "save dnssec key", err)
		return
	}
	s.audit(r, &p.user.ID, "dns_zone.dnssec", map[string]any{"zone_id": z.ID, "zone": z.Name, "enabled": req.Enabled})
	s.pushDNSAll(r.Context())
	z, ok = s.pathZone(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"zone": viewDNSZone(z)})
}

type dnsRecordView struct {
	ID int64 `json:"id"`
	dns.Record
}

type dnsServerView struct {
	ServerID int64  `json:"server_id"`
	Name     string `json:"name"`
	Online   bool   `json:"online"`
	// Port is the DNS port (0: this server does not serve DNS).
	Port        int    `json:"port"`
	ListenError string `json:"listen_error,omitempty"`
	IP          string `json:"ip,omitempty"`
	InSync      bool   `json:"in_sync"`
	Error       string `json:"error,omitempty"`
}

// invalidZone marks a validation failure (422), as opposed to a store error.
type invalidZone struct{ error }

func checkZone(z *dns.Zone) error {
	if err := z.Validate(); err != nil {
		return invalidZone{err}
	}
	return nil
}

// PushDNS sends all zones to one agent and records the outcome.
func (s *Server) PushDNS(ctx context.Context, serverID int64) {
	if !s.agents.Connected(serverID) {
		return
	}
	rev, zones, err := s.db.DesiredDNS(ctx, s.unsealZoneKey)
	if err != nil {
		s.log.Error("build desired dns state", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	applyErr := ""
	if err := s.agents.Call(ctx, serverID, agentproto.TypeDNSApply, agentproto.DNSApply{Revision: rev, Zones: zones}, nil); err != nil {
		applyErr = err.Error()
		s.log.Warn("dns apply failed", "server_id", serverID, "revision", rev, "err", err)
	}
	if err := s.db.SetDNSStatus(context.WithoutCancel(ctx), serverID, rev, applyErr); err != nil {
		s.log.Error("record dns status", "err", err)
	}
}

// syncAutoDNS brings the serials of zones in line with their automatic
// records and, when any changed, sends the zones to every agent.
func (s *Server) syncAutoDNS(ctx context.Context) {
	changed, err := s.db.SyncAutoDNS(ctx, s.now())
	if err != nil {
		s.log.Error("sync automatic dns records", "err", err)
		return
	}
	if changed {
		s.pushDNSAll(ctx)
	}
}

// dnsLoop catches changes no handler announces, such as a server reporting
// new addresses when it reconnects, and drops expired challenges.
func (s *Server) dnsLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := s.db.DeleteExpiredDNSChallenges(ctx, s.now()); err != nil {
				s.log.Error("delete expired dns challenges", "err", err)
			}
			s.syncAutoDNS(ctx)
		}
	}
}

// checkWildcards refuses wildcard domains that cannot get a certificate:
// a public one needs the DNS-01 challenge, which works only for domains in
// a zone Basalt serves.
func (s *Server) checkWildcards(ctx context.Context, domains []string, tls proxy.TLSMode) error {
	var names []string
	for _, d := range domains {
		if !proxy.IsWildcard(d) || tls == proxy.TLSInternal || !proxy.PublicCertEligible(d) {
			continue
		}
		if names == nil {
			zones, err := s.db.DNSZones(ctx)
			if err != nil {
				return err
			}
			names = []string{}
			for _, z := range zones {
				names = append(names, z.Name)
			}
		}
		base := strings.TrimPrefix(d, "*.")
		if dns.ZoneOf("_acme-challenge."+base, names) == "" {
			return fmt.Errorf("%s needs its DNS in Basalt for a certificate: add the zone %s under DNS first (or use an internal certificate)", d, base)
		}
	}
	return nil
}

// challengeTTL bounds how long a DNS-01 record stays if the proxy never
// removes it (the CA validates within minutes).
const challengeTTL = time.Hour

// ACMEChallenge publishes (or withdraws) a DNS-01 challenge record for a
// server's proxy. The name must be _acme-challenge.<d> for a domain d (or
// *.d) the server serves, in one of our zones; it returns once every
// connected agent has the zones.
func (s *Server) ACMEChallenge(ctx context.Context, serverID int64, c agentproto.ACMEChallenge) error {
	name := strings.TrimSuffix(strings.ToLower(c.Name), ".")
	domain, ok := strings.CutPrefix(name, "_acme-challenge.")
	if !ok || len(c.Value) == 0 || len(c.Value) > 255 || strings.ContainsAny(c.Value, "\x00\r\n") {
		return errors.New("invalid challenge record")
	}
	served := false
	for _, d := range []string{domain, "*." + domain} {
		has, err := s.db.ServerHasDomain(ctx, serverID, d)
		if err != nil {
			return err
		}
		served = served || has
	}
	if !served {
		return fmt.Errorf("%s is not a domain of this server", domain)
	}
	if !c.Present {
		if err := s.db.RemoveDNSChallenge(ctx, serverID, name, c.Value); err != nil {
			return err
		}
		s.syncAutoDNS(ctx)
		return nil
	}
	zones, err := s.db.DNSZones(ctx)
	if err != nil {
		return err
	}
	names := make([]string, len(zones))
	for i, z := range zones {
		names[i] = z.Name
	}
	if dns.ZoneOf(name, names) == "" {
		return fmt.Errorf("%s is not in a DNS zone managed by Basalt: add the zone under DNS, or use a domain whose DNS Basalt serves", domain)
	}
	if err := s.db.AddDNSChallenge(ctx, serverID, name, c.Value, s.now().Add(challengeTTL)); err != nil {
		return err
	}
	s.log.Info("dns challenge published", "server_id", serverID, "name", name)
	s.syncAutoDNS(ctx)
	return nil
}

// pushDNSAll sends the zones to every connected agent, in parallel.
func (s *Server) pushDNSAll(ctx context.Context) {
	servers, err := s.db.DNSServers(ctx)
	if err != nil {
		s.log.Error("list servers for dns push", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Go(func() { s.PushDNS(ctx, srv.ServerID) })
	}
	wg.Wait()
}

func (s *Server) handleDNSOverview(w http.ResponseWriter, r *http.Request, p *principal) {
	ctx := r.Context()
	zones, err := s.db.DNSZones(ctx)
	if err != nil {
		s.internalError(w, r, "list dns zones", err)
		return
	}
	ns, err := s.db.DNSNameservers(ctx)
	if err != nil {
		s.internalError(w, r, "load nameservers", err)
		return
	}
	rev, _, err := s.db.DesiredDNS(ctx, nil)
	if err != nil {
		s.internalError(w, r, "load dns revision", err)
		return
	}
	servers, err := s.db.DNSServers(ctx)
	if err != nil {
		s.internalError(w, r, "list dns servers", err)
		return
	}
	zv := make([]dnsZoneView, len(zones))
	for i, z := range zones {
		zv[i] = viewDNSZone(z)
	}
	sv := make([]dnsServerView, len(servers))
	for i, st := range servers {
		sv[i] = dnsServerView{ServerID: st.ServerID, Name: st.Name, Online: s.agents.Connected(st.ServerID),
			Port: st.Port, ListenError: st.ListenError, IP: st.LastIP,
			InSync: st.AppliedRevision >= rev, Error: st.Error}
	}
	writeJSON(w, http.StatusOK, map[string]any{"zones": zv, "nameservers": ns, "servers": sv})
}

type nameserversRequest struct {
	Nameservers []string `json:"nameservers"`
}

// handleSetNameservers sets the host names new zones get NS records for.
func (s *Server) handleSetNameservers(w http.ResponseWriter, r *http.Request, p *principal) {
	var req nameserversRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Nameservers) > dns.MaxNameservers {
		writeError(w, http.StatusUnprocessableEntity, "at most 8 nameservers")
		return
	}
	hosts := []string{}
	for _, h := range req.Nameservers {
		rec := dns.Record{Name: "@", Type: dns.TypeNS, Value: h}
		rec.Normalize()
		if err := rec.Validate("example.com"); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "nameserver "+strconv.Quote(h)+": "+err.Error())
			return
		}
		hosts = append(hosts, rec.Value)
	}
	if err := s.db.SetDNSNameservers(r.Context(), hosts); err != nil {
		s.internalError(w, r, "save nameservers", err)
		return
	}
	s.audit(r, &p.user.ID, "dns.nameservers_updated", map[string]any{"nameservers": hosts})
	writeJSON(w, http.StatusOK, map[string]any{"nameservers": hosts})
}

type createZoneRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleCreateZone(w http.ResponseWriter, r *http.Request, p *principal) {
	var req createZoneRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := dns.NormalizeZoneName(req.Name)
	if err := dns.ValidateZoneName(name); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	ns, err := s.db.DNSNameservers(r.Context())
	if err != nil {
		s.internalError(w, r, "load nameservers", err)
		return
	}
	records := []dns.Record{}
	for _, h := range ns {
		rec := dns.Record{Name: "@", Type: dns.TypeNS, Value: h, TTL: 86400}
		rec.Normalize()
		records = append(records, rec)
	}
	z, err := s.db.CreateDNSZone(r.Context(), name, records, checkZone, s.now())
	var invalid invalidZone
	switch {
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "this zone already exists")
		return
	case errors.As(err, &invalid):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil:
		s.internalError(w, r, "create dns zone", err)
		return
	}
	s.audit(r, &p.user.ID, "dns_zone.created", map[string]any{"zone_id": z.ID, "name": z.Name})
	s.syncAutoDNS(r.Context())
	s.pushDNSAll(r.Context())
	writeJSON(w, http.StatusCreated, map[string]any{"zone": viewDNSZone(z)})
}

func (s *Server) pathZone(w http.ResponseWriter, r *http.Request) (*store.DNSZone, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		z, err := s.db.DNSZone(r.Context(), id)
		if err == nil {
			return z, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "load dns zone", err)
			return nil, false
		}
	}
	writeError(w, http.StatusNotFound, "zone not found")
	return nil, false
}

func (s *Server) handleGetZone(w http.ResponseWriter, r *http.Request, p *principal) {
	z, ok := s.pathZone(w, r)
	if !ok {
		return
	}
	recs, err := s.db.DNSRecords(r.Context(), z.ID)
	if err != nil {
		s.internalError(w, r, "list dns records", err)
		return
	}
	out := make([]dnsRecordView, len(recs))
	for i, rec := range recs {
		out[i] = dnsRecordView{ID: rec.ID, Record: rec.Record}
	}
	auto, err := s.db.DNSZoneAuto(r.Context(), z.ID)
	if err != nil {
		s.internalError(w, r, "list automatic dns records", err)
		return
	}
	if auto == nil {
		auto = []dns.AutoRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"zone": viewDNSZone(z), "records": out, "auto": auto, "types": dns.Types})
}

func (s *Server) handleDeleteZone(w http.ResponseWriter, r *http.Request, p *principal) {
	z, ok := s.pathZone(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteDNSZone(r.Context(), z.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, "delete dns zone", err)
		return
	}
	s.audit(r, &p.user.ID, "dns_zone.deleted", map[string]any{"zone_id": z.ID, "name": z.Name})
	s.syncAutoDNS(r.Context())
	s.pushDNSAll(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// handlePutRecord creates a record (POST) or replaces one (PUT …/{rid}).
func (s *Server) handlePutRecord(w http.ResponseWriter, r *http.Request, p *principal) {
	z, ok := s.pathZone(w, r)
	if !ok {
		return
	}
	var id int64
	if v := r.PathValue("rid"); v != "" {
		var err error
		if id, err = strconv.ParseInt(v, 10, 64); err != nil || id <= 0 {
			writeError(w, http.StatusNotFound, "record not found")
			return
		}
	}
	var rec dns.Record
	if !decodeJSON(w, r, &rec) {
		return
	}
	rec.Normalize()
	if err := rec.Validate(z.Name); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	out, err := s.db.PutDNSRecord(r.Context(), z.ID, id, &rec, checkZone, s.now())
	if s.recordError(w, r, err) {
		return
	}
	event, status := "dns_record.created", http.StatusCreated
	if id != 0 {
		event, status = "dns_record.updated", http.StatusOK
	}
	s.audit(r, &p.user.ID, event, map[string]any{"zone_id": z.ID, "zone": z.Name, "record_id": out.ID,
		"name": rec.Name, "type": rec.Type, "value": rec.Value, "health_check": rec.HealthCheck})
	s.syncAutoDNS(r.Context())
	s.pushDNSAll(r.Context())
	writeJSON(w, status, map[string]any{"record": dnsRecordView{ID: out.ID, Record: out.Record}})
}

func (s *Server) handleDeleteRecord(w http.ResponseWriter, r *http.Request, p *principal) {
	z, ok := s.pathZone(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "record not found")
		return
	}
	_, err = s.db.PutDNSRecord(r.Context(), z.ID, id, nil, checkZone, s.now())
	if s.recordError(w, r, err) {
		return
	}
	s.audit(r, &p.user.ID, "dns_record.deleted", map[string]any{"zone_id": z.ID, "zone": z.Name, "record_id": id})
	s.syncAutoDNS(r.Context())
	s.pushDNSAll(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) recordError(w http.ResponseWriter, r *http.Request, err error) bool {
	var invalid invalidZone
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "record not found")
	case errors.As(err, &invalid):
		writeError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, store.ErrNoHealthCheck):
		writeError(w, http.StatusUnprocessableEntity, "the health check does not exist")
	default:
		s.internalError(w, r, "save dns record", err)
	}
	return true
}
