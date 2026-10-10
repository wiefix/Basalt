package docker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// PullProgress is one status line of an image pull.
type PullProgress struct {
	ID       string `json:"id,omitempty"`
	Status   string `json:"status,omitempty"`
	Progress string `json:"progress,omitempty"`
}

// SplitRef splits an image reference into repository and tag or digest
// ("nginx:1.27" -> "nginx", "1.27"; "a/b@sha256:..." -> "a/b", "sha256:...").
// A reference without either gets the tag "latest".
func SplitRef(ref string) (repo, tagOrDigest string) {
	if repo, digest, ok := strings.Cut(ref, "@"); ok {
		return repo, digest
	}
	// A colon after the last slash separates the tag; one before it belongs
	// to a registry host:port.
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// AuthConfig holds registry credentials for one pull or push. They are sent
// to the daemon with the request only; the daemon does not store them.
type AuthConfig struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ServerAddress string `json:"serveraddress"`
}

func authHeader(auth *AuthConfig) (http.Header, error) {
	if auth == nil {
		return nil, nil
	}
	b, err := json.Marshal(auth)
	if err != nil {
		return nil, err
	}
	return http.Header{"X-Registry-Auth": {base64.URLEncoding.EncodeToString(b)}}, nil
}

// DockerHub is the registry of references without a registry host.
const DockerHub = "docker.io"

// RegistryHost returns the registry an image reference points to:
// "ghcr.io/org/app:1" -> "ghcr.io", "localhost:5000/app" -> "localhost:5000",
// "nginx" and "library/nginx" -> "docker.io".
func RegistryHost(ref string) string {
	first, _, ok := strings.Cut(ref, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		first = strings.ToLower(first)
		if first == "index.docker.io" || first == "registry-1.docker.io" {
			return DockerHub
		}
		return first
	}
	return DockerHub
}

// ServerAddress is the address credentials for host are presented with.
func ServerAddress(host string) string {
	if host == DockerHub {
		return "https://index.docker.io/v1/"
	}
	return host
}

// Pull downloads an image, with registry credentials when auth is not nil.
// progress (may be nil) receives status lines as the daemon reports them.
// Errors the daemon reports mid-stream are returned, so a failed pull never
// looks like a success.
func (c *Client) Pull(ctx context.Context, ref string, auth *AuthConfig, progress func(PullProgress)) error {
	repo, tag := SplitRef(ref)
	q := url.Values{"fromImage": {repo}, "tag": {tag}}
	h, err := authHeader(auth)
	if err != nil {
		return err
	}
	resp, err := c.doHeader(ctx, http.MethodPost, "/images/create", q, nil, h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return readProgress(resp.Body, progress)
}

// Push uploads a local image (tag included in ref) to its registry.
func (c *Client) Push(ctx context.Context, ref string, auth *AuthConfig, progress func(PullProgress)) error {
	repo, tag := SplitRef(ref)
	h, err := authHeader(auth)
	if err != nil {
		return err
	}
	if h == nil {
		// The daemon requires the header even for anonymous pushes.
		h = http.Header{"X-Registry-Auth": {base64.URLEncoding.EncodeToString([]byte("{}"))}}
	}
	resp, err := c.doHeader(ctx, http.MethodPost, "/images/"+escapeRef(repo)+"/push", url.Values{"tag": {tag}}, nil, h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return readProgress(resp.Body, progress)
}

// Tag adds the reference ref (repository and tag) to a local image.
func (c *Client) Tag(ctx context.Context, image, ref string) error {
	repo, tag := SplitRef(ref)
	return c.call(ctx, http.MethodPost, "/images/"+escapeRef(image)+"/tag", url.Values{"repo": {repo}, "tag": {tag}}, nil, nil)
}

// readProgress decodes the JSON status stream of a pull or push.
func readProgress(r io.Reader, progress func(PullProgress)) error {
	dec := json.NewDecoder(r)
	for {
		var m struct {
			PullProgress
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if m.Error != "" || m.ErrorDetail.Message != "" {
			msg := m.ErrorDetail.Message
			if msg == "" {
				msg = m.Error
			}
			return &Error{Status: http.StatusOK, Message: msg}
		}
		if progress != nil {
			progress(m.PullProgress)
		}
	}
}

type ImageInfo struct {
	ID          string   `json:"Id"`
	RepoTags    []string `json:"RepoTags"`
	RepoDigests []string `json:"RepoDigests"`
	Created     string   `json:"Created"`
	Size        int64    `json:"Size"`
	Config      struct {
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		User         string              `json:"User"`
		Healthcheck  *HealthConfig       `json:"Healthcheck"`
	} `json:"Config"`
}

// ImageInspect returns a local image; ErrNotFound if it is not present.
func (c *Client) ImageInspect(ctx context.Context, ref string) (*ImageInfo, error) {
	var info ImageInfo
	if err := c.call(ctx, http.MethodGet, "/images/"+escapeRef(ref)+"/json", nil, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ImageRemove removes a reference (tag or digest) to a local image, and the
// image itself once nothing else refers to it. An image still used by a
// container is a conflict, which callers pruning old images can ignore.
func (c *Client) ImageRemove(ctx context.Context, ref string) error {
	return c.call(ctx, http.MethodDelete, "/images/"+escapeRef(ref), nil, nil, nil)
}

// escapeRef escapes each path segment of an image reference; the daemon's
// image routes expect the slashes of "registry/org/name" unescaped.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// Image reference grammar (distribution/reference), without uppercase
// repository names: [registry[:port]/]path[:tag][@sha256:digest].
var refRE = func() *regexp.Regexp {
	const (
		host = `[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?`
		comp = `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`
	)
	domain := `(?:` + host + `(?:\.` + host + `)*(?::[0-9]{1,5})?/)?`
	return regexp.MustCompile(`^` + domain + comp + `(?:/` + comp + `)*` +
		`(?::[\w][\w.-]{0,127})?(?:@sha256:[a-f0-9]{64})?$`)
}()

// ValidRef reports whether ref is a well-formed image reference.
func ValidRef(ref string) bool { return len(ref) <= 255 && refRE.MatchString(ref) }
