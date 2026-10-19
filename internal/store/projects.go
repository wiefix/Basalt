package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// DefaultProject holds every resource created without a project, and all
// those that existed before projects; it cannot be deleted.
const DefaultProject int64 = 1

// Project groups sites, apps (container databases included), DNS zones and
// health checks; users get a role in the projects they belong to.
type Project struct {
	ID          int64
	Name        string
	Description string
	// Resources counts what the project holds (sites, apps, zones, checks).
	Resources int
	Members   int
	CreatedAt time.Time
}

// ProjectMember is a user's role in a project.
type ProjectMember struct {
	UserID    int64
	Email     string
	Name      string
	Role      Role // admin, member or viewer
	CreatedAt time.Time
}

var (
	ErrProjectNotEmpty  = errors.New("the project still holds sites, apps, DNS zones or health checks: move or delete them first")
	ErrDefaultProject   = errors.New("the default project cannot be deleted")
	ErrNoProject        = errors.New("no such project")
	ErrProjectMismatch  = errors.New("a database and the apps linked to it must be in the same project")
	ErrReplicaCheckMove = errors.New("this health check belongs to an app on several servers: it moves with the app")
)

// ValidProjectRole reports whether r is a role within a project.
func ValidProjectRole(r Role) bool {
	return r == RoleAdmin || r == RoleMember || r == RoleViewer
}

// ProjectResource is a kind of resource a project holds (MoveToProject).
type ProjectResource string

const (
	ResourceSite        ProjectResource = "sites"
	ResourceApp         ProjectResource = "apps"
	ResourceDNSZone     ProjectResource = "dns_zones"
	ResourceHealthCheck ProjectResource = "health_checks"
)

const projectColumns = `p.id, p.name, p.description, p.created_at,
	(SELECT COUNT(*) FROM sites WHERE project_id = p.id) + (SELECT COUNT(*) FROM apps WHERE project_id = p.id) +
	(SELECT COUNT(*) FROM dns_zones WHERE project_id = p.id) + (SELECT COUNT(*) FROM health_checks WHERE project_id = p.id),
	(SELECT COUNT(*) FROM project_members WHERE project_id = p.id)`

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	var created int64
	err := row.Scan(&p.ID, &p.Name, &p.Description, &created, &p.Resources, &p.Members)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.CreatedAt = time.Unix(created, 0)
	return &p, nil
}

func projectNameTaken(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: projects.name") {
		return ErrNameTaken
	}
	return err
}

// Projects lists the projects, the default one first, then by name.
func (db *DB) Projects(ctx context.Context) ([]*Project, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects p ORDER BY p.id != 1, p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (db *DB) Project(ctx context.Context, id int64) (*Project, error) {
	return scanProject(db.R.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects p WHERE p.id = ?`, id))
}

func (db *DB) CreateProject(ctx context.Context, name, description string) (*Project, error) {
	var id int64
	err := db.W.QueryRowContext(ctx, `INSERT INTO projects (name, description) VALUES (?, ?) RETURNING id`, name, description).Scan(&id)
	if err != nil {
		return nil, projectNameTaken(err)
	}
	return db.Project(ctx, id)
}

func (db *DB) UpdateProject(ctx context.Context, id int64, name, description string) error {
	return projectNameTaken(db.execOne(ctx, `UPDATE projects SET name = ?, description = ? WHERE id = ?`, name, description, id))
}

// DeleteProject removes an empty project (its memberships go with it).
func (db *DB) DeleteProject(ctx context.Context, id int64) error {
	if id == DefaultProject {
		return ErrDefaultProject
	}
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM sites WHERE project_id = ?) + (SELECT COUNT(*) FROM apps WHERE project_id = ?) +
		(SELECT COUNT(*) FROM dns_zones WHERE project_id = ?) + (SELECT COUNT(*) FROM health_checks WHERE project_id = ?)`,
		id, id, id, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrProjectNotEmpty
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// ProjectMembers lists a project's members by name.
func (db *DB) ProjectMembers(ctx context.Context, projectID int64) ([]ProjectMember, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT m.user_id, u.email, u.name, m.role, m.created_at FROM project_members m JOIN users u ON u.id = m.user_id
		 WHERE m.project_id = ? ORDER BY u.name, u.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProjectMember{}
	for rows.Next() {
		var m ProjectMember
		var created int64
		if err := rows.Scan(&m.UserID, &m.Email, &m.Name, &m.Role, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(created, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetProjectMember adds a user to a project or changes their role there.
func (db *DB) SetProjectMember(ctx context.Context, projectID, userID int64, role Role) error {
	if !ValidProjectRole(role) {
		return errors.New("a project role is admin, member or viewer")
	}
	_, err := db.W.ExecContext(ctx,
		`INSERT INTO project_members (project_id, user_id, role) VALUES (?, ?, ?)
		 ON CONFLICT (project_id, user_id) DO UPDATE SET role = excluded.role`, projectID, userID, role)
	if err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		return ErrNotFound
	}
	return err
}

func (db *DB) RemoveProjectMember(ctx context.Context, projectID, userID int64) error {
	return db.execOne(ctx, `DELETE FROM project_members WHERE project_id = ? AND user_id = ?`, projectID, userID)
}

// UserProjectRoles maps the projects a user belongs to to their role there.
func (db *DB) UserProjectRoles(ctx context.Context, userID int64) (map[int64]Role, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT project_id, role FROM project_members WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Role{}
	for rows.Next() {
		var id int64
		var r Role
		if err := rows.Scan(&id, &r); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

// projectExists reports ErrNoProject for an unknown project.
func projectExists(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNoProject
	}
	return nil
}

// projectOrDefault is the project a new resource goes to.
func projectOrDefault(id int64) int64 { return ProjectOrDefault(id) }

// ProjectOrDefault is the project a new resource with project id goes to
// (0: the default one).
func ProjectOrDefault(id int64) int64 {
	if id <= 0 {
		return DefaultProject
	}
	return id
}

// MoveToProject moves a resource to another project. An app takes the
// health checks of its placements along; an app and the databases linked
// to it must end up in the same project (the mail server, which serves
// every project, aside); the check of an app's placement
// moves only with the app.
func (db *DB) MoveToProject(ctx context.Context, kind ProjectResource, id, projectID int64) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := projectExists(ctx, tx, projectID); err != nil {
		return err
	}
	switch kind {
	case ResourceApp:
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM app_links l JOIN apps o ON o.id = CASE WHEN l.database_id = ? THEN l.app_id ELSE l.database_id END
			 WHERE (l.database_id = ? OR l.app_id = ?) AND o.project_id != ? AND o.source != 'mail'`, id, id, id, projectID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrProjectMismatch
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE health_checks SET project_id = ? WHERE id IN (SELECT health_check_id FROM proxy_routes WHERE app_id = ?)`,
			projectID, id); err != nil {
			return err
		}
	case ResourceHealthCheck:
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM proxy_routes WHERE health_check_id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrReplicaCheckMove
		}
	case ResourceSite, ResourceDNSZone:
	default:
		return errors.New("unknown kind of resource")
	}
	res, err := tx.ExecContext(ctx, `UPDATE `+string(kind)+` SET project_id = ? WHERE id = ?`, projectID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// DomainProject is a domain a site or app of a project answers to.
type DomainProject struct {
	Domain  string
	Project int64
}

// DomainProjects lists the domains of sites and apps with their projects
// (the mail server's names and manual routes belong to no project).
func (db *DB) DomainProjects(ctx context.Context) ([]DomainProject, error) {
	rows, err := db.R.QueryContext(ctx,
		`SELECT DISTINCT d.domain, COALESCE(s.project_id, a.project_id) FROM proxy_route_domains d
		 JOIN proxy_routes r ON r.id = d.route_id
		 LEFT JOIN sites s ON s.id = r.site_id LEFT JOIN apps a ON a.id = r.app_id
		 WHERE s.id IS NOT NULL OR (a.id IS NOT NULL AND a.source != 'mail') ORDER BY d.domain`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DomainProject
	for rows.Next() {
		var d DomainProject
		if err := rows.Scan(&d.Domain, &d.Project); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
