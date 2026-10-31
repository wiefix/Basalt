package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/proxy"
)

const (
	proxyLogScan     = 4 << 20 // bytes read from the end of the log for the last lines
	proxyLogPoll     = time.Second
	proxyLogMaxLine  = 8 << 10
	proxyLogMaxValue = 2 << 10
)

// proxyLogLevels ranks Caddy's (zap's) levels; a filter shows its level
// and above.
var proxyLogLevels = map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3, "dpanic": 4, "panic": 5, "fatal": 6}

func proxyLogMin(level string) (int, error) {
	switch level {
	case "":
		return 0, nil
	case "warn", "error":
		return proxyLogLevels[level], nil
	}
	return 0, errors.New(`level must be "warn" or "error"`)
}

// proxyLog returns the last lines of the proxy's own log, readable and
// without host paths.
func (a *Agent) proxyLog(req agentproto.ProxyLogRequest) (agentproto.ProxyLogResponse, error) {
	minLevel, err := proxyLogMin(req.Level)
	if err != nil {
		return agentproto.ProxyLogResponse{}, err
	}
	f, err := os.Open(proxy.OwnLogPath(a.proxy.opts.Dir))
	if errors.Is(err, os.ErrNotExist) {
		return agentproto.ProxyLogResponse{}, nil
	} else if err != nil {
		return agentproto.ProxyLogResponse{}, publicError(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return agentproto.ProxyLogResponse{}, publicError(err)
	}
	text, err := a.proxyLogTail(f, st.Size(), req.Tail, minLevel)
	return agentproto.ProxyLogResponse{Log: text}, publicError(err)
}

// proxyLogTail formats the last tail lines (of minLevel and above) before
// offset end of f.
func (a *Agent) proxyLogTail(f *os.File, end int64, tail, minLevel int) (string, error) {
	tail = min(max(tail, 1), maxLogTail)
	off := max(0, end-proxyLogScan)
	buf := make([]byte, end-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	lines := bytes.Split(buf, []byte("\n"))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // cut in the middle
	}
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < tail; i-- {
		if s, ok := formatProxyLine(lines[i], minLevel); ok {
			out = append(out, s)
		}
	}
	var b strings.Builder
	for i := len(out) - 1; i >= 0; i-- {
		b.WriteString(out[i])
		b.WriteByte('\n')
	}
	return a.paths.text(b.String()), nil
}

// followProxyLog sends the last tail lines of the proxy's log, then new
// lines as the proxy writes them, across rotations, until ctx ends or send
// fails.
func (a *Agent) followProxyLog(ctx context.Context, req agentproto.ProxyLogRequest, send func(string) error) error {
	minLevel, err := proxyLogMin(req.Level)
	if err != nil {
		return err
	}
	path := proxy.OwnLogPath(a.proxy.opts.Dir)
	var f *os.File
	var st os.FileInfo
	var off int64
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	open := func() error {
		nf, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil // not written yet: try again later
		} else if err != nil {
			return publicError(err)
		}
		nst, err := nf.Stat()
		if err != nil {
			nf.Close()
			return publicError(err)
		}
		f, st, off = nf, nst, 0
		return nil
	}
	if err := open(); err != nil {
		return err
	}
	if f != nil {
		off = st.Size()
		text, err := a.proxyLogTail(f, off, req.Tail, minLevel)
		if err != nil {
			return publicError(err)
		}
		if text != "" {
			if err := send(text); err != nil {
				return err
			}
		}
	}

	var partial []byte // a line the proxy has not finished writing
	// drain sends what was appended to the open file since off.
	drain := func() error {
		if f == nil {
			return nil
		}
		if cur, err := f.Stat(); err == nil && cur.Size() < off {
			off, partial = 0, nil // truncated
		}
		var b strings.Builder
		flush := func() error {
			if b.Len() == 0 {
				return nil
			}
			text := a.paths.text(b.String())
			b.Reset()
			return send(text)
		}
		chunk := make([]byte, 256<<10)
		for {
			n, err := f.ReadAt(chunk, off)
			off += int64(n)
			data := append(partial, chunk[:n]...)
			partial = nil
			for {
				i := bytes.IndexByte(data, '\n')
				if i < 0 {
					break
				}
				if s, ok := formatProxyLine(data[:i], minLevel); ok {
					b.WriteString(s)
					b.WriteByte('\n')
					if b.Len() >= logBatchBytes {
						if err := flush(); err != nil {
							return err
						}
					}
				}
				data = data[i+1:]
			}
			if len(data) > proxyLogMaxLine*4 {
				data = nil // not a log line; do not grow without bound
			}
			partial = append([]byte(nil), data...)
			if n < len(chunk) || err != nil {
				break
			}
		}
		return flush()
	}

	t := time.NewTicker(proxyLogPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if err := drain(); err != nil {
			return err
		}
		// Rotated: the old file is complete (drained above); continue with
		// the new one from its start.
		if cur, err := os.Stat(path); err == nil && (f == nil || !os.SameFile(st, cur)) {
			if f != nil {
				f.Close()
				f, partial = nil, nil
			}
			if err := open(); err != nil {
				return err
			}
			if err := drain(); err != nil {
				return err
			}
		}
	}
}

// formatProxyLine turns one JSON line of Caddy's log into
// "time LEVEL logger: message key=value …", or reports false for a line
// below minLevel or a known routine one.
func formatProxyLine(line []byte, minLevel int) (string, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return "", false
	}
	fields, ok := jsonFields(line)
	if !ok {
		return cutString(strings.ToValidUTF8(string(line), "?"), proxyLogMaxLine), true
	}
	var level, logger, msg string
	var ts float64
	var rest []jsonField
	for _, f := range fields {
		switch f.key {
		case "level":
			json.Unmarshal(f.value, &level)
		case "ts":
			json.Unmarshal(f.value, &ts)
		case "logger":
			json.Unmarshal(f.value, &logger)
		case "msg":
			json.Unmarshal(f.value, &msg)
		default:
			rest = append(rest, f)
		}
	}
	if rank, known := proxyLogLevels[level]; known && rank < minLevel {
		return "", false
	}
	if routineProxyLine(logger, msg, rest) {
		return "", false
	}
	var b strings.Builder
	if ts > 0 {
		sec, frac := math.Modf(ts)
		b.WriteString(time.Unix(int64(sec), int64(frac*1e9)).UTC().Format("2006-01-02T15:04:05.000Z"))
		b.WriteByte(' ')
	}
	b.WriteString(strings.ToUpper(level))
	if logger != "" {
		b.WriteString(" " + logger + ":")
	}
	b.WriteString(" " + msg)
	for _, f := range rest {
		if v, ok := fieldValue(f); ok {
			b.WriteString(" " + f.key + "=" + v)
		}
	}
	return cutString(strings.ToValidUTF8(b.String(), "?"), proxyLogMaxLine), true
}

// routineProxyLine tells lines that say nothing to an admin: the agent
// asks the admin API every few seconds whether the proxy runs its
// configuration (Caddy logs each request, and a 404 while it does not),
// and every load warns about the admin unix socket and the plain HTTP
// port having no HTTP/2 and HTTP/3.
func routineProxyLine(logger, msg string, fields []jsonField) bool {
	switch logger {
	case "admin.api":
		if msg == "received request" {
			return true
		}
		for _, f := range fields {
			var e string
			if f.key == "error" && json.Unmarshal(f.value, &e) == nil && strings.HasPrefix(e, "unknown object ID") {
				return true
			}
		}
	case "admin":
		return strings.HasPrefix(msg, "admin endpoint on open interface")
	case "http":
		return msg == "HTTP/2 skipped because it requires TLS" || msg == "HTTP/3 skipped because it requires TLS"
	}
	return false
}

type jsonField struct {
	key   string
	value json.RawMessage
}

// jsonFields splits a JSON object into its fields, in order.
func jsonFields(line []byte) ([]jsonField, bool) {
	dec := json.NewDecoder(bytes.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var out []jsonField
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		out = append(out, jsonField{key, v})
	}
	return out, true
}

// fieldValue renders a field's value: strings bare unless they need
// quotes, objects and arrays as compact JSON without HTTP headers (Caddy
// redacts credentials in them, but cookies of other names and the like
// are not the proxy log's business).
func fieldValue(f jsonField) (string, bool) {
	if strings.HasSuffix(f.key, "headers") {
		return "", false
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(f.value))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
		if s == "" || strings.ContainsAny(s, " \t\n\"=") {
			s = strconv.Quote(s)
		}
	case map[string]any, []any:
		b, err := json.Marshal(dropHeaders(x))
		if err != nil {
			return "", false
		}
		s = string(b)
	default:
		s = string(f.value)
	}
	return cutString(s, proxyLogMaxValue), true
}

func dropHeaders(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if strings.HasSuffix(k, "headers") {
				delete(x, k)
			} else {
				x[k] = dropHeaders(e)
			}
		}
	case []any:
		for i, e := range x {
			x[i] = dropHeaders(e)
		}
	}
	return v
}

func cutString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
