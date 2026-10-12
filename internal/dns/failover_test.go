package dns

import (
	"strings"
	"testing"
)

func TestFailover(t *testing.T) {
	zone := func() Zone {
		return Zone{Name: "example.com", Serial: 1, Records: []Record{
			{Name: "www", Type: TypeA, TTL: 60, Value: "192.0.2.1", HealthCheck: 1},
			{Name: "www", Type: TypeA, TTL: 60, Value: "192.0.2.2", HealthCheck: 2},
			{Name: "www", Type: TypeAAAA, TTL: 60, Value: "2001:db8::1", HealthCheck: 1},
			{Name: "api", Type: TypeA, TTL: 60, Value: "192.0.2.3", HealthCheck: 1},
			{Name: "api", Type: TypeA, TTL: 60, Value: "192.0.2.4"},
			{Name: "@", Type: TypeMX, TTL: 3600, Value: "mail.example.com", Priority: 10},
		}}
	}
	values := func(z Zone) string {
		var out []string
		for _, r := range z.Records {
			out = append(out, r.Name+" "+r.Value)
		}
		return strings.Join(out, ", ")
	}
	for _, tc := range []struct {
		down []int64
		want string
	}{
		{nil, "www 192.0.2.1, www 192.0.2.2, www 2001:db8::1, api 192.0.2.3, api 192.0.2.4, @ mail.example.com"},
		// Check 1 down: its addresses go where another address stays; the
		// AAAA set has no other address, so it is kept whole.
		{[]int64{1}, "www 192.0.2.2, www 2001:db8::1, api 192.0.2.4, @ mail.example.com"},
		{[]int64{2}, "www 192.0.2.1, www 2001:db8::1, api 192.0.2.3, api 192.0.2.4, @ mail.example.com"},
		// Every checked address of www is down: all of them are answered.
		{[]int64{1, 2}, "www 192.0.2.1, www 192.0.2.2, www 2001:db8::1, api 192.0.2.4, @ mail.example.com"},
		{[]int64{3}, "www 192.0.2.1, www 192.0.2.2, www 2001:db8::1, api 192.0.2.3, api 192.0.2.4, @ mail.example.com"},
	} {
		z := zone()
		down := map[int64]bool{}
		for _, id := range tc.down {
			down[id] = true
		}
		Failover(&z, func(id int64) bool { return down[id] })
		if got := values(z); got != tc.want {
			t.Errorf("down %v:\n got %s\nwant %s", tc.down, got, tc.want)
		}
		if err := z.Validate(); err != nil {
			t.Errorf("down %v: %v", tc.down, err)
		}
	}
	// The input slice is not reused: the caller's records stay intact.
	z := zone()
	orig := z.Records
	Failover(&z, func(int64) bool { return true })
	if orig[1].Value != "192.0.2.2" || len(orig) != 6 {
		t.Fatal("records changed in place")
	}
}

func TestHealthCheckRecords(t *testing.T) {
	r := Record{Name: "www", Type: "a", Value: "192.0.2.1", HealthCheck: 3}
	r.Normalize()
	if r.TTL != FailoverTTL {
		t.Fatalf("default TTL of a followed record = %d", r.TTL)
	}
	if err := r.Validate("example.com"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Record{
		{Name: "www", Type: TypeA, TTL: 3600, Value: "192.0.2.1", HealthCheck: 3},
		{Name: "www", Type: TypeCNAME, TTL: 60, Value: "other.example.com", HealthCheck: 3},
		{Name: "@", Type: TypeALIAS, TTL: 60, Value: "other.example.net", HealthCheck: 3},
		{Name: "@", Type: TypeMX, TTL: 60, Value: "mail.example.com", HealthCheck: 3},
		{Name: "www", Type: TypeA, TTL: 60, Value: "192.0.2.1", HealthCheck: -1},
	} {
		if err := bad.Validate("example.com"); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
