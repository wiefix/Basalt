package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/proxy"
)

const accessLogTail = 1 << 20 // bytes read from the end of the log

// accessLog returns the latest requests (newest first) from the access log
// the proxy writes for a site or app.
func (a *Agent) accessLog(req agentproto.AccessLogRequest) (agentproto.AccessLogResponse, error) {
	name := proxy.AccessLogName(proxy.Route{SiteID: req.SiteID, AppID: req.AppID})
	if name == "" || (req.SiteID > 0 && req.AppID > 0) {
		return agentproto.AccessLogResponse{}, errors.New("a site or an app is required")
	}
	limit := min(max(req.Limit, 1), 1000)
	out := agentproto.AccessLogResponse{Entries: []agentproto.AccessEntry{}}
	f, err := os.Open(proxy.AccessLogPath(a.proxy.opts.Dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	} else if err != nil {
		return out, publicError(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return out, publicError(err)
	}
	off := max(0, st.Size()-accessLogTail)
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return out, publicError(err)
	}
	lines := bytes.Split(buf, []byte("\n"))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // cut in the middle
	}
	for i := len(lines) - 1; i >= 0 && len(out.Entries) < limit; i-- {
		if e, ok := parseAccessLine(lines[i]); ok {
			out.Entries = append(out.Entries, e)
		}
	}
	return out, nil
}

// parseAccessLine reads one line of Caddy's JSON access log.
func parseAccessLine(line []byte) (agentproto.AccessEntry, bool) {
	var l struct {
		TS      float64 `json:"ts"`
		Request struct {
			ClientIP string              `json:"client_ip"`
			RemoteIP string              `json:"remote_ip"`
			Proto    string              `json:"proto"`
			Method   string              `json:"method"`
			Host     string              `json:"host"`
			URI      string              `json:"uri"`
			Headers  map[string][]string `json:"headers"`
		} `json:"request"`
		Duration float64 `json:"duration"`
		Size     int64   `json:"size"`
		Status   int     `json:"status"`
	}
	if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &l) != nil || l.TS == 0 {
		return agentproto.AccessEntry{}, false
	}
	sec, frac := math.Modf(l.TS)
	ip := l.Request.ClientIP
	if ip == "" {
		ip = l.Request.RemoteIP
	}
	ua := ""
	if v := l.Request.Headers["User-Agent"]; len(v) > 0 {
		ua = v[0]
	}
	cut := func(s string, n int) string {
		s = strings.ToValidUTF8(s, "?")
		if len(s) > n {
			return s[:n]
		}
		return s
	}
	return agentproto.AccessEntry{
		At: time.Unix(int64(sec), int64(frac*1e9)).UTC(), IP: cut(ip, 64), Method: cut(l.Request.Method, 16),
		Host: cut(l.Request.Host, 255), URI: cut(l.Request.URI, 2048), Proto: cut(l.Request.Proto, 16),
		Status: l.Status, Bytes: l.Size, DurationMS: math.Round(l.Duration*1e6) / 1e3, UserAgent: cut(ua, 512),
	}, true
}
