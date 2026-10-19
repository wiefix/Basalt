import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, can, type HealthCheck, type HealthCheckInput, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, timeAgo, useConfirm, useToast } from "../components/ui";
import { MoveProjectCard, ProjectBadge, ProjectFilter, ProjectSelect, roleIn, useCanCreate, useNewProject, useProjectFilter, useProjects } from "../components/projects";

const statusBadge: Record<HealthCheck["status"], string> = { up: "badge ok", down: "badge err", unknown: "badge plain" };

const target = (c: HealthCheck) =>
  c.kind === "tcp" ? `tcp ${c.host}:${c.port}` : `${c.kind}://${c.host_header || c.host}:${c.port}${c.path}`;

export function MonitoringPage({ user }: { user: User }) {
  const checks = useQuery({ queryKey: ["health-checks"], queryFn: api.healthChecks, refetchInterval: 5_000 });
  const [editing, setEditing] = useState<HealthCheck | "new" | null>(null);
  const [open, setOpen] = useState<number | null>(null);
  const canCreate = useCanCreate(user);
  const projects = useProjects();
  const [project, setProject] = useProjectFilter();
  // Who may change a check: an admin of its project.
  const admin = (c: HealthCheck) => can({ ...user, role: roleIn(user, projects.data?.projects, c.project_id) }, "admin");
  if (checks.isPending) return <Loading />;
  if (checks.error) return <ErrorBox error={checks.error} />;
  const list = checks.data.checks.filter((c) => !project || c.project_id === project);
  const anyAdmin = list.some(admin);
  const down = list.filter((c) => c.status === "down").length;
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Monitoring</h1>
          <div className="sub">
            Every server checks each target; a target is down when most of them fail it twice in a row.
          </div>
        </div>
        <div className="row">
          <ProjectFilter value={project} onChange={setProject} />
          {canCreate && (
            <button className="btn primary" onClick={() => setEditing("new")}>
              <Icon name="plus" /> Add check
            </button>
          )}
        </div>
      </div>
      {list.length === 0 ? (
        <div className="card empty">
          <h3>No health checks</h3>
          <p>Check a website, an API or a port from all your servers at once.</p>
        </div>
      ) : (
        <div className="card">
          {down > 0 && (
            <div className="card-body" style={{ paddingBottom: 0 }}>
              <div className="alert err">
                {down} of {list.length} targets down
              </div>
            </div>
          )}
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>Status</th>
                  <th>Check</th>
                  <th className="hide-sm">Servers</th>
                  {anyAdmin && <th />}
                </tr>
              </thead>
              <tbody>
                {list.map((c) => [
                  <tr key={c.id} className="clickable" onClick={() => setOpen(open === c.id ? null : c.id)}>
                    <td style={{ width: 1, whiteSpace: "nowrap" }}>
                      <span className={statusBadge[c.status]}>{c.status}</span>
                      {c.status_since && <div className="faint small">{timeAgo(c.status_since)}</div>}
                    </td>
                    <td>
                      <div>
                        {c.name}
                        <ProjectBadge id={c.project_id} />
                      </div>
                      <div className="mono faint small" style={{ overflowWrap: "anywhere" }}>
                        {target(c)} · every {c.interval}s
                      </div>
                    </td>
                    <td className="hide-sm">
                      <div className="row wrap" style={{ gap: 4 }}>
                        {c.results.map((r) => (
                          <span
                            key={r.server_id}
                            className={`badge ${r.stale ? "plain" : r.ok ? "ok" : "err"}`}
                            title={r.ok ? `${r.latency_ms} ms` : r.error}
                          >
                            {r.server_name}
                          </span>
                        ))}
                      </div>
                    </td>
                    {anyAdmin && (
                      <td className="num" onClick={(e) => e.stopPropagation()}>
                        {admin(c) && (
                          <button className="btn sm icon ghost" title="Edit" onClick={() => setEditing(c)}>
                            <Icon name="edit" size={14} />
                          </button>
                        )}
                      </td>
                    )}
                  </tr>,
                  open === c.id && (
                    <tr key={`${c.id}-detail`}>
                      <td colSpan={anyAdmin ? 4 : 3}>
                        <CheckDetail check={c} />
                      </td>
                    </tr>
                  ),
                ])}
              </tbody>
            </table>
          </div>
        </div>
      )}
      {editing && <CheckModal check={editing === "new" ? null : editing} panelAdmin={can(user, "admin")} onClose={() => setEditing(null)} />}
    </>
  );
}

function CheckDetail({ check: c }: { check: HealthCheck }) {
  const events = useQuery({ queryKey: ["health-events", c.id, c.status], queryFn: () => api.healthEvents(c.id) });
  return (
    <div className="grid-2">
      <div>
        <h3 style={{ marginTop: 0 }}>Latest results</h3>
        {c.results.length === 0 ? (
          <div className="muted small">No results yet.</div>
        ) : (
          <table className="table">
            <tbody>
              {c.results.map((r) => (
                <tr key={r.server_id}>
                  <td>{r.server_name}</td>
                  <td>
                    <span className={`badge ${r.stale ? "plain" : r.ok ? "ok" : "err"}`}>
                      {r.stale ? "no recent result" : r.ok ? "ok" : "failing"}
                    </span>
                  </td>
                  <td className="small" style={{ overflowWrap: "anywhere" }}>
                    {r.ok ? `${r.latency_ms} ms` : r.error} <span className="faint">· {timeAgo(r.at)}</span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      <div>
        <h3 style={{ marginTop: 0 }}>Changes</h3>
        {events.data?.events.length ? (
          <table className="table">
            <tbody>
              {events.data.events.map((e, i) => (
                <tr key={i}>
                  <td style={{ width: 1 }}>
                    <span className={statusBadge[e.status as HealthCheck["status"]] ?? "badge"}>{e.status}</span>
                  </td>
                  <td className="small">
                    {e.detail}
                    <div className="faint">{formatDate(e.at)}</div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <div className="muted small">No changes yet.</div>
        )}
      </div>
    </div>
  );
}

function CheckModal({ check, panelAdmin, onClose }: { check: HealthCheck | null; panelAdmin: boolean; onClose: () => void }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const [f, setF] = useState<HealthCheckInput>(
    check
      ? { name: check.name, kind: check.kind, host: check.host, port: check.port, path: check.path,
          host_header: check.host_header, insecure: check.insecure, interval: check.interval, timeout: check.timeout }
      : { name: "", kind: "https", host: "", port: 0, path: "/", host_header: "", insecure: false, interval: 30, timeout: 5 },
  );
  const set = (k: keyof HealthCheckInput, v: unknown) => setF({ ...f, [k]: v });
  const [projectId, setProjectId] = useNewProject();
  const save = useMutation({
    mutationFn: async () => {
      const b = { ...f, port: Number(f.port) || 0, interval: Number(f.interval) || 0, timeout: Number(f.timeout) || 0 };
      if (b.kind === "tcp") Object.assign(b, { path: "", host_header: "", insecure: false });
      if (check) await api.updateHealthCheck(check.id, b);
      else await api.createHealthCheck({ ...b, project_id: projectId });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["health-checks"] });
      onClose();
    },
  });
  const remove = async () => {
    if (!check) return;
    const ok = await confirm({ title: `Delete ${check.name}?`, body: <p>The servers stop checking it.</p>, confirm: "Delete", danger: true });
    if (!ok) return;
    try {
      await api.deleteHealthCheck(check.id);
      qc.invalidateQueries({ queryKey: ["health-checks"] });
      onClose();
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  const web = f.kind !== "tcp";
  return (
    <Modal
      title={check ? `Edit ${check.name}` : "New health check"}
      onClose={onClose}
      footer={
        <>
          {check && (
            <button className="btn danger" style={{ marginRight: "auto" }} onClick={remove}>
              Delete
            </button>
          )}
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="health-form" disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <form
        id="health-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        {!check && <ProjectSelect value={projectId} onChange={setProjectId} />}
        <Field label="Name">
          <input className="input" value={f.name} onChange={(e) => set("name", e.target.value)} placeholder="shop" required />
        </Field>
        <Field label="Kind">
          <div className="segmented">
            {(["https", "http", "tcp"] as const).map((k) => (
              <button type="button" key={k} className={f.kind === k ? "active" : ""} onClick={() => set("kind", k)}>
                {k.toUpperCase()}
              </button>
            ))}
          </div>
        </Field>
        <Field label="Target" help="An IP address or host name; for a site behind Basalt's proxy, the server's address and the site's domain below.">
          <div className="row">
            <input className="input mono" value={f.host} onChange={(e) => set("host", e.target.value)} placeholder="203.0.113.10" required />
            <input
              className="input mono"
              style={{ maxWidth: 110 }}
              type="number"
              min={0}
              max={65535}
              value={f.port || ""}
              onChange={(e) => set("port", e.target.value)}
              placeholder={f.kind === "https" ? "443" : f.kind === "http" ? "80" : "port"}
              required={!web}
            />
          </div>
        </Field>
        {web && (
          <>
            <Field label="Path" help="An answer with a status below 400 passes; redirects are not followed.">
              <input className="input mono" value={f.path} onChange={(e) => set("path", e.target.value)} placeholder="/" />
            </Field>
            <Field label="Host name" help="Sent as the Host header (and TLS name). Empty: the target.">
              <input className="input mono" value={f.host_header} onChange={(e) => set("host_header", e.target.value)} placeholder="shop.example.com" />
            </Field>
            {f.kind === "https" && (
              <label className="row small">
                <input type="checkbox" checked={f.insecure} onChange={(e) => set("insecure", e.target.checked)} />
                Accept any certificate
              </label>
            )}
          </>
        )}
        <div className="grid-2">
          <Field label="Every (seconds)">
            <input className="input mono" type="number" min={10} max={600} value={f.interval} onChange={(e) => set("interval", e.target.value)} />
          </Field>
          <Field label="Timeout (seconds)">
            <input className="input mono" type="number" min={1} max={30} value={f.timeout} onChange={(e) => set("timeout", e.target.value)} />
          </Field>
        </div>
      </form>
      {check && panelAdmin && (
        <div style={{ marginTop: 16 }}>
          <MoveProjectCard kind="health-checks" id={check.id} current={check.project_id} what="this check" />
        </div>
      )}
    </Modal>
  );
}
