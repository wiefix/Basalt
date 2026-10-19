package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wiefix/basalt/internal/dns"
	"github.com/wiefix/basalt/internal/proxy"
)

func TestProjects(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	list, err := db.Projects(ctx)
	if err != nil || len(list) != 1 || list[0].ID != DefaultProject || list[0].Name != "Default" {
		t.Fatalf("projects at start: %+v %v", list, err)
	}
	shop, err := db.CreateProject(ctx, "Shop", "the web shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(ctx, "shop", ""); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("same name in another case: %v", err)
	}
	if err := db.UpdateProject(ctx, 999, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update a missing project: %v", err)
	}

	// Resources go to the default project unless one is given.
	srv, _ := db.CreateServer(ctx, "s", []byte("t"), time.Now().Add(time.Hour))
	s1, err := db.CreateSite(ctx, 0, srv.ID, "blog", []string{"blog.example.com"}, proxy.TLSAuto, "")
	if err != nil || s1.ProjectID != DefaultProject {
		t.Fatalf("site in the default project: %+v %v", s1, err)
	}
	s2, err := db.CreateSite(ctx, shop.ID, srv.ID, "shop", []string{"shop.example.com"}, proxy.TLSAuto, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := db.SiteByID(ctx, s2.ID); got.ProjectID != shop.ID {
		t.Fatalf("site project: %d", got.ProjectID)
	}
	if _, err := db.CreateSite(ctx, 999, srv.ID, "x", []string{"x.example.com"}, proxy.TLSAuto, ""); !errors.Is(err, ErrNoProject) {
		t.Fatalf("site in a missing project: %v", err)
	}
	check := func(z *dns.Zone) error { return z.Validate() }
	z, err := db.CreateDNSZone(ctx, shop.ID, "example.com", nil, check, time.Now())
	if err != nil || z.ProjectID != shop.ID {
		t.Fatalf("zone: %+v %v", z, err)
	}
	if got, _ := db.DNSZone(ctx, z.ID); got.ProjectID != shop.ID {
		t.Fatalf("zone project: %d", got.ProjectID)
	}
	hc := &HealthCheck{ProjectID: shop.ID, Name: "shop", Kind: "tcp", Host: "192.0.2.1", Port: 80, Interval: 30, Timeout: 5}
	if err := db.CreateHealthCheck(ctx, hc); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.HealthCheck(ctx, hc.ID); got.ProjectID != shop.ID {
		t.Fatalf("check project: %d", got.ProjectID)
	}
	web, err := db.CreateApp(ctx, srv.ID, "web", AppSettings{ProjectID: shop.ID, Image: "nginx:1.27", Port: 80}, []string{}, proxy.TLSAuto)
	if err != nil || web.ProjectID != shop.ID {
		t.Fatalf("app: %+v %v", web, err)
	}
	if got, _ := db.AppByID(ctx, web.ID); got.ProjectID != shop.ID {
		t.Fatalf("app project: %d", got.ProjectID)
	}
	p, _ := db.Project(ctx, shop.ID)
	if p.Resources != 4 {
		t.Fatalf("resources of shop: %d", p.Resources)
	}

	// Members.
	db.CreateFirstOwner(ctx, "o@example.com", "O", "x")
	u, _ := db.CreateUser(ctx, "u@example.com", "U", "x", RoleViewer)
	if err := db.SetProjectMember(ctx, shop.ID, u.ID, RoleMember); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectMember(ctx, shop.ID, u.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := db.SetProjectMember(ctx, shop.ID, u.ID, RoleOwner); err == nil {
		t.Fatal("owner role in a project accepted")
	}
	if err := db.SetProjectMember(ctx, 999, u.ID, RoleViewer); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member of a missing project: %v", err)
	}
	if roles, _ := db.UserProjectRoles(ctx, u.ID); len(roles) != 1 || roles[shop.ID] != RoleAdmin {
		t.Fatalf("roles: %v", roles)
	}
	if m, _ := db.ProjectMembers(ctx, shop.ID); len(m) != 1 || m[0].Email != "u@example.com" || m[0].Role != RoleAdmin {
		t.Fatalf("members: %+v", m)
	}

	// Deleting: only empty projects, never the default one.
	if err := db.DeleteProject(ctx, shop.ID); !errors.Is(err, ErrProjectNotEmpty) {
		t.Fatalf("delete a project with resources: %v", err)
	}
	if err := db.DeleteProject(ctx, DefaultProject); !errors.Is(err, ErrDefaultProject) {
		t.Fatalf("delete the default project: %v", err)
	}
	for _, m := range []struct {
		kind ProjectResource
		id   int64
	}{{ResourceSite, s2.ID}, {ResourceDNSZone, z.ID}, {ResourceHealthCheck, hc.ID}, {ResourceApp, web.ID}} {
		if err := db.MoveToProject(ctx, m.kind, m.id, DefaultProject); err != nil {
			t.Fatalf("move %s: %v", m.kind, err)
		}
	}
	if err := db.MoveToProject(ctx, ResourceSite, s2.ID, 999); !errors.Is(err, ErrNoProject) {
		t.Fatalf("move to a missing project: %v", err)
	}
	if err := db.MoveToProject(ctx, ResourceSite, 999, DefaultProject); !errors.Is(err, ErrNotFound) {
		t.Fatalf("move a missing site: %v", err)
	}
	if err := db.DeleteProject(ctx, shop.ID); err != nil {
		t.Fatal(err)
	}
	if roles, _ := db.UserProjectRoles(ctx, u.ID); len(roles) != 0 {
		t.Fatalf("memberships of a deleted project kept: %v", roles)
	}
	// Deleting a user ends their memberships.
	other, _ := db.CreateProject(ctx, "Other", "")
	db.SetProjectMember(ctx, other.ID, u.ID, RoleViewer)
	if err := db.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.ProjectMembers(ctx, other.ID); len(m) != 0 {
		t.Fatalf("members after deleting the user: %+v", m)
	}
}

// TestMoveLinkedApps: an app and its databases stay in one project; the
// checks of an app's placements move with it, not alone.
func TestMoveLinkedApps(t *testing.T) {
	ctx := context.Background()
	db, _ := openTemp(t)
	shop, _ := db.CreateProject(ctx, "Shop", "")
	srv, _ := db.CreateServer(ctx, "s1", []byte("t1"), time.Now().Add(time.Hour))
	srv2, _ := db.CreateServer(ctx, "s2", []byte("t2"), time.Now().Add(time.Hour))
	web, _ := db.CreateApp(ctx, srv.ID, "web", AppSettings{Image: "nginx:1.27", Port: 80}, []string{"web.example.com"}, proxy.TLSAuto)
	pg, _ := db.CreateApp(ctx, srv.ID, "pg", AppSettings{Source: SourceDatabase, Engine: "postgres", EngineVersion: "18", Image: "postgres:18-alpine"}, []string{}, proxy.TLSAuto)
	mail, _ := db.CreateApp(ctx, srv.ID, "mail", AppSettings{Source: SourceMail, Image: "stalwart", Port: 8080, Hostname: "mail.example.com"}, []string{}, proxy.TLSAuto)
	if err := db.LinkApp(ctx, pg.ID, web.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.LinkApp(ctx, mail.ID, web.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.MoveToProject(ctx, ResourceApp, web.ID, shop.ID); !errors.Is(err, ErrProjectMismatch) {
		t.Fatalf("move an app away from its database: %v", err)
	}
	if err := db.MoveToProject(ctx, ResourceApp, pg.ID, shop.ID); !errors.Is(err, ErrProjectMismatch) {
		t.Fatalf("move a database away from its app: %v", err)
	}
	// Unlinked from the database, the app moves (the mail server serves every project).
	db.UnlinkApp(ctx, pg.ID, web.ID)
	hc := func(name string) *HealthCheck {
		return &HealthCheck{Name: name, Kind: "https", Host: "192.0.2.1", Port: 443, Path: "/", Interval: 30, Timeout: 5}
	}
	c1, c0 := hc("replica"), hc("primary")
	if err := db.AddAppReplica(ctx, web.ID, srv2.ID, c1, c0); err != nil {
		t.Fatal(err)
	}
	if err := db.MoveToProject(ctx, ResourceHealthCheck, c1.ID, shop.ID); !errors.Is(err, ErrReplicaCheckMove) {
		t.Fatalf("move a placement's check alone: %v", err)
	}
	if err := db.MoveToProject(ctx, ResourceApp, web.ID, shop.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{c0.ID, c1.ID} {
		if got, _ := db.HealthCheck(ctx, id); got.ProjectID != shop.ID {
			t.Fatalf("check %d stayed in project %d", id, got.ProjectID)
		}
	}
	// A new replica's check belongs to the app's project.
	srv3, _ := db.CreateServer(ctx, "s3", []byte("t3"), time.Now().Add(time.Hour))
	c2 := hc("replica-2")
	if err := db.AddAppReplica(ctx, web.ID, srv3.ID, c2, nil); err != nil || c2.ProjectID != shop.ID {
		t.Fatalf("new replica check: project %d, %v", c2.ProjectID, err)
	}
}

// TestProjectsMigration: everything that existed before projects lands in
// the default project, and foreign keys hold.
func TestProjectsMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "basalt.db")
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	f.Close()
	w, _ := sql.Open("sqlite", dsn(path, false))
	w.SetMaxOpenConns(1)
	r, _ := sql.Open("sqlite", dsn(path, true))
	db := &DB{W: w, R: r}
	defer db.Close()
	if err := db.migrateTo(ctx, 36); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO servers (id, name) VALUES (1, 's')`,
		`INSERT INTO sites (id, server_id, name, kind) VALUES (5, 1, 'blog', 'static')`,
		`INSERT INTO apps (id, server_id, name, image, port) VALUES (7, 1, 'web', 'nginx:1.27', 80)`,
		`INSERT INTO dns_zones (id, name, serial) VALUES (3, 'example.com', 1)`,
		`INSERT INTO health_checks (id, name, kind, host, port, interval_s, timeout_s) VALUES (2, 'c', 'tcp', 'x', 80, 30, 5)`,
		`INSERT INTO users (id, email, name, password_hash, role, webauthn_handle) VALUES (4, 'o@example.com', 'O', 'h', 'owner', x'01')`,
		`INSERT INTO sessions (token_hash, user_id, ip, user_agent, expires_at) VALUES (x'aa', 4, '', '', 9999999999)`,
	} {
		if _, err := db.W.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := db.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sites", "apps", "dns_zones", "health_checks"} {
		var p int64
		if err := db.R.QueryRowContext(ctx, `SELECT project_id FROM `+table).Scan(&p); err != nil || p != DefaultProject {
			t.Fatalf("%s: project %d, %v", table, p, err)
		}
	}
	var fk int
	db.W.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Fatal("foreign keys left off")
	}
	// The users table was rebuilt for the role "user": rows, references and
	// the passkey handle's uniqueness stay.
	if u, err := db.UserByID(ctx, 4); err != nil || u.Role != RoleOwner {
		t.Fatalf("user after the rebuild: %+v %v", u, err)
	}
	if _, err := db.CreateUser(ctx, "u@example.com", "U", "h", RoleUser); err != nil {
		t.Fatalf("role user: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE users SET webauthn_handle = x'01' WHERE email = 'u@example.com'`); err == nil {
		t.Fatal("two users with one passkey handle")
	}
	if _, err := db.W.ExecContext(ctx, `DELETE FROM users WHERE id = 4`); err != nil {
		t.Fatal(err)
	}
	var n int
	db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 0 {
		t.Fatal("sessions outlived their user after the rebuild")
	}
	// The default project cannot go while it holds anything, even in SQL.
	if _, err := db.W.ExecContext(ctx, `DELETE FROM projects WHERE id = 1`); err == nil {
		t.Fatal("deleted the default project under its resources")
	}
}
