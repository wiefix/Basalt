// Package proxy turns the desired set of routes for a host into Caddy
// configuration and runs the embedded Caddy server (`basalt proxy`).
//
// Configuration is always generated from typed values and serialised with
// encoding/json; user input is never spliced into config text.
package proxy

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type RouteKind string

const (
	KindStatic       RouteKind = "static"
	KindReverseProxy RouteKind = "reverse_proxy"
)

type TLSMode string

const (
	// TLSAuto uses public ACME certificates (Let's Encrypt, ZeroSSL fallback),
	// except for names that cannot get one (localhost, *.test, IPs), which get
	// certificates from the proxy's internal CA.
	TLSAuto TLSMode = "auto"
	// TLSInternal always uses the internal CA (private networks, testing).
	TLSInternal TLSMode = "internal"
)

// Route is one virtual host on a proxy.
type Route struct {
	ID      int64     `json:"id"`
	Kind    RouteKind `json:"kind"`
	Domains []string  `json:"domains"`
	Root    string    `json:"root,omitempty"` // static
	// SiteID marks a static route serving a managed site. The panel leaves
	// Root empty; the agent resolves it inside its sites directory.
	SiteID int64 `json:"site_id,omitempty"`
	// PHP sends *.php requests of a site route to the site's PHP-FPM pool.
	// PHPSocket is resolved by the agent; a value from the panel is ignored.
	PHP       bool   `json:"php,omitempty"`
	PHPSocket string `json:"php_socket,omitempty"`
	Upstream  string `json:"upstream,omitempty"` // reverse_proxy, host:port
	// AppID marks a reverse_proxy route serving a managed app. The agent
	// resolves Upstream to the app's current container; a value from the
	// panel is ignored, and an app without a running container gets 502.
	AppID int64   `json:"app_id,omitempty"`
	TLS   TLSMode `json:"tls"`
	HSTS  bool    `json:"hsts,omitempty"`
}

// Desired is the complete proxy state the panel wants on one host.
type Desired struct {
	// Revision increases with every change on the panel. Agents ignore a
	// state older than the one they run, so out-of-order delivery is harmless.
	Revision  int64   `json:"revision"`
	ACMEEmail string  `json:"acme_email,omitempty"`
	Routes    []Route `json:"routes"`
}

const maxDomainsPerRoute = 100

// NormalizeDomain lowercases and validates a hostname. Internationalised
// names must be given in punycode (xn--) form; wildcards are not supported
// until DNS-01 issuance is available.
func NormalizeDomain(d string) (string, error) {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	if d == "" {
		return "", errors.New("domain is empty")
	}
	if len(d) > 253 {
		return "", fmt.Errorf("domain %q is too long", d)
	}
	if strings.HasPrefix(d, "*.") {
		return "", fmt.Errorf("wildcard domain %q is not supported yet", d)
	}
	if _, err := netip.ParseAddr(d); err == nil {
		return d, nil
	}
	labels := strings.Split(d, ".")
	for _, l := range labels {
		if len(l) == 0 || len(l) > 63 {
			return "", fmt.Errorf("domain %q has an invalid label", d)
		}
		for i, c := range l {
			ok := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' && i > 0 && i < len(l)-1
			if !ok {
				return "", fmt.Errorf("domain %q contains invalid characters (use punycode for IDN)", d)
			}
		}
	}
	if len(labels) < 2 && d != "localhost" {
		return "", fmt.Errorf("domain %q must contain a dot", d)
	}
	return d, nil
}

// publicCertEligible reports whether a public CA can issue for d.
func publicCertEligible(d string) bool {
	if _, err := netip.ParseAddr(d); err == nil {
		return false
	}
	if d == "localhost" {
		return false
	}
	for _, suffix := range []string{".localhost", ".test", ".local", ".internal", ".invalid", ".example", ".home.arpa", ".lan"} {
		if strings.HasSuffix(d, suffix) {
			return false
		}
	}
	return true
}

// ValidateUpstream checks a host:port dial address.
func ValidateUpstream(u string) error {
	host, port, err := net.SplitHostPort(u)
	if err != nil {
		return fmt.Errorf("upstream must be host:port: %w", err)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("upstream port %q is invalid", port)
	}
	if host == "" {
		return errors.New("upstream host is empty")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if _, err := NormalizeDomain(host); err != nil && host != "localhost" {
		return fmt.Errorf("upstream host: %w", err)
	}
	return nil
}

// Validate checks a route. sitesRoot, when non-empty, confines static roots.
func (r *Route) Validate(sitesRoot string) error {
	if len(r.Domains) == 0 {
		return errors.New("at least one domain is required")
	}
	if len(r.Domains) > maxDomainsPerRoute {
		return fmt.Errorf("at most %d domains per route", maxDomainsPerRoute)
	}
	for i, d := range r.Domains {
		n, err := NormalizeDomain(d)
		if err != nil {
			return err
		}
		r.Domains[i] = n
	}
	slices.Sort(r.Domains)
	if len(slices.Compact(slices.Clone(r.Domains))) != len(r.Domains) {
		return errors.New("duplicate domain in route")
	}
	switch r.TLS {
	case TLSAuto, TLSInternal:
	case "":
		r.TLS = TLSAuto
	default:
		return fmt.Errorf("tls must be %q or %q", TLSAuto, TLSInternal)
	}
	switch r.Kind {
	case KindStatic:
		if r.Upstream != "" {
			return errors.New("static route cannot have an upstream")
		}
		if r.SiteID < 0 {
			return errors.New("invalid site id")
		}
		if r.AppID != 0 {
			return errors.New("static route cannot serve an app")
		}
		if r.PHP && r.SiteID == 0 {
			return errors.New("PHP is only available for sites")
		}
		if r.PHPSocket != "" && (!filepath.IsAbs(r.PHPSocket) || filepath.Clean(r.PHPSocket) != r.PHPSocket) {
			return errors.New("invalid PHP socket path")
		}
		if r.SiteID > 0 && r.Root == "" {
			return nil // resolved by the agent
		}
		return validateRoot(r.Root, sitesRoot)
	case KindReverseProxy:
		if r.Root != "" || r.SiteID != 0 || r.PHP {
			return errors.New("reverse_proxy route cannot have a root, site or PHP")
		}
		if r.AppID < 0 {
			return errors.New("invalid app id")
		}
		if r.AppID > 0 && r.Upstream == "" {
			return nil // resolved by the agent
		}
		return ValidateUpstream(r.Upstream)
	default:
		return fmt.Errorf("kind must be %q or %q", KindStatic, KindReverseProxy)
	}
}

func validateRoot(root, sitesRoot string) error {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("root must be a clean absolute path, got %q", root)
	}
	if sitesRoot != "" && !strings.HasPrefix(root, filepath.Clean(sitesRoot)+string(filepath.Separator)) {
		return fmt.Errorf("root %q is outside the sites directory %q", root, sitesRoot)
	}
	return nil
}

// Validate checks all routes and that no domain is served twice.
func (d *Desired) Validate(sitesRoot string) error {
	seen := map[string]int64{}
	for i := range d.Routes {
		r := &d.Routes[i]
		if err := r.Validate(sitesRoot); err != nil {
			return fmt.Errorf("route %d: %w", r.ID, err)
		}
		for _, dom := range r.Domains {
			if other, dup := seen[dom]; dup {
				return fmt.Errorf("domain %s is used by routes %d and %d", dom, other, r.ID)
			}
			seen[dom] = r.ID
		}
	}
	return nil
}
