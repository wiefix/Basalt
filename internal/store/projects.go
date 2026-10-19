package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	Limits    ProjectLimits
	// DiskAlerted: the project's admins were told that its sites went
	// over Limits.DiskMB (and were not under it since).
	DiskAlerted bool
	CreatedAt   time.Time
}

// ProjectLimits caps what a project holds; 0 is no limit. The counts are
// hard: creating or moving in one more is refused. The disk limit (files
// and MariaDB databases of the sites) is soft: the project's admins hear
// when the sites go over it.
type ProjectLimits struct {
	Sites     int `json:"sites"`
	Apps      int `json:"apps"`      // apps other than databases
	Databases int `json:"databases"` // container databases
	DNSZones  int `json:"dns_zones"`
	// HealthChecks counts the checks made by hand (those of an app on
	// several servers come with the app).
	HealthChecks int `json:"health_checks"`
	DiskMB       int `json:"disk_mb"`
}

// ProjectUsage is what a project holds, counted as ProjectLimits counts.
type ProjectUsage struct {
	Sites, Apps, Databases, DNSZones, HealthChecks int
	// DiskBytes adds up the last measurement of the sites.
	DiskBytes int64
}

// LimitError refuses one more resource in a project at its limit.
type LimitError struct {
	What  string // "sites", "apps", …
	Limit int
}

func (e *LimitError) Error() string {
	what := strings.ReplaceAll(e.What, "_", " ")
	what = strings.Replace(what, "dns", "DNS", 1)
	if e.Limit == 1 {
		what = strings.TrimSuffix(what, "s")
	}
	return fmt.Sprintf("the project may hold at most %d %s: an admin of the panel can raise its limits", e.Limit, what)
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
	(SELECT COUNT(*) FROM project_members WHERE project_id = p.id), p.limits, p.disk_alerted`

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	var p Project
	var created int64
	var limits string
	err := row.Scan(&p.ID, &p.Name, &p.Description, &created, &p.Resources, &p.Members, &limits, &p.DiskAlerted)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(limits), &p.Limits); err != nil {
		return nil, fmt.Errorf("project %d limits: %w", p.ID, err)
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

type querier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func projectUsage(ctx context.Context, q querier, id int64) (ProjectUsage, error) {
	var u ProjectUsage
	err := q.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM sites WHERE project_id = ?1),
		(SELECT COUNT(*) FROM apps WHERE project_id = ?1 AND source NOT IN ('database', 'mail')),
		(SELECT COUNT(*) FROM apps WHERE project_id = ?1 AND source = 'database'),
		(SELECT COUNT(*) FROM dns_zones WHERE project_id = ?1),
		(SELECT COUNT(*) FROM health_checks WHERE project_id = ?1
		 AND id NOT IN (SELECT health_check_id FROM proxy_routes WHERE health_check_id IS NOT NULL)),
		(SELECT COALESCE(SUM(disk_bytes + db_bytes), 0) FROM sites WHERE project_id = ?1)`, id).
		Scan(&u.Sites, &u.Apps, &u.Databases, &u.DNSZones, &u.HealthChecks, &u.DiskBytes)
	return u, err
}

// ProjectUsage reports what a project holds.
func (db *DB) ProjectUsage(ctx context.Context, id int64) (ProjectUsage, error) {
	return projectUsage(ctx, db.R, id)
}

// SetProjectLimits changes a project's limits; what it holds already stays
// (a project over a new limit only takes no more).
func (db *DB) SetProjectLimits(ctx context.Context, id int64, l ProjectLimits) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return db.execOne(ctx, `UPDATE projects SET limits = ?, disk_alerted = 0 WHERE id = ?`, string(b), id)
}

func (db *DB) SetProjectDiskAlerted(ctx context.Context, id int64, alerted bool) error {
	return db.execOne(ctx, `UPDATE projects SET disk_alerted = ? WHERE id = ?`, alerted, id)
}

// checkRoom refuses one more resource of a kind ("sites", "apps",
// "databases", "dns_zones", "health_checks") in a project at its limit;
// it runs in the transaction that adds the resource.
func checkRoom(ctx context.Context, q querier, project int64, what string) error {
	var limits string
	err := q.QueryRowContext(ctx, `SELECT limits FROM projects WHERE id = ?`, project).Scan(&limits)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoProject
	}
	if err != nil {
		return err
	}
	var l ProjectLimits
	if err := json.Unmarshal([]byte(limits), &l); err != nil {
		return fmt.Errorf("project %d limits: %w", project, err)
	}
	limit := map[string]int{"sites": l.Sites, "apps": l.Apps, "databases": l.Databases,
		"dns_zones": l.DNSZones, "health_checks": l.HealthChecks}[what]
	if limit <= 0 {
		return nil
	}
	u, err := projectUsage(ctx, q, project)
	if err != nil {
		return err
	}
	have := map[string]int{"sites": u.Sites, "apps": u.Apps, "databases": u.Databases,
		"dns_zones": u.DNSZones, "health_checks": u.HealthChecks}[what]
	if have >= limit {
		return &LimitError{What: what, Limit: limit}
	}
	return nil
}

// appKind is what an app of a source counts as in a project's limits ("":
// nothing, the mail server serves every project).
func appKind(source string) string {
	switch source {
	case SourceMail:
		return ""
	case SourceDatabase:
		return "databases"
	}
	return "apps"
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
	// One more in the project it goes to.
	if kind == ResourceSite || kind == ResourceApp || kind == ResourceDNSZone || kind == ResourceHealthCheck {
		var current int64
		what := string(kind)
		var err error
		if kind == ResourceApp {
			var source string
			err = tx.QueryRowContext(ctx, `SELECT project_id, source FROM apps WHERE id = ?`, id).Scan(&current, &source)
			what = appKind(source)
		} else {
			err = tx.QueryRowContext(ctx, `SELECT project_id FROM `+string(kind)+` WHERE id = ?`, id).Scan(&current)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if current != projectID && what != "" {
			if err := checkRoom(ctx, tx, projectID, what); err != nil {
				return err
			}
		}
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
