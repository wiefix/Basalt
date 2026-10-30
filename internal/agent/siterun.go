package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/wiefix/basalt/internal/sysuser"
)

// maxRunOutput is how much of a program's output an error carries.
const maxRunOutput = 2000

// runAsSite runs a program as the site's system user (with isolation; as
// the agent otherwise) in dir, with nothing but env, in a session of its
// own that goes whole when ctx ends.
func (m *sitesManager) runAsSite(ctx context.Context, id int64, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if m.isolate {
		uid, gid, err := sysuser.LookupUser(sysuser.SiteUser(id))
		if err != nil {
			return fmt.Errorf("site %d has no system user", id)
		}
		// No supplementary groups: the site's own group only.
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}
	}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%s did not finish in time", name)
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
