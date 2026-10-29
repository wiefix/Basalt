package agentproto

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/wiefix/basalt/internal/docker"
)

// GitSource tells the agent to build the deployment's image from a git
// repository instead of pulling it.
type GitSource struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
	// Commit, when set, is built instead of the branch head.
	Commit     string `json:"commit,omitempty"`
	Dockerfile string `json:"dockerfile"` // path in the repository
	Context    string `json:"context"`    // build context directory in the repository
	// Builder: "" builds Dockerfile; BuilderRailpack works out how to build
	// the context (Node, Python, Go, PHP…) without one; BuilderCompose runs
	// ComposeFile, building the services it builds (ServiceSpec.Build).
	Builder     string `json:"builder,omitempty"`
	ComposeFile string `json:"compose_file,omitempty"` // path in the repository

	// BuildEnv are variables the build gets as secrets (Railpack passes them
	// to its steps; a Dockerfile reads them with
	// RUN --mount=type=secret,id=NAME,env=NAME): never in the image.
	BuildEnv map[string]string `json:"build_env,omitempty"`
	// DeployKey is an OpenSSH private key for ssh:// and scp-style URLs.
	DeployKey string `json:"deploy_key,omitempty"`
	// Token is a short-lived token for an https URL (a GitHub App's
	// installation token, sent as x-access-token).
	Token string `json:"token,omitempty"`
	// ReuseImage is the image of an earlier deployment of Commit; when it is
	// still on the host it is used instead of building (rollbacks).
	ReuseImage string `json:"reuse_image,omitempty"`
	// RegistryAuths are offered to the build for base images in private
	// registries.
	RegistryAuths []docker.AuthConfig `json:"registry_auths,omitempty"`
}

var (
	branchRE   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	gitTokenRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{20,512}$`)
	commitRE   = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	scpRE      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}@[A-Za-z0-9.-]{1,253}:[A-Za-z0-9._~/-]{1,512}$`)
)

// ValidateGitURL accepts https://host/path, ssh://[user@]host[:port]/path
// and user@host:path. Other transports (file, ext, plain http, git) are
// refused: ext runs commands and the others are unauthenticated or local.
// Credentials in https URLs are refused too: use a deploy key over ssh.
func ValidateGitURL(s string) error {
	if s == "" || len(s) > 1024 || strings.HasPrefix(s, "-") || strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return errors.New("invalid repository URL")
	}
	if scpRE.MatchString(s) && !strings.Contains(s, "://") {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Path == "" || u.Path == "/" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("repository URL must look like https://host/org/repo.git or git@host:org/repo.git")
	}
	switch u.Scheme {
	case "https":
		if u.User != nil {
			return errors.New("do not put credentials in the URL; use the SSH URL with the app's deploy key")
		}
	case "ssh":
		if _, has := u.User.Password(); has {
			return errors.New("do not put a password in the URL; use the app's deploy key")
		}
	default:
		return errors.New("repository URL must use https:// or ssh (git@host:org/repo.git)")
	}
	return nil
}

// IsSSHGitURL reports whether a valid repository URL is reached over SSH.
func IsSSHGitURL(s string) bool { return !strings.HasPrefix(s, "https://") }

// ValidateBranch checks a branch name as git refs allow it, more strictly.
func ValidateBranch(b string) error {
	if !branchRE.MatchString(b) || strings.HasPrefix(b, "-") || strings.HasPrefix(b, "/") ||
		strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".lock") || strings.Contains(b, "..") || strings.Contains(b, "//") {
		return fmt.Errorf("invalid branch name %q", b)
	}
	return nil
}

// ValidateRepoPath checks a relative path inside the repository ("." for
// the root when dir is set).
func ValidateRepoPath(p string, dir bool) error {
	if p == "" || len(p) > 512 || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") ||
		path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || (!dir && p == ".") {
		return fmt.Errorf("invalid path %q: use a path inside the repository such as Dockerfile or docker/web", p)
	}
	return nil
}

// Validate checks a git source.
func (g *GitSource) Validate() error {
	if err := ValidateGitURL(g.URL); err != nil {
		return err
	}
	if err := ValidateBranch(g.Branch); err != nil {
		return err
	}
	if g.Commit != "" && !commitRE.MatchString(g.Commit) {
		return errors.New("invalid commit")
	}
	total := 0
	for k, v := range g.BuildEnv {
		if err := ValidBuildEnvName(k); err != nil {
			return err
		}
		total += len(k) + len(v)
	}
	if len(g.BuildEnv) > MaxBuildEnv || total > 256<<10 {
		return errors.New("too many build variables")
	}
	switch g.Builder {
	case BuilderDockerfile, BuilderRailpack:
		if g.ComposeFile != "" {
			return errors.New("a compose file is only for the compose builder")
		}
	case BuilderCompose:
		if err := ValidateRepoPath(g.ComposeFile, false); err != nil {
			return err
		}
	default:
		return errors.New(`builder must be "" (Dockerfile), "railpack" or "compose"`)
	}
	if err := ValidateRepoPath(g.Dockerfile, false); err != nil {
		return err
	}
	if err := ValidateRepoPath(g.Context, true); err != nil {
		return err
	}
	if g.Token != "" && (!gitTokenRE.MatchString(g.Token) || IsSSHGitURL(g.URL)) {
		return errors.New("invalid repository token")
	}
	if len(g.DeployKey) > 16<<10 || g.ReuseImage != "" && !docker.ValidRef(g.ReuseImage) || len(g.RegistryAuths) > 50 {
		return errors.New("invalid git source")
	}
	return nil
}

// Builders of a git app.
const (
	BuilderDockerfile = "" // the repository's Dockerfile
	BuilderRailpack   = "railpack"
	BuilderCompose    = "compose"
)

// MaxComposeFile bounds a compose file.
const MaxComposeFile = 256 << 10

// ComposeFileRequest asks the agent for the compose file of a git app at
// the head of its branch (or at Git.Commit).
type ComposeFileRequest struct {
	AppID int64      `json:"app_id"`
	Git   *GitSource `json:"git"`
}

type ComposeFileResponse struct {
	Commit        string `json:"commit"`
	CommitMessage string `json:"commit_message,omitempty"`
	Compose       string `json:"compose"`
}

// Validate checks a request for a compose file.
func (r *ComposeFileRequest) Validate() error {
	if r.AppID <= 0 || r.Git == nil || r.Git.Builder != BuilderCompose {
		return errors.New("invalid compose file request")
	}
	return r.Git.Validate()
}

// Railpack: the BuildKit frontend that builds Railpack's plans, of the
// version the agents' railpack command is (the installer installs it), and
// the BuildKit a builder of Basalt runs where Docker's own cannot build them.
const (
	RailpackVersion  = "0.43.0"
	RailpackFrontend = "ghcr.io/railwayapp/railpack-frontend:v0.43.0@sha256:355ef61fae6f2551103cd8b10c710cb07167dd353d561f15c69b8d7ac1c676cc"
	BuildKitImage    = "moby/buildkit:v0.34.1@sha256:99368f3e46177c6405b9740e4461d91cee004128daeecca78d4ceac50ec1da5c"
)

// MaxBuildEnv is how many variables a build may get.
const MaxBuildEnv = 50

var buildEnvRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// ValidBuildEnvName checks the name of a variable a build gets: a plain
// name, not one the build tools themselves read.
func ValidBuildEnvName(k string) error {
	if !buildEnvRE.MatchString(k) {
		return fmt.Errorf("invalid build variable name %q", k)
	}
	u := strings.ToUpper(k)
	if u == "PATH" || u == "HOME" {
		return fmt.Errorf("%s cannot be a build variable", k)
	}
	for _, p := range []string{"DOCKER_", "BUILDX_", "BUILDKIT_", "GIT_", "SSH_"} {
		if strings.HasPrefix(u, p) {
			return fmt.Errorf("%s cannot be a build variable (names starting with %s are the build tools')", k, p)
		}
	}
	return nil
}

// ValidCommit reports whether s is a full commit hash.
func ValidCommit(s string) bool { return commitRE.MatchString(s) }
