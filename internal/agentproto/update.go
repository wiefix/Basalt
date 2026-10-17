package agentproto

import (
	"errors"

	"github.com/wiefix/basalt/internal/release"
)

// Updating Basalt on a server, in two steps so that every server has the
// new binary in place before any restarts (the panel's own host restarts
// the panel too).
const (
	// TypeUpdatePrepare downloads a release's binary from the panel, checks
	// it against the signed checksums and stages it. Streaming (no frames).
	TypeUpdatePrepare = "update_prepare"
	// TypeUpdateActivate puts the staged binary in place and restarts the
	// Basalt services shortly after answering.
	TypeUpdateActivate = "update_activate"

	// PathRelease is where agents download staged release files:
	// PathRelease + "<version>/<file>".
	PathRelease = "/agent/v1/release/"
)

// UpdatePrepare carries the signed checksum list: the agent checks the
// signature with the release key built into it, so the panel alone cannot
// make it run another binary.
type UpdatePrepare struct {
	Version string `json:"version"`
	Sums    []byte `json:"sums"`
	Sig     []byte `json:"sig"`
}

func (r *UpdatePrepare) Validate() error {
	if !release.ValidVersion(r.Version) {
		return errors.New("invalid version")
	}
	if len(r.Sums) == 0 || len(r.Sums) > 64<<10 || len(r.Sig) == 0 || len(r.Sig) > 16<<10 {
		return errors.New("invalid checksums")
	}
	return nil
}

// UpdateActivate switches to the version staged by UpdatePrepare.
type UpdateActivate struct {
	Version string `json:"version"`
}

func (r *UpdateActivate) Validate() error {
	if !release.ValidVersion(r.Version) {
		return errors.New("invalid version")
	}
	return nil
}
