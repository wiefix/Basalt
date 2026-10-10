import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { api, can, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, formatDate } from "../components/ui";

export function AppsPage({ user }: { user: User }) {
  const apps = useQuery({ queryKey: ["apps"], queryFn: api.apps });
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const [adding, setAdding] = useState(false);
  const serverName = (id: number) => servers.data?.servers.find((s) => s.id === id)?.name ?? `#${id}`;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Apps</h1>
          <div className="sub">Containers deployed from images, with zero-downtime updates.</div>
        </div>
        {can(user, "admin") && (
          <button className="btn primary" onClick={() => setAdding(true)}>
            <Icon name="plus" /> New app
          </button>
        )}
      </div>
      {apps.isPending ? (
        <Loading />
      ) : apps.error ? (
        <ErrorBox error={apps.error} />
      ) : apps.data.apps.length === 0 ? (
        <div className="card empty">
          <h3>No apps yet</h3>
          <p>Run any Docker image on one of your servers and put it behind HTTPS.</p>
          {can(user, "admin") && (
            <button className="btn primary" onClick={() => setAdding(true)}>
              <Icon name="plus" /> Create an app
            </button>
          )}
        </div>
      ) : (
        <div className="card table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>App</th>
                <th>Image</th>
                <th>Domains</th>
                <th>Server</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {apps.data.apps.map((a) => (
                <tr key={a.id}>
                  <td>
                    <Link to={`/apps/${a.id}`}>
                      <strong>{a.name}</strong>
                    </Link>
                  </td>
                  <td className="mono muted">{a.image}</td>
                  <td>
                    {a.domains.length === 0 ? (
                      <span className="faint">—</span>
                    ) : (
                      a.domains.map((d) => (
                        <div key={d}>
                          <a href={`https://${d}`} target="_blank" rel="noreferrer noopener">
                            {d}
                          </a>
                        </div>
                      ))
                    )}
                  </td>
                  <td>
                    <Link to={`/servers/${a.server_id}`} className="muted">
                      {serverName(a.server_id)}
                    </Link>
                  </td>
                  <td className="muted">{formatDate(a.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {adding && <NewAppModal onClose={() => setAdding(false)} />}
    </>
  );
}

function NewAppModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const withDocker = servers.data?.servers.filter((s) => s.docker) ?? [];
  const [serverId, setServerId] = useState<number | "">("");
  const [name, setName] = useState("");
  const [image, setImage] = useState("");
  const [port, setPort] = useState("80");
  const [domains, setDomains] = useState("");
  const [tls, setTls] = useState("auto");
  const create = useMutation({
    mutationFn: async () => {
      const r = await api.createApp({
        server_id: Number(serverId || withDocker[0]?.id),
        name: name.trim(),
        image: image.trim(),
        port: Number(port) || 0,
        domains: domains.split(/[\s,]+/).filter(Boolean),
        tls,
        volumes: [],
        memory_mb: 0,
        cpus: 0,
      });
      // Start the first deployment right away; it runs in the background.
      await api.deployApp(r.app.id).catch(() => {});
      return r;
    },
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["apps"] });
      navigate(`/apps/${r.app.id}`);
    },
  });

  return (
    <Modal
      title="New app"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="new-app" disabled={create.isPending || withDocker.length === 0}>
            Create and deploy
          </button>
        </>
      }
    >
      {servers.isPending ? (
        <Loading />
      ) : withDocker.length === 0 ? (
        <div className="alert warn">
          No server has Docker. Install it (<code>apt install docker.io</code>, or re-run the installer with{" "}
          <code>--with-docker</code>); the agent detects it when it reconnects.
        </div>
      ) : (
        <form
          id="new-app"
          className="form"
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            create.mutate();
          }}
        >
          <ErrorBox error={create.error} />
          <Field label="Name">
            <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="my-api" required autoFocus />
          </Field>
          <Field label="Image" help="Any public image, e.g. nginx:1.27 or ghcr.io/org/app:v1. Pin a version rather than latest.">
            <input className="input mono" value={image} onChange={(e) => setImage(e.target.value)} placeholder="traefik/whoami:v1.10" required />
          </Field>
          <Field label="HTTP port" help="The port the app listens on inside the container. 0 for background workers.">
            <input className="input mono" type="number" min={0} max={65535} value={port} onChange={(e) => setPort(e.target.value)} />
          </Field>
          <Field label="Domains" help="Optional. Separate several with spaces; point their DNS at the server.">
            <input className="input" value={domains} onChange={(e) => setDomains(e.target.value)} placeholder="api.example.com" />
          </Field>
          <Field label="Server">
            <select className="input" value={serverId || withDocker[0].id} onChange={(e) => setServerId(Number(e.target.value))}>
              {withDocker.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name} · Docker {s.docker} {s.status !== "online" ? `(${s.status})` : ""}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Certificate">
            <select className="input" value={tls} onChange={(e) => setTls(e.target.value)}>
              <option value="auto">Let's Encrypt (public domains)</option>
              <option value="internal">Internal CA (private or test domains)</option>
            </select>
          </Field>
        </form>
      )}
    </Modal>
  );
}
