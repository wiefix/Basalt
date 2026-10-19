import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { api, appPath, sourceLabel, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, formatDate } from "../components/ui";
import { ProjectBadge, ProjectFilter, ProjectSelect, ServerLink, useCanCreate, useNewProject, useProjectFilter } from "../components/projects";

export function DatabasesPage({ user }: { user: User }) {
  const apps = useQuery({ queryKey: ["apps"], queryFn: api.apps });
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const [adding, setAdding] = useState(false);
  const serverName = (id: number) => servers.data?.servers.find((s) => s.id === id)?.name ?? `#${id}`;
  const [project, setProject] = useProjectFilter();
  const canCreate = useCanCreate(user);
  const list = apps.data?.apps.filter((a) => a.source === "database" && (!project || a.project_id === project)) ?? [];
  const appName = (id: number) => apps.data?.apps.find((a) => a.id === id)?.name ?? `#${id}`;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Databases</h1>
          <div className="sub">PostgreSQL, MySQL, MariaDB and Redis in containers, reachable only from the apps you link.</div>
        </div>
        <div className="row">
          <ProjectFilter value={project} onChange={setProject} />
          {canCreate && (
            <button className="btn primary" onClick={() => setAdding(true)}>
              <Icon name="plus" /> New database
            </button>
          )}
        </div>
      </div>
      {apps.isPending ? (
        <Loading />
      ) : apps.error ? (
        <ErrorBox error={apps.error} />
      ) : list.length === 0 ? (
        <div className="card empty">
          <h3>No databases yet</h3>
          <p>Create one, then link it to the apps that use it.</p>
          {canCreate && (
            <button className="btn primary" onClick={() => setAdding(true)}>
              <Icon name="plus" /> Create a database
            </button>
          )}
        </div>
      ) : (
        <div className="card table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Database</th>
                <th>Engine</th>
                <th>Used by</th>
                <th>Server</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {list.map((a) => (
                <tr key={a.id}>
                  <td>
                    <Link to={appPath(a)}>
                      <strong>{a.name}</strong>
                    </Link>
                    <ProjectBadge id={a.project_id} />
                  </td>
                  <td className="muted">{sourceLabel(a)}</td>
                  <td>
                    {(a.links ?? []).length === 0 ? (
                      <span className="faint">—</span>
                    ) : (
                      (a.links ?? []).map((id) => (
                        <div key={id}>
                          <Link to={`/apps/${id}`}>{appName(id)}</Link>
                        </div>
                      ))
                    )}
                  </td>
                  <td>
                    <ServerLink id={a.server_id} name={serverName(a.server_id)} user={user} className="muted" />
                  </td>
                  <td className="muted">{formatDate(a.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {adding && <NewDatabaseModal onClose={() => setAdding(false)} />}
    </>
  );
}

function NewDatabaseModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const engines = useQuery({ queryKey: ["database-engines"], queryFn: api.databaseEngines, staleTime: Infinity });
  const withDocker = servers.data?.servers.filter((s) => s.docker) ?? [];
  const [serverId, setServerId] = useState<number | "">("");
  const [engine, setEngine] = useState("postgres");
  const [version, setVersion] = useState("");
  const [name, setName] = useState("");
  const [memory, setMemory] = useState("");
  const [projectId, setProjectId] = useNewProject();
  const current = engines.data?.engines.find((e) => e.id === engine);
  const create = useMutation({
    mutationFn: async () => {
      const r = await api.createApp({
        server_id: Number(serverId || withDocker[0]?.id),
        name: name.trim(),
        source: "database",
        engine,
        version: version || current?.versions[0],
        image: "",
        port: 0,
        domains: [],
        tls: "auto",
        volumes: [],
        memory_mb: Number(memory) || 0,
        cpus: 0,
        project_id: projectId,
      });
      await api.deployApp(r.app.id).catch(() => {});
      return r;
    },
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["apps"] });
      navigate(appPath(r.app));
    },
  });

  return (
    <Modal
      title="New database"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="new-db" disabled={create.isPending || withDocker.length === 0 || !current}>
            Create and start
          </button>
        </>
      }
    >
      {servers.isPending || engines.isPending ? (
        <Loading />
      ) : engines.error ? (
        <ErrorBox error={engines.error} />
      ) : withDocker.length === 0 ? (
        <div className="alert warn">
          No server has Docker. Install it (<code>apt install docker.io</code>, or re-run the installer with{" "}
          <code>--with-docker</code>); the agent detects it when it reconnects.
        </div>
      ) : (
        <form
          id="new-db"
          className="form"
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            create.mutate();
          }}
        >
          <ErrorBox error={create.error} />
          <ProjectSelect value={projectId} onChange={setProjectId} />
          <div className="segmented">
            {engines.data.engines.map((e) => (
              <button
                key={e.id}
                type="button"
                className={engine === e.id ? "active" : ""}
                onClick={() => {
                  setEngine(e.id);
                  setVersion("");
                }}
              >
                {e.name}
              </button>
            ))}
          </div>
          <Field label="Name" help="Also its host name for linked apps: lowercase letters, digits and dashes.">
            <input
              className="input mono"
              value={name}
              onChange={(e) => setName(e.target.value.toLowerCase())}
              placeholder={engine === "redis" ? "cache" : "main-db"}
              pattern="[a-z][a-z0-9\-]*[a-z0-9]"
              required
              autoFocus
            />
          </Field>
          <div className="grid-2">
            <Field label="Version" help="Minor updates come with each deployment; the major version is fixed.">
              <select className="input" value={version || current?.versions[0]} onChange={(e) => setVersion(e.target.value)}>
                {current?.versions.map((v) => (
                  <option key={v} value={v}>
                    {current.name} {v}
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Memory limit (MB)" help="Empty for no limit.">
              <input className="input mono" type="number" min={16} value={memory} onChange={(e) => setMemory(e.target.value)} />
            </Field>
          </div>
          <Field label="Server">
            <select className="input" value={serverId || withDocker[0].id} onChange={(e) => setServerId(Number(e.target.value))}>
              {withDocker.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name} · Docker {s.docker} {s.status !== "online" ? `(${s.status})` : ""}
                </option>
              ))}
            </select>
          </Field>
          <p className="faint small" style={{ margin: 0 }}>
            Credentials are generated and stored encrypted. The data lives in a volume on the server; no port is opened.
          </p>
        </form>
      )}
    </Modal>
  );
}
