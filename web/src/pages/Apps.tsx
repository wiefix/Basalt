import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { api, sourceLabel, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, formatDate } from "../components/ui";
import { ProjectBadge, ProjectFilter, ProjectSelect, ServerLink, useCanCreate, useNewProject, useProjectFilter } from "../components/projects";

export function AppsPage({ user }: { user: User }) {
  const apps = useQuery({ queryKey: ["apps"], queryFn: api.apps });
  // Databases have their own page.
  const [project, setProject] = useProjectFilter();
  const canCreate = useCanCreate(user);
  const list = apps.data?.apps.filter((a) => a.source !== "database" && a.source !== "mail" && (!project || a.project_id === project)) ?? [];
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const [adding, setAdding] = useState(false);
  const serverName = (id: number) => servers.data?.servers.find((s) => s.id === id)?.name ?? `#${id}`;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Apps</h1>
          <div className="sub">Containers from images, git repositories or compose files, with zero-downtime updates.</div>
        </div>
        <div className="row">
          <ProjectFilter value={project} onChange={setProject} />
          {canCreate && (
            <button className="btn primary" onClick={() => setAdding(true)}>
              <Icon name="plus" /> New app
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
          <h3>No apps yet</h3>
          <p>Run any Docker image on one of your servers and put it behind HTTPS.</p>
          {canCreate && (
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
                <th>Source</th>
                <th>Domains</th>
                <th>Server</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {list.map((a) => (
                <tr key={a.id}>
                  <td>
                    <Link to={`/apps/${a.id}`}>
                      <strong>{a.name}</strong>
                    </Link>
                    <ProjectBadge id={a.project_id} />
                  </td>
                  <td className="mono muted">
                    {sourceLabel(a)}
                    {a.template_update && <span className="badge warn" style={{ marginLeft: 8 }}>update: {a.template_update}</span>}
                  </td>
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
                    <ServerLink id={a.server_id} name={serverName(a.server_id)} user={user} className="muted" />
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

// SSH repositories need the app's deploy key added before the first build.
const isSSH = (url: string) => !url.trim().startsWith("https://");

function NewAppModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const withDocker = servers.data?.servers.filter((s) => s.docker) ?? [];
  const [serverId, setServerId] = useState<number | "">("");
  const [source, setSource] = useState<"image" | "git" | "compose">("image");
  const [compose, setCompose] = useState("");
  const [service, setService] = useState("");
  const [name, setName] = useState("");
  const [image, setImage] = useState("");
  const [repoURL, setRepoURL] = useState("");
  const [branch, setBranch] = useState("main");
  const [dockerfile, setDockerfile] = useState("Dockerfile");
  const [port, setPort] = useState("80");
  const [warnings, setWarnings] = useState<string[] | null>(null);
  const [createdId, setCreatedId] = useState<number | null>(null);
  const [domains, setDomains] = useState("");
  const [tls, setTls] = useState("auto");
  const [projectId, setProjectId] = useNewProject();
  // Repositories through a connected GitHub App (0: any URL).
  const sources = useQuery({ queryKey: ["git-sources"], queryFn: api.gitSources, enabled: source === "git", retry: false });
  const ghSources = sources.data?.sources ?? [];
  const [ghSource, setGhSource] = useState<number | null>(null);
  const gh = ghSource ?? ghSources[0]?.id ?? 0;
  const [repo, setRepo] = useState("");
  const repos = useQuery({ queryKey: ["git-source-repos", gh], queryFn: () => api.gitSourceRepos(gh), enabled: source === "git" && gh > 0 });
  const branches = useQuery({
    queryKey: ["git-source-branches", gh, repo],
    queryFn: () => api.gitSourceBranches(gh, repo),
    enabled: source === "git" && gh > 0 && repo !== "",
  });
  const create = useMutation({
    mutationFn: async () => {
      const r = await api.createApp({
        server_id: Number(serverId || withDocker[0]?.id),
        name: name.trim(),
        source,
        image: source === "image" ? image.trim() : "",
        git:
          source !== "git"
            ? undefined
            : gh > 0
              ? { url: "", source_id: gh, repo, branch: branch.trim(), dockerfile: dockerfile.trim() }
              : { url: repoURL.trim(), branch: branch.trim(), dockerfile: dockerfile.trim() },
        compose: source === "compose" ? compose : undefined,
        service: source === "compose" ? service.trim() : undefined,
        // A compose app's port defaults to the one its file mentions for the service.
        port: source === "compose" && !service.trim() ? 0 : Number(port) || 0,
        domains: domains.split(/[\s,]+/).filter(Boolean),
        tls,
        volumes: [],
        memory_mb: 0,
        cpus: 0,
        project_id: projectId,
      });
      const waitForKey = source === "git" && gh === 0 && isSSH(repoURL);
      // Compose files usually need variables first: they are deployed from the app page.
      const deployNow = !waitForKey && source !== "compose";
      if (deployNow) await api.deployApp(r.app.id).catch(() => {});
      return { ...r, waitForKey };
    },
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["apps"] });
      if (r.warnings.length > 0) {
        // Shown once here; the Compose tab lists them again.
        setWarnings(r.warnings);
        setCreatedId(r.app.id);
        return;
      }
      navigate(r.waitForKey ? `/apps/${r.app.id}/source` : source === "compose" ? `/apps/${r.app.id}/env` : `/apps/${r.app.id}`);
    },
  });

  if (warnings && createdId !== null) {
    return (
      <Modal
        title="App created"
        onClose={onClose}
        footer={
          <button className="btn primary" onClick={() => navigate(`/apps/${createdId}/env`)}>
            Continue
          </button>
        }
      >
        <div className="stack">
          <p className="muted" style={{ margin: 0 }}>
            The compose file is valid. Before deploying, check these points; set the variables it uses under Environment.
          </p>
          <ul className="steps small">
            {warnings.map((w) => (
              <li key={w}>{w}</li>
            ))}
          </ul>
        </div>
      </Modal>
    );
  }

  return (
    <Modal
      title="New app"
      onClose={onClose}
      wide={source === "compose"}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="new-app" disabled={create.isPending || withDocker.length === 0}>
            {(source === "git" && gh === 0 && isSSH(repoURL) && repoURL) || source === "compose" ? "Create" : "Create and deploy"}
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
          <ProjectSelect value={projectId} onChange={setProjectId} />
          <Field label="Name">
            <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="my-api" required autoFocus />
          </Field>
          <div className="segmented">
            <button type="button" className={source === "image" ? "active" : ""} onClick={() => setSource("image")}>
              Docker image
            </button>
            <button type="button" className={source === "git" ? "active" : ""} onClick={() => setSource("git")}>
              Git repository
            </button>
            <button type="button" className={source === "compose" ? "active" : ""} onClick={() => setSource("compose")}>
              Docker Compose
            </button>
          </div>
          {source === "compose" ? (
            <>
              <Field
                label="Compose file"
                help={
                  <>
                    Paste a docker-compose.yml. Services run ready images on a private network; named volumes keep
                    their data. Use <code>${"{"}NAME{"}"}</code> for secrets and set them under Environment.
                  </>
                }
              >
                <textarea
                  className="input mono"
                  rows={14}
                  value={compose}
                  onChange={(e) => setCompose(e.target.value)}
                  spellCheck={false}
                  required
                  placeholder={"services:\n  web:\n    image: ghost:5\n    ports: [\"2368\"]\n    volumes: [content:/var/lib/ghost/content]\nvolumes:\n  content:"}
                />
              </Field>
              <div className="grid-2">
                <Field label="Published service" help="Its port gets the app's domains. Empty: the first service with a port.">
                  <input className="input mono" value={service} onChange={(e) => setService(e.target.value)} placeholder="web" />
                </Field>
                <Field label="Port" help="Inside the container; empty uses the one in the file.">
                  <input
                    className="input mono"
                    type="number"
                    min={0}
                    max={65535}
                    value={service.trim() ? port : ""}
                    disabled={!service.trim()}
                    onChange={(e) => setPort(e.target.value)}
                  />
                </Field>
              </div>
            </>
          ) : source === "image" ? (
            <Field
              label="Image"
              help={
                <>
                  e.g. nginx:1.27 or ghcr.io/org/app:v1. Pin a version rather than latest. Private images need credentials
                  under <Link to="/registries">Registries</Link>.
                </>
              }
            >
              <input className="input mono" value={image} onChange={(e) => setImage(e.target.value)} placeholder="traefik/whoami:v1.10" required />
            </Field>
          ) : (
            <>
              {ghSources.length > 0 && (
                <Field label="From">
                  <select className="input" value={gh} onChange={(e) => { setGhSource(Number(e.target.value)); setRepo(""); }}>
                    {ghSources.map((g) => (
                      <option key={g.id} value={g.id}>
                        GitHub: {g.name} ({g.owner})
                      </option>
                    ))}
                    <option value={0}>Any repository URL (deploy key)</option>
                  </select>
                </Field>
              )}
              {gh > 0 ? (
                <Field
                  label="Repository"
                  help={
                    <>
                      Repositories the GitHub App may read; add more under <Link to="/git-sources">Git sources</Link>. Pushes to
                      the branch deploy automatically.
                    </>
                  }
                >
                  {repos.isPending ? (
                    <Loading />
                  ) : repos.error ? (
                    <ErrorBox error={repos.error} />
                  ) : (
                    <select
                      className="input mono"
                      value={repo}
                      required
                      onChange={(e) => {
                        setRepo(e.target.value);
                        const r = repos.data.repos.find((x) => x.full_name === e.target.value);
                        if (r) setBranch(r.default_branch);
                      }}
                    >
                      <option value="">Choose a repository…</option>
                      {repos.data.repos.map((r) => (
                        <option key={r.full_name} value={r.full_name}>
                          {r.full_name}
                          {r.private ? " (private)" : ""}
                        </option>
                      ))}
                    </select>
                  )}
                </Field>
              ) : (
                <Field
                  label="Repository"
                  help={
                    ghSources.length === 0 ? (
                      <>
                        https://… for public repositories; git@host:org/repo.git for private ones (you add the app's deploy key
                        next). Or <Link to="/git-sources">connect GitHub</Link> to pick from your repositories.
                      </>
                    ) : (
                      "https://… for public repositories; git@host:org/repo.git for private ones (you add the app's deploy key next)."
                    )
                  }
                >
                  <input className="input mono" value={repoURL} onChange={(e) => setRepoURL(e.target.value)} placeholder="git@github.com:org/app.git" required />
                </Field>
              )}
              <div className="grid-2">
                <Field label="Branch">
                  {gh > 0 && branches.data ? (
                    <select className="input mono" value={branch} onChange={(e) => setBranch(e.target.value)}>
                      {!branches.data.branches.includes(branch) && <option value={branch}>{branch}</option>}
                      {branches.data.branches.map((b) => (
                        <option key={b} value={b}>
                          {b}
                        </option>
                      ))}
                    </select>
                  ) : (
                    <input className="input mono" value={branch} onChange={(e) => setBranch(e.target.value)} required />
                  )}
                </Field>
                <Field label="Dockerfile" help="Path in the repository.">
                  <input className="input mono" value={dockerfile} onChange={(e) => setDockerfile(e.target.value)} required />
                </Field>
              </div>
            </>
          )}
          {source !== "compose" && (
            <Field label="HTTP port" help="The port the app listens on inside the container. 0 for background workers.">
              <input className="input mono" type="number" min={0} max={65535} value={port} onChange={(e) => setPort(e.target.value)} />
            </Field>
          )}
          <Field label="Domains" help="Optional. Separate several with spaces; point their DNS at the server. *.example.com covers every subdomain (its zone must be under DNS).">
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
