package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
)

// The anchor of the panel's audit log (agentproto.AuditAnchor), kept in
// the agent's data directory: on another server than the panel's, or, on
// the panel's own, owned by root where the panel's user cannot write.
const auditAnchorFile = "audit-anchor.json"

var auditHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

type storedAnchor struct {
	// CA is the fingerprint of the panel's CA: an anchor of another panel
	// (the agent enrolled again elsewhere) does not count.
	CA   string    `json:"ca"`
	ID   int64     `json:"id"`
	Hash string    `json:"hash"`
	At   time.Time `json:"at"`
}

var auditAnchorMu sync.Mutex

func (a *Agent) auditAnchor(req agentproto.AuditAnchor) (agentproto.AuditAnchorResponse, error) {
	var resp agentproto.AuditAnchorResponse
	if req.ID <= 0 || !auditHashRE.MatchString(req.Hash) {
		return resp, errors.New("invalid audit anchor")
	}
	sum := sha256.Sum256(a.identity().ca.Raw)
	ca := hex.EncodeToString(sum[:])
	auditAnchorMu.Lock()
	defer auditAnchorMu.Unlock()
	path := filepath.Join(a.dir, auditAnchorFile)
	var cur storedAnchor
	if b, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(b, &cur) != nil || cur.CA != ca {
			cur = storedAnchor{}
		}
	}
	resp.Before, resp.At = agentproto.AuditAnchor{ID: cur.ID, Hash: cur.Hash}, cur.At
	switch {
	case cur.ID == req.ID && cur.Hash == req.Hash:
		return resp, nil
	case cur.ID >= req.ID && !req.Replace:
		// Older, or the same entry with another hash: the panel's log is
		// not the one this agent saw grow.
		resp.Kept = true
		return resp, nil
	}
	if req.Replace && cur.ID != 0 {
		a.log.Warn("audit log anchor replaced by the panel", "was_entry", cur.ID, "was_hash", cur.Hash, "entry", req.ID, "hash", req.Hash)
	}
	b, err := json.Marshal(storedAnchor{CA: ca, ID: req.ID, Hash: req.Hash, At: time.Now().UTC()})
	if err != nil {
		return resp, err
	}
	return resp, writeAtomic(path, b, 0o600)
}
