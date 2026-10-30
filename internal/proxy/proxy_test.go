package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeDomain(t *testing.T) {
	for in, want := range map[string]string{
		"Example.COM.": "example.com", "a-b.example.com": "a-b.example.com",
		"xn--80ak6aa92e.com": "xn--80ak6aa92e.com", "localhost": "localhost", "10.0.0.1": "10.0.0.1",
	} {
		if got, err := NormalizeDomain(in); err != nil || got != want {
			t.Errorf("NormalizeDomain(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "*.example.com", "-a.com", "a-.com", "a..com", "exa mple.com",
		"ex\"ample.com", "пример.рф", "com", strings.Repeat("a", 64) + ".com", "a.com/evil"} {
		if _, err := NormalizeDomain(bad); err == nil {
			t.Errorf("NormalizeDomain(%q) accepted", bad)
		}
	}
}

func TestRouteValidation(t *testing.T) {
	ok := Desired{Routes: []Route{
		{ID: 1, Kind: KindStatic, Domains: []string{"A.example.com"}, Root: "/srv/basalt/sites/1/public"},
		{ID: 2, Kind: KindReverseProxy, Domains: []string{"b.example.com"}, Upstream: "127.0.0.1:3000"},
	}}
	if err := ok.Validate("/srv/basalt"); err != nil {
		t.Fatal(err)
	}
	if ok.Routes[0].Domains[0] != "a.example.com" || ok.Routes[0].TLS != TLSAuto {
		t.Fatal("route not normalized")
	}

	bad := map[string]Desired{
		"root outside sites": {Routes: []Route{{ID: 1, Kind: KindStatic, Domains: []string{"a.com"}, Root: "/etc"}}},
		"root traversal":     {Routes: []Route{{ID: 1, Kind: KindStatic, Domains: []string{"a.com"}, Root: "/srv/basalt/../etc"}}},
		"relative root":      {Routes: []Route{{ID: 1, Kind: KindStatic, Domains: []string{"a.com"}, Root: "srv/basalt/x"}}},
		"sites root itself":  {Routes: []Route{{ID: 1, Kind: KindStatic, Domains: []string{"a.com"}, Root: "/srv/basalt"}}},
		"prefix trick":       {Routes: []Route{{ID: 1, Kind: KindStatic, Domains: []string{"a.com"}, Root: "/srv/basalt-evil/x"}}},
		"bad upstream":       {Routes: []Route{{ID: 1, Kind: KindReverseProxy, Domains: []string{"a.com"}, Upstream: "a.com"}}},
		"no domains":         {Routes: []Route{{ID: 1, Kind: KindReverseProxy, Upstream: "a.com:80"}}},
		"bad kind":           {Routes: []Route{{ID: 1, Kind: "php", Domains: []string{"a.com"}}}},
		"bad tls":            {Routes: []Route{{ID: 1, Kind: KindReverseProxy, Domains: []string{"a.com"}, Upstream: "a.com:80", TLS: "off"}}},
		"duplicate across routes": {Routes: []Route{
			{ID: 1, Kind: KindReverseProxy, Domains: []string{"a.com"}, Upstream: "x.com:80"},
			{ID: 2, Kind: KindReverseProxy, Domains: []string{"A.com"}, Upstream: "y.com:80"},
		}},
	}
	for name, d := range bad {
		if err := d.Validate("/srv/basalt"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBuildTLSPolicies(t *testing.T) {
	cfg, id, err := Build(Desired{ACMEEmail: "ops@example.com", Routes: []Route{
		{ID: 1, Kind: KindReverseProxy, Domains: []string{"public.example.com", "dev.localhost"}, Upstream: "127.0.0.1:1", TLS: TLSAuto},
		{ID: 2, Kind: KindReverseProxy, Domains: []string{"private.example.com"}, Upstream: "127.0.0.1:1", TLS: TLSInternal},
	}}, BuildOptions{Dir: "/x", HTTPPort: 80, HTTPSPort: 443})
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Apps struct {
			TLS struct {
				Automation struct {
					Policies []struct {
						Subjects []string         `json:"subjects"`
						Issuers  []map[string]any `json:"issuers"`
					} `json:"policies"`
				} `json:"automation"`
			} `json:"tls"`
		} `json:"apps"`
	}
	json.Unmarshal(cfg, &c)
	p := c.Apps.TLS.Automation.Policies
	if len(p) != 2 || strings.Join(p[0].Subjects, ",") != "dev.localhost,private.example.com" || p[0].Issuers[0]["module"] != "internal" {
		t.Fatalf("internal policy = %+v", p)
	}
	if len(p[1].Issuers) != 2 || p[1].Issuers[0]["email"] != "ops@example.com" || p[1].Issuers[1]["ca"] != zeroSSLDirectory {
		t.Fatalf("public policy = %+v", p[1])
	}

	// Same input, same id; different input, different id.
	cfg2, id2, _ := Build(Desired{ACMEEmail: "ops@example.com", Routes: []Route{
		{ID: 1, Kind: KindReverseProxy, Domains: []string{"public.example.com", "dev.localhost"}, Upstream: "127.0.0.1:1", TLS: TLSAuto},
		{ID: 2, Kind: KindReverseProxy, Domains: []string{"private.example.com"}, Upstream: "127.0.0.1:1", TLS: TLSInternal},
	}}, BuildOptions{Dir: "/x", HTTPPort: 80, HTTPSPort: 443})
	if id2 != id || string(cfg2) != string(cfg) {
		t.Fatal("build is not deterministic")
	}
	if _, id3, _ := Build(Desired{}, BuildOptions{Dir: "/x", HTTPPort: 80, HTTPSPort: 443}); id3 == id {
		t.Fatal("different config, same id")
	}
}

func TestAppRoute(t *testing.T) {
	bad := Desired{Routes: []Route{{ID: 1, Kind: KindReverseProxy, AppID: 3, Domains: []string{"a.com"}, Upstream: "10.0.0.1:80"}}}
	if err := bad.Validate(""); err == nil {
		t.Fatal("app route with a fixed upstream accepted")
	}
	if err := (&Route{Kind: KindStatic, AppID: 3, Domains: []string{"a.com"}, Root: "/srv/x"}).Validate(""); err == nil {
		t.Fatal("static app route accepted")
	}
	d := Desired{Routes: []Route{{ID: 1, Kind: KindReverseProxy, AppID: 3, Domains: []string{"a.com"}}}}
	if err := d.Validate(""); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Build(d, BuildOptions{Dir: "/x", HTTPPort: 80, HTTPSPort: 443})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), `"dynamic_upstreams":{"app_id":3,"source":"basalt_app"}`) || strings.Contains(string(cfg), `"upstreams":`) {
		t.Fatalf("app route config: %s", cfg)
	}
}

func TestServiceRoutes(t *testing.T) {
	r := Route{ID: 1, Kind: KindReverseProxy, AppID: 3, Services: []ServiceRoute{{Service: "api", Domains: []string{"API.a.com"}}}}
	if err := r.Validate(""); err != nil {
		t.Fatalf("services without published domains: %v", err)
	}
	if r.Services[0].Domains[0] != "api.a.com" {
		t.Fatalf("service domain not normalized: %v", r.Services)
	}
	for name, bad := range map[string]Route{
		"site route":       {Kind: KindStatic, SiteID: 1, Domains: []string{"a.com"}, Services: []ServiceRoute{{Service: "api", Domains: []string{"b.com"}}}},
		"duplicate domain": {Kind: KindReverseProxy, AppID: 3, Domains: []string{"a.com"}, Services: []ServiceRoute{{Service: "api", Domains: []string{"a.com"}}}},
		"repeated service": {Kind: KindReverseProxy, AppID: 3, Services: []ServiceRoute{{Service: "api", Domains: []string{"a.com"}}, {Service: "api", Domains: []string{"b.com"}}}},
		"bad service":      {Kind: KindReverseProxy, AppID: 3, Services: []ServiceRoute{{Service: "../x", Domains: []string{"a.com"}}}},
		"no domains":       {Kind: KindReverseProxy, AppID: 3, Services: []ServiceRoute{{Service: "api"}}},
	} {
		if err := bad.Validate(""); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	other := Desired{Routes: []Route{r, {ID: 2, Kind: KindReverseProxy, AppID: 4, Domains: []string{"api.a.com"}}}}
	if err := other.Validate(""); err == nil {
		t.Fatal("a service domain served by another route accepted")
	}

	d := Desired{Routes: []Route{{ID: 1, Kind: KindReverseProxy, AppID: 3, Domains: []string{"a.com"}, DNSChallenge: true,
		Services: []ServiceRoute{{Service: "api", Domains: []string{"api.a.com", "*.api.a.com"}}}}}}
	if err := d.Validate(""); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Build(d, BuildOptions{Dir: "/x", HTTPPort: 80, HTTPSPort: 443})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"match":[{"host":["a.com"]}]`, `"dynamic_upstreams":{"app_id":3,"source":"basalt_app"}`,
		`"match":[{"host":["api.a.com"]}]`, `"match":[{"host":["*.api.a.com"]}]`,
		`"dynamic_upstreams":{"app_id":3,"service":"api","source":"basalt_app"}`,
		`"subjects":["a.com","*.api.a.com","api.a.com"]`, `"api.a.com":"app-3"`,
	} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("config lacks %s:\n%s", want, cfg)
		}
	}

	for k, want := range map[string]struct {
		id  int64
		svc string
	}{"7": {7, ""}, "7/api": {7, "api"}} {
		id, svc, err := ParseAppUpstreamKey(k)
		if err != nil || id != want.id || svc != want.svc || AppUpstreamKey(id, svc) != k {
			t.Errorf("key %q: %d %q %v", k, id, svc, err)
		}
	}
	for _, k := range []string{"", "0", "-1", "07", "7/", "7/../x", "x/api"} {
		if _, _, err := ParseAppUpstreamKey(k); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
}

func TestWildcardDomains(t *testing.T) {
	for in, want := range map[string]string{"*.Example.com": "*.example.com", "*.a.b.example.com.": "*.a.b.example.com", "www.example.com": "www.example.com"} {
		if got, err := NormalizeRouteDomain(in); err != nil || got != want {
			t.Errorf("NormalizeRouteDomain(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"*.com", "*", "*.*.example.com", "a.*.example.com", "*example.com", "*.10.0.0.1", "**.example.com"} {
		if _, err := NormalizeRouteDomain(bad); err == nil {
			t.Errorf("NormalizeRouteDomain(%q) accepted", bad)
		}
	}
	// Upstreams never take a wildcard.
	if err := ValidateUpstream("*.example.com:80"); err == nil {
		t.Error("wildcard upstream accepted")
	}

	d := Desired{ACMEEmail: "ops@example.com", Routes: []Route{
		{ID: 1, Kind: KindReverseProxy, Domains: []string{"*.example.com", "example.org"}, Upstream: "127.0.0.1:1"},
		{ID: 2, Kind: KindReverseProxy, Domains: []string{"www.example.com"}, Upstream: "127.0.0.1:2"},
		{ID: 3, Kind: KindReverseProxy, Domains: []string{"*.dev.localhost"}, Upstream: "127.0.0.1:3"},
	}}
	if err := d.Validate(""); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Build(d, BuildOptions{Dir: "/p", HTTPPort: 80, HTTPSPort: 443,
		ACMECARoots: []string{"/ca.pem"}, ACMEResolvers: []string{"127.0.0.1:53"}})
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Routes []struct {
						Match []struct {
							Host []string `json:"host"`
						} `json:"match"`
					} `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
			TLS struct {
				Automation struct {
					Policies []struct {
						Subjects []string         `json:"subjects"`
						Issuers  []map[string]any `json:"issuers"`
					} `json:"policies"`
				} `json:"automation"`
			} `json:"tls"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	// Exact names come before wildcards, whatever the route order.
	var hosts []string
	for _, r := range c.Apps.HTTP.Servers["basalt"].Routes {
		if len(r.Match) > 0 {
			hosts = append(hosts, strings.Join(r.Match[0].Host, " "))
		}
	}
	if strings.Join(hosts, " | ") != "example.org | www.example.com | *.example.com | *.dev.localhost" {
		t.Fatalf("route order: %v", hosts)
	}
	p := c.Apps.TLS.Automation.Policies
	if len(p) != 3 || strings.Join(p[0].Subjects, ",") != "*.dev.localhost" || strings.Join(p[1].Subjects, ",") != "*.example.com" {
		t.Fatalf("policies: %+v", p)
	}
	// Wildcards: DNS-01 through the agent, for Let's Encrypt and ZeroSSL.
	for _, iss := range p[1].Issuers {
		b, _ := json.Marshal(iss)
		if !strings.Contains(string(b), `"provider":{"name":"basalt","socket":"/p/acme.sock"}`) ||
			!strings.Contains(string(b), `"resolvers":["127.0.0.1:53"]`) || !strings.Contains(string(b), `"trusted_roots_pem_files":["/ca.pem"]`) {
			t.Fatalf("wildcard issuer: %s", b)
		}
	}
	if len(p[1].Issuers) != 2 {
		t.Fatal("no ZeroSSL fallback for wildcards")
	}
	// Other names keep the HTTP and TLS-ALPN challenges.
	if b, _ := json.Marshal(p[2]); strings.Contains(string(b), "challenges") {
		t.Fatalf("default policy: %s", b)
	}
}

// A route served by several servers takes its certificates through DNS-01:
// an HTTP challenge could reach another server.
func TestDNSChallengeRoute(t *testing.T) {
	d := Desired{Routes: []Route{
		{ID: 1, Kind: KindReverseProxy, AppID: 1, Domains: []string{"app.example.com"}, DNSChallenge: true},
		{ID: 2, Kind: KindReverseProxy, AppID: 2, Domains: []string{"one.example.com"}},
		{ID: 3, Kind: KindReverseProxy, AppID: 3, Domains: []string{"app.localhost"}, DNSChallenge: true},
	}}
	if err := d.Validate(""); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Build(d, BuildOptions{Dir: "/p", HTTPPort: 80, HTTPSPort: 443})
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Apps struct {
			TLS struct {
				Automation struct {
					Policies []struct {
						Subjects []string         `json:"subjects"`
						Issuers  []map[string]any `json:"issuers"`
					} `json:"policies"`
				} `json:"automation"`
			} `json:"tls"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatal(err)
	}
	p := c.Apps.TLS.Automation.Policies
	// Local names keep the internal CA; the replicated public name gets
	// DNS-01; everything else the default policy.
	if len(p) != 3 || strings.Join(p[0].Subjects, ",") != "app.localhost" || strings.Join(p[1].Subjects, ",") != "app.example.com" ||
		len(p[2].Subjects) != 0 {
		t.Fatalf("policies: %+v", p)
	}
	b, _ := json.Marshal(p[1].Issuers)
	if !strings.Contains(string(b), `"provider":{"name":"basalt","socket":"/p/acme.sock"}`) {
		t.Fatalf("issuers: %s", b)
	}
	b, _ = json.Marshal(p[2].Issuers)
	if strings.Contains(string(b), "acme.sock") {
		t.Fatalf("default policy uses DNS-01: %s", b)
	}
}
