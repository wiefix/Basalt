package agenthub

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/wiefix/basalt/internal/pki"
	"github.com/wiefix/basalt/internal/secrets"
	"github.com/wiefix/basalt/internal/store"
)

const (
	settingCACert = "agent_ca_cert"
	settingCAKey  = "agent_ca_key"
)

var caKeyAD = []byte("agent-ca-key")

// LoadOrCreateCA returns the agent CA, creating it on first run. The private
// key is stored in the database encrypted with the master key.
func LoadOrCreateCA(ctx context.Context, db *store.DB, box *secrets.Box, instanceID string, now time.Time) (*pki.CA, error) {
	certB64, err := db.Setting(ctx, settingCACert)
	if errors.Is(err, store.ErrNotFound) {
		ca, err := pki.NewCA("Basalt agent CA "+instanceID, now)
		if err != nil {
			return nil, fmt.Errorf("create agent CA: %w", err)
		}
		keyDER, err := ca.MarshalKey()
		if err != nil {
			return nil, err
		}
		enc := base64.StdEncoding
		if err := db.SetSettingIfAbsent(ctx, settingCAKey, enc.EncodeToString(box.Seal(keyDER, caKeyAD))); err != nil {
			return nil, err
		}
		if err := db.SetSettingIfAbsent(ctx, settingCACert, enc.EncodeToString(ca.Cert.Raw)); err != nil {
			return nil, err
		}
		return LoadOrCreateCA(ctx, db, box, instanceID, now)
	}
	if err != nil {
		return nil, err
	}

	keyB64, err := db.Setting(ctx, settingCAKey)
	if err != nil {
		return nil, fmt.Errorf("agent CA key: %w", err)
	}
	certDER, err := base64.StdEncoding.DecodeString(certB64)
	if err != nil {
		return nil, fmt.Errorf("decode agent CA certificate: %w", err)
	}
	sealed, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("decode agent CA key: %w", err)
	}
	keyDER, err := box.Open(sealed, caKeyAD)
	if err != nil {
		return nil, fmt.Errorf("decrypt agent CA key (wrong master.key?): %w", err)
	}
	return pki.LoadCA(certDER, keyDER)
}
