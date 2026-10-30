import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { api, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatBytes, formatDate, useToast } from "../components/ui";
import { ProjectBadge, ProjectFilter, ProjectSelect, ServerLink, useCanCreate, useNewProject, useProjectFilter } from "../components/projects";

export function SitesPage({ user }: { user: User }) {
  const sites = useQuery({ queryKey: ["sites"], queryFn: api.sites });
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const [adding, setAdding] = useState(false);
  const serverName = (id: number) => servers.data?.servers.find((s) => s.id === id)?.name ?? `#${id}`;
  const canCreate = useCanCreate(user);
  const [project, setProject] = useProjectFilter();
  const list = (sites.data?.sites ?? []).filter((s) => !project || s.project_id === project);

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Sites</h1>
          <div className="sub">Static and PHP websites. Upload files and they are live over HTTPS.</div>
        </div>
        <div className="row">
        <ProjectFilter value={project} onChange={setProject} />
        {canCreate && (
          <button className="btn primary" onClick={() => setAdding(true)}>
            <Icon name="plus" /> New site
          </button>
        )}
        </div>
      </div>
      {sites.isPending ? (
        <Loading />
      ) : sites.error ? (
        <ErrorBox error={sites.error} />
      ) : list.length === 0 ? (
        <div className="card empty">
          <h3>No sites yet</h3>
          <p>Create a site, point its domain at your server, and upload your files.</p>
          {canCreate && (
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
                <th className="num hide-sm">Disk</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {list.map((s) => (
                <tr key={s.id}>
                  <td>
                    <Link to={`/sites/${s.id}`}>
                      <strong>{s.name}</strong>
                    </Link>{" "}
                    <span className="badge plain">{s.php_version ? `PHP ${s.php_version}` : "static"}</span>
                    <ProjectBadge id={s.project_id} />
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
                    <ServerLink id={s.server_id} name={serverName(s.server_id)} user={user} className="muted" />
                  </td>
                  <td className="num muted hide-sm">
                    {s.usage?.at ? formatBytes(s.usage.disk_bytes + s.usage.db_bytes) : "—"}
                    {s.over_quota && (
                      <span className="badge err" style={{ marginLeft: 6 }}>
                        over quota
                      </span>
                    )}
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
  const [projectId, setProjectId] = useNewProject();
  const server = enrolled.find((s) => s.id === Number(serverId || enrolled[0]?.id));
  const phpVersions = server?.isolated ? server.php_versions : [];
  const canApps = phpVersions.length > 0 && !!server?.mariadb;
  const toast = useToast();
  const create = useMutation({
    mutationFn: async () => {
      const app = php === "wordpress" || php === "laravel" || php === "symfony" ? php : null;
      const r = await api.createSite({
        server_id: Number(serverId || enrolled[0]?.id),
        name: name.trim(),
        domains: domains.split(/[\s,]+/).filter(Boolean),
        tls,
        php_version: app ? phpVersions[0] : php,
        project_id: projectId,
      });
      if (app) {
        const name = app === "wordpress" ? "WordPress" : app === "laravel" ? "Laravel" : "Symfony";
        try {
          const w = await api.installSite(r.site.id, app);
          toast(
            app === "wordpress"
              ? `WordPress ${w.version} installed: open the site now to choose its admin account`
              : app === "laravel"
                ? `Laravel ${w.version} installed, with its tables in database ${w.database.name}`
                : `Symfony ${w.version} installed; DATABASE_URL in .env.local points at database ${w.database.name}`,
          );
        } catch (e) {
          toast(`The site was created, but ${name} was not installed: ${errorMessage(e)}`, true);
        }
      }
      return r;
    },
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
          <ProjectSelect value={projectId} onChange={setProjectId} />
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
              {canApps && <option value="wordpress">WordPress (PHP {phpVersions[0]}, MariaDB)</option>}
              {canApps && <option value="laravel">Laravel (PHP {phpVersions[0]}, MariaDB)</option>}
              {canApps && <option value="symfony">Symfony (PHP {phpVersions[0]}, MariaDB)</option>}
            </select>
          </Field>
          {php === "wordpress" && (
            <div className="alert warn small">
              WordPress is downloaded from wordpress.org with a new database. Open the site right after: whoever opens it
              first chooses the admin account.
            </div>
          )}
          {php === "laravel" && (
            <div className="alert small">
              A new Laravel project is created with Composer (from Packagist) as the site's user, with a new database and
              its tables. It takes a minute or two. The project sits next to <code>public/</code>; edit it over SFTP or in
              Files.
            </div>
          )}
          {php === "symfony" && (
            <div className="alert small">
              A new Symfony project (the skeleton, set up by Symfony Flex) is created with Composer as the site's user, with a new
              database: <code>DATABASE_URL</code> in <code>.env.local</code> points at it, for Doctrine when you add it. The project
              sits next to <code>public/</code>.
            </div>
          )}
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
