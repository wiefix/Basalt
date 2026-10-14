package notify

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var msg = Message{Event: "health.down", Title: "shop is down", Text: "2 of 3 servers fail: HTTP 503",
	At: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC), Link: "https://panel.example.com/monitoring"}

type captured struct {
	path    string
	headers http.Header
	body    []byte
}

func capture(t *testing.T, status int) (*httptest.Server, chan captured) {
	ch := make(chan captured, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- captured{r.URL.Path, r.Header, b}
		w.WriteHeader(status)
		io.WriteString(w, "nope\x00\n")
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func TestHTTPChannels(t *testing.T) {
	ctx := context.Background()
	srv, ch := capture(t, 200)

	// Webhook: the message as JSON, signed with the secret.
	if err := Webhook(ctx, srv.Client(), srv.URL+"/hook", "s3cret", msg); err != nil {
		t.Fatal(err)
	}
	c := <-ch
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(c.body)
	if c.headers.Get("X-Basalt-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) ||
		c.headers.Get("X-Basalt-Event") != "health.down" || c.headers.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", c.headers)
	}
	var got Message
	if err := json.Unmarshal(c.body, &got); err != nil || got.Title != msg.Title || got.Link != msg.Link || !got.At.Equal(msg.At) {
		t.Fatalf("body %s", c.body)
	}
	// Without a secret there is no signature.
	Webhook(ctx, srv.Client(), srv.URL, "", msg)
	if c = <-ch; c.headers.Get("X-Basalt-Signature") != "" {
		t.Fatal("signed without a secret")
	}

	want := "shop is down\n2 of 3 servers fail: HTTP 503\nhttps://panel.example.com/monitoring"
	Slack(ctx, srv.Client(), srv.URL, msg)
	if c = <-ch; string(c.body) != `{"text":`+strconv.Quote(want)+`}` {
		t.Fatalf("slack %s", c.body)
	}
	Discord(ctx, srv.Client(), srv.URL, Message{Title: strings.Repeat("é", 1500)})
	var d struct {
		Content string `json:"content"`
	}
	if c = <-ch; json.Unmarshal(c.body, &d) != nil || len(d.Content) > 2000 || !strings.HasSuffix(d.Content, "é…") ||
		!strings.Contains(string(c.body), `"allowed_mentions":{"parse":[]}`) {
		t.Fatalf("discord %s", c.body)
	}
	if err := Telegram(ctx, srv.Client(), srv.URL+"/", "123:ABC", "-100200", msg); err != nil {
		t.Fatal(err)
	}
	if c = <-ch; c.path != "/bot123:ABC/sendMessage" || !strings.Contains(string(c.body), `"chat_id":"-100200"`) {
		t.Fatalf("telegram %s %s", c.path, c.body)
	}

	// A refusal comes back with its status and a cleaned excerpt.
	bad, _ := capture(t, 403)
	if err := Slack(ctx, bad.Client(), bad.URL, msg); err == nil || err.Error() != "HTTP 403: nope" {
		t.Fatalf("refusal: %v", err)
	}
	// Errors never carry the bot token, which is part of the URL.
	err := Telegram(ctx, &http.Client{Timeout: time.Second}, "http://127.0.0.1:1", "123:SECRET", "1", msg)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("telegram error: %v", err)
	}
}

// fakeSMTP accepts one message; with cert, the connection is TLS from the
// start. It records the login and the data.
func fakeSMTP(t *testing.T, cert *tls.Certificate) (int, chan string) {
	var l net.Listener
	var err error
	if cert != nil {
		l, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{*cert}})
	} else {
		l, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	out := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		say := func(s string) { io.WriteString(conn, s+"\r\n") }
		var log strings.Builder
		say("220 fake")
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250-fake")
				say("250 AUTH PLAIN")
			case strings.HasPrefix(cmd, "AUTH PLAIN"):
				raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(cmd, "AUTH PLAIN "))
				log.WriteString("auth " + strings.ReplaceAll(string(raw), "\x00", "|") + "\n")
				say("235 ok")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				log.WriteString(cmd + "\n")
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				for {
					l, _ := r.ReadString('\n')
					if l == ".\r\n" {
						break
					}
					log.WriteString(l)
				}
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				out <- log.String()
				return
			default:
				say("502 ?")
			}
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, out
}

func TestEmail(t *testing.T) {
	ctx := context.Background()
	m := msg
	m.Title = "Деплой не удался"
	m.Text = ".starts with a dot"

	port, out := fakeSMTP(t, nil)
	c := SMTP{Host: "127.0.0.1", Port: port, TLS: "none", From: "panel@example.com", To: []string{"ops@example.com", "me@example.org"}}
	if err := Email(ctx, c, m); err != nil {
		t.Fatal(err)
	}
	got := <-out
	for _, want := range []string{"MAIL FROM:<panel@example.com>", "RCPT TO:<ops@example.com>", "RCPT TO:<me@example.org>",
		"Subject: =?utf-8?b?", "X-Basalt-Event: health.down", "\r\n..starts with a dot\r\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}

	// Implicit TLS with a login.
	tlsSrv := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsSrv.Close()
	port, out = fakeSMTP(t, &tlsSrv.TLS.Certificates[0])
	c = SMTP{Host: "127.0.0.1", Port: port, TLS: "implicit", Username: "u", Password: "p", From: "panel@example.com",
		To: []string{"ops@example.com"}, Insecure: true}
	if err := Email(ctx, c, m); err != nil {
		t.Fatal(err)
	}
	if got := <-out; !strings.Contains(got, "auth |u|p") {
		t.Fatalf("no login:\n%s", got)
	}

	for _, bad := range []SMTP{
		{Host: "", Port: 25, TLS: "none", From: "a@b.c", To: []string{"d@e.f"}},
		{Host: "h", Port: 0, TLS: "none", From: "a@b.c", To: []string{"d@e.f"}},
		{Host: "h", Port: 25, TLS: "ssl", From: "a@b.c", To: []string{"d@e.f"}},
		{Host: "h", Port: 25, TLS: "none", Username: "u", From: "a@b.c", To: []string{"d@e.f"}},
		{Host: "h", Port: 25, TLS: "none", From: "A <a@b.c>", To: []string{"d@e.f"}},
		{Host: "h", Port: 25, TLS: "none", From: "a@b.c", To: []string{"d@e.f\r\nBcc: x@y.z"}},
		{Host: "h", Port: 25, TLS: "none", From: "a@b.c"},
		{Host: "h\r\n", Port: 25, TLS: "none", From: "a@b.c", To: []string{"d@e.f"}},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
