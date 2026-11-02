package privhelper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/release"
)

// An update replaces the Basalt binary (root's) and restarts the services.
// The agent downloads the release's binary and hands the helper the open
// file; the helper itself checks it against the release's checksums,
// signed with the release key built into it: a compromised agent cannot
// make it install anything else.

const (
	// MsgUpdateInfo tells whether this host can be updated (→ UpdateInfo).
	MsgUpdateInfo = "update_info"
	// MsgUpdateStage stages a release's binary, the request's file
	// (agentproto.UpdatePrepare); MsgUpdateActivate puts it in place and
	// restarts the services (agentproto.UpdateActivate).
	MsgUpdateStage    = "update_stage"
	MsgUpdateActivate = "update_activate"
)

// UpdateInfo: Updatable is false when the installer did not set this host
// up (no systemd service of the agent): nothing to restart.
type UpdateInfo struct {
	Updatable bool `json:"updatable"`
}

// The units the installer writes; an update restarts those that exist,
// in this order. The helper comes last: systemctl runs in its service, and
// restarting it stops systemctl too, before it asked for any unit after.
var basaltUnits = []string{"basalt-server.service", "basalt-proxy.service", "basalt-agent.service", "basalt-agent-helper.service"}

const unitDir = "/etc/systemd/system"

// maxBinary bounds a release's binary.
const maxBinary = 512 << 20

// InstalledAsService tells whether the installer set this host up: its
// basalt-agent service runs the agent.
func InstalledAsService() bool { return systemdUnits() != nil }

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

// updateExe is the binary an update replaces: this one, when the installer
// set the host up and the process may replace it.
func updateExe() string {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 || systemdUnits() == nil {
		return ""
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	exe, _ = filepath.EvalSymlinks(exe)
	return exe
}

func (h *Helper) handleUpdate(ctx context.Context, msgType string, payload json.RawMessage) (any, error) {
	switch msgType {
	case MsgUpdateInfo:
		ok, err := h.Updatable(ctx)
		return UpdateInfo{Updatable: ok}, err
	case MsgUpdateStage:
		req, err := decode[agentproto.UpdatePrepare](payload)
		if err != nil {
			return nil, err
		}
		files := requestFiles(ctx)
		if len(files) != 1 {
			return nil, errors.New("no binary to stage")
		}
		return struct{}{}, h.StageUpdate(ctx, req, files[0])
	default:
		req, err := decode[agentproto.UpdateActivate](payload)
		if err != nil {
			return nil, err
		}
		return struct{}{}, h.ActivateUpdate(ctx, req)
	}
}

func (h *Helper) Updatable(context.Context) (bool, error) { return h.cfg.UpdateExe != "", nil }

// StageUpdate checks bin against the signed checksums of req and puts it
// next to the binary it replaces, after checking it runs here and is the
// version announced.
func (h *Helper) StageUpdate(ctx context.Context, req agentproto.UpdatePrepare, bin *os.File) error {
	if err := req.Validate(); err != nil {
		return err
	}
	if h.cfg.UpdateExe == "" {
		return errors.New("this server's Basalt was not installed by the installer: update it by running the installer again")
	}
	sums, err := release.VerifySums(req.Sums, req.Sig, h.cfg.ReleaseKey)
	if err != nil {
		return err
	}
	if v, ok := sums[release.VersionFile]; !ok || v == "" {
		return errors.New("the checksums do not cover the version file")
	}
	want, ok := sums[release.BinaryName(runtime.GOARCH)]
	if !ok {
		return fmt.Errorf("the release has no binary for %s", runtime.GOARCH)
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	h.staged = ""
	staged := h.cfg.UpdateExe + ".new"
	if err := stageBinary(bin, staged, want); err != nil {
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
	h.staged = req.Version
	h.log.Info("update staged", "version", req.Version)
	return nil
}

// stageBinary copies a binary to path, checking its SHA-256.
func stageBinary(bin io.Reader, path, wantHex string) error {
	os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), io.LimitReader(bin, maxBinary+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("stage the new version: %w", err)
	}
	if n > maxBinary {
		return errors.New("the new binary is too large")
	}
	if hex.EncodeToString(sum.Sum(nil)) != wantHex {
		return errors.New("the new binary does not match the signed checksum")
	}
	return nil
}

// ActivateUpdate puts the staged binary in place, then restarts the
// services a moment after the answer has gone out.
func (h *Helper) ActivateUpdate(_ context.Context, req agentproto.UpdateActivate) error {
	if err := req.Validate(); err != nil {
		return err
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	if h.staged == "" || h.staged != req.Version {
		return fmt.Errorf("version %s is not prepared here", req.Version)
	}
	if err := os.Rename(h.cfg.UpdateExe+".new", h.cfg.UpdateExe); err != nil {
		return err
	}
	h.staged = ""
	units := systemdUnits()
	if len(units) == 0 {
		units = basaltUnits[1:] // tests: the restart hook gets the usual ones
	}
	h.log.Info("updated; restarting", "version", req.Version, "units", units)
	go func() {
		time.Sleep(time.Second)
		if err := h.cfg.Restart(units); err != nil {
			h.log.Error("restart after update", "err", err)
		}
	}()
	return nil
}
