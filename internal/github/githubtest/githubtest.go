// Package githubtest is a fake GitHub API for tests: one GitHub App,
// created by the manifest flow, installed on some repositories.
package githubtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Repo is a repository of the fake, under an installation.
type Repo struct {
	FullName      string
	Private       bool
	DefaultBranch string
	Branches      []string
	CloneURL      string // default https://github.example/<full name>.git
	Installation  int64
}

// Server is the fake GitHub.
type Server struct {
	*httptest.Server
	AppID         int64
	Slug          string
	Owner         string
	Key           *rsa.PrivateKey
	WebhookSecret string
	// Code is the manifest code that converts (once).
	Code string

	mu       sync.Mutex
	repos    []Repo
	tokens   map[string][]string // token -> repositories it reads (nil: all of its installation)
	tokenIns map[string]int64
	used     bool
}

// New starts the fake with repos.
func New(t *testing.T, repos ...Repo) *Server {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{AppID: 4242, Slug: "basalt-test", Owner: "acme", Key: key, WebhookSecret: "hook-secret-123",
		Code: "manifestcode123", repos: repos, tokens: map[string][]string{}, tokenIns: map[string]int64{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// SetRepos replaces the repositories.
func (s *Server) SetRepos(repos ...Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos = repos
}

// Tokens returns the tokens issued so far.
func (s *Server) Tokens() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]string{}
	for k, v := range s.tokens {
		out[k] = slices.Clone(v)
	}
	return out
}

func (s *Server) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, code int, msg string) {
	s.json(w, code, map[string]string{"message": msg})
}

// appJWT checks the app's own token.
func (s *Server) appJWT(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&s.Key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	json.Unmarshal(b, &c)
	return c.Iss == strconv.FormatInt(s.AppID, 10) && c.Exp > time.Now().Unix()
}

// installationToken returns the repositories an installation token reads.
func (s *Server) installationToken(r *http.Request) (int64, []string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "token ")
	if !ok {
		return 0, nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.tokenIns[tok]
	return in, s.tokens[tok], ok
}

func (s *Server) cloneURL(r Repo) string {
	if r.CloneURL != "" {
		return r.CloneURL
	}
	return "https://github.example/" + r.FullName + ".git"
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	s.mu.Lock()
	repos := slices.Clone(s.repos)
	s.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && p == "/app-manifests/"+s.Code+"/conversions":
		s.mu.Lock()
		used := s.used
		s.used = true
		s.mu.Unlock()
		if used {
			s.fail(w, http.StatusNotFound, "Not Found")
			return
		}
		der := x509.MarshalPKCS1PrivateKey(s.Key)
		s.json(w, http.StatusCreated, map[string]any{"id": s.AppID, "slug": s.Slug, "name": "Basalt test", "html_url": "https://github.example/apps/" + s.Slug,
			"client_id": "Iv1.abc", "client_secret": "client-secret", "webhook_secret": s.WebhookSecret,
			"pem": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})), "owner": map[string]string{"login": s.Owner}})
	case strings.HasPrefix(p, "/app-manifests/"):
		s.fail(w, http.StatusNotFound, "Not Found")
	case strings.HasPrefix(p, "/app/"):
		if !s.appJWT(r) {
			s.fail(w, http.StatusUnauthorized, "A JSON web token could not be decoded")
			return
		}
		s.handleApp(w, r, repos)
	case strings.HasPrefix(p, "/repos/") && strings.HasSuffix(p, "/installation"):
		if !s.appJWT(r) {
			s.fail(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		full := strings.TrimSuffix(strings.TrimPrefix(p, "/repos/"), "/installation")
		for _, rp := range repos {
			if strings.EqualFold(rp.FullName, full) {
				s.json(w, http.StatusOK, map[string]any{"id": rp.Installation})
				return
			}
		}
		s.fail(w, http.StatusNotFound, "Not Found")
	default:
		in, allowed, ok := s.installationToken(r)
		if !ok {
			s.fail(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		reach := func(rp Repo) bool {
			_, name, _ := strings.Cut(rp.FullName, "/")
			return rp.Installation == in && (allowed == nil || slices.Contains(allowed, name))
		}
		switch {
		case p == "/installation/repositories":
			out := []map[string]any{}
			for _, rp := range repos {
				if reach(rp) {
					out = append(out, map[string]any{"full_name": rp.FullName, "private": rp.Private, "default_branch": rp.DefaultBranch,
						"clone_url": s.cloneURL(rp), "html_url": "https://github.example/" + rp.FullName})
				}
			}
			if page := r.URL.Query().Get("page"); page != "" && page != "1" {
				out = []map[string]any{}
			}
			s.json(w, http.StatusOK, map[string]any{"total_count": len(out), "repositories": out})
		case strings.HasPrefix(p, "/repos/"):
			rest := strings.TrimPrefix(p, "/repos/")
			full, branches := strings.CutSuffix(rest, "/branches")
			for _, rp := range repos {
				if !strings.EqualFold(rp.FullName, full) || !reach(rp) {
					continue
				}
				if branches {
					out := []map[string]string{}
					if page := r.URL.Query().Get("page"); page == "" || page == "1" {
						for _, b := range rp.Branches {
							out = append(out, map[string]string{"name": b})
						}
					}
					s.json(w, http.StatusOK, out)
					return
				}
				s.json(w, http.StatusOK, map[string]any{"full_name": rp.FullName, "private": rp.Private, "default_branch": rp.DefaultBranch,
					"clone_url": s.cloneURL(rp), "html_url": "https://github.example/" + rp.FullName})
				return
			}
			s.fail(w, http.StatusNotFound, "Not Found")
		default:
			s.fail(w, http.StatusNotFound, "Not Found")
		}
	}
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request, repos []Repo) {
	p := r.URL.Path
	if r.Method == http.MethodGet && p == "/app/installations" {
		seen := map[int64]bool{}
		out := []map[string]any{}
		for _, rp := range repos {
			if seen[rp.Installation] {
				continue
			}
			seen[rp.Installation] = true
			owner, _, _ := strings.Cut(rp.FullName, "/")
			out = append(out, map[string]any{"id": rp.Installation, "account": map[string]string{"login": owner},
				"repository_selection": "selected", "html_url": fmt.Sprintf("https://github.example/settings/installations/%d", rp.Installation)})
		}
		s.json(w, http.StatusOK, out)
		return
	}
	if r.Method == http.MethodPost && strings.HasPrefix(p, "/app/installations/") && strings.HasSuffix(p, "/access_tokens") {
		id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(p, "/app/installations/"), "/access_tokens"), 10, 64)
		if err != nil {
			s.fail(w, http.StatusNotFound, "Not Found")
			return
		}
		var body struct {
			Repositories []string `json:"repositories"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		b := make([]byte, 24)
		rand.Read(b)
		tok := "ghs_" + base64.RawURLEncoding.EncodeToString(b)
		s.mu.Lock()
		s.tokens[tok] = body.Repositories
		s.tokenIns[tok] = id
		s.mu.Unlock()
		s.json(w, http.StatusCreated, map[string]any{"token": tok, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		return
	}
	s.fail(w, http.StatusNotFound, "Not Found")
}
