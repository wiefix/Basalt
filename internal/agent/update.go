package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/release"
	"github.com/wiefix/basalt/internal/sshsig"
)

// maxBinary bounds a downloaded binary.
const maxBinary = 512 << 20

// updater downloads a release's binary for the privileged helper, which
// checks it against the signed checksums itself, puts it in place and
// restarts the services.
type updater struct {
	log  *slog.Logger
	key  string
	dir  string // where downloads wait for the helper
	priv privileged
}

func newUpdater(cfg Config, priv privileged, log *slog.Logger) *updater {
	u := &updater{log: log, key: cfg.ReleaseKey, dir: filepath.Join(cfg.DataDir, "update"), priv: priv}
	if u.key == "" {
		u.key = sshsig.ReleaseKey
	}
	return u
}

func (u *updater) updatable(ctx context.Context) bool {
	ok, err := u.priv.Updatable(ctx)
	if err != nil {
		u.log.Warn("ask whether updates are possible", "err", err)
	}
	return ok
}

func (u *updater) prepare(ctx context.Context, hc *http.Client, panelURL string, req agentproto.UpdatePrepare) error {
	if err := req.Validate(); err != nil {
		return err
	}
	// Checked here too, before downloading anything: the helper repeats it.
	sums, err := release.VerifySums(req.Sums, req.Sig, u.key)
	if err != nil {
		return err
	}
	name := release.BinaryName(runtime.GOARCH)
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("the release has no binary for %s", runtime.GOARCH)
	}
	if !u.updatable(ctx) {
		return errors.New("this server's Basalt was not installed by the installer: update it by running the installer again")
	}
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(u.dir, name)
	defer os.Remove(path)
	if err := download(ctx, hc, panelURL+agentproto.PathRelease+req.Version+"/"+name, path, want); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return u.priv.StageUpdate(ctx, req, f)
}

// download saves url to path, checking its SHA-256.
func download(ctx context.Context, hc *http.Client, url, path, wantHex string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("download the new version: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download the new version: %s", resp.Status)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxBinary+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download the new version: %w", err)
	}
	if n > maxBinary {
		return errors.New("the new binary is too large")
	}
	if hex.EncodeToString(h.Sum(nil)) != wantHex {
		return errors.New("the downloaded binary does not match the signed checksum")
	}
	return nil
}

// activate has the helper put the staged binary in place and restart the
// services.
func (u *updater) activate(ctx context.Context, req agentproto.UpdateActivate) error {
	if err := req.Validate(); err != nil {
		return err
	}
	return u.priv.ActivateUpdate(ctx, req)
}
