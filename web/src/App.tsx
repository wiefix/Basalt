import { useEffect } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Navigate, NavLink, Route, Routes, useNavigate } from "react-router";
import { api, ApiError, can, onUnauthorized, type User } from "./api";
import { Icon, Loading, Logo, useTheme } from "./components/ui";
import { LoginPage, SetupPage } from "./pages/Auth";
import { ServersPage } from "./pages/Servers";
import { ServerPage } from "./pages/Server";
import { SitesPage } from "./pages/Sites";
import { SitePage } from "./pages/Site";
import { AppsPage } from "./pages/Apps";
import { AppPage } from "./pages/App";
import { DatabasesPage } from "./pages/Databases";
import { UsersPage } from "./pages/Users";
import { RegistriesPage } from "./pages/Registries";
import { BackupsPage } from "./pages/Backups";
import { DNSPage, ZonePage } from "./pages/DNS";
import { MailPage } from "./pages/Mail";
import { CatalogPage } from "./pages/Catalog";
import { MonitoringPage } from "./pages/Monitoring";
import { NotificationsPage } from "./pages/Notifications";
import { AccountPage } from "./pages/Account";
import { AuditPage } from "./pages/Audit";
import { APIDocsPage } from "./pages/APIDocs";
import { ProjectPage, ProjectsPage } from "./pages/Projects";
import { useCanCreate } from "./components/projects";

// The signed-in user, or null when signed out. A 401 is a normal answer
// here, not an error: as an error, every refetch (e.g. on window focus) would
// put the query back into loading and remount the login form.
const fetchMe = () =>
  api.me().catch((e) => {
    if (e instanceof ApiError && e.status === 401) return null;
    throw e;
  });

export function useMe() {
  return useQuery({ queryKey: ["me"], queryFn: fetchMe, staleTime: 60_000 });
}

export function App() {
  const qc = useQueryClient();
  const setup = useQuery({ queryKey: ["setup"], queryFn: api.setupStatus, staleTime: Infinity });
  const me = useQuery({
    queryKey: ["me"],
    queryFn: fetchMe,
    enabled: setup.data?.required === false,
    staleTime: 60_000,
  });

  // A 401 from any request means the session ended: drop cached data.
  useEffect(
    () =>
      onUnauthorized(() => {
        qc.setQueryData(["me"], null);
        qc.removeQueries({ predicate: (q) => q.queryKey[0] !== "me" && q.queryKey[0] !== "setup" });
      }),
    [qc],
  );

  if (setup.isPending) return <Loading />;
  if (setup.data?.required) return <SetupPage />;
  if (me.isPending) return <Loading />;
  if (me.data === null) return <LoginPage />;
  if (me.error) return <div className="auth-page"><div className="alert err">{String(me.error)}</div></div>;

  return <Shell user={me.data!.user} />;
}

// SupersededBanner warns that a standby panel took over from this one: its
// servers no longer connect here, so changes made here reach none of them.
function SupersededBanner() {
  const panel = useQuery({ queryKey: ["panel"], queryFn: api.panel, refetchInterval: 60_000 });
  const s = panel.data?.panel.superseded;
  if (!s) return null;
  return (
    <div className="alert err" style={{ marginBottom: 16 }}>
      <strong>A standby panel took over from this one.</strong> Its servers connect to the new panel and refuse this one
      (seen {new Date(s.at).toLocaleString()}): changes made here reach no server. Use the new panel. To keep this
      server as its standby, stop this panel (<code>systemctl stop basalt-server</code>), move{" "}
      <code>/var/lib/basalt/server</code> aside, add a standby panel on the new panel and run its command here.
    </div>
  );
}

function Shell({ user }: { user: User }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [, toggleTheme] = useTheme();
  // Admins of a project manage its notification channels.
  const projectAdmin = useCanCreate(user);

  const logout = async () => {
    await api.logout().catch(() => {});
    // Not qc.clear(): it would detach the app from the "me" query, and the
    // login form would not appear.
    qc.setQueryData(["me"], null);
    qc.removeQueries({ predicate: (q) => q.queryKey[0] !== "me" && q.queryKey[0] !== "setup" });
    navigate("/");
  };

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">
          <Logo /> Basalt
        </div>
        {can(user, "viewer") && (
          <NavLink className="nav-link" to="/servers">
            <Icon name="server" /> Servers
          </NavLink>
        )}
        <NavLink className="nav-link" to="/projects">
          <Icon name="layers" /> Projects
        </NavLink>
        <NavLink className="nav-link" to="/sites">
          <Icon name="globe" /> Sites
        </NavLink>
        <NavLink className="nav-link" to="/apps">
          <Icon name="box" /> Apps
        </NavLink>
        <NavLink className="nav-link" to="/catalog">
          <Icon name="grid" /> Catalog
        </NavLink>
        <NavLink className="nav-link" to="/databases">
          <Icon name="database" /> Databases
        </NavLink>
        {can(user, "viewer") && (
          <NavLink className="nav-link" to="/mail">
            <Icon name="mail" /> Mail
          </NavLink>
        )}
        <NavLink className="nav-link" to="/monitoring">
          <Icon name="pulse" /> Monitoring
        </NavLink>
        <NavLink className="nav-link" to="/dns">
          <Icon name="route" /> DNS
        </NavLink>
        {projectAdmin && (
          <NavLink className="nav-link" to="/notifications">
            <Icon name="bell" /> Notifications
          </NavLink>
        )}
        {can(user, "admin") && (
          <NavLink className="nav-link" to="/backups">
            <Icon name="archive" /> Backups
          </NavLink>
        )}
        {can(user, "admin") && (
          <NavLink className="nav-link" to="/registries">
            <Icon name="key" /> Registries
          </NavLink>
        )}
        {can(user, "admin") && (
          <NavLink className="nav-link" to="/users">
            <Icon name="users" /> Users
          </NavLink>
        )}
        {can(user, "admin") && (
          <NavLink className="nav-link" to="/audit">
            <Icon name="shield" /> Audit log
          </NavLink>
        )}
        <div className="sidebar-footer">
          <NavLink className="nav-link" to="/account">
            <Icon name="user" />
            <span style={{ overflow: "hidden", textOverflow: "ellipsis" }}>{user.name}</span>
          </NavLink>
          <div className="row">
            <button className="btn ghost sm" onClick={toggleTheme} title="Toggle theme">
              <Icon name="moon" size={14} /> Theme
            </button>
            <button className="btn ghost sm" onClick={logout}>
              <Icon name="logout" size={14} /> Sign out
            </button>
          </div>
        </div>
      </aside>
      <main className="main">
        <SupersededBanner />
        <Routes>
          {/* Users with no role on the panel work in their projects' sites and apps. */}
          <Route path="/" element={<Navigate to={can(user, "viewer") ? "/servers" : "/sites"} replace />} />
          <Route path="/servers" element={<ServersPage user={user} />} />
          <Route path="/servers/:id" element={<ServerPage user={user} />} />
          <Route path="/projects" element={<ProjectsPage user={user} />} />
          <Route path="/projects/:id" element={<ProjectPage user={user} />} />
          <Route path="/sites" element={<SitesPage user={user} />} />
          <Route path="/sites/:id/*" element={<SitePage user={user} />} />
          <Route path="/apps" element={<AppsPage user={user} />} />
          <Route path="/apps/:id/*" element={<AppPage user={user} />} />
          <Route path="/databases" element={<DatabasesPage user={user} />} />
          <Route path="/databases/:id/*" element={<AppPage user={user} />} />
          <Route path="/mail" element={<MailPage user={user} />} />
          <Route path="/catalog" element={<CatalogPage user={user} />} />
          <Route path="/monitoring" element={<MonitoringPage user={user} />} />
          <Route path="/notifications" element={<NotificationsPage user={user} />} />
          <Route path="/dns" element={<DNSPage user={user} />} />
          <Route path="/dns/:id" element={<ZonePage user={user} />} />
          <Route path="/registries" element={<RegistriesPage user={user} />} />
          <Route path="/backups" element={<BackupsPage user={user} />} />
          <Route path="/users" element={<UsersPage user={user} />} />
          <Route path="/audit" element={<AuditPage user={user} />} />
          <Route path="/api-reference" element={<APIDocsPage />} />
          <Route path="/account" element={<AccountPage user={user} />} />
          <Route path="*" element={<div className="empty"><h3>Page not found</h3></div>} />
        </Routes>
      </main>
    </div>
  );
}
