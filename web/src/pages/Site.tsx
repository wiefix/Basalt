import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, NavLink, Route, Routes, useNavigate, useParams } from "react-router";
import { api, can, type Site, type User } from "../api";
import { FileManager } from "../components/FileManager";
import { CodeBox, CopyButton, ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, useConfirm, useToast } from "../components/ui";
import { SyncBadge } from "./Server";

export function SitePage({ user }: { user: User }) {
  const id = Number(useParams().id);
  const site = useQuery({ queryKey: ["site", id], queryFn: () => api.site(id) });
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });

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
            <SyncBadge sync={site.data.sync} />
          </div>
          <div className="sub row wrap">
            {s.domains.map((d) => (
              <a key={d} href={`https://${d}`} target="_blank" rel="noreferrer noopener">
                {d} ↗
              </a>
            ))}
            <span className="faint">
              on <Link to={`/servers/${s.server_id}`}>{server?.name ?? `server #${s.server_id}`}</Link>
            </span>
          </div>
        </div>
      </div>
      {server && server.status !== "online" && (
        <div className="alert warn" style={{ marginBottom: 16 }}>
          The server is {server.status}. Files cannot be managed until its agent reconnects; the site itself keeps running.
        </div>
      )}
      <div className="tabs">
        {tab(`/sites/${id}`, "Files")}
        {tab(`/sites/${id}/domains`, "Domains")}
        {tab(`/sites/${id}/sftp`, "SFTP")}
        {can(user, "admin") && tab(`/sites/${id}/settings`, "Settings")}
      </div>
      <Routes>
        <Route index element={<FileManager siteId={id} canWrite={can(user, "member")} />} />
        <Route path="domains" element={<DomainsTab site={s} user={user} />} />
        <Route path="sftp" element={<SFTPTab site={s} user={user} host={server?.last_ip} />} />
        <Route path="settings" element={<SettingsTab site={s} />} />
      </Routes>
    </>
  );
}

function DomainsTab({ site, user }: { site: Site; user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [text, setText] = useState(site.domains.join("\n"));
  const save = useMutation({
    mutationFn: () => api.setSiteDomains(site.id, text.split(/[\s,]+/).filter(Boolean)),
    onSuccess: (r) => {
      qc.setQueryData(["site", site.id], { site: r.site, sync: r.sync });
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
        <Field label="One per line" help="Certificates are requested automatically once DNS points at the server.">
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
  const [created, setCreated] = useState<{ username: string; password?: string } | null>(null);
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
            <h2>Account {created.username} created</h2>
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
                      <button className="btn sm icon ghost" title="Delete" onClick={() => remove(u.id, u.username)}>
                        <Icon name="trash" size={14} />
                      </button>
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

function SettingsTab({ site }: { site: Site }) {
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
    <div className="card card-body stack" style={{ maxWidth: 640 }}>
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
