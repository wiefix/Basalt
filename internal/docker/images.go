package docker

import (
	"context"
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

// Pull downloads an image. progress (may be nil) receives status lines as
// the daemon reports them. Errors the daemon reports mid-stream are
// returned, so a failed pull never looks like a success.
func (c *Client) Pull(ctx context.Context, ref string, progress func(PullProgress)) error {
	repo, tag := SplitRef(ref)
	q := url.Values{"fromImage": {repo}, "tag": {tag}}
	resp, err := c.do(ctx, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
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

// ImageRemove deletes a local image. An image still used by a container is
// a conflict, which callers pruning old images can ignore.
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
