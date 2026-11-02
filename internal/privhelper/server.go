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

// maxConns bounds the helper's connections: the agent needs one, a few
// for a moment while it reconnects.
const maxConns = 8

// Serve answers requests on a unix socket at path until ctx ends. Only
// processes running as uid get answers: the kernel tells who is connected
// (SO_PEERCRED), on top of the socket's mode (0600, of the helper's user).
func Serve(ctx context.Context, path string, uid int, h agentproto.Handler, log *slog.Logger) error {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return fmt.Errorf("%s exists and is not a socket", path)
		}
		// Left by a helper that did not stop cleanly.
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return err
	}
	defer l.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if uid != os.Geteuid() && os.Geteuid() == 0 {
		// The agent's user owns the socket (still only it may connect).
		if err := os.Chown(path, uid, -1); err != nil {
			return err
		}
	}
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	log.Info("listening", "socket", path, "agent_uid", uid)
	slots := make(chan struct{}, maxConns)
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			log.Warn("accept", "err", err)
			continue
		}
		peer, err := peerUID(c)
		switch {
		case err != nil:
			log.Warn("refused a connection: unknown peer", "err", err)
			c.Close()
			continue
		case peer != uid:
			log.Warn("refused a connection from another user", "uid", peer)
			c.Close()
			continue
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
			p := agentproto.NewPeer(c, c, nil, h)
			if err := p.Run(ctx); err != nil && !errors.Is(err, agentproto.ErrClosed) && ctx.Err() == nil {
				log.Debug("agent connection ended", "err", err)
			}
		}()
	}
}
