package agent

import (
	"os/exec"
	"strings"
	"testing"

	"encoding/base64"
	"encoding/pem"
	"github.com/wiefix/basalt/internal/agentproto"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
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

// TestGitEnvToken: a repository token reaches the git server as the
// x-access-token password, through the environment (never the command
// line).
func TestGitEnvToken(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	const token = "ghs_abcdefghijklmnopqrstuvwxyz0123"
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644)
	m := &appsManager{dataDir: t.TempDir(), gitCAFile: ca}
	spec := agentproto.AppSpec{AppID: 1, Git: &agentproto.GitSource{URL: srv.URL + "/acme/shop.git", Token: token}}
	env, err := m.gitEnv(spec, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gitBin, "ls-remote", spec.Git.URL)
	cmd.Env = env
	cmd.Run() // the fake has no repository
	if strings.Contains(strings.Join(cmd.Args, " "), token) {
		t.Fatal("the token is on the command line")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || seen[0] != want {
		t.Fatalf("authorization sent: %q", seen)
	}
	for _, bad := range []agentproto.GitSource{
		{URL: "https://github.com/a/b.git", Branch: "main", Dockerfile: "Dockerfile", Context: ".", Token: "short"},
		{URL: "https://github.com/a/b.git", Branch: "main", Dockerfile: "Dockerfile", Context: ".", Token: "has a space in it 123456789"},
		{URL: "git@github.com:a/b.git", Branch: "main", Dockerfile: "Dockerfile", Context: ".", Token: token},
	} {
		if bad.Validate() == nil {
			t.Errorf("valid: %+v", bad)
		}
	}
}
