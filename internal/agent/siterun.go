package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/privhelper"
)

// maxRunOutput is how much of a program's output an error carries.
const maxRunOutput = 2000

// runSitePHPCLI runs the site's PHP CLI (php<version> args) as the site's
// system user (with isolation; as the agent otherwise) in dir, a folder of
// the site, with nothing but env and stdin: the privileged helper starts
// it, in a session of its own that goes whole when ctx ends.
func (m *sitesManager) runSitePHPCLI(ctx context.Context, id int64, dir string, env []string, stdin, version string, args ...string) error {
	site, err := m.siteDir(id)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(site, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return errors.New("the working folder must be inside the site's folder")
	}
	conn, done, err := m.startProcess(ctx, id, privhelper.SiteProcess{SiteID: id, PHP: version, Dir: rel, Env: env, Args: args})
	if err != nil {
		return fmt.Errorf("start php%s: %w", version, err)
	}
	defer conn.Close()
	go func() {
		io.Copy(conn, strings.NewReader(stdin))
		conn.CloseWrite()
	}()
	var out bytes.Buffer
	read := make(chan struct{})
	go func() {
		io.Copy(&out, io.LimitReader(conn, 1<<20))
		io.Copy(io.Discard, conn)
		close(read)
	}()
	err = <-done
	// The program is gone (conn closed): what it wrote is all there.
	select {
	case <-read:
	case <-time.After(5 * time.Second): // a child of it holds the socket
		conn.Close()
		<-read
	}
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("php%s did not finish in time", version)
		}
		return fmt.Errorf("%w: %s", err, tail(out.String(), maxRunOutput))
	}
	return nil
}

// tail is the end of s, at most n bytes, from a line start when it can.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return "…" + s
}
