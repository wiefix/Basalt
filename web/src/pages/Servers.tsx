import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { api, can, type Enrollment, type User } from "../api";
import { CodeBox, ErrorBox, Field, Icon, Loading, Modal, StatusBadge, formatBytes, timeAgo } from "../components/ui";

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

      {adding && <AddServerModal onClose={() => setAdding(false)} />}
    </>
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
