package dns

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mdns "github.com/miekg/dns"
)

const (
	// maxUDPSize is the largest UDP answer: the DNS flag day 2020 value,
	// which avoids IP fragmentation.
	maxUDPSize = 1232
	// maxChain bounds CNAME/ALIAS chasing inside our zones.
	maxChain = 8
	// Per-prefix UDP response limit (amplification defense). Over the
	// limit, every second answer is a truncated empty one (the client
	// retries over TCP), the rest are dropped.
	udpRate  = 50 // answers per second
	udpBurst = 100
	// TCP connection limits.
	maxTCPConns      = 256
	maxTCPConnsPerIP = 16
	maxTCPQueries    = 128
	// ALIAS targets outside our zones are resolved and cached this long.
	aliasCacheTTL    = time.Minute
	aliasNegativeTTL = 15 * time.Second
	aliasCacheSize   = 10000
	aliasTimeout     = 2 * time.Second
)

// Resolver looks up ALIAS targets outside our zones; net.DefaultResolver
// satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Server is an authoritative-only DNS server: it answers for its zones,
// refuses everything else and never recurses.
type Server struct {
	log      *slog.Logger
	now      func() time.Time // signature times
	cat      atomic.Pointer[catalog]
	resolver Resolver
	limiter  *prefixLimiter
	slip     atomic.Uint32

	aliasMu    sync.Mutex
	aliasCache map[string]aliasEntry
}

type aliasEntry struct {
	addrs   []netip.Addr
	expires time.Time
}

// NewServer creates a server with no zones. resolver may be nil (ALIAS
// targets outside our zones then do not resolve).
func NewServer(log *slog.Logger, resolver Resolver) *Server {
	s := &Server{log: log, now: time.Now, resolver: resolver, limiter: newPrefixLimiter(udpRate, udpBurst, time.Now),
		aliasCache: map[string]aliasEntry{}}
	s.cat.Store(&catalog{zones: map[string]*zoneData{}})
	return s
}

// Check validates zones the way SetZones would, without serving them.
func Check(zones []Zone) error {
	_, err := compile(zones, time.Now)
	return err
}

// SetZones replaces every zone at once; on error nothing changes.
func (s *Server) SetZones(zones []Zone) error {
	c, err := compile(zones, s.now)
	if err != nil {
		return err
	}
	s.cat.Store(c)
	return nil
}

// Sockets are the UDP and TCP sockets a server answers on.
type Sockets struct {
	Packet []net.PacketConn
	Stream []net.Listener
}

// Port is the port the sockets are bound to.
func (k *Sockets) Port() int {
	if len(k.Packet) == 0 {
		return 0
	}
	return k.Packet[0].LocalAddr().(*net.UDPAddr).Port
}

// Addrs lists the bound UDP addresses.
func (k *Sockets) Addrs() []string {
	var out []string
	for _, pc := range k.Packet {
		out = append(out, pc.LocalAddr().String())
	}
	return out
}

func (k *Sockets) Close() {
	for _, pc := range k.Packet {
		pc.Close()
	}
	for _, ln := range k.Stream {
		ln.Close()
	}
}

// Listen opens UDP and TCP sockets on addr. For a wildcard address (":53")
// that is partly taken — systemd-resolved listens on 127.0.0.53:53 on
// Ubuntu — it binds every address of the host's interfaces instead, as
// BIND and NSD do; addresses added later are then not served.
func Listen(ctx context.Context, addr string) (*Sockets, error) {
	k, err := listenOne(ctx, addr)
	if err == nil {
		return k, nil
	}
	host, port, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); host != "" && (ip == nil || !ip.IsUnspecified()) {
		return nil, err
	}
	ifAddrs, ifErr := net.InterfaceAddrs()
	if ifErr != nil {
		return nil, err
	}
	all := &Sockets{}
	for _, a := range ifAddrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLinkLocalUnicast() || ipn.IP.IsMulticast() {
			continue
		}
		if (host == "0.0.0.0" && ipn.IP.To4() == nil) || (host == "::" && ipn.IP.To4() != nil) {
			continue
		}
		one, oneErr := listenOne(ctx, net.JoinHostPort(ipn.IP.String(), port))
		if oneErr != nil {
			continue // e.g. an address another server already uses
		}
		all.Packet = append(all.Packet, one.Packet...)
		all.Stream = append(all.Stream, one.Stream...)
	}
	if len(all.Packet) == 0 {
		return nil, err
	}
	return all, nil
}

func listenOne(ctx context.Context, addr string) (*Sockets, error) {
	lc := net.ListenConfig{Control: reuseAddr}
	pc, err := lc.ListenPacket(ctx, "udp", addr)
	if err != nil {
		return nil, err
	}
	// Bind TCP to the port UDP got, so ":0" works in tests.
	host, _, _ := net.SplitHostPort(addr)
	_, port, _ := net.SplitHostPort(pc.LocalAddr().String())
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		pc.Close()
		return nil, err
	}
	return &Sockets{Packet: []net.PacketConn{pc}, Stream: []net.Listener{ln}}, nil
}

// Serve answers queries on the sockets until ctx is cancelled, then
// closes them.
func (s *Server) Serve(ctx context.Context, k *Sockets) error {
	var servers []*mdns.Server
	for _, pc := range k.Packet {
		// Queries are small; 4 KiB covers EDNS options with room to spare.
		servers = append(servers, &mdns.Server{PacketConn: pc, Handler: s, UDPSize: 4096,
			ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second})
	}
	for _, ln := range k.Stream {
		servers = append(servers, &mdns.Server{Listener: newLimitListener(ln, maxTCPConns, maxTCPConnsPerIP), Handler: s,
			ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second, MaxTCPQueries: maxTCPQueries,
			IdleTimeout: func() time.Duration { return 8 * time.Second }})
	}
	errc := make(chan error, len(servers))
	for _, srv := range servers {
		go func() { errc <- srv.ActivateAndServe() }()
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	for _, srv := range servers {
		srv.ShutdownContext(sctx)
	}
	k.Close()
	return err
}

// ServeDNS implements mdns.Handler.
func (s *Server) ServeDNS(w mdns.ResponseWriter, req *mdns.Msg) {
	_, isUDP := w.RemoteAddr().(*net.UDPAddr)
	if isUDP && !s.limiter.allow(w.RemoteAddr()) {
		if s.slip.Add(1)%2 == 0 {
			return // dropped
		}
		m := new(mdns.Msg)
		m.SetReply(req)
		m.Truncated = true
		m.Question = nil
		if len(req.Question) == 1 {
			m.Question = req.Question
		}
		w.WriteMsg(m)
		return
	}
	resp := s.answer(req)
	size := 65535
	if isUDP {
		size = mdns.MinMsgSize
		if opt := req.IsEdns0(); opt != nil {
			size = min(max(int(opt.UDPSize()), mdns.MinMsgSize), maxUDPSize)
		}
	}
	resp.Truncate(size)
	if err := w.WriteMsg(resp); err != nil {
		s.log.Debug("write dns answer", "err", err)
	}
}

// answer builds the response to one query.
func (s *Server) answer(req *mdns.Msg) *mdns.Msg {
	m := new(mdns.Msg)
	m.SetReply(req)
	m.Compress = true
	m.RecursionAvailable = false
	opt := req.IsEdns0()
	do := opt != nil && opt.Do()
	if opt != nil {
		m.SetEdns0(maxUDPSize, do)
		if opt.Version() != 0 {
			m.Rcode = mdns.RcodeBadVers
			m.Question = req.Question
			return m
		}
	}
	if req.Opcode != mdns.OpcodeQuery {
		m.Rcode = mdns.RcodeNotImplemented
		return m
	}
	if len(req.Question) != 1 {
		m.Rcode = mdns.RcodeFormatError
		return m
	}
	q := req.Question[0]
	if q.Qclass != mdns.ClassINET {
		m.Rcode = mdns.RcodeRefused
		return m
	}
	switch q.Qtype {
	case mdns.TypeAXFR, mdns.TypeIXFR:
		m.Rcode = mdns.RcodeRefused
		return m
	}
	name := strings.TrimSuffix(strings.ToLower(q.Name), ".")
	cat := s.cat.Load()
	zd := cat.zoneFor(name)
	if zd == nil {
		m.Rcode = mdns.RcodeRefused
		return m
	}
	m.Authoritative = true
	switch {
	case q.Qtype == mdns.TypeANY:
		// RFC 8482: a minimal answer instead of everything at the name.
		m.Answer = []mdns.RR{&mdns.HINFO{Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeHINFO,
			Class: mdns.ClassINET, Ttl: 3600}, Cpu: "RFC8482"}}
	case q.Qtype == mdns.TypeDS && name == zd.name && cat.parentOf(zd) != nil:
		// The DS of a zone is the parent's data: when we serve the parent
		// too, answer from it (with the child's key when it is signed).
		s.dsFromParent(m, cat.parentOf(zd), zd, q.Name, do)
	default:
		s.resolve(m, cat, zd, q.Name, name, q.Qtype, 0, do)
	}
	if do {
		s.signAnswer(m, cat)
	}
	return m
}

// dsFromParent answers a DS query for child's apex from parent.
func (s *Server) dsFromParent(m *mdns.Msg, parent, child *zoneData, owner string, do bool) {
	if len(child.ds) > 0 {
		for _, rr := range child.ds {
			m.Answer = append(m.Answer, withOwner(rr, owner))
		}
		return
	}
	m.Ns = append(m.Ns, parent.negativeSOA())
	if do {
		m.Ns = append(m.Ns, denial(owner, []uint16{mdns.TypeNS}))
	}
}

// denial is the compact denial of existence (RFC 9824) for owner: an NSEC
// whose next name is the immediate successor, listing the types present.
func denial(owner string, types []uint16) mdns.RR {
	bitmap := append(slices.Clone(types), mdns.TypeRRSIG, mdns.TypeNSEC)
	slices.Sort(bitmap)
	bitmap = slices.Compact(bitmap)
	return &mdns.NSEC{
		Hdr:        mdns.RR_Header{Name: owner, Rrtype: mdns.TypeNSEC, Class: mdns.ClassINET, Ttl: NegativeTTL},
		NextDomain: `\000.` + owner,
		TypeBitMap: bitmap,
	}
}

// signAnswer adds RRSIGs for every authoritative RRset in the answer and
// authority sections, each signed by the zone that holds it. Referral NS
// records and glue are not signed (they are the child's data).
func (s *Server) signAnswer(m *mdns.Msg, cat *catalog) {
	dsQuery := len(m.Question) == 1 && m.Question[0].Qtype == mdns.TypeDS
	sign := func(section []mdns.RR, skipNS bool) []mdns.RR {
		var out []mdns.RR
		for i := 0; i < len(section); {
			h := section[i].Header()
			j := i + 1
			for j < len(section) && section[j].Header().Rrtype == h.Rrtype &&
				strings.EqualFold(section[j].Header().Name, h.Name) {
				j++
			}
			set := section[i:j]
			out = append(out, set...)
			i = j
			if skipNS && h.Rrtype == mdns.TypeNS {
				continue
			}
			zd := cat.zoneFor(strings.TrimSuffix(strings.ToLower(h.Name), "."))
			// The DS of a zone, or the proof there is none, is the parent's.
			if (h.Rrtype == mdns.TypeDS || h.Rrtype == mdns.TypeNSEC && dsQuery) && zd != nil &&
				zd.name+"." == strings.ToLower(h.Name) {
				if p := cat.parentOf(zd); p != nil {
					zd = p
				}
			}
			if zd == nil || zd.signer == nil {
				continue
			}
			sig, err := zd.signer.sign(set)
			if err != nil {
				s.log.Warn("sign dns answer", "zone", zd.name, "err", err)
				continue
			}
			out = append(out, sig)
		}
		return out
	}
	m.Answer = sign(m.Answer, false)
	m.Ns = sign(m.Ns, true)
}

// resolve appends the answer for (name, qtype) in zone zd to m, following
// CNAMEs and ALIASes into our zones. owner is the name as asked (its case
// is kept in the answer).
func (s *Server) resolve(m *mdns.Msg, cat *catalog, zd *zoneData, owner, name string, qtype uint16, depth int, do bool) {
	signed := do && zd.signer != nil
	// A delegation on the way down hands the query to the child's servers.
	if cut := zd.cutAbove(name); cut != "" {
		if !(cut == name && qtype == mdns.TypeDS) {
			if depth == 0 {
				m.Authoritative = false
			}
			n := zd.nodes[cut]
			m.Ns = append(m.Ns, n.rrsets[mdns.TypeNS]...)
			if signed {
				// Proves the child has no DS: an insecure delegation.
				m.Ns = append(m.Ns, denial(fqdn(cut), []uint16{mdns.TypeNS}))
			}
			m.Extra = append(m.Extra, zd.glue(n.rrsets[mdns.TypeNS])...)
			return
		}
	}
	n, ok := zd.nodes[name]
	if !ok {
		n = zd.wildcard(name)
		if n == nil {
			m.Ns = append(m.Ns, zd.negativeSOA())
			if signed {
				// Compact denial: NOERROR and an NSEC with the NXNAME
				// pseudo-type instead of NXDOMAIN and a proof of the gap.
				m.Ns = append(m.Ns, denial(owner, []uint16{mdns.TypeNXNAME}))
				return
			}
			m.Rcode = mdns.RcodeNameError // RFC 6604: the code of the last name
			return
		}
	}
	nodata := func() {
		m.Ns = append(m.Ns, zd.negativeSOA())
		if signed {
			m.Ns = append(m.Ns, denial(owner, n.types(qtype)))
		}
	}
	if cname := n.rrsets[mdns.TypeCNAME]; len(cname) > 0 && qtype != mdns.TypeCNAME {
		rr := withOwner(cname[0], owner)
		m.Answer = append(m.Answer, rr)
		s.chase(m, cat, rr.(*mdns.CNAME).Target, qtype, depth, do)
		return
	}
	if n.alias != "" && (qtype == mdns.TypeA || qtype == mdns.TypeAAAA) {
		addrs, ok := s.aliasAddrs(cat, n.alias, qtype, depth)
		if !ok {
			m.Rcode = mdns.RcodeServerFailure
			m.Authoritative = false
			return
		}
		for _, a := range addrs {
			m.Answer = append(m.Answer, addrRR(owner, qtype, n.aliasTTL, a))
		}
		if len(addrs) == 0 {
			nodata()
		}
		return
	}
	rrs := n.rrsets[qtype]
	if len(rrs) == 0 {
		nodata()
		return
	}
	for _, rr := range rrs {
		m.Answer = append(m.Answer, withOwner(rr, owner))
	}
}

// chase continues a CNAME into our own zones; a target elsewhere is left
// to the resolver.
func (s *Server) chase(m *mdns.Msg, cat *catalog, target string, qtype uint16, depth int, do bool) {
	if depth+1 >= maxChain {
		return
	}
	name := strings.TrimSuffix(strings.ToLower(target), ".")
	zd := cat.zoneFor(name)
	if zd == nil {
		return
	}
	before := len(m.Ns)
	s.resolve(m, cat, zd, target, name, qtype, depth+1, do)
	// Only the final answer matters; drop authority data of the chain.
	if len(m.Answer) > 0 {
		m.Ns = m.Ns[:before]
	}
}

// aliasAddrs returns the addresses of an ALIAS target: from our zones when
// we serve it, otherwise from the resolver (cached). ok is false when the
// target could not be resolved.
func (s *Server) aliasAddrs(cat *catalog, target string, qtype uint16, depth int) ([]netip.Addr, bool) {
	if depth+1 >= maxChain {
		return nil, false
	}
	if zd := cat.zoneFor(target); zd != nil {
		sub := new(mdns.Msg)
		s.resolve(sub, cat, zd, fqdn(target), target, qtype, depth+1, false)
		if sub.Rcode == mdns.RcodeServerFailure {
			return nil, false
		}
		var out []netip.Addr
		for _, rr := range sub.Answer {
			switch v := rr.(type) {
			case *mdns.A:
				a, _ := netip.AddrFromSlice(v.A.To4())
				out = append(out, a)
			case *mdns.AAAA:
				a, _ := netip.AddrFromSlice(v.AAAA)
				out = append(out, a)
			}
		}
		return out, true
	}
	return s.lookupAlias(target, qtype)
}

func (s *Server) lookupAlias(target string, qtype uint16) ([]netip.Addr, bool) {
	network := "ip4"
	if qtype == mdns.TypeAAAA {
		network = "ip6"
	}
	key := network + " " + target
	now := time.Now()
	s.aliasMu.Lock()
	e, hit := s.aliasCache[key]
	s.aliasMu.Unlock()
	if hit && now.Before(e.expires) {
		return e.addrs, e.addrs != nil
	}
	if s.resolver == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), aliasTimeout)
	defer cancel()
	addrs, err := s.resolver.LookupNetIP(ctx, network, target+".")
	ttl := aliasCacheTTL
	var dnsErr *net.DNSError
	switch {
	case err == nil:
		if addrs == nil {
			addrs = []netip.Addr{}
		}
		for i, a := range addrs {
			addrs[i] = a.Unmap()
		}
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		addrs, ttl = []netip.Addr{}, aliasNegativeTTL // no addresses of this family
	default:
		if hit {
			return e.addrs, e.addrs != nil // serve stale rather than fail
		}
		s.log.Warn("resolve ALIAS target", "target", target, "err", err)
		addrs, ttl = nil, aliasNegativeTTL
	}
	s.aliasMu.Lock()
	if len(s.aliasCache) >= aliasCacheSize {
		clear(s.aliasCache)
	}
	s.aliasCache[key] = aliasEntry{addrs: addrs, expires: now.Add(ttl)}
	s.aliasMu.Unlock()
	return addrs, addrs != nil
}

func addrRR(owner string, qtype uint16, ttl uint32, a netip.Addr) mdns.RR {
	hdr := mdns.RR_Header{Name: owner, Rrtype: qtype, Class: mdns.ClassINET, Ttl: ttl}
	if qtype == mdns.TypeA {
		b := a.As4()
		return &mdns.A{Hdr: hdr, A: net.IP(b[:])}
	}
	b := a.As16()
	return &mdns.AAAA{Hdr: hdr, AAAA: net.IP(b[:])}
}

func withOwner(rr mdns.RR, owner string) mdns.RR {
	c := mdns.Copy(rr)
	c.Header().Name = owner
	return c
}

// cutAbove returns the closest delegation point at or above name (below
// the apex), or "".
func (zd *zoneData) cutAbove(name string) string {
	cut := ""
	for n := name; n != zd.name && strings.HasSuffix(n, "."+zd.name); {
		if nd := zd.nodes[n]; nd != nil && nd.cut {
			cut = n // keep going: the topmost cut wins
		}
		_, n, _ = strings.Cut(n, ".")
	}
	return cut
}

// wildcard finds the wildcard node that covers a name with no node of its
// own (RFC 4592: "*" directly below the closest existing ancestor).
func (zd *zoneData) wildcard(name string) *node {
	for n := name; n != zd.name; {
		_, parent, ok := strings.Cut(n, ".")
		if !ok {
			return nil
		}
		if _, exists := zd.nodes[parent]; exists {
			return zd.nodes["*."+parent]
		}
		n = parent
	}
	return nil
}

// glue returns in-zone addresses of delegated nameservers.
func (zd *zoneData) glue(ns []mdns.RR) []mdns.RR {
	var out []mdns.RR
	for _, rr := range ns {
		host := strings.TrimSuffix(rr.(*mdns.NS).Ns, ".")
		if n := zd.nodes[host]; n != nil {
			out = append(out, n.rrsets[mdns.TypeA]...)
			out = append(out, n.rrsets[mdns.TypeAAAA]...)
		}
	}
	return out
}

// negativeSOA is the SOA in the authority section of NXDOMAIN and NODATA
// answers; its TTL is the negative caching time (RFC 2308).
func (zd *zoneData) negativeSOA() mdns.RR {
	c := mdns.Copy(zd.soa)
	c.Header().Ttl = min(c.Header().Ttl, NegativeTTL)
	return c
}
