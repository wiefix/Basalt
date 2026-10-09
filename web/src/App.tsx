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
import { UsersPage } from "./pages/Users";
import { AccountPage } from "./pages/Account";

export function useMe() {
  return useQuery({ queryKey: ["me"], queryFn: api.me, staleTime: 60_000 });
}

export function App() {
  const qc = useQueryClient();
  const setup = useQuery({ queryKey: ["setup"], queryFn: api.setupStatus, staleTime: Infinity });
  const me = useQuery({
    queryKey: ["me"],
    queryFn: api.me,
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
  const unauthenticated = me.data === null || (me.error instanceof ApiError && me.error.status === 401);
  if (unauthenticated) return <LoginPage />;
  if (me.error) return <div className="auth-page"><div className="alert err">{String(me.error)}</div></div>;

  return <Shell user={me.data!.user} />;
}

function Shell({ user }: { user: User }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [, toggleTheme] = useTheme();

  const logout = async () => {
    await api.logout().catch(() => {});
    qc.clear();
    qc.setQueryData(["me"], null);
    navigate("/");
  };

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">
          <Logo /> Basalt
        </div>
        <NavLink className="nav-link" to="/servers">
          <Icon name="server" /> Servers
        </NavLink>
        <NavLink className="nav-link" to="/sites">
          <Icon name="globe" /> Sites
        </NavLink>
        {can(user, "admin") && (
          <NavLink className="nav-link" to="/users">
            <Icon name="users" /> Users
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
        <Routes>
          <Route path="/" element={<Navigate to="/servers" replace />} />
          <Route path="/servers" element={<ServersPage user={user} />} />
          <Route path="/servers/:id" element={<ServerPage user={user} />} />
          <Route path="/sites" element={<SitesPage user={user} />} />
          <Route path="/sites/:id/*" element={<SitePage user={user} />} />
          <Route path="/users" element={<UsersPage user={user} />} />
          <Route path="/account" element={<AccountPage user={user} />} />
          <Route path="*" element={<div className="empty"><h3>Page not found</h3></div>} />
        </Routes>
      </main>
    </div>
  );
}
