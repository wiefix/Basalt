package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/sftpd"
	"github.com/wiefix/basalt/internal/siteworker"
	"github.com/wiefix/basalt/internal/sysuser"
)

// workerIdle is how long a site's file worker lives without requests (and
// without an upload in progress).
const workerIdle = 10 * time.Minute

// siteWorker gives the file manager access to one site. With isolation it
// is a "basalt site-worker" process of the site's user, so the requests of
// the file manager are carried out with the site user's rights, not root's;
// otherwise the same handler runs in the agent.
type siteWorker struct {
	local *siteworker.Files

	peer *agentproto.Peer
	cmd  *exec.Cmd
	done chan struct{}

	lastUsed time.Time
	uploads  int
}

func (w *siteWorker) alive() bool {
	if w.done == nil {
		return true
	}
	select {
	case <-w.done:
		return false
	default:
		return true
	}
}

// stop ends the worker; its uploads in progress are lost.
func (w *siteWorker) stop() {
	if w.local != nil {
		w.local.Close()
		return
	}
	w.cmd.Process.Kill()
	<-w.done
}

// handleFiles passes a file manager request to the site's worker.
func (m *sitesManager) handleFiles(ctx context.Context, req agentproto.FilesRequest) (*agentproto.FilesResponse, error) {
	w, err := m.worker(req.SiteID)
	if err != nil {
		return nil, err
	}
	var resp *agentproto.FilesResponse
	if w.local != nil {
		resp, err = w.local.Handle(ctx, req)
	} else {
		resp = &agentproto.FilesResponse{}
		err = w.peer.Call(ctx, siteworker.MsgFiles, req, resp)
		if re := (*agentproto.RemoteError)(nil); errors.As(err, &re) {
			err = errors.New(re.Msg) // the panel reads the worker's own message
		} else if err != nil {
			err = fmt.Errorf("site file worker: %w", err)
		}
	}
	m.mu.Lock()
	w.lastUsed = time.Now()
	switch req.Op {
	case agentproto.FilesUploadBegin:
		if err == nil {
			w.uploads++
		}
	case agentproto.FilesUploadCommit, agentproto.FilesUploadAbort:
		w.uploads = max(w.uploads-1, 0)
	}
	m.mu.Unlock()
	return resp, err
}

// worker is the running worker of a site, started if there is none.
func (m *sitesManager) worker(id int64) (*siteWorker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w := m.workers[id]; w != nil && w.alive() {
		return w, nil
	}
	w, err := m.startWorker(id)
	if err != nil {
		return nil, err
	}
	m.workers[id] = w
	return w, nil
}

func (m *sitesManager) startWorker(id int64) (*siteWorker, error) {
	now := time.Now()
	if !m.isolate {
		if _, err := m.siteDir(id); err != nil {
			return nil, err
		}
		return &siteWorker{local: siteworker.NewFiles(func() (*files.Site, error) { return m.open(id) }), lastUsed: now}, nil
	}
	cmd, err := m.workerCommand(context.Background(), id, siteworker.ModeFiles)
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start site file worker: %w", err)
	}
	w := &siteWorker{cmd: cmd, done: make(chan struct{}), lastUsed: now}
	w.peer = agentproto.NewPeer(stdout, stdin, nil, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("the agent serves no requests of a site worker")
	})
	go func() {
		err := w.peer.Run(context.Background())
		cmd.Process.Kill()
		werr := cmd.Wait()
		if !errors.Is(err, agentproto.ErrClosed) {
			m.log.Warn("site file worker ended", "site_id", id, "err", err, "exit", werr)
		}
		close(w.done)
	}()
	return w, nil
}

// siteDir is the directory of a site that exists on this server.
func (m *sitesManager) siteDir(id int64) (string, error) {
	d, err := m.dir(id)
	if err != nil {
		return "", err
	}
	if fi, err := os.Lstat(d); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("site %d does not exist on this server", id)
	}
	return d, nil
}

// workerCommand is a worker process for the site, as the site's user with
// no other group, an empty environment and in a session of its own (killed
// whole when ctx ends), that dies with the agent.
func (m *sitesManager) workerCommand(ctx context.Context, id int64, mode string) (*exec.Cmd, error) {
	d, err := m.siteDir(id)
	if err != nil {
		return nil, err
	}
	uid, gid, err := sysuser.LookupUser(sysuser.SiteUser(id))
	if err != nil {
		return nil, fmt.Errorf("site %d has no system user", id)
	}
	if m.workerExe == "" {
		return nil, errors.New("site file worker: agent executable unknown")
	}
	cmd := exec.CommandContext(ctx, m.workerExe, siteworker.Args(mode, d)...)
	// HOME only so that libraries in the binary (Caddy) find no reason to
	// complain at start; the worker reads and writes nothing there.
	cmd.Dir, cmd.Env = "/", []string{"HOME=/nonexistent"}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:     true,
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}},
	}
	dieWithParent(cmd.SysProcAttr)
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stderr = &workerLog{m: m, id: id}
	return cmd, nil
}

// workerLog puts what a worker says on stderr into the agent's log.
type workerLog struct {
	m  *sitesManager
	id int64
}

func (l *workerLog) Write(b []byte) (int, error) {
	if s := strings.TrimSpace(string(b)); s != "" {
		l.m.log.Warn("site worker", "site_id", l.id, "msg", tail(s, maxRunOutput))
	}
	return len(b), nil
}

// sweepWorkers stops workers left idle, and all of them when ctx is done.
func (m *sitesManager) sweepWorkers(all bool) {
	m.mu.Lock()
	var idle []*siteWorker
	for id, w := range m.workers {
		limit := workerIdle
		if w.uploads > 0 {
			limit = siteworker.UploadIdleTimeout + time.Minute
		}
		if all || !w.alive() || time.Since(w.lastUsed) > limit {
			idle = append(idle, w)
			delete(m.workers, id)
		} else if w.local != nil {
			w.local.Expire(siteworker.UploadIdleTimeout)
		}
	}
	m.mu.Unlock()
	for _, w := range idle {
		w.stop()
	}
}

// stopWorker ends the worker of a site (being deleted, say).
func (m *sitesManager) stopWorker(id int64) {
	m.mu.Lock()
	w := m.workers[id]
	delete(m.workers, id)
	m.mu.Unlock()
	if w != nil {
		w.stop()
	}
}

// serveSFTP runs an SFTP session: in a worker process of the site's user
// with isolation, so what the session does to files is limited by the
// kernel to what that user may do.
func (m *sitesManager) serveSFTP(ctx context.Context, id int64, ch io.ReadWriteCloser) error {
	if !m.isolate {
		site, err := m.open(id)
		if err != nil {
			return err
		}
		defer site.Close()
		return sftpd.ServeSFTP(site, ch)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, err := m.workerCommand(ctx, id, siteworker.ModeSFTP)
	if err != nil {
		return err
	}
	cmd.Stdout = ch
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start site SFTP worker: %w", err)
	}
	// The client closing the channel ends the worker's input; the worker
	// leaving ends the session (the caller closes ch, which ends the copy).
	go func() {
		io.Copy(stdin, ch)
		stdin.Close()
	}()
	return cmd.Wait()
}
