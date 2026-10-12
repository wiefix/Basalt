package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/wiefix/basalt/internal/agent"
	"github.com/wiefix/basalt/internal/docker"
	"github.com/wiefix/basalt/internal/server"
)

// TestMailWithDocker runs the mail server (Stalwart) on the local Docker:
// set up, a domain with its DNS records in a Basalt zone, mailboxes,
// delivery over SMTP, reading over JMAP through the proxy, a redeployment
// that keeps the mail. Opt-in.
func TestMailWithDocker(t *testing.T) {
	if os.Getenv("BASALT_DOCKER_TESTS") != "1" {
		t.Skip("set BASALT_DOCKER_TESTS=1 to run against the local Docker daemon")
	}
	sock := os.Getenv("DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	smtpPort := freePort(t)
	ports := map[int]int{25: smtpPort, 465: freePort(t), 587: freePort(t), 993: freePort(t)}
	p := startPanelWith(t, server.Options{MailHostPorts: ports})
	// The agent's own DNS server is the resolver of its deliverability
	// checks, and the mail server the SMTP server it reaches on port 25.
	dnsListen := "127.0.0.1:" + strconv.Itoa(freePort(t))
	h := startHostWith(t, p, sock, func(c *agent.Config) {
		c.DNSListen, c.Resolvers = dnsListen, []string{dnsListen}
		c.MailCheckProbes = []string{"127.0.0.1:" + strconv.Itoa(smtpPort)}
	})
	var dnsAddr string
	for _, s := range p.call(200, "GET", "/api/v1/dns", nil)["servers"].([]any) {
		dnsAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(int(s.(map[string]any)["port"].(float64))))
	}
	p.call(200, "PUT", "/api/v1/servers/"+h.id+"/public-ips", map[string]any{"ips": []string{"127.0.0.1"}})
	p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]string{"name": "example.test"})

	// Before setup, nothing works.
	p.call(http.StatusConflict, "POST", "/api/v1/mail/domains", map[string]string{"name": "example.test"})
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/mail", map[string]any{"server_id": mustAtoi(h.id), "hostname": "mail"})
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/apps", map[string]any{"server_id": mustAtoi(h.id), "name": "x", "source": "mail"})

	r := p.call(http.StatusCreated, "POST", "/api/v1/mail", map[string]any{"server_id": mustAtoi(h.id), "hostname": "Mail.Example.test"})
	app := r["server"].(map[string]any)
	aid := jsonID(app["id"])
	t.Cleanup(func() {
		ctx := context.Background()
		dc := docker.New(sock)
		list, _ := dc.ContainerList(ctx, map[string]string{"basalt.app": aid})
		for _, c := range list {
			dc.ContainerRemove(ctx, c.ID, true)
		}
		dc.NetworkRemove(ctx, "basalt-app-"+aid)
		dc.VolumeRemove(ctx, "basalt-app-"+aid+"-etc")
		dc.VolumeRemove(ctx, "basalt-app-"+aid+"-data")
	})
	if r["deployment"] == nil {
		t.Fatalf("no deployment: %v", r)
	}
	p.call(http.StatusConflict, "POST", "/api/v1/mail", map[string]any{"server_id": mustAtoi(h.id), "hostname": "mx.example.test"})
	base := "/api/v1/apps/" + aid
	d, log := followDeployment(t, p, base, jsonID(r["deployment"].(map[string]any)["id"]))
	if d["status"] != "succeeded" {
		t.Fatalf("mail deployment: %v\n%s", d, log)
	}
	// The administrator password is stored encrypted only.
	if env := p.call(http.StatusUnprocessableEntity, "PUT", base+"/env", map[string]any{"env": map[string]string{}}); env["error"] == nil {
		t.Fatal("mail env editable")
	}

	// A domain: the mail server's DNS records land in the Basalt zone.
	p.call(http.StatusUnprocessableEntity, "POST", "/api/v1/mail/domains", map[string]string{"name": "not a domain"})
	var dom map[string]any
	eventually(t, "mail server configured", func() bool {
		code, _, body := p.raw("POST", "/api/v1/mail/domains", "application/json", []byte(`{"name":"example.test"}`))
		if code == http.StatusCreated {
			var r map[string]any
			json.Unmarshal(body, &r)
			dom = r["domain"].(map[string]any)
		}
		return code == http.StatusCreated
	})
	if len(dom["dns_records"].([]any)) < 6 {
		t.Fatalf("domain = %v", dom)
	}
	did := jsonID(dom["id"])
	ask := func(name string, qtype uint16) []string {
		_, ans := dnsQuery(t, dnsAddr, name, qtype)
		return ans
	}
	eventually(t, "mail records served", func() bool {
		mx, a := ask("example.test.", mdns.TypeMX), ask("mail.example.test.", mdns.TypeA)
		return len(mx) == 1 && strings.Contains(mx[0], "MX 10 mail.example.test.") && len(a) == 1 && strings.Contains(a[0], "127.0.0.1")
	})
	for name, want := range map[string]string{"_dmarc.example.test.": "v=DMARC1", "example.test.": "v=spf1"} {
		if txt := ask(name, mdns.TypeTXT); len(txt) == 0 || !strings.Contains(strings.Join(txt, " "), want) {
			t.Errorf("%s TXT = %v", name, txt)
		}
	}
	dkim := false
	for _, rec := range dom["dns_records"].([]any) {
		rec := rec.(map[string]any)
		if strings.Contains(rec["name"].(string), "._domainkey.") {
			dkim = len(ask(rec["name"].(string)+".", mdns.TypeTXT)) == 1
		}
	}
	if !dkim {
		t.Error("DKIM key not served")
	}
	// mta-sts and autoconfig are served by the proxy.
	eventually(t, "mail host names routed", func() bool {
		a := p.call(200, "GET", base, nil)["app"].(map[string]any)
		return strings.Contains(jsonString(a["domains"]), "mta-sts.example.test")
	})

	// Deliverability checks: everything the panel controls passes; the
	// loopback address has no reverse DNS or blocklist entries to check.
	eventually(t, "deliverability checks pass", func() bool {
		checks := p.call(200, "POST", "/api/v1/mail/checks", nil)["checks"].(map[string]any)
		got := map[string]string{}
		for _, c := range checks["results"].([]any) {
			c := c.(map[string]any)
			key := c["check"].(string)
			if d, _ := c["domain"].(string); d != "" {
				key = d + " " + key
			}
			got[key] = c["status"].(string)
			if c["status"] != "ok" {
				t.Logf("%s: %s %s", key, c["status"], c["detail"])
			}
		}
		want := map[string]string{"outbound_smtp": "ok", "hostname": "ok", "ptr": "unknown", "blocklist": "unknown",
			"example.test mx": "ok", "example.test spf": "ok", "example.test dkim": "ok", "example.test dmarc": "ok"}
		return checks["error"] == nil && jsonString(got) == jsonString(want)
	})
	if checks := p.call(200, "GET", "/api/v1/mail", nil)["checks"].(map[string]any); len(checks["results"].([]any)) != 8 {
		t.Fatalf("stored checks = %v", checks)
	}

	// The mail server logs to the container log (its default is a file
	// inside the container).
	// An SMTP connection is logged; logging switches over with the
	// settings reload after the deployment, so give it time.
	logged := false
	for deadline := time.Now().Add(time.Minute); !logged && time.Now().Before(deadline); time.Sleep(time.Second) {
		if c, err := smtp.Dial("127.0.0.1:" + strconv.Itoa(smtpPort)); err == nil {
			c.Hello("client.example.net")
			c.Quit()
		}
		logged = strings.Contains(p.call(200, "GET", base+"/logs?tail=200", nil)["log"].(string), " INFO ")
	}
	if !logged {
		t.Fatal("the mail server does not log to the container log")
	}

	// Mailboxes.
	mb := "/api/v1/mail/domains/" + did + "/mailboxes"
	p.call(http.StatusUnprocessableEntity, "POST", mb, map[string]any{"local": "Bad Name", "password": "long enough pass"})
	p.call(http.StatusUnprocessableEntity, "POST", mb, map[string]any{"local": "alice", "password": "short"})
	created := p.call(http.StatusCreated, "POST", mb, map[string]any{"local": "alice", "name": "Alice", "password": "alice mail pass 1",
		"quota_mb": 512, "aliases": []string{"info"}})["mailbox"].(map[string]any)
	if created["address"] != "alice@example.test" {
		t.Fatalf("mailbox = %v", created)
	}
	p.call(http.StatusConflict, "POST", mb, map[string]any{"local": "alice", "password": "another pass 123"})
	// An empty display name is no name (the UI always sends one).
	p.call(http.StatusCreated, "POST", mb, map[string]any{"local": "carol", "name": "", "password": "carol mail pass 1"})
	boxes := p.call(200, "GET", mb, nil)["mailboxes"].([]any)
	if len(boxes) != 2 || jsonString(boxes[0].(map[string]any)["aliases"]) != `["info"]` || boxes[0].(map[string]any)["quota_mb"].(float64) != 512 {
		t.Fatalf("mailboxes = %v", boxes)
	}
	box := boxes[0].(map[string]any)["id"].(string)

	// Mail to the alias is delivered to alice.
	msg := "From: tester@example.net\r\nTo: info@example.test\r\nSubject: hello from basalt\r\n\r\nIt works.\r\n"
	eventually(t, "smtp delivery", func() bool {
		err := sendMail("127.0.0.1:"+strconv.Itoa(smtpPort), "tester@example.net", "info@example.test", msg)
		if err != nil {
			t.Logf("smtp: %v", err)
		}
		return err == nil
	})
	// ...and alice reads it over JMAP, through the proxy on the mail host name.
	jmap := func(user, pass string) (int, string) {
		hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:"+strconv.Itoa(h.httpsPort))
			},
		}}
		req, _ := http.NewRequest("GET", "https://mail.example.test/jmap/session", nil)
		req.SetBasicAuth(user, pass)
		resp, err := hc.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		var sess struct {
			APIURL          string            `json:"apiUrl"`
			PrimaryAccounts map[string]string `json:"primaryAccounts"`
		}
		json.NewDecoder(resp.Body).Decode(&sess)
		if resp.StatusCode != 200 {
			return resp.StatusCode, ""
		}
		acc := sess.PrimaryAccounts["urn:ietf:params:jmap:mail"]
		body, _ := json.Marshal(map[string]any{
			"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
			"methodCalls": []any{
				[]any{"Email/query", map[string]any{"accountId": acc}, "q"},
				[]any{"Email/get", map[string]any{"accountId": acc, "#ids": map[string]any{"resultOf": "q", "name": "Email/query", "path": "/ids"}, "properties": []string{"subject"}}, "g"},
			}})
		req, _ = http.NewRequest("POST", "https://mail.example.test/jmap/", bytes.NewReader(body))
		req.SetBasicAuth(user, pass)
		req.Header.Set("Content-Type", "application/json")
		resp, err = hc.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	eventually(t, "mail read over JMAP", func() bool {
		code, body := jmap("alice@example.test", "alice mail pass 1")
		return code == 200 && strings.Contains(body, "hello from basalt")
	})
	if code, _ := jmap("alice@example.test", "wrong password 1"); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", code)
	}

	// Webmail from the catalog reaches the mail server on its own network
	// by the mail host name (the name the certificate has).
	wr := p.call(http.StatusCreated, "POST", "/api/v1/catalog/roundcube/install", map[string]any{
		"server_id": mustAtoi(h.id), "inputs": map[string]string{"DOMAIN": "webmail.localhost"}})
	if wr["attach_error"] != nil {
		t.Fatalf("install webmail: %v", wr)
	}
	wid := jsonID(wr["app"].(map[string]any)["id"])
	t.Cleanup(func() { p.call(200, "DELETE", "/api/v1/apps/"+wid+"?volumes=1", nil) })
	if d, log := followDeployment(t, p, "/api/v1/apps/"+wid, jsonID(wr["deployment"].(map[string]any)["id"])); d["status"] != "succeeded" {
		t.Fatalf("webmail deployment: %v\n%s", d, log)
	}
	if env := p.call(200, "GET", "/api/v1/apps/"+wid+"/env", nil)["env"].(map[string]any); env["MAIL_HOST"] != "mail.example.test" {
		t.Fatalf("webmail env = %v", env)
	}
	if links := jsonString(p.call(200, "GET", base, nil)["app"].(map[string]any)["links"]); links != "["+wid+"]" {
		t.Fatalf("mail server links = %s", links)
	}
	eventually(t, "webmail login page", func() bool {
		code, body := h.get("webmail.localhost", "/")
		return code == 200 && strings.Contains(body, "rcmloginuser")
	})
	dc := docker.New(sock)
	var webmail string
	for _, c := range func() []docker.ContainerSummary {
		l, _ := dc.ContainerList(context.Background(), map[string]string{"basalt.app": wid})
		return l
	}() {
		if c.State == "running" {
			webmail = c.ID
		}
	}
	// IMAPS from the webmail container to mail.example.test:993 answers
	// with the mail server's greeting (the certificate is the proxy's
	// internal one here, so it is not verified in this check).
	probe := `$c = stream_context_create(["ssl" => ["verify_peer" => false, "verify_peer_name" => false]]);
$s = stream_socket_client("ssl://mail.example.test:993", $e, $m, 5, STREAM_CLIENT_CONNECT, $c);
echo $s ? fgets($s) : "error: $m";`
	out, err := exec.Command("docker", "--host", "unix://"+sock, "exec", webmail, "php", "-r", probe).CombinedOutput()
	if err != nil || !strings.HasPrefix(string(out), "* OK") {
		t.Fatalf("imaps from webmail: %v %s", err, out)
	}

	// IMAP over TLS uses the proxy's certificate for the mail host name.
	imaps := "127.0.0.1:" + strconv.Itoa(ports[993])
	var issuer, greeting string
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", imaps,
			&tls.Config{ServerName: "mail.example.test", InsecureSkipVerify: true})
		if err == nil {
			cert := conn.ConnectionState().PeerCertificates[0]
			conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			b := make([]byte, 64)
			n, _ := conn.Read(b)
			conn.Close()
			if slices.Contains(cert.DNSNames, "mail.example.test") {
				issuer, greeting = cert.Issuer.CommonName, string(b[:n])
				break
			}
		}
		time.Sleep(time.Second)
	}
	if !strings.Contains(issuer, "Caddy") || !strings.HasPrefix(greeting, "* OK") {
		t.Fatalf("imaps: issuer %q greeting %q; tls %v", issuer, greeting, p.call(200, "GET", "/api/v1/mail", nil)["tls"])
	}
	if tlsState := p.call(200, "GET", "/api/v1/mail", nil)["tls"].(map[string]any); tlsState["not_after"] == nil {
		t.Fatalf("tls status = %v", tlsState)
	}

	// Password change, then the domain cannot go while it has mailboxes.
	p.call(http.StatusNoContent, "PUT", mb+"/"+box, map[string]any{"password": "new mail pass 22"})
	eventually(t, "new password works", func() bool {
		code, _ := jmap("alice@example.test", "new mail pass 22")
		return code == 200
	})
	p.call(http.StatusNotFound, "PUT", mb+"/nope", map[string]any{"password": "new mail pass 33"})
	p.call(http.StatusConflict, "DELETE", "/api/v1/mail/domains/"+did, nil)

	// Outgoing mail through a relay that requires STARTTLS and a login.
	relayHost, listen := "host.docker.internal", "127.0.0.1:0"
	if runtime.GOOS == "linux" {
		ni, err := docker.New(sock).NetworkInspect(context.Background(), "basalt-app-"+aid)
		if err != nil || len(ni.IPAM.Config) == 0 {
			t.Fatalf("app network: %v %v", ni, err)
		}
		relayHost, listen = ni.IPAM.Config[0].Gateway, "0.0.0.0:0"
	}
	sink := startSMTPSink(t, listen, "relayuser", "relay secret 1")
	settings := "/api/v1/mail/settings"
	relay := func(kv ...any) map[string]any {
		r := map[string]any{"host": relayHost, "port": sink.port(), "tls": "starttls"}
		for i := 0; i < len(kv); i += 2 {
			r[kv[i].(string)] = kv[i+1]
		}
		return map[string]any{"relay": r}
	}
	for _, bad := range []map[string]any{relay("host", "bad host"), relay("port", 0), relay("tls", "none"),
		relay("username", "relayuser"), relay("username", "relayuser", "password", "x\r\nRCPT"), {"send_limit": -1}} {
		p.call(http.StatusUnprocessableEntity, "PUT", settings, bad)
	}
	p.call(http.StatusBadRequest, "PUT", settings, relay("extra", true))
	ok := relay("username", "relayuser", "password", "relay secret 1", "insecure", true)
	ok["send_limit"] = 3
	p.call(200, "PUT", settings, ok)
	view := p.call(200, "GET", "/api/v1/mail", nil)["settings"].(map[string]any)
	if r := view["relay"].(map[string]any); r["has_password"] != true || r["username"] != "relayuser" || view["send_limit"].(float64) != 3 ||
		strings.Contains(jsonString(view), "secret") {
		t.Fatalf("settings = %v", view)
	}
	var stored string
	p.db.R.QueryRow(`SELECT value FROM settings WHERE key = 'mail_settings'`).Scan(&stored)
	if !strings.Contains(stored, relayHost) || strings.Contains(stored, "relay secret") {
		t.Fatalf("stored settings: %s", stored)
	}
	// Changing the limit alone keeps the relay and its password.
	p.call(200, "PUT", settings, map[string]any{"send_limit": 3})
	submit := func(to, subject string) error {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", "127.0.0.1:"+strconv.Itoa(ports[465]),
			&tls.Config{ServerName: "mail.example.test", InsecureSkipVerify: true})
		if err != nil {
			return err
		}
		c, err := smtp.NewClient(conn, "mail.example.test")
		if err != nil {
			return err
		}
		defer c.Close()
		if err := c.Hello("client.example.net"); err != nil {
			return err
		}
		if err := c.Auth(smtp.PlainAuth("", "alice@example.test", "new mail pass 22", "mail.example.test")); err != nil {
			return err
		}
		if err := c.Mail("alice@example.test"); err != nil {
			return err
		}
		if err := c.Rcpt(to); err != nil {
			return err
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		io.WriteString(w, "From: alice@example.test\r\nTo: "+to+"\r\nSubject: "+subject+"\r\n\r\nHi.\r\n")
		if err := w.Close(); err != nil {
			return err
		}
		return c.Quit()
	}
	// waitRelayed waits for a message at the relay (delivery is queued),
	// showing the mail server's log if it does not arrive.
	waitRelayed := func(rcpt, subject string) {
		t.Helper()
		for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			for _, m := range sink.messages() {
				if strings.Contains(strings.Join(m.To, " "), rcpt) && strings.Contains(m.Data, subject) {
					if m.User != "relayuser" || !m.TLS {
						t.Fatalf("relayed without TLS or login: %+v", m)
					}
					return
				}
			}
		}
		t.Fatalf("not relayed: %s to %s; relay got %d messages; mail server log:\n%s", subject, rcpt, len(sink.messages()),
			p.call(200, "GET", base+"/logs?tail=80", nil)["log"])
	}
	if err := submit("someone@example.net", "through the relay"); err != nil {
		t.Fatalf("submit: %v", err)
	}
	waitRelayed("someone@example.net", "through the relay")

	// Forwarders: to a mailbox and, through the relay, outside.
	fw := "/api/v1/mail/domains/" + did + "/forwarders"
	for _, bad := range []map[string]any{{"local": "sales", "recipients": []string{}}, {"local": "sales", "recipients": []string{"not an address"}},
		{"local": "sales", "recipients": []string{"Sales <x@example.net>"}}, {"local": "sales", "recipients": []string{"sales@example.test"}},
		{"local": "Bad Name", "recipients": []string{"x@example.net"}}} {
		p.call(http.StatusUnprocessableEntity, "POST", fw, bad)
	}
	p.call(http.StatusConflict, "POST", fw, map[string]any{"local": "alice", "recipients": []string{"x@example.net"}})
	fwd := p.call(http.StatusCreated, "POST", fw, map[string]any{"local": "sales", "recipients": []string{"Partner@Example.net", "alice@example.test"}})["forwarder"].(map[string]any)
	fid := fwd["id"].(string)
	if list := p.call(200, "GET", fw, nil)["forwarders"].([]any); len(list) != 1 ||
		jsonString(list[0].(map[string]any)["recipients"]) != `["alice@example.test","partner@example.net"]` {
		t.Fatalf("forwarders = %v", list)
	}
	p.call(http.StatusConflict, "POST", mb, map[string]any{"local": "sales", "password": "sales mail pass 1"})
	if err := sendMail("127.0.0.1:"+strconv.Itoa(smtpPort), "tester@example.net", "sales@example.test",
		"From: tester@example.net\r\nTo: sales@example.test\r\nSubject: forwarded order\r\n\r\nHi.\r\n"); err != nil {
		t.Fatalf("to forwarder: %v", err)
	}
	waitRelayed("partner@example.net", "forwarded order")
	eventually(t, "forwarded to the mailbox", func() bool {
		_, body := jmap("alice@example.test", "new mail pass 22")
		return strings.Contains(body, "forwarded order")
	})
	p.call(http.StatusNoContent, "PUT", fw+"/"+fid, map[string]any{"recipients": []string{"alice@example.test"}})
	p.call(http.StatusUnprocessableEntity, "PUT", fw+"/"+fid, map[string]any{"local": "other", "recipients": []string{"alice@example.test"}})
	p.call(http.StatusNotFound, "PUT", fw+"/nope", map[string]any{"recipients": []string{"alice@example.test"}})

	// Catch-all: unknown addresses reach alice while it is set.
	dom1 := "/api/v1/mail/domains/" + did
	p.call(http.StatusUnprocessableEntity, "PUT", dom1, map[string]any{"catch_all": "Bad Name"})
	p.call(http.StatusNoContent, "PUT", dom1, map[string]any{"catch_all": "alice"})
	if d := p.call(200, "GET", "/api/v1/mail", nil)["domains"].([]any)[0].(map[string]any); d["catch_all"] != "alice" {
		t.Fatalf("domain = %v", d)
	}
	eventually(t, "catch-all accepts", func() bool {
		return sendMail("127.0.0.1:"+strconv.Itoa(smtpPort), "tester@example.net", "nobody@example.test",
			"From: tester@example.net\r\nTo: nobody@example.test\r\nSubject: to nobody\r\n\r\nHi.\r\n") == nil
	})
	eventually(t, "catch-all delivers", func() bool {
		_, body := jmap("alice@example.test", "new mail pass 22")
		return strings.Contains(body, "to nobody")
	})
	p.call(http.StatusNoContent, "PUT", dom1, map[string]any{"catch_all": ""})
	eventually(t, "catch-all off refuses", func() bool {
		return sendMail("127.0.0.1:"+strconv.Itoa(smtpPort), "tester@example.net", "nobody2@example.test",
			"From: tester@example.net\r\nTo: nobody2@example.test\r\nSubject: x\r\n\r\nHi.\r\n") != nil
	})

	// The send limit (3 an hour, one used) refuses a mailbox's extra mail.
	var refused error
	for i := range 6 {
		if err := submit("someone@example.net", "burst "+strconv.Itoa(i)); err != nil {
			refused = err
			break
		}
	}
	if refused == nil || !strings.HasPrefix(refused.Error(), "4") {
		t.Fatalf("send limit not enforced: %v", refused)
	}
	t.Logf("send limit: %v", refused)
	if w := p.call(200, "PUT", settings, map[string]any{"send_limit": 0, "relay": nil})["warning"]; w != nil {
		t.Logf("settings: %v", w)
	}
	var lastErr error
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		if lastErr = submit("alice@example.test", "unlimited"); lastErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the limit stays: %v", lastErr)
		}
	}
	if view := p.call(200, "GET", "/api/v1/mail", nil)["settings"].(map[string]any); view["relay"] != nil || view["send_limit"].(float64) != 0 {
		t.Fatalf("settings = %v", view)
	}
	p.call(http.StatusConflict, "DELETE", "/api/v1/mail/domains/"+did, nil)
	p.call(http.StatusNoContent, "DELETE", fw+"/"+fid, nil)

	// A redeployment keeps the mail.
	dep := p.call(http.StatusAccepted, "POST", base+"/deploy", map[string]any{})["deployment"].(map[string]any)
	if d, log := followDeployment(t, p, base, jsonID(dep["id"])); d["status"] != "succeeded" {
		t.Fatalf("redeploy: %v\n%s", d, log)
	}
	eventually(t, "mail kept", func() bool {
		code, body := jmap("alice@example.test", "new mail pass 22")
		return code == 200 && strings.Contains(body, "hello from basalt")
	})

	// A second domain; the first (the server's default) can then go, and
	// the last one too (it is only turned off in the mail server).
	p.call(http.StatusCreated, "POST", "/api/v1/dns/zones", map[string]string{"name": "example.org"})
	other := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/mail/domains", map[string]string{"name": "example.org"})["domain"].(map[string]any)["id"])
	p.call(http.StatusConflict, "POST", "/api/v1/mail/domains", map[string]string{"name": "example.org"})
	p.call(http.StatusNoContent, "DELETE", mb+"/"+box, nil)
	p.call(http.StatusNoContent, "DELETE", mb+"/"+boxes[1].(map[string]any)["id"].(string), nil)
	p.call(http.StatusNoContent, "DELETE", "/api/v1/mail/domains/"+did, nil)
	eventually(t, "mail records gone", func() bool { return len(ask("example.test.", mdns.TypeMX)) == 0 })
	if mx := ask("example.org.", mdns.TypeMX); len(mx) != 1 {
		t.Fatalf("example.org MX = %v", mx)
	}
	p.call(http.StatusNoContent, "DELETE", "/api/v1/mail/domains/"+other, nil)
	// Added again, it works again.
	again := jsonID(p.call(http.StatusCreated, "POST", "/api/v1/mail/domains", map[string]string{"name": "example.org"})["domain"].(map[string]any)["id"])
	p.call(http.StatusCreated, "POST", "/api/v1/mail/domains/"+again+"/mailboxes", map[string]any{"local": "bob", "password": "bob mail pass 1"})
	eventually(t, "re-added domain delivers", func() bool {
		return sendMail("127.0.0.1:"+strconv.Itoa(smtpPort), "tester@example.net", "bob@example.org",
			"From: tester@example.net\r\nTo: bob@example.org\r\nSubject: again\r\n\r\nHi.\r\n") == nil
	})
	// Viewers see the domains and mailboxes but change nothing.
	p.call(http.StatusCreated, "POST", "/api/v1/users", map[string]string{
		"email": "v@example.com", "name": "V", "password": "viewer password 1", "role": "viewer",
	})
	viewer := *p
	viewer.hc = &http.Client{Jar: newJar()}
	viewer.call(200, "POST", "/api/v1/auth/login", map[string]string{"email": "v@example.com", "password": "viewer password 1"})
	viewer.call(200, "GET", "/api/v1/mail", nil)
	viewer.call(200, "GET", "/api/v1/mail/domains/"+again+"/mailboxes", nil)
	viewer.call(http.StatusForbidden, "POST", "/api/v1/mail/domains/"+again+"/mailboxes", map[string]any{"local": "eve", "password": "eve mail pass 1"})
	viewer.call(http.StatusForbidden, "POST", "/api/v1/mail/domains", map[string]string{"name": "example.net"})
	viewer.call(http.StatusForbidden, "POST", "/api/v1/mail/checks", nil)
	viewer.call(200, "GET", "/api/v1/mail/domains/"+again+"/forwarders", nil)
	viewer.call(http.StatusForbidden, "POST", "/api/v1/mail/domains/"+again+"/forwarders", map[string]any{"local": "x", "recipients": []string{"a@example.net"}})
	viewer.call(http.StatusForbidden, "PUT", "/api/v1/mail/settings", map[string]any{"send_limit": 1})
	viewer.call(http.StatusForbidden, "PUT", "/api/v1/mail/domains/"+again, map[string]any{"catch_all": ""})
	// No password reaches the audit log.
	var audit string
	p.db.R.QueryRow(`SELECT group_concat(details) FROM audit_log WHERE action LIKE 'mail.%'`).Scan(&audit)
	if strings.Contains(audit, "mail pass") || strings.Contains(audit, "relay secret") || !strings.Contains(audit, "alice@example.test") {
		t.Fatalf("audit: %s", audit)
	}
}

// sendMail delivers a message over plain SMTP, greeting with a real name
// (Stalwart refuses "localhost").
func sendMail(addr, from, to, msg string) error {
	c, err := smtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Hello("client.example.net"); err != nil {
		return err
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
