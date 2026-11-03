package privhelper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wiefix/basalt/internal/php"
	"github.com/wiefix/basalt/internal/siteworker"
	"github.com/wiefix/basalt/internal/sysuser"
)

// MsgSiteProcess runs a program as a site's user (SiteProcess →
// SiteProcessResult) on the files that come with the request: the first is
// its standard input and output (a socket), the second, if any, its
// standard error (else the first). The program is the site's file worker
// or the site's PHP CLI, nothing else; the request lasts as long as the
// program, and stopping the request kills it.
const MsgSiteProcess = "site_process"

// SiteProcess names the program: a site worker in mode Worker, or else PHP
// version PHP with Args, Env and the working folder Dir (relative to the
// site's folder).
type SiteProcess struct {
	SiteID int64    `json:"site_id"`
	Worker string   `json:"worker,omitempty"`
	PHP    string   `json:"php,omitempty"`
	Dir    string   `json:"dir,omitempty"`
	Env    []string `json:"env,omitempty"`
	Args   []string `json:"args,omitempty"`
}

// SiteProcessResult is how the program ended (exit code, -1 when killed).
type SiteProcessResult struct {
	Exit int `json:"exit"`
}

// SocketPair makes the two ends of a stream socket: the caller keeps one
// and hands the other to a SiteProcess.
func SocketPair() (*os.File, *os.File, error) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	return os.NewFile(uintptr(fds[0]), "site-process"), os.NewFile(uintptr(fds[1]), "site-process"), nil
}

// processWait is how long a killed program's output may still be read.
const processWait = 10 * time.Second

// SiteProcess runs the program on stdio (and stderr, may be nil) until it
// ends or ctx does.
func (h *Helper) SiteProcess(ctx context.Context, req SiteProcess, stdio, stderr *os.File) (SiteProcessResult, error) {
	res := SiteProcessResult{Exit: -1}
	if stdio == nil {
		return res, errors.New("a site process needs its standard input and output")
	}
	cmd, err := h.siteCommand(req)
	if err != nil {
		return res, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdio, stdio, stdio
	if stderr != nil {
		cmd.Stderr = stderr
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	dieWithParent(cmd.SysProcAttr)
	if err := cmd.Start(); err != nil {
		return res, fmt.Errorf("start %s: %w", filepath.Base(cmd.Path), err)
	}
	// Killed whole (its session) when the request stops.
	stop := context.AfterFunc(ctx, func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	err = cmd.Wait()
	stop()
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		res.Exit = 0
	case errors.As(err, &ee):
		res.Exit = ee.ExitCode()
	default:
		return res, err
	}
	return res, nil
}

// siteCommand is the program of req, as the site's user with isolation
// (its own group only, an environment of nothing but req.Env).
func (h *Helper) siteCommand(req SiteProcess) (*exec.Cmd, error) {
	if req.SiteID <= 0 {
		return nil, errSiteID
	}
	site := filepath.Join(h.cfg.SitesRoot, strconv.FormatInt(req.SiteID, 10))
	if fi, err := os.Lstat(site); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("site %d does not exist on this server", req.SiteID)
	}
	for _, s := range append(append([]string{}, req.Env...), req.Args...) {
		if strings.ContainsRune(s, 0) {
			return nil, errors.New("invalid argument")
		}
	}
	for _, e := range req.Env {
		if k, _, ok := strings.Cut(e, "="); !ok || k == "" {
			return nil, fmt.Errorf("invalid environment variable %q", e)
		}
	}
	var cmd *exec.Cmd
	switch {
	case req.Worker != "":
		if req.Worker != siteworker.ModeFiles && req.Worker != siteworker.ModeSFTP {
			return nil, fmt.Errorf("unknown site worker %q", req.Worker)
		}
		if req.PHP != "" || req.Dir != "" || len(req.Env) > 0 || len(req.Args) > 0 {
			return nil, errors.New("a site worker takes no program, folder, environment or arguments")
		}
		if h.cfg.WorkerExe == "" {
			return nil, errors.New("site worker: executable unknown")
		}
		cmd = exec.Command(h.cfg.WorkerExe, siteworker.Args(req.Worker, site)...)
		// HOME only so that libraries in the binary (Caddy) find no reason
		// to complain at start; the worker reads and writes nothing there.
		cmd.Dir, cmd.Env = "/", []string{"HOME=/nonexistent"}
	case php.ValidVersion(req.PHP):
		dir := site
		if req.Dir != "" {
			c := filepath.Clean(req.Dir)
			if filepath.IsAbs(c) || c == ".." || strings.HasPrefix(c, "../") {
				return nil, errors.New("the working folder must be inside the site's folder")
			}
			dir = filepath.Join(site, c)
		}
		cmd = exec.Command(h.cfg.PHPBinary(req.PHP), req.Args...)
		// The site's user changes into it (after taking the site's
		// identity): no further than it could go on its own.
		cmd.Dir, cmd.Env = dir, req.Env
	default:
		return nil, fmt.Errorf("invalid PHP version %q", req.PHP)
	}
	if h.cfg.Isolate {
		uid, gid, err := sysuser.LookupUser(sysuser.SiteUser(req.SiteID))
		if err != nil {
			return nil, fmt.Errorf("site %d has no system user", req.SiteID)
		}
		// No supplementary groups: the site's own group only.
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
	}
	cmd.WaitDelay = processWait
	return cmd, nil
}
