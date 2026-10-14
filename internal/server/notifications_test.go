package server

import (
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/agentproto"
)

func TestNewMailProblems(t *testing.T) {
	r := func(domain, check, status string) agentproto.MailCheckResult {
		return agentproto.MailCheckResult{Domain: domain, Check: check, Status: status, Detail: check + " detail"}
	}
	prev := &mailChecks{Results: []agentproto.MailCheckResult{r("", "ptr", "fail"), r("a.com", "spf", "warn")}}
	cur := &mailChecks{Results: []agentproto.MailCheckResult{
		r("", "ptr", "fail"),      // known: not repeated
		r("a.com", "spf", "fail"), // was a warning
		r("a.com", "dkim", "ok"),  // fine
		r("", "outbound_smtp", "fail"),
		r("b.com", "mx", "unknown"), // not a failure
	}}
	got := strings.Join(newMailProblems(prev, cur), "; ")
	if got != "a.com spf: spf detail; server outbound_smtp: outbound_smtp detail" {
		t.Fatalf("got %q", got)
	}
	// The first check reports every failure; a check that failed to run, none.
	if n := len(newMailProblems(nil, cur)); n != 3 {
		t.Fatalf("first check: %d", n)
	}
	if newMailProblems(prev, &mailChecks{Error: "agent offline"}) != nil {
		t.Fatal("reported a check that did not run")
	}
}
