package siteworker

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/files"
	"github.com/wiefix/basalt/internal/sftpd"
)

const (
	// Command is the basalt subcommand of a worker.
	Command = "site-worker"
	// ModeFiles serves file requests (agentproto frames, message MsgFiles)
	// on stdin and stdout; ModeSFTP an SFTP session.
	ModeFiles = "files"
	ModeSFTP  = "sftp"

	MsgFiles = "files"
)

// Args is the command line (after the binary) of a worker for the site in
// dir.
func Args(mode, dir string) []string { return []string{Command, mode, "--dir", dir} }

// Main runs a worker: args are those after Command. It never runs as root:
// a worker exists to have the kernel confine it to the site's user.
func Main(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if err := run(args, stdin, stdout); err != nil {
		fmt.Fprintln(stderr, "site worker:", err)
		return 1
	}
	return 0
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("mode required (files or sftp)")
	}
	mode := args[0]
	fl := flag.NewFlagSet(Command, flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	dir := fl.String("dir", "", "site directory")
	if err := fl.Parse(args[1:]); err != nil {
		return err
	}
	if *dir == "" || fl.NArg() > 0 {
		return errors.New("usage: site-worker files|sftp --dir DIR")
	}
	if os.Geteuid() == 0 {
		return errors.New("refusing to run as root")
	}
	switch mode {
	case ModeFiles:
		return serveFiles(*dir, stdin, stdout)
	case ModeSFTP:
		site, err := files.Open(*dir)
		if err != nil {
			return files.PublicError(err)
		}
		defer site.Close()
		return sftpd.ServeSFTP(site, stdio{stdin, stdout})
	}
	return fmt.Errorf("unknown mode %q", mode)
}

type stdio struct {
	io.Reader
	io.Writer
}

func (stdio) Close() error { return nil }

func serveFiles(dir string, stdin io.Reader, stdout io.Writer) error {
	f := NewFiles(func() (*files.Site, error) {
		s, err := files.Open(dir)
		return s, files.PublicError(err)
	})
	defer f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				f.Expire(UploadIdleTimeout)
			}
		}
	}()
	peer := agentproto.NewPeer(stdin, stdout, nil, func(ctx context.Context, msgType string, payload json.RawMessage) (any, error) {
		if msgType != MsgFiles {
			return nil, fmt.Errorf("unknown message %q", msgType)
		}
		var req agentproto.FilesRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		return f.Handle(ctx, req)
	})
	err := peer.Run(ctx)
	if errors.Is(err, agentproto.ErrClosed) || errors.Is(err, io.EOF) {
		return nil // the agent went away or closed us
	}
	return err
}
