package standby

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/buildinfo"
	"github.com/wiefix/basalt/internal/pki"
	"github.com/wiefix/basalt/internal/walship"
)

// Files of a standby panel in its data directory, next to the copy of the
// database (basalt.db) and the master key (master.key).
const (
	FileConfig  = "standby.json"
	fileCreds   = "standby.pem"
	fileCA      = "standby-ca.crt"
	fileRemoved = "standby-removed"
	fileDB      = "basalt.db"
	fileKey     = "master.key"
)

// ErrRemoved: the panel removed this standby; it stops following.
var ErrRemoved = errors.New("the panel removed this standby panel")

type config struct {
	PanelURL  string `json:"panel_url"`
	StandbyID int64  `json:"standby_id"`
}

// IsStandby reports whether dir belongs to a standby panel.
func IsStandby(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, FileConfig))
	return err == nil
}

type EnrollOptions struct {
	DataDir  string
	PanelURL string // https://host:port of the agents' port
	Token    string
	CASHA256 string
}

// Enroll makes dir a standby panel of the panel at o.PanelURL. The
// directory must not hold a panel's database already.
func Enroll(ctx context.Context, o EnrollOptions) (int64, error) {
	if IsStandby(o.DataDir) {
		return 0, fmt.Errorf("%s already belongs to a standby panel", o.DataDir)
	}
	if _, err := os.Stat(filepath.Join(o.DataDir, fileDB)); err == nil {
		return 0, fmt.Errorf("%s holds a panel's database: a standby needs an empty data directory", o.DataDir)
	}
	base, err := panelURL(o.PanelURL)
	if err != nil {
		return 0, err
	}
	if o.Token == "" || o.CASHA256 == "" {
		return 0, errors.New("token and CA fingerprint are required")
	}
	key, err := pki.NewKey()
	if err != nil {
		return 0, err
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion:            tls.VersionTLS13,
		InsecureSkipVerify:    true, // checked against the pinned CA instead
		VerifyPeerCertificate: pki.VerifyPinnedPanel(o.CASHA256, time.Now),
	}}}
	var er EnrollResponse
	if err := postJSON(ctx, client, base+PathEnroll, EnrollRequest{Token: o.Token, CSR: csr}, &er); err != nil {
		return 0, fmt.Errorf("enroll: %w", err)
	}
	if pki.Fingerprint(er.CACert) != strings.ToLower(strings.ReplaceAll(o.CASHA256, ":", "")) {
		return 0, errors.New("panel returned a CA that does not match the pinned fingerprint")
	}
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return 0, err
	}
	if err := writeAtomic(filepath.Join(o.DataDir, fileCA), pemBytes("CERTIFICATE", er.CACert), 0o644); err != nil {
		return 0, err
	}
	if err := saveCreds(o.DataDir, key, er.Cert); err != nil {
		return 0, err
	}
	b, _ := json.MarshalIndent(config{PanelURL: base, StandbyID: er.StandbyID}, "", "  ")
	if err := writeAtomic(filepath.Join(o.DataDir, FileConfig), b, 0o600); err != nil {
		return 0, err
	}
	return er.StandbyID, nil
}

func panelURL(s string) (string, error) {
	u, err := url.Parse(strings.TrimRight(s, "/"))
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return "", fmt.Errorf("panel URL must look like https://host:port, got %q", s)
	}
	return u.Scheme + "://" + u.Host, nil
}

func postJSON(ctx context.Context, c *http.Client, url string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone {
		return ErrRemoved
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// Client keeps a standby panel's copy of the panel.
type Client struct {
	dir string
	log *slog.Logger

	mu        sync.Mutex
	cfg       config
	key       *ecdsa.PrivateKey
	cert      *x509.Certificate
	ca        *x509.Certificate
	connected bool
	ready     bool
	applied   time.Time
	lastErr   string
	removed   bool
}

func NewClient(dir string, log *slog.Logger) (*Client, error) {
	c := &Client{dir: dir, log: log}
	b, err := os.ReadFile(filepath.Join(dir, FileConfig))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &c.cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", FileConfig, err)
	}
	if c.key, c.cert, err = readCreds(filepath.Join(dir, fileCreds)); err != nil {
		return nil, err
	}
	if c.ca, err = readCert(filepath.Join(dir, fileCA)); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, fileRemoved)); err == nil {
		c.removed = true
	}
	return c, nil
}

// Status is how the standby's copy goes.
type Status struct {
	Panel     string    `json:"panel"`
	StandbyID int64     `json:"standby_id"`
	Connected bool      `json:"connected"`
	Ready     bool      `json:"ready"`
	Applied   time.Time `json:"applied"`
	Error     string    `json:"error,omitempty"`
	Removed   bool      `json:"removed"`
}

func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Status{Panel: c.cfg.PanelURL, StandbyID: c.cfg.StandbyID, Connected: c.connected, Ready: c.ready,
		Applied: c.applied, Error: c.lastErr, Removed: c.removed}
}

func (c *Client) tlsCert() *tls.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &tls.Certificate{Certificate: [][]byte{c.cert.Raw}, PrivateKey: c.key, Leaf: c.cert}
}

func (c *Client) httpClient(timeout time.Duration) *http.Client {
	roots := x509.NewCertPool()
	roots.AddCert(c.ca)
	var protocols http.Protocols
	protocols.SetHTTP2(true)
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		Protocols: &protocols,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    roots,
			ServerName: pki.PanelServerName,
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				return c.tlsCert(), nil
			},
		},
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}}
}

// Run follows the panel until ctx ends or the panel removes this standby.
func (c *Client) Run(ctx context.Context) error {
	if c.Status().Removed {
		c.log.Error("this standby panel was removed from the panel; not following it")
		<-ctx.Done()
		return nil
	}
	backoff := time.Second
	for {
		c.renew(ctx)
		start := time.Now()
		err := c.follow(ctx)
		c.mu.Lock()
		c.connected = false
		if err != nil && ctx.Err() == nil {
			c.lastErr = err.Error()
		}
		c.mu.Unlock()
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrRemoved) {
			os.WriteFile(filepath.Join(c.dir, fileRemoved), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
			c.mu.Lock()
			c.removed = true
			c.mu.Unlock()
			c.log.Error("the panel removed this standby panel; not following it any more")
			<-ctx.Done()
			return nil
		}
		c.log.Warn("standby stream ended", "err", err)
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff + time.Duration(rand.Int64N(int64(backoff)/2+1))):
		}
		backoff = min(backoff*2, time.Minute)
	}
}

// renew gets a new certificate once a third of the current one is left.
func (c *Client) renew(ctx context.Context) {
	c.mu.Lock()
	cert := c.cert
	c.mu.Unlock()
	if time.Until(cert.NotAfter) > cert.NotAfter.Sub(cert.NotBefore)/3 {
		return
	}
	key, err := pki.NewKey()
	if err != nil {
		return
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		return
	}
	var resp RenewResponse
	if err := postJSON(ctx, c.httpClient(30*time.Second), c.cfg.PanelURL+PathRenew, RenewRequest{CSR: csr}, &resp); err != nil {
		c.log.Warn("renew the standby certificate", "err", err)
		return
	}
	nc, err := x509.ParseCertificate(resp.Cert)
	if err != nil || !key.PublicKey.Equal(nc.PublicKey) {
		c.log.Warn("renew the standby certificate: the panel returned another key")
		return
	}
	if err := saveCreds(c.dir, key, resp.Cert); err != nil {
		c.log.Warn("save the standby certificate", "err", err)
		return
	}
	c.mu.Lock()
	c.key, c.cert = key, nc
	c.mu.Unlock()
	c.log.Info("standby certificate renewed", "until", nc.NotAfter)
}

// follow runs one stream: the master key, then the copy of the database.
func (c *Client) follow(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	defer pw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.PanelURL+PathReplicate, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.httpClient(0).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone {
		return ErrRemoved
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("the panel refused (%s): %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	c.mu.Lock()
	c.connected, c.lastErr = true, ""
	c.mu.Unlock()
	c.log.Info("following the panel", "panel", c.cfg.PanelURL)

	m := walship.NewMirror(filepath.Join(c.dir, fileDB))
	defer func() {
		m.Close()
		c.mu.Lock()
		c.ready = false
		c.mu.Unlock()
	}()
	// Reports to the panel.
	go func() {
		t := time.NewTicker(reportEvery)
		defer t.Stop()
		enc := json.NewEncoder(pw)
		for {
			st := c.Status()
			if enc.Encode(Report{Ready: st.Ready, Applied: st.Applied, Error: st.Error, Version: buildinfo.Version}) != nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	// A silent stream is dead: the panel pings it.
	alive := time.AfterFunc(silence, cancel)
	defer alive.Stop()
	// A read of the body does not end with ctx by itself.
	stopRead := context.AfterFunc(ctx, func() { resp.Body.Close() })
	defer stopRead()
	r := bufio.NewReaderSize(resp.Body, 1<<20)
	for {
		msg, err := readFrame(r)
		if err != nil {
			if ctx.Err() != nil && !alive.Stop() {
				return errors.New("the panel went silent")
			}
			return err
		}
		alive.Reset(silence)
		switch msg.Kind {
		case kindPing:
			continue
		case kindMasterKey:
			if err := c.saveKey(msg.Data); err != nil {
				return err
			}
			continue
		}
		if err := m.Apply(ctx, msg); err != nil {
			return fmt.Errorf("apply the panel's changes: %w", err)
		}
		c.mu.Lock()
		c.ready, c.applied = m.Ready(), m.Applied
		c.mu.Unlock()
	}
}

// saveKey keeps the panel's master key (it opens the secrets in the copy).
func (c *Client) saveKey(key []byte) error {
	if len(key) == 0 || len(key) > 1024 {
		return errors.New("the panel sent no master key")
	}
	path := filepath.Join(c.dir, fileKey)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, key) {
		return nil
	}
	return writeAtomic(path, key, 0o600)
}

var page = template.Must(template.New("standby").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Basalt standby panel</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:3rem auto;padding:0 1rem;color:#222;background:#fff}
@media (prefers-color-scheme:dark){body{color:#ddd;background:#111}}code{font-size:.9em}</style></head>
<body><h1>Standby panel</h1>
<p>This server keeps a copy of the Basalt panel at <code>{{.Panel}}</code>, ready to take over if it fails.</p>
{{if .Removed}}<p><strong>The panel removed this standby.</strong> It no longer follows the panel.</p>
{{else if .Ready}}<p>The copy is complete{{if not .Applied.IsZero}}; it last changed {{.Applied.UTC.Format "2006-01-02 15:04:05"}} UTC{{end}}.{{if not .Connected}} The panel cannot be reached now.{{end}}</p>
{{else if .Connected}}<p>Copying the panel…</p>
{{else}}<p>The panel cannot be reached{{if .Error}}: {{.Error}}{{end}}.</p>{{end}}
<p>To make this server the panel: <code>basalt server promote</code>, then restart its service.</p>
</body></html>`))

// ServeHTTP answers on the panel's address while this is a standby.
func (c *Client) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	st := c.Status()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch r.URL.Path {
	case "/healthz", "/api/v1/standby":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(st)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	page.Execute(w, st)
}

func saveCreds(dir string, key *ecdsa.PrivateKey, certDER []byte) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, fileCreds), append(pemBytes("PRIVATE KEY", keyDER), pemBytes("CERTIFICATE", certDER)...), 0o600)
}

func readCreds(path string) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	if fi, err := os.Stat(path); err != nil {
		return nil, nil, err
	} else if fi.Mode().Perm()&0o077 != 0 {
		return nil, nil, fmt.Errorf("%s has mode %v; must be 0600", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var key *ecdsa.PrivateKey
	var cert *x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		switch blk.Type {
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
			if err != nil {
				return nil, nil, err
			}
			var ok bool
			if key, ok = k.(*ecdsa.PrivateKey); !ok {
				return nil, nil, errors.New("the standby key is not ECDSA")
			}
		case "CERTIFICATE":
			if cert, err = x509.ParseCertificate(blk.Bytes); err != nil {
				return nil, nil, err
			}
		}
	}
	if key == nil || cert == nil || !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, fmt.Errorf("%s: no matching key and certificate", path)
	}
	return key, cert, nil
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: expected a PEM certificate", path)
	}
	return x509.ParseCertificate(blk.Bytes)
}

func pemBytes(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
