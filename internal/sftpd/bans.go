package sftpd

import (
	"slices"
	"sync"
	"time"
)

// Brute force: an address that fails too many password logins is refused
// for a while; each repeat within a day doubles the time.
const (
	banAfter   = 10               // failed passwords...
	banWindow  = 15 * time.Minute // ...within this time
	banFor     = time.Hour
	banMax     = 24 * time.Hour
	banMemory  = 24 * time.Hour // how long a ban counts as a repeat
	maxTracked = 10000          // addresses remembered (failures and bans)
)

// Ban is an address refused for failed logins.
type Ban struct {
	IP       string    `json:"ip"`
	Until    time.Time `json:"until"`
	Failures int       `json:"failures,omitempty"` // failed passwords that led to it (when banned)
	Strikes  int       `json:"strikes"`            // bans within the last day
}

type banState struct {
	failures []time.Time // recent failures
	until    time.Time   // banned until
	strikes  int
	lastBan  time.Time
}

type banList struct {
	mu    sync.Mutex
	now   func() time.Time
	ips   map[string]*banState
	onBan func(Ban)
}

func newBanList() *banList { return &banList{now: time.Now, ips: map[string]*banState{}} }

// banned reports whether ip is refused now.
func (b *banList) banned(ip string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.ips[ip]
	return st != nil && b.now().Before(st.until)
}

// fail records a failed password from ip, banning it past the limit.
func (b *banList) fail(ip string) {
	b.mu.Lock()
	now := b.now()
	st := b.ips[ip]
	if st == nil {
		if len(b.ips) >= maxTracked {
			b.prune(now)
			if len(b.ips) >= maxTracked {
				b.mu.Unlock()
				return // flooded by addresses: the connection limits still apply
			}
		}
		st = &banState{}
		b.ips[ip] = st
	}
	keep := st.failures[:0]
	for _, t := range st.failures {
		if now.Sub(t) < banWindow {
			keep = append(keep, t)
		}
	}
	st.failures = append(keep, now)
	var ban *Ban
	if len(st.failures) >= banAfter {
		if now.Sub(st.lastBan) > banMemory {
			st.strikes = 0
		}
		d := banFor << st.strikes
		if d > banMax || d <= 0 {
			d = banMax
		}
		st.strikes++
		st.until, st.lastBan = now.Add(d), now
		ban = &Ban{IP: ip, Until: st.until, Failures: len(st.failures), Strikes: st.strikes}
		st.failures = nil
	}
	onBan := b.onBan
	b.mu.Unlock()
	if ban != nil && onBan != nil {
		onBan(*ban)
	}
}

// prune forgets addresses with no recent failures, ban or strike.
func (b *banList) prune(now time.Time) {
	for ip, st := range b.ips {
		recent := len(st.failures) > 0 && now.Sub(st.failures[len(st.failures)-1]) < banWindow
		if !recent && now.After(st.until) && now.Sub(st.lastBan) > banMemory {
			delete(b.ips, ip)
		}
	}
}

func (b *banList) list() []Ban {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	out := []Ban{}
	for ip, st := range b.ips {
		if now.Before(st.until) {
			out = append(out, Ban{IP: ip, Until: st.until, Strikes: st.strikes})
		}
	}
	slices.SortFunc(out, func(a, b Ban) int { return a.Until.Compare(b.Until) })
	return out
}

// unban lifts a ban and forgets the address's history.
func (b *banList) unban(ip string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.ips[ip]
	if st == nil || !b.now().Before(st.until) {
		return false
	}
	delete(b.ips, ip)
	return true
}
