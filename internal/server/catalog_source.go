package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/wiefix/basalt/internal/catalog"
	"github.com/wiefix/basalt/internal/dns"
	"github.com/wiefix/basalt/internal/store"
)

const (
	// settingCatalogSource is where the panel downloads more templates
	// from (catalogSource); settingCatalogCache the last good index.
	settingCatalogSource = "catalog_source"
	settingCatalogCache  = "catalog_cache"
	catalogRefreshEvery  = 6 * time.Hour
	catalogFetchTimeout  = 30 * time.Second
)

type catalogSource struct {
	URL string `json:"url"`
	// Key verifies the index (an authorized_keys line); empty: Basalt's.
	Key string `json:"key,omitempty"`
	// AllowPrivate lets the URL point to a private address (a catalog on
	// the local network); plain http is then allowed too.
	AllowPrivate bool `json:"allow_private,omitempty"`
}

func (c *catalogSource) key() string {
	if c.Key == "" {
		return catalog.BasaltKey
	}
	return c.Key
}

// catalogCache is the last index that passed, and how the last attempt went.
type catalogCache struct {
	URL       string    `json:"url"`
	Index     []byte    `json:"index,omitempty"`
	Signature []byte    `json:"signature,omitempty"`
	Generated time.Time `json:"generated"`
	FetchedAt time.Time `json:"fetched_at"`
	Error     string    `json:"error,omitempty"`
	ErrorAt   time.Time `json:"error_at"`
}

// remoteCatalog is the parsed downloaded catalog in memory.
type remoteCatalog struct {
	source    string // host, shown on its templates
	templates []*catalog.Template
	skipped   []string
}

func (s *Server) loadSetting(ctx context.Context, key string, v any) error {
	raw, err := s.db.Setting(ctx, key)
	if errors.Is(err, store.ErrNotFound) || (err == nil && raw == "") {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), v)
}

func (s *Server) saveSetting(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.SetSetting(ctx, key, string(b))
}

// loadRemoteCatalog reads the cached index at start.
func (s *Server) loadRemoteCatalog(ctx context.Context) {
	var src catalogSource
	var cache catalogCache
	if err := s.loadSetting(ctx, settingCatalogSource, &src); err != nil || src.URL == "" {
		return
	}
	if err := s.loadSetting(ctx, settingCatalogCache, &cache); err != nil || cache.URL != src.URL || len(cache.Index) == 0 {
		return
	}
	r, err := catalog.ParseIndex(ctx, cache.Index, cache.Signature, src.key())
	if err != nil {
		s.log.Warn("cached catalog", "err", err)
		return
	}
	s.setRemoteCatalog(src.URL, r)
}

func (s *Server) setRemoteCatalog(rawURL string, r *catalog.Remote) {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	if r == nil {
		s.remote = nil
		return
	}
	u, _ := url.Parse(rawURL)
	s.remote = &remoteCatalog{source: u.Host, templates: r.Templates, skipped: r.Skipped}
}

// validCatalogURL checks a catalog URL: https (http only for a private
// catalog), no credentials.
func validCatalogURL(raw string, allowPrivate bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || len(raw) > 500 {
		return nil, errors.New("the catalog URL must be like https://example.com/catalog.json")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && allowPrivate) {
		return nil, errors.New("the catalog URL must use https")
	}
	return u, nil
}

// fetchClient downloads from a catalog; unless allowPrivate, it connects
// only to public addresses (whatever the name resolves to).
func fetchClient(allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		d.Control = func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !dns.PublicAddr(ap.Addr()) {
				return fmt.Errorf("%s is not a public address (allow private addresses for a catalog on your network)", address)
			}
			return nil
		}
	}
	return &http.Client{
		Timeout:   catalogFetchTimeout,
		Transport: &http.Transport{DialContext: d.DialContext, Proxy: nil, TLSHandshakeTimeout: 10 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if _, err := validCatalogURL(req.URL.String(), allowPrivate); err != nil {
				return err
			}
			return nil
		},
	}
}

func fetchFile(ctx context.Context, hc *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, catalog.MaxIndexSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > catalog.MaxIndexSize {
		return nil, errors.New("the catalog index is too large")
	}
	return b, nil
}

// downloadCatalog fetches and verifies the index of src (src.URL and
// src.URL + ".sig"); an index older than the cached one is refused.
func (s *Server) downloadCatalog(ctx context.Context, src catalogSource, cached *catalogCache) (*catalog.Remote, []byte, []byte, error) {
	if _, err := validCatalogURL(src.URL, src.AllowPrivate); err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*catalogFetchTimeout)
	defer cancel()
	hc := fetchClient(src.AllowPrivate)
	data, err := fetchFile(ctx, hc, src.URL)
	if err != nil {
		return nil, nil, nil, err
	}
	sig, err := fetchFile(ctx, hc, src.URL+".sig")
	if err != nil {
		return nil, nil, nil, err
	}
	r, err := catalog.ParseIndex(ctx, data, sig, src.key())
	if err != nil {
		return nil, nil, nil, err
	}
	if cached != nil && cached.URL == src.URL && r.Generated.Before(cached.Generated) {
		return nil, nil, nil, fmt.Errorf("the catalog index is older (%s) than the one the panel has (%s)",
			r.Generated.Format(time.RFC3339), cached.Generated.Format(time.RFC3339))
	}
	return r, data, sig, nil
}

// refreshCatalog downloads the configured catalog and records the outcome.
func (s *Server) refreshCatalog(ctx context.Context) error {
	s.catalogFetchMu.Lock()
	defer s.catalogFetchMu.Unlock()
	var src catalogSource
	var cache catalogCache
	if err := s.loadSetting(ctx, settingCatalogSource, &src); err != nil {
		return err
	}
	if src.URL == "" {
		return nil
	}
	if err := s.loadSetting(ctx, settingCatalogCache, &cache); err != nil {
		return err
	}
	r, data, sig, err := s.downloadCatalog(ctx, src, &cache)
	if err != nil {
		cache.Error, cache.ErrorAt = err.Error(), s.now()
		if cache.URL != src.URL {
			cache = catalogCache{URL: src.URL, Error: err.Error(), ErrorAt: s.now()}
		}
		if serr := s.saveSetting(context.WithoutCancel(ctx), settingCatalogCache, cache); serr != nil {
			s.log.Warn("save catalog state", "err", serr)
		}
		return err
	}
	cache = catalogCache{URL: src.URL, Index: data, Signature: sig, Generated: r.Generated, FetchedAt: s.now()}
	if err := s.saveSetting(context.WithoutCancel(ctx), settingCatalogCache, cache); err != nil {
		return err
	}
	s.setRemoteCatalog(src.URL, r)
	return nil
}

// catalogLoop refreshes the downloaded catalog regularly.
func (s *Server) catalogLoop(ctx context.Context) {
	s.loadRemoteCatalog(ctx)
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			t.Reset(catalogRefreshEvery)
		}
		if err := s.refreshCatalog(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("catalog refresh", "err", err)
		}
	}
}

type catalogSourceView struct {
	URL          string    `json:"url"`
	Key          string    `json:"key"`
	AllowPrivate bool      `json:"allow_private"`
	Generated    time.Time `json:"generated,omitzero"`
	FetchedAt    time.Time `json:"fetched_at,omitzero"`
	Error        string    `json:"error,omitempty"`
	ErrorAt      time.Time `json:"error_at,omitzero"`
	Templates    int       `json:"templates"`
	Skipped      []string  `json:"skipped"`
}

func (s *Server) viewCatalogSource(ctx context.Context) (*catalogSourceView, error) {
	var src catalogSource
	var cache catalogCache
	if err := s.loadSetting(ctx, settingCatalogSource, &src); err != nil {
		return nil, err
	}
	if src.URL == "" {
		return nil, nil
	}
	if err := s.loadSetting(ctx, settingCatalogCache, &cache); err != nil {
		return nil, err
	}
	v := &catalogSourceView{URL: src.URL, Key: src.Key, AllowPrivate: src.AllowPrivate, Skipped: []string{}}
	if cache.URL == src.URL {
		v.Generated, v.FetchedAt, v.Error, v.ErrorAt = cache.Generated, cache.FetchedAt, cache.Error, cache.ErrorAt
	}
	s.catalogMu.Lock()
	if s.remote != nil {
		v.Templates, v.Skipped = len(s.remote.templates), append(v.Skipped, s.remote.skipped...)
	}
	s.catalogMu.Unlock()
	return v, nil
}

type catalogSourceRequest struct {
	URL          string `json:"url"`
	Key          string `json:"key"`
	AllowPrivate bool   `json:"allow_private"`
}

// handleCatalogSource sets (or, with an empty URL, removes) the catalog to
// download templates from. It is saved only if it downloads and verifies.
func (s *Server) handleCatalogSource(w http.ResponseWriter, r *http.Request, p *principal) {
	var req catalogSourceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	src := catalogSource{URL: strings.TrimSpace(req.URL), Key: strings.TrimSpace(req.Key), AllowPrivate: req.AllowPrivate}
	ctx := r.Context()
	if src.URL == "" {
		if err := s.db.SetSetting(ctx, settingCatalogSource, ""); err != nil {
			s.internalError(w, r, "save catalog source", err)
			return
		}
		s.setRemoteCatalog("", nil)
		s.audit(r, &p.user.ID, "catalog.source_removed", nil)
		writeJSON(w, http.StatusOK, map[string]any{"source": nil})
		return
	}
	if src.Key != "" {
		if strings.ContainsAny(src.Key, "\r\n") || len(src.Key) > 2000 {
			writeError(w, http.StatusUnprocessableEntity, "the key is one line of an authorized_keys file")
			return
		}
		if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(src.Key)); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "the key is one line of an authorized_keys file, e.g. ssh-ed25519 AAAA…")
			return
		}
	}
	s.catalogFetchMu.Lock()
	rem, data, sig, err := s.downloadCatalog(ctx, src, nil)
	s.catalogFetchMu.Unlock()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "the catalog was not added: "+err.Error())
		return
	}
	if err := s.saveSetting(ctx, settingCatalogSource, src); err == nil {
		err = s.saveSetting(ctx, settingCatalogCache, catalogCache{URL: src.URL, Index: data, Signature: sig,
			Generated: rem.Generated, FetchedAt: s.now()})
		if err != nil {
			s.internalError(w, r, "save catalog", err)
			return
		}
	} else {
		s.internalError(w, r, "save catalog source", err)
		return
	}
	s.setRemoteCatalog(src.URL, rem)
	s.audit(r, &p.user.ID, "catalog.source_set", map[string]any{"url": src.URL, "own_key": src.Key != "",
		"allow_private": src.AllowPrivate, "templates": len(rem.Templates)})
	v, _ := s.viewCatalogSource(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"source": v})
}

func (s *Server) handleCatalogRefresh(w http.ResponseWriter, r *http.Request, p *principal) {
	if err := s.refreshCatalog(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, "the catalog was not updated: "+err.Error())
		return
	}
	v, err := s.viewCatalogSource(r.Context())
	if err != nil {
		s.internalError(w, r, "describe catalog source", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": v})
}
