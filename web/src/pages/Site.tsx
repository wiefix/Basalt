import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, NavLink, Route, Routes, useNavigate, useParams } from "react-router";
import { api, ApiError, can, type DatabaseCredentials, type SFTPUser, type Site, type SitePlacement, type User } from "../api";
import { FileManager } from "../components/FileManager";
import { CodeBox, CopyButton, ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatBytes, formatDate, timeAgo, useConfirm, useToast } from "../components/ui";
import { LogView } from "../components/LogView";
import { AccessLogCard } from "../components/AccessLog";
import { SyncBadge } from "./Server";
import { MoveProjectCard, ProjectBadge, ServerLink, useAs } from "../components/projects";
import { BackupsTab } from "./BackupsTab";

export function SitePage({ user: me }: { user: User }) {
  const id = Number(useParams().id);
  const site = useQuery({ queryKey: ["site", id], queryFn: () => api.site(id) });
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  // What the user may do here: their role in the site's project.
  const user = useAs(me, site.data?.site.project_id);

  if (site.isPending) return <Loading />;
  if (site.error) return <ErrorBox error={site.error} />;
  const s = site.data.site;
  const server = servers.data?.servers.find((x) => x.id === s.server_id);
  const tab = (to: string, label: string) => (
    <NavLink to={to} end className={({ isActive }) => (isActive ? "tab active" : "tab")}>
      {label}
    </NavLink>
  );

  return (
    <>
      <div className="page-head">
        <div>
          <div className="row">
            <h1>{s.name}</h1>
            <span className="badge plain">{s.php_version ? `PHP ${s.php_version}` : "static"}</span>
            <ProjectBadge id={s.project_id} />
            <SyncBadge sync={site.data.sync} />
          </div>
          <div className="sub row wrap">
            {s.domains.map((d) => (
              <a key={d} href={`https://${d}`} target="_blank" rel="noreferrer noopener">
                {d} ↗
              </a>
            ))}
            <span className="faint">
              on <ServerLink id={s.server_id} name={server?.name ?? `server #${s.server_id}`} user={me} />
            </span>
          </div>
        </div>
      </div>
      {server && server.status !== "online" && (
        <div className="alert warn" style={{ marginBottom: 16 }}>
          The server is {server.status}. Files cannot be managed until its agent reconnects; the site itself keeps running.
        </div>
      )}
      {s.replicas.length > 0 && (
        <div className="alert" style={{ marginBottom: 16 }}>
          Served by {s.replicas.length + 1} servers: the files here are copied to the others seconds after they change. Change them here (or over SFTP to
          this server) only — changes made on another server are undone.
          {site.data.placements?.some((p) => p.databases) &&
            " Its databases live here too: the copies on the other servers use them through the servers' tunnel (with “localhost” as the database host), and keep read-only copies for a switch-over."}
        </div>
      )}
      <div className="tabs">
        {tab(`/sites/${id}`, "Files")}
        {tab(`/sites/${id}/domains`, "Domains")}
        {tab(`/sites/${id}/sftp`, "SFTP")}
        {tab(`/sites/${id}/databases`, "Databases")}
        {tab(`/sites/${id}/backups`, "Backups")}
        {tab(`/sites/${id}/logs`, "Logs")}
        {can(user, "admin") && tab(`/sites/${id}/settings`, "Settings")}
      </div>
      <Routes>
        <Route index element={<FileManager siteId={id} canWrite={can(user, "member")} />} />
        <Route
          path="domains"
          element={
            <>
              <DomainsTab site={s} user={user} />
              <SiteServersCard site={s} placements={site.data.placements ?? []} user={user} me={me} />
            </>
          }
        />
        <Route path="sftp" element={<SFTPTab site={s} user={user} host={server?.last_ip} />} />
        <Route path="databases" element={<DatabasesTab site={s} user={user} />} />
        <Route path="backups" element={<BackupsTab target={{ base: `/api/v1/sites/${s.id}`, name: s.name, siteId: s.id }} user={user} />} />
        <Route path="logs" element={<LogsTab site={s} user={user} />} />
        <Route
          path="settings"
          element={<SettingsTab site={s} phpVersions={server?.isolated ? server.php_versions : []} phpLimits={!!server?.php_limits} panelAdmin={can(me, "admin")} />}
        />
      </Routes>
    </>
  );
}

const checkBadge: Record<string, string> = { up: "badge ok", down: "badge err", unknown: "badge plain" };

// SiteServersCard lists the servers serving a site and lets admins add or
// remove copies.
function SiteServersCard({ site, placements, user, me }: { site: Site; placements: SitePlacement[]; user: User; me: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const [target, setTarget] = useState<number | "">("");
  const admin = can(user, "admin");
  const list = placements.length > 0 ? placements : [{ server_id: site.server_id, primary: true } as SitePlacement];
  const on = new Set(list.map((p) => p.server_id));
  const candidates = (servers.data?.servers ?? []).filter(
    (s) => !on.has(s.id) && (!site.php_version || s.php_versions.includes(site.php_version)),
  );
  const name = (id: number) => servers.data?.servers.find((s) => s.id === id)?.name ?? `server #${id}`;
  const syncing = list.some((p) => p.syncing);
  useQuery({
    // While a copy runs, follow it.
    queryKey: ["site", site.id, "syncing"],
    queryFn: async () => {
      const r = await api.site(site.id);
      qc.setQueryData(["site", site.id], r);
      return r.placements.some((p) => p.syncing);
    },
    enabled: syncing,
    refetchInterval: 2000,
  });
  const refresh = (r?: { site: Site; placements: SitePlacement[] }) => {
    qc.invalidateQueries({ queryKey: ["site", site.id] });
    qc.invalidateQueries({ queryKey: ["sites"] });
    qc.invalidateQueries({ queryKey: ["health-checks"] });
    if (r) qc.setQueryData(["site", site.id], (old: object | undefined) => old && { ...old, ...r });
  };
  const add = useMutation({
    mutationFn: () => api.addSiteReplica(site.id, Number(target || candidates[0]?.id)),
    onSuccess: (r) => {
      setTarget("");
      refresh(r);
      toast("Added; the files are being copied there");
    },
  });
  const sync = useMutation({
    mutationFn: () => api.syncSite(site.id),
    onSuccess: (r) => refresh(r),
    onError: (e) => toast(errorMessage(e), true),
  });
  const makePrimary = async (id: number) => {
    const ok = await confirm({
      title: `Make ${name(id)} the primary of ${site.name}?`,
      body: (
        <p>
          Its copy becomes the site: files are managed there and its databases take the writes. {name(site.server_id)} gets its files
          and databases copied from it from then on (what it has is replaced). With {name(site.server_id)} online nothing is lost: its
          files are copied a last time and its databases wait for the copy to catch up.
        </p>
      ),
      confirm: "Make primary",
    });
    if (!ok) return;
    const run = async (force: boolean) => {
      const r = await api.setSitePrimary(site.id, id, force);
      refresh(r);
      toast(r.warning ? `Switched; ${r.warning}` : `${name(id)} is now the primary`, !!r.warning);
    };
    try {
      await run(false);
    } catch (e) {
      if (e instanceof ApiError && e.status === 409 && /offline/.test(e.message)) {
        const force = await confirm({
          title: `${name(site.server_id)} is offline`,
          body: <p>{e.message}.</p>,
          confirm: "Switch anyway",
          danger: true,
        });
        if (!force) return;
        try {
          await run(true);
        } catch (e2) {
          toast(errorMessage(e2), true);
        }
        return;
      }
      toast(errorMessage(e), true);
    }
  };
  const remove = async (id: number) => {
    const ok = await confirm({
      title: `Stop serving ${site.name} from ${name(id)}?`,
      body: <p>Its domains stop pointing at {name(id)}, and the copy of the files there goes to that server's trash.</p>,
      confirm: "Remove",
      danger: true,
    });
    if (!ok) return;
    try {
      const r = await api.removeSiteReplica(site.id, id);
      refresh(r);
      if (r.cleanup_error) toast(`Removed; cleanup pending: ${r.cleanup_error}`, true);
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  return (
    <div className="card" style={{ marginTop: 16 }}>
      <div className="card-head">
        <h2>Servers</h2>
        {list.length > 1 && can(user, "member") && (
          <button className="btn sm" onClick={() => sync.mutate()} disabled={sync.isPending || syncing}>
            {syncing ? "Copying…" : "Copy now"}
          </button>
        )}
      </div>
      <div className="table-wrap">
        <table className="table">
          <tbody>
            {list.map((p) => (
              <tr key={p.server_id}>
                <td>
                  <strong>
                    <ServerLink id={p.server_id} name={name(p.server_id)} user={me} />
                  </strong>
                  {list.length > 1 && <span className="faint small"> · {p.primary ? "primary: the files live here" : "copy"}</span>}
                </td>
                <td className="small">
                  {!p.primary &&
                    (p.sync_error ? (
                      <span className="err" title={p.sync_error}>
                        copy failed: {p.sync_error}
                      </span>
                    ) : p.synced_at ? (
                      <span className="muted" title={formatDate(p.synced_at)}>
                        files as of {timeAgo(p.synced_at)}
                      </span>
                    ) : (
                      <span className="muted">first copy…</span>
                    ))}
                  {!p.primary && p.databases && (
                    <div>
                      {p.databases.error ? (
                        <span className="err" title={p.databases.error}>
                          databases: {p.databases.error}
                        </span>
                      ) : p.databases.waiting.length > 0 ? (
                        <span className="muted">databases: copying {p.databases.waiting.join(", ")}…</span>
                      ) : p.databases.running ? (
                        <span className="muted" title="Copied from the primary's MariaDB as it changes; read-only here">
                          databases: {p.databases.lag > 0 ? `${p.databases.lag} s behind` : "up to date"}
                        </span>
                      ) : (
                        <span className="muted">databases: starting…</span>
                      )}
                    </div>
                  )}
                </td>
                <td>
                  {p.health_check ? (
                    <Link
                      to="/monitoring"
                      className={checkBadge[p.status ?? "unknown"] ?? "badge"}
                      title="Its health check: DNS leaves out a server whose copy is down (unless every copy is)"
                    >
                      {p.status ?? "unknown"}
                    </Link>
                  ) : null}
                </td>
                <td className="num" style={{ whiteSpace: "nowrap" }}>
                  {admin && !p.primary && (
                    <>
                      <button className="btn sm ghost" title="Make this copy the site's primary" onClick={() => makePrimary(p.server_id)}>
                        Make primary
                      </button>
                      <button className="btn sm icon ghost" title="Remove" onClick={() => remove(p.server_id)}>
                        <Icon name="trash" size={14} />
                      </button>
                    </>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="card-body stack">
        <p className="faint small" style={{ margin: 0 }}>
          {list.length > 1
            ? "The files are copied from the primary to the others seconds after they change, and checked every minute (not the logs), and its MariaDB databases as they change (read-only there). DNS answers with the servers whose copy passes its health check (HTTPS on /, editable under Monitoring); certificates come through DNS."
            : "Serve the site from more servers for failover: its files are copied there seconds after they change, its databases as they change, and DNS answers only with the servers whose copy is up. It needs domains in a zone under DNS; databases also need MariaDB on both servers and the servers' tunnel (port 8444 open between them)."}
        </p>
        {admin &&
          (candidates.length === 0 ? (
            <p className="faint small" style={{ margin: 0 }}>
              No other server{site.php_version ? ` with PHP ${site.php_version}` : ""}.
            </p>
          ) : (
            <form
              className="row wrap"
              onSubmit={(e: FormEvent) => {
                e.preventDefault();
                add.mutate();
              }}
            >
              <select className="input" style={{ width: "auto", minWidth: 180 }} value={target || candidates[0].id} onChange={(e) => setTarget(Number(e.target.value))}>
                {candidates.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name} {s.status !== "online" ? `(${s.status})` : ""}
                  </option>
                ))}
              </select>
              <button className="btn sm" disabled={add.isPending}>
                <Icon name="plus" size={14} /> Serve from here too
              </button>
            </form>
          ))}
        <ErrorBox error={add.error} />
      </div>
    </div>
  );
}

function DomainsTab({ site, user }: { site: Site; user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [text, setText] = useState(site.domains.join("\n"));
  const save = useMutation({
    mutationFn: () => api.setSiteDomains(site.id, text.split(/[\s,]+/).filter(Boolean)),
    onSuccess: (r) => {
      qc.setQueryData(["site", site.id], (old: object | undefined) => ({ ...old, site: r.site, sync: r.sync }));
      qc.invalidateQueries({ queryKey: ["sites"] });
      toast(r.sync.in_sync ? "Domains updated" : "Saved; the server will apply it when it reconnects");
    },
  });
  return (
    <div className="grid-2">
      <form
        className="card card-body form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <h2>Domains</h2>
        <ErrorBox error={save.error} />
        <Field label="One per line" help="Certificates are requested automatically once DNS points at the server. *.example.com covers every subdomain (its zone must be under DNS).">
          <textarea className="input mono" rows={6} value={text} onChange={(e) => setText(e.target.value)} disabled={!can(user, "admin")} />
        </Field>
        {can(user, "admin") && (
          <div>
            <button className="btn primary" disabled={save.isPending}>
              Save domains
            </button>
          </div>
        )}
      </form>
      <div className="card card-body stack">
        <h2>DNS setup</h2>
        <p className="muted" style={{ margin: 0 }}>
          At your DNS provider, create an <strong>A</strong> record (and <strong>AAAA</strong> for IPv6) for each domain
          pointing to the server's public address. HTTPS is enabled automatically.
        </p>
        <p className="faint small" style={{ margin: 0 }}>
          Certificate: {site.tls === "auto" ? "Let's Encrypt (falls back to ZeroSSL)" : "internal CA (browsers will warn)"}
        </p>
      </div>
    </div>
  );
}

function SFTPTab({ site, user, host }: { site: Site; user: User; host?: string }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const users = useQuery({ queryKey: ["sftp", site.id], queryFn: () => api.sftpUsers(site.id) });
  const [adding, setAdding] = useState(false);
  const [created, setCreated] = useState<{ username: string; password?: string; changed?: boolean } | null>(null);
  const [editing, setEditing] = useState<SFTPUser | null>(null);
  const conn = users.data?.connection;
  const hostName = site.domains[0] ?? host ?? "your-server";

  const remove = async (id: number, name: string) => {
    if (!(await confirm({ title: `Delete ${name}?`, body: <p>This account will be signed out and cannot log in again.</p>, confirm: "Delete", danger: true })))
      return;
    try {
      await api.deleteSFTPUser(site.id, id);
      qc.invalidateQueries({ queryKey: ["sftp", site.id] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };

  return (
    <div className="stack">
      {created && (
        <div className="card card-body stack">
          <div className="row">
            <Icon name="check" />
            <h2>
              Account {created.username} {created.changed ? "changed" : "created"}
            </h2>
          </div>
          {created.password && (
            <>
              <div className="alert warn small">Copy the password now. It is not stored and will not be shown again.</div>
              <Field label="Password">
                <CodeBox text={created.password} />
              </Field>
            </>
          )}
          <div>
            <button className="btn sm" onClick={() => setCreated(null)}>
              Done
            </button>
          </div>
        </div>
      )}
      <div className="card">
        <div className="card-head">
          <h2>SFTP accounts</h2>
          {can(user, "admin") && (
            <button className="btn sm" onClick={() => setAdding(true)}>
              <Icon name="plus" size={14} /> Add account
            </button>
          )}
        </div>
        {users.isPending ? (
          <Loading />
        ) : users.error ? (
          <div className="card-body">
            <ErrorBox error={users.error} />
          </div>
        ) : users.data.users.length === 0 ? (
          <div className="empty small">No SFTP accounts. Add one to upload files with FileZilla, Cyberduck or the sftp command.</div>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Username</th>
                <th>Sign-in</th>
                <th>Created</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {users.data.users.map((u) => (
                <tr key={u.id}>
                  <td className="mono">{u.username}</td>
                  <td className="muted small">
                    {u.has_password && <div>password</div>}
                    {u.public_keys.map((k) => (
                      <div key={k} className="mono">
                        {k}
                      </div>
                    ))}
                  </td>
                  <td className="muted">{formatDate(u.created_at)}</td>
                  <td className="num">
                    {can(user, "admin") && (
                      <div className="row" style={{ justifyContent: "flex-end", gap: 2 }}>
                        <button className="btn sm icon ghost" title="Change password or keys" onClick={() => setEditing(u)}>
                          <Icon name="edit" size={14} />
                        </button>
                        <button className="btn sm icon ghost" title="Delete" onClick={() => remove(u.id, u.username)}>
                          <Icon name="trash" size={14} />
                        </button>
                      </div>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {conn && (
        <div className="card card-body stack">
          <h2>Connection</h2>
          <dl className="kv">
            <dt>Host</dt>
            <dd className="mono">{hostName}</dd>
            <dt>Port</dt>
            <dd className="mono">{conn.port}</dd>
            <dt>Host key</dt>
            <dd className="row">
              <span className="mono">{conn.host_key_fingerprint}</span>
              <CopyButton text={conn.host_key_fingerprint} />
            </dd>
          </dl>
          <CodeBox text={`sftp -P ${conn.port} ${users.data?.users[0]?.username ?? "USERNAME"}@${hostName}`} />
          <p className="faint small" style={{ margin: 0 }}>
            On first connect your client shows the server's key fingerprint. It must match the one above. Your files are
            in <code>/public</code>.
          </p>
        </div>
      )}

      {editing && (
        <EditSFTPModal
          site={site}
          account={editing}
          onClose={() => setEditing(null)}
          onSaved={(c) => {
            setEditing(null);
            if (c.password) setCreated({ ...c, changed: true });
            else toast(`${c.username} changed`);
            qc.invalidateQueries({ queryKey: ["sftp", site.id] });
          }}
        />
      )}
      {adding && (
        <AddSFTPModal
          site={site}
          onClose={() => setAdding(false)}
          onCreated={(c) => {
            setAdding(false);
            setCreated(c);
            qc.invalidateQueries({ queryKey: ["sftp", site.id] });
          }}
        />
      )}
    </div>
  );
}

function AddSFTPModal({
  site,
  onClose,
  onCreated,
}: {
  site: Site;
  onClose: () => void;
  onCreated: (c: { username: string; password?: string }) => void;
}) {
  const suggested = site.name.toLowerCase().replace(/[^a-z0-9_-]/g, "-").replace(/^[^a-z]+/, "").slice(0, 32) || "site";
  const [username, setUsername] = useState(suggested.length >= 3 ? suggested : `${suggested}-ftp`);
  const [auth, setAuth] = useState<"generate" | "password" | "key">("generate");
  const [password, setPassword] = useState("");
  const [keys, setKeys] = useState("");
  const create = useMutation({
    mutationFn: () => {
      const public_keys = keys.split("\n").map((k) => k.trim()).filter(Boolean);
      return api.createSFTPUser(site.id, {
        username,
        password: auth === "generate" ? "" : auth === "password" ? password : "-",
        public_keys,
      });
    },
    onSuccess: (r) => onCreated({ username: r.user.username, password: r.password }),
  });
  return (
    <Modal
      title="Add SFTP account"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="sftp-form" disabled={create.isPending}>
            Create account
          </button>
        </>
      }
    >
      <form
        id="sftp-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <ErrorBox error={create.error} />
        <Field label="Username" help="3–32 characters: a–z, 0–9, _ and -.">
          <input className="input mono" value={username} onChange={(e) => setUsername(e.target.value)} required autoFocus />
        </Field>
        <Field label="Sign-in method">
          <select className="input" value={auth} onChange={(e) => setAuth(e.target.value as typeof auth)}>
            <option value="generate">Generate a strong password</option>
            <option value="password">Choose a password</option>
            <option value="key">SSH key only (most secure)</option>
          </select>
        </Field>
        {auth === "password" && (
          <Field label="Password" help="At least 12 characters.">
            <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} minLength={12} required autoComplete="new-password" />
          </Field>
        )}
        <Field label={auth === "key" ? "Public keys" : "Public keys (optional)"} help="One per line, e.g. the contents of ~/.ssh/id_ed25519.pub">
          <textarea className="input mono" rows={3} value={keys} onChange={(e) => setKeys(e.target.value)} required={auth === "key"} placeholder="ssh-ed25519 AAAA… you@laptop" />
        </Field>
      </form>
    </Modal>
  );
}

function EditSFTPModal({
  site,
  account,
  onClose,
  onSaved,
}: {
  site: Site;
  account: SFTPUser;
  onClose: () => void;
  onSaved: (c: { username: string; password?: string }) => void;
}) {
  const [pw, setPw] = useState<"keep" | "generate" | "password" | "none">("keep");
  const [password, setPassword] = useState("");
  const [replaceKeys, setReplaceKeys] = useState(false);
  const [keys, setKeys] = useState("");
  const save = useMutation({
    mutationFn: () => {
      const b: { password?: string; public_keys?: string[] } = {};
      if (pw !== "keep") b.password = pw === "generate" ? "" : pw === "password" ? password : "-";
      if (replaceKeys) b.public_keys = keys.split("\n").map((k) => k.trim()).filter(Boolean);
      return api.updateSFTPUser(site.id, account.id, b);
    },
    onSuccess: (r) => onSaved({ username: r.user.username, password: r.password }),
  });
  return (
    <Modal
      title={`Change ${account.username}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="sftp-edit" disabled={save.isPending || (pw === "keep" && !replaceKeys)}>
            Save
          </button>
        </>
      }
    >
      <form
        id="sftp-edit"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        <Field label="Password" help={account.has_password ? "The account has a password now." : "The account signs in with keys only now."}>
          <select className="input" value={pw} onChange={(e) => setPw(e.target.value as typeof pw)}>
            <option value="keep">Keep as it is</option>
            <option value="generate">Generate a new strong password</option>
            <option value="password">Choose a new password</option>
            {account.has_password && <option value="none">Remove it (keys only)</option>}
          </select>
        </Field>
        {pw === "password" && (
          <Field label="New password" help="At least 12 characters.">
            <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} minLength={12} required autoComplete="new-password" />
          </Field>
        )}
        <label className="row small">
          <input type="checkbox" checked={replaceKeys} onChange={(e) => setReplaceKeys(e.target.checked)} />
          Replace the public keys ({account.public_keys.length} now)
        </label>
        {replaceKeys && (
          <Field label="Public keys" help="One per line; leave empty to remove all keys.">
            <textarea className="input mono" rows={3} value={keys} onChange={(e) => setKeys(e.target.value)} placeholder="ssh-ed25519 AAAA… you@laptop" />
          </Field>
        )}
        <p className="faint small" style={{ margin: 0 }}>
          Sessions already open stay open; new sign-ins use the new password and keys.
        </p>
      </form>
    </Modal>
  );
}

function SettingsTab({ site, phpVersions, phpLimits, panelAdmin }: { site: Site; phpVersions: string[]; phpLimits: boolean; panelAdmin: boolean }) {
  return (
    <div className="stack" style={{ maxWidth: 640 }}>
      <UsageCard site={site} />
      <RuntimeCard site={site} phpVersions={phpVersions} />
      {site.php_version && <PHPOptionsCard site={site} limits={phpLimits} />}
      {site.php_version && <LaravelCard site={site} systemd={phpLimits} />}
      {panelAdmin && <MoveProjectCard kind="sites" id={site.id} current={site.project_id} what="the site" />}
      <DeleteCard site={site} />
    </div>
  );
}

function RuntimeCard({ site, phpVersions }: { site: Site; phpVersions: string[] }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [version, setVersion] = useState(site.php_version);
  const options = Array.from(new Set([...phpVersions, ...(site.php_version ? [site.php_version] : [])]));
  const save = useMutation({
    mutationFn: () => api.setSitePHP(site.id, version),
    onSuccess: (r) => {
      qc.setQueryData(["site", site.id], (old: object | undefined) => ({ ...old, site: r.site, sync: r.sync }));
      qc.invalidateQueries({ queryKey: ["sites"] });
      toast(version ? `Switched to PHP ${version}` : "Switched to static");
    },
  });
  return (
    <form
      className="card card-body form"
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <h2>Runtime</h2>
      <ErrorBox error={save.error} />
      <Field
        label="Type"
        help="Static sites never expose .php files. PHP runs in a dedicated pool as this site's system user, limited to its directory."
      >
        <select className="input" value={version} onChange={(e) => setVersion(e.target.value)}>
          <option value="">Static</option>
          {options.map((v) => (
            <option key={v} value={v}>
              PHP {v}
            </option>
          ))}
        </select>
      </Field>
      <div>
        <button className="btn primary" disabled={save.isPending || version === site.php_version}>
          Apply
        </button>
      </div>
    </form>
  );
}

// UsageCard shows the disk space a site takes and its soft quota.
function UsageCard({ site }: { site: Site }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [quota, setQuota] = useState(site.quota_mb ? String(site.quota_mb) : "");
  const update = (r: { site: Site }) => qc.setQueryData(["site", site.id], (old: { site: Site } | undefined) => (old ? { ...old, site: r.site } : old));
  const measure = useMutation({ mutationFn: () => api.measureSite(site.id), onSuccess: update, onError: (e) => toast(errorMessage(e), true) });
  const save = useMutation({
    mutationFn: () => api.setSiteQuota(site.id, quota.trim() ? Number(quota) : 0),
    onSuccess: (r) => {
      update(r);
      toast(r.site.quota_mb ? `Quota set to ${r.site.quota_mb} MB` : "Quota removed");
    },
    onError: (e) => toast(errorMessage(e), true),
  });
  const u = site.usage;
  const total = u.disk_bytes + u.db_bytes;
  const pct = site.quota_mb ? Math.min(100, (total / (site.quota_mb * 1048576)) * 100) : 0;
  return (
    <div className="card card-body form">
      <div className="row" style={{ justifyContent: "space-between" }}>
        <h2>Disk usage</h2>
        <button className="btn sm" disabled={measure.isPending} onClick={() => measure.mutate()}>
          <Icon name="refresh" size={14} /> {measure.isPending ? "Measuring…" : "Measure now"}
        </button>
      </div>
      {u.at ? (
        <p className="muted small" style={{ margin: 0 }}>
          {formatBytes(total)} in all: files {formatBytes(u.disk_bytes)} ({u.disk_files.toLocaleString()} files), databases{" "}
          {formatBytes(u.db_bytes)}. Measured {formatDate(u.at)}; again every 6 hours.
        </p>
      ) : (
        <p className="muted small" style={{ margin: 0 }}>Not measured yet: it is measured every 6 hours.</p>
      )}
      {site.quota_mb > 0 && u.at && (
        <div className="meter" title={`${pct.toFixed(0)}% of the quota`}>
          <div className={site.over_quota ? "meter-fill err" : "meter-fill"} style={{ width: `${pct}%` }} />
        </div>
      )}
      <Field label="Quota, MB" help="A soft limit: nothing stops the site, but going over it is notified (event “A site goes over its disk quota”). Empty: none.">
        <div className="row">
          <input className="input mono" style={{ width: 160 }} type="number" min={0} value={quota} onChange={(e) => setQuota(e.target.value)} />
          <button className="btn" disabled={save.isPending} onClick={() => save.mutate()}>
            Save
          </button>
        </div>
      </Field>
    </div>
  );
}

// PHPOptionsCard edits the PHP options a site may change.
function PHPOptionsCard({ site, limits }: { site: Site; limits: boolean }) {
  const qc = useQueryClient();
  const toast = useToast();
  const cur = site.php_settings ?? {};
  const [f, setF] = useState({
    memory: String(cur.memory_mb ?? ""),
    upload: String(cur.upload_mb ?? ""),
    children: String(cur.max_children ?? ""),
    time: String(cur.max_execution_time ?? ""),
    vars: String(cur.max_input_vars ?? ""),
    timezone: cur.timezone ?? "",
    display: !!cur.display_errors,
    cpu: String(cur.cpu_percent ?? ""),
    ram: String(cur.ram_mb ?? ""),
  });
  const n = (v: string) => (v.trim() === "" ? undefined : Number(v));
  const save = useMutation({
    mutationFn: () =>
      api.setSitePHPSettings(site.id, {
        memory_mb: n(f.memory),
        upload_mb: n(f.upload),
        max_children: n(f.children),
        max_execution_time: n(f.time),
        max_input_vars: n(f.vars),
        timezone: f.timezone.trim() || undefined,
        display_errors: f.display || undefined,
        cpu_percent: n(f.cpu),
        ram_mb: n(f.ram),
      }),
    onSuccess: (r) => {
      qc.setQueryData(["site", site.id], (old: { site: Site } | undefined) => (old ? { ...old, site: r.site } : old));
      toast("PHP options applied");
    },
  });
  const num = (k: keyof typeof f, label: string, placeholder: string, help: string) => (
    <Field label={label} help={help}>
      <input
        className="input mono"
        type="number"
        value={f[k] as string}
        placeholder={placeholder}
        onChange={(e) => setF({ ...f, [k]: e.target.value })}
      />
    </Field>
  );
  return (
    <form
      className="card card-body form"
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <h2>PHP options</h2>
      <ErrorBox error={save.error} />
      <div className="grid-2">
        {num("memory", "Memory limit, MB", "256", "memory_limit, 32–8192")}
        {num("upload", "Largest upload, MB", "64", "upload_max_filesize and post_max_size")}
        {num("time", "Time limit, seconds", "30", "max_execution_time, 1–3600")}
        {num("children", "PHP processes at once", "10", "requests served in parallel, 1–200")}
        {num("vars", "Input variables", "1000", "max_input_vars, 100–100000")}
        <Field label="Timezone" help="date.timezone, e.g. Europe/Berlin">
          <input className="input mono" value={f.timezone} placeholder="UTC" onChange={(e) => setF({ ...f, timezone: e.target.value })} />
        </Field>
      </div>
      <h3 style={{ margin: "4px 0 0" }}>Limits for the whole site</h3>
      {!limits && (
        <p className="faint small" style={{ margin: 0 }}>
          Not enforced on this server: they need each site's PHP to run as its own systemd service (servers installed with the
          installer on systemd).
        </p>
      )}
      <div className="grid-2">
        {num("cpu", "CPU, %", "no limit", "100% is one core, 10–1600")}
        {num("ram", "Memory, MB", "no limit", "all the site's PHP processes together, 64–65536")}
      </div>
      <label className="row small">
        <input type="checkbox" checked={f.display} onChange={(e) => setF({ ...f, display: e.target.checked })} />
        Show PHP errors in pages (for development; errors always go to the PHP log)
      </label>
      <div>
        <button className="btn primary" disabled={save.isPending}>
          Apply
        </button>
      </div>
    </form>
  );
}

// LaravelCard turns a Laravel site's scheduler and queue workers on or off.
function LaravelCard({ site, systemd }: { site: Site; systemd: boolean }) {
  const qc = useQueryClient();
  const toast = useToast();
  const cur = site.laravel ?? { scheduler: false, workers: 0 };
  const [scheduler, setScheduler] = useState(cur.scheduler);
  const [workers, setWorkers] = useState(String(cur.workers));
  const save = useMutation({
    mutationFn: () => api.setSiteLaravel(site.id, { scheduler, workers: Number(workers) || 0 }),
    onSuccess: (r) => {
      qc.setQueryData(["site", site.id], (old: { site: Site } | undefined) => (old ? { ...old, site: r.site } : old));
      toast("Laravel processes applied");
    },
  });
  const running = cur.scheduler || cur.workers > 0;
  if (!systemd && !running) {
    return (
      <div className="card card-body stack">
        <h2>Laravel</h2>
        <p className="faint small" style={{ margin: 0 }}>
          The scheduler and queue workers of a Laravel site need each site's PHP to run as its own systemd service (servers installed
          with the installer on systemd).
        </p>
      </div>
    );
  }
  return (
    <form
      className="card card-body form"
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <h2>Laravel</h2>
      <p className="faint small" style={{ margin: 0 }}>
        For a Laravel project in the site's folder (with <code>artisan</code> beside <code>public/</code>). The processes run as the
        site's user on its primary server, within the site's CPU and memory limits; their output goes to the server's journal, errors
        to the PHP log. Unlike pages, they may start other programs (the scheduler runs commands that way).
      </p>
      <ErrorBox error={save.error} />
      <label className="row small">
        <input type="checkbox" checked={scheduler} onChange={(e) => setScheduler(e.target.checked)} />
        Run the scheduler (<code>artisan schedule:work</code>: scheduled tasks every minute)
      </label>
      <Field label="Queue workers" help="artisan queue:work processes, 0–8; each restarts every hour to pick up new code">
        <input className="input mono" type="number" min={0} max={8} value={workers} onChange={(e) => setWorkers(e.target.value)} />
      </Field>
      <div>
        <button className="btn primary" disabled={save.isPending}>
          Apply
        </button>
      </div>
    </form>
  );
}

// LogsTab shows the end of the site's PHP error log.
function LogsTab({ site, user }: { site: Site; user: User }) {
  const log = useQuery({ queryKey: ["php-log", site.id], queryFn: () => api.phpLog(site.id), refetchInterval: 10_000 });
  return (
    <div className="stack">
      {can(user, "member") && <AccessLogCard base={`/api/v1/sites/${site.id}`} />}
      <div className="card">
        <div className="card-head">
          <h2>PHP errors</h2>
          <div className="row">
            {log.data && log.data.size > 0 && (
              <a className="btn sm" href={api.downloadUrl(site.id, log.data.path)}>
                <Icon name="download" size={14} /> Download ({formatBytes(log.data.size)})
              </a>
            )}
            <button className="btn sm" onClick={() => log.refetch()} disabled={log.isFetching}>
              <Icon name="refresh" size={14} /> Refresh
            </button>
          </div>
        </div>
        <div className="card-body stack">
          {log.isPending ? (
            <Loading />
          ) : log.error ? (
            <ErrorBox error={log.error} />
          ) : !site.php_version && !log.data.log ? (
            <div className="empty small">This is a static site: it runs no PHP.</div>
          ) : !log.data.log ? (
            <div className="empty small">No PHP errors logged.</div>
          ) : (
            <>
              {log.data.truncated && <p className="faint small" style={{ margin: 0 }}>The last 256 KB of the log; download it for all of it.</p>}
              <LogView text={log.data.log} tall />
            </>
          )}
        </div>
      </div>
    </div>
  );
}

function DeleteCard({ site }: { site: Site }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const toast = useToast();
  const [confirmName, setConfirmName] = useState("");
  const [open, setOpen] = useState(false);
  const del = useMutation({
    mutationFn: () => api.deleteSite(site.id),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["sites"] });
      toast(r.cleanup_error ? `Site deleted; files left on server: ${r.cleanup_error}` : "Site deleted", !!r.cleanup_error);
      navigate("/sites");
    },
  });
  return (
    <div className="card card-body stack">
      <h2>Delete site</h2>
      <p className="muted" style={{ margin: 0 }}>
        The site stops being served immediately and its SFTP accounts are revoked. Files are moved to the server's trash
        folder (<code>.trash</code> in the sites directory) rather than erased, so an operator can still recover them.
      </p>
      <div>
        <button className="btn danger" onClick={() => setOpen(true)}>
          <Icon name="trash" /> Delete {site.name}
        </button>
      </div>
      {open && (
        <Modal
          title={`Delete ${site.name}?`}
          onClose={() => setOpen(false)}
          footer={
            <>
              <button className="btn" onClick={() => setOpen(false)}>
                Cancel
              </button>
              <button className="btn danger solid" disabled={confirmName !== site.name || del.isPending} onClick={() => del.mutate()}>
                Delete site
              </button>
            </>
          }
        >
          <div className="form">
            <ErrorBox error={del.error} />
            <Field label={`Type ${site.name} to confirm`}>
              <input className="input" value={confirmName} onChange={(e) => setConfirmName(e.target.value)} autoFocus />
            </Field>
          </div>
        </Modal>
      )}
    </div>
  );
}

function AdminerCard({ site }: { site: Site }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const status = useQuery({ queryKey: ["adminer", site.id], queryFn: () => api.siteAdminer(site.id), retry: false });
  const change = useMutation({
    mutationFn: (install: boolean) => (install ? api.installAdminer(site.id) : api.removeAdminer(site.id)),
    onSuccess: (r) => qc.setQueryData(["adminer", site.id], r),
    onError: (e) => toast(errorMessage(e), true),
  });
  const a = status.data?.adminer;
  return (
    <div className="card card-body stack">
      <div className="row" style={{ justifyContent: "space-between" }}>
        <h2>Adminer</h2>
        {a?.installed && <span className="faint small">{a.version}</span>}
      </div>
      <p className="muted small">
        A page of the site for browsing and editing its databases, run by the site&apos;s PHP. It signs in to this
        server&apos;s MariaDB only, with a database user and password from above. Its address is random; remove it when you
        are done.
      </p>
      {status.isPending ? (
        <Loading />
      ) : status.error ? (
        <ErrorBox error={status.error} />
      ) : a?.installed ? (
        <>
          {a.url ? <CodeBox text={a.url} /> : <code className="mono">{a.path}</code>}
          <div className="row">
            {a.url && (
              <a className="btn sm primary" href={a.url} target="_blank" rel="noopener noreferrer">
                Open Adminer
              </a>
            )}
            <button
              className="btn sm"
              disabled={change.isPending}
              onClick={async () => {
                if (!(await confirm({ title: "Move Adminer to a new address?", body: <p>The current address stops working.</p>, confirm: "New address" })))
                  return;
                change.mutate(true);
              }}
            >
              New address
            </button>
            <button className="btn sm ghost" disabled={change.isPending} onClick={() => change.mutate(false)}>
              <Icon name="trash" size={14} /> Remove
            </button>
          </div>
        </>
      ) : (
        <div>
          <button className="btn sm" disabled={change.isPending} onClick={() => change.mutate(true)}>
            {change.isPending ? "Installing…" : "Install Adminer"}
          </button>
        </div>
      )}
    </div>
  );
}

function DatabasesTab({ site, user }: { site: Site; user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const dbs = useQuery({ queryKey: ["databases", site.id], queryFn: () => api.databases(site.id) });
  const [name, setName] = useState("");
  const [creds, setCreds] = useState<DatabaseCredentials | null>(null);
  const prefix = `s${site.id}_`;
  const create = useMutation({
    mutationFn: () => api.createDatabase(site.id, name.trim()),
    onSuccess: (r) => {
      setCreds(r.credentials);
      setName("");
      qc.invalidateQueries({ queryKey: ["databases", site.id] });
    },
  });
  const act = async (f: () => Promise<unknown>) => {
    try {
      await f();
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };

  if (dbs.isPending) return <Loading />;
  if (dbs.error) return <ErrorBox error={dbs.error} />;
  if (!dbs.data.mariadb)
    return (
      <div className="card empty">
        <h3>MariaDB is not installed on this server</h3>
        <p>
          Install it with <code>apt install mariadb-server</code>; the agent detects it automatically.
        </p>
      </div>
    );

  return (
    <div className="stack">
      {creds && (
        <div className="card card-body stack">
          <h2>Database {creds.database} is ready</h2>
          <div className="alert warn small">Copy the password now. It is not stored and will not be shown again.</div>
          <dl className="kv">
            <dt>Host</dt>
            <dd className="mono">{creds.host}</dd>
            <dt>Database</dt>
            <dd className="mono">{creds.database}</dd>
            <dt>Username</dt>
            <dd className="mono">{creds.username}</dd>
            <dt>Password</dt>
            <dd>
              <CodeBox text={creds.password} />
            </dd>
          </dl>
          <div>
            <button className="btn sm" onClick={() => setCreds(null)}>
              Done
            </button>
          </div>
        </div>
      )}
      <div className="card">
        <div className="card-head">
          <h2>Databases</h2>
          <span className="faint small">{dbs.data.mariadb}</span>
        </div>
        {dbs.data.databases.length === 0 ? (
          <div className="empty small">No databases yet.</div>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Database</th>
                <th>User</th>
                <th>Created</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {dbs.data.databases.map((d) => (
                <tr key={d.id}>
                  <td className="mono">{d.name}</td>
                  <td className="mono">{d.username}</td>
                  <td className="muted">{formatDate(d.created_at)}</td>
                  <td className="num">
                    {can(user, "admin") && (
                      <div className="row" style={{ justifyContent: "flex-end" }}>
                        <button
                          className="btn sm"
                          onClick={async () => {
                            if (!(await confirm({ title: "Reset password?", body: <p>Apps using the old password stop connecting until updated.</p>, confirm: "Reset" })))
                              return;
                            act(async () => setCreds((await api.resetDatabasePassword(site.id, d.id)).credentials));
                          }}
                        >
                          <Icon name="key" size={14} /> New password
                        </button>
                        <button
                          className="btn sm icon ghost"
                          title="Delete"
                          onClick={async () => {
                            if (!(await confirm({ title: `Delete ${d.name}?`, body: <p>All tables and data in this database are permanently deleted.</p>, confirm: "Delete database", danger: true })))
                              return;
                            act(async () => {
                              await api.deleteDatabase(site.id, d.id);
                              qc.invalidateQueries({ queryKey: ["databases", site.id] });
                            });
                          }}
                        >
                          <Icon name="trash" size={14} />
                        </button>
                      </div>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {can(user, "admin") && site.php_version && <AdminerCard site={site} />}
      {can(user, "admin") && (
        <form
          className="card card-body form"
          style={{ maxWidth: 520 }}
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            create.mutate();
          }}
        >
          <h2>New database</h2>
          <ErrorBox error={create.error} />
          <Field label="Name" help={`Created as ${prefix}${name || "name"}, with a user of the same name.`}>
            <div className="row">
              <span className="mono faint">{prefix}</span>
              <input className="input mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="wordpress" required />
            </div>
          </Field>
          <div>
            <button className="btn primary" disabled={create.isPending || !name.trim()}>
              Create database
            </button>
          </div>
        </form>
      )}
    </div>
  );
}
