package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
)

const (
	maxDockerfile  = 1 << 20
	buildTailLines = 40
	// Builds are heavy; more than this many on one host wait.
	maxConcurrentBuilds = 2
)

// buildImage produces spec.Image for a git app: it reuses the image of an
// earlier deployment of the same commit when that is still on the host,
// and otherwise clones the repository and builds its Dockerfile with the
// docker CLI (BuildKit). It returns the commit built and its subject line.
func (m *appsManager) buildImage(ctx context.Context, dc *docker.Client, spec agentproto.AppSpec, lg *deployLog) (commit, subject string, err error) {
	g := spec.Git
	if g.ReuseImage != "" && g.Commit != "" {
		if _, err := dc.ImageInspect(ctx, g.ReuseImage); err == nil {
			lg.printf("Reusing the image built from commit %s", short(g.Commit))
			return g.Commit, "", dc.Tag(ctx, g.ReuseImage, spec.Image)
		}
		lg.printf("The image of commit %s was removed; building it again", short(g.Commit))
	}
	dockerBin, err := exec.LookPath("docker")
	if err != nil {
		return "", "", errors.New("building needs the docker command line tool on this server (apt install docker.io docker-buildx)")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return "", "", errors.New("building needs git on this server (apt install git)")
	}
	select {
	case m.builds <- struct{}{}:
		defer func() { <-m.builds }()
	default:
		lg.printf("Waiting for another build on this server to finish")
		select {
		case m.builds <- struct{}{}:
			defer func() { <-m.builds }()
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}

	work := filepath.Join(m.dataDir, "builds", fmt.Sprintf("app-%d-d%d", spec.AppID, spec.DeploymentID))
	os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return "", "", err
	}
	defer os.RemoveAll(work)
	src := filepath.Join(work, "src")
	home := filepath.Join(work, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", "", err
	}

	env, err := m.gitEnv(spec, work, home)
	if err != nil {
		return "", "", err
	}
	run := func(name string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env, cmd.Dir, cmd.WaitDelay = env, work, 5*time.Second
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%s: %s", strings.TrimSpace(lastLines(string(out), 3)), err)
		}
		return strings.TrimSpace(string(out)), nil
	}

	target := g.Branch
	if g.Commit != "" {
		target = g.Commit
	}
	lg.printf("Fetching %s from %s", target, g.URL)
	if _, err := run(gitBin, "init", "-q", src); err != nil {
		return "", "", fmt.Errorf("git init: %w", err)
	}
	if _, err := run(gitBin, "-C", src, "remote", "add", "--", "origin", g.URL); err != nil {
		return "", "", fmt.Errorf("git remote: %w", err)
	}
	if _, err := run(gitBin, "-C", src, "fetch", "-q", "--depth", "1", "--no-tags", "origin", target); err != nil {
		if g.Commit == "" {
			return "", "", fmt.Errorf("fetch %s: %w%s", target, err, accessHint(g))
		}
		// Some servers refuse fetching a commit by hash: fetch the branch.
		if _, err := run(gitBin, "-C", src, "fetch", "-q", "--no-tags", "origin", g.Branch); err != nil {
			return "", "", fmt.Errorf("fetch %s: %w%s", g.Branch, err, accessHint(g))
		}
		target = g.Commit
	} else {
		target = "FETCH_HEAD"
	}
	if _, err := run(gitBin, "-C", src, "checkout", "-q", "--detach", target); err != nil {
		return "", "", fmt.Errorf("checkout: %w", err)
	}
	if commit, err = run(gitBin, "-C", src, "rev-parse", "HEAD"); err != nil {
		return "", "", err
	}
	subject, _ = run(gitBin, "-C", src, "log", "-1", "--format=%s")
	subject = truncateRunes(subject, 200)
	lg.printf("Commit %s %s", short(commit), subject)

	// Paths from the repository are resolved inside it only: a symlink in
	// the repository must not make the build read files of this host.
	root, err := os.OpenRoot(src)
	if err != nil {
		return "", "", err
	}
	defer root.Close()
	if fi, err := root.Stat(g.Context); err != nil || !fi.IsDir() {
		return "", "", fmt.Errorf("build context %s is not a directory in the repository", g.Context)
	}
	if fi, err := root.Lstat(g.Context); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("build context %s must not be a symlink", g.Context)
	}
	cfg, err := writeDockerConfig(filepath.Join(work, "docker"), g.RegistryAuths)
	if err != nil {
		return "", "", err
	}
	if g.Builder == agentproto.BuilderRailpack {
		if err := m.railpackBuild(ctx, dc, spec, lg, dockerBin, filepath.Join(src, g.Context), work, home, cfg, commit); err != nil {
			return "", "", err
		}
		lg.printf("Image built")
		return commit, subject, nil
	}
	dockerfile, err := readLimited(root, g.Dockerfile, maxDockerfile)
	if err != nil {
		return "", "", fmt.Errorf("Dockerfile %s: %w", g.Dockerfile, err)
	}

	lg.printf("Building %s from %s", spec.Image, g.Dockerfile)
	cmd := exec.CommandContext(ctx, dockerBin, "--host", "unix://"+m.socket, "build",
		"--progress=plain", "--file", "-", "--tag", spec.Image,
		"--label", labelApp+"="+strconv.FormatInt(spec.AppID, 10), "--label", "basalt.commit="+commit,
		filepath.Join(src, g.Context))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "DOCKER_CONFIG=" + cfg,
		"DOCKER_BUILDKIT=1", "BUILDX_NO_DEFAULT_ATTESTATIONS=1"}
	cmd.Dir, cmd.WaitDelay = work, 10*time.Second
	cmd.Stdin = strings.NewReader(dockerfile)
	out := &buildOutput{lg: lg}
	cmd.Stdout, cmd.Stderr = out, out
	err = cmd.Run()
	out.flush()
	if err != nil {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		if lg.full() {
			lg.forceWrite("Last lines of the build:\n" + strings.Join(out.tail, "\n") + "\n")
		}
		return "", "", fmt.Errorf("the build failed (%v)", err)
	}
	lg.printf("Image built")
	return commit, subject, nil
}

// gitEnv is the environment git runs with: only https and ssh transports,
// no prompts, no system or user configuration, and for ssh the app's
// deploy key with the host keys first seen for the app.
func (m *appsManager) gitEnv(spec agentproto.AppSpec, work, home string) ([]string, error) {
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home,
		"GIT_ALLOW_PROTOCOL=https:ssh", "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
	}
	if m.gitCAFile != "" {
		env = append(env, "GIT_SSL_CAINFO="+m.gitCAFile)
	}
	if t := spec.Git.Token; t != "" {
		// Through the environment, not the command line: the token is not
		// in the process list. GitHub takes it as x-access-token.
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + t))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic "+auth)
	}
	knownHosts := filepath.Join(m.dataDir, "known_hosts")
	if err := os.MkdirAll(knownHosts, 0o700); err != nil {
		return nil, err
	}
	// Host keys are trusted on first use, per app.
	ssh := []string{"ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + filepath.Join(knownHosts, "app-"+strconv.FormatInt(spec.AppID, 10)),
		"-o", "ConnectTimeout=20", "-o", "IdentitiesOnly=yes", "-F", os.DevNull}
	if spec.Git.DeployKey != "" {
		key := filepath.Join(work, "deploy_key")
		k := spec.Git.DeployKey
		if !strings.HasSuffix(k, "\n") {
			k += "\n"
		}
		if err := os.WriteFile(key, []byte(k), 0o600); err != nil {
			return nil, err
		}
		ssh = append(ssh, "-i", key)
	}
	quoted := make([]string, len(ssh))
	for i, a := range ssh {
		quoted[i] = shellQuote(a) // git runs GIT_SSH_COMMAND through the shell
	}
	return append(env, "GIT_SSH_COMMAND="+strings.Join(quoted, " ")), nil
}

// accessHint is added to fetch errors of SSH repositories, which most often
// fail because the deploy key was not added.
func accessHint(g *agentproto.GitSource) string {
	if agentproto.IsSSHGitURL(g.URL) {
		return " (check that the app's deploy key is added to the repository)"
	}
	if g.Token != "" {
		return " (check that the GitHub App is still installed on the repository)"
	}
	return ""
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// writeDockerConfig creates a DOCKER_CONFIG directory holding the registry
// credentials offered to the build.
func writeDockerConfig(dir string, auths []docker.AuthConfig) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	creds := map[string]map[string]string{}
	for _, a := range auths {
		creds[a.ServerAddress] = map[string]string{
			"auth": base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password)),
		}
	}
	// The CLI finds buildx in its config directory too; this one replaces
	// the user's, so point at where the plugins really are.
	var plugins []string
	for _, d := range []string{os.Getenv("DOCKER_CONFIG"), filepath.Join(os.Getenv("HOME"), ".docker")} {
		if d != "" && d != dir {
			if fi, err := os.Stat(filepath.Join(d, "cli-plugins")); err == nil && fi.IsDir() {
				plugins = append(plugins, filepath.Join(d, "cli-plugins"))
			}
		}
	}
	cfg := map[string]any{"auths": creds, "cliPluginsExtraDirs": plugins}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return dir, os.WriteFile(filepath.Join(dir, "config.json"), b, 0o600)
}

func readLimited(root *os.Root, name string, limit int64) (string, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > limit {
		return "", errors.New("file too large")
	}
	return string(b), nil
}

// buildOutput copies build output into the deployment log line by line
// and remembers the last lines for when the log is already full.
type buildOutput struct {
	lg      *deployLog
	partial []byte
	tail    []string
}

func (b *buildOutput) Write(p []byte) (int, error) {
	b.partial = append(b.partial, p...)
	for {
		i := strings.IndexByte(string(b.partial), '\n')
		if i < 0 {
			break
		}
		b.line(string(b.partial[:i]))
		b.partial = b.partial[i+1:]
	}
	if len(b.partial) > 64<<10 { // a line without end
		b.line(string(b.partial))
		b.partial = nil
	}
	return len(p), nil
}

func (b *buildOutput) line(s string) {
	s = strings.TrimRight(s, "\r")
	b.lg.write("  " + s + "\n")
	b.tail = append(b.tail, s)
	if len(b.tail) > buildTailLines {
		b.tail = b.tail[1:]
	}
}

func (b *buildOutput) flush() {
	if len(b.partial) > 0 {
		b.line(string(b.partial))
		b.partial = nil
	}
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

func lastLines(s string, n int) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
