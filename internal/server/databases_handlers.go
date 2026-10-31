package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wiefix/basalt/internal/agentproto"
	"github.com/wiefix/basalt/internal/mariadb"
	"github.com/wiefix/basalt/internal/store"
)

type databaseView struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

func viewDatabase(d *store.Database) databaseView {
	return databaseView{ID: d.ID, Name: d.Name, Username: d.Username, CreatedAt: d.CreatedAt}
}

// dbCredentialsView is how a site connects to its database (shown once).
type dbCredentialsView struct {
	Host     string `json:"host"`
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// Connection details shown with credentials. PHP on the same host connects
// over the local socket; "localhost" makes PHP drivers use it.
func dbConnection(name, user, password string) dbCredentialsView {
	return dbCredentialsView{Host: "localhost", Database: name, Username: user, Password: password}
}

func (s *Server) dbCall(ctx context.Context, serverID int64, req agentproto.DatabaseRequest) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.agents.Call(ctx, serverID, agentproto.TypeDatabase, req, nil)
}

func (s *Server) handleListDatabases(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	list, err := s.db.Databases(r.Context(), site.ID)
	if err != nil {
		s.internalError(w, r, "list databases", err)
		return
	}
	out := make([]databaseView, len(list))
	for i, d := range list {
		out[i] = viewDatabase(d)
	}
	version, _ := s.db.ServerMariaDB(r.Context(), site.ServerID)
	writeJSON(w, http.StatusOK, map[string]any{"databases": out, "mariadb": version})
}

type createDatabaseRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleCreateDatabase(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	var req createDatabaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg, err := s.replicatedDBProblem(r, site); err != nil {
		s.internalError(w, r, "check servers", err)
		return
	} else if msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	// Prefixing with the site id keeps names unique per server and makes the
	// owner obvious in MariaDB tooling.
	short := strings.ToLower(strings.TrimSpace(req.Name))
	name := "s" + strconv.FormatInt(site.ID, 10) + "_" + short
	if short == "" || !mariadb.ValidName(name) || strings.Contains(short, "__") {
		writeError(w, http.StatusUnprocessableEntity, "name must be letters, digits and _ (database names are limited to 32 characters including the site prefix)")
		return
	}
	if v, _ := s.db.ServerMariaDB(r.Context(), site.ServerID); v == "" {
		writeError(w, http.StatusUnprocessableEntity, "MariaDB is not installed on this server")
		return
	}
	if !s.agents.Connected(site.ServerID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	// Reserve the name first so concurrent requests cannot both create it.
	d, err := s.db.CreateDatabaseRecord(r.Context(), site.ID, site.ServerID, name, name)
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "a database with this name already exists")
		return
	}
	if err != nil {
		s.internalError(w, r, "create database record", err)
		return
	}
	password := rand.Text()
	if err := s.dbCall(r.Context(), site.ServerID, agentproto.DatabaseRequest{
		Op: agentproto.DBCreate, Name: name, User: name, Password: password,
	}); err != nil {
		s.db.DeleteDatabaseRecord(context.WithoutCancel(r.Context()), d.ID)
		writeAgentError(w, err)
		return
	}
	s.audit(r, &p.user.ID, "database.created", map[string]any{"site_id": site.ID, "database": name})
	if len(site.Replicas) > 0 {
		// The copies reach the databases on the site's server (or no longer).
		if err := s.ensureReplicas(r.Context(), site); err != nil {
			s.log.Warn("ensure site replicas", "site_id", site.ID, "err", err)
		}
		s.startDBReplication()
	}
	// The password is shown once, never stored.
	writeJSON(w, http.StatusCreated, map[string]any{"database": viewDatabase(d), "credentials": dbConnection(name, name, password)})
}

func (s *Server) pathDatabase(w http.ResponseWriter, r *http.Request) (*store.Site, *store.Database, bool) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return nil, nil, false
	}
	id, err := strconv.ParseInt(r.PathValue("did"), 10, 64)
	if err == nil {
		if d, err := s.db.DatabaseByID(r.Context(), site.ID, id); err == nil {
			return site, d, true
		}
	}
	writeError(w, http.StatusNotFound, "database not found")
	return nil, nil, false
}

func (s *Server) handleResetDatabasePassword(w http.ResponseWriter, r *http.Request, p *principal) {
	site, d, ok := s.pathDatabase(w, r)
	if !ok {
		return
	}
	password := rand.Text()
	if err := s.dbCall(r.Context(), site.ServerID, agentproto.DatabaseRequest{
		Op: agentproto.DBPassword, Name: d.Name, User: d.Username, Password: password,
	}); err != nil {
		writeAgentError(w, err)
		return
	}
	s.audit(r, &p.user.ID, "database.password_reset", map[string]any{"site_id": site.ID, "database": d.Name})
	writeJSON(w, http.StatusOK, map[string]any{"credentials": dbConnection(d.Name, d.Username, password)})
}

func (s *Server) handleDeleteDatabase(w http.ResponseWriter, r *http.Request, p *principal) {
	site, d, ok := s.pathDatabase(w, r)
	if !ok {
		return
	}
	if err := s.dbCall(r.Context(), site.ServerID, agentproto.DatabaseRequest{
		Op: agentproto.DBDrop, Name: d.Name, User: d.Username,
	}); err != nil {
		writeAgentError(w, err)
		return
	}
	if err := s.db.DeleteDatabaseRecord(r.Context(), d.ID); err != nil {
		s.internalError(w, r, "delete database record", err)
		return
	}
	s.audit(r, &p.user.ID, "database.deleted", map[string]any{"site_id": site.ID, "database": d.Name})
	if len(site.Replicas) > 0 {
		// The copies reach the databases on the site's server (or no longer).
		if err := s.ensureReplicas(r.Context(), site); err != nil {
			s.log.Warn("ensure site replicas", "site_id", site.ID, "err", err)
		}
		s.startDBReplication()
	}
	w.WriteHeader(http.StatusNoContent)
}

type siteInstallRequest struct {
	App string `json:"app"` // wordpress, laravel, symfony
	// WordPress: its admin account, made at install with a password the
	// panel makes and shows once (admin_email: the user's own by default),
	// and the site's title (the site's name by default).
	AdminUser  string `json:"admin_user,omitempty"`
	AdminEmail string `json:"admin_email,omitempty"`
	Title      string `json:"title,omitempty"`
}

// wordpressAdmin is WordPress's admin account, as made at install: the
// password is shown this once.
type wordpressAdmin struct {
	User     string `json:"user"`
	Email    string `json:"email"`
	Password string `json:"password"`
	LoginURL string `json:"login_url"`
}

// siteApps are the applications a site can get, with the suffix of their
// database's name.
var siteApps = map[string]struct{ name, db string }{
	agentproto.AppWordPress: {"WordPress", "_wp"},
	agentproto.AppLaravel:   {"Laravel", "_lv"},
	agentproto.AppSymfony:   {"Symfony", "_sf"},
}

// handleSiteInstall installs an application (WordPress, Laravel, Symfony) into a
// PHP site's empty public folder, with a new database for it.
func (s *Server) handleSiteInstall(w http.ResponseWriter, r *http.Request, p *principal) {
	site, ok := s.pathSite(w, r)
	if !ok {
		return
	}
	var req siteInstallRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	app, ok := siteApps[req.App]
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "unknown application (wordpress, laravel and symfony are available)")
		return
	}
	if site.PHPVersion == "" {
		writeError(w, http.StatusUnprocessableEntity, app.name+" needs PHP: switch the site to a PHP version first")
		return
	}
	if msg, err := s.replicatedDBProblem(r, site); err != nil {
		s.internalError(w, r, "check servers", err)
		return
	} else if msg != "" {
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	// The site's address: its first domain that is not a wildcard.
	url := ""
	for _, d := range site.Domains {
		if !strings.HasPrefix(d, "*.") {
			url = "https://" + d
			break
		}
	}
	var admin *wordpressAdmin
	if req.App == agentproto.AppWordPress {
		if req.AdminEmail == "" {
			req.AdminEmail = p.user.Email
		}
		if req.Title == "" {
			req.Title = site.Name
		}
		switch {
		case url == "":
			writeError(w, http.StatusUnprocessableEntity, "WordPress needs a domain that is not a wildcard (its address)")
			return
		case !agentproto.ValidWordPressUser(req.AdminUser):
			writeError(w, http.StatusUnprocessableEntity, "admin_user: letters, digits, dots, dashes and underscores, up to 60")
			return
		case !agentproto.ValidEmail(req.AdminEmail):
			writeError(w, http.StatusUnprocessableEntity, "admin_email: an email address is needed")
			return
		case !agentproto.ValidSiteTitle(req.Title):
			writeError(w, http.StatusUnprocessableEntity, "title: one line, up to 200 bytes")
			return
		}
		admin = &wordpressAdmin{User: req.AdminUser, Email: req.AdminEmail, Password: rand.Text(), LoginURL: url + "/wp-login.php"}
	}
	if v, _ := s.db.ServerMariaDB(r.Context(), site.ServerID); v == "" {
		writeError(w, http.StatusUnprocessableEntity, app.name+" needs MariaDB, which is not installed on this server")
		return
	}
	if !s.agents.Connected(site.ServerID) {
		writeError(w, http.StatusConflict, "the server is offline")
		return
	}
	var d *store.Database
	var err error
	for i := 1; i <= 9; i++ {
		name := "s" + strconv.FormatInt(site.ID, 10) + app.db
		if i > 1 {
			name += strconv.Itoa(i)
		}
		d, err = s.db.CreateDatabaseRecord(r.Context(), site.ID, site.ServerID, name, name)
		if !errors.Is(err, store.ErrNameTaken) {
			break
		}
	}
	if err != nil {
		s.internalError(w, r, "create database record", err)
		return
	}
	password := rand.Text()
	if err := s.dbCall(r.Context(), site.ServerID, agentproto.DatabaseRequest{
		Op: agentproto.DBCreate, Name: d.Name, User: d.Username, Password: password,
	}); err != nil {
		s.db.DeleteDatabaseRecord(context.WithoutCancel(r.Context()), d.ID)
		writeAgentError(w, err)
		return
	}
	install := agentproto.SiteInstall{
		SiteID: site.ID, App: req.App, DBName: d.Name, DBUser: d.Username, DBPassword: password,
		PHPVersion: site.PHPVersion, URL: url,
	}
	if admin != nil {
		install.Title, install.AdminUser, install.AdminEmail, install.AdminPassword = req.Title, admin.User, admin.Email, admin.Password
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	var resp agentproto.SiteInstallResponse
	err = s.agents.Stream(ctx, site.ServerID, agentproto.TypeSiteInstall, install, &resp, func(json.RawMessage) error { return nil })
	if err != nil {
		// Nothing half-done stays: the new database goes again.
		cleanup := context.WithoutCancel(r.Context())
		if derr := s.dbCall(cleanup, site.ServerID, agentproto.DatabaseRequest{Op: agentproto.DBDrop, Name: d.Name, User: d.Username}); derr == nil {
			s.db.DeleteDatabaseRecord(cleanup, d.ID)
		}
		writeAgentError(w, err)
		return
	}
	ev := map[string]any{"site_id": site.ID, "app": req.App, "version": resp.Version, "database": d.Name}
	if admin != nil {
		ev["admin_user"] = admin.User
	}
	s.audit(r, &p.user.ID, "site.app_installed", ev)
	if len(site.Replicas) > 0 {
		// The copies reach the databases on the site's server (or no longer).
		if err := s.ensureReplicas(r.Context(), site); err != nil {
			s.log.Warn("ensure site replicas", "site_id", site.ID, "err", err)
		}
		s.startDBReplication()
	}
	out := map[string]any{"app": req.App, "version": resp.Version, "files": resp.Files, "database": viewDatabase(d)}
	if url != "" {
		out["url"] = url + "/"
	}
	if admin != nil {
		out["admin"] = admin
	}
	writeJSON(w, http.StatusCreated, out)
}
