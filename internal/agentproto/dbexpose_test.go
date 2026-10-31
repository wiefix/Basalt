package agentproto

import (
	"fmt"
	"testing"
)

func TestParseAllow(t *testing.T) {
	got, err := ParseAllow([]string{"192.0.2.7", "198.51.100.9/24", "198.51.100.0/24", "::ffff:203.0.113.5", "::ffff:203.0.113.0/120", "2001:db8::1/32", "0.0.0.0/0", "::/0"})
	if err != nil {
		t.Fatal(err)
	}
	want := "[192.0.2.7/32 198.51.100.0/24 203.0.113.5/32 203.0.113.0/24 2001:db8::/32 0.0.0.0/0 ::/0]"
	if fmt.Sprint(got) != want {
		t.Fatalf("got %v, want %s", got, want)
	}
	for _, bad := range [][]string{nil, {}, {"example.com"}, {"192.0.2.1/33"}, {"fe80::1%eth0"}, {"::ffff:1.2.3.4/64"}, {""}} {
		if _, err := ParseAllow(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	many := make([]string, MaxDBExposeAllow+1)
	for i := range many {
		many[i] = fmt.Sprintf("10.0.0.%d", i)
	}
	if _, err := ParseAllow(many); err == nil {
		t.Error("too many addresses accepted")
	}
}

func TestDBExposeValidate(t *testing.T) {
	ok := DBExpose{AppID: 1, Service: "pg", TargetPort: 5432, Port: 15432, Allow: []string{"192.0.2.1"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*DBExpose){
		"app":         func(e *DBExpose) { e.AppID = 0 },
		"service":     func(e *DBExpose) { e.Service = "../x" },
		"target":      func(e *DBExpose) { e.TargetPort = 0 },
		"privileged":  func(e *DBExpose) { e.Port = 443 },
		"port":        func(e *DBExpose) { e.Port = 70000 },
		"empty allow": func(e *DBExpose) { e.Allow = nil },
	} {
		e := ok
		mod(&e)
		if err := e.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	other := ok
	other.AppID = 2
	if err := (&DBExposeApply{Entries: []DBExpose{ok, other}}).Validate(); err == nil {
		t.Error("two databases on one port accepted")
	}
	other.Port = 15433
	if err := (&DBExposeApply{Entries: []DBExpose{ok, other}}).Validate(); err != nil {
		t.Error(err)
	}
	if err := (&DBExposeApply{Entries: []DBExpose{ok, ok}}).Validate(); err == nil {
		t.Error("a database listed twice accepted")
	}
}
