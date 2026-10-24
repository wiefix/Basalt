package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/wiefix/basalt/internal/notify"
	"github.com/wiefix/basalt/internal/standby"
	"github.com/wiefix/basalt/internal/store"
)

// Automatic failover (19.5): the owner chooses one standby panel that
// takes over by itself when the panel is gone. It asks the agents of the
// other servers first (the witnesses: every server with the tunnel on,
// but the panel's own host and the standby's), and takes over only if
// most of them do not reach the panel either (standby.Failover).

const settingFailover = "panel_failover"

const (
	defaultFailoverAfter = 5 * time.Minute
	maxFailoverAfter     = time.Hour
)

type failoverSetting struct {
	StandbyID int64 `json:"standby_id"`
	After     int64 `json:"after_s"`
}

func (s *Server) failoverSetting(ctx context.Context) (failoverSetting, error) {
	var f failoverSetting
	v, err := s.db.Setting(ctx, settingFailover)
	if errors.Is(err, store.ErrNotFound) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	return f, json.Unmarshal([]byte(v), &f)
}

// witnessView is a server a standby asks before it takes over.
type witnessView struct {
	ServerID int64  `json:"server_id"`
	Name     string `json:"name"`
	Addr     string `json:"addr"`
}

// witnesses are the servers standby sb would ask: those with the tunnel
// on, but the panel's own host and sb's.
func (s *Server) witnesses(ctx context.Context, sb *store.Standby) ([]witnessView, error) {
	servers, err := s.db.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	panelHost := s.db.PanelServer(ctx)
	out := []witnessView{}
	for _, srv := range servers {
		if srv.ID == panelHost || srv.ID == sb.ServerID {
			continue
		}
		if addr := peerAddr(srv); addr != "" && len(out) < standby.MaxWitnesses {
			out = append(out, witnessView{ServerID: srv.ID, Name: srv.Name, Addr: addr})
		}
	}
	return out, nil
}

// FailoverFor is what standby id is told: whether it takes over by itself,
// after how long, and whom it asks (standby.Feeder).
func (s *Server) FailoverFor(ctx context.Context, id int64) (standby.Failover, error) {
	f, err := s.failoverSetting(ctx)
	if err != nil || f.StandbyID != id {
		return standby.Failover{}, err
	}
	sb, err := s.db.StandbyByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return standby.Failover{}, nil
	} else if err != nil {
		return standby.Failover{}, err
	}
	ws, err := s.witnesses(ctx, sb)
	if err != nil || len(ws) == 0 {
		return standby.Failover{}, err // nobody to ask: never by itself
	}
	out := standby.Failover{Auto: true, After: f.After}
	for _, w := range ws {
		out.Witnesses = append(out.Witnesses, standby.Witness{ServerID: w.ServerID, Addr: w.Addr})
	}
	return out, nil
}

// failoverView is the automatic failover as the panel shows it.
type failoverView struct {
	// StandbyID takes over by itself (0: none does).
	StandbyID int64 `json:"standby_id"`
	After     int64 `json:"after_s"`
	// Witnesses are the servers it asks.
	Witnesses []witnessView `json:"witnesses"`
}

func (s *Server) viewFailover(ctx context.Context) (failoverView, error) {
	f, err := s.failoverSetting(ctx)
	if err != nil {
		return failoverView{}, err
	}
	v := failoverView{StandbyID: f.StandbyID, After: f.After, Witnesses: []witnessView{}}
	if v.After == 0 {
		v.After = int64(defaultFailoverAfter / time.Second)
	}
	if f.StandbyID == 0 {
		return v, nil
	}
	sb, err := s.db.StandbyByID(ctx, f.StandbyID)
	if errors.Is(err, store.ErrNotFound) {
		v.StandbyID = 0
		return v, nil
	} else if err != nil {
		return v, err
	}
	v.Witnesses, err = s.witnesses(ctx, sb)
	return v, err
}

type failoverRequest struct {
	// StandbyID takes over by itself (0: none).
	StandbyID int64 `json:"standby_id"`
	// After is how long the panel must be gone first, in seconds.
	After int64 `json:"after_s"`
}

func (s *Server) handleSetFailover(w http.ResponseWriter, r *http.Request, p *principal) {
	var req failoverRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	if req.StandbyID != 0 {
		if req.After < int64(s.failoverMin/time.Second) || req.After > int64(maxFailoverAfter/time.Second) {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("the delay must be between %d and %d seconds",
				int64(s.failoverMin/time.Second), int64(maxFailoverAfter/time.Second)))
			return
		}
		sb, err := s.db.StandbyByID(ctx, req.StandbyID)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "standby panel not found")
			return
		} else if err != nil {
			s.internalError(w, r, "load standby panel", err)
			return
		}
		if sb.EnrolledAt == nil {
			writeError(w, http.StatusUnprocessableEntity, "install the standby panel first")
			return
		}
		ws, err := s.witnesses(ctx, sb)
		if err != nil {
			s.internalError(w, r, "list servers", err)
			return
		}
		if len(ws) == 0 {
			writeError(w, http.StatusUnprocessableEntity, "the standby needs at least one other server to ask whether the panel is gone: "+
				"a server besides the panel's and the standby's, with the tunnel on (port 8444)")
			return
		}
	} else {
		req.After = 0
	}
	b, _ := json.Marshal(failoverSetting{StandbyID: req.StandbyID, After: req.After})
	if err := s.db.SetSetting(ctx, settingFailover, string(b)); err != nil {
		s.internalError(w, r, "save failover setting", err)
		return
	}
	s.audit(r, &p.user.ID, "standby.failover_set", map[string]any{"standby_id": req.StandbyID, "after_s": req.After})
	if s.standbys != nil {
		s.standbys.PushPanels(context.WithoutCancel(ctx))
	}
	v, err := s.viewFailover(ctx)
	if err != nil {
		s.internalError(w, r, "failover setting", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"failover": v})
}

// announcePromotion tells, once, that this panel took over from another
// (by hand or by itself).
func (s *Server) announcePromotion(ctx context.Context) {
	p, err := s.db.TakePromotion(ctx)
	if err != nil {
		s.log.Error("read the promotion", "err", err)
		return
	}
	if p == nil {
		return
	}
	how := "by hand (basalt server promote)"
	if p.Auto {
		how = "by itself: the panel was unreachable and most servers agreed"
	}
	if err := s.db.Audit(ctx, nil, "panel.promoted", "", map[string]any{"epoch": p.Epoch, "auto": p.Auto}); err != nil {
		s.log.Error("audit", "err", err)
	}
	s.notify(notify.Message{
		Event: EventPanelFailover,
		Title: "A standby panel took over",
		Text: fmt.Sprintf("This server became the panel %s, at %s (epoch %d). Point the panel's domain here, and keep the former panel stopped.",
			how, p.At.UTC().Format(time.RFC3339), p.Epoch),
		Link: "/servers",
	})
}
