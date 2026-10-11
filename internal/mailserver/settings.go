package mailserver

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Names of the objects Basalt manages in the mail server's settings.
const (
	relayName     = "basalt-relay"
	sendLimitName = "basalt-send-limit"
)

// Relay is a smarthost all outgoing mail goes through (a provider such as
// SES, Mailgun or Postmark, or any SMTP server).
type Relay struct {
	Host string
	Port int
	// ImplicitTLS connects with TLS (usually port 465); otherwise
	// STARTTLS is required.
	ImplicitTLS bool
	Username    string
	Password    string
	// Insecure accepts a relay certificate that does not verify.
	Insecure bool
}

// Expressions of the outbound strategy: the server's defaults, and with a
// relay every remote recipient goes through it, with TLS required.
const (
	exprLocal     = "is_local_domain(rcpt_domain)"
	exprRemote    = "!is_local_domain(rcpt_domain)"
	defaultTLSIf  = "retry_num > 0 && last_error == 'tls'"
	quotedRelay   = "'" + relayName + "'"
	quotedDefault = "'default'"
)

func expression(match [][2]string, otherwise string) map[string]any {
	m := map[string]any{}
	for i, kv := range match {
		m[fmt.Sprint(i)] = map[string]any{"if": kv[0], "then": kv[1]}
	}
	return map[string]any{"match": m, "else": otherwise}
}

// named finds the ID of the object of a type with the given name field
// ("" if none).
func (c *Client) named(ctx context.Context, object, field, name string) (string, error) {
	var r struct {
		List []map[string]any `json:"list"`
	}
	if err := c.getAll(ctx, object, []string{"id", field}, &r); err != nil {
		return "", err
	}
	for _, o := range r.List {
		if o[field] == name {
			id, _ := o["id"].(string)
			return id, nil
		}
	}
	return "", nil
}

// upsert updates the object named name, or creates it with createOnly
// fields added (immutable ones such as the name).
func (c *Client) upsert(ctx context.Context, object, field, name string, fields, createOnly map[string]any) error {
	id, err := c.named(ctx, object, field, name)
	if err != nil {
		return err
	}
	if id != "" {
		return c.update(ctx, object, id, fields)
	}
	all := maps.Clone(fields)
	maps.Copy(all, createOnly)
	all[field] = name
	_, err = c.create(ctx, object, all)
	return err
}

func (c *Client) destroyNamed(ctx context.Context, object, field, name string) error {
	id, err := c.named(ctx, object, field, name)
	if err != nil || id == "" {
		return err
	}
	return c.destroy(ctx, object, id)
}

// SetRelay sends all mail for remote recipients through r, or delivers it
// directly (to the recipients' MX) when r is nil.
func (c *Client) SetRelay(ctx context.Context, r *Relay) error {
	strategy := "x:MtaOutboundStrategy"
	if r == nil {
		err := c.update(ctx, strategy, "singleton", map[string]any{
			"route": expression([][2]string{{exprLocal, "'local'"}}, "'mx'"),
			"tls":   expression([][2]string{{defaultTLSIf, "'invalid-tls'"}}, quotedDefault),
		})
		if err != nil {
			return fmt.Errorf("deliver directly: %w", err)
		}
		if err := c.destroyNamed(ctx, "x:MtaRoute", "name", relayName); err != nil {
			return err
		}
		return c.destroyNamed(ctx, "x:MtaTlsStrategy", "name", relayName)
	}
	secret := map[string]any{"@type": "None"}
	var user any // null
	if r.Username != "" {
		user, secret = r.Username, map[string]any{"@type": "Value", "secret": r.Password}
	}
	err := c.upsert(ctx, "x:MtaRoute", "name", relayName, map[string]any{
		"description": "Relay set in Basalt", "address": r.Host, "port": r.Port, "protocol": "smtp",
		"implicitTls": r.ImplicitTLS, "authUsername": user, "authSecret": secret, "allowInvalidCerts": r.Insecure,
	}, map[string]any{"@type": "Relay"})
	if err != nil {
		return fmt.Errorf("relay route: %w", err)
	}
	// Credentials never go over a connection without TLS.
	err = c.upsert(ctx, "x:MtaTlsStrategy", "name", relayName, map[string]any{
		"description": "Relay set in Basalt", "startTls": "require", "dane": "disable", "mtaSts": "disable",
		"allowInvalidCerts": r.Insecure,
	}, nil)
	if err != nil {
		return fmt.Errorf("relay TLS: %w", err)
	}
	err = c.update(ctx, strategy, "singleton", map[string]any{
		"route": expression([][2]string{{exprLocal, "'local'"}}, quotedRelay),
		"tls":   expression([][2]string{{exprRemote, quotedRelay}}, quotedDefault),
	})
	if err != nil {
		return fmt.Errorf("route through the relay: %w", err)
	}
	return nil
}

// SetSendLimit limits how many messages each mailbox (authenticated user)
// may send per hour; 0 removes the limit. Mail arriving from other
// servers is not counted.
func (c *Client) SetSendLimit(ctx context.Context, perHour int) error {
	if perHour <= 0 {
		return c.destroyNamed(ctx, "x:MtaInboundThrottle", "description", sendLimitName)
	}
	return c.upsert(ctx, "x:MtaInboundThrottle", "description", sendLimitName, map[string]any{
		"enable": true,
		"key":    map[string]bool{"authenticatedAs": true},
		"match":  expression(nil, "!is_empty(authenticated_as)"),
		"rate":   map[string]any{"count": perHour, "period": 3600_000},
	}, nil)
}

// SetCatchAll delivers mail for unknown addresses of a domain to address
// ("" for none: such mail is refused).
func (c *Client) SetCatchAll(ctx context.Context, domainID, address string) error {
	var v any // null
	if address != "" {
		v = address
	}
	return c.update(ctx, "x:Domain", domainID, map[string]any{"catchAllAddress": v})
}

// Forwarder is an address that only forwards: mail to it goes to its
// recipients (other mailboxes or external addresses), nothing is stored.
type Forwarder struct {
	ID         string   `json:"id"`
	Address    string   `json:"address"`
	DomainID   string   `json:"domain_id"`
	Recipients []string `json:"recipients"`
}

// Forwarders lists the forwarders of a domain.
func (c *Client) Forwarders(ctx context.Context, domainID string) ([]Forwarder, error) {
	var r struct {
		List []struct {
			ID           string          `json:"id"`
			DomainID     string          `json:"domainId"`
			EmailAddress string          `json:"emailAddress"`
			Recipients   map[string]bool `json:"recipients"`
		} `json:"list"`
	}
	if err := c.getAll(ctx, "x:MailingList", []string{"id", "domainId", "emailAddress", "recipients"}, &r); err != nil {
		return nil, err
	}
	out := []Forwarder{}
	for _, l := range r.List {
		if l.DomainID != domainID {
			continue
		}
		out = append(out, Forwarder{ID: l.ID, Address: l.EmailAddress, DomainID: l.DomainID,
			Recipients: slices.Sorted(maps.Keys(l.Recipients))})
	}
	slices.SortFunc(out, func(a, b Forwarder) int { return strings.Compare(a.Address, b.Address) })
	return out, nil
}

func recipientSet(rcpts []string) map[string]bool {
	m := map[string]bool{}
	for _, r := range rcpts {
		m[r] = true
	}
	return m
}

// CreateForwarder adds local@domain forwarding to recipients.
func (c *Client) CreateForwarder(ctx context.Context, domainID, local string, recipients []string) (string, error) {
	return c.create(ctx, "x:MailingList", map[string]any{"name": local, "domainId": domainID,
		"recipients": recipientSet(recipients)})
}

func (c *Client) UpdateForwarder(ctx context.Context, id string, recipients []string) error {
	return c.update(ctx, "x:MailingList", id, map[string]any{"recipients": recipientSet(recipients)})
}

func (c *Client) DeleteForwarder(ctx context.Context, id string) error {
	return c.destroy(ctx, "x:MailingList", id)
}
