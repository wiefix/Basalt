import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { api, can, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, formatDate } from "../components/ui";

export function SitesPage({ user }: { user: User }) {
  const sites = useQuery({ queryKey: ["sites"], queryFn: api.sites });
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const [adding, setAdding] = useState(false);
  const serverName = (id: number) => servers.data?.servers.find((s) => s.id === id)?.name ?? `#${id}`;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Sites</h1>
          <div className="sub">Static websites. Upload files and they are live over HTTPS.</div>
        </div>
        {can(user, "admin") && (
          <button className="btn primary" onClick={() => setAdding(true)}>
            <Icon name="plus" /> New site
          </button>
        )}
      </div>
      {sites.isPending ? (
        <Loading />
      ) : sites.error ? (
        <ErrorBox error={sites.error} />
      ) : sites.data.sites.length === 0 ? (
        <div className="card empty">
          <h3>No sites yet</h3>
          <p>Create a site, point its domain at your server, and upload your files.</p>
          {can(user, "admin") && (
            <button className="btn primary" onClick={() => setAdding(true)}>
              <Icon name="plus" /> Create a site
            </button>
          )}
        </div>
      ) : (
        <div className="card table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Site</th>
                <th>Domains</th>
                <th>Server</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {sites.data.sites.map((s) => (
                <tr key={s.id}>
                  <td>
                    <Link to={`/sites/${s.id}`}>
                      <strong>{s.name}</strong>
                    </Link>{" "}
                    <span className="badge plain">{s.php_version ? `PHP ${s.php_version}` : "static"}</span>
                  </td>
                  <td>
                    {s.domains.map((d) => (
                      <div key={d}>
                        <a href={`https://${d}`} target="_blank" rel="noreferrer noopener">
                          {d}
                        </a>
                      </div>
                    ))}
                  </td>
                  <td>
                    <Link to={`/servers/${s.server_id}`} className="muted">
                      {serverName(s.server_id)}
                    </Link>
                  </td>
                  <td className="muted">{formatDate(s.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {adding && <NewSiteModal onClose={() => setAdding(false)} />}
    </>
  );
}

function NewSiteModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const enrolled = servers.data?.servers.filter((s) => s.status !== "pending") ?? [];
  const [serverId, setServerId] = useState<number | "">("");
  const [name, setName] = useState("");
  const [domains, setDomains] = useState("");
  const [tls, setTls] = useState("auto");
  const [php, setPhp] = useState("");
  const server = enrolled.find((s) => s.id === Number(serverId || enrolled[0]?.id));
  const phpVersions = server?.isolated ? server.php_versions : [];
  const create = useMutation({
    mutationFn: () =>
      api.createSite({
        server_id: Number(serverId || enrolled[0]?.id),
        name: name.trim(),
        domains: domains.split(/[\s,]+/).filter(Boolean),
        tls,
        php_version: php,
      }),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["sites"] });
      navigate(`/sites/${r.site.id}`);
    },
  });

  return (
    <Modal
      title="New site"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="new-site" disabled={create.isPending || enrolled.length === 0}>
            Create site
          </button>
        </>
      }
    >
      {servers.isPending ? (
        <Loading />
      ) : enrolled.length === 0 ? (
        <div className="alert warn">Connect a server first: sites run on your servers.</div>
      ) : (
        <form
          id="new-site"
          className="form"
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            create.mutate();
          }}
        >
          <ErrorBox error={create.error} />
          <Field label="Name" help="Used in the panel and as a default SFTP username.">
            <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="my-blog" required autoFocus />
          </Field>
          <Field label="Domains" help="Separate several with spaces. Point their DNS (A/AAAA records) at the server. *.example.com covers every subdomain (its zone must be under DNS).">
            <input className="input" value={domains} onChange={(e) => setDomains(e.target.value)} placeholder="example.com www.example.com" required />
          </Field>
          <Field label="Server">
            <select
              className="input"
              value={serverId || enrolled[0].id}
              onChange={(e) => {
                setServerId(Number(e.target.value));
                setPhp("");
              }}
            >
              {enrolled.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name} {s.status !== "online" ? `(${s.status})` : ""}
                </option>
              ))}
            </select>
          </Field>
          <Field
            label="Type"
            help={
              phpVersions.length === 0
                ? server?.isolated
                  ? "Install php-fpm on the server to host PHP sites."
                  : "PHP needs the agent running as root on Linux (per-site users)."
                : "PHP runs as the site's own system user, isolated from other sites."
            }
          >
            <select className="input" value={php} onChange={(e) => setPhp(e.target.value)}>
              <option value="">Static (HTML, CSS, JS)</option>
              {phpVersions.map((v) => (
                <option key={v} value={v}>
                  PHP {v}
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
