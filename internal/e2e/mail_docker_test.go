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
	// No password reaches the audit log.
	var audit string
	p.db.R.QueryRow(`SELECT group_concat(details) FROM audit_log WHERE action LIKE 'mail.%'`).Scan(&audit)
	if strings.Contains(audit, "mail pass") || !strings.Contains(audit, "alice@example.test") {
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
