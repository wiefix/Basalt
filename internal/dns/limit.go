package dns

import (
	"hash/maphash"
	"net"
	"net/netip"
	"sync"
	"time"
)

// prefixLimiter is a token bucket per client network (/24 for IPv4, /56
// for IPv6). Buckets live in a fixed table indexed by a hash of the
// prefix, so spoofed sources cannot grow memory; colliding prefixes share
// a bucket, which only makes the limit stricter.
type prefixLimiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	now     func() time.Time
	seed    maphash.Seed
	buckets [1 << 14]limitBucket
}

type limitBucket struct {
	tokens float64
	last   int64 // unix nanoseconds; 0 = unused
}

func newPrefixLimiter(rate, burst float64, now func() time.Time) *prefixLimiter {
	return &prefixLimiter{rate: rate, burst: burst, now: now, seed: maphash.MakeSeed()}
}

func clientPrefix(addr net.Addr) netip.Prefix {
	var ip netip.Addr
	switch a := addr.(type) {
	case *net.UDPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	case *net.TCPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	}
	ip = ip.Unmap()
	bits := 56
	if ip.Is4() {
		bits = 24
	}
	p, _ := ip.Prefix(bits)
	return p
}

func (l *prefixLimiter) allow(addr net.Addr) bool {
	p := clientPrefix(addr)
	b, _ := p.MarshalBinary()
	i := maphash.Bytes(l.seed, b) % uint64(len(l.buckets))
	now := l.now().UnixNano()
	l.mu.Lock()
	defer l.mu.Unlock()
	bk := &l.buckets[i]
	if bk.last == 0 {
		bk.tokens = l.burst
	} else {
		bk.tokens = min(l.burst, bk.tokens+float64(now-bk.last)/1e9*l.rate)
	}
	bk.last = now
	if bk.tokens < 1 {
		return false
	}
	bk.tokens--
	return true
}

// limitListener caps concurrent TCP connections in total (Accept waits)
// and per client address (extra connections are closed at once).
type limitListener struct {
	net.Listener
	sem   chan struct{}
	perIP int

	mu    sync.Mutex
	conns map[netip.Addr]int
}

func newLimitListener(ln net.Listener, total, perIP int) *limitListener {
	return &limitListener{Listener: ln, sem: make(chan struct{}, total), perIP: perIP, conns: map[netip.Addr]int{}}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		l.sem <- struct{}{}
		c, err := l.Listener.Accept()
		if err != nil {
			<-l.sem
			return nil, err
		}
		ip := netip.Addr{}
		if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
			ip, _ = netip.AddrFromSlice(a.IP)
			ip = ip.Unmap()
		}
		l.mu.Lock()
		if l.conns[ip] >= l.perIP {
			l.mu.Unlock()
			c.Close()
			<-l.sem
			continue
		}
		l.conns[ip]++
		l.mu.Unlock()
		return &limitConn{Conn: c, l: l, ip: ip}, nil
	}
}

type limitConn struct {
	net.Conn
	l    *limitListener
	ip   netip.Addr
	once sync.Once
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.l.mu.Lock()
		if c.l.conns[c.ip]--; c.l.conns[c.ip] <= 0 {
			delete(c.l.conns, c.ip)
		}
		c.l.mu.Unlock()
		<-c.l.sem
	})
	return err
}
