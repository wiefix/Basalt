package mailserver

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// Transport posts a JMAP request body to the mail server and returns the
// HTTP status and body (through the agent: see agentproto.MailAPI).
type Transport func(ctx context.Context, body []byte) (status int, resp []byte, err error)

// Client calls the mail server's management API.
type Client struct {
	t Transport
}

func NewClient(t Transport) *Client { return &Client{t: t} }

var using = []string{"urn:ietf:params:jmap:core", "urn:stalwart:jmap"}

// call runs one method and decodes its response arguments into out.
func (c *Client) call(ctx context.Context, method string, args, out any) error {
	body, err := json.Marshal(map[string]any{
		"using":       using,
		"methodCalls": []any{[]any{method, args, "c"}},
	})
	if err != nil {
		return err
	}
	status, resp, err := c.t(ctx, body)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("mail server: HTTP %d: %s", status, problem(resp))
	}
	var r struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(resp, &r); err != nil || len(r.MethodResponses) != 1 || len(r.MethodResponses[0]) < 2 {
		return fmt.Errorf("mail server: unexpected answer to %s", method)
	}
	var name string
	json.Unmarshal(r.MethodResponses[0][0], &name)
	if name == "error" {
		var e struct{ Type, Description string }
		json.Unmarshal(r.MethodResponses[0][1], &e)
		return fmt.Errorf("mail server: %s: %s %s", method, e.Type, e.Description)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.MethodResponses[0][1], out)
}

// problem extracts the message of an RFC 7807 error body.
func problem(b []byte) string {
	var p struct{ Title, Detail string }
	if json.Unmarshal(b, &p) == nil && (p.Title != "" || p.Detail != "") {
		return strings.TrimSpace(p.Title + ": " + p.Detail)
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}

// SetError is a refused create/update/destroy.
type SetError struct {
	Type             string   `json:"type"`
	Description      string   `json:"description"`
	Properties       []string `json:"properties"`
	ValidationErrors []struct {
		Type     string `json:"type"`
		Property string `json:"property"`
		Value    string `json:"value"` // an explanation, when given
	} `json:"validationErrors"`
}

func (e *SetError) Error() string {
	msg := e.Description
	if msg == "" {
		msg = e.Type
	}
	props := slices.Clone(e.Properties)
	for _, v := range e.ValidationErrors {
		why := v.Type
		if v.Value != "" {
			why = v.Value
		}
		props = append(props, v.Property+": "+why)
	}
	if len(props) > 0 {
		msg += " (" + strings.Join(props, ", ") + ")"
	}
	return msg
}

type setResponse struct {
	Created      map[string]struct{ ID string } `json:"created"`
	NotCreated   map[string]*SetError           `json:"notCreated"`
	NotUpdated   map[string]*SetError           `json:"notUpdated"`
	NotDestroyed map[string]*SetError           `json:"notDestroyed"`
}

func (r *setResponse) err() error {
	for _, m := range []map[string]*SetError{r.NotCreated, r.NotUpdated, r.NotDestroyed} {
		for _, e := range m {
			return e
		}
	}
	return nil
}

// create creates one object and returns its ID.
func (c *Client) create(ctx context.Context, object string, fields map[string]any) (string, error) {
	var r setResponse
	if err := c.call(ctx, object+"/set", map[string]any{"create": map[string]any{"n": fields}}, &r); err != nil {
		return "", err
	}
	if err := r.err(); err != nil {
		return "", err
	}
	id := r.Created["n"].ID
	if id == "" {
		return "", fmt.Errorf("mail server did not create the %s", object)
	}
	return id, nil
}

func (c *Client) update(ctx context.Context, object, id string, patch map[string]any) error {
	var r setResponse
	if err := c.call(ctx, object+"/set", map[string]any{"update": map[string]any{id: patch}}, &r); err != nil {
		return err
	}
	return r.err()
}

func (c *Client) destroy(ctx context.Context, object, id string) error {
	var r setResponse
	if err := c.call(ctx, object+"/set", map[string]any{"destroy": []string{id}}, &r); err != nil {
		return err
	}
	if e, ok := r.NotDestroyed[id]; ok && e.Type != "notFound" {
		return e
	}
	return nil
}

// getAll lists every object of a type with the given properties.
func (c *Client) getAll(ctx context.Context, object string, props []string, out any) error {
	var q struct {
		IDs []string `json:"ids"`
	}
	if err := c.call(ctx, object+"/query", map[string]any{}, &q); err != nil {
		return err
	}
	if len(q.IDs) == 0 {
		return json.Unmarshal([]byte(`{"list":[]}`), out)
	}
	args := map[string]any{"ids": q.IDs}
	if props != nil {
		args["properties"] = props
	}
	return c.call(ctx, object+"/get", args, out)
}

// Configure makes sure a submission listener with STARTTLS runs on 587
// (Stalwart starts with 465 only) and, once a domain exists, sets the
// server's host name and default domain (Stalwart takes both together:
// it starts with a placeholder domain).
func (c *Client) Configure(ctx context.Context, hostname, defaultDomainID string) error {
	if defaultDomainID != "" {
		err := c.update(ctx, "x:SystemSettings", "singleton", map[string]any{
			"defaultHostname": hostname, "defaultDomainId": defaultDomainID,
			"mailExchangers": map[string]any{"0": map[string]any{"hostname": hostname, "priority": 10}},
		})
		if err != nil {
			return fmt.Errorf("set host name: %w", err)
		}
	}
	var ls struct {
		List []struct {
			Name string `json:"name"`
		} `json:"list"`
	}
	if err := c.getAll(ctx, "x:NetworkListener", []string{"name"}, &ls); err != nil {
		return err
	}
	if !slices.ContainsFunc(ls.List, func(l struct {
		Name string `json:"name"`
	}) bool {
		return l.Name == "submission"
	}) {
		_, err := c.create(ctx, "x:NetworkListener", map[string]any{
			"name": "submission", "bind": map[string]bool{"[::]:587": true}, "protocol": "smtp", "tlsImplicit": false,
		})
		if err != nil {
			return fmt.Errorf("add submission listener: %w", err)
		}
	}
	return c.logToStdout(ctx)
}

// logToStdout makes the server log to its standard output (the container
// log) instead of a file inside the container, its default.
func (c *Client) logToStdout(ctx context.Context) error {
	var ts struct {
		List []struct {
			ID   string `json:"id"`
			Type string `json:"@type"`
		} `json:"list"`
	}
	if err := c.getAll(ctx, "x:Tracer", []string{"id", "@type"}, &ts); err != nil {
		return err
	}
	var files []string
	stdout := false
	for _, t := range ts.List {
		switch t.Type {
		case "Stdout":
			stdout = true
		case "Log":
			files = append(files, t.ID)
		}
	}
	if !stdout {
		if _, err := c.create(ctx, "x:Tracer", map[string]any{"@type": "Stdout", "ansi": false, "level": "info", "enable": true}); err != nil {
			return fmt.Errorf("log to stdout: %w", err)
		}
	}
	for _, id := range files {
		if err := c.destroy(ctx, "x:Tracer", id); err != nil {
			return err
		}
	}
	return nil
}

// reloadTries and reloadPause: a reload validates every setting, some by
// looking names up in DNS (the Pyzor spam server's), so a passing DNS
// failure makes it fail.
var (
	reloadTries = 5
	reloadPause = 2 * time.Second
)

// Reload applies changed server settings (routes, throttles, logging).
func (c *Client) Reload(ctx context.Context) error {
	var err error
	for i := range reloadTries {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(reloadPause):
			}
		}
		if _, err = c.create(ctx, "x:Action", map[string]any{"@type": "ReloadSettings"}); err == nil {
			return nil
		}
		var se *SetError
		if !errors.As(err, &se) {
			break
		}
	}
	return fmt.Errorf("the mail server did not reload its settings (they apply once it does): %w", err)
}

// Domain is a mail domain in the mail server.
type Domain struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DNSZoneFile string `json:"dnsZoneFile"`
}

func (c *Client) Domains(ctx context.Context) ([]Domain, error) {
	var r struct {
		List []Domain `json:"list"`
	}
	err := c.getAll(ctx, "x:Domain", []string{"id", "name", "dnsZoneFile"}, &r)
	return r.List, err
}

// CreateDomain adds a domain whose DKIM keys the server manages (and
// rotates); its DNS records are published by Basalt.
func (c *Client) CreateDomain(ctx context.Context, name string) (string, error) {
	id, err := c.create(ctx, "x:Domain", map[string]any{
		"name":                  name,
		"certificateManagement": map[string]any{"@type": "Manual"},
		"dkimManagement":        map[string]any{"@type": "Automatic"},
		"dnsManagement":         map[string]any{"@type": "Manual"},
		"subAddressing":         map[string]any{"@type": "Enabled"},
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// DeleteDomain removes a domain and its DKIM keys (which keep it linked).
func (c *Client) DeleteDomain(ctx context.Context, id string) error {
	var keys struct {
		List []struct {
			ID       string `json:"id"`
			DomainID string `json:"domainId"`
		} `json:"list"`
	}
	if err := c.getAll(ctx, "x:DkimSignature", []string{"id", "domainId"}, &keys); err != nil {
		return err
	}
	for _, k := range keys.List {
		if k.DomainID == id {
			if err := c.destroy(ctx, "x:DkimSignature", k.ID); err != nil {
				return err
			}
		}
	}
	return c.destroy(ctx, "x:Domain", id)
}

// EnableDomain turns a domain on or off. The server's default domain
// cannot be deleted, only turned off.
func (c *Client) EnableDomain(ctx context.Context, id string, on bool) error {
	return c.update(ctx, "x:Domain", id, map[string]any{"isEnabled": on})
}

// Mailbox is a user account. Address is local@domain.
type Mailbox struct {
	ID       string   `json:"id"`
	Address  string   `json:"address"`
	Name     string   `json:"name"`
	DomainID string   `json:"domain_id"`
	Aliases  []string `json:"aliases"` // local parts in the same domain
	QuotaMB  int64    `json:"quota_mb"`
	UsedMB   float64  `json:"used_mb"`
}

type remoteAccount struct {
	ID            string                    `json:"id"`
	Type          string                    `json:"@type"`
	Name          string                    `json:"name"`
	DomainID      string                    `json:"domainId"`
	EmailAddress  string                    `json:"emailAddress"`
	Description   *string                   `json:"description"`
	Aliases       map[string]map[string]any `json:"aliases"`
	Quotas        map[string]int64          `json:"quotas"`
	UsedDiskQuota float64                   `json:"usedDiskQuota"`
}

// Mailboxes lists the user accounts of a domain.
func (c *Client) Mailboxes(ctx context.Context, domainID string) ([]Mailbox, error) {
	var r struct {
		List []remoteAccount `json:"list"`
	}
	if err := c.getAll(ctx, "x:Account", nil, &r); err != nil {
		return nil, err
	}
	out := []Mailbox{}
	for _, a := range r.List {
		if a.Type != "User" || a.DomainID != domainID {
			continue
		}
		m := Mailbox{ID: a.ID, Address: a.EmailAddress, DomainID: a.DomainID, Aliases: []string{},
			QuotaMB: a.Quotas["maxDiskQuota"] >> 20, UsedMB: a.UsedDiskQuota / (1 << 20)}
		if a.Description != nil {
			m.Name = *a.Description
		}
		for _, k := range slices.Sorted(maps.Keys(a.Aliases)) {
			if n, _ := a.Aliases[k]["name"].(string); n != "" {
				m.Aliases = append(m.Aliases, n)
			}
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b Mailbox) int { return strings.Compare(a.Address, b.Address) })
	return out, nil
}

// MailboxChange is what to set on a mailbox; nil fields stay as they are.
type MailboxChange struct {
	Password *string
	Name     *string
	Aliases  []string // local parts; nil leaves them
	QuotaMB  *int64   // 0: no limit
}

func (ch MailboxChange) fields(domainID string) map[string]any {
	f := map[string]any{}
	if ch.Password != nil {
		f["credentials"] = map[string]any{"0": map[string]any{"@type": "Password", "secret": *ch.Password}}
	}
	if ch.Name != nil {
		f["description"] = *ch.Name
		if *ch.Name == "" {
			f["description"] = nil // Stalwart refuses an empty string
		}
	}
	if ch.Aliases != nil {
		al := map[string]any{}
		for i, a := range ch.Aliases {
			al[fmt.Sprint(i)] = map[string]any{"name": a, "domainId": domainID}
		}
		f["aliases"] = al
	}
	if ch.QuotaMB != nil {
		q := map[string]int64{}
		if *ch.QuotaMB > 0 {
			q["maxDiskQuota"] = *ch.QuotaMB << 20
		}
		f["quotas"] = q
	}
	return f
}

// CreateMailbox adds a user account local@domain.
func (c *Client) CreateMailbox(ctx context.Context, domainID, local string, ch MailboxChange) (string, error) {
	f := ch.fields(domainID)
	f["@type"], f["name"], f["domainId"] = "User", local, domainID
	return c.create(ctx, "x:Account", f)
}

func (c *Client) UpdateMailbox(ctx context.Context, id, domainID string, ch MailboxChange) error {
	return c.update(ctx, "x:Account", id, ch.fields(domainID))
}

func (c *Client) DeleteMailbox(ctx context.Context, id string) error {
	return c.destroy(ctx, "x:Account", id)
}

// InstallCertificate loads a TLS certificate (PEM chain and key) for the
// mail protocols, unless the server already has one valid as long, makes
// it the default one and removes older ones. It reports whether it
// installed the certificate.
func (c *Client) InstallCertificate(ctx context.Context, chainPEM, keyPEM []byte) (bool, error) {
	leaf, err := leafCertificate(chainPEM)
	if err != nil {
		return false, err
	}
	var have struct {
		List []struct {
			ID            string    `json:"id"`
			NotValidAfter time.Time `json:"notValidAfter"`
		} `json:"list"`
	}
	if err := c.getAll(ctx, "x:Certificate", []string{"id", "notValidAfter"}, &have); err != nil {
		return false, err
	}
	for _, h := range have.List {
		if !h.NotValidAfter.Before(leaf.NotAfter.Truncate(time.Second)) {
			return false, nil
		}
	}
	id, err := c.create(ctx, "x:Certificate", map[string]any{
		"certificate": map[string]any{"@type": "Text", "value": string(chainPEM)},
		"privateKey":  map[string]any{"@type": "Text", "secret": string(keyPEM)},
	})
	if err != nil {
		return false, err
	}
	// Needs a default domain: without one, the certificate still serves
	// the names it covers.
	if c.update(ctx, "x:SystemSettings", "singleton", map[string]any{"defaultCertificateId": id}) == nil {
		for _, h := range have.List {
			c.destroy(ctx, "x:Certificate", h.ID)
		}
	}
	// Certificates take effect on a reload.
	if _, err := c.create(ctx, "x:Action", map[string]any{"@type": "ReloadTlsCertificates"}); err != nil {
		return true, fmt.Errorf("reload certificates: %w", err)
	}
	return true, nil
}

func leafCertificate(chain []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(chain)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no certificate in PEM data")
	}
	return x509.ParseCertificate(block.Bytes)
}
