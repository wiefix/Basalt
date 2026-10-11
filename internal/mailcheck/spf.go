package mailcheck

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"

	mdns "github.com/miekg/dns"
)

// SPF results (RFC 7208 section 2.6).
const (
	spfPass      = "pass"
	spfFail      = "fail"
	spfSoftFail  = "softfail"
	spfNeutral   = "neutral"
	spfNone      = "none"
	spfPermError = "permerror"
	spfTempError = "temperror"
)

// spfLookupLimit is the number of DNS-querying terms an evaluation may use.
const spfLookupLimit = 10

// errMacros marks a policy that uses macros, which this evaluator does not
// expand (they depend on the sender and are rare in published policies).
var errMacros = errors.New("the SPF policy uses macros, which Basalt does not evaluate")

// spfEval evaluates SPF policies for one address (check_host without
// macros; the sender is the domain itself).
type spfEval struct {
	r       *resolver
	ip      net.IP
	lookups int
	voids   int
}

// spfRecord finds the one SPF policy of domain.
func spfRecord(ctx context.Context, r *resolver, domain string) (string, string, error) {
	txts, err := r.txt(ctx, domain)
	if err != nil && !errors.Is(err, errNXDomain) {
		return "", spfTempError, err
	}
	var recs []string
	for _, t := range txts {
		if isSPF(t) {
			recs = append(recs, t)
		}
	}
	switch len(recs) {
	case 0:
		return "", spfNone, nil
	case 1:
		return recs[0], "", nil
	default:
		return "", spfPermError, errors.New(domain + " has several SPF records; receivers reject them all")
	}
}

func isSPF(t string) bool {
	return len(t) >= 6 && strings.EqualFold(t[:6], "v=spf1") && (len(t) == 6 || t[6] == ' ')
}

// check returns the SPF result for domain and why it is not a pass.
func (e *spfEval) check(ctx context.Context, domain string) (string, error) {
	rec, res, err := spfRecord(ctx, e.r, domain)
	if rec == "" {
		return res, err
	}
	var redirect string
	for _, term := range strings.Fields(rec)[1:] {
		if strings.Contains(term, "%") {
			return spfPermError, errMacros
		}
		if name, val, ok := strings.Cut(term, "="); ok && !strings.ContainsAny(name, ":/") {
			if strings.EqualFold(name, "redirect") {
				redirect = val
			}
			continue // exp and unknown modifiers
		}
		result := spfPass
		switch term[0] {
		case '+':
			term = term[1:]
		case '-':
			result, term = spfFail, term[1:]
		case '~':
			result, term = spfSoftFail, term[1:]
		case '?':
			result, term = spfNeutral, term[1:]
		}
		match, err := e.mechanism(ctx, domain, term)
		if err != nil {
			return spfResultOf(err), err
		}
		if match {
			return result, nil
		}
	}
	if redirect != "" {
		if err := e.count(); err != nil {
			return spfPermError, err
		}
		res, err := e.check(ctx, redirect)
		if res == spfNone {
			return spfPermError, errors.New("the SPF redirect to " + redirect + " finds no policy")
		}
		return res, err
	}
	return spfNeutral, nil
}

type spfError struct {
	result string
	msg    string
}

func (e *spfError) Error() string { return e.msg }

func spfResultOf(err error) string {
	var se *spfError
	if errors.As(err, &se) {
		return se.result
	}
	return spfPermError
}

func (e *spfEval) count() error {
	e.lookups++
	if e.lookups > spfLookupLimit {
		return &spfError{spfPermError, "the SPF policy needs more than 10 DNS lookups; receivers treat it as an error"}
	}
	return nil
}

// void counts lookups that found nothing (at most 2 are allowed).
func (e *spfEval) void(n int, err error) error {
	if err != nil && !errors.Is(err, errNXDomain) {
		return &spfError{spfTempError, err.Error()}
	}
	if n == 0 {
		e.voids++
		if e.voids > 2 {
			return &spfError{spfPermError, "the SPF policy has more than 2 lookups that find nothing"}
		}
	}
	return nil
}

// mechanism reports whether one mechanism matches the address.
func (e *spfEval) mechanism(ctx context.Context, domain, term string) (bool, error) {
	name, arg := term, ""
	if i := strings.IndexAny(term, ":/"); i >= 0 {
		name, arg = term[:i], term[i:]
	}
	switch strings.ToLower(name) {
	case "all":
		return arg == "", nil
	case "ip4", "ip6":
		_, n, err := parseNet(strings.TrimPrefix(arg, ":"), name == "ip4")
		if err != nil {
			return false, &spfError{spfPermError, "invalid SPF term " + term}
		}
		return n.Contains(e.ip), nil
	case "include":
		if !strings.HasPrefix(arg, ":") || len(arg) < 2 {
			return false, &spfError{spfPermError, "invalid SPF term " + term}
		}
		if err := e.count(); err != nil {
			return false, err
		}
		res, err := e.check(ctx, arg[1:])
		switch res {
		case spfPass:
			return true, nil
		case spfFail, spfSoftFail, spfNeutral:
			return false, nil
		case spfTempError:
			return false, &spfError{spfTempError, errText(err, "include:"+arg[1:]+" could not be resolved")}
		default:
			return false, &spfError{spfPermError, errText(err, "include:"+arg[1:]+" has no valid SPF policy")}
		}
	case "a", "mx":
		target, v4, v6, ok := parseDomainCIDR(arg, domain)
		if !ok {
			return false, &spfError{spfPermError, "invalid SPF term " + term}
		}
		if err := e.count(); err != nil {
			return false, err
		}
		hosts := []string{target}
		if strings.EqualFold(name, "mx") {
			mx, err := e.r.mx(ctx, target)
			if err := e.void(len(mx), err); err != nil {
				return false, err
			}
			hosts = hosts[:0]
			for i, m := range mx {
				if i == 10 {
					return false, &spfError{spfPermError, target + " has more than 10 MX records"}
				}
				hosts = append(hosts, hostOf(m.Mx))
			}
		}
		for _, h := range hosts {
			ips, err := e.r.addrs(ctx, h)
			if err := e.void(len(ips), err); err != nil {
				return false, err
			}
			for _, ip := range ips {
				bits, size := v6, 128
				if ip4 := ip.To4(); ip4 != nil {
					ip, bits, size = ip4, v4, 32
				}
				mask := net.CIDRMask(bits, size)
				if n := (net.IPNet{IP: ip.Mask(mask), Mask: mask}); n.Contains(e.ip) {
					return true, nil
				}
			}
		}
		return false, nil
	case "exists":
		if !strings.HasPrefix(arg, ":") || len(arg) < 2 {
			return false, &spfError{spfPermError, "invalid SPF term " + term}
		}
		if err := e.count(); err != nil {
			return false, err
		}
		rrs, err := e.r.lookup(ctx, arg[1:], mdns.TypeA)
		if err := e.void(len(rrs), err); err != nil {
			return false, err
		}
		return len(rrs) > 0, nil
	case "ptr":
		// Deprecated (RFC 7208 section 5.5): count it, never match.
		return false, e.count()
	}
	return false, &spfError{spfPermError, "unknown SPF term " + term}
}

func errText(err error, fallback string) string {
	if err != nil {
		return err.Error()
	}
	return fallback
}

// parseNet parses an address with an optional prefix length.
func parseNet(s string, v4 bool) (net.IP, *net.IPNet, error) {
	if !strings.Contains(s, "/") {
		if v4 {
			s += "/32"
		} else {
			s += "/128"
		}
	}
	ip, n, err := net.ParseCIDR(s)
	if err != nil || (ip.To4() != nil) != v4 {
		return nil, nil, errors.New("invalid network")
	}
	return ip, n, nil
}

// parseDomainCIDR parses the argument of a and mx: [:domain][/v4][//v6].
func parseDomainCIDR(arg, domain string) (string, int, int, bool) {
	v4, v6 := 32, 128
	if i := strings.Index(arg, "//"); i >= 0 {
		n, err := strconv.Atoi(arg[i+2:])
		if err != nil || n < 0 || n > 128 {
			return "", 0, 0, false
		}
		v6, arg = n, arg[:i]
	}
	if i := strings.Index(arg, "/"); i >= 0 {
		n, err := strconv.Atoi(arg[i+1:])
		if err != nil || n < 0 || n > 32 {
			return "", 0, 0, false
		}
		v4, arg = n, arg[:i]
	}
	if arg != "" {
		if !strings.HasPrefix(arg, ":") || len(arg) < 2 {
			return "", 0, 0, false
		}
		domain = arg[1:]
	}
	return domain, v4, v6, true
}
