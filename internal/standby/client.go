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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
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
	PanelURL  string `json:"panel_url"` // the address that last worked
	StandbyID int64  `json:"standby_id"`
	// Panels and Epoch are what the panel last told (kindPanels): the
	// addresses to try when PanelURL fails, and its epoch.
	Panels []string `json:"panels,omitempty"`
	Epoch  int64    `json:"epoch,omitempty"`
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
	return readJSON(resp, out)
}

func readJSON(resp *http.Response, out any) error {
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
	// agentURL is where agents will reach this standby once it takes over
	// (its own agent port), told to the panel.
	agentURL string

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

// NewClient loads the standby panel in dir; agentURL is the address of its
// agent port.
func NewClient(dir, agentURL string, log *slog.Logger) (*Client, error) {
	c := &Client{dir: dir, log: log, agentURL: agentURL}
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
	Epoch     int64     `json:"epoch"`
}

func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Status{Panel: c.cfg.PanelURL, StandbyID: c.cfg.StandbyID, Connected: c.connected, Ready: c.ready,
		Applied: c.applied, Error: c.lastErr, Removed: c.removed, Epoch: c.cfg.Epoch}
}

// panelURLs are the addresses to try, the one that last worked first. The
// standby's own address is left out: it answers there only once promoted.
func (c *Client) panelURLs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []string{c.cfg.PanelURL}
	for _, u := range c.cfg.Panels {
		if u != c.agentURL && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}

// updateConfig changes standby.json; change reports whether there is
// anything to write.
func (c *Client) updateConfig(change func(*config) bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.cfg
	next.Panels = slices.Clone(next.Panels)
	if !change(&next) {
		return nil
	}
	b, _ := json.MarshalIndent(next, "", "  ")
	if err := writeAtomic(filepath.Join(c.dir, FileConfig), b, 0o600); err != nil {
		return err
	}
	c.cfg = next
	return nil
}

// setPanels records what the panel says of itself and its standbys.
func (c *Client) setPanels(data []byte) error {
	var info agentproto.PanelInfo
	if err := json.Unmarshal(data, &info); err != nil || info.Epoch < 0 || len(info.Panels) > agentproto.MaxPanels {
		return errors.New("the panel sent invalid addresses")
	}
	for _, u := range info.Panels {
		if err := agentproto.ValidPanelURL(u); err != nil {
			return fmt.Errorf("the panel sent an invalid address %s: %w", u, err)
		}
	}
	return c.updateConfig(func(cfg *config) bool {
		if info.Epoch < cfg.Epoch || (info.Epoch == cfg.Epoch && slices.Equal(info.Panels, cfg.Panels)) {
			return false
		}
		if info.Epoch > cfg.Epoch && c.log != nil {
			c.log.Info("the panel was taken over by a standby", "epoch", info.Epoch)
		}
		cfg.Epoch, cfg.Panels = info.Epoch, slices.Clone(info.Panels)
		return true
	})
}

// usePanel remembers the address that worked, to try it first.
func (c *Client) usePanel(u string) error {
	return c.updateConfig(func(cfg *config) bool {
		if cfg.PanelURL == u {
			return false
		}
		c.log.Info("following the panel at another address from now on", "panel", u, "was", cfg.PanelURL)
		cfg.PanelURL = u
		return true
	})
}

func (c *Client) epoch() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.Epoch
}

// checkEpoch refuses a panel older than the one the standby knows (it was
// replaced): it would hand out a stale database.
func (c *Client) checkEpoch(resp *http.Response) error {
	known := c.epoch()
	if known == 0 {
		return nil
	}
	got, err := strconv.ParseInt(resp.Header.Get(agentproto.HeaderPanelEpoch), 10, 64)
	if err != nil || got < known {
		return fmt.Errorf("the panel there was replaced by a standby that took over (epoch %d)", known)
	}
	return nil
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

// Lock keeps a promotion from changing the copy while this process runs
// (call before Run); it fails if another process holds the standby.
func (c *Client) Lock() (unlock func(), err error) {
	unlock, err = lockDir(c.dir)
	if err != nil {
		return nil, fmt.Errorf("another process holds the standby panel in %s (%s): %w", c.dir, fileLock, err)
	}
	return unlock, nil
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
		// The addresses in turn: the next one at once when one cannot be
		// followed, a pause after a whole round or a stream that ended.
		for _, u := range c.panelURLs() {
			start := time.Now()
			connected, err := c.follow(ctx, u)
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
			c.log.Warn("standby stream ended", "panel", u, "err", err)
			if connected {
				if time.Since(start) > time.Minute {
					backoff = time.Second
				}
				break
			}
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
func (c *Client) renew(ctx context.Context, panel string) {
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
	if err := c.postJSON(ctx, panel+PathRenew, RenewRequest{CSR: csr}, &resp); err != nil {
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

// postJSON is a request to the panel at url, with the epoch the standby
// knows; a panel older than that is not believed.
func (c *Client) postJSON(ctx context.Context, url string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(agentproto.HeaderPanelEpoch, strconv.FormatInt(c.epoch(), 10))
	resp, err := c.httpClient(30 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := c.checkEpoch(resp); err != nil {
		return err
	}
	return readJSON(resp, out)
}

// follow runs one stream from the panel at panel: the master key, then the
// copy of the database. connected: the panel took the standby.
func (c *Client) follow(ctx context.Context, panel string) (connected bool, err error) {
	c.renew(ctx, panel)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	defer pw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, panel+PathReplicate, pr)
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set(agentproto.HeaderPanelEpoch, strconv.FormatInt(c.epoch(), 10))
	resp, err := c.httpClient(0).Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone {
		return false, ErrRemoved
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("the panel at %s refused (%s): %s", panel, resp.Status, strings.TrimSpace(string(msg)))
	}
	if err := c.checkEpoch(resp); err != nil {
		return false, fmt.Errorf("%s: %w", panel, err)
	}
	if err := c.usePanel(panel); err != nil {
		return true, err
	}
	c.mu.Lock()
	c.connected, c.lastErr = true, ""
	c.mu.Unlock()
	c.log.Info("following the panel", "panel", panel)
	return true, c.stream(ctx, cancel, resp, pw)
}

// stream applies what the panel sends and reports back until it ends.
func (c *Client) stream(ctx context.Context, cancel context.CancelFunc, resp *http.Response, pw *io.PipeWriter) error {

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
			if enc.Encode(Report{Ready: st.Ready, Applied: st.Applied, Error: st.Error, Version: buildinfo.Version, AgentURL: c.agentURL}) != nil {
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
		case kindPanels:
			if err := c.setPanels(msg.Data); err != nil {
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
<p>To make this server the panel (when the panel is gone): <code>systemctl stop basalt-server &amp;&amp; basalt server promote &amp;&amp; systemctl start basalt-server</code>, then point the panel's domain here.</p>
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
