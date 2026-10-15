package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/mail"
	"regexp"
	"slices"
	"strings"

	"github.com/wiefix/basalt/internal/mailserver"
	"github.com/wiefix/basalt/internal/store"
)

const (
	// settingMail holds the mail server settings Basalt manages (mailSettings).
	settingMail = "mail_settings"
	// defaultSendLimit is how many messages a mailbox may send per hour
	// unless set otherwise: enough for people, a brake on a stolen password.
	defaultSendLimit = 200
	maxForwardTo     = 50
)

var mailRelayPasswordAD = []byte("mail-relay-password")

type mailRelaySettings struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	TLS      string `json:"tls"` // "implicit" or "starttls"
	Username string `json:"username,omitempty"`
	Password []byte `json:"password,omitempty"` // sealed
	Insecure bool   `json:"insecure,omitempty"`
}

type mailSettings struct {
	Relay *mailRelaySettings `json:"relay,omitempty"`
	// SendLimit is messages per mailbox per hour (nil: the default, 0: none).
	SendLimit *int `json:"send_limit,omitempty"`
}

func (m *mailSettings) sendLimit() int {
	if m.SendLimit == nil {
		return defaultSendLimit
	}
	return *m.SendLimit
}

func (s *Server) mailSettings(ctx context.Context) (*mailSettings, error) {
	v, err := s.db.Setting(ctx, settingMail)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	m := &mailSettings{}
	if v != "" {
		if err := json.Unmarshal([]byte(v), m); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// relay returns the relay to configure in the mail server (nil for none).
func (s *Server) relay(m *mailSettings) (*mailserver.Relay, error) {
	if m.Relay == nil {
		return nil, nil
	}
	r := &mailserver.Relay{Host: m.Relay.Host, Port: m.Relay.Port, ImplicitTLS: m.Relay.TLS == "implicit",
		Username: m.Relay.Username, Insecure: m.Relay.Insecure}
	if len(m.Relay.Password) > 0 {
		p, err := s.box.Open(m.Relay.Password, mailRelayPasswordAD)
		if err != nil {
			return nil, err
		}
		r.Password = string(p)
	}
	return r, nil
}

// reloadError: the settings are stored in the mail server but it did
// not reload them yet; the panel retries in the background.
type reloadError struct{ err error }

func (e *reloadError) Error() string { return e.err.Error() }

// applyMailSettings configures the relay and the send limit, then has
// the mail server reload its settings (also those Configure changed).
func (s *Server) applyMailSettings(ctx context.Context, c *mailserver.Client, m *mailSettings) error {
	r, err := s.relay(m)
	if err != nil {
		return err
	}
	if err := c.SetRelay(ctx, r); err != nil {
		return err
	}
	if err := c.SetSendLimit(ctx, m.sendLimit()); err != nil {
		return err
	}
	return s.reloadMail(ctx, c)
}

// reloadMail reloads the mail server's settings, remembering a failure
// for refreshMailLoop to retry.
func (s *Server) reloadMail(ctx context.Context, c *mailserver.Client) error {
	err := c.Reload(ctx)
	s.mailReloadPending.Store(err != nil)
	if err != nil {
		return &reloadError{err}
	}
	return nil
}

type mailRelayView struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	TLS         string `json:"tls"`
	Username    string `json:"username"`
	HasPassword bool   `json:"has_password"`
	Insecure    bool   `json:"insecure"`
}

func viewMailSettings(m *mailSettings) map[string]any {
	var relay *mailRelayView
	if r := m.Relay; r != nil {
		relay = &mailRelayView{Host: r.Host, Port: r.Port, TLS: r.TLS, Username: r.Username,
			HasPassword: len(r.Password) > 0, Insecure: r.Insecure}
	}
	return map[string]any{"relay": relay, "send_limit": m.sendLimit()}
}

type mailRelayRequest struct {
	Host     string  `json:"host"`
	Port     int     `json:"port"`
	TLS      string  `json:"tls"`
	Username string  `json:"username"`
	Password *string `json:"password"` // nil keeps the stored one
	Insecure bool    `json:"insecure"`
}

type mailSettingsRequest struct {
	// Relay: absent keeps it, null removes it.
	Relay     json.RawMessage `json:"relay" openapi:"mailRelayRequest"`
	SendLimit *int            `json:"send_limit"`
}

var errMailRelay = errors.New("invalid relay")

// relaySettings validates a relay; old supplies the password when the
// request keeps it.
func (s *Server) relaySettings(req *mailRelayRequest, old *mailRelaySettings) (*mailRelaySettings, error) {
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(req.Host)), ".")
	if net.ParseIP(host) == nil && (len(host) > 253 || !hostnameRE.MatchString(host)) {
		return nil, errors.New("the relay host must be a host name or an IP address")
	}
	if req.Port < 1 || req.Port > 65535 {
		return nil, errors.New("invalid relay port")
	}
	if req.TLS != "implicit" && req.TLS != "starttls" {
		return nil, errors.New(`relay TLS must be "implicit" (usually port 465) or "starttls" (usually port 587)`)
	}
	if len(req.Username) > 256 || strings.ContainsAny(req.Username, "\x00\r\n") {
		return nil, errors.New("invalid relay user name")
	}
	r := &mailRelaySettings{Host: host, Port: req.Port, TLS: req.TLS, Username: req.Username, Insecure: req.Insecure}
	switch {
	case req.Username == "":
	case req.Password != nil:
		if *req.Password == "" || len(*req.Password) > 1024 || strings.ContainsAny(*req.Password, "\x00\r\n") {
			return nil, errors.New("invalid relay password")
		}
		r.Password = s.box.Seal([]byte(*req.Password), mailRelayPasswordAD)
	case old != nil && len(old.Password) > 0:
		r.Password = old.Password
	default:
		return nil, errors.New("the relay password is required with a user name")
	}
	return r, nil
}

// hostnameRE is a DNS host name (labels of letters, digits and hyphens).
var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

func (s *Server) handleMailSettings(w http.ResponseWriter, r *http.Request, p *principal) {
	var req mailSettingsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	_, c, ok := s.mailServer(w, r)
	if !ok {
		return
	}
	s.mailMu.Lock()
	defer s.mailMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	m, err := s.mailSettings(ctx)
	if err != nil {
		s.internalError(w, r, "load mail settings", err)
		return
	}
	details := map[string]any{}
	if len(req.Relay) > 0 {
		if string(req.Relay) == "null" {
			m.Relay = nil
			details["relay"] = nil
		} else {
			var rr mailRelayRequest
			dec := json.NewDecoder(strings.NewReader(string(req.Relay)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&rr); err != nil {
				writeError(w, http.StatusBadRequest, errMailRelay.Error())
				return
			}
			relay, err := s.relaySettings(&rr, m.Relay)
			if err != nil {
				writeError(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
			m.Relay = relay
			details["relay"] = relay.Host
		}
	}
	if req.SendLimit != nil {
		if *req.SendLimit < 0 || *req.SendLimit > 1_000_000 {
			writeError(w, http.StatusUnprocessableEntity, "the send limit is 0 (none) to 1000000 messages per hour")
			return
		}
		m.SendLimit = req.SendLimit
		details["send_limit"] = *req.SendLimit
	}
	var re *reloadError
	err = s.applyMailSettings(ctx, c, m)
	if err != nil && !errors.As(err, &re) {
		s.mailError(w, err)
		return
	}
	b, _ := json.Marshal(m)
	if err := s.db.SetSetting(ctx, settingMail, string(b)); err != nil {
		s.internalError(w, r, "save mail settings", err)
		return
	}
	s.audit(r, &p.user.ID, "mail.settings_updated", details)
	resp := map[string]any{"settings": viewMailSettings(m)}
	if re != nil {
		resp["warning"] = re.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

type mailDomainUpdate struct {
	CatchAll *string `json:"catch_all"`
}

// handleUpdateMailDomain sets a domain's catch-all address.
func (s *Server) handleUpdateMailDomain(w http.ResponseWriter, r *http.Request, p *principal) {
	d, ok := s.pathMailDomain(w, r)
	if !ok {
		return
	}
	var req mailDomainUpdate
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CatchAll == nil {
		writeError(w, http.StatusUnprocessableEntity, "nothing to change")
		return
	}
	local := strings.ToLower(strings.TrimSpace(*req.CatchAll))
	if local != "" && !localPartRE.MatchString(local) {
		writeError(w, http.StatusUnprocessableEntity, "the catch-all is an address of this domain: give the part before @")
		return
	}
	_, c, ok := s.mailServer(w, r)
	if !ok {
		return
	}
	s.mailMu.Lock()
	defer s.mailMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	if err := c.SetCatchAll(ctx, d.RemoteID, catchAllAddress(local, d.Name)); err != nil {
		s.mailError(w, err)
		return
	}
	if err := s.db.SetMailDomainCatchAll(ctx, d.ID, local); err != nil {
		s.internalError(w, r, "save catch-all", err)
		return
	}
	s.audit(r, &p.user.ID, "mail.domain_updated", map[string]any{"mail_domain_id": d.ID, "catch_all": local})
	w.WriteHeader(http.StatusNoContent)
}

func catchAllAddress(local, domain string) string {
	if local == "" {
		return ""
	}
	return local + "@" + domain
}

// forwardTo validates the recipients of a forwarder.
func forwardTo(list []string, self string) ([]string, error) {
	if len(list) == 0 || len(list) > maxForwardTo {
		return nil, errors.New("give 1 to 50 addresses to forward to")
	}
	var out []string
	for _, a := range list {
		a = strings.ToLower(strings.TrimSpace(a))
		addr, err := mail.ParseAddress(a)
		if err != nil || addr.Name != "" || addr.Address != a || len(a) > 254 || !strings.Contains(a[strings.LastIndex(a, "@"):], ".") {
			return nil, errors.New("invalid address " + a)
		}
		if a == self {
			return nil, errors.New("a forwarder cannot forward to itself")
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out, nil
}

type forwarderRequest struct {
	Local      string   `json:"local"`
	Recipients []string `json:"recipients"`
}

func (s *Server) handleListForwarders(w http.ResponseWriter, r *http.Request, p *principal) {
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
	list, err := c.Forwarders(ctx, d.RemoteID)
	if err != nil {
		s.mailError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"forwarders": list})
}

func (s *Server) handleCreateForwarder(w http.ResponseWriter, r *http.Request, p *principal) {
	d, ok := s.pathMailDomain(w, r)
	if !ok {
		return
	}
	var req forwarderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	local := strings.ToLower(strings.TrimSpace(req.Local))
	if !localPartRE.MatchString(local) {
		writeError(w, http.StatusUnprocessableEntity, "the address is the part before @: letters, digits, . _ + -")
		return
	}
	rcpts, err := forwardTo(req.Recipients, local+"@"+d.Name)
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
	id, err := c.CreateForwarder(ctx, d.RemoteID, local, rcpts)
	if err != nil {
		s.mailError(w, err)
		return
	}
	s.audit(r, &p.user.ID, "mail.forwarder_created", map[string]any{"address": local + "@" + d.Name, "recipients": rcpts})
	writeJSON(w, http.StatusCreated, map[string]any{"forwarder": mailserver.Forwarder{ID: id, Address: local + "@" + d.Name,
		DomainID: d.RemoteID, Recipients: rcpts}})
}

// forwarder finds the forwarder {fid} of domain d (only of that domain).
func (s *Server) forwarder(w http.ResponseWriter, r *http.Request, d *store.MailDomain) (*mailserver.Forwarder, *mailserver.Client, bool) {
	_, c, ok := s.mailServer(w, r)
	if !ok {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	list, err := c.Forwarders(ctx, d.RemoteID)
	if err != nil {
		s.mailError(w, err)
		return nil, nil, false
	}
	i := slices.IndexFunc(list, func(f mailserver.Forwarder) bool { return f.ID == r.PathValue("fid") })
	if i < 0 {
		writeError(w, http.StatusNotFound, "forwarder not found")
		return nil, nil, false
	}
	return &list[i], c, true
}

func (s *Server) handleUpdateForwarder(w http.ResponseWriter, r *http.Request, p *principal) {
	d, ok := s.pathMailDomain(w, r)
	if !ok {
		return
	}
	var req forwarderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Local != "" {
		writeError(w, http.StatusUnprocessableEntity, "the address cannot be changed")
		return
	}
	f, c, ok := s.forwarder(w, r, d)
	if !ok {
		return
	}
	rcpts, err := forwardTo(req.Recipients, f.Address)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	if err := c.UpdateForwarder(ctx, f.ID, rcpts); err != nil {
		s.mailError(w, err)
		return
	}
	s.audit(r, &p.user.ID, "mail.forwarder_updated", map[string]any{"address": f.Address, "recipients": rcpts})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteForwarder(w http.ResponseWriter, r *http.Request, p *principal) {
	d, ok := s.pathMailDomain(w, r)
	if !ok {
		return
	}
	f, c, ok := s.forwarder(w, r, d)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), mailTimeout)
	defer cancel()
	if err := c.DeleteForwarder(ctx, f.ID); err != nil {
		s.mailError(w, err)
		return
	}
	s.audit(r, &p.user.ID, "mail.forwarder_deleted", map[string]any{"address": f.Address})
	w.WriteHeader(http.StatusNoContent)
}
