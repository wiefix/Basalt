// Package notify sends panel events (a health check going down, a failed
// deployment or backup) to a channel: a webhook, Slack, Discord, Telegram
// or email.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Message is one event.
type Message struct {
	Event string    `json:"event"` // e.g. health.down
	Title string    `json:"title"` // one line
	Text  string    `json:"text"`  // details, may be several lines
	At    time.Time `json:"at"`
	// Link is the panel page about it ("" when unknown).
	Link string `json:"link,omitempty"`
	// Project is the project of the resource it is about (0: none); the
	// panel picks the channels by it.
	Project int64 `json:"-"`
}

// plain is the message as text for chats and email.
func (m Message) plain() string {
	s := m.Title
	if m.Text != "" {
		s += "\n" + m.Text
	}
	if m.Link != "" {
		s += "\n" + m.Link
	}
	return s
}

// Webhook posts the message as JSON. With a secret, the body is signed:
// X-Basalt-Signature: sha256=<hex HMAC-SHA256 of the body>.
func Webhook(ctx context.Context, hc *http.Client, url, secret string, m Message) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	h := map[string]string{"X-Basalt-Event": m.Event}
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		h["X-Basalt-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	return post(ctx, hc, url, body, h)
}

// Slack posts to a Slack incoming webhook.
func Slack(ctx context.Context, hc *http.Client, url string, m Message) error {
	body, _ := json.Marshal(map[string]string{"text": m.plain()})
	return post(ctx, hc, url, body, nil)
}

// Discord posts to a Discord webhook (2000 characters at most).
func Discord(ctx context.Context, hc *http.Client, url string, m Message) error {
	body, _ := json.Marshal(map[string]any{"content": truncate(m.plain(), 2000),
		"allowed_mentions": map[string]any{"parse": []string{}}})
	return post(ctx, hc, url, body, nil)
}

// Telegram sends through a bot (api is https://api.telegram.org).
func Telegram(ctx context.Context, hc *http.Client, api, token, chatID string, m Message) error {
	body, _ := json.Marshal(map[string]any{"chat_id": chatID, "text": truncate(m.plain(), 4000),
		"disable_web_page_preview": true})
	err := post(ctx, hc, strings.TrimSuffix(api, "/")+"/bot"+token+"/sendMessage", body, nil)
	if err != nil {
		// The URL holds the token: never pass it on in an error.
		return errors.New(strings.ReplaceAll(err.Error(), token, "<token>"))
	}
	return nil
}

func post(ctx context.Context, hc *http.Client, url string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Basalt-Notifications")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(printable(string(answer))))
	}
	return nil
}

// SMTP is a mail server notifications are sent through.
type SMTP struct {
	Host     string
	Port     int
	TLS      string // "implicit" (usually 465), "starttls" (587) or "none" (25, local relays)
	Username string
	Password string
	From     string
	To       []string
	Insecure bool // do not verify the server's certificate
	// Dialer, when set, connects to the server (it may refuse private
	// addresses).
	Dialer *net.Dialer `json:"-"`
}

// Validate checks the settings.
func (c *SMTP) Validate() error {
	switch {
	case c.Host == "" || len(c.Host) > 253 || strings.ContainsAny(c.Host, " /\r\n"):
		return errors.New("invalid SMTP host")
	case c.Port < 1 || c.Port > 65535:
		return errors.New("invalid SMTP port")
	case c.TLS != "implicit" && c.TLS != "starttls" && c.TLS != "none":
		return errors.New(`tls is "implicit", "starttls" or "none"`)
	case c.TLS == "none" && c.Username != "":
		return errors.New("a login needs TLS: the password would travel in clear")
	case len(c.To) == 0 || len(c.To) > 20:
		return errors.New("give 1 to 20 recipients")
	}
	if err := address(c.From); err != nil {
		return fmt.Errorf("from: %w", err)
	}
	for _, to := range c.To {
		if err := address(to); err != nil {
			return fmt.Errorf("recipient %q: %w", to, err)
		}
	}
	if strings.ContainsAny(c.Username+c.Password, "\r\n") {
		return errors.New("invalid login")
	}
	return nil
}

// address accepts a bare address (no display name, no line breaks).
func address(s string) error {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s || strings.ContainsAny(s, "\r\n<>") {
		return errors.New("not an email address")
	}
	return nil
}

// Email sends the message through c.
func Email(ctx context.Context, c SMTP, m Message) error {
	if err := c.Validate(); err != nil {
		return err
	}
	addr := net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	d := &net.Dialer{Timeout: 15 * time.Second}
	if c.Dialer != nil {
		d = c.Dialer
	}
	var conn net.Conn
	var err error
	tlsConf := &tls.Config{ServerName: c.Host, InsecureSkipVerify: c.Insecure, MinVersion: tls.VersionTLS12}
	if c.TLS == "implicit" {
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsConf}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(time.Minute))
	}
	cl, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		return err
	}
	defer cl.Close()
	if err := cl.Hello("basalt"); err != nil {
		return err
	}
	if c.TLS == "starttls" {
		if ok, _ := cl.Extension("STARTTLS"); !ok {
			return errors.New("the server does not offer STARTTLS")
		}
		if err := cl.StartTLS(tlsConf); err != nil {
			return err
		}
	}
	if c.Username != "" {
		// PlainAuth refuses to send the password without TLS, which the
		// settings already guarantee.
		if err := cl.Auth(smtp.PlainAuth("", c.Username, c.Password, c.Host)); err != nil {
			return err
		}
	}
	if err := cl.Mail(c.From); err != nil {
		return err
	}
	for _, to := range c.To {
		if err := cl.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := cl.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(emailBody(c, m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return cl.Quit()
}

func emailBody(c SMTP, m Message) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: Basalt <%s>\r\n", c.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(c.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mimeHeader("[Basalt] "+m.Title))
	fmt.Fprintf(&b, "Date: %s\r\n", m.At.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n")
	fmt.Fprintf(&b, "X-Basalt-Event: %s\r\n\r\n", m.Event)
	// The DATA writer escapes lines starting with a dot.
	for line := range strings.SplitSeq(m.plain(), "\n") {
		b.WriteString(strings.TrimRight(line, "\r") + "\r\n")
	}
	return b.Bytes()
}

// mimeHeader encodes a header value that is not plain ASCII.
func mimeHeader(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	for _, r := range s {
		if r > 126 || r < 32 {
			return "=?utf-8?b?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
		}
	}
	return s
}

// truncate keeps s within n bytes (limits counted in characters are then
// met too), cut on a character boundary and marked with an ellipsis.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, s)
}
