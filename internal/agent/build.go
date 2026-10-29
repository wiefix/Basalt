package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
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
	dockerBin, err := dockerCLI()
	if err != nil {
		return "", "", err
	}
	release, err := m.buildSlot(ctx, lg)
	if err != nil {
		return "", "", err
	}
	defer release()
	co, err := m.checkout(ctx, spec, fmt.Sprintf("d%d", spec.DeploymentID), lg)
	if err != nil {
		return "", "", err
	}
	defer co.close()
	if err := co.contextDir(g.Context); err != nil {
		return "", "", err
	}
	cfg, err := writeDockerConfig(filepath.Join(co.work, "docker"), g.RegistryAuths)
	if err != nil {
		return "", "", err
	}
	if g.Builder == agentproto.BuilderRailpack {
		if err := m.railpackBuild(ctx, dc, spec, lg, dockerBin, filepath.Join(co.src, g.Context), co.work, co.home, cfg, co.commit); err != nil {
			return "", "", err
		}
		lg.printf("Image built")
		return co.commit, co.subject, nil
	}
	dockerfile, err := readLimited(co.root, g.Dockerfile, maxDockerfile)
	if err != nil {
		return "", "", fmt.Errorf("Dockerfile %s: %w", g.Dockerfile, err)
	}
	lg.printf("Building %s from %s", spec.Image, g.Dockerfile)
	err = m.dockerBuild(ctx, co, dockerBin, cfg, dockerBuildOptions{
		image: spec.Image, dockerfile: dockerfile, context: g.Context, secrets: g.BuildEnv,
		labels: []string{labelApp + "=" + strconv.FormatInt(spec.AppID, 10), "basalt.commit=" + co.commit},
	}, lg)
	if err != nil {
		return "", "", err
	}
	lg.printf("Image built")
	return co.commit, co.subject, nil
}

// buildServices builds the images of the services of a compose app from
// git that build theirs (all from one checkout of spec.Git.Commit), reusing
// the image an earlier deployment built when it is still on the host. It
// returns the commit built (empty when nothing was) and its subject line.
func (m *appsManager) buildServices(ctx context.Context, dc *docker.Client, spec agentproto.AppSpec, lg *deployLog) (commit, subject string, err error) {
	g := spec.Git
	var todo []agentproto.ServiceSpec
	for _, svc := range spec.Services {
		b := svc.Build
		if b == nil {
			continue
		}
		if b.ReuseImage != "" {
			if _, err := dc.ImageInspect(ctx, b.ReuseImage); err == nil {
				lg.printf("Service %s: reusing the image built from commit %s", svc.Name, short(g.Commit))
				if err := dc.Tag(ctx, b.ReuseImage, svc.Image); err != nil {
					return "", "", err
				}
				continue
			}
		}
		todo = append(todo, svc)
	}
	if len(todo) == 0 {
		return g.Commit, "", nil
	}
	dockerBin, err := dockerCLI()
	if err != nil {
		return "", "", err
	}
	release, err := m.buildSlot(ctx, lg)
	if err != nil {
		return "", "", err
	}
	defer release()
	co, err := m.checkout(ctx, spec, fmt.Sprintf("d%d", spec.DeploymentID), lg)
	if err != nil {
		return "", "", err
	}
	defer co.close()
	cfg, err := writeDockerConfig(filepath.Join(co.work, "docker"), g.RegistryAuths)
	if err != nil {
		return "", "", err
	}
	for _, svc := range todo {
		b := svc.Build
		if err := co.contextDir(b.Context); err != nil {
			return "", "", fmt.Errorf("service %s: %w", svc.Name, err)
		}
		dockerfile, err := readLimited(co.root, b.Dockerfile, maxDockerfile)
		if err != nil {
			return "", "", fmt.Errorf("service %s: Dockerfile %s: %w", svc.Name, b.Dockerfile, err)
		}
		for k := range b.Args {
			if _, ok := g.BuildEnv[k]; ok {
				return "", "", fmt.Errorf("service %s: %s is both a build argument and a variable at build time", svc.Name, k)
			}
		}
		lg.printf("Building service %s from %s", svc.Name, b.Dockerfile)
		// No commit label: a service whose build did not change keeps its
		// image ID, and so its container.
		err = m.dockerBuild(ctx, co, dockerBin, cfg, dockerBuildOptions{
			image: svc.Image, dockerfile: dockerfile, context: b.Context, target: b.Target, args: b.Args,
			noCache: b.NoCache, pull: b.Pull, secrets: g.BuildEnv,
			labels: []string{labelApp + "=" + strconv.FormatInt(spec.AppID, 10)},
		}, lg)
		if err != nil {
			return "", "", fmt.Errorf("service %s: %w", svc.Name, err)
		}
	}
	lg.printf("Images built")
	return co.commit, co.subject, nil
}

// ComposeFile reads the compose file of a git app at the head of its
// branch (or at the commit asked for).
func (m *appsManager) ComposeFile(ctx context.Context, req agentproto.ComposeFileRequest) (agentproto.ComposeFileResponse, error) {
	if err := req.Validate(); err != nil {
		return agentproto.ComposeFileResponse{}, err
	}
	var name [6]byte
	rand.Read(name[:])
	var lg deployLog
	co, err := m.checkout(ctx, agentproto.AppSpec{AppID: req.AppID, Git: req.Git}, "f"+hex.EncodeToString(name[:]), &lg)
	if err != nil {
		return agentproto.ComposeFileResponse{}, err
	}
	defer co.close()
	content, err := readLimited(co.root, req.Git.ComposeFile, agentproto.MaxComposeFile)
	if errors.Is(err, fs.ErrNotExist) {
		err = fmt.Errorf("%s is not in the repository", req.Git.ComposeFile)
		for _, alt := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
			if fi, e := co.root.Lstat(path.Join(path.Dir(req.Git.ComposeFile), alt)); e == nil && fi.Mode().IsRegular() {
				err = fmt.Errorf("%s is not in the repository (it has %s: set that as the compose file)", req.Git.ComposeFile,
					path.Join(path.Dir(req.Git.ComposeFile), alt))
				break
			}
		}
	} else if err != nil {
		err = fmt.Errorf("compose file %s: %w", req.Git.ComposeFile, err)
	}
	if err != nil {
		return agentproto.ComposeFileResponse{}, err
	}
	return agentproto.ComposeFileResponse{Commit: co.commit, CommitMessage: co.subject, Compose: content}, nil
}

func dockerCLI() (string, error) {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return "", errors.New("building needs the docker command line tool on this server (apt install docker.io docker-buildx)")
	}
	return bin, nil
}

// buildSlot waits for a turn to build: builds are heavy, only a few run at
// once on a host.
func (m *appsManager) buildSlot(ctx context.Context, lg *deployLog) (release func(), err error) {
	release = func() { <-m.builds }
	select {
	case m.builds <- struct{}{}:
		return release, nil
	default:
	}
	lg.printf("Waiting for another build on this server to finish")
	select {
	case m.builds <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// checkout is a commit of an app's repository fetched into a work
// directory of its own, which close removes.
type checkout struct {
	work, src, home string
	env             []string // git's
	root            *os.Root // src
	commit, subject string
}

func (c *checkout) close() {
	if c.root != nil {
		c.root.Close()
	}
	os.RemoveAll(c.work)
}

// contextDir checks a build context directory of the repository: paths
// from the repository are resolved inside it only, so a symlink in it
// cannot make the build read files of this host.
func (c *checkout) contextDir(dir string) error {
	if fi, err := c.root.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("build context %s is not a directory in the repository", dir)
	}
	if fi, err := c.root.Lstat(dir); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("build context %s must not be a symlink", dir)
	}
	return nil
}

// checkout fetches spec.Git (its Commit, or the head of its branch) with
// its submodules into <agent-data>/builds/app-<id>-<name>.
func (m *appsManager) checkout(ctx context.Context, spec agentproto.AppSpec, name string, lg *deployLog) (_ *checkout, err error) {
	g := spec.Git
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return nil, errors.New("building needs git on this server (apt install git)")
	}
	work := filepath.Join(m.dataDir, "builds", fmt.Sprintf("app-%d-%s", spec.AppID, name))
	os.RemoveAll(work)
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	co := &checkout{work: work, src: filepath.Join(work, "src"), home: filepath.Join(work, "home")}
	defer func() {
		if err != nil {
			co.close()
		}
	}()
	if err := os.MkdirAll(co.home, 0o700); err != nil {
		return nil, err
	}
	if co.env, err = m.gitEnv(spec, work, co.home); err != nil {
		return nil, err
	}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, gitBin, args...)
		cmd.Env, cmd.Dir, cmd.WaitDelay = co.env, work, 5*time.Second
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%s: %s", strings.TrimSpace(lastLines(string(out), 3)), err)
		}
		return strings.TrimSpace(string(out)), nil
	}
	src := co.src

	target := g.Branch
	if g.Commit != "" {
		target = g.Commit
	}
	lg.printf("Fetching %s from %s", target, g.URL)
	if _, err := run("init", "-q", src); err != nil {
		return nil, fmt.Errorf("git init: %w", err)
	}
	if _, err := run("-C", src, "remote", "add", "--", "origin", g.URL); err != nil {
		return nil, fmt.Errorf("git remote: %w", err)
	}
	if _, err := run("-C", src, "fetch", "-q", "--depth", "1", "--no-tags", "origin", target); err != nil {
		if g.Commit == "" {
			return nil, fmt.Errorf("fetch %s: %w%s", target, err, accessHint(g))
		}
		// Some servers refuse fetching a commit by hash: fetch the branch.
		if _, err := run("-C", src, "fetch", "-q", "--no-tags", "origin", g.Branch); err != nil {
			return nil, fmt.Errorf("fetch %s: %w%s", g.Branch, err, accessHint(g))
		}
		target = g.Commit
	} else {
		target = "FETCH_HEAD"
	}
	if _, err := run("-C", src, "checkout", "-q", "--detach", target); err != nil {
		return nil, fmt.Errorf("checkout: %w", err)
	}
	// Submodules, with the same transports (https, ssh) and credentials:
	// the deploy key, or a token that reaches the repository itself only.
	if fi, err := os.Lstat(filepath.Join(src, ".gitmodules")); err == nil && fi.Mode().IsRegular() {
		lg.printf("Fetching submodules")
		if _, err := run("-C", src, "-c", "protocol.file.allow=never", "submodule", "update", "--init", "--recursive", "--depth", "1"); err != nil {
			return nil, fmt.Errorf("submodules: %w", err)
		}
	}
	if co.commit, err = run("-C", src, "rev-parse", "HEAD"); err != nil {
		return nil, err
	}
	co.subject, _ = run("-C", src, "log", "-1", "--format=%s")
	co.subject = truncateRunes(co.subject, 200)
	lg.printf("Commit %s %s", short(co.commit), co.subject)
	if co.root, err = os.OpenRoot(src); err != nil {
		return nil, err
	}
	return co, nil
}

// dockerBuildOptions describe one docker build from a checkout.
type dockerBuildOptions struct {
	image      string
	dockerfile string // its content
	context    string // directory in the repository
	target     string
	args       map[string]string // build arguments
	secrets    map[string]string // BuildKit secrets
	labels     []string          // key=value
	noCache    bool
	pull       bool
}

// dockerBuild runs docker build with the output in the deployment log.
// Build arguments and secrets are passed by name, their values through
// the docker command's environment: never on its command line, which every
// user of the host may read.
func (m *appsManager) dockerBuild(ctx context.Context, co *checkout, dockerBin, cfg string, o dockerBuildOptions, lg *deployLog) error {
	secretArgs, secretEnv, _ := buildSecrets(o.secrets)
	args := []string{"--host", "unix://" + m.socket, "build", "--progress=plain", "--file", "-", "--tag", o.image}
	for _, l := range o.labels {
		args = append(args, "--label", l)
	}
	if o.target != "" {
		args = append(args, "--target", o.target)
	}
	if o.noCache {
		args = append(args, "--no-cache")
	}
	if o.pull {
		args = append(args, "--pull")
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + co.home, "DOCKER_CONFIG=" + cfg,
		"DOCKER_BUILDKIT=1", "BUILDX_NO_DEFAULT_ATTESTATIONS=1"}
	for _, k := range slices.Sorted(maps.Keys(o.args)) {
		args = append(args, "--build-arg", k)
		env = append(env, k+"="+o.args[k])
	}
	args = append(append(args, secretArgs...), filepath.Join(co.src, o.context))
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	cmd.Env = append(env, secretEnv...)
	cmd.Dir, cmd.WaitDelay = co.work, 10*time.Second
	cmd.Stdin = strings.NewReader(o.dockerfile)
	out := &buildOutput{lg: lg}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	out.flush()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if lg.full() {
			lg.forceWrite("Last lines of the build:\n" + strings.Join(out.tail, "\n") + "\n")
		}
		return fmt.Errorf("the build failed (%v)", err)
	}
	return nil
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
		// in the process list. GitHub takes it as x-access-token. Only for
		// the repository's URL: submodules elsewhere must not get it.
		u, err := url.Parse(spec.Git.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, errors.New("a repository token needs an https URL")
		}
		auth := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + t))
		env = append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http."+u.Scheme+"://"+u.Host+u.EscapedPath()+".extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+auth)
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

// buildSecrets turns the variables a build gets into BuildKit secrets: the
// arguments name them (--secret id=NAME,env=NAME), the values go in the
// environment of the docker command only (never its command line, which
// every user of the host may read), and hash identifies the values, so a
// change rebuilds what used them.
func buildSecrets(vars map[string]string) (args, env []string, hash string) {
	names := make([]string, 0, len(vars))
	for k := range vars {
		names = append(names, k)
	}
	slices.Sort(names)
	h := sha256.New()
	for _, k := range names {
		args = append(args, "--secret", "id="+k+",env="+k)
		env = append(env, k+"="+vars[k])
		fmt.Fprintf(h, "%s=%d:%s\n", k, len(vars[k]), vars[k])
	}
	return args, env, hex.EncodeToString(h.Sum(nil))
}
