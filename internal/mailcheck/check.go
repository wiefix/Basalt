// Package mailcheck checks what decides whether a mail server's mail is
// delivered: that the host can send on port 25, that its addresses have
// matching reverse DNS and are on no blocklist, and that each domain's
// MX, SPF, DKIM and DMARC records, as recursive resolvers see them, point
// at this server and allow it to send.
//
// The checks run on the mail server's host (its network and resolvers are
// what matter) and only read: DNS queries and an SMTP greeting.
package mailcheck

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/dns"
)

// Statuses of a check.
const (
	OK      = "ok"
	Warn    = "warn"
	Fail    = "fail"
	Unknown = "unknown"
)

// Checks, in the order they are reported.
const (
	CheckOutbound  = "outbound_smtp"
	CheckHostname  = "hostname"
	CheckPTR       = "ptr"
	CheckBlocklist = "blocklist"
	CheckMX        = "mx"
	CheckSPF       = "spf"
	CheckDKIM      = "dkim"
	CheckDMARC     = "dmarc"
)

// DefaultBlocklists are widely used DNS blocklists of sending addresses.
var DefaultBlocklists = []string{"zen.spamhaus.org", "bl.spamcop.net", "b.barracudacentral.org", "psbl.surriel.com"}

// ipv6Blocklists answer for IPv6 addresses too.
var ipv6Blocklists = []string{"zen.spamhaus.org"}

// probeDomains are large mail providers whose servers the outgoing port 25
// check connects to.
var probeDomains = []string{"gmail.com", "outlook.com"}

// Checker runs the checks. The zero value uses the host's resolvers, the
// default blocklists and the mail servers of probeDomains.
type Checker struct {
	// Resolvers are recursive DNS servers (host:port).
	Resolvers []string
	// SMTPProbes are SMTP servers (host:port) to reach on port 25.
	SMTPProbes []string
	// Blocklists are DNS blocklist zones.
	Blocklists []string
	// Timeout bounds the whole run (default 45 s).
	Timeout time.Duration
}

type result = agentproto.MailCheckResult

// Run checks the mail server's host and domains.
func (c *Checker) Run(ctx context.Context, req agentproto.MailCheck) []result {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	servers := c.Resolvers
	if len(servers) == 0 {
		var err error
		if servers, err = systemResolvers(); err != nil {
			return []result{{Check: CheckHostname, Status: Unknown, Detail: err.Error()}}
		}
	}
	r := &resolver{servers: servers, timeout: 5 * time.Second}
	host := strings.ToLower(req.Hostname)
	var ips []netip.Addr
	for _, s := range req.IPs {
		if a, err := netip.ParseAddr(s); err == nil && !slices.Contains(ips, a.Unmap()) {
			ips = append(ips, a.Unmap())
		}
	}

	var jobs []func() result
	jobs = append(jobs,
		func() result { return c.outbound(ctx, r) },
		func() result { return hostname(ctx, r, host, ips) })
	for _, ip := range ips {
		jobs = append(jobs, func() result { return ptr(ctx, r, host, ip) })
	}
	lists := c.Blocklists
	if len(lists) == 0 {
		lists = DefaultBlocklists
	}
	for _, ip := range ips {
		jobs = append(jobs, func() result { return blocklists(ctx, r, ip, lists) })
	}
	for _, d := range req.Domains {
		name := strings.ToLower(d.Name)
		withDomain := func(f func() result) func() result {
			return func() result {
				res := f()
				res.Domain = name
				return res
			}
		}
		jobs = append(jobs,
			withDomain(func() result { return checkMX(ctx, r, name, host) }),
			withDomain(func() result { return checkSPF(ctx, r, name, ips, d.Records) }),
			withDomain(func() result { return checkDKIM(ctx, r, d.Records) }),
			withDomain(func() result { return checkDMARC(ctx, r, name, d.Records) }))
	}

	out := make([]result, len(jobs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = job()
		})
	}
	wg.Wait()
	return out
}

// outbound connects to other mail servers on port 25 and reads their
// greeting: many providers block outgoing port 25 on new servers.
func (c *Checker) outbound(ctx context.Context, r *resolver) result {
	res := result{Check: CheckOutbound, Target: "25"}
	targets := c.SMTPProbes
	if len(targets) == 0 {
		for _, d := range probeDomains {
			if mx, err := r.mx(ctx, d); err == nil && len(mx) > 0 {
				targets = append(targets, net.JoinHostPort(hostOf(mx[0].Mx), "25"))
			}
		}
	}
	if len(targets) == 0 {
		res.Status, res.Detail = Unknown, "Could not look up other mail servers to connect to."
		return res
	}
	type probe struct {
		target string
		err    error
	}
	done := make(chan probe, len(targets))
	for _, t := range targets {
		go func() { done <- probe{t, greet(ctx, t)} }()
	}
	var failed []probe
	for range targets {
		p := <-done
		if p.err == nil {
			res.Status, res.Detail = OK, "Connected to "+p.target+" and got its greeting."
			return res
		}
		failed = append(failed, p)
	}
	res.Status = Fail
	res.Detail = fmt.Sprintf("Cannot reach other mail servers on port 25 (%s: %v). Many providers block outgoing port 25 on new servers: "+
		"ask yours to open it, or send through a relay.", failed[0].target, failed[0].err)
	return res
}

// greet connects to an SMTP server and reads its 220 greeting.
func greet(ctx context.Context, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("no answer within 10 s")
		}
		return errors.New(strings.TrimPrefix(err.Error(), "dial tcp "+target+": "))
	}
	defer conn.Close()
	dl, _ := ctx.Deadline()
	conn.SetDeadline(dl)
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return errors.New("connected, but no greeting")
	}
	if !strings.HasPrefix(line, "220") {
		return fmt.Errorf("refused: %.80s", strings.TrimSpace(line))
	}
	fmt.Fprint(conn, "QUIT\r\n")
	return nil
}

func joinAddrs(ips []net.IP) string {
	s := make([]string, len(ips))
	for i, ip := range ips {
		s[i] = ip.String()
	}
	return strings.Join(s, ", ")
}

func containsIP(list []net.IP, a netip.Addr) bool {
	return slices.ContainsFunc(list, func(ip net.IP) bool {
		b, ok := netip.AddrFromSlice(ip)
		return ok && b.Unmap() == a
	})
}

// hostname checks that the mail host name points at this server.
func hostname(ctx context.Context, r *resolver, host string, ips []netip.Addr) result {
	res := result{Check: CheckHostname, Target: host}
	addrs, err := r.addrs(ctx, host)
	switch {
	case err != nil && !errors.Is(err, errNXDomain):
		res.Status, res.Detail = Unknown, err.Error()
	case len(addrs) == 0:
		res.Status, res.Detail = Fail, host+" has no A or AAAA record: other servers and mail clients cannot find it."
	case len(ips) == 0:
		res.Status, res.Detail = Warn, host+" resolves to "+joinAddrs(addrs)+"; this server's public address is unknown (set it on the server's page)."
	case slices.ContainsFunc(ips, func(a netip.Addr) bool { return containsIP(addrs, a) }):
		res.Status, res.Detail = OK, host+" resolves to "+joinAddrs(addrs)+"."
	default:
		res.Status, res.Detail = Fail, host+" resolves to "+joinAddrs(addrs)+", not to this server ("+addrList(ips)+")."
	}
	return res
}

func addrList(ips []netip.Addr) string {
	s := make([]string, len(ips))
	for i, a := range ips {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// ptr checks the reverse DNS of an address: receivers reject or mark as
// spam mail from addresses without a PTR that resolves back to them.
func ptr(ctx context.Context, r *resolver, host string, ip netip.Addr) result {
	res := result{Check: CheckPTR, Target: ip.String()}
	if !dns.PublicAddr(ip) {
		res.Status, res.Detail = Unknown, ip.String()+" is not a public address."
		return res
	}
	rev, _ := mdns.ReverseAddr(ip.String())
	rrs, err := r.lookup(ctx, rev, mdns.TypePTR)
	if err != nil && !errors.Is(err, errNXDomain) {
		res.Status, res.Detail = Unknown, err.Error()
		return res
	}
	var names []string
	for _, rr := range rrs {
		names = append(names, hostOf(rr.(*mdns.PTR).Ptr))
	}
	if len(names) == 0 {
		res.Status, res.Detail = Fail, "No reverse DNS (PTR) for "+ip.String()+". Ask the server's provider to set it to "+host+
			" (often in its control panel)."
		return res
	}
	resolvesBack := func(name string) bool {
		addrs, _ := r.addrs(ctx, name)
		return containsIP(addrs, ip)
	}
	switch {
	case slices.Contains(names, host) && resolvesBack(host):
		res.Status, res.Detail = OK, ip.String()+" → "+host+" → "+ip.String()+"."
	case slices.Contains(names, host):
		res.Status, res.Detail = Fail, "The PTR of "+ip.String()+" is "+host+", but "+host+" does not resolve to "+ip.String()+"."
	case resolvesBack(names[0]):
		res.Status, res.Detail = Warn, "The PTR of "+ip.String()+" is "+names[0]+". Receivers accept it, but setting it to "+host+
			" (the name the server greets with) is safer."
	default:
		res.Status, res.Detail = Fail, "The PTR of "+ip.String()+" is "+names[0]+", which does not resolve back to it. Ask the provider to set it to "+host+"."
	}
	return res
}

// blocklists looks an address up on DNS blocklists.
func blocklists(ctx context.Context, r *resolver, ip netip.Addr, lists []string) result {
	res := result{Check: CheckBlocklist, Target: ip.String()}
	if !dns.PublicAddr(ip) {
		res.Status, res.Detail = Unknown, ip.String()+" is not a public address."
		return res
	}
	rev, _ := mdns.ReverseAddr(ip.String())
	rev = strings.TrimSuffix(strings.TrimSuffix(rev, "in-addr.arpa."), "ip6.arpa.")
	var clean, listed, unknown []string
	for _, l := range lists {
		if ip.Is6() && !slices.Contains(ipv6Blocklists, l) {
			continue
		}
		rrs, err := r.lookup(ctx, rev+l, mdns.TypeA)
		if errors.Is(err, errNXDomain) || (err == nil && len(rrs) == 0) {
			clean = append(clean, l)
			continue
		}
		if err != nil {
			unknown = append(unknown, l+" (no answer)")
			continue
		}
		code := rrs[0].(*mdns.A).A
		if code[0] == 127 && code[1] == 255 && code[2] == 255 {
			// Spamhaus and others refuse queries from public resolvers
			// and over-quota users this way.
			unknown = append(unknown, l+" (refused the query: the list does not answer through public or busy resolvers)")
			continue
		}
		listed = append(listed, l+" ("+code.String()+")")
	}
	switch {
	case len(listed) > 0:
		res.Status = Fail
		res.Detail = "Listed on " + strings.Join(listed, ", ") + ". Receivers using these lists reject the mail: look the address up on each list's site to see why and how to be removed."
	case len(unknown) > 0:
		res.Status = Unknown
		res.Detail = "Could not check " + strings.Join(unknown, ", ") + "."
		if len(clean) > 0 {
			res.Detail += " Not listed on " + strings.Join(clean, ", ") + "."
		}
	default:
		res.Status, res.Detail = OK, "Not listed on "+strings.Join(clean, ", ")+"."
	}
	return res
}

// checkMX checks that the domain's mail is delivered to this server.
func checkMX(ctx context.Context, r *resolver, domain, host string) result {
	res := result{Check: CheckMX, Target: domain}
	mx, err := r.mx(ctx, domain)
	switch {
	case errors.Is(err, errNXDomain):
		res.Status, res.Detail = Fail, domain+" does not exist in DNS: register it, or point it at nameservers that serve it."
		return res
	case err != nil:
		res.Status, res.Detail = Unknown, err.Error()
		return res
	case len(mx) == 0:
		res.Status, res.Detail = Fail, "No MX record: other servers do not know where to deliver mail for "+domain+". Add MX 10 "+host+"."
		return res
	}
	i := slices.IndexFunc(mx, func(m *mdns.MX) bool { return hostOf(m.Mx) == host })
	switch {
	case i < 0:
		res.Status, res.Detail = Fail, "Mail for "+domain+" goes to "+hostOf(mx[0].Mx)+", not to this server ("+host+")."
	case mx[0].Preference < mx[i].Preference:
		res.Status, res.Detail = Warn, hostOf(mx[0].Mx)+" has priority over "+host+": mail is delivered there first."
	default:
		res.Status, res.Detail = OK, "Mail for "+domain+" is delivered to "+host+"."
	}
	return res
}

// wanted returns the TXT record the mail server asks for at name with the
// given prefix ("" if none).
func wanted(recs []dns.Record, name, prefix string) string {
	for _, r := range recs {
		if r.Type == dns.TypeTXT && r.Name == name && strings.HasPrefix(strings.ToLower(r.Value), strings.ToLower(prefix)) {
			return r.Value
		}
	}
	return ""
}

// checkSPF evaluates the domain's SPF policy for each server address.
func checkSPF(ctx context.Context, r *resolver, domain string, ips []netip.Addr, recs []dns.Record) result {
	res := result{Check: CheckSPF, Target: domain}
	rec, st, err := spfRecord(ctx, r, domain)
	if rec == "" {
		switch st {
		case spfNone:
			res.Status, res.Detail = Fail, "No SPF record: receivers cannot tell that this server may send for "+domain+"."
			if w := wanted(recs, domain, "v=spf1"); w != "" {
				res.Detail += ` Add TXT "` + w + `".`
			}
		case spfTempError:
			res.Status, res.Detail = Unknown, err.Error()
		default:
			res.Status, res.Detail = Fail, err.Error()
		}
		return res
	}
	if len(ips) == 0 {
		res.Status, res.Detail = Unknown, `SPF: "`+rec+`". This server's public address is unknown, so it cannot be checked.`
		return res
	}
	for _, ip := range ips {
		e := &spfEval{r: r, ip: net.IP(ip.AsSlice())}
		st, err := e.check(ctx, domain)
		why := ""
		if err != nil {
			why = " " + err.Error() + "."
		}
		switch st {
		case spfPass:
			continue
		case spfTempError:
			res.Status, res.Detail = Unknown, "Could not evaluate the SPF policy."+why
		case spfSoftFail, spfNeutral:
			res.Status, res.Detail = Warn, `The SPF policy "`+rec+`" does not allow `+ip.String()+` (`+st+`): receivers may treat the mail as spam.`
		case spfFail:
			mech := "ip6:"
			if ip.Is4() {
				mech = "ip4:"
			}
			res.Status, res.Detail = Fail, `The SPF policy "`+rec+`" forbids `+ip.String()+`: receivers reject the mail unless DKIM passes. Add mx or `+
				mech+ip.String()+" to it."
		default:
			res.Status, res.Detail = Fail, "The SPF policy is invalid."+why
		}
		return res
	}
	res.Status, res.Detail = OK, `"`+rec+`" allows this server.`
	return res
}

// dkimKey returns the public key (p=) of a DKIM record, without spaces.
func dkimKey(v string) string {
	for _, tag := range strings.Split(v, ";") {
		k, val, ok := strings.Cut(strings.TrimSpace(tag), "=")
		if ok && strings.TrimSpace(k) == "p" {
			return strings.Join(strings.Fields(val), "")
		}
	}
	return ""
}

// checkDKIM checks that the keys the mail server signs with are published.
func checkDKIM(ctx context.Context, r *resolver, recs []dns.Record) result {
	res := result{Check: CheckDKIM}
	var ok, bad []string
	for _, w := range recs {
		i := strings.Index(w.Name, "._domainkey.")
		if w.Type != dns.TypeTXT || i <= 0 {
			continue
		}
		sel := w.Name[:i]
		txts, err := r.txt(ctx, w.Name)
		if err != nil && !errors.Is(err, errNXDomain) {
			res.Status, res.Detail = Unknown, err.Error()
			return res
		}
		want := dkimKey(w.Value)
		switch {
		case slices.ContainsFunc(txts, func(t string) bool { return dkimKey(t) == want }):
			ok = append(ok, sel)
		case len(txts) == 0:
			bad = append(bad, sel+": not published")
		default:
			bad = append(bad, sel+": a different key is published")
		}
	}
	switch {
	case len(ok) == 0 && len(bad) == 0:
		res.Status, res.Detail = Unknown, "The mail server has not created DKIM keys yet."
	case len(bad) > 0:
		res.Status, res.Detail = Fail, "DKIM keys "+strings.Join(bad, "; ")+". Receivers cannot verify the signatures: publish the records below."
	default:
		res.Status, res.Detail = OK, "Keys published: "+strings.Join(ok, ", ")+"."
	}
	return res
}

func isDMARC(t string) bool {
	t = strings.TrimSpace(t)
	if len(t) < 8 || !strings.EqualFold(t[:8], "v=DMARC1") {
		return false
	}
	rest := strings.TrimSpace(t[8:])
	return rest == "" || rest[0] == ';'
}

// checkDMARC checks the domain's DMARC policy (or its parent's).
func checkDMARC(ctx context.Context, r *resolver, domain string, recs []dns.Record) result {
	res := result{Check: CheckDMARC, Target: domain}
	find := func(d string) ([]string, error) {
		txts, err := r.txt(ctx, "_dmarc."+d)
		if errors.Is(err, errNXDomain) {
			err = nil
		}
		return slices.DeleteFunc(txts, func(t string) bool { return !isDMARC(t) }), err
	}
	found, err := find(domain)
	from := domain
	if err == nil && len(found) == 0 && strings.Count(domain, ".") >= 2 {
		// Policies of the organizational domain cover subdomains; without
		// the public suffix list, try the parent.
		from = domain[strings.Index(domain, ".")+1:]
		found, err = find(from)
	}
	switch {
	case err != nil:
		res.Status, res.Detail = Unknown, err.Error()
		return res
	case len(found) == 0:
		res.Status, res.Detail = Fail, "No DMARC record: large providers (Gmail, Yahoo, Microsoft) expect one."
		if w := wanted(recs, "_dmarc."+domain, "v=DMARC1"); w != "" {
			res.Detail += ` Add TXT "` + w + `" at _dmarc.` + domain + "."
		}
		return res
	case len(found) > 1:
		res.Status, res.Detail = Fail, "_dmarc."+from+" has several DMARC records; receivers ignore them all."
		return res
	}
	policy := ""
	for _, tag := range strings.Split(found[0], ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(tag), "="); ok && strings.TrimSpace(k) == "p" {
			policy = strings.ToLower(strings.TrimSpace(v))
		}
	}
	note := ""
	if from != domain {
		note = " (from " + from + ")"
	}
	switch policy {
	case "reject", "quarantine":
		res.Status, res.Detail = OK, "Policy "+policy+note+": receivers "+policy+" mail that fails SPF and DKIM."
	case "none":
		res.Status, res.Detail = Warn, "Policy none"+note+": receivers only report forged mail. Once reports look right, use p=quarantine or p=reject."
	default:
		res.Status, res.Detail = Fail, `The DMARC record "`+found[0]+`" has no valid policy (p=none, quarantine or reject).`
	}
	return res
}
