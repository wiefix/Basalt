package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/wiefix/basalt/internal/agentproto"
)

// proxyLogRequest reads ?tail= and ?level= of the proxy log routes.
func proxyLogRequest(w http.ResponseWriter, r *http.Request) (agentproto.ProxyLogRequest, bool) {
	req := agentproto.ProxyLogRequest{Tail: 200, Level: r.URL.Query().Get("level")}
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLogTail {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("tail must be between 1 and %d", maxLogTail))
			return req, false
		}
		req.Tail = n
	}
	switch req.Level {
	case "", "warn", "error":
	default:
		writeError(w, http.StatusUnprocessableEntity, `level must be "warn" or "error"`)
		return req, false
	}
	return req, true
}

// handleProxyLog returns the last lines of the server's proxy log: Caddy's
// own messages (errors, certificate issuance, configuration loads).
func (s *Server) handleProxyLog(w http.ResponseWriter, r *http.Request, p *principal) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	req, ok := proxyLogRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), appCallTimeout)
	defer cancel()
	var resp agentproto.ProxyLogResponse
	if err := s.agents.Call(ctx, srv.ID, agentproto.TypeProxyLog, req, &resp); err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"log": resp.Log})
}

// handleProxyLogStream follows the server's proxy log as server-sent
// events: "log" {text}, and "end" {error} if the server stops it.
func (s *Server) handleProxyLogStream(w http.ResponseWriter, r *http.Request, p *principal) {
	srv, ok := s.pathServer(w, r)
	if !ok {
		return
	}
	req, ok := proxyLogRequest(w, r)
	if !ok {
		return
	}
	if !s.agents.Connected(srv.ID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	if !s.acquireStream(p.user.ID) {
		writeError(w, http.StatusTooManyRequests, "too many open log streams")
		return
	}
	defer s.releaseStream(p.user.ID)
	ev, err := startSSE(w)
	if err != nil {
		return
	}
	ctx := r.Context()
	stop := ev.keepAlive(ctx)
	defer stop()
	err = s.agents.Stream(ctx, srv.ID, agentproto.TypeProxyLogFollow, req, nil, func(raw json.RawMessage) error {
		var c agentproto.LogChunk
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		return ev.send("log", c)
	})
	if ctx.Err() == nil {
		ev.send("end", map[string]string{"error": agentErrorText(err)})
	}
}
