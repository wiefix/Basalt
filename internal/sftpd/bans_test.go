package sftpd

import (
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestBanList(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := newBanList()
	b.now = func() time.Time { return now }
	var got []Ban
	b.onBan = func(x Ban) { got = append(got, x) }
	fail := func(n int) {
		for range n {
			b.fail("192.0.2.1")
			now = now.Add(time.Second)
		}
	}
	fail(banAfter - 1)
	if b.banned("192.0.2.1") {
		t.Fatal("banned before the limit")
	}
	// Failures older than the window do not count.
	now = now.Add(banWindow)
	fail(banAfter - 1)
	if b.banned("192.0.2.1") {
		t.Fatal("old failures counted")
	}
	fail(1)
	if !b.banned("192.0.2.1") || len(got) != 1 || got[0].Until.Sub(now) > banFor || got[0].Strikes != 1 {
		t.Fatalf("first ban: %+v", got)
	}
	if b.banned("192.0.2.2") {
		t.Fatal("another address banned")
	}
	// A repeat within a day doubles the time.
	now = got[0].Until.Add(time.Second)
	if b.banned("192.0.2.1") {
		t.Fatal("ban did not end")
	}
	fail(banAfter)
	if len(got) != 2 || got[1].Strikes != 2 || got[1].Until.Sub(now) < banFor+time.Minute {
		t.Fatalf("second ban: %+v", got[1])
	}
	if l := b.list(); len(l) != 1 || l[0].IP != "192.0.2.1" {
		t.Fatalf("list = %+v", l)
	}
	if !b.unban("192.0.2.1") || b.banned("192.0.2.1") || b.unban("192.0.2.1") {
		t.Fatal("unban")
	}
	// After a quiet day the count starts again.
	now = now.Add(2 * banMemory)
	fail(banAfter)
	if got[2].Strikes != 1 {
		t.Fatalf("strikes after a quiet day: %+v", got[2])
	}
}

// TestBanAfterFailedPasswords: real connections; after too many wrong
// passwords even the right one is refused, until the ban is lifted.
func TestBanAfterFailedPasswords(t *testing.T) {
	f := start(t)
	for i := range banAfter {
		if c, err := f.dial("blog", ssh.Password("wrong password")); err == nil {
			c.Close()
			t.Fatalf("attempt %d logged in", i)
		}
	}
	if c, err := f.dial("blog", ssh.Password("site password 1")); err == nil {
		c.Close()
		t.Fatal("a banned address logged in")
	}
	// Keys are refused too: the connection is closed before authentication.
	if c, err := f.dial("deploy", ssh.PublicKeys(f.userKey)); err == nil {
		c.Close()
		t.Fatal("a banned address logged in with a key")
	}
	bans := f.srv.Bans()
	if len(bans) != 1 || bans[0].IP != "127.0.0.1" {
		t.Fatalf("bans = %+v", bans)
	}
	if !f.srv.Unban("127.0.0.1") {
		t.Fatal("unban failed")
	}
	// (The account's own password limiter is still spent: use the key.)
	c, err := f.dial("deploy", ssh.PublicKeys(f.userKey))
	if err != nil {
		t.Fatalf("after unban: %v", err)
	}
	c.Close()
}
