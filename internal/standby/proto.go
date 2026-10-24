// Package standby runs standby panels: a standby enrolls with the panel
// like a server, then keeps a copy of the panel's database (walship) and
// its master key, ready to take over. The panel side (Feeder) serves on
// the agents' port; the standby side (Client) is `basalt server` started
// in a data directory holding standby.json.
package standby

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/wiefix/basalt/internal/walship"
)

const (
	PathEnroll    = "/panel/v1/standby/enroll"
	PathReplicate = "/panel/v1/standby/replicate"
	PathRenew     = "/panel/v1/standby/renew"
)

type EnrollRequest struct {
	Token string `json:"token"`
	CSR   []byte `json:"csr"`
}

type EnrollResponse struct {
	StandbyID int64  `json:"standby_id"`
	Cert      []byte `json:"cert"`
	CACert    []byte `json:"ca_cert"`
}

type RenewRequest struct {
	CSR []byte `json:"csr"`
}

type RenewResponse struct {
	Cert []byte `json:"cert"`
}

// Report is what a standby tells the panel about its copy, a JSON line
// every reportEvery on the request body of the replicate stream.
type Report struct {
	Ready   bool      `json:"ready"`
	Applied time.Time `json:"applied"` // when the copy last took a change
	Error   string    `json:"error,omitempty"`
	Version string    `json:"version,omitempty"`
	// AgentURL is where agents reach the standby once it takes over.
	AgentURL string `json:"agent_url,omitempty"`
}

const (
	reportEvery = 5 * time.Second
	pingEvery   = 15 * time.Second
	// A stream silent this long is dead.
	silence = 45 * time.Second
)

// Frames of the replicate stream (panel to standby): the walship messages,
// and these.
const (
	kindMasterKey walship.Kind = 16 // the panel's master key, first
	kindPing      walship.Kind = 17
	// The panel's epoch and the addresses of the panel and its standbys
	// (JSON agentproto.PanelInfo), after the master key and when they
	// change; only to standbys that send agentproto.HeaderPanelEpoch.
	kindPanels walship.Kind = 18
)

// Like agents, standby panels learn the panel's epoch and addresses: when
// the panel fails and another standby takes over, they follow that one.
// Both sides send agentproto.HeaderPanelEpoch: a panel with a lower epoch
// than the standby knows refuses it (409) and learns it was replaced, and
// a standby does not follow such a panel.

const (
	frameHeader = 1 + 8 + 4
	maxFrame    = 4 << 20
)

func writeFrame(w io.Writer, m walship.Msg) error {
	b := make([]byte, frameHeader+len(m.Data))
	b[0] = byte(m.Kind)
	binary.BigEndian.PutUint64(b[1:], uint64(m.Offset))
	binary.BigEndian.PutUint32(b[9:], uint32(len(m.Data)))
	copy(b[frameHeader:], m.Data)
	_, err := w.Write(b)
	return err
}

func readFrame(r io.Reader) (walship.Msg, error) {
	var h [frameHeader]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return walship.Msg{}, err
	}
	n := binary.BigEndian.Uint32(h[9:])
	if n > maxFrame {
		return walship.Msg{}, fmt.Errorf("frame of %d bytes", n)
	}
	off := int64(binary.BigEndian.Uint64(h[1:]))
	if off < 0 {
		return walship.Msg{}, errors.New("negative offset")
	}
	m := walship.Msg{Kind: walship.Kind(h[0]), Offset: off}
	if n > 0 {
		m.Data = make([]byte, n)
		if _, err := io.ReadFull(r, m.Data); err != nil {
			return walship.Msg{}, err
		}
	}
	return m, nil
}
