// Package github talks to GitHub as a GitHub App the panel created on the
// user's account or organization (the manifest flow): it lists the
// repositories the app was installed on, their branches, and mints
// short-lived tokens to clone them. No dependencies: the app's JWT is
// signed with crypto/rsa.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultAPI and DefaultWeb are GitHub's (GitHub Enterprise has others).
const (
	DefaultAPI = "https://api.github.com"
	DefaultWeb = "https://github.com"
)

const (
	maxBody = 4 << 20
	// MaxRepos bounds the repositories listed.
	MaxRepos = 1000
	// MaxBranches bounds the branches listed for a repository.
	MaxBranches = 300
)

// Client calls GitHub's API at API (DefaultAPI if empty) with HTTP.
type Client struct {
	API  string
	HTTP *http.Client
	Now  func() time.Time
}

func (c *Client) api() string {
	if c.API == "" {
		return DefaultAPI
	}
	return strings.TrimRight(c.API, "/")
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Error is GitHub's answer to a request that failed.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitHub answered HTTP %d", e.Status)
	}
	return fmt.Sprintf("GitHub answered HTTP %d: %s", e.Status, e.Message)
}

// do sends a request with auth ("Bearer x" or "token x"; "" for none) and
// decodes the JSON answer into out (nil: ignored).
func (c *Client) do(ctx context.Context, method, path, auth string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.api()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "Basalt")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(b, &e)
		if len(e.Message) > 300 {
			e.Message = e.Message[:300]
		}
		return &Error{Status: resp.StatusCode, Message: e.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("GitHub's answer: %w", err)
	}
	return nil
}

// Manifest describes the GitHub App the panel at panelURL asks GitHub to
// create: read access to code, commit statuses (how a deployment of a
// commit went), push and pull request events (previews) to the panel's
// webhook.
func Manifest(name, panelURL string) map[string]any {
	return map[string]any{
		"name":         name,
		"url":          panelURL,
		"redirect_url": panelURL + "/git-sources/github/callback",
		"setup_url":    panelURL + "/git-sources",
		"hook_attributes": map[string]any{
			"url":    panelURL + "/api/v1/webhooks/github",
			"active": true,
		},
		"public":              false,
		"setup_on_update":     true,
		"default_permissions": map[string]string{"contents": "read", "metadata": "read", "statuses": "write", "pull_requests": "read"},
		"default_events":      []string{"push", "pull_request"},
	}
}

// AppCredentials is what GitHub returns for a manifest: the new app and
// its secrets.
type AppCredentials struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	HTMLURL       string `json:"html_url"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

var codeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

// ConvertManifest turns the code GitHub gave the browser back into the
// app's credentials (once; the code expires within an hour).
func (c *Client) ConvertManifest(ctx context.Context, code string) (*AppCredentials, error) {
	if !codeRE.MatchString(code) {
		return nil, errors.New("invalid code")
	}
	var a AppCredentials
	if err := c.do(ctx, http.MethodPost, "/app-manifests/"+code+"/conversions", "", nil, &a); err != nil {
		return nil, err
	}
	if a.ID <= 0 || a.Slug == "" || a.WebhookSecret == "" || a.PEM == "" {
		return nil, errors.New("GitHub returned an incomplete app")
	}
	if _, err := ParseKey(a.PEM); err != nil {
		return nil, err
	}
	return &a, nil
}

// App is a GitHub App: its id and private key.
type App struct {
	ID  int64
	Key *rsa.PrivateKey
}

// ParseKey reads a GitHub App's private key (PEM, PKCS#1 or PKCS#8).
func ParseKey(s string) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode([]byte(s))
	if blk == nil {
		return nil, errors.New("the app's private key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the app's private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the app's private key is not RSA")
	}
	return rk, nil
}

// jwt is the app's own token (RS256, 9 minutes; issued a minute back for
// clock skew), for the /app endpoints.
func (c *Client) jwt(app App) (string, error) {
	now := c.now()
	enc := base64.RawURLEncoding
	head := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(app.ID, 10)})
	signing := head + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, app.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// Installation is an account or organization the app is installed on.
type Installation struct {
	ID      int64  `json:"id"`
	Account string `json:"account"`
	// All: on all its repositories, not chosen ones.
	All     bool   `json:"all"`
	HTMLURL string `json:"html_url"`
	// Statuses: the installation lets the app set commit statuses (apps
	// created before Basalt asked for it lack the permission until it is
	// added in the app's settings and accepted on the installation).
	Statuses bool `json:"statuses"`
	// PullRequests: GitHub sends the app's pull requests (previews).
	PullRequests bool `json:"pull_requests"`
}

// Installations lists where the app is installed.
func (c *Client) Installations(ctx context.Context, app App) ([]Installation, error) {
	tok, err := c.jwt(app)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		Selection   string            `json:"repository_selection"`
		HTMLURL     string            `json:"html_url"`
		Permissions map[string]string `json:"permissions"`
		Events      []string          `json:"events"`
	}
	if err := c.do(ctx, http.MethodGet, "/app/installations?per_page=100", "Bearer "+tok, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Installation, 0, len(raw))
	for _, r := range raw {
		out = append(out, Installation{ID: r.ID, Account: r.Account.Login, All: r.Selection == "all", HTMLURL: r.HTMLURL,
			Statuses:     r.Permissions["statuses"] == "write",
			PullRequests: r.Permissions["pull_requests"] != "" && slices.Contains(r.Events, "pull_request")})
	}
	return out, nil
}

// Token is a short-lived installation token.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// InstallationToken mints a token of installation id that reads code,
// limited to the repositories named (names without owner; none: all it
// may reach).
func (c *Client) InstallationToken(ctx context.Context, app App, id int64, repos ...string) (*Token, error) {
	return c.token(ctx, app, id, map[string]string{"contents": "read", "metadata": "read"}, repos)
}

// token mints an installation token with perms, limited to repos.
func (c *Client) token(ctx context.Context, app App, id int64, perms map[string]string, repos []string) (*Token, error) {
	tok, err := c.jwt(app)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"permissions": perms}
	if len(repos) > 0 {
		body["repositories"] = repos
	}
	var t Token
	if err := c.do(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(id, 10)+"/access_tokens", "Bearer "+tok, body, &t); err != nil {
		return nil, err
	}
	if !ValidToken(t.Token) {
		return nil, errors.New("GitHub returned an invalid token")
	}
	return &t, nil
}

var tokenRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{20,512}$`)

// ValidToken reports whether s looks like a GitHub token.
func ValidToken(s string) bool { return tokenRE.MatchString(s) }

// Repo is a repository the app can read.
type Repo struct {
	FullName       string `json:"full_name"`
	Private        bool   `json:"private"`
	DefaultBranch  string `json:"default_branch"`
	CloneURL       string `json:"clone_url"`
	HTMLURL        string `json:"html_url"`
	InstallationID int64  `json:"installation_id"`
}

// Repos lists the repositories of every installation (at most MaxRepos).
func (c *Client) Repos(ctx context.Context, app App) ([]Repo, error) {
	insts, err := c.Installations(ctx, app)
	if err != nil {
		return nil, err
	}
	out := []Repo{}
	for _, in := range insts {
		t, err := c.InstallationToken(ctx, app, in.ID)
		if err != nil {
			return nil, err
		}
		for page := 1; len(out) < MaxRepos; page++ {
			var resp struct {
				Repositories []Repo `json:"repositories"`
			}
			if err := c.do(ctx, http.MethodGet, "/installation/repositories?per_page=100&page="+strconv.Itoa(page), "token "+t.Token, nil, &resp); err != nil {
				return nil, err
			}
			for _, r := range resp.Repositories {
				if ValidRepo(r.FullName) && len(out) < MaxRepos {
					r.InstallationID = in.ID
					out = append(out, r)
				}
			}
			if len(resp.Repositories) < 100 {
				break
			}
		}
	}
	return out, nil
}

var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

// ValidRepo checks a repository's full name, owner/name.
func ValidRepo(s string) bool { return repoRE.MatchString(s) && !strings.Contains(s, "..") }

// RepoInstallation finds the installation that reaches repo (owner/name).
func (c *Client) RepoInstallation(ctx context.Context, app App, repo string) (int64, error) {
	if !ValidRepo(repo) {
		return 0, errors.New("invalid repository")
	}
	tok, err := c.jwt(app)
	if err != nil {
		return 0, err
	}
	var in struct {
		ID int64 `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+repoPath(repo)+"/installation", "Bearer "+tok, nil, &in); err != nil {
		var ge *Error
		if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
			return 0, fmt.Errorf("the GitHub App is not installed on %s", repo)
		}
		return 0, err
	}
	return in.ID, nil
}

// RepoToken mints a token that reads repo only.
func (c *Client) RepoToken(ctx context.Context, app App, repo string) (*Token, error) {
	id, err := c.RepoInstallation(ctx, app, repo)
	if err != nil {
		return nil, err
	}
	_, name, _ := strings.Cut(repo, "/")
	return c.InstallationToken(ctx, app, id, name)
}

// Status is a commit status: how something (Context) about a commit went.
type Status struct {
	State       string `json:"state"` // pending, success, failure or error
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description"`
	Context     string `json:"context"`
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

// SetStatus sets a status of commit sha of repo, with a token that may set
// statuses of this repository only.
func (c *Client) SetStatus(ctx context.Context, app App, repo, sha string, st Status) error {
	if !shaRE.MatchString(sha) {
		return errors.New("invalid commit")
	}
	switch st.State {
	case "pending", "success", "failure", "error":
	default:
		return errors.New("invalid state")
	}
	if len(st.Description) > 140 {
		st.Description = st.Description[:137] + "..."
	}
	if st.Context == "" || len(st.Context) > 100 {
		return errors.New("invalid context")
	}
	id, err := c.RepoInstallation(ctx, app, repo)
	if err != nil {
		return err
	}
	_, name, _ := strings.Cut(repo, "/")
	t, err := c.token(ctx, app, id, map[string]string{"statuses": "write"}, []string{name})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/repos/"+repoPath(repo)+"/statuses/"+sha, "token "+t.Token, st, nil)
}

// RepoInfo returns a repository the app reaches.
func (c *Client) RepoInfo(ctx context.Context, app App, repo string) (*Repo, error) {
	t, err := c.RepoToken(ctx, app, repo)
	if err != nil {
		return nil, err
	}
	var r Repo
	if err := c.do(ctx, http.MethodGet, "/repos/"+repoPath(repo), "token "+t.Token, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Branches lists a repository's branches (at most MaxBranches).
func (c *Client) Branches(ctx context.Context, app App, repo string) ([]string, error) {
	t, err := c.RepoToken(ctx, app, repo)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for page := 1; len(out) < MaxBranches; page++ {
		var bs []struct {
			Name string `json:"name"`
		}
		if err := c.do(ctx, http.MethodGet, "/repos/"+repoPath(repo)+"/branches?per_page=100&page="+strconv.Itoa(page), "token "+t.Token, nil, &bs); err != nil {
			return nil, err
		}
		for _, b := range bs {
			if len(out) < MaxBranches {
				out = append(out, b.Name)
			}
		}
		if len(bs) < 100 {
			break
		}
	}
	return out, nil
}

func repoPath(repo string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}
