package agenthub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/pki"
)

// takeoverCheckEvery: how often the panel asks its standbys whether one
// took over (the first time soon after it starts).
const takeoverCheckEvery = time.Minute

// Agents learn the panel's epoch and the addresses of the panel and its
// standbys (agentproto.PanelInfo); one that knows a higher epoch than
// this panel's is refused: a standby took over.

// panelState is what the hub knows of the panel's place among its copies.
type panelState struct {
	url string // this panel's agent port, as agents reach it
	// Superseded: an agent knew a higher epoch (a standby took over).
	supersededBy int64
	supersededAt time.Time
	// onInfo runs when the addresses change (standby panels learn them).
	onInfo func(ctx context.Context)
}

// SetPanelURL sets the address agents reach this panel's agent port at.
// Call before serving.
func (h *Hub) SetPanelURL(u string) { h.panel.url = u }

// Superseded reports whether a standby took over from this panel: an agent
// came with a higher epoch, and when it was last seen.
func (h *Hub) Superseded() (epoch int64, at time.Time, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.panel.supersededBy, h.panel.supersededAt, h.panel.supersededBy > 0
}

// PanelInfo is what agents are told about the panel and its standbys.
func (h *Hub) PanelInfo(ctx context.Context) (agentproto.PanelInfo, error) {
	epoch, err := h.db.PanelEpoch(ctx)
	if err != nil {
		return agentproto.PanelInfo{}, err
	}
	info := agentproto.PanelInfo{Epoch: epoch, Panels: []string{}}
	add := func(u string) {
		if u == "" || len(info.Panels) >= agentproto.MaxPanels || agentproto.ValidPanelURL(u) != nil {
			return
		}
		for _, have := range info.Panels {
			if have == u {
				return
			}
		}
		info.Panels = append(info.Panels, u)
	}
	add(h.panel.url)
	list, err := h.db.Standbys(ctx)
	if err != nil {
		return agentproto.PanelInfo{}, err
	}
	for _, sb := range list {
		if sb.EnrolledAt != nil {
			add(sb.AgentURL)
		}
	}
	return info, nil
}

// OnPanelInfo registers what else to tell when the panel's addresses
// change (the standby panels). Call before serving.
func (h *Hub) OnPanelInfo(fn func(ctx context.Context)) { h.panel.onInfo = fn }

// PushPanelInfo tells every connected agent the panel's addresses (a
// standby was added, removed or moved).
func (h *Hub) PushPanelInfo(ctx context.Context) {
	if h.panel.onInfo != nil {
		h.panel.onInfo(ctx)
	}
	h.mu.Lock()
	ids := make([]int64, 0, len(h.conns))
	for id := range h.conns {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	for _, id := range ids {
		go h.sendPanelInfo(ctx, id)
	}
}

func (h *Hub) sendPanelInfo(ctx context.Context, id int64) {
	info, err := h.PanelInfo(ctx)
	if err != nil {
		h.log.Error("panel addresses", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Agents before 0.5 do not know the message: they stay with this panel.
	if err := h.Call(ctx, id, agentproto.TypePanelInfo, info, nil); err != nil {
		h.log.Debug("tell the agent the panel's addresses", "server_id", id, "err", err)
	}
}

// superseded refuses an agent that knows a newer panel than this one; it
// writes the response itself.
func (h *Hub) superseded(w http.ResponseWriter, r *http.Request, id int64) bool {
	known, err := strconv.ParseInt(r.Header.Get(agentproto.HeaderPanelEpoch), 10, 64)
	if err != nil || known <= 0 {
		return false
	}
	epoch, err := h.db.PanelEpoch(r.Context())
	if err != nil {
		h.internalError(w, "load panel epoch", err)
		return true
	}
	if known <= epoch {
		return false
	}
	h.MarkSuperseded(epoch, known, "server_id", id)
	http.Error(w, "this panel was replaced by a standby that took over (epoch "+strconv.FormatInt(known, 10)+")", http.StatusConflict)
	return true
}

// MarkSuperseded records that a panel of a higher epoch took over (an
// agent or a standby panel knew it).
func (h *Hub) MarkSuperseded(ours, theirs int64, why ...any) {
	h.mu.Lock()
	first := h.panel.supersededBy == 0
	h.panel.supersededBy, h.panel.supersededAt = max(h.panel.supersededBy, theirs), h.now()
	h.mu.Unlock()
	if first {
		h.log.Error("a standby panel took over from this panel: its agents no longer connect here",
			append([]any{"epoch", ours, "new_epoch", theirs}, why...)...)
	}
}

func (h *Hub) handleEpoch(w http.ResponseWriter, r *http.Request) {
	epoch, err := h.db.PanelEpoch(r.Context())
	if err != nil {
		h.internalError(w, "load panel epoch", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"epoch": epoch})
}

// takeoverLoop asks the standbys, now and then, whether one of them took
// over: a panel that comes back after its standby was promoted learns it
// even if no agent tries it.
func (h *Hub) takeoverLoop(ctx context.Context) {
	t := time.NewTimer(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			t.Reset(takeoverCheckEvery)
		}
		h.checkTakeover(ctx)
	}
}

func (h *Hub) checkTakeover(ctx context.Context) {
	ours, err := h.db.PanelEpoch(ctx)
	if err != nil {
		return
	}
	list, err := h.db.Standbys(ctx)
	if err != nil {
		return
	}
	roots := x509.NewCertPool()
	roots.AddCert(h.ca.Cert)
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: pki.PanelServerName},
	}}
	defer hc.CloseIdleConnections()
	for _, sb := range list {
		if sb.AgentURL == "" || agentproto.ValidPanelURL(sb.AgentURL) != nil {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, sb.AgentURL+agentproto.PathPanelEpoch, nil)
		if err != nil {
			continue
		}
		resp, err := hc.Do(req)
		if err != nil {
			continue // a standby does not listen there until it takes over
		}
		var v struct {
			Epoch int64 `json:"epoch"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&v)
		resp.Body.Close()
		if err == nil && resp.StatusCode == http.StatusOK && v.Epoch > ours {
			h.MarkSuperseded(ours, v.Epoch, "standby", sb.Name)
		}
	}
}
