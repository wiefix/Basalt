import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "react-router";
import { api, can, type Enrollment, type Route, type Server, type Sync, type User } from "../api";
import { Chart } from "../components/Chart";
import {
  ErrorBox,
  Field,
  Icon,
  Loading,
  Modal,
  StatusBadge,
  errorMessage,
  formatBytes,
  formatDate,
  formatUptime,
  timeAgo,
  useConfirm,
  useToast,
} from "../components/ui";
import { EnrollInstructions } from "./Servers";

const UNINSTALL = "curl -fsSL https://github.com/wiefix/Basalt/releases/latest/download/install.sh | sudo sh -s -- uninstall";

export function ServerPage({ user }: { user: User }) {
  const id = Number(useParams().id);
  const qc = useQueryClient();
  const navigate = useNavigate();
  const toast = useToast();
  const confirm = useConfirm();
  const [enrollment, setEnrollment] = useState<Enrollment | null>(null);

  const server = useQuery({ queryKey: ["server", id], queryFn: () => api.server(id), refetchInterval: 10_000 });
  const metrics = useQuery({
    queryKey: ["metrics", id],
    queryFn: () => api.serverMetrics(id, 60),
    refetchInterval: 10_000,
    enabled: server.data?.server.status !== "pending",
  });

  const ping = useMutation({
    mutationFn: () => api.pingServer(id),
    onSuccess: (r) => toast(`Agent replied in ${r.rtt_ms.toFixed(1)} ms`),
    onError: (e) => toast(errorMessage(e), true),
  });
  const reset = useMutation({
    mutationFn: () => api.resetEnrollToken(id),
    onSuccess: (r) => setEnrollment(r.enrollment),
    onError: (e) => toast(errorMessage(e), true),
  });

  if (server.isPending) return <Loading />;
  if (server.error) return <ErrorBox error={server.error} />;
  const s = server.data.server;
  const m = server.data.metrics;

  const remove = async () => {
    const ok = await confirm({
      title: `Remove ${s.name}?`,
      body: (
        <>
          <p>
            The agent loses access immediately and stops connecting. Sites, apps and files on the machine keep running, but
            the panel will no longer manage them, and its records of them here are deleted.
          </p>
          <p className="muted small">
            To remove Basalt from the machine afterwards, run there (<span className="mono">--purge</span> also deletes the
            sites, apps and their data):
          </p>
          <pre className="mono small" style={{ whiteSpace: "pre-wrap", wordBreak: "break-all" }}>
            {UNINSTALL}
          </pre>
        </>
      ),
      confirm: "Remove server",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteServer(id);
      qc.invalidateQueries({ queryKey: ["servers"] });
      toast(`${s.name} removed`);
      navigate("/servers");
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };

  const series = (f: (x: NonNullable<typeof metrics.data>["metrics"][number]) => number) =>
    (metrics.data?.metrics ?? []).map((x) => ({ t: new Date(x.at).getTime(), v: f(x) }));

  return (
    <div className="stack">
      <div className="page-head">
        <div>
          <div className="row">
            <h1>{s.name}</h1>
            <StatusBadge status={s.status} />
          </div>
          <div className="sub">
            {s.hostname ? `${s.hostname} · ` : ""}
            {s.status === "pending" ? "Waiting for the agent to connect" : `Last seen ${timeAgo(s.last_seen)}`}
          </div>
        </div>
        <div className="row">
          {s.status === "online" && can(user, "member") && (
            <button className="btn" onClick={() => ping.mutate()} disabled={ping.isPending}>
              <Icon name="refresh" /> Ping
            </button>
          )}
          {can(user, "admin") && (
            <button className="btn danger" onClick={remove}>
              <Icon name="trash" /> Remove
            </button>
          )}
        </div>
      </div>

      {s.status === "pending" && (
        <div className="card card-body stack">
          <h2>Connect this server</h2>
          {enrollment ? (
            <EnrollInstructions enrollment={enrollment} />
          ) : (
            <>
              <p className="muted" style={{ margin: 0 }}>
                The enrollment command is shown only once. Generate a new one if you lost it or it expired
                {s.enroll_expires ? ` (current one expires ${formatDate(s.enroll_expires)})` : ""}.
              </p>
              {can(user, "admin") && (
                <div>
                  <button className="btn primary" onClick={() => reset.mutate()} disabled={reset.isPending}>
                    Generate enrollment command
                  </button>
                </div>
              )}
            </>
          )}
        </div>
      )}

      {s.status !== "pending" && (
        <>
          <div className="grid-4">
            <Stat label="CPU" value={m ? `${m.cpu_pct.toFixed(0)}%` : "—"} hint={`${s.cpu_count ?? "?"} cores · load ${m?.load1.toFixed(2) ?? "—"}`} />
            <Stat
              label="Memory"
              value={m && m.mem_total ? `${((m.mem_used / m.mem_total) * 100).toFixed(0)}%` : "—"}
              hint={m && m.mem_total ? `${formatBytes(m.mem_used)} of ${formatBytes(m.mem_total)}` : "not reported"}
            />
            <Stat
              label="Disk"
              value={m ? `${((m.disk_used / m.disk_total) * 100).toFixed(0)}%` : "—"}
              hint={m ? `${formatBytes(m.disk_used)} of ${formatBytes(m.disk_total)}` : ""}
            />
            <Stat label="Uptime" value={m && m.uptime ? formatUptime(m.uptime) : "—"} hint={s.kernel ? `kernel ${s.kernel}` : ""} />
          </div>

          <div className="grid-2">
            <div className="card card-body">
              <h3>CPU · last hour</h3>
              <Chart points={series((x) => x.cpu_pct)} max={100} format={(v) => `${v.toFixed(0)}%`} />
            </div>
            <div className="card card-body">
              <h3>Memory · last hour</h3>
              <Chart
                points={series((x) => x.mem_used)}
                max={m?.mem_total || undefined}
                format={(v) => formatBytes(v)}
              />
            </div>
          </div>

          <div className="card card-body">
            <h2 style={{ marginBottom: 14 }}>Details</h2>
            <dl className="kv">
              <dt>System</dt>
              <dd>{[s.distro, s.os, s.arch].filter(Boolean).join(" · ") || "—"}</dd>
              <dt>Site isolation</dt>
              <dd>{s.isolated ? "per-site system users" : "off (agent not running as root on Linux)"}</dd>
              <dt>PHP</dt>
              <dd>{s.php_versions?.length ? s.php_versions.join(", ") : "not installed"}</dd>
              <dt>Docker</dt>
              <dd>{s.docker || "not installed"}</dd>
              {s.docker && s.docker_security && (
                <>
                  <dt>Container isolation</dt>
                  <dd>
                    <DockerSecurity sec={s.docker_security} />
                  </dd>
                </>
              )}
              <dt>Agent version</dt>
              <dd className="mono">{s.agent_version || "—"}</dd>
              <dt>Connects from</dt>
              <dd className="mono">{s.last_ip || "—"}</dd>
              <dt>Public addresses</dt>
              <dd>
                <PublicIPs server={s} canEdit={can(user, "admin")} />
              </dd>
              <dt>Certificate</dt>
              <dd>renews automatically · expires {formatDate(s.cert_expires)}</dd>
              <dt>Added</dt>
              <dd>{formatDate(s.created_at)}</dd>
            </dl>
          </div>

          {s.status === "online" && <CertificatesCard serverId={id} />}
          {s.status === "online" && can(user, "admin") && <SFTPBansCard serverId={id} />}
          {s.status === "online" && can(user, "admin") && <TrashCard serverId={id} />}
          <RoutesCard serverId={id} user={user} />
        </>
      )}
    </div>
  );
}

// PublicIPs shows (and lets admins set) the addresses automatic DNS
// records point to.
function PublicIPs({ server, canEdit }: { server: Server; canEdit: boolean }) {
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [text, setText] = useState("");
  const save = useMutation({
    mutationFn: () => api.setPublicIPs(server.id, text.split(/[\s,]+/).filter(Boolean)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["server", server.id] });
      setEditing(false);
    },
  });
  const source =
    server.public_ips.length > 0
      ? "set by an admin"
      : server.detected_ips.length > 0
        ? "found on the server"
        : server.dns_ips.length > 0
          ? "the address it connects from"
          : "";
  return (
    <div className="stack" style={{ gap: 6 }}>
      <div className="row wrap">
        {server.dns_ips.length ? (
          server.dns_ips.map((ip) => (
            <code key={ip}>{ip}</code>
          ))
        ) : (
          <span className="muted">none known: automatic DNS records are not created for this server</span>
        )}
        {source && <span className="faint small">{source}</span>}
        {canEdit && !editing && (
          <button
            className="btn sm ghost"
            onClick={() => {
              setText(server.public_ips.join("\n"));
              setEditing(true);
            }}
          >
            <Icon name="edit" size={14} /> Change
          </button>
        )}
      </div>
      {editing && (
        <form
          className="stack"
          style={{ gap: 6 }}
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            save.mutate();
          }}
        >
          <ErrorBox error={save.error} />
          <textarea className="input mono" rows={2} value={text} onChange={(e) => setText(e.target.value)} placeholder="203.0.113.10" autoFocus />
          <div className="faint small">
            Used for the A/AAAA records of sites and apps on this server whose domains are in your DNS zones. Leave empty
            to use the public addresses found on the server{server.detected_ips.length ? ` (${server.detected_ips.join(", ")})` : ""}.
          </div>
          <div className="row">
            <button className="btn sm primary" disabled={save.isPending}>
              Save
            </button>
            <button type="button" className="btn sm" onClick={() => setEditing(false)}>
              Cancel
            </button>
          </div>
        </form>
      )}
    </div>
  );
}

export function Stat({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="card stat">
      <div className="label">{label}</div>
      <div className="value">{value}</div>
      <div className="hint">{hint}</div>
    </div>
  );
}

function DockerSecurity({ sec }: { sec: NonNullable<Server["docker_security"]> }) {
  const item = (ok: boolean, label: string, title: string) => (
    <span className={`badge ${ok ? "ok" : "warn"}`} title={title}>
      {label}
    </span>
  );
  return (
    <span className="row wrap" style={{ gap: 4 }}>
      {item(
        sec.userns || !!sec.rootless,
        sec.rootless ? "rootless" : sec.userns ? "user namespaces" : "no user namespaces",
        sec.userns || sec.rootless
          ? "Root in a container is an unprivileged user on the host."
          : "Root in a container is root on the host. Enable userns-remap in /etc/docker/daemon.json (the installer does it for a Docker it installs).",
      )}
      {item(!!sec.seccomp, sec.seccomp ? `seccomp: ${sec.seccomp}` : "no seccomp", "System calls containers may make are filtered.")}
      {(sec.apparmor || sec.selinux) && item(true, sec.apparmor ? "AppArmor" : "SELinux", "Mandatory access control confines containers.")}
      {item(sec.cgroup_v2, sec.cgroup_v2 ? "cgroup v2" : "cgroup v1", "Resource limits and accounting.")}
    </span>
  );
}

export function SyncBadge({ sync }: { sync?: Sync }) {
  if (!sync) return null;
  if (sync.error) return <span className="badge err" title={sync.error}>not applied</span>;
  return sync.in_sync ? <span className="badge ok">applied</span> : <span className="badge warn">pending</span>;
}

function RoutesCard({ serverId, user }: { serverId: number; user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const routes = useQuery({ queryKey: ["routes", serverId], queryFn: () => api.routes(serverId) });
  const [adding, setAdding] = useState(false);

  const remove = async (r: Route) => {
    if (!(await confirm({ title: "Remove route?", body: <p>{r.domains.join(", ")} will stop being served.</p>, confirm: "Remove", danger: true })))
      return;
    try {
      await api.deleteRoute(serverId, r.id);
      qc.invalidateQueries({ queryKey: ["routes", serverId] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };

  return (
    <div className="card">
      <div className="card-head">
        <div className="row">
          <h2>Proxy routes</h2>
          <SyncBadge sync={routes.data?.sync} />
        </div>
        {can(user, "admin") && (
          <button className="btn sm" onClick={() => setAdding(true)}>
            <Icon name="plus" size={14} /> Add route
          </button>
        )}
      </div>
      {routes.data?.sync.error && (
        <div className="card-body" style={{ paddingBottom: 0 }}>
          <div className="alert err small">Last change was rejected by the server: {routes.data.sync.error}</div>
        </div>
      )}
      {routes.isPending ? (
        <Loading />
      ) : routes.data && routes.data.routes.length > 0 ? (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Domains</th>
                <th>Target</th>
                <th>TLS</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {routes.data.routes.map((r) => (
                <tr key={r.id}>
                  <td>
                    {r.domains.map((d) => (
                      <div key={d}>
                        <a href={`https://${d}`} target="_blank" rel="noreferrer noopener">
                          {d}
                        </a>
                      </div>
                    ))}
                  </td>
                  <td className="muted">
                    {r.site_id ? (
                      <Link to={`/sites/${r.site_id}`}>site #{r.site_id}</Link>
                    ) : r.app_id ? (
                      <Link to={`/apps/${r.app_id}`}>app #{r.app_id}</Link>
                    ) : r.kind === "reverse_proxy" ? (
                      <span className="mono">→ {r.upstream}</span>
                    ) : (
                      <span className="mono">{r.root}</span>
                    )}
                  </td>
                  <td>
                    <span className="badge plain">{r.tls === "auto" ? "Let's Encrypt" : "internal CA"}</span>
                  </td>
                  <td className="num">
                    {can(user, "admin") && !r.site_id && !r.app_id && (
                      <button className="btn sm icon ghost" onClick={() => remove(r)} title="Remove">
                        <Icon name="trash" size={14} />
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="empty small">No routes. Sites create their routes automatically; add one here to proxy an app.</div>
      )}
      {adding && <AddRouteModal serverId={serverId} onClose={() => setAdding(false)} />}
    </div>
  );
}

function AddRouteModal({ serverId, onClose }: { serverId: number; onClose: () => void }) {
  const qc = useQueryClient();
  const [domains, setDomains] = useState("");
  const [upstream, setUpstream] = useState("127.0.0.1:3000");
  const [tls, setTls] = useState<"auto" | "internal">("auto");
  const [hsts, setHsts] = useState(false);
  const create = useMutation({
    mutationFn: () =>
      api.createRoute(serverId, {
        kind: "reverse_proxy",
        domains: domains.split(/[\s,]+/).filter(Boolean),
        upstream,
        tls,
        hsts,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["routes", serverId] });
      onClose();
    },
  });
  return (
    <Modal
      title="Add reverse proxy route"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="add-route" disabled={create.isPending}>
            Add route
          </button>
        </>
      }
    >
      <form
        id="add-route"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <ErrorBox error={create.error} />
        <Field label="Domains" help="Separate several with spaces or commas. Point their DNS at this server first.">
          <input className="input" value={domains} onChange={(e) => setDomains(e.target.value)} placeholder="app.example.com" required autoFocus />
        </Field>
        <Field label="Upstream" help="host:port the server can reach, e.g. a container or local service.">
          <input className="input mono" value={upstream} onChange={(e) => setUpstream(e.target.value)} required />
        </Field>
        <Field label="Certificate">
          <select className="input" value={tls} onChange={(e) => setTls(e.target.value as "auto" | "internal")}>
            <option value="auto">Let's Encrypt (public domains)</option>
            <option value="internal">Internal CA (private networks)</option>
          </select>
        </Field>
        <label className="check">
          <input type="checkbox" checked={hsts} onChange={(e) => setHsts(e.target.checked)} /> Send HSTS header
        </label>
      </form>
    </Modal>
  );
}

// TrashCard lists the files of deleted sites still on the server (hidden
// when there are none).
function TrashCard({ serverId }: { serverId: number }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const trash = useQuery({ queryKey: ["trash", serverId], queryFn: () => api.trash(serverId), retry: false });
  const remove = useMutation({
    mutationFn: (name: string) => api.removeTrash(serverId, name),
    onSuccess: (d) => qc.setQueryData(["trash", serverId], d),
    onError: (e) => toast(errorMessage(e), true),
  });
  const list = trash.data?.entries ?? [];
  if (trash.isPending || trash.error || list.length === 0) return null;
  const ask = async (name: string, siteId: number) => {
    if (await confirm({ title: `Delete the files of site #${siteId} now?`, body: <p>They cannot be recovered afterwards.</p>, confirm: "Delete", danger: true }))
      remove.mutate(name);
  };
  return (
    <div className="card">
      <div className="card-head">
        <h2>Deleted sites</h2>
      </div>
      <div className="card-body muted small" style={{ paddingBottom: 0 }}>
        The files of deleted sites stay on the server for {trash.data.keep_days} days (in the sites folder, .trash), then go.
      </div>
      <div className="table-wrap">
        <table className="table">
          <tbody>
            {list.map((e) => (
              <tr key={e.name}>
                <td>site #{e.site_id}</td>
                <td className="muted small">deleted {formatDate(e.deleted_at)}</td>
                <td className="num muted">{formatBytes(e.bytes)}</td>
                <td className="num">
                  <button className="btn sm" disabled={remove.isPending} onClick={() => ask(e.name, e.site_id)}>
                    Delete now
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// SFTPBansCard lists the addresses the server's SFTP refuses after too many
// wrong passwords (hidden when there are none).
function SFTPBansCard({ serverId }: { serverId: number }) {
  const qc = useQueryClient();
  const toast = useToast();
  const bans = useQuery({ queryKey: ["sftp-bans", serverId], queryFn: () => api.sftpBans(serverId), retry: false, refetchInterval: 60_000 });
  const unban = useMutation({
    mutationFn: (ip: string) => api.sftpUnban(serverId, ip),
    onSuccess: (d) => qc.setQueryData(["sftp-bans", serverId], d),
    onError: (e) => toast(errorMessage(e), true),
  });
  const list = bans.data?.bans ?? [];
  if (bans.isPending || bans.error || list.length === 0) return null;
  return (
    <div className="card">
      <div className="card-head">
        <h2>Blocked SFTP addresses</h2>
      </div>
      <div className="card-body muted small" style={{ paddingBottom: 0 }}>
        Refused after 10 wrong passwords within 15 minutes: for an hour, doubling on each repeat within a day.
      </div>
      <div className="table-wrap">
        <table className="table">
          <tbody>
            {list.map((b) => (
              <tr key={b.ip}>
                <td className="mono">{b.ip}</td>
                <td className="muted small">
                  until {formatDate(b.until)}
                  {b.strikes > 1 ? ` · blocked ${b.strikes} times today` : ""}
                </td>
                <td className="num">
                  <button className="btn sm" disabled={unban.isPending} onClick={() => unban.mutate(b.ip)}>
                    Unblock
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// CertificatesCard lists the public certificates the server's proxy holds
// for its domains.
function CertificatesCard({ serverId }: { serverId: number }) {
  const certs = useQuery({ queryKey: ["certificates", serverId], queryFn: () => api.serverCertificates(serverId), retry: false });
  const list = certs.data?.certificates ?? [];
  if (certs.isPending || (!certs.error && list.length === 0)) return null;
  return (
    <div className="card">
      <div className="card-head">
        <h2>Certificates</h2>
      </div>
      {certs.error ? (
        <div className="card-body">
          <ErrorBox error={certs.error} />
        </div>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <tbody>
              {list.map((c) => (
                <tr key={c.name}>
                  <td className="mono" style={{ overflowWrap: "anywhere" }}>
                    {c.name}
                  </td>
                  <td className="muted small hide-sm">{c.issuer}</td>
                  <td className="small">
                    {c.not_after ? (
                      <>
                        <span
                          className={`badge ${c.expiring ? "err" : "ok"}`}
                          title={c.expiring ? "Not renewed: certificates are renewed 30 days before they expire" : undefined}
                        >
                          {(c.days_left ?? 0) < 0 ? "expired" : `${c.days_left} days left`}
                        </span>{" "}
                        <span className="faint">until {formatDate(c.not_after)}</span>
                      </>
                    ) : (
                      <span className="badge plain" title="Issued once the domain points to this server">
                        not issued yet
                      </span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
