package agent

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/wiefix/basalt/internal/agentproto"
)

func TestGitEnvQuotesSSHCommand(t *testing.T) {
	m := &appsManager{dataDir: t.TempDir() + "/it's here"}
	work := t.TempDir()
	env, err := m.gitEnv(agentproto.AppSpec{AppID: 7, Git: &agentproto.GitSource{DeployKey: "KEY"}}, work, work)
	if err != nil {
		t.Fatal(err)
	}
	var sshCmd string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "GIT_SSH_COMMAND="); ok {
			sshCmd = v
		}
		if strings.HasPrefix(e, "GIT_ALLOW_PROTOCOL=") && e != "GIT_ALLOW_PROTOCOL=https:ssh" {
			t.Fatalf("protocols: %s", e)
		}
	}
	// The shell must see the paths as single arguments, quote included.
	out, err := exec.Command("sh", "-c", `for a in `+strings.TrimPrefix(sshCmd, "'ssh' ")+`; do echo "[$a]"; done`).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "/it's here/known_hosts/app-7]") || !strings.Contains(string(out), "deploy_key]") {
		t.Fatalf("ssh arguments as the shell sees them:\n%s", out)
	}
}

func TestBuildOutputKeepsTail(t *testing.T) {
	var lg deployLog
	out := &buildOutput{lg: &lg}
	for i := range 100 {
		out.Write([]byte("step " + strings.Repeat("x", i%3) + "\n"))
	}
	out.Write([]byte("no newline at the end"))
	out.flush()
	if len(out.tail) != buildTailLines || out.tail[len(out.tail)-1] != "no newline at the end" {
		t.Fatalf("tail = %d lines, last %q", len(out.tail), out.tail[len(out.tail)-1])
	}
	if !strings.Contains(lg.String(), "  no newline at the end\n") {
		t.Fatal("partial line lost")
	}
}
