package server

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/cloud"
	"github.com/wiefix/basalt/internal/store"
)

// Floating IPs (22.1): an admin adds a cloud provider's account (its API
// token), picks a floating IP of it and the servers of its group; every
// server of the group has the address on its interface, and the provider
// routes it to one of them, which the panel changes on request.

type cloudAccountView struct {
	ID          int64     `json:"id"`
	Provider    string    `json:"provider"`
	Name        string    `json:"name"`
	FloatingIPs int       `json:"floating_ips"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func cloudAccountAD(id int64) []byte { return []byte("cloud-account:" + strconv.FormatInt(id, 10)) }

// maxFloatingServers bounds the group of a floating IP.
const maxFloatingServers = 16

func (s *Server) cloudProvider(acc *store.CloudAccount) (cloud.Provider, error) {
	token, err := s.box.Open(acc.Secret, cloudAccountAD(acc.ID))
	if err != nil {
		return nil, err
	}
	api := ""
	if acc.Provider == cloud.Hetzner {
		api = s.hetznerAPI
	}
	return cloud.New(acc.Provider, string(token), api)
}

// providerError answers a failed call to the provider's API.
func (s *Server) providerError(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case cloud.Unauthorized(err):
		writeError(w, http.StatusBadGateway, "the provider refused the account's API token; replace it")
	case errors.Is(err, cloud.ErrNotFound):
		writeError(w, http.StatusNotFound, "the provider does not have this floating IP (any more)")
	default:
		var e *cloud.Error
		if errors.As(err, &e) || strings.HasPrefix(err.Error(), "provider API") {
			writeError(w, http.StatusBadGateway, what+": "+err.Error())
			return
		}
		s.internalError(w, r, what, err)
	}
}

func (s *Server) handleListCloudAccounts(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.db.CloudAccounts(r.Context())
	if err != nil {
		s.internalError(w, r, "list cloud accounts", err)
		return
	}
	ips, err := s.db.FloatingIPs(r.Context())
	if err != nil {
		s.internalError(w, r, "list floating IPs", err)
		return
	}
	out := make([]cloudAccountView, len(list))
	for i, a := range list {
		out[i] = cloudAccountView{ID: a.ID, Provider: a.Provider, Name: a.Name, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
		for _, f := range ips {
			if f.AccountID == a.ID {
				out[i].FloatingIPs++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

type cloudAccountRequest struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	// Token is the account's API token; on an update "" keeps it.
	Token string `json:"token"`
}

func validAccountName(name string) error {
	if name == "" || len(name) > 64 || strings.ContainsAny(name, "\x00\r\n") {
		return errors.New("name is required (at most 64 characters)")
	}
	return nil
}

// checkToken makes sure the provider takes the token.
func (s *Server) checkToken(w http.ResponseWriter, r *http.Request, provider, token string) bool {
	api := ""
	if provider == cloud.Hetzner {
		api = s.hetznerAPI
	}
	pr, err := cloud.New(provider, token, api)
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		_, err = pr.FloatingIPs(ctx)
	}
	if cloud.Unauthorized(err) {
		writeError(w, http.StatusUnprocessableEntity, "the provider refused the API token")
		return false
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "the provider's API could not be reached: "+err.Error())
		return false
	}
	return true
}

func (s *Server) handleCreateCloudAccount(w http.ResponseWriter, r *http.Request, p *principal) {
	var req cloudAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !cloud.ValidProvider(req.Provider) {
		writeError(w, http.StatusUnprocessableEntity, "provider must be hetzner")
		return
	}
	if err := validAccountName(req.Name); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := cloud.ValidToken(req.Token); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if !s.checkToken(w, r, req.Provider, req.Token) {
		return
	}
	// The token is sealed to the account's id: created with a placeholder,
	// then sealed once the id is known.
	acc, err := s.db.CreateCloudAccount(r.Context(), req.Provider, req.Name, []byte{})
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "an account with this name already exists")
		return
	}
	if err != nil {
		s.internalError(w, r, "create cloud account", err)
		return
	}
	if err := s.db.UpdateCloudAccount(r.Context(), acc.ID, acc.Name, s.box.Seal([]byte(req.Token), cloudAccountAD(acc.ID))); err != nil {
		_ = s.db.DeleteCloudAccount(r.Context(), acc.ID)
		s.internalError(w, r, "seal cloud account token", err)
		return
	}
	s.audit(r, &p.user.ID, "cloud_account.created", map[string]any{"account_id": acc.ID, "provider": acc.Provider, "name": acc.Name})
	writeJSON(w, http.StatusCreated, map[string]any{"account": cloudAccountView{ID: acc.ID, Provider: acc.Provider,
		Name: acc.Name, CreatedAt: acc.CreatedAt, UpdatedAt: acc.UpdatedAt}})
}

func (s *Server) pathCloudAccount(w http.ResponseWriter, r *http.Request) (*store.CloudAccount, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		acc, err := s.db.CloudAccount(r.Context(), id)
		if err == nil {
			return acc, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "load cloud account", err)
			return nil, false
		}
	}
	writeError(w, http.StatusNotFound, "cloud account not found")
	return nil, false
}

func (s *Server) handleUpdateCloudAccount(w http.ResponseWriter, r *http.Request, p *principal) {
	acc, ok := s.pathCloudAccount(w, r)
	if !ok {
		return
	}
	var req cloudAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Provider != "" && req.Provider != acc.Provider {
		writeError(w, http.StatusUnprocessableEntity, "the provider of an account cannot be changed")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if err := validAccountName(req.Name); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	var secret []byte
	if req.Token != "" {
		if err := cloud.ValidToken(req.Token); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		if !s.checkToken(w, r, acc.Provider, req.Token) {
			return
		}
		secret = s.box.Seal([]byte(req.Token), cloudAccountAD(acc.ID))
	}
	err := s.db.UpdateCloudAccount(r.Context(), acc.ID, req.Name, secret)
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "an account with this name already exists")
		return
	}
	if err != nil {
		s.internalError(w, r, "update cloud account", err)
		return
	}
	s.audit(r, &p.user.ID, "cloud_account.updated", map[string]any{"account_id": acc.ID, "name": req.Name, "token_replaced": secret != nil})
	acc, _ = s.db.CloudAccount(r.Context(), acc.ID)
	writeJSON(w, http.StatusOK, map[string]any{"account": cloudAccountView{ID: acc.ID, Provider: acc.Provider,
		Name: acc.Name, CreatedAt: acc.CreatedAt, UpdatedAt: acc.UpdatedAt}})
}

func (s *Server) handleDeleteCloudAccount(w http.ResponseWriter, r *http.Request, p *principal) {
	acc, ok := s.pathCloudAccount(w, r)
	if !ok {
		return
	}
	err := s.db.DeleteCloudAccount(r.Context(), acc.ID)
	if errors.Is(err, store.ErrInUse) {
		writeError(w, http.StatusConflict, "the account still has floating IPs in Basalt; remove them first")
		return
	}
	if err != nil {
		s.internalError(w, r, "delete cloud account", err)
		return
	}
	s.audit(r, &p.user.ID, "cloud_account.deleted", map[string]any{"account_id": acc.ID, "name": acc.Name})
	w.WriteHeader(http.StatusNoContent)
}

// providerServerView is a server of the provider's account, and the
// panel's server that has one of its addresses (0: none).
type providerServerView struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	ServerID   int64  `json:"server_id"`
}

type providerFloatingIPView struct {
	ProviderID string `json:"provider_id"`
	Name       string `json:"name"`
	Address    string `json:"address"`
	Network    string `json:"network"`
	Location   string `json:"location"`
	// AssignedTo is the provider's server it is routed to ("": none).
	AssignedTo string `json:"assigned_to"`
	// FloatingIPID is the panel's floating IP for it (0: not added).
	FloatingIPID int64 `json:"floating_ip_id"`
}

// serverAddrs are every address the panel knows a server by.
func serverAddrs(srv *store.Server) []netip.Addr {
	var out []netip.Addr
	for _, v := range append(append(slices.Clone(srv.PublicIPs), srv.DetectedIPs...), srv.LastIP) {
		if a, err := netip.ParseAddr(v); err == nil {
			out = append(out, a.Unmap())
		}
	}
	return out
}

// matchServers finds, for each of the panel's servers, the provider's
// server with one of its addresses.
func matchServers(servers []*store.Server, theirs []cloud.Server) map[int64]cloud.Server {
	out := map[int64]cloud.Server{}
	for _, srv := range servers {
		for _, a := range serverAddrs(srv) {
			if i := slices.IndexFunc(theirs, func(c cloud.Server) bool { return c.Matches(a) }); i >= 0 {
				out[srv.ID] = theirs[i]
				break
			}
		}
	}
	return out
}

// handleProviderFloatingIPs lists what the account has: its floating IPs
// and its servers (with the panel's servers they are).
func (s *Server) handleProviderFloatingIPs(w http.ResponseWriter, r *http.Request, p *principal) {
	acc, ok := s.pathCloudAccount(w, r)
	if !ok {
		return
	}
	pr, err := s.cloudProvider(acc)
	if err != nil {
		s.internalError(w, r, "cloud account", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ips, err := pr.FloatingIPs(ctx)
	if err != nil {
		s.providerError(w, r, "list floating IPs", err)
		return
	}
	theirs, err := pr.Servers(ctx)
	if err != nil {
		s.providerError(w, r, "list servers", err)
		return
	}
	servers, err := s.db.ListServers(r.Context())
	if err != nil {
		s.internalError(w, r, "list servers", err)
		return
	}
	added, err := s.db.FloatingIPs(r.Context())
	if err != nil {
		s.internalError(w, r, "list floating IPs", err)
		return
	}
	match := matchServers(servers, theirs)
	outServers := make([]providerServerView, len(theirs))
	for i, c := range theirs {
		outServers[i] = providerServerView{ProviderID: c.ID, Name: c.Name}
		for id, m := range match {
			if m.ID == c.ID {
				outServers[i].ServerID = id
			}
		}
	}
	outIPs := make([]providerFloatingIPView, len(ips))
	for i, f := range ips {
		outIPs[i] = providerFloatingIPView{ProviderID: f.ID, Name: f.Name, Address: f.Address, Network: f.Network,
			Location: f.Location, AssignedTo: f.ServerID}
		for _, a := range added {
			if a.AccountID == acc.ID && a.ProviderID == f.ID {
				outIPs[i].FloatingIPID = a.ID
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"floating_ips": outIPs, "servers": outServers})
}

type floatingServerView struct {
	ServerID         int64  `json:"server_id"`
	Name             string `json:"name"`
	ProviderServerID string `json:"provider_server_id"`
	Online           bool   `json:"online"`
	// Error is why the server could not put its floating IPs on its
	// interface ("": it did, or has not been told yet).
	Error string `json:"error"`
}

type floatingIPView struct {
	ID          int64  `json:"id"`
	AccountID   int64  `json:"account_id"`
	AccountName string `json:"account_name"`
	Provider    string `json:"provider"`
	ProviderID  string `json:"provider_id"`
	Name        string `json:"name"`
	Address     string `json:"address"`
	Network     string `json:"network"`
	Location    string `json:"location"`
	// ServerID is the server of the group the provider routes it to, as
	// the panel last saw (0: none of them).
	ServerID   int64                `json:"server_id"`
	AssignedAt *time.Time           `json:"assigned_at"`
	Servers    []floatingServerView `json:"servers"`
	CreatedAt  time.Time            `json:"created_at"`
}

func (s *Server) viewFloatingIPs(ctx context.Context, only int64) ([]floatingIPView, error) {
	list, err := s.db.FloatingIPs(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := s.db.CloudAccounts(ctx)
	if err != nil {
		return nil, err
	}
	servers, err := s.db.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	errs, err := s.db.ServerFloatingErrors(ctx)
	if err != nil {
		return nil, err
	}
	names := map[int64]string{}
	for _, srv := range servers {
		names[srv.ID] = srv.Name
	}
	out := []floatingIPView{}
	for _, f := range list {
		if only != 0 && f.ID != only {
			continue
		}
		v := floatingIPView{ID: f.ID, AccountID: f.AccountID, ProviderID: f.ProviderID, Name: f.Name, Address: f.Address,
			Network: f.Network, Location: f.Location, ServerID: f.ServerID, AssignedAt: f.AssignedAt,
			Servers: []floatingServerView{}, CreatedAt: f.CreatedAt}
		if i := slices.IndexFunc(accounts, func(a *store.CloudAccount) bool { return a.ID == f.AccountID }); i >= 0 {
			v.AccountName, v.Provider = accounts[i].Name, accounts[i].Provider
		}
		for _, m := range f.Servers {
			v.Servers = append(v.Servers, floatingServerView{ServerID: m.ServerID, Name: names[m.ServerID],
				ProviderServerID: m.ProviderServerID, Online: s.agents.Connected(m.ServerID), Error: errs[m.ServerID]})
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Server) handleListFloatingIPs(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.viewFloatingIPs(r.Context(), 0)
	if err != nil {
		s.internalError(w, r, "list floating IPs", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"floating_ips": list})
}

// writeFloatingIP answers with one floating IP.
func (s *Server) writeFloatingIP(w http.ResponseWriter, r *http.Request, status int, id int64) {
	list, err := s.viewFloatingIPs(r.Context(), id)
	if err != nil || len(list) == 0 {
		s.internalError(w, r, "load floating IP", errors.Join(err, store.ErrNotFound))
		return
	}
	writeJSON(w, status, map[string]any{"floating_ip": list[0]})
}

type addFloatingIPRequest struct {
	AccountID  int64  `json:"account_id"`
	ProviderID string `json:"provider_id"`
	// ServerIDs is the group, in the order failover tries them.
	ServerIDs []int64 `json:"server_ids"`
}

type floatingServersRequest struct {
	ServerIDs []int64 `json:"server_ids"`
}

// floatingGroup checks a group of servers and finds them at the provider;
// it answers the request itself when it cannot.
func (s *Server) floatingGroup(w http.ResponseWriter, r *http.Request, pr cloud.Provider, ids []int64) ([]store.FloatingIPServer, bool) {
	if len(ids) == 0 || len(ids) > maxFloatingServers {
		writeError(w, http.StatusUnprocessableEntity, "choose 1 to 16 servers")
		return nil, false
	}
	var servers []*store.Server
	for _, id := range ids {
		srv, err := s.db.ServerByID(r.Context(), id)
		if errors.Is(err, store.ErrNotFound) || err == nil && !srv.Enrolled {
			writeError(w, http.StatusUnprocessableEntity, "server "+strconv.FormatInt(id, 10)+" not found")
			return nil, false
		}
		if err != nil {
			s.internalError(w, r, "load server", err)
			return nil, false
		}
		if slices.ContainsFunc(servers, func(o *store.Server) bool { return o.ID == id }) {
			writeError(w, http.StatusUnprocessableEntity, "a server is given twice")
			return nil, false
		}
		servers = append(servers, srv)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	theirs, err := pr.Servers(ctx)
	if err != nil {
		s.providerError(w, r, "list servers", err)
		return nil, false
	}
	match := matchServers(servers, theirs)
	out := make([]store.FloatingIPServer, len(servers))
	for i, srv := range servers {
		m, ok := match[srv.ID]
		if !ok {
			writeError(w, http.StatusUnprocessableEntity, "server "+srv.Name+" is not one of the account's servers (none of its addresses is)")
			return nil, false
		}
		out[i] = store.FloatingIPServer{ServerID: srv.ID, ProviderServerID: m.ID}
	}
	return out, true
}

// memberFor is the server of the group the provider's server is (0: none).
func memberFor(group []store.FloatingIPServer, providerServer string) int64 {
	for _, m := range group {
		if providerServer != "" && m.ProviderServerID == providerServer {
			return m.ServerID
		}
	}
	return 0
}

func (s *Server) handleAddFloatingIP(w http.ResponseWriter, r *http.Request, p *principal) {
	var req addFloatingIPRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	acc, err := s.db.CloudAccount(r.Context(), req.AccountID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "cloud account not found")
		return
	}
	if err != nil {
		s.internalError(w, r, "load cloud account", err)
		return
	}
	pr, err := s.cloudProvider(acc)
	if err != nil {
		s.internalError(w, r, "cloud account", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ip, err := pr.FloatingIP(ctx, req.ProviderID)
	if err != nil {
		s.providerError(w, r, "read floating IP", err)
		return
	}
	group, ok := s.floatingGroup(w, r, pr, req.ServerIDs)
	if !ok {
		return
	}
	f, err := s.db.CreateFloatingIP(r.Context(), store.FloatingIP{AccountID: acc.ID, ProviderID: ip.ID, Name: ip.Name,
		Address: ip.Address, Network: ip.Network, Location: ip.Location, ServerID: memberFor(group, ip.ServerID), Servers: group})
	if errors.Is(err, store.ErrAddressTaken) {
		writeError(w, http.StatusConflict, "this floating IP is already added")
		return
	}
	if err != nil {
		s.internalError(w, r, "add floating IP", err)
		return
	}
	s.audit(r, &p.user.ID, "floating_ip.added", map[string]any{"floating_ip_id": f.ID, "address": f.Address,
		"account_id": acc.ID, "server_ids": req.ServerIDs})
	for _, m := range group {
		s.pushFloating(r.Context(), m.ServerID)
	}
	s.writeFloatingIP(w, r, http.StatusCreated, f.ID)
}

func (s *Server) pathFloatingIP(w http.ResponseWriter, r *http.Request) (*store.FloatingIP, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		f, err := s.db.FloatingIP(r.Context(), id)
		if err == nil {
			return f, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "load floating IP", err)
			return nil, false
		}
	}
	writeError(w, http.StatusNotFound, "floating IP not found")
	return nil, false
}

// floatingProvider is the API of a floating IP's account.
func (s *Server) floatingProvider(w http.ResponseWriter, r *http.Request, f *store.FloatingIP) (cloud.Provider, bool) {
	acc, err := s.db.CloudAccount(r.Context(), f.AccountID)
	if err == nil {
		var pr cloud.Provider
		if pr, err = s.cloudProvider(acc); err == nil {
			return pr, true
		}
	}
	s.internalError(w, r, "cloud account", err)
	return nil, false
}

func (s *Server) handleSetFloatingIPServers(w http.ResponseWriter, r *http.Request, p *principal) {
	f, ok := s.pathFloatingIP(w, r)
	if !ok {
		return
	}
	var req floatingServersRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	pr, ok := s.floatingProvider(w, r, f)
	if !ok {
		return
	}
	group, ok := s.floatingGroup(w, r, pr, req.ServerIDs)
	if !ok {
		return
	}
	if err := s.db.SetFloatingIPServers(r.Context(), f.ID, group); err != nil {
		s.internalError(w, r, "set floating IP servers", err)
		return
	}
	if f.ServerID != 0 && !slices.ContainsFunc(group, func(m store.FloatingIPServer) bool { return m.ServerID == f.ServerID }) {
		// Still routed there by the provider, but no longer one of the
		// group: the panel no longer counts it.
		if err := s.db.SetFloatingIPServer(r.Context(), f.ID, 0); err != nil {
			s.internalError(w, r, "set floating IP server", err)
			return
		}
	}
	s.audit(r, &p.user.ID, "floating_ip.servers_changed", map[string]any{"floating_ip_id": f.ID, "address": f.Address,
		"server_ids": req.ServerIDs})
	touched := map[int64]bool{}
	for _, m := range append(slices.Clone(f.Servers), group...) {
		if !touched[m.ServerID] {
			touched[m.ServerID] = true
			s.pushFloating(r.Context(), m.ServerID)
		}
	}
	s.writeFloatingIP(w, r, http.StatusOK, f.ID)
}

type assignFloatingIPRequest struct {
	ServerID int64 `json:"server_id"`
}

// handleAssignFloatingIP has the provider route the address to another
// server of the group.
func (s *Server) handleAssignFloatingIP(w http.ResponseWriter, r *http.Request, p *principal) {
	f, ok := s.pathFloatingIP(w, r)
	if !ok {
		return
	}
	var req assignFloatingIPRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	i := slices.IndexFunc(f.Servers, func(m store.FloatingIPServer) bool { return m.ServerID == req.ServerID })
	if i < 0 {
		writeError(w, http.StatusUnprocessableEntity, "the server is not one of the floating IP's servers")
		return
	}
	pr, ok := s.floatingProvider(w, r, f)
	if !ok {
		return
	}
	// Assigning takes the provider a few seconds.
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err := pr.Assign(ctx, f.ProviderID, f.Servers[i].ProviderServerID); err != nil {
		s.providerError(w, r, "assign floating IP", err)
		return
	}
	if err := s.db.SetFloatingIPServer(r.Context(), f.ID, req.ServerID); err != nil {
		s.internalError(w, r, "set floating IP server", err)
		return
	}
	s.audit(r, &p.user.ID, "floating_ip.assigned", map[string]any{"floating_ip_id": f.ID, "address": f.Address,
		"server_id": req.ServerID, "previous_server_id": f.ServerID})
	s.log.Info("floating IP assigned", "address", f.Address, "server_id", req.ServerID, "previous_server_id", f.ServerID)
	s.writeFloatingIP(w, r, http.StatusOK, f.ID)
}

// handleRefreshFloatingIP reads where the provider routes the address
// (it may have been moved outside the panel).
func (s *Server) handleRefreshFloatingIP(w http.ResponseWriter, r *http.Request, p *principal) {
	f, ok := s.pathFloatingIP(w, r)
	if !ok {
		return
	}
	pr, ok := s.floatingProvider(w, r, f)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ip, err := pr.FloatingIP(ctx, f.ProviderID)
	if err != nil {
		s.providerError(w, r, "read floating IP", err)
		return
	}
	if err := s.db.SetFloatingIPServer(r.Context(), f.ID, memberFor(f.Servers, ip.ServerID)); err != nil {
		s.internalError(w, r, "set floating IP server", err)
		return
	}
	for _, m := range f.Servers {
		s.pushFloating(r.Context(), m.ServerID)
	}
	s.writeFloatingIP(w, r, http.StatusOK, f.ID)
}

// handleDeleteFloatingIP removes the floating IP from the panel and the
// servers' interfaces; the provider keeps it (and where it routes it).
func (s *Server) handleDeleteFloatingIP(w http.ResponseWriter, r *http.Request, p *principal) {
	f, ok := s.pathFloatingIP(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteFloatingIP(r.Context(), f.ID); err != nil {
		s.internalError(w, r, "delete floating IP", err)
		return
	}
	s.audit(r, &p.user.ID, "floating_ip.removed", map[string]any{"floating_ip_id": f.ID, "address": f.Address})
	for _, m := range f.Servers {
		s.pushFloating(r.Context(), m.ServerID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// pushFloating gives a server's agent the floating IPs of its groups and
// records whether it could put them on its interface. An offline server
// gets them when it connects.
func (s *Server) pushFloating(ctx context.Context, serverID int64) {
	if !s.agents.Connected(serverID) {
		return
	}
	mu, _ := s.floatingLocks.LoadOrStore(serverID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	addrs, err := s.db.FloatingAddressesOn(ctx, serverID)
	if err != nil {
		s.log.Error("floating IPs of a server", "server_id", serverID, "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	msg := ""
	if err := s.agents.Call(ctx, serverID, agentproto.TypeFloatingIPs, agentproto.FloatingIPs{Addresses: addrs}, nil); err != nil {
		msg = agentErrorText(err)
		s.log.Warn("push floating IPs", "server_id", serverID, "err", err)
	}
	if err := s.db.SetServerFloatingError(context.WithoutCancel(ctx), serverID, msg); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Error("record floating IP error", "server_id", serverID, "err", err)
	}
}
