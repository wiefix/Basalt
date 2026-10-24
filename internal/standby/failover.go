package standby

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/pki"
)

// ErrTakeOver: the standby decided to take over (Failover): Run returns
// it, and the caller promotes the copy and starts it as the panel.
var ErrTakeOver = errors.New("the panel is gone: this standby takes over")

const (
	// failoverCheckEvery is how often a standby that takes over by itself
	// looks at the panel's absence.
	failoverCheckEvery = 5 * time.Second
	witnessTimeout     = 10 * time.Second
)

// setFailover records how the panel wants this standby to take over.
func (c *Client) setFailover(data []byte) error {
	var f Failover
	if err := json.Unmarshal(data, &f); err != nil {
		return errors.New("the panel sent an invalid failover setting")
	}
	if err := f.Validate(); err != nil {
		return err
	}
	return c.updateConfig(func(cfg *config) bool {
		var next *Failover
		if f.Auto {
			next = &f
		}
		if equalFailover(cfg.Failover, next) {
			return false
		}
		if c.log != nil {
			if next != nil {
				c.log.Info("this standby takes over by itself when the panel is gone", "after_s", f.After, "witnesses", len(f.Witnesses))
			} else if cfg.Failover != nil {
				c.log.Info("this standby no longer takes over by itself")
			}
		}
		cfg.Failover = next
		return true
	})
}

func equalFailover(a, b *Failover) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Auto == b.Auto && a.After == b.After && slices.Equal(a.Witnesses, b.Witnesses)
}

// witnessAnswer is what one server said (or why it could not).
type witnessAnswer struct {
	ServerID int64
	Status   agentproto.PanelStatus
	Err      error
}

// decide says whether to take over on the witnesses' answers: most of all
// the witnesses must have answered that they have not reached the panel
// for at least half the delay, none that it reaches it, none of a newer
// panel. reason says why not (or why).
func decide(f *Failover, answers []witnessAnswer, epoch int64) (bool, string) {
	yes := 0
	for _, a := range answers {
		switch {
		case a.Err != nil:
		case a.Status.Epoch > epoch:
			return false, fmt.Sprintf("server %d knows a newer panel (epoch %d)", a.ServerID, a.Status.Epoch)
		case a.Status.Connected:
			return false, fmt.Sprintf("server %d reaches the panel", a.ServerID)
		case a.Status.For*2 >= f.After:
			yes++
		}
	}
	n := len(f.Witnesses)
	if yes*2 <= n {
		return false, fmt.Sprintf("%d of %d servers confirm the panel is gone; more than half must", yes, n)
	}
	return true, fmt.Sprintf("%d of %d servers confirm the panel is gone", yes, n)
}

// askWitness asks a server's agent, through its tunnel, whether it
// reaches the panel. The agent is checked like the agents check each
// other: a certificate of the panel's CA for that server.
func (c *Client) askWitness(ctx context.Context, w Witness) (agentproto.PanelStatus, error) {
	var st agentproto.PanelStatus
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return c.tlsCert(), nil
		},
		InsecureSkipVerify: true, // agents' certificates carry no host names: checked below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			roots := x509.NewCertPool()
			roots.AddCert(c.ca)
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
				return err
			}
			if id, err := pki.AgentID(leaf); err != nil || id != w.ServerID {
				return fmt.Errorf("reached another server than %d", w.ServerID)
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(ctx, witnessTimeout)
	defer cancel()
	d := tls.Dialer{NetDialer: &net.Dialer{}, Config: cfg}
	nc, err := d.DialContext(ctx, "tcp", w.Addr)
	if err != nil {
		return st, err
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(witnessTimeout))
	b, _ := json.Marshal(agentproto.TunnelHello{Service: agentproto.ServicePanelStatus})
	if _, err := nc.Write(append(b, '\n')); err != nil {
		return st, err
	}
	r := bufio.NewReaderSize(nc, 1024)
	line, err := r.ReadSlice('\n')
	if err != nil {
		return st, err
	}
	var reply agentproto.TunnelReply
	if err := json.Unmarshal(line, &reply); err != nil || !reply.OK {
		return st, fmt.Errorf("refused: %s", reply.Error)
	}
	if line, err = r.ReadSlice('\n'); err != nil {
		return st, err
	}
	if err := json.Unmarshal(line, &st); err != nil {
		return st, err
	}
	return st, nil
}

// watchPanel calls takeOver once this standby should become the panel:
// it takes over by itself, its copy is there, it has not reached the
// panel for the delay, and the witnesses agree.
func (c *Client) watchPanel(ctx context.Context, takeOver func()) {
	t := time.NewTicker(failoverCheckEvery)
	defer t.Stop()
	last := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.mu.Lock()
		f, connected, lost, epoch := c.cfg.Failover, c.connected, c.lostAt, c.cfg.Epoch
		c.mu.Unlock()
		if f == nil || connected || time.Since(lost) < time.Duration(f.After)*time.Second || !c.hasCopy() {
			continue
		}
		answers := make([]witnessAnswer, len(f.Witnesses))
		done := make(chan struct{})
		for i, w := range f.Witnesses {
			go func() {
				st, err := c.askWitness(ctx, w)
				answers[i] = witnessAnswer{ServerID: w.ServerID, Status: st, Err: err}
				done <- struct{}{}
			}()
		}
		for range f.Witnesses {
			<-done
		}
		ok, reason := decide(f, answers, epoch)
		c.mu.Lock()
		c.failoverNote = reason
		c.mu.Unlock()
		if ok {
			c.log.Warn("taking over the panel", "reason", reason, "unreachable_for", time.Since(lost).Round(time.Second))
			takeOver()
			return
		}
		if reason != last {
			c.log.Warn("the panel is unreachable, not taking over", "reason", reason)
			last = reason
		}
	}
}

// hasCopy: the copy of the database and the master key are there (the
// copy's file appears only once a snapshot is complete; the promotion
// checks it).
func (c *Client) hasCopy() bool {
	for _, f := range []string{fileDB, fileKey} {
		if _, err := os.Stat(filepath.Join(c.dir, f)); err != nil {
			return false
		}
	}
	return true
}
