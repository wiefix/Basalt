package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// The panel tells the agent its epoch and the addresses of the panel and
// its standbys (agentproto.PanelInfo). They are kept in agent.json: when
// the panel cannot be reached, the agent tries the others, and it tells
// each the epoch it knows, so a former panel refuses it.

// panelURLs are the addresses to try, the one that last worked first.
func (a *Agent) panelURLs() []string {
	cfg := a.identity().config
	out := []string{cfg.PanelURL}
	for _, u := range cfg.Panels {
		if !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}

// updateConfig changes agent.json and the agent's identity; change
// reports whether there is anything to write.
func (a *Agent) updateConfig(change func(*config) bool) error {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	next := a.identity().config
	next.Panels = slices.Clone(next.Panels)
	if !change(&next) {
		return nil
	}
	b, _ := json.MarshalIndent(next, "", "  ")
	if err := writeAtomic(filepath.Join(a.dir, fileConfig), b, 0o600); err != nil {
		return err
	}
	a.mu.Lock()
	id := *a.id
	id.config = next
	a.id = &id
	a.mu.Unlock()
	return nil
}

// setPanelInfo records what the panel says of itself and its standbys.
func (a *Agent) setPanelInfo(info agentproto.PanelInfo) error {
	if info.Epoch < 0 || len(info.Panels) > agentproto.MaxPanels {
		return errors.New("invalid panel information")
	}
	for _, u := range info.Panels {
		if err := agentproto.ValidPanelURL(u); err != nil {
			return fmt.Errorf("%s: %w", u, err)
		}
	}
	return a.updateConfig(func(c *config) bool {
		if info.Epoch < c.Epoch {
			// The hub refuses agents that know a newer panel: not reached.
			return false
		}
		if info.Epoch == c.Epoch && slices.Equal(info.Panels, c.Panels) {
			return false
		}
		if info.Epoch > c.Epoch {
			a.log.Info("the panel was taken over by a standby", "epoch", info.Epoch)
		}
		c.Epoch, c.Panels = info.Epoch, slices.Clone(info.Panels)
		return true
	})
}

// usePanel remembers the address that worked, to try it first.
func (a *Agent) usePanel(u string) error {
	return a.updateConfig(func(c *config) bool {
		if c.PanelURL == u {
			return false
		}
		a.log.Info("connecting to the panel at another address from now on", "panel", u, "was", c.PanelURL)
		c.PanelURL = u
		return true
	})
}

// panelLink is whether the agent is connected to the panel and since when
// (a standby panel asks before it takes over: agentproto.PanelStatus).
type panelLink struct {
	mu        sync.Mutex
	connected bool
	since     time.Time
}

func (l *panelLink) set(connected bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.connected != connected || l.since.IsZero() {
		l.connected, l.since = connected, time.Now()
	}
}

// panelStatus answers agentproto.ServicePanelStatus.
func (a *Agent) panelStatus() agentproto.PanelStatus {
	a.link.mu.Lock()
	defer a.link.mu.Unlock()
	return agentproto.PanelStatus{Connected: a.link.connected, For: int64(time.Since(a.link.since) / time.Second),
		Epoch: a.identity().Epoch}
}
