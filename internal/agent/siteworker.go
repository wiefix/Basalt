package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/privhelper"
	"github.com/wiefix/basalt/internal/sftpd"
	"github.com/wiefix/basalt/internal/siteworker"
)

// workerIdle is how long a site's file worker lives without requests (and
// without an upload in progress).
const workerIdle = 10 * time.Minute

// siteWorker gives the agent access to one site's files: the file
// manager's requests and the agent's own work in the site's folder (copies
// between servers, applications put in place, backups restored as copies).
// With isolation it is a "basalt site-worker" process of the site's user,
// started by the privileged helper, so all of it is carried out with the
// site user's rights; otherwise the same handler runs in the agent.
type siteWorker struct {
	local *siteworker.Handler

	peer   *agentproto.Peer
	cancel context.CancelFunc
	done   chan struct{}

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
	w.cancel()
	<-w.done
}

// call sends a request to the worker (a stream when fn is set: its frames
// go to fn).
func (w *siteWorker) call(ctx context.Context, msgType string, req, resp any, fn func(json.RawMessage) error) error {
	if w.local == nil {
		var err error
		if fn != nil {
			err = w.peer.Stream(ctx, msgType, req, resp, fn)
		} else {
			err = w.peer.Call(ctx, msgType, req, resp)
		}
		if re := (*agentproto.RemoteError)(nil); errors.As(err, &re) {
			return errors.New(re.Msg) // the panel reads the worker's own message
		} else if err != nil {
			return fmt.Errorf("site file worker: %w", err)
		}
		return nil
	}
	// In this process: the same handler, through JSON as over the pipe.
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if fn != nil {
		ctx = agentproto.WithStreamSender(ctx, func(v any) error {
			b, err := json.Marshal(v)
			if err != nil {
				return err
			}
			return fn(b)
		})
	}
	out, err := w.local.Handle(ctx, msgType, payload)
	if err != nil {
		return err
	}
	if resp == nil {
		return nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, resp)
}

// callSite sends a request to the site's worker.
func (m *sitesManager) callSite(ctx context.Context, id int64, msgType string, req, resp any, fn func(json.RawMessage) error) error {
	w, err := m.worker(id)
	if err != nil {
		return err
	}
	err = w.call(ctx, msgType, req, resp, fn)
	m.mu.Lock()
	w.lastUsed = time.Now()
	m.mu.Unlock()
	return err
}

// handleFiles passes a file manager request to the site's worker.
func (m *sitesManager) handleFiles(ctx context.Context, req agentproto.FilesRequest) (*agentproto.FilesResponse, error) {
	w, err := m.worker(req.SiteID)
	if err != nil {
		return nil, err
	}
	resp := &agentproto.FilesResponse{}
	err = w.call(ctx, siteworker.MsgFiles, req, resp, nil)
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
	if _, err := m.siteDir(id); err != nil {
		return nil, err
	}
	if !m.isolate {
		return &siteWorker{local: siteworker.NewHandler(func() (*files.Site, error) { return m.open(id) }), lastUsed: now}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn, done, err := m.startProcess(ctx, id, privhelper.SiteProcess{SiteID: id, Worker: siteworker.ModeFiles})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("start site file worker: %w", err)
	}
	w := &siteWorker{cancel: cancel, done: make(chan struct{}), lastUsed: now}
	w.peer = agentproto.NewPeer(conn, conn, nil, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("the agent serves no requests of a site worker")
	})
	go func() {
		err := w.peer.Run(context.Background())
		cancel()
		conn.Close()
		perr := <-done
		if !errors.Is(err, agentproto.ErrClosed) {
			m.log.Warn("site file worker ended", "site_id", id, "err", err, "exit", perr)
		}
		close(w.done)
	}()
	return w, nil
}

// siteDir is the folder of a site that exists on this server.
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

// open gives the agent's own process access to a site's files: only
// without isolation (with it, they belong to the site's user and its
// worker reaches them).
func (m *sitesManager) open(id int64) (*files.Site, error) {
	d, err := m.dir(id)
	if err != nil {
		return nil, err
	}
	s, err := files.Open(d)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("site %d does not exist on this server", id)
	}
	return s, err
}

// startProcess has the helper run a program as the site's user on a
// socket whose other end it returns; what the program writes on standard
// error goes to the agent's log. done tells how the program ended (it ends
// with ctx); conn then reads to the end of what it wrote. The caller
// closes conn.
func (m *sitesManager) startProcess(ctx context.Context, id int64, req privhelper.SiteProcess) (*net.UnixConn, <-chan error, error) {
	mine, theirs, err := privhelper.SocketPair()
	if err != nil {
		return nil, nil, err
	}
	c, err := net.FileConn(mine)
	mine.Close()
	if err != nil {
		theirs.Close()
		return nil, nil, err
	}
	conn := c.(*net.UnixConn)
	errR, errW, err := os.Pipe()
	if err != nil {
		theirs.Close()
		conn.Close()
		return nil, nil, err
	}
	go func() {
		defer errR.Close()
		io.Copy(&workerLog{m: m, id: id}, errR)
	}()
	done := make(chan error, 1)
	go func() {
		res, err := m.priv.SiteProcess(ctx, req, theirs, errW)
		// Its ends of the socket and of the pipe are the program's alone:
		// with them closed, the agent's ends see the program leave.
		theirs.Close()
		errW.Close()
		if err == nil && res.Exit != 0 {
			err = fmt.Errorf("exit status %d", res.Exit)
		}
		done <- err
	}()
	return conn, done, nil
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
	if _, err := m.siteDir(id); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, done, err := m.startProcess(ctx, id, privhelper.SiteProcess{SiteID: id, Worker: siteworker.ModeSFTP})
	if err != nil {
		return fmt.Errorf("start site SFTP worker: %w", err)
	}
	defer conn.Close()
	// The client closing the channel ends the worker's input; the worker
	// leaving ends the session (conn closes, which ends the copy to the
	// client; the caller closes ch).
	go func() {
		io.Copy(conn, ch)
		conn.CloseWrite()
	}()
	io.Copy(ch, conn)
	return <-done
}
