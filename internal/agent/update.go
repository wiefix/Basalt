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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/release"
	"github.com/wiefix/basalt/internal/sshsig"
)

// The units the installer writes; an update restarts those that exist.
var basaltUnits = []string{"basalt-server.service", "basalt-proxy.service", "basalt-agent.service"}

const unitDir = "/etc/systemd/system"

// maxBinary bounds a downloaded binary.
const maxBinary = 512 << 20

// updater replaces the Basalt binary with a release's and restarts the
// services. The new binary must match the checksums signed with the
// release key built into this agent.
type updater struct {
	log     *slog.Logger
	key     string
	exe     string // the binary to replace ("" when updates are not possible)
	restart func(units []string) error

	mu     sync.Mutex
	staged string // version waiting in exe + ".new"
}

func newUpdater(cfg Config, log *slog.Logger) *updater {
	u := &updater{log: log, key: cfg.ReleaseKey, exe: cfg.UpdateExe, restart: cfg.Restart}
	if u.key == "" {
		u.key = sshsig.ReleaseKey
	}
	if u.exe == "" && runtime.GOOS == "linux" && os.Geteuid() == 0 && systemdUnits() != nil {
		if exe, err := os.Executable(); err == nil {
			u.exe, _ = filepath.EvalSymlinks(exe)
		}
	}
	if u.restart == nil {
		u.restart = systemctlRestart
	}
	return u
}

// systemdUnits lists the installed Basalt units; nil without systemd or
// without the agent's unit (not installed by the installer).
func systemdUnits() []string {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(unitDir, "basalt-agent.service")); err != nil {
		return nil
	}
	var units []string
	for _, u := range basaltUnits {
		if _, err := os.Stat(filepath.Join(unitDir, u)); err == nil {
			units = append(units, u)
		}
	}
	return units
}

// systemctlRestart has systemd restart the units without waiting: this
// process is one of them.
func systemctlRestart(units []string) error {
	args := append([]string{"--no-block", "restart"}, units...)
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (u *updater) updatable() bool { return u.exe != "" }

func (u *updater) prepare(ctx context.Context, hc *http.Client, panelURL string, req agentproto.UpdatePrepare) error {
	if err := req.Validate(); err != nil {
		return err
	}
	if !u.updatable() {
		return errors.New("this server's Basalt was not installed by the installer: update it by running the installer again")
	}
	sums, err := release.VerifySums(req.Sums, req.Sig, u.key)
	if err != nil {
		return err
	}
	if v, ok := sums[release.VersionFile]; !ok || v == "" {
		return errors.New("the checksums do not cover the version file")
	}
	name := release.BinaryName(runtime.GOARCH)
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("the release has no binary for %s", runtime.GOARCH)
	}
	u.mu.Lock()
	defer u.mu.Unlock()

	staged := u.exe + ".new"
	if err := download(ctx, hc, panelURL+agentproto.PathRelease+req.Version+"/"+name, staged, want); err != nil {
		os.Remove(staged)
		return err
	}
	// The new binary must run here and be the version announced.
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, staged, "version").Output()
	if err != nil || !strings.Contains(string(out), " "+req.Version+" ") {
		os.Remove(staged)
		return fmt.Errorf("the new binary does not run here or is not %s (%v)", req.Version, err)
	}
	u.staged = req.Version
	u.log.Info("update staged", "version", req.Version)
	return nil
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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
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

// activate puts the staged binary in place, then restarts the services a
// moment after the answer has gone out.
func (u *updater) activate(req agentproto.UpdateActivate) error {
	if err := req.Validate(); err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.staged != req.Version {
		return fmt.Errorf("version %s is not prepared here", req.Version)
	}
	if err := os.Rename(u.exe+".new", u.exe); err != nil {
		return err
	}
	u.staged = ""
	units := systemdUnits()
	if len(units) == 0 {
		units = basaltUnits[1:] // tests: the restart hook gets the usual ones
	}
	u.log.Info("updated; restarting", "version", req.Version, "units", units)
	go func() {
		time.Sleep(time.Second)
		if err := u.restart(units); err != nil {
			u.log.Error("restart after update", "err", err)
		}
	}()
	return nil
}
