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
  return (
    <div className="stack">
      <p className="muted" style={{ margin: 0 }}>
        Run these commands on the server as root. The token works once and expires in{" "}
        <strong>{Math.max(0, Math.round((new Date(enrollment.expires_at).getTime() - Date.now()) / 60000))} minutes</strong>.
      </p>
      <Field label="1. Register the agent">
        <CodeBox text={enrollment.command} />
      </Field>
      <Field label="2. Start the agent and the proxy">
        <CodeBox text={"basalt proxy &\nbasalt agent"} />
      </Field>
      <div className="alert small">
        The command contains a fingerprint of this panel's certificate authority. The agent refuses to talk to anything
        else, so the token cannot be intercepted by an impostor.
      </div>
    </div>
  );
}
