package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wiefix/basalt/internal/store"
)

// Projects group sites, apps (container databases included), DNS zones and
// health checks. A user's role in a project applies to its resources (and
// a global role to all of them). Global admins create and delete projects
// and move resources between them; project admins manage the members.

type projectView struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Resources   int    `json:"resources"`
	Members     int    `json:"members"`
	Default     bool   `json:"default"`
	// Role is what the caller may do in the project (their global role
	// counts in every project).
	Role      store.Role `json:"role"`
	CreatedAt time.Time  `json:"created_at"`
}

func viewProject(p *store.Project, role store.Role) projectView {
	return projectView{ID: p.ID, Name: p.Name, Description: p.Description, Resources: p.Resources, Members: p.Members,
		Default: p.ID == store.DefaultProject, Role: role, CreatedAt: p.CreatedAt}
}

// viewProjectFor describes a project with the principal's role in it.
func (s *Server) viewProjectFor(r *http.Request, p *principal, pr *store.Project) projectView {
	role, _ := s.roleIn(r.Context(), p, pr.ID)
	return viewProject(pr, role)
}

type projectMemberView struct {
	UserID    int64      `json:"user_id"`
	Email     string     `json:"email"`
	Name      string     `json:"name"`
	Role      store.Role `json:"role"`
	CreatedAt time.Time  `json:"created_at"`
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := s.db.Projects(r.Context())
	if err != nil {
		s.internalError(w, r, "list projects", err)
		return
	}
	out := []projectView{}
	for _, x := range list {
		// Users see the projects they belong to; global roles see all.
		role, err := s.roleIn(r.Context(), p, x.ID)
		if err != nil {
			s.internalError(w, r, "load project roles", err)
			return
		}
		if role.AtLeast(store.RoleViewer) {
			out = append(out, viewProject(x, role))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

// pathProject loads the project {id}; the principal needs min in it (a
// project it cannot see is not found).
func (s *Server) pathProject(w http.ResponseWriter, r *http.Request, p *principal, min store.Role) (*store.Project, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		pr, err := s.db.Project(r.Context(), id)
		if err == nil {
			role, err := s.roleIn(r.Context(), p, pr.ID)
			if err != nil {
				s.internalError(w, r, "load project roles", err)
				return nil, false
			}
			if !roleAllows(w, role, min, "project not found") {
				return nil, false
			}
			return pr, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "load project", err)
			return nil, false
		}
	}
	writeError(w, http.StatusNotFound, "project not found")
	return nil, false
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request, p *principal) {
	pr, ok := s.pathProject(w, r, p, store.RoleViewer)
	if !ok {
		return
	}
	members, err := s.db.ProjectMembers(r.Context(), pr.ID)
	if err != nil {
		s.internalError(w, r, "list project members", err)
		return
	}
	out := make([]projectMemberView, len(members))
	for i, m := range members {
		out[i] = projectMemberView{UserID: m.UserID, Email: m.Email, Name: m.Name, Role: m.Role, CreatedAt: m.CreatedAt}
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": s.viewProjectFor(r, p, pr), "members": out})
}

type projectRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// check validates and trims a project's name and description.
func (req *projectRequest) check() error {
	req.Name, req.Description = strings.TrimSpace(req.Name), strings.TrimSpace(req.Description)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 64 {
		return errors.New("name: 1 to 64 characters")
	}
	if utf8.RuneCountInString(req.Description) > 500 {
		return errors.New("description: at most 500 characters")
	}
	for _, c := range req.Name + req.Description {
		if unicode.IsControl(c) && c != '\n' {
			return errors.New("name and description cannot hold control characters")
		}
	}
	if strings.ContainsRune(req.Name, '\n') {
		return errors.New("name: one line")
	}
	return nil
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request, p *principal) {
	var req projectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.check(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	pr, err := s.db.CreateProject(r.Context(), req.Name, req.Description)
	if errors.Is(err, store.ErrNameTaken) {
		writeError(w, http.StatusConflict, "a project with this name exists")
		return
	}
	if err != nil {
		s.internalError(w, r, "create project", err)
		return
	}
	s.audit(r, &p.user.ID, "project.created", map[string]any{"project_id": pr.ID, "name": pr.Name})
	writeJSON(w, http.StatusCreated, map[string]any{"project": s.viewProjectFor(r, p, pr)})
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request, p *principal) {
	pr, ok := s.pathProject(w, r, p, store.RoleAdmin)
	if !ok {
		return
	}
	var req projectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.check(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	switch err := s.db.UpdateProject(r.Context(), pr.ID, req.Name, req.Description); {
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, "a project with this name exists")
		return
	case err != nil:
		s.internalError(w, r, "update project", err)
		return
	}
	s.audit(r, &p.user.ID, "project.updated", map[string]any{"project_id": pr.ID, "name": req.Name})
	pr, _ = s.db.Project(r.Context(), pr.ID)
	writeJSON(w, http.StatusOK, map[string]any{"project": s.viewProjectFor(r, p, pr)})
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request, p *principal) {
	pr, ok := s.pathProject(w, r, p, store.RoleAdmin)
	if !ok {
		return
	}
	switch err := s.db.DeleteProject(r.Context(), pr.ID); {
	case errors.Is(err, store.ErrDefaultProject), errors.Is(err, store.ErrProjectNotEmpty):
		writeError(w, http.StatusConflict, err.Error())
		return
	case err != nil && !errors.Is(err, store.ErrNotFound):
		s.internalError(w, r, "delete project", err)
		return
	}
	s.audit(r, &p.user.ID, "project.deleted", map[string]any{"project_id": pr.ID, "name": pr.Name})
	w.WriteHeader(http.StatusNoContent)
}

type projectMemberRequest struct {
	Role store.Role `json:"role"`
}

// handleSetProjectMember adds a user to a project or changes their role.
func (s *Server) handleSetProjectMember(w http.ResponseWriter, r *http.Request, p *principal) {
	// The project's admins manage its members.
	pr, ok := s.pathProject(w, r, p, store.RoleAdmin)
	if !ok {
		return
	}
	uid, _ := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	u, err := s.db.UserByID(r.Context(), uid)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	} else if err != nil {
		s.internalError(w, r, "load user", err)
		return
	}
	var req projectMemberRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !store.ValidProjectRole(req.Role) {
		writeError(w, http.StatusUnprocessableEntity, "a project role is admin, member or viewer")
		return
	}
	if err := s.db.SetProjectMember(r.Context(), pr.ID, u.ID, req.Role); err != nil {
		s.internalError(w, r, "set project member", err)
		return
	}
	s.audit(r, &p.user.ID, "project.member_set", map[string]any{"project_id": pr.ID, "user_id": u.ID, "email": u.Email, "role": req.Role})
	w.WriteHeader(http.StatusNoContent)
}

type addMemberRequest struct {
	Email string     `json:"email"`
	Role  store.Role `json:"role"`
}

// handleAddProjectMember adds a user, found by email, to a project (or
// changes their role): the project's admins cannot list the panel's users.
func (s *Server) handleAddProjectMember(w http.ResponseWriter, r *http.Request, p *principal) {
	pr, ok := s.pathProject(w, r, p, store.RoleAdmin)
	if !ok {
		return
	}
	var req addMemberRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !store.ValidProjectRole(req.Role) {
		writeError(w, http.StatusUnprocessableEntity, "a project role is admin, member or viewer")
		return
	}
	email, err := normalizeEmail(req.Email)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	u, err := s.db.UserByEmail(r.Context(), email)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "no user with this email: an administrator of the panel adds users first")
		return
	} else if err != nil {
		s.internalError(w, r, "load user", err)
		return
	}
	if err := s.db.SetProjectMember(r.Context(), pr.ID, u.ID, req.Role); err != nil {
		s.internalError(w, r, "set project member", err)
		return
	}
	s.audit(r, &p.user.ID, "project.member_set", map[string]any{"project_id": pr.ID, "user_id": u.ID, "email": u.Email, "role": req.Role})
	writeJSON(w, http.StatusCreated, map[string]any{"member": projectMemberView{UserID: u.ID, Email: u.Email, Name: u.Name, Role: req.Role, CreatedAt: s.now()}})
}

func (s *Server) handleRemoveProjectMember(w http.ResponseWriter, r *http.Request, p *principal) {
	// The project's admins manage its members.
	pr, ok := s.pathProject(w, r, p, store.RoleAdmin)
	if !ok {
		return
	}
	uid, _ := strconv.ParseInt(r.PathValue("uid"), 10, 64)
	if err := s.db.RemoveProjectMember(r.Context(), pr.ID, uid); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not a member of this project")
		return
	} else if err != nil {
		s.internalError(w, r, "remove project member", err)
		return
	}
	s.audit(r, &p.user.ID, "project.member_removed", map[string]any{"project_id": pr.ID, "user_id": uid})
	w.WriteHeader(http.StatusNoContent)
}

type moveRequest struct {
	ProjectID int64 `json:"project_id"`
}

// handleMove returns the handler that moves a site, app, DNS zone or
// health check to another project.
func (s *Server) handleMove(kind store.ProjectResource) principalHandler {
	return func(w http.ResponseWriter, r *http.Request, p *principal) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		var req moveRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		// Domains stay with the project of their DNS zone.
		var check error
		switch kind {
		case store.ResourceApp:
			app, err := s.db.AppByID(r.Context(), id)
			if err == nil && app.Source == store.SourceMail {
				writeError(w, http.StatusUnprocessableEntity, "the mail server belongs to no project")
				return
			}
			if err == nil {
				check = s.checkDomainProjects(r.Context(), app.Domains, req.ProjectID)
			}
		case store.ResourceSite:
			if site, err := s.db.SiteByID(r.Context(), id); err == nil {
				check = s.checkDomainProjects(r.Context(), site.Domains, req.ProjectID)
			}
		case store.ResourceDNSZone:
			if z, err := s.db.DNSZone(r.Context(), id); err == nil {
				check = s.checkZoneProjects(r.Context(), z.Name, req.ProjectID)
			}
		}
		if s.domainError(w, r, check) {
			return
		}
		switch err := s.db.MoveToProject(r.Context(), kind, id, req.ProjectID); {
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "not found")
			return
		case errors.Is(err, store.ErrNoProject):
			writeError(w, http.StatusUnprocessableEntity, "project not found")
			return
		case errors.Is(err, store.ErrProjectMismatch), errors.Is(err, store.ErrReplicaCheckMove):
			writeError(w, http.StatusConflict, err.Error())
			return
		case err != nil:
			s.internalError(w, r, "move to project", err)
			return
		}
		s.audit(r, &p.user.ID, "project.resource_moved", map[string]any{"kind": string(kind), "id": id, "project_id": req.ProjectID})
		if kind == store.ResourceApp || kind == store.ResourceHealthCheck {
			// The checks in memory carry their project.
			if err := s.loadHealth(r.Context()); err != nil {
				s.log.Error("load health checks", "err", err)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// checkCreate checks that the principal may create resources (admin) in
// project (0: the default one) and returns it; it writes the error
// response itself.
func (s *Server) checkCreate(w http.ResponseWriter, r *http.Request, p *principal, project int64) (int64, bool) {
	project = store.ProjectOrDefault(project)
	role, err := s.roleIn(r.Context(), p, project)
	if err != nil {
		s.internalError(w, r, "load project roles", err)
		return 0, false
	}
	if !role.AtLeast(store.RoleViewer) {
		writeError(w, http.StatusUnprocessableEntity, "project not found")
		return 0, false
	}
	if !role.AtLeast(store.RoleAdmin) {
		writeError(w, http.StatusForbidden, "insufficient permissions in this project")
		return 0, false
	}
	p.project = project
	return project, true
}

// visibleIn returns a filter of the projects whose resources the principal
// may see.
func (s *Server) visibleIn(r *http.Request, p *principal) (func(project int64) bool, error) {
	if p.user.Role.AtLeast(store.RoleViewer) {
		return func(int64) bool { return true }, nil
	}
	if _, err := s.roleIn(r.Context(), p, 0); err != nil { // loads the memberships
		return nil, err
	}
	return func(project int64) bool {
		role, _ := s.roleIn(r.Context(), p, project)
		return role.AtLeast(store.RoleViewer)
	}, nil
}

// projectError answers a create request naming an unknown project; it
// reports whether it did.
func projectError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, store.ErrNoProject) {
		writeError(w, http.StatusUnprocessableEntity, "project not found")
		return true
	}
	return false
}
