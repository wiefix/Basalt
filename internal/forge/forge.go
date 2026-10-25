// Package forge talks to GitLab and Gitea (or Forgejo) with an access
// token, for what the panel does with a repository there: list them and
// their branches, add an app's read-only deploy key and its push webhook
// (and remove them), and set the status of a commit. The token stays in
// the panel; servers clone with the deploy key.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kinds of forge.
const (
	GitLab = "gitlab"
	Gitea  = "gitea" // also Forgejo
)

const (
	MaxRepos    = 1000
	MaxBranches = 300
	maxBody     = 8 << 20
)

// Client is a forge at BaseURL (e.g. https://gitlab.com), with a token.
type Client struct {
	Kind    string
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Error is the forge's answer to a request that failed.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("answered HTTP %d: %s", e.Status, e.Message)
}

// ValidBaseURL checks a forge's address: http(s)://host[:port][/path], no
// query, no credentials.
func ValidBaseURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("the address must be like https://gitlab.example.com")
	}
	return nil
}

var tokenRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{8,512}$`)

// ValidToken reports whether s looks like an access token.
func ValidToken(s string) bool { return tokenRE.MatchString(s) }

var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}(/[A-Za-z0-9_.-]{1,100}){1,9}$`)

// ValidRepo checks a repository's full path: owner/name, or group/sub/name
// on GitLab.
func ValidRepo(s string) bool {
	if !repoRE.MatchString(s) {
		return false
	}
	for _, p := range strings.Split(s, "/") {
		if p == "." || p == ".." {
			return false
		}
	}
	return true
}

func (c *Client) api() string {
	base := strings.TrimRight(c.BaseURL, "/")
	if c.Kind == GitLab {
		return base + "/api/v4"
	}
	return base + "/api/v1"
}

// do sends a request and decodes the JSON answer into out (nil: ignored);
// it returns the answer's headers.
func (c *Client) do(ctx context.Context, method, path string, body, out any) (http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api()+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Basalt")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Kind == GitLab {
		req.Header.Set("PRIVATE-TOKEN", c.Token)
	} else {
		req.Header.Set("Authorization", "token "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message any    `json:"message"`
			Error   string `json:"error"`
		}
		json.Unmarshal(b, &e)
		msg := e.Error
		if e.Message != nil {
			if s, ok := e.Message.(string); ok {
				msg = s
			} else if j, err := json.Marshal(e.Message); err == nil {
				msg = string(j)
			}
		}
		return nil, &Error{Status: resp.StatusCode, Message: truncate(msg, 300)}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return nil, fmt.Errorf("the answer: %w", err)
		}
	}
	return resp.Header, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// User returns the login the token belongs to (checks the token).
func (c *Client) User(ctx context.Context) (string, error) {
	var u struct {
		Username string `json:"username"` // GitLab
		Login    string `json:"login"`    // Gitea
	}
	if _, err := c.do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return "", err
	}
	if u.Username != "" {
		return u.Username, nil
	}
	if u.Login != "" {
		return u.Login, nil
	}
	return "", errors.New("the token belongs to no user")
}

// Repo is a repository the token may administer (add keys and webhooks).
type Repo struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	SSHURL        string `json:"ssh_url"`
	HTMLURL       string `json:"html_url"`
}

type gitlabProject struct {
	PathWithNamespace string `json:"path_with_namespace"`
	Visibility        string `json:"visibility"`
	DefaultBranch     string `json:"default_branch"`
	SSHURL            string `json:"ssh_url_to_repo"`
	WebURL            string `json:"web_url"`
}

func (p gitlabProject) repo() Repo {
	return Repo{FullName: p.PathWithNamespace, Private: p.Visibility != "public", DefaultBranch: p.DefaultBranch, SSHURL: p.SSHURL, HTMLURL: p.WebURL}
}

type giteaRepo struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	SSHURL        string `json:"ssh_url"`
	HTMLURL       string `json:"html_url"`
	Permissions   struct {
		Admin bool `json:"admin"`
	} `json:"permissions"`
}

func (r giteaRepo) repo() Repo {
	return Repo{FullName: r.FullName, Private: r.Private, DefaultBranch: r.DefaultBranch, SSHURL: r.SSHURL, HTMLURL: r.HTMLURL}
}

// Repos lists the repositories the token administers (at most MaxRepos):
// on GitLab those it is a maintainer of, on Gitea those it is an admin of.
func (c *Client) Repos(ctx context.Context) ([]Repo, error) {
	out := []Repo{}
	for page := 1; len(out) < MaxRepos; page++ {
		n := 0
		if c.Kind == GitLab {
			var ps []gitlabProject
			if _, err := c.do(ctx, http.MethodGet, "/projects?membership=true&min_access_level=40&simple=true&order_by=path&sort=asc&per_page=100&page="+strconv.Itoa(page), nil, &ps); err != nil {
				return nil, err
			}
			n = len(ps)
			for _, p := range ps {
				if ValidRepo(p.PathWithNamespace) && len(out) < MaxRepos {
					out = append(out, p.repo())
				}
			}
			if n < 100 {
				break
			}
		} else {
			var rs []giteaRepo
			if _, err := c.do(ctx, http.MethodGet, "/user/repos?limit=50&page="+strconv.Itoa(page), nil, &rs); err != nil {
				return nil, err
			}
			n = len(rs)
			for _, r := range rs {
				if r.Permissions.Admin && ValidRepo(r.FullName) && len(out) < MaxRepos {
					out = append(out, r.repo())
				}
			}
			if n < 50 {
				break
			}
		}
	}
	return out, nil
}

// path is a repository's API path.
func (c *Client) path(repo string) string {
	if c.Kind == GitLab {
		return "/projects/" + url.PathEscape(repo)
	}
	owner, name, _ := strings.Cut(repo, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func (c *Client) check(repo string) error {
	if !ValidRepo(repo) || (c.Kind == Gitea && strings.Count(repo, "/") != 1) {
		return errors.New("invalid repository")
	}
	return nil
}

// Repo returns a repository the token reaches.
func (c *Client) Repo(ctx context.Context, repo string) (*Repo, error) {
	if err := c.check(repo); err != nil {
		return nil, err
	}
	var r Repo
	if c.Kind == GitLab {
		var p gitlabProject
		if _, err := c.do(ctx, http.MethodGet, c.path(repo), nil, &p); err != nil {
			return nil, err
		}
		r = p.repo()
	} else {
		var g giteaRepo
		if _, err := c.do(ctx, http.MethodGet, c.path(repo), nil, &g); err != nil {
			return nil, err
		}
		r = g.repo()
	}
	if !strings.EqualFold(r.FullName, repo) {
		return nil, errors.New("the forge returned another repository")
	}
	return &r, nil
}

// Branches lists a repository's branches (at most MaxBranches).
func (c *Client) Branches(ctx context.Context, repo string) ([]string, error) {
	if err := c.check(repo); err != nil {
		return nil, err
	}
	out := []string{}
	list, per := c.path(repo)+"/branches?limit=50", 50
	if c.Kind == GitLab {
		list, per = c.path(repo)+"/repository/branches?per_page=100", 100
	}
	for page := 1; len(out) < MaxBranches; page++ {
		var bs []struct {
			Name string `json:"name"`
		}
		if _, err := c.do(ctx, http.MethodGet, list+"&page="+strconv.Itoa(page), nil, &bs); err != nil {
			return nil, err
		}
		for _, b := range bs {
			if len(out) < MaxBranches {
				out = append(out, b.Name)
			}
		}
		if len(bs) < per {
			break
		}
	}
	return out, nil
}

// AddDeployKey adds a read-only deploy key (an OpenSSH public key line).
func (c *Client) AddDeployKey(ctx context.Context, repo, title, key string) (int64, error) {
	if err := c.check(repo); err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	var err error
	if c.Kind == GitLab {
		_, err = c.do(ctx, http.MethodPost, c.path(repo)+"/deploy_keys", map[string]any{"title": title, "key": key, "can_push": false}, &out)
	} else {
		_, err = c.do(ctx, http.MethodPost, c.path(repo)+"/keys", map[string]any{"title": title, "key": key, "read_only": true}, &out)
	}
	if err == nil && out.ID <= 0 {
		err = errors.New("no key id in the answer")
	}
	return out.ID, err
}

// DeleteDeployKey removes a deploy key; one already gone is fine.
func (c *Client) DeleteDeployKey(ctx context.Context, repo string, id int64) error {
	if err := c.check(repo); err != nil {
		return err
	}
	p := "/keys/"
	if c.Kind == GitLab {
		p = "/deploy_keys/"
	}
	return gone(c.do(ctx, http.MethodDelete, c.path(repo)+p+strconv.FormatInt(id, 10), nil, nil))
}

// AddHook adds a webhook of push events to hookURL, signed with secret
// (GitLab sends it as X-Gitlab-Token, Gitea signs the body with it).
func (c *Client) AddHook(ctx context.Context, repo, hookURL, secret string) (int64, error) {
	if err := c.check(repo); err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	var err error
	if c.Kind == GitLab {
		_, err = c.do(ctx, http.MethodPost, c.path(repo)+"/hooks", map[string]any{"url": hookURL, "token": secret,
			"push_events": true, "enable_ssl_verification": strings.HasPrefix(hookURL, "https://")}, &out)
	} else {
		_, err = c.do(ctx, http.MethodPost, c.path(repo)+"/hooks", map[string]any{"type": "gitea", "active": true, "events": []string{"push"},
			"config": map[string]string{"url": hookURL, "content_type": "json", "secret": secret}}, &out)
	}
	if err == nil && out.ID <= 0 {
		err = errors.New("no webhook id in the answer")
	}
	return out.ID, err
}

// DeleteHook removes a webhook; one already gone is fine.
func (c *Client) DeleteHook(ctx context.Context, repo string, id int64) error {
	if err := c.check(repo); err != nil {
		return err
	}
	return gone(c.do(ctx, http.MethodDelete, c.path(repo)+"/hooks/"+strconv.FormatInt(id, 10), nil, nil))
}

func gone(_ http.Header, err error) error {
	var fe *Error
	if errors.As(err, &fe) && fe.Status == http.StatusNotFound {
		return nil
	}
	return err
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

// SetStatus sets the status of commit sha: state pending, success, failure
// or error (a canceled deployment), with a link and a context naming it.
func (c *Client) SetStatus(ctx context.Context, repo, sha, state, targetURL, description, statusContext string) error {
	if err := c.check(repo); err != nil {
		return err
	}
	if !shaRE.MatchString(sha) {
		return errors.New("invalid commit")
	}
	if statusContext == "" || len(statusContext) > 100 {
		return errors.New("invalid context")
	}
	if len(description) > 140 {
		description = description[:137] + "..."
	}
	if c.Kind == GitLab {
		st := map[string]string{"pending": "running", "success": "success", "failure": "failed", "error": "canceled"}[state]
		if st == "" {
			return errors.New("invalid state")
		}
		body := map[string]string{"state": st, "name": statusContext, "description": description}
		if targetURL != "" {
			body["target_url"] = targetURL
		}
		_, err := c.do(ctx, http.MethodPost, c.path(repo)+"/statuses/"+sha, body, nil)
		return err
	}
	switch state {
	case "pending", "success", "failure", "error":
	default:
		return errors.New("invalid state")
	}
	body := map[string]string{"state": state, "context": statusContext, "description": description}
	if targetURL != "" {
		body["target_url"] = targetURL
	}
	_, err := c.do(ctx, http.MethodPost, c.path(repo)+"/statuses/"+sha, body, nil)
	return err
}
