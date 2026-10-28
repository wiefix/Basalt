package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/docker"
)

// Builds without a Dockerfile (20.7): `railpack prepare` works out how to
// build the repository (language, versions, install, build and start
// commands) into a plan, which BuildKit builds with Railpack's frontend,
// pinned by digest. Docker's own builder cannot build these plans unless
// the daemon keeps images in containerd; otherwise the agent runs a
// BuildKit of its own in a container (builder basalt-railpack, its state
// under the agent's data so it and its cache last) and loads the image into
// Docker.

// railpackBuilder is the name of the agent's BuildKit builder (its
// container is buildx_buildkit_<name>0).
func (m *appsManager) railpackBuilder() string {
	if m.namespace != "" {
		return "basalt-" + m.namespace + "-railpack"
	}
	return "basalt-railpack"
}

var railpackVersionRE = regexp.MustCompile(`\b(\d+\.\d+\.\d+)\b`)

// railpackVersion is the version of the railpack command ("" if missing).
func (m *appsManager) railpackVersion(ctx context.Context) string {
	bin, err := m.railpackBin()
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	return railpackVersionRE.FindString(string(out))
}

func (m *appsManager) railpackBin() (string, error) {
	if m.railpack == "" {
		return "", errors.New("off")
	}
	return exec.LookPath(m.railpack)
}

// linksInside refuses a tree with a symbolic link leading outside it:
// railpack reads the files it analyses, and must not read this host's.
func linksInside(dir string) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	n := 0
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if n++; n > 200000 {
			return errors.New("the repository has too many files to build without a Dockerfile")
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if filepath.IsAbs(target) {
			return fmt.Errorf("%s links to %s, outside the repository: build it with a Dockerfile", rel, target)
		}
		resolved := filepath.Clean(filepath.Join(filepath.Dir(path), target))
		if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
			return fmt.Errorf("%s links outside the repository: build it with a Dockerfile", rel)
		}
		return nil
	})
}

// railpackInfo is what of `railpack prepare --info-out` the log shows.
type railpackInfo struct {
	DetectedProviders []string `json:"detectedProviders"`
	Logs              []struct {
		Level string `json:"Level"`
		Msg   string `json:"Msg"`
	} `json:"logs"`
}

// railpackBuild builds spec.Image from the context directory ctxDir of the
// checkout, without a Dockerfile.
func (m *appsManager) railpackBuild(ctx context.Context, dc *docker.Client, spec agentproto.AppSpec, lg *deployLog,
	dockerBin, ctxDir, work, home, cfg, commit string) error {
	bin, err := m.railpackBin()
	if err != nil {
		return errors.New("building without a Dockerfile needs Railpack on this server: re-run the installer with --with-docker")
	}
	if err := linksInside(ctxDir); err != nil {
		return err
	}
	plan, info := filepath.Join(work, "railpack-plan.json"), filepath.Join(work, "railpack-info.json")
	lg.printf("Working out how to build it (Railpack %s)", agentproto.RailpackVersion)
	prep := exec.CommandContext(ctx, bin, "prepare", ctxDir, "--plan-out", plan, "--info-out", info, "--hide-pretty-plan")
	prep.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	prep.Dir, prep.WaitDelay = work, 5*time.Second
	out, prepErr := prep.CombinedOutput()
	var ri railpackInfo
	if b, err := os.ReadFile(info); err == nil {
		json.Unmarshal(b, &ri)
	}
	if len(ri.DetectedProviders) > 0 {
		lg.printf("Detected: %s", strings.Join(ri.DetectedProviders, ", "))
	}
	var problems []string
	for _, l := range ri.Logs {
		if l.Level == "warn" || l.Level == "error" {
			lg.printf("Railpack %s: %s", l.Level, truncateRunes(l.Msg, 300))
		}
		if l.Level == "error" {
			problems = append(problems, truncateRunes(l.Msg, 300))
		}
	}
	if prepErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(problems) == 0 {
			problems = []string{strings.TrimSpace(lastLines(string(out), 3))}
		}
		return fmt.Errorf("could not work out how to build it without a Dockerfile: %s", strings.Join(problems, "; "))
	}

	sysInfo, err := dc.Info(ctx)
	if err != nil {
		return err
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "DOCKER_CONFIG=" + cfg, "DOCKER_HOST=unix://" + m.socket,
		"DOCKER_BUILDKIT=1", "BUILDX_NO_DEFAULT_ATTESTATIONS=1", "BUILDX_CONFIG=" + filepath.Join(m.dataDir, "buildx")}
	args := []string{"buildx", "build", "--progress=plain", "--load"}
	if sysInfo.ContainerdStore() {
		args = []string{"build", "--progress=plain"}
	} else {
		if err := m.ensureBuilder(ctx, dockerBin, env, lg); err != nil {
			return err
		}
		args = append(args, "--builder", m.railpackBuilder())
	}
	args = append(args, "--file", plan,
		"--build-arg", "BUILDKIT_SYNTAX="+agentproto.RailpackFrontend,
		// Each app its own build cache.
		"--build-arg", "cache-key="+m.network(spec.AppID),
		"--tag", spec.Image, "--label", labelApp+"="+strconv.FormatInt(spec.AppID, 10), "--label", "basalt.commit="+commit,
		ctxDir)
	lg.printf("Building %s", spec.Image)
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	cmd.Env, cmd.Dir, cmd.WaitDelay = env, work, 10*time.Second
	bo := &buildOutput{lg: lg}
	cmd.Stdout, cmd.Stderr = bo, bo
	err = cmd.Run()
	bo.flush()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if lg.full() {
			lg.forceWrite("Last lines of the build:\n" + strings.Join(bo.tail, "\n") + "\n")
		}
		return fmt.Errorf("the build failed (%v)", err)
	}
	return nil
}

// ensureBuilder makes sure the agent's BuildKit builder exists.
func (m *appsManager) ensureBuilder(ctx context.Context, dockerBin string, env []string, lg *deployLog) error {
	if err := os.MkdirAll(filepath.Join(m.dataDir, "buildx"), 0o700); err != nil {
		return err
	}
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, dockerBin, args...)
		cmd.Env, cmd.WaitDelay = env, 5*time.Second
		return cmd.CombinedOutput()
	}
	if _, err := run("buildx", "inspect", m.railpackBuilder()); err == nil {
		return nil
	}
	lg.printf("Setting up the builder for builds without a Dockerfile (once)")
	if out, err := run("buildx", "create", "--name", m.railpackBuilder(), "--driver", "docker-container",
		"--driver-opt", "image="+agentproto.BuildKitImage); err != nil {
		if strings.Contains(string(out), "unknown command") || strings.Contains(string(out), "is not a docker command") {
			return errors.New("building without a Dockerfile needs docker buildx on this server (apt install docker-buildx)")
		}
		return fmt.Errorf("create the builder: %s", strings.TrimSpace(lastLines(string(out), 3)))
	}
	return nil
}
