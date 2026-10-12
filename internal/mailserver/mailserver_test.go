package mailserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/dns"
)

// zoneFile is what Stalwart v0.16.25 returned for a new domain.
const zoneFile = `v1-ed25519-20261011._domainkey.example.test. IN TXT "v=DKIM1; k=ed25519; h=sha256; p=VCncmVsA++DEozUscdjeXqTEAS41xx2jR/B9eSgpwYY="
v1-rsa-20261011._domainkey.example.test. IN TXT (
    "v=DKIM1; k=rsa; h=sha256; p=MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA1n6KfOWzK894EFAtS+LgEcRo71JSBTZQgWy4hlb8JDYwpzUmVjtxL3GGj0NBFKHTeJ3i/TimPha9TbT/44Ug7XlXskEksfYNPAAB1tuUhPEQf+FsSWf+CMptFhH1U/7fhR2YCzqyx9lwks+UQZNn/oHGZWdOJDIC8fQ8gw9Gj2X/JDNbQXVL3yu"
    "5mgbuky9VTwOULYCk/MCNJE+NbgZ1FIn/uK5y16Lxp2LrZ9lKmeyfDA7mfspusUmmI+FBGp48lZiccbjWARE6fIsLteMTM1qzcL4q6E2GXxYYfxSlGKi7hpTGlj+o5LIwBWE/bpTn+FnjLj1vlokumAftps6zeQIDAQAB"
)
mail.example.test. IN TXT "v=spf1 a -all"
example.test. IN TXT "v=spf1 mx -all"
example.test. IN MX 10 mail.example.test.
_dmarc.example.test. IN TXT "v=DMARC1; p=reject; rua=mailto:postmaster@example.test"
_caldavs._tcp.example.test. IN SRV 0 1 443 mail.example.test.
_imaps._tcp.example.test. IN SRV 0 1 993 mail.example.test.
_submissions._tcp.example.test. IN SRV 0 1 465 mail.example.test.
mta-sts.example.test. IN CNAME mail.example.test.
_mta-sts.example.test. IN TXT "v=STSv1; id=8430040749345519133"
autoconfig.example.test. IN CNAME mail.example.test.
weird.example.test. IN A 192.0.2.1
`

func TestParseZone(t *testing.T) {
	recs := ParseZone(zoneFile)
	got := map[string]dns.Record{}
	for _, r := range recs {
		got[r.Name+" "+r.Type] = r
	}
	if len(recs) != 12 {
		t.Fatalf("%d records: %+v", len(recs), recs)
	}
	if r := got["example.test MX"]; r.Priority != 10 || r.Value != "mail.example.test" {
		t.Fatalf("MX %+v", r)
	}
	rsa := got["v1-rsa-20261011._domainkey.example.test TXT"].Value
	if !strings.HasPrefix(rsa, "v=DKIM1; k=rsa; h=sha256; p=MIIB") || !strings.HasSuffix(rsa, "IDAQAB") || strings.ContainsAny(rsa, "\" \n\t") && strings.Contains(rsa, "\"") {
		t.Fatalf("DKIM joined wrongly: %q", rsa)
	}
	if r := got["_imaps._tcp.example.test SRV"]; r.Port != 993 || r.Weight != 1 || r.Value != "mail.example.test" {
		t.Fatalf("SRV %+v", r)
	}
	if _, ok := got["weird.example.test A"]; ok {
		t.Fatal("A record taken from the mail server")
	}
	// They fit a zone.
	z := dns.Zone{Name: "example.test"}
	auto := dns.ServiceRecords(&z, []string{"example.test"}, recs, "mail", nil)
	for _, r := range auto {
		z.Records = append(z.Records, r.Record)
	}
	if len(auto) != 12 || z.Validate() != nil {
		t.Fatalf("%d records, %v", len(auto), z.Validate())
	}
}

func TestServiceSpec(t *testing.T) {
	env := NewEnv()
	user, pass, err := Credentials(env)
	if err != nil || user != AdminUser || len(pass) < 20 {
		t.Fatal(user, pass, err)
	}
	svc, err := ServiceSpec(env, "mail.example.com", map[int]int{25: 2525}, Limits{MemoryMB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	spec := agentproto.AppSpec{AppID: 1, DeploymentID: 1, Services: []agentproto.ServiceSpec{svc},
		ComposeVolumes: Volumes, Mail: true}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	if svc.HostPorts[0] != (agentproto.HostPort{Container: 25, Host: 2525}) || svc.HostPorts[1].Host != 465 {
		t.Fatalf("ports %+v", svc.HostPorts)
	}
	// Only the mail server may publish ports, and only mail ports.
	spec.Mail = false
	if spec.Validate() == nil {
		t.Fatal("host ports accepted for an ordinary app")
	}
	spec.Mail = true
	spec.Services[0].HostPorts = append(spec.Services[0].HostPorts, agentproto.HostPort{Container: 22, Host: 22})
	if spec.Validate() == nil {
		t.Fatal("port 22 accepted")
	}
	for _, bad := range []string{"localhost", "mail", "Mail.example.com", "-x.example.com", "mail.example.com."} {
		if _, err := ServiceSpec(env, bad, nil, Limits{}); err == nil {
			t.Errorf("hostname %q accepted", bad)
		}
	}
	if _, err := ServiceSpec(map[string]string{}, "mail.example.com", nil, Limits{}); err == nil {
		t.Fatal("no credentials accepted")
	}
}

// fakeServer answers JMAP calls from a table of method handlers.
type fakeServer map[string]func(args map[string]any) any

func (f fakeServer) transport(ctx context.Context, body []byte) (int, []byte, error) {
	var req struct {
		Using       []string            `json:"using"`
		MethodCalls [][]json.RawMessage `json:"methodCalls"`
	}
	json.Unmarshal(body, &req)
	var name string
	var args map[string]any
	json.Unmarshal(req.MethodCalls[0][0], &name)
	json.Unmarshal(req.MethodCalls[0][1], &args)
	h := f[name]
	var res any = map[string]any{"type": "unknownMethod"}
	resName := name
	if h == nil {
		resName = "error"
	} else {
		res = h(args)
	}
	b, _ := json.Marshal(map[string]any{"methodResponses": []any{[]any{resName, res, "c"}}})
	return 200, b, nil
}

func TestClient(t *testing.T) {
	var created map[string]any
	f := fakeServer{
		"x:Account/set": func(a map[string]any) any {
			if c, ok := a["create"].(map[string]any); ok {
				created = c["n"].(map[string]any)
				if created["name"] == "taken" {
					return map[string]any{"notCreated": map[string]any{"n": map[string]any{"type": "alreadyExists", "description": "Account already exists"}}}
				}
				return map[string]any{"created": map[string]any{"n": map[string]any{"id": "acc1"}}}
			}
			return map[string]any{}
		},
		"x:Account/query": func(map[string]any) any { return map[string]any{"ids": []string{"a", "b", "g"}} },
		"x:Account/get": func(map[string]any) any {
			return map[string]any{"list": []any{
				map[string]any{"id": "a", "@type": "User", "name": "alice", "domainId": "d1", "emailAddress": "alice@example.com",
					"description": "Alice", "aliases": map[string]any{"0": map[string]any{"name": "info"}},
					"quotas": map[string]any{"maxDiskQuota": 1 << 30}, "usedDiskQuota": 1 << 20},
				map[string]any{"id": "b", "@type": "User", "name": "bob", "domainId": "d2", "emailAddress": "bob@example.org"},
				map[string]any{"id": "g", "@type": "Group", "name": "team", "domainId": "d1"},
			}}
		},
	}
	c := NewClient(f.transport)
	ctx := context.Background()
	pw, q := "secret pass", int64(512)
	id, err := c.CreateMailbox(ctx, "d1", "carol", MailboxChange{Password: &pw, QuotaMB: &q, Aliases: []string{"sales"}})
	if err != nil || id != "acc1" {
		t.Fatal(id, err)
	}
	b, _ := json.Marshal(created)
	if !strings.Contains(string(b), `"credentials":{"0":{"@type":"Password","secret":"secret pass"}}`) ||
		!strings.Contains(string(b), `"quotas":{"maxDiskQuota":536870912}`) || !strings.Contains(string(b), `"aliases":{"0":{"domainId":"d1","name":"sales"}}`) {
		t.Fatalf("create %s", b)
	}
	if _, err := c.CreateMailbox(ctx, "d1", "taken", MailboxChange{}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("refusal: %v", err)
	}
	list, err := c.Mailboxes(ctx, "d1")
	if err != nil || len(list) != 1 || list[0].Address != "alice@example.com" || list[0].QuotaMB != 1024 ||
		list[0].Name != "Alice" || list[0].Aliases[0] != "info" || list[0].UsedMB != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	if err := c.Configure(ctx, "mail.example.com", "d1"); err == nil {
		t.Fatal("unknown method not reported")
	}
}

func TestSettings(t *testing.T) {
	var calls []string
	var bodies []map[string]any
	record := func(name string, res any) func(map[string]any) any {
		return func(a map[string]any) any {
			calls = append(calls, name)
			bodies = append(bodies, a)
			return res
		}
	}
	created := map[string]any{"created": map[string]any{"n": map[string]any{"id": "new"}}}
	f := fakeServer{
		"x:MtaRoute/query":           record("route?", map[string]any{"ids": []string{"r1"}}),
		"x:MtaRoute/get":             record("route get", map[string]any{"list": []any{map[string]any{"id": "r1", "name": "mx"}}}),
		"x:MtaRoute/set":             record("route set", created),
		"x:MtaTlsStrategy/query":     record("tls?", map[string]any{"ids": []string{"t1"}}),
		"x:MtaTlsStrategy/get":       record("tls get", map[string]any{"list": []any{map[string]any{"id": "t1", "name": "basalt-relay"}}}),
		"x:MtaTlsStrategy/set":       record("tls set", map[string]any{"updated": map[string]any{"t1": nil}}),
		"x:MtaOutboundStrategy/set":  record("strategy", map[string]any{"updated": map[string]any{"singleton": nil}}),
		"x:MtaInboundThrottle/query": record("throttle?", map[string]any{"ids": []string{}}),
		"x:MtaInboundThrottle/set":   record("throttle set", created),
		"x:Action/set":               record("action", created),
	}
	c := NewClient(f.transport)
	ctx := context.Background()
	if err := c.SetRelay(ctx, &Relay{Host: "smtp.example.net", Port: 587, Username: "u", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, ","); got != "route?,route get,route set,tls?,tls get,tls set,strategy" {
		t.Fatalf("calls %s", got)
	}
	b, _ := json.Marshal(bodies)
	for _, want := range []string{`"@type":"Relay"`, `"name":"basalt-relay"`, `"authSecret":{"@type":"Value","secret":"p"}`,
		`"implicitTls":false`, `"startTls":"require"`, `"else":"'basalt-relay'"`, `"if":"!is_local_domain(rcpt_domain)"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
	// The TLS strategy existed: it is updated, not created again.
	if strings.Contains(jsonOf(bodies[5]), "create") {
		t.Errorf("tls strategy recreated: %v", bodies[5])
	}
	calls, bodies = nil, nil
	if err := c.SetSendLimit(ctx, 50); err != nil {
		t.Fatal(err)
	}
	if b := jsonOf(bodies[len(bodies)-1]); !strings.Contains(b, `"rate":{"count":50,"period":3600000}`) ||
		!strings.Contains(b, `"description":"basalt-send-limit"`) || !strings.Contains(b, `"authenticatedAs":true`) {
		t.Fatalf("throttle %s", b)
	}
	if err := c.Reload(ctx); err != nil || calls[len(calls)-1] != "action" {
		t.Fatal(calls, err)
	}
	// Without a relay: direct delivery, Basalt's objects removed.
	calls, bodies = nil, nil
	if err := c.SetRelay(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if b := jsonOf(bodies[0]); !strings.Contains(b, `"else":"'mx'"`) || !strings.Contains(b, `'invalid-tls'`) {
		t.Fatalf("direct %s", b)
	}
	if b := jsonOf(bodies[len(bodies)-1]); !strings.Contains(b, `"destroy":["t1"]`) {
		t.Fatalf("tls strategy kept: %s", b)
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestReloadRetries(t *testing.T) {
	reloadPause = time.Millisecond
	calls := 0
	refuse := 2
	f := fakeServer{"x:Action/set": func(map[string]any) any {
		calls++
		if calls <= refuse {
			return map[string]any{"notCreated": map[string]any{"n": map[string]any{"type": "validationFailed",
				"description": "Invalid address: failed to lookup address information"}}}
		}
		return map[string]any{"created": map[string]any{"n": map[string]any{"id": "a"}}}
	}}
	c := NewClient(f.transport)
	if err := c.Reload(context.Background()); err != nil || calls != 3 {
		t.Fatalf("%v after %d calls", err, calls)
	}
	calls, refuse = 0, 100
	err := c.Reload(context.Background())
	if err == nil || calls != reloadTries || !strings.Contains(err.Error(), "lookup address") {
		t.Fatalf("%v after %d calls", err, calls)
	}
	// Transport errors are not retried.
	broken := NewClient(func(context.Context, []byte) (int, []byte, error) { calls++; return 0, nil, context.Canceled })
	calls = 0
	if broken.Reload(context.Background()) == nil || calls != 1 {
		t.Fatalf("%d calls", calls)
	}
}
