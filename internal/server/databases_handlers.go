package server

import (
	"context"
	"crypto/rand"
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

// Connection details shown with credentials. PHP on the same host connects
// over the local socket; "localhost" makes PHP drivers use it.
func dbConnection(name, user string) map[string]string {
	return map[string]string{"host": "localhost", "database": name, "username": user}
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
	conn := dbConnection(name, name)
	conn["password"] = password // shown once, never stored
	writeJSON(w, http.StatusCreated, map[string]any{"database": viewDatabase(d), "credentials": conn})
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
	conn := dbConnection(d.Name, d.Username)
	conn["password"] = password
	writeJSON(w, http.StatusOK, map[string]any{"credentials": conn})
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
	w.WriteHeader(http.StatusNoContent)
}
