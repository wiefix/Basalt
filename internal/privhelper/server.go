package privhelper

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"

	"github.com/wiefix/basalt/internal/agentproto"
)

// maxConns bounds the helper's connections: the agent keeps one, and opens
// one for each restic command (two backups, browsing and downloads of
// backups, integrity checks) and each update.
const maxConns = 32

// Serve answers requests on a unix socket at path until ctx ends. Only
// processes running as uid get answers: the kernel tells who is connected
// (SO_PEERCRED), on top of the socket's mode (0600, of the helper's user).
func Serve(ctx context.Context, path string, uid int, h agentproto.Handler, log *slog.Logger) error {
	l, err := listen(path, uid, log)
	if err != nil {
		return err
	}
	defer l.Close()
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	log.Info("listening", "socket", path, "agent_uid", uid)
	slots := make(chan struct{}, maxConns)
	for {
		c, err := l.accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			log.Warn("refused a connection: too many")
			c.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			defer c.Close()
			r, files, err := receiveFiles(c)
			if err != nil {
				log.Debug("agent connection ended", "err", err)
				return
			}
			defer closeFiles(files)
			p := agentproto.NewPeer(r, c, nil, h)
			if err := p.Run(context.WithValue(ctx, filesKey{}, files)); err != nil && !errors.Is(err, agentproto.ErrClosed) && ctx.Err() == nil {
				log.Debug("agent connection ended", "err", err)
			}
		}()
	}
}

// listen opens a unix socket at path that only uid may use: mode 0600,
// owned by uid; a socket left there is replaced.
func listen(path string, uid int, log *slog.Logger) (*peerListener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		// Left by a helper that did not stop cleanly.
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	if uid != os.Geteuid() && os.Geteuid() == 0 {
		// The agent's user owns the socket (still only it may connect).
		if err := os.Chown(path, uid, -1); err != nil {
			l.Close()
			return nil, err
		}
	}
	return &peerListener{UnixListener: l, uid: uid, log: log}, nil
}

// peerListener accepts connections of processes running as uid only: the
// kernel tells who is connected (SO_PEERCRED), on top of the socket's mode.
type peerListener struct {
	*net.UnixListener
	uid int
	log *slog.Logger
}

func (l *peerListener) accept() (*net.UnixConn, error) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil, err
			}
			l.log.Warn("accept", "err", err)
			continue
		}
		peer, err := peerUID(c)
		switch {
		case err != nil:
			l.log.Warn("refused a connection: unknown peer", "err", err)
		case peer != l.uid:
			l.log.Warn("refused a connection from another user", "uid", peer)
		default:
			return c, nil
		}
		c.Close()
	}
}

// Accept makes it a net.Listener (for an HTTP server).
func (l *peerListener) Accept() (net.Conn, error) { return l.accept() }
