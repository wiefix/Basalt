package agent

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// TestFloatingManager: the set is saved and put back when the agent
// starts, and its addresses are not published as the host's own.
func TestFloatingManager(t *testing.T) {
	dir := t.TempDir()
	got := make(chan []string, 4)
	cfg := Config{DataDir: dir, FloatingIPs: func(_ context.Context, f agentproto.FloatingIPs) error {
		got <- f.Addresses
		return nil
	}}
	log := slog.New(slog.DiscardHandler)
	m := newFloatingManager(cfg, nil, log)
	if err := m.Apply(context.Background(), agentproto.FloatingIPs{Addresses: []string{"10.0.0.1"}}); err == nil {
		t.Fatal("private address accepted")
	}
	if err := m.Apply(context.Background(), agentproto.FloatingIPs{Addresses: []string{"203.0.113.10", "2001:db8:f::1"}}); err != nil {
		t.Fatal(err)
	}
	if a := <-got; !slices.Equal(a, []string{"2001:db8:f::1", "203.0.113.10"}) {
		t.Fatalf("applied %v", a)
	}
	if pub := m.exclude([]string{"198.51.100.1", "203.0.113.10"}); !slices.Equal(pub, []string{"198.51.100.1"}) {
		t.Fatalf("public addresses %v", pub)
	}

	// A new start: the saved set goes back to the interface by itself.
	m = newFloatingManager(cfg, nil, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.run(ctx)
	select {
	case a := <-got:
		if len(a) != 2 {
			t.Fatalf("restored %v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the saved set was not put back")
	}
}
