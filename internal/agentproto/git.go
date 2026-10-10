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
	// DeployKey is an OpenSSH private key for ssh:// and scp-style URLs.
	DeployKey string `json:"deploy_key,omitempty"`
	// ReuseImage is the image of an earlier deployment of Commit; when it is
	// still on the host it is used instead of building (rollbacks).
	ReuseImage string `json:"reuse_image,omitempty"`
	// RegistryAuths are offered to the build for base images in private
	// registries.
	RegistryAuths []docker.AuthConfig `json:"registry_auths,omitempty"`
}

var (
	branchRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	commitRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	scpRE    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}@[A-Za-z0-9.-]{1,253}:[A-Za-z0-9._~/-]{1,512}$`)
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
	if err := ValidateRepoPath(g.Dockerfile, false); err != nil {
		return err
	}
	if err := ValidateRepoPath(g.Context, true); err != nil {
		return err
	}
	if len(g.DeployKey) > 16<<10 || g.ReuseImage != "" && !docker.ValidRef(g.ReuseImage) || len(g.RegistryAuths) > 50 {
		return errors.New("invalid git source")
	}
	return nil
}

// ValidCommit reports whether s is a full commit hash.
func ValidCommit(s string) bool { return commitRE.MatchString(s) }
