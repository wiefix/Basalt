package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/store"
)

// Events a channel can subscribe to.
const (
	EventHealthDown   = "health.down"
	EventHealthUp     = "health.up"
	EventDeployFailed = "deploy.failed"
	EventBackupFailed = "backup.failed"
)

var notificationEvents = []struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}{
	{EventHealthDown, "A health check goes down"},
	{EventHealthUp, "A health check is back up"},
	{EventDeployFailed, "A deployment fails"},
	{EventBackupFailed, "A backup fails (sites, apps, the panel)"},
}

func validEvent(e string) bool {
	return slices.ContainsFunc(notificationEvents, func(x struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}) bool {
		return x.ID == e
	})
}

// Delivery: attempts and the pauses before them; at most notifyWorkers
// deliveries at once (more wait).
var notifyRetries = []time.Duration{0, 10 * time.Second, time.Minute}

const (
	notifyWorkers = 8
	notifyTimeout = 30 * time.Second
	telegramAPI   = "https://api.telegram.org"
)

// channelConfig is a channel's settings, stored encrypted as a whole: the
// webhook URLs of Slack and Discord, bot tokens and passwords are secrets.
type channelConfig struct {
	URL          string       `json:"url,omitempty"`    // webhook, slack, discord
	Secret       string       `json:"secret,omitempty"` // webhook: HMAC key
	AllowPrivate bool         `json:"allow_private,omitempty"`
	Token        string       `json:"token,omitempty"`   // telegram bot
	ChatID       string       `json:"chat_id,omitempty"` // telegram
	SMTP         *notify.SMTP `json:"smtp,omitempty"`    // email
}

func channelAD(id int64) []byte { return []byte("notification:" + strconv.FormatInt(id, 10)) }

func (s *Server) channelConfig(c *store.NotificationChannel) (*channelConfig, error) {
	b, err := s.box.Open(c.Config, channelAD(c.ID))
	if err != nil {
		return nil, err
	}
	var cfg channelConfig
	return &cfg, json.Unmarshal(b, &cfg)
}

// channelView shows a channel without its secrets.
type channelView struct {
	ID     int64    `json:"id"`
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	Events []string `json:"events"`
	// Settings without secrets.
	URL          string   `json:"url,omitempty"` // webhook (Slack and Discord URLs are secret)
	Secret       string   `json:"secret,omitempty"`
	AllowPrivate bool     `json:"allow_private,omitempty"`
	ChatID       string   `json:"chat_id,omitempty"`
	SMTPHost     string   `json:"smtp_host,omitempty"`
	SMTPPort     int      `json:"smtp_port,omitempty"`
	SMTPTLS      string   `json:"smtp_tls,omitempty"`
	SMTPUser     string   `json:"smtp_user,omitempty"`
	SMTPInsecure bool     `json:"smtp_insecure,omitempty"`
	From         string   `json:"from,omitempty"`
	To           []string `json:"to,omitempty"`
	// Where a secret URL points, to recognise it.
	Host string `json:"host,omitempty"`

	LastSentAt  *time.Time `json:"last_sent_at"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
}

func (s *Server) viewChannel(c *store.NotificationChannel) channelView {
	v := channelView{ID: c.ID, Name: c.Name, Kind: c.Kind, Events: nonNil(c.Events),
		LastSentAt: c.LastSentAt, LastError: c.LastError, LastErrorAt: c.LastErrorAt}
	cfg, err := s.channelConfig(c)
	if err != nil {
		v.LastError = "cannot decrypt the settings"
		return v
	}
	switch c.Kind {
	case "webhook":
		v.URL, v.Secret, v.AllowPrivate = cfg.URL, cfg.Secret, cfg.AllowPrivate
	case "slack", "discord":
		if u, err := url.Parse(cfg.URL); err == nil {
			v.Host = u.Host
		}
	case "telegram":
		v.ChatID = cfg.ChatID
	case "email":
		if m := cfg.SMTP; m != nil {
			v.SMTPHost, v.SMTPPort, v.SMTPTLS, v.SMTPUser, v.SMTPInsecure, v.From, v.To =
				m.Host, m.Port, m.TLS, m.Username, m.Insecure, m.From, m.To
		}
	}
	return v
}

// channelRequest creates or changes a channel. An empty secret field
// (Slack/Discord URL, token, SMTP password) keeps the current one.
type channelRequest struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Events       []string `json:"events"`
	URL          string   `json:"url"`
	Secret       *string  `json:"secret"` // webhook: nil keeps it, "" removes it
	AllowPrivate bool     `json:"allow_private"`
	Token        string   `json:"token"`
	ChatID       string   `json:"chat_id"`
	SMTPHost     string   `json:"smtp_host"`
	SMTPPort     int      `json:"smtp_port"`
	SMTPTLS      string   `json:"smtp_tls"`
	SMTPUser     string   `json:"smtp_user"`
	SMTPPassword string   `json:"smtp_password"`
	SMTPInsecure bool     `json:"smtp_insecure"`
	From         string   `json:"from"`
	To           []string `json:"to"`
}

var chatIDOK = func(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	if s[0] == '@' { // a public channel's username
		return len(s) > 1 && strings.Trim(s[1:], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") == ""
	}
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func validHookURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || len(raw) > 1000 || strings.ContainsAny(raw, " \r\n") {
		return errors.New("the URL must be like https://example.com/hook")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && allowPrivate) {
		return errors.New("the URL must use https (http only on a private network)")
	}
	return nil
}

// config builds a channel's settings from the request; old holds the
// current ones (nil for a new channel).
func (req *channelRequest) config(old *channelConfig) (*channelConfig, error) {
	cfg := &channelConfig{}
	if old != nil {
		*cfg = *old
	}
	switch req.Kind {
	case "webhook":
		cfg.URL, cfg.AllowPrivate = strings.TrimSpace(req.URL), req.AllowPrivate
		if err := validHookURL(cfg.URL, cfg.AllowPrivate); err != nil {
			return nil, err
		}
		if req.Secret != nil {
			cfg.Secret = *req.Secret
		} else if old == nil {
			cfg.Secret = randomHex(24)
		}
		if len(cfg.Secret) > 200 {
			return nil, errors.New("the secret is too long")
		}
	case "slack", "discord":
		if u := strings.TrimSpace(req.URL); u != "" || old == nil {
			cfg.URL = u
		}
		if err := validHookURL(cfg.URL, false); err != nil {
			return nil, err
		}
		host, _ := url.Parse(cfg.URL)
		if req.Kind == "slack" && host.Hostname() != "hooks.slack.com" {
			return nil, errors.New("a Slack webhook URL starts with https://hooks.slack.com/")
		}
		if req.Kind == "discord" && host.Hostname() != "discord.com" && host.Hostname() != "discordapp.com" {
			return nil, errors.New("a Discord webhook URL starts with https://discord.com/api/webhooks/")
		}
	case "telegram":
		if t := strings.TrimSpace(req.Token); t != "" || old == nil {
			cfg.Token = t
		}
		if i := strings.IndexByte(cfg.Token, ':'); i < 1 || len(cfg.Token) > 100 ||
			strings.Trim(cfg.Token, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789:_-") != "" {
			return nil, errors.New("the bot token looks like 123456789:AA… (from @BotFather)")
		}
		cfg.ChatID = strings.TrimSpace(req.ChatID)
		if !chatIDOK(cfg.ChatID) {
			return nil, errors.New("the chat ID is a number (e.g. -1001234567890) or @channelname")
		}
	case "email":
		m := &notify.SMTP{Host: strings.TrimSpace(req.SMTPHost), Port: req.SMTPPort, TLS: req.SMTPTLS,
			Username: strings.TrimSpace(req.SMTPUser), Password: req.SMTPPassword, Insecure: req.SMTPInsecure,
			From: strings.TrimSpace(req.From), To: req.To}
		if m.Password == "" && old != nil && old.SMTP != nil && m.Username != "" {
			m.Password = old.SMTP.Password
		}
		if m.Username == "" {
			m.Password = ""
		}
		for i := range m.To {
			m.To[i] = strings.TrimSpace(m.To[i])
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		if m.Username != "" && m.Password == "" {
			return nil, errors.New("give the SMTP password")
		}
		cfg.SMTP = m
	default:
		return nil, errors.New("kind is webhook, slack, discord, telegram or email")
	}
	return cfg, nil
}

func (req *channelRequest) check() error {
	req.Name = strings.TrimSpace(req.Name)
	if !serverNameRE.MatchString(req.Name) {
		return errors.New("name must be 1-63 characters: letters, digits, dot, dash, underscore")
	}
	slices.Sort(req.Events)
	req.Events = slices.Compact(req.Events)
	for _, e := range req.Events {
		if !validEvent(e) {
			return fmt.Errorf("unknown event %q", e)
		}
	}
	return nil
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.db.NotificationChannels(r.Context())
	if err != nil {
		s.internalError(w, r, "list notification channels", err)
		return
	}
	out := make([]channelView, len(list))
	for i, c := range list {
		out[i] = s.viewChannel(c)
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": out, "events": notificationEvents})
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request, p *principal) {
	var req channelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.check(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	cfg, err := req.config(nil)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// The config is sealed with the channel's ID: insert, then store it.
	c := &store.NotificationChannel{Name: req.Name, Kind: req.Kind, Events: req.Events, Config: []byte{}}
	if err := s.db.CreateNotificationChannel(r.Context(), c); errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "a channel with this name exists")
		return
	} else if err != nil {
		s.internalError(w, r, "create notification channel", err)
		return
	}
	if !s.saveChannel(w, r, c, cfg) {
		s.db.DeleteNotificationChannel(context.WithoutCancel(r.Context()), c.ID)
		return
	}
	s.audit(r, &p.user.ID, "notification.created", map[string]any{"channel_id": c.ID, "name": c.Name, "kind": c.Kind, "events": c.Events})
	writeJSON(w, http.StatusCreated, map[string]any{"channel": s.viewChannel(c)})
}

func (s *Server) saveChannel(w http.ResponseWriter, r *http.Request, c *store.NotificationChannel, cfg *channelConfig) bool {
	b, err := json.Marshal(cfg)
	if err != nil {
		s.internalError(w, r, "encode channel", err)
		return false
	}
	c.Config = s.box.Seal(b, channelAD(c.ID))
	if err := s.db.UpdateNotificationChannel(r.Context(), c); errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "a channel with this name exists")
		return false
	} else if err != nil {
		s.internalError(w, r, "save notification channel", err)
		return false
	}
	return true
}

func (s *Server) pathChannel(w http.ResponseWriter, r *http.Request) (*store.NotificationChannel, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		c, err := s.db.NotificationChannel(r.Context(), id)
		if err == nil {
			return c, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "load notification channel", err)
			return nil, false
		}
	}
	writeError(w, http.StatusNotFound, "channel not found")
	return nil, false
}

func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request, p *principal) {
	c, ok := s.pathChannel(w, r)
	if !ok {
		return
	}
	var req channelRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Kind == "" {
		req.Kind = c.Kind
	}
	if req.Kind != c.Kind {
		writeError(w, http.StatusUnprocessableEntity, "the kind of a channel cannot change")
		return
	}
	if err := req.check(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	old, err := s.channelConfig(c)
	if err != nil {
		s.internalError(w, r, "decrypt channel", err)
		return
	}
	cfg, err := req.config(old)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	c.Name, c.Events = req.Name, req.Events
	if !s.saveChannel(w, r, c, cfg) {
		return
	}
	s.audit(r, &p.user.ID, "notification.updated", map[string]any{"channel_id": c.ID, "name": c.Name, "events": c.Events})
	writeJSON(w, http.StatusOK, map[string]any{"channel": s.viewChannel(c)})
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request, p *principal) {
	c, ok := s.pathChannel(w, r)
	if !ok {
		return
	}
	if err := s.db.DeleteNotificationChannel(r.Context(), c.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, "delete notification channel", err)
		return
	}
	s.audit(r, &p.user.ID, "notification.deleted", map[string]any{"channel_id": c.ID, "name": c.Name})
	w.WriteHeader(http.StatusNoContent)
}

// handleTestChannel sends a test message now and reports the outcome.
func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request, p *principal) {
	c, ok := s.pathChannel(w, r)
	if !ok {
		return
	}
	m := notify.Message{Event: "test", Title: "Test notification from Basalt",
		Text: "This channel receives the panel's notifications.", At: s.now()}
	ctx, cancel := context.WithTimeout(r.Context(), notifyTimeout)
	defer cancel()
	err := s.deliver(ctx, c, m)
	s.recordDelivery(c.ID, err)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// deliver sends one message through one channel.
func (s *Server) deliver(ctx context.Context, c *store.NotificationChannel, m notify.Message) error {
	cfg, err := s.channelConfig(c)
	if err != nil {
		return errors.New("cannot decrypt the channel's settings")
	}
	hook := func(allowPrivate bool) *http.Client {
		return outboundClient(allowPrivate, notifyTimeout, func(u *url.URL) error {
			return validHookURL(u.String(), allowPrivate)
		}, "allow private addresses for a webhook on your network")
	}
	switch c.Kind {
	case "webhook":
		return notify.Webhook(ctx, hook(cfg.AllowPrivate), cfg.URL, cfg.Secret, m)
	case "slack":
		return notify.Slack(ctx, hook(s.notifyPrivate), cfg.URL, m)
	case "discord":
		return notify.Discord(ctx, hook(s.notifyPrivate), cfg.URL, m)
	case "telegram":
		return notify.Telegram(ctx, hook(s.notifyPrivate), s.telegramAPI, cfg.Token, cfg.ChatID, m)
	case "email":
		if cfg.SMTP == nil {
			return errors.New("no SMTP settings")
		}
		return notify.Email(ctx, *cfg.SMTP, m)
	}
	return fmt.Errorf("unknown kind %q", c.Kind)
}

func (s *Server) recordDelivery(id int64, err error) {
	msg := ""
	if err != nil {
		msg = truncate(err.Error(), 500)
	}
	if err := s.db.SetNotificationResult(context.Background(), id, msg, s.now()); err != nil {
		s.log.Error("record notification result", "err", err)
	}
}

// notify sends m to every channel subscribed to its event, in the
// background, retrying a failed delivery. It never blocks the caller.
func (s *Server) notify(m notify.Message) {
	if m.At.IsZero() {
		m.At = s.now()
	}
	go func() {
		ctx := context.Background()
		list, err := s.db.NotificationChannels(ctx)
		if err != nil {
			s.log.Error("list notification channels", "err", err)
			return
		}
		for _, c := range list {
			if !slices.Contains(c.Events, m.Event) {
				continue
			}
			go s.deliverWithRetries(c, m)
		}
	}()
}

func (s *Server) deliverWithRetries(c *store.NotificationChannel, m notify.Message) {
	var err error
	for _, pause := range s.notifyRetries {
		time.Sleep(pause)
		s.notifySem <- struct{}{}
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		err = s.deliver(ctx, c, m)
		cancel()
		<-s.notifySem
		if err == nil {
			break
		}
		s.log.Warn("notification failed", "channel", c.Name, "event", m.Event, "err", err)
	}
	s.recordDelivery(c.ID, err)
}

// resourceName names what a backup is of, for messages.
func (s *Server) resourceName(b *store.Backup) string {
	ctx := context.Background()
	switch {
	case b.SiteID != 0:
		if site, err := s.db.SiteByID(ctx, b.SiteID); err == nil {
			return "site " + site.Name
		}
		return fmt.Sprintf("site #%d", b.SiteID)
	case b.AppID != 0:
		if app, err := s.db.AppByID(ctx, b.AppID); err == nil {
			return app.Name
		}
		return fmt.Sprintf("app #%d", b.AppID)
	}
	return fmt.Sprintf("backup #%d", b.ID)
}
