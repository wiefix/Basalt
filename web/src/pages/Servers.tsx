import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { api, can, type Enrollment, type User } from "../api";
import { CodeBox, ErrorBox, Field, Icon, Loading, Modal, StatusBadge, errorMessage, formatBytes, timeAgo, useConfirm, useToast } from "../components/ui";

export function ServersPage({ user }: { user: User }) {
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers, refetchInterval: 10_000 });
  const [adding, setAdding] = useState(false);

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Servers</h1>
          <div className="sub">Machines managed by this panel.</div>
        </div>
        {can(user, "admin") && (
          <button className="btn primary" onClick={() => setAdding(true)}>
            <Icon name="plus" /> Add server
          </button>
        )}
      </div>

      {servers.isPending ? (
        <Loading />
      ) : servers.error ? (
        <ErrorBox error={servers.error} />
      ) : servers.data.servers.length === 0 ? (
        <div className="card empty">
          <h3>No servers yet</h3>
          <p>Add a server and run one command on it to connect it to the panel.</p>
          {can(user, "admin") && (
            <button className="btn primary" onClick={() => setAdding(true)}>
              <Icon name="plus" /> Add your first server
            </button>
          )}
        </div>
      ) : (
        <div className="card table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Status</th>
                <th>System</th>
                <th className="num">CPU</th>
                <th className="num">Memory</th>
                <th>Last seen</th>
              </tr>
            </thead>
            <tbody>
              {servers.data.servers.map((s) => (
                <tr key={s.id}>
                  <td>
                    <Link to={`/servers/${s.id}`}>
                      <strong>{s.name}</strong>
                    </Link>
                    {s.hostname && <div className="faint small">{s.hostname}</div>}
                  </td>
                  <td>
                    <StatusBadge status={s.status} />
                  </td>
                  <td className="muted">{s.distro || s.os ? `${s.distro || s.os} · ${s.arch}` : "—"}</td>
                  <td className="num">{s.cpu_count ? `${s.cpu_count} cores` : "—"}</td>
                  <td className="num">{s.mem_total ? formatBytes(s.mem_total) : "—"}</td>
                  <td className="muted">{s.status === "pending" ? "waiting for agent" : timeAgo(s.last_seen)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {can(user, "admin") && servers.data && servers.data.servers.length > 0 && <UpdatesCard />}
      {can(user, "admin") && servers.data && servers.data.servers.length > 0 && <StandbysCard user={user} />}

      {adding && <AddServerModal onClose={() => setAdding(false)} />}
    </>
  );
}

// UpdatesCard shows the versions of Basalt in use and updates every server
// to the newest release.
function UpdatesCard() {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const info = useQuery({
    queryKey: ["updates"],
    queryFn: api.updates,
    refetchInterval: (q) => (q.state.data?.running ? 2000 : 60_000),
  });
  const check = useMutation({
    mutationFn: api.checkUpdates,
    onSuccess: (d) => qc.setQueryData(["updates"], d),
    onError: (e) => toast(errorMessage(e), true),
  });
  const apply = useMutation({
    mutationFn: api.applyUpdate,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["updates"] }),
    onError: (e) => toast(errorMessage(e), true),
  });
  if (info.isPending) return null;
  if (info.error) return <ErrorBox error={info.error} />;
  const d = info.data;
  const outdated = d.servers.filter((s) => s.version !== d.latest);
  const start = async () => {
    const ok = await confirm({
      title: `Update to ${d.latest}?`,
      body: (
        <p>
          Every online server installed with the installer gets the new version; each checks the release signature, then
          restarts the Basalt services (sites stay up, the proxy restarts in a moment). The panel restarts too.
        </p>
      ),
      confirm: "Update",
    });
    if (ok) apply.mutate();
  };
  const run = d.run;
  return (
    <div className="card" style={{ marginTop: 16 }}>
      <div className="card-head">
        <h2>Updates</h2>
        <div className="row wrap">
          {d.enabled && (
            <button className="btn sm" disabled={check.isPending || d.running} onClick={() => check.mutate()}>
              <Icon name="refresh" size={14} /> {check.isPending ? "Checking…" : "Check now"}
            </button>
          )}
          {d.available && (
            <button className="btn sm primary" disabled={apply.isPending || d.running} onClick={start}>
              {d.running ? "Updating…" : `Update to ${d.latest}`}
            </button>
          )}
        </div>
      </div>
      <div className="card-body stack">
        <p className="muted small" style={{ margin: 0 }}>
          This panel runs <span className="mono">{d.current}</span>.{" "}
          {!d.enabled
            ? "Update checks are turned off (--release-url)."
            : d.latest
              ? d.available
                ? `Basalt ${d.latest} is available.`
                : `It is the newest release (${d.latest}).`
              : "No release found yet."}{" "}
          {d.checked_at && `Checked ${timeAgo(d.checked_at)}.`}
        </p>
        {d.error && <div className="alert err small">Last check: {d.error}</div>}
        {run && (
          <div className={`alert small ${run.state === "failed" ? "err" : run.state === "done" ? "ok" : "warn"}`}>
            Update to {run.version} by {run.by}:{" "}
            {run.state === "downloading"
              ? "downloading the release…"
              : run.state === "preparing"
                ? "servers are fetching and checking it…"
                : run.state === "restarting"
                  ? "restarting the services…"
                  : run.state === "done"
                    ? `done ${run.finished_at ? timeAgo(run.finished_at) : ""}.`
                    : `failed: ${run.error}`}
            {run.state === "done" && run.error ? ` ${run.error}.` : ""}
            {run.servers
              .filter((s) => s.error)
              .map((s) => (
                <div key={s.id}>
                  {s.name}: {s.error}
                </div>
              ))}
          </div>
        )}
        {d.latest && outdated.length > 0 && (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>Server</th>
                  <th>Version</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {outdated.map((s) => (
                  <tr key={s.id}>
                    <td>{s.name}</td>
                    <td className="mono">{s.version || "—"}</td>
                    <td className="muted small">
                      {!s.online ? "offline" : !s.updatable ? "not installed by the installer: re-run it to update" : ""}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}

// StandbysCard lists the standby panels: copies of this panel, kept up to
// date, that can take over if it fails.
function StandbysCard({ user }: { user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const [adding, setAdding] = useState(false);
  const list = useQuery({ queryKey: ["standbys"], queryFn: api.standbys, refetchInterval: 5000 });
  const remove = useMutation({
    mutationFn: api.deleteStandby,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["standbys"] }),
    onError: (e) => toast(errorMessage(e), true),
  });
  if (list.isPending) return null;
  if (list.error) return <ErrorBox error={list.error} />;
  const owner = can(user, "owner");
  const { standbys, available } = list.data;
  if (!available && standbys.length === 0) return null;
  const drop = async (id: number, name: string) => {
    const ok = await confirm({
      title: `Remove standby panel ${name}?`,
      body: (
        <p>
          It stops getting this panel's database and keeps the copy it has. Its server stays in the list: remove it
          there if you no longer need it.
        </p>
      ),
      confirm: "Remove",
      danger: true,
    });
    if (ok) remove.mutate(id);
  };
  return (
    <div className="card" style={{ marginTop: 16 }}>
      <div className="card-head">
        <h2>Standby panels</h2>
        {owner && available && (
          <button className="btn sm" onClick={() => setAdding(true)}>
            <Icon name="plus" size={14} /> Add standby panel
          </button>
        )}
      </div>
      <div className="card-body stack">
        <p className="muted small" style={{ margin: 0 }}>
          A standby panel on another server keeps a copy of this panel, updated within a second. If this panel's
          server fails, the standby takes over: run <span className="mono">basalt server promote</span> on it. Sites
          and apps keep running either way.
        </p>
        {standbys.length > 0 && (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Copy</th>
                  <th>Last change</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {standbys.map((sb) => (
                  <tr key={sb.id}>
                    <td>
                      {sb.server_id ? <Link to={`/servers/${sb.server_id}`}>{sb.name}</Link> : sb.name}
                      {sb.last_ip && <div className="faint small">{sb.last_ip}</div>}
                    </td>
                    <td>
                      {!sb.enrolled ? (
                        <span className="badge">waiting to be installed</span>
                      ) : sb.connected && sb.ready ? (
                        <span className="badge ok">up to date</span>
                      ) : sb.connected ? (
                        <span className="badge warn">copying…</span>
                      ) : (
                        <span className="badge err">not connected</span>
                      )}
                      {sb.error && <div className="small" style={{ color: "var(--err)" }}>{sb.error}</div>}
                    </td>
                    <td className="muted">
                      {sb.applied_at ? timeAgo(sb.applied_at) : sb.last_seen_at ? `seen ${timeAgo(sb.last_seen_at)}` : "—"}
                    </td>
                    <td className="num">
                      {owner && (
                        <button className="btn icon ghost" title="Remove" onClick={() => drop(sb.id, sb.name)}>
                          <Icon name="trash" size={14} />
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {adding && <AddStandbyModal onClose={() => setAdding(false)} />}
    </div>
  );
}

function AddStandbyModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const create = useMutation({
    mutationFn: () => api.createStandby(name.trim()),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["standbys"] });
      qc.invalidateQueries({ queryKey: ["servers"] });
    },
  });
  if (create.data) {
    const e = create.data.enrollment;
    const minutes = Math.max(0, Math.round((new Date(e.expires_at).getTime() - Date.now()) / 60000));
    return (
      <Modal title="Install the standby panel" onClose={onClose} wide footer={<button className="btn primary" onClick={onClose}>Done</button>}>
        <div className="stack">
          <p className="muted" style={{ margin: 0 }}>
            Run this as <strong>root</strong> on a fresh Debian or Ubuntu server, not this panel's. It installs Basalt
            as a standby of this panel and connects its agent as server <strong>{create.data.server.name}</strong>. The
            command works once and expires in <strong>{minutes} minutes</strong>.
          </p>
          <CodeBox text={e.install_command} />
          <div className="alert small">
            Add <span className="mono">--domain</span> with this panel's domain to the command, so the standby serves it
            once it takes over (then point the domain at it). Open port 8443 of this panel to the standby.
          </div>
          <div className="alert warn small">
            A standby holds everything this panel does, its secrets included: protect its server like this one.
          </div>
        </div>
      </Modal>
    );
  }
  return (
    <Modal
      title="Add standby panel"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="add-standby" disabled={create.isPending || !name.trim()}>
            Continue
          </button>
        </>
      }
    >
      <form
        id="add-standby"
        className="form"
        onSubmit={(ev: FormEvent) => {
          ev.preventDefault();
          create.mutate();
        }}
      >
        <ErrorBox error={create.error} />
        <Field label="Name" help="Its server gets this name too, e.g. panel-2.">
          <input className="input" value={name} onChange={(ev) => setName(ev.target.value)} autoFocus required />
        </Field>
      </form>
    </Modal>
  );
}

function AddServerModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [result, setResult] = useState<{ id: number; enrollment: Enrollment } | null>(null);
  const create = useMutation({
    mutationFn: () => api.createServer(name.trim()),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["servers"] });
      setResult({ id: r.server.id, enrollment: r.enrollment });
    },
  });

  if (result) {
    return (
      <Modal
        title="Connect your server"
        onClose={onClose}
        wide
        footer={
          <button className="btn primary" onClick={() => navigate(`/servers/${result.id}`)}>
            Go to server
          </button>
        }
      >
        <EnrollInstructions enrollment={result.enrollment} />
      </Modal>
    );
  }

  return (
    <Modal
      title="Add server"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="add-server" disabled={create.isPending || !name.trim()}>
            Continue
          </button>
        </>
      }
    >
      <form
        id="add-server"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <ErrorBox error={create.error} />
        <Field label="Name" help="A short label, e.g. web-1 or hetzner-fsn.">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} autoFocus required />
        </Field>
      </form>
    </Modal>
  );
}

export function EnrollInstructions({ enrollment }: { enrollment: Enrollment }) {
  const minutes = Math.max(0, Math.round((new Date(enrollment.expires_at).getTime() - Date.now()) / 60000));
  return (
    <div className="stack">
      <p className="muted" style={{ margin: 0 }}>
        Run this as <strong>root</strong> on a fresh Debian or Ubuntu server. It installs Basalt, connects the server
        to this panel and starts everything. The command works once and expires in <strong>{minutes} minutes</strong>.
      </p>
      <CodeBox text={enrollment.install_command} />
      <div className="alert small">
        The command pins this panel's certificate authority by its fingerprint: the installer and the binary are only
        accepted from this panel, so nobody in between can substitute them.
      </div>
      <details>
        <summary className="muted small" style={{ cursor: "pointer" }}>
          Basalt already installed on that server?
        </summary>
        <div style={{ marginTop: 10 }}>
          <CodeBox text={enrollment.command} />
        </div>
      </details>
    </div>
  );
}
