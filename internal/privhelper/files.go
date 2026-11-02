package privhelper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"syscall"
)

// Some requests hand the helper open files: the pipe restic reads a
// database dump from or writes a snapshot's file to, the downloaded binary
// of an update. They travel with the connection as SCM_RIGHTS, so the
// helper reads and writes them directly (no copy through frames) and never
// opens a path the agent names.
//
// Such a request has a connection of its own, which starts with one byte,
// filesMarker, sent together with the files; the frames follow. A
// connection without files starts with a frame.

// filesMarker is the first byte of a connection that carries files.
const filesMarker = 'F'

// maxFiles bounds the files of one request.
const maxFiles = 2

type filesKey struct{}

// requestFiles returns the files that came with the connection of the
// request being served.
func requestFiles(ctx context.Context) []*os.File {
	f, _ := ctx.Value(filesKey{}).([]*os.File)
	return f
}

// receiveFiles reads the start of a connection: the files it carries, if
// any. The reader continues with the frames.
func receiveFiles(c *net.UnixConn) (io.Reader, []*os.File, error) {
	buf := make([]byte, 1)
	oob := make([]byte, syscall.CmsgSpace(maxFiles*4))
	n, oobn, flags, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, nil, err
	}
	if n == 0 {
		return nil, nil, io.EOF
	}
	var files []*os.File
	if oobn > 0 {
		msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return nil, nil, err
		}
		for _, m := range msgs {
			fds, err := syscall.ParseUnixRights(&m)
			if err != nil {
				continue
			}
			for _, fd := range fds {
				syscall.CloseOnExec(fd)
				files = append(files, os.NewFile(uintptr(fd), "agent-file"))
			}
		}
	}
	if flags&syscall.MSG_CTRUNC != 0 || len(files) > maxFiles || (len(files) > 0) != (buf[0] == filesMarker) {
		closeFiles(files)
		return nil, nil, errors.New("malformed files of a request")
	}
	if len(files) > 0 {
		return c, files, nil
	}
	return io.MultiReader(bytes.NewReader(buf[:n]), c), nil, nil
}

// sendFiles starts a connection with files.
func sendFiles(c *net.UnixConn, files []*os.File) error {
	if len(files) == 0 || len(files) > maxFiles {
		return fmt.Errorf("%d files for one request", len(files))
	}
	fds := make([]int, len(files))
	for i, f := range files {
		fds[i] = int(f.Fd())
	}
	_, _, err := c.WriteMsgUnix([]byte{filesMarker}, syscall.UnixRights(fds...), nil)
	runtime.KeepAlive(files)
	return err
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}
