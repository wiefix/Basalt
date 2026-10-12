package server

import (
	"strings"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/store"
)

func TestEvaluateHealth(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	st := &healthState{check: store.HealthCheck{ID: 1, Interval: 30, Status: store.HealthUnknown}, votes: map[int64]healthVote{}}
	online := map[int64]bool{1: true, 2: true, 3: true}
	connected := func(id int64) bool { return online[id] }
	vote := func(id int64, ok bool, err string) { st.votes[id] = healthVote{OK: ok, Error: err, At: now} }
	eval := func() (string, string, bool) { return evaluateHealth(st, now, connected) }

	if _, _, ok := eval(); ok {
		t.Fatal("no votes decided")
	}
	// From unknown, one evaluation is enough.
	vote(1, true, "")
	vote(2, true, "")
	vote(3, false, "HTTP 502")
	if s, d, ok := eval(); !ok || s != store.HealthUp || d != "2 of 3 servers reach it" {
		t.Fatalf("%s %s %v", s, d, ok)
	}
	st.check.Status = store.HealthUp
	// A minority failing changes nothing.
	if _, _, ok := eval(); ok {
		t.Fatal("minority decided")
	}
	// A majority failing must be seen twice.
	vote(2, false, "no answer in time")
	if _, _, ok := eval(); ok {
		t.Fatal("down on the first evaluation")
	}
	s, d, ok := eval()
	if !ok || s != store.HealthDown || !strings.Contains(d, "2 of 3 servers fail") || !strings.Contains(d, "HTTP 502; no answer in time") {
		t.Fatalf("%s %q %v", s, d, ok)
	}
	st.check.Status = store.HealthDown
	// A flapping majority restarts the count.
	vote(2, true, "")
	vote(3, true, "")
	eval()
	vote(3, false, "x")
	vote(1, false, "x")
	if _, _, ok := eval(); ok {
		t.Fatal("flap decided")
	}
	// Results of disconnected servers and stale ones do not count: a tie
	// of the rest keeps the status.
	vote(1, true, "")
	vote(2, true, "")
	vote(3, false, "x")
	online[1] = false
	if _, _, ok := eval(); ok {
		t.Fatal("tie decided")
	}
	st.votes[3] = healthVote{OK: false, At: now.Add(-3 * time.Minute)}
	eval()
	if s, _, ok := eval(); !ok || s != store.HealthUp {
		t.Fatalf("one fresh vote: %s %v", s, ok)
	}
}
