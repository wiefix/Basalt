// Package forgetest is a fake GitLab or Gitea API for tests: one user with
// a token, administering some repositories.
package forgetest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Repo is a repository of the fake.
type Repo struct {
	FullName      string
	Private       bool
	DefaultBranch string
	Branches      []string
}

// Key, Hook and Status are what the panel added.
type Key struct {
	ID          int64
	Repo, Title string
	Key         string
	ReadOnly    bool
}

type Hook struct {
	ID         int64
	Repo, URL  string
	Secret     string
	PushEvents bool
	// PREvents: merge requests (GitLab) or pull requests (Gitea).
	PREvents bool
}

type Status struct {
	Repo, SHA, State, TargetURL, Description, Context string
}

// Server is the fake forge.
type Server struct {
	*httptest.Server
	Kind  string
	Token string
	User  string

	mu       sync.Mutex
	repos    []Repo
	keys     []Key
	hooks    []Hook
	statuses []Status
	next     int64
}

// New starts a fake of kind ("gitlab" or "gitea").
func New(t *testing.T, kind string, repos ...Repo) *Server {
	t.Helper()
	s := &Server{Kind: kind, Token: "glpat-test-token-123", User: "alice", repos: repos, next: 100}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) Keys() []Key   { s.mu.Lock(); defer s.mu.Unlock(); return slices.Clone(s.keys) }
func (s *Server) Hooks() []Hook { s.mu.Lock(); defer s.mu.Unlock(); return slices.Clone(s.hooks) }
func (s *Server) Statuses() []Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.statuses)
}

func (s *Server) json(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, code int, msg string) {
	s.json(w, code, map[string]string{"message": msg})
}

func (s *Server) sshURL(r Repo) string {
	u, _ := url.Parse(s.URL)
	return "git@" + u.Hostname() + ":" + r.FullName + ".git"
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	tok := r.Header.Get("PRIVATE-TOKEN")
	prefix := "/api/v4"
	if s.Kind == "gitea" {
		tok, _ = strings.CutPrefix(r.Header.Get("Authorization"), "token ")
		prefix = "/api/v1"
	}
	if tok != s.Token {
		s.fail(w, http.StatusUnauthorized, "401 Unauthorized")
		return
	}
	p, ok := strings.CutPrefix(r.URL.EscapedPath(), prefix)
	if !ok {
		s.fail(w, http.StatusNotFound, "404 Not Found")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p == "/user" {
		if s.Kind == "gitlab" {
			s.json(w, 200, map[string]any{"username": s.User})
		} else {
			s.json(w, 200, map[string]any{"login": s.User})
		}
		return
	}
	if p == "/projects" || p == "/user/repos" {
		page := r.URL.Query().Get("page")
		out := []map[string]any{}
		if page == "" || page == "1" {
			for _, rp := range s.repos {
				out = append(out, s.view(rp))
			}
		}
		s.json(w, 200, out)
		return
	}
	// The repository, then what of it.
	var full, rest string
	if s.Kind == "gitlab" {
		after, ok := strings.CutPrefix(p, "/projects/")
		if !ok {
			s.fail(w, 404, "404 Not Found")
			return
		}
		enc, r2, _ := strings.Cut(after, "/")
		full, _ = url.PathUnescape(enc)
		rest = r2
	} else {
		after, ok := strings.CutPrefix(p, "/repos/")
		if !ok {
			s.fail(w, 404, "404 Not Found")
			return
		}
		parts := strings.SplitN(after, "/", 3)
		if len(parts) < 2 {
			s.fail(w, 404, "404 Not Found")
			return
		}
		full = parts[0] + "/" + parts[1]
		if len(parts) == 3 {
			rest = parts[2]
		}
	}
	idx := slices.IndexFunc(s.repos, func(x Repo) bool { return strings.EqualFold(x.FullName, full) })
	if idx < 0 {
		s.fail(w, 404, "404 Project Not Found")
		return
	}
	rp := s.repos[idx]
	var body map[string]any
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}
	switch {
	case rest == "" && r.Method == http.MethodGet:
		s.json(w, 200, s.view(rp))
	case (rest == "repository/branches" || rest == "branches") && r.Method == http.MethodGet:
		out := []map[string]string{}
		if pg := r.URL.Query().Get("page"); pg == "" || pg == "1" {
			for _, b := range rp.Branches {
				out = append(out, map[string]string{"name": b})
			}
		}
		s.json(w, 200, out)
	case (rest == "deploy_keys" || rest == "keys") && r.Method == http.MethodPost:
		s.next++
		k := Key{ID: s.next, Repo: rp.FullName, Title: str(body["title"]), Key: str(body["key"])}
		k.ReadOnly = body["read_only"] == true || body["can_push"] == false
		s.keys = append(s.keys, k)
		s.json(w, 201, map[string]any{"id": k.ID})
	case (strings.HasPrefix(rest, "deploy_keys/") || strings.HasPrefix(rest, "keys/")) && r.Method == http.MethodDelete:
		id, _ := strconv.ParseInt(rest[strings.LastIndex(rest, "/")+1:], 10, 64)
		n := len(s.keys)
		s.keys = slices.DeleteFunc(s.keys, func(k Key) bool { return k.ID == id && k.Repo == rp.FullName })
		if len(s.keys) == n {
			s.fail(w, 404, "404 Not found")
			return
		}
		w.WriteHeader(204)
	case rest == "hooks" && r.Method == http.MethodPost:
		s.next++
		h := Hook{ID: s.next, Repo: rp.FullName}
		if s.Kind == "gitlab" {
			h.URL, h.Secret, h.PushEvents = str(body["url"]), str(body["token"]), body["push_events"] == true
			h.PREvents = body["merge_requests_events"] == true
		} else {
			cfg, _ := body["config"].(map[string]any)
			evs, _ := body["events"].([]any)
			h.URL, h.Secret, h.PushEvents = str(cfg["url"]), str(cfg["secret"]), slices.Contains(evs, any("push"))
			h.PREvents = slices.Contains(evs, any("pull_request"))
		}
		s.hooks = append(s.hooks, h)
		s.json(w, 201, map[string]any{"id": h.ID})
	case strings.HasPrefix(rest, "hooks/") && r.Method == http.MethodDelete:
		id, _ := strconv.ParseInt(strings.TrimPrefix(rest, "hooks/"), 10, 64)
		n := len(s.hooks)
		s.hooks = slices.DeleteFunc(s.hooks, func(h Hook) bool { return h.ID == id && h.Repo == rp.FullName })
		if len(s.hooks) == n {
			s.fail(w, 404, "404 Not found")
			return
		}
		w.WriteHeader(204)
	case strings.HasPrefix(rest, "statuses/") && r.Method == http.MethodPost:
		st := Status{Repo: rp.FullName, SHA: strings.TrimPrefix(rest, "statuses/"), State: str(body["state"]),
			TargetURL: str(body["target_url"]), Description: str(body["description"]), Context: str(body["context"])}
		if s.Kind == "gitlab" {
			st.Context = str(body["name"])
		}
		s.statuses = append(s.statuses, st)
		s.json(w, 201, map[string]any{"state": st.State})
	default:
		s.fail(w, 404, "404 Not Found")
	}
}

func (s *Server) view(rp Repo) map[string]any {
	web := s.URL + "/" + rp.FullName
	if s.Kind == "gitlab" {
		vis := "public"
		if rp.Private {
			vis = "private"
		}
		return map[string]any{"path_with_namespace": rp.FullName, "visibility": vis, "default_branch": rp.DefaultBranch,
			"ssh_url_to_repo": s.sshURL(rp), "web_url": web}
	}
	return map[string]any{"full_name": rp.FullName, "private": rp.Private, "default_branch": rp.DefaultBranch,
		"ssh_url": s.sshURL(rp), "html_url": web, "permissions": map[string]bool{"admin": true}}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
