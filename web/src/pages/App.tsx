import { useEffect, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, NavLink, Route, Routes, useNavigate, useParams } from "react-router";
import {
  api,
  appPath,
  can,
  isCompose,
  sourceLabel,
  type App,
  type AppPlacement,
  type AppVolume,
  type Deployment,
  type ServiceStatus,
  type User,
} from "../api";
import {
  CodeBox,
  ErrorBox,
  Field,
  Icon,
  Loading,
  Modal,
  errorMessage,
  formatBytes,
  formatDate,
  timeAgo,
  useConfirm,
  useToast,
} from "../components/ui";
import { useEventStream, type StreamState } from "../components/stream";
import { Stat, SyncBadge } from "./Server";
import { Chart, RangePicker, rangeRefresh, rangeTitle } from "../components/Chart";
import { BackupsTab } from "./BackupsTab";
import { LogView } from "../components/LogView";
import { AccessLogCard } from "../components/AccessLog";
import { TemplateCard } from "./AppTemplate";
import { MoveProjectCard, ProjectBadge, ServerLink, useAs } from "../components/projects";

export function AppPage({ user: me }: { user: User }) {
  const id = Number(useParams().id);
  const app = useQuery({ queryKey: ["app", id], queryFn: () => api.app(id) });
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  // What the user may do here: their role in the app's project (the mail
  // server is the panel's).
  const asProject = useAs(me, app.data?.app.project_id);
  const user = app.data?.app.source === "mail" ? me : asProject;

  if (app.isPending) return <Loading />;
  if (app.error) return <ErrorBox error={app.error} />;
  const a = app.data.app;
  const server = servers.data?.servers.find((x) => x.id === a.server_id);
  const base = appPath(a);
  const isDB = a.source === "database";
  const isMail = a.source === "mail";
  const tab = (to: string, label: string) => (
    <NavLink to={to} end className={({ isActive }) => (isActive ? "tab active" : "tab")}>
      {label}
    </NavLink>
  );

  return (
    <>
      <div className="page-head">
        <div>
          <div className="row">
            <h1>{a.name}</h1>
            {!isMail && <ProjectBadge id={a.project_id} />}
            {!isDB && <SyncBadge sync={app.data.sync} />}
          </div>
          <div className="sub row wrap">
            <span className="mono">{sourceLabel(a)}</span>
            {a.domains.map((d) => (
              <a key={d} href={`https://${d}`} target="_blank" rel="noreferrer noopener">
                {d} ↗
              </a>
            ))}
            <span className="faint">
              on <ServerLink id={a.server_id} name={server?.name ?? `server #${a.server_id}`} user={me} />
            </span>
          </div>
        </div>
      </div>
      {a.preview_of ? <PreviewBanner app={a} /> : null}
      {server && server.status !== "online" && (
        <div className="alert warn" style={{ marginBottom: 16 }}>
          The server is {server.status}. The {isDB ? "database" : "app"} keeps running; it cannot be deployed or inspected
          until the agent reconnects.
        </div>
      )}
      <div className="tabs">
        {tab(base, "Overview")}
        {can(user, "member") && tab(`${base}/logs`, "Logs")}
        {isDB && can(user, "admin") && tab(`${base}/connection`, "Connection")}
        {!isDB && !isMail && can(user, "admin") && tab(`${base}/env`, "Environment")}
        {!isDB && !isMail && tab(`${base}/domains`, "Domains")}
        {a.source === "git" && can(user, "admin") && tab(`${base}/source`, "Source")}
        {isCompose(a) && can(user, "admin") && tab(`${base}/compose`, "Compose")}
        {tab(`${base}/backups`, "Backups")}
        {can(user, "admin") && tab(`${base}/settings`, "Settings")}
      </div>
      <Routes>
        <Route index element={<Overview app={a} user={user} me={me} placements={app.data.placements ?? []} />} />
        <Route path="connection" element={<ConnectionTab app={a} />} />
        <Route path="backups" element={<BackupsTab target={{ base: `/api/v1/apps/${a.id}`, name: a.name }} user={user} />} />
        <Route path="logs" element={<LogsTab app={a} />} />
        <Route path="env" element={<EnvTab app={a} />} />
        <Route path="domains" element={<DomainsTab app={a} user={user} />} />
        <Route path="source" element={<SourceTab app={a} />} />
        <Route path="compose" element={<ComposeTab app={a} />} />
        <Route path="settings" element={<SettingsTab app={a} panelAdmin={can(me, "admin")} />} />
      </Routes>
    </>
  );
}

function DeployBadge({ status }: { status: Deployment["status"] }) {
  const cls = status === "succeeded" ? "ok" : status === "running" ? "warn" : "err";
  return <span className={`badge ${cls}`}>{status === "running" ? "deploying" : status}</span>;
}

function Overview({ app, user, me, placements }: { app: App; user: User; me: User; placements: AppPlacement[] }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const [shown, setShown] = useState<number | null>(null);
  const deployments = useQuery({
    queryKey: ["deployments", app.id],
    queryFn: () => api.deployments(app.id),
    refetchInterval: 15_000,
  });
  const runningDep = deployments.data?.deployments.find((d) => d.status === "running");
  const running = !!runningDep;
  const status = useQuery({
    queryKey: ["app-status", app.id],
    queryFn: () => api.appStatus(app.id),
    refetchInterval: 10_000,
    retry: false,
  });
  // The running deployment's stream tells when it ends; no polling needed.
  useEventStream(runningDep ? api.deploymentStreamUrl(app.id, runningDep.id) : null, {
    end: () => {
      qc.invalidateQueries({ queryKey: ["deployments", app.id] });
      qc.invalidateQueries({ queryKey: ["app-status", app.id] });
    },
  });
  const deploy = useMutation({
    mutationFn: (rollbackTo?: number) => api.deployApp(app.id, rollbackTo),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["deployments", app.id] });
      setShown(r.deployment.id);
    },
    onError: (e) => toast(errorMessage(e), true),
  });
  const st = status.data?.status;
  const current = st?.deployment_id;

  return (
    <div className="stack">
      <div className="card card-body">
        <div className="row wrap" style={{ justifyContent: "space-between" }}>
          <div className="row wrap">
            <h2>Container</h2>
            {status.isPending ? (
              <span className="spinner" />
            ) : status.error ? (
              <span className="badge err" title={errorMessage(status.error)}>
                unknown
              </span>
            ) : (
              <span className={`badge ${st?.state === "running" ? "ok" : st?.state === "missing" ? "" : "err"}`}>
                {st?.state === "missing" ? "not deployed" : st?.state}
              </span>
            )}
            {st?.health && <span className={`badge ${st.health === "healthy" ? "ok" : "warn"}`}>{st.health}</span>}
            {st?.started_at && st.state === "running" && <span className="faint small">up since {timeAgo(st.started_at)}</span>}
            {!!st?.restart_count && <span className="badge warn">{st.restart_count} restarts</span>}
            {st?.oom_killed && <span className="badge err">out of memory</span>}
          </div>
          {can(user, "member") && (
            <div className="row">
              {runningDep && <CancelButton appId={app.id} deploymentId={runningDep.id} />}
              <button className="btn primary" onClick={() => deploy.mutate(undefined)} disabled={deploy.isPending || running}>
                <Icon name="rocket" /> {running ? "Deploying…" : "Deploy"}
              </button>
            </div>
          )}
        </div>
        <p className="faint small" style={{ marginBottom: 0 }}>
          {app.source === "mail" ? (
            <>
              A deployment pulls <span className="mono">{app.image}</span> and, if it changed (or the limits did),
              restarts the mail server on it: it is stopped first, so incoming mail waits a few seconds (senders
              retry). Mail and settings are kept. If the new container fails, the previous one is started again.
            </>
          ) : app.source === "database" ? (
            <>
              A deployment pulls the latest <span className="mono">{app.image}</span> and, if it changed (or the limits did),
              restarts the database on it: it is stopped first, so its data is never used twice. Without changes it keeps
              running. If the new container fails, the previous one is started again.
            </>
          ) : app.source === "compose" ? (
            <>
              A deployment pulls the images of the compose file and starts the services in dependency order. Unchanged
              services keep running; a changed service without volumes gets its new container before the old one stops,
              one with volumes is restarted. If any service fails, the previous containers are restored.
            </>
          ) : app.source === "git" && app.git?.builder === "compose" ? (
            <>
              A deployment reads <span className="mono">{app.git.compose_file}</span> at the head of branch{" "}
              <span className="mono">{app.git.branch}</span>
              {app.auto_deploy ? " (also on every push, through the webhook)" : ""}, builds the services that build from the
              repository, pulls the others' images and starts the services in dependency order. Unchanged services keep
              running; a changed service without volumes gets its new container before the old one stops, one with volumes
              is restarted. If any service fails, the previous containers are restored.
            </>
          ) : app.source === "git" ? (
            <>
              A deployment builds branch <span className="mono">{app.git?.branch}</span>{" "}
              {app.git?.builder === "railpack" ? "(worked out by Railpack)" : <>with <span className="mono">{app.git?.dockerfile}</span></>}
              {app.auto_deploy ? " (also on every push, through the webhook)" : ""},
            </>
          ) : (
            <>
              A deployment pulls <span className="mono">{app.image}</span>,
            </>
          )}{" "}
          {!isCompose(app) && app.source !== "database" && app.source !== "mail" &&
            " starts a new container and switches traffic once it is ready. If the new container fails, the current one keeps serving."}
        </p>
      </div>

      {app.template && <TemplateCard app={app} user={user} />}
      {app.source === "git" && !!app.git?.source_id && !app.preview_of && (
        <PreviewsCard app={app} user={user} />
      )}
      {(app.source === "image" || app.source === "git" || app.source === "compose") && !app.preview_of && (
        <ServersCard app={app} user={user} me={me} placements={placements} />
      )}
      {isCompose(app) && <ServicesCard app={app} services={st?.services} />}
      <ResourcesCard app={app} running={st?.state === "running"} />
      {app.source === "database" ? <LinksCard app={app} user={user} /> : app.source !== "mail" && <LinkedDatabases app={app} />}

      <div className="card">
        <div className="card-head">
          <h2>Deployments</h2>
        </div>
        {deployments.isPending ? (
          <Loading />
        ) : deployments.error ? (
          <div className="card-body">
            <ErrorBox error={deployments.error} />
          </div>
        ) : deployments.data.deployments.length === 0 ? (
          <div className="empty small">Not deployed yet.</div>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>#</th>
                  <th>Status</th>
                  <th>{app.source === "git" ? "Commit" : app.source === "compose" ? "Services" : "Image"}</th>
                  <th>Started</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {deployments.data.deployments.map((d) => (
                  <tr key={d.id} className="clickable" onClick={() => setShown(d.id)}>
                    <td className="mono">
                      {d.id}
                      {d.id === current && st?.state === "running" && (
                        <span className="badge ok plain" style={{ marginLeft: 6 }}>
                          live
                        </span>
                      )}
                    </td>
                    <td>
                      <DeployBadge status={d.status} />
                    </td>
                    {app.source === "database" || app.source === "mail" ? (
                      <td className="mono muted small" title={d.digests?.[app.source === "mail" ? "stalwart" : app.name]}>
                        {d.digests?.[app.source === "mail" ? "stalwart" : app.name]?.split("@")[1]?.slice(0, 19) ?? "—"}
                      </td>
                    ) : app.source === "compose" ? (
                      <td className="muted small">{d.digests ? Object.keys(d.digests).sort().join(", ") : "—"}</td>
                    ) : app.source === "git" ? (
                      <td className="muted small" title={d.commit}>
                        {d.commit ? (
                          <>
                            <span className="mono">{d.commit.slice(0, 8)}</span> {d.commit_message}
                          </>
                        ) : (
                          "—"
                        )}
                      </td>
                    ) : (
                      <td className="mono muted small" title={d.image_digest}>
                        {d.image}
                      </td>
                    )}
                    <td className="muted">{formatDate(d.created_at)}</td>
                    <td className="num" onClick={(e) => e.stopPropagation()}>
                      {can(user, "member") && d.status === "succeeded" && d.id !== current && (
                        <button
                          className="btn sm"
                          disabled={running}
                          onClick={async () => {
                            const ok = await confirm({
                              title: `Roll back to #${d.id}?`,
                              body:
                                app.source === "database" || app.source === "mail" ? (
                                  <p>
                                    The image of deployment #{d.id} runs again on the current data. Going back to an older
                                    minor version is usually fine; check the engine's notes if unsure.
                                  </p>
                                ) : isCompose(app) ? (
                                  <p>The compose file and images of deployment #{d.id} are deployed again with the app's current environment.</p>
                                ) : (
                                  <p>The image of deployment #{d.id} is deployed again with the app's current settings and environment.</p>
                                ),
                              confirm: "Roll back",
                            });
                            if (ok) deploy.mutate(d.id);
                          }}
                        >
                          Roll back
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
      {shown !== null && <DeploymentModal appId={app.id} id={shown} canCancel={can(user, "member")} onClose={() => setShown(null)} />}
    </div>
  );
}

// CancelButton stops a running deployment: the server undoes what it
// started and the previous deployment keeps running.
function CancelButton({ appId, deploymentId }: { appId: number; deploymentId: number }) {
  const toast = useToast();
  const confirm = useConfirm();
  const [sent, setSent] = useState(false);
  const cancel = async () => {
    const ok = await confirm({
      title: `Cancel deployment #${deploymentId}?`,
      body: <p>What it started is removed and the current version keeps running. Once traffic has switched to the new version, the deployment completes anyway.</p>,
      confirm: "Cancel deployment",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.cancelDeployment(appId, deploymentId);
      setSent(true);
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  return (
    <button className="btn danger" onClick={cancel} disabled={sent}>
      {sent ? "Canceling…" : "Cancel"}
    </button>
  );
}

function DeploymentModal({ appId, id, canCancel, onClose }: { appId: number; id: number; canCancel: boolean; onClose: () => void }) {
  const qc = useQueryClient();
  const d = useQuery({ queryKey: ["deployment", appId, id], queryFn: () => api.deployment(appId, id) });
  const dep = d.data?.deployment;
  const following = dep?.status === "running";
  const [live, setLive] = useState("");
  useEventStream(following ? api.deploymentStreamUrl(appId, id) : null, {
    open: () => setLive(""), // the stream starts from the beginning of the log
    log: (c: { text: string }) => setLive((t) => t + c.text),
    end: () => {
      qc.invalidateQueries({ queryKey: ["deployment", appId, id] });
      qc.invalidateQueries({ queryKey: ["deployments", appId] });
      qc.invalidateQueries({ queryKey: ["app-status", appId] });
    },
  });
  const text = following ? live : dep?.log;
  return (
    <Modal
      wide
      onClose={onClose}
      title={
        <span className="row">
          Deployment #{id} {dep && <DeployBadge status={dep.status} />}
          {following && canCancel && <CancelButton appId={appId} deploymentId={id} />}
        </span>
      }
    >
      {d.isPending ? (
        <Loading />
      ) : d.error ? (
        <ErrorBox error={d.error} />
      ) : (
        <div className="stack">
          {dep?.error && <div className="alert err small">{dep.error}</div>}
          <LogView text={text || (following ? "Waiting for the server…" : "(no output)")} />
          {dep?.image_digest && <div className="faint small mono">{dep.image_digest}</div>}
          {dep?.digests && (
            <dl className="kv small">
              {Object.keys(dep.digests)
                .sort()
                .map((k) => (
                  <div key={k} style={{ display: "contents" }}>
                    <dt>{k}</dt>
                    <dd className="mono faint">{dep.digests![k]}</dd>
                  </div>
                ))}
            </dl>
          )}
        </div>
      )}
    </Modal>
  );
}

const MAX_LOG_CHARS = 1_000_000;

// Drop the oldest lines once the view holds too much text.
function trimLog(t: string): string {
  if (t.length <= MAX_LOG_CHARS) return t;
  const cut = t.indexOf("\n", t.length - MAX_LOG_CHARS * 0.8);
  return cut < 0 ? t.slice(-MAX_LOG_CHARS) : t.slice(cut + 1);
}

function StreamBadge({ state, ended }: { state: StreamState; ended: string | null }) {
  if (ended !== null) return <span className="badge err" title={ended}>stopped</span>;
  if (state === "open") return <span className="badge ok">live</span>;
  if (state === "failed") return <span className="badge err">unavailable</span>;
  return <span className="badge warn">connecting</span>;
}

function LogsTab({ app }: { app: App }) {
  const services = app.compose?.services ?? [];
  const [service, setService] = useState(app.service || services[0]?.name || "");
  const [server, setServer] = useState(0); // 0: the primary server
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers, enabled: app.replicas.length > 0 });
  const serverName = (id: number) => servers.data?.servers.find((s) => s.id === id)?.name ?? `server #${id}`;
  const [tail, setTail] = useState(200);
  const [live, setLive] = useState(true);
  const [text, setText] = useState("");
  const [ended, setEnded] = useState<string | null>(null);
  const snapshot = useQuery({
    queryKey: ["app-logs", app.id, tail, service, server],
    queryFn: () => api.appLogs(app.id, tail, service, server),
    enabled: !live,
    retry: false,
  });
  const state = useEventStream(live ? api.appLogsStreamUrl(app.id, tail, service, server) : null, {
    open: () => {
      setText(""); // a (re)connected stream starts with the last lines again
      setEnded(null);
    },
    log: (c: { text: string }) => setText((t) => trimLog(t + c.text)),
    end: (d: { error?: string } | null) => setEnded(d?.error || "the stream ended"),
  });
  return (
    <div className="stack">
      <div className="card">
        <div className="card-head">
          <div className="row">
            <h2>Container output</h2>
            {live && <StreamBadge state={state} ended={ended} />}
          </div>
          <div className="row">
            {app.replicas.length > 0 && (
              <select className="input" style={{ width: "auto", height: 28 }} value={server} onChange={(e) => setServer(Number(e.target.value))}>
                <option value={0}>{serverName(app.server_id)}</option>
                {app.replicas.map((id) => (
                  <option key={id} value={id}>
                    {serverName(id)}
                  </option>
                ))}
              </select>
            )}
            {services.length > 0 && (
              <select className="input" style={{ width: "auto", height: 28 }} value={service} onChange={(e) => setService(e.target.value)}>
                {services.map((s) => (
                  <option key={s.name} value={s.name}>
                    {s.name}
                  </option>
                ))}
              </select>
            )}
            <select className="input" style={{ width: "auto", height: 28 }} value={tail} onChange={(e) => setTail(Number(e.target.value))}>
              {[100, 200, 1000, 5000].map((n) => (
                <option key={n} value={n}>
                  last {n} lines
                </option>
              ))}
            </select>
            <label className="check small">
              <input type="checkbox" checked={live} onChange={(e) => setLive(e.target.checked)} /> live
            </label>
            {!live && (
              <button className="btn sm icon ghost" title="Refresh" onClick={() => snapshot.refetch()}>
                <Icon name="refresh" size={14} />
              </button>
            )}
          </div>
        </div>
        <div className="card-body">
          {live ? (
            <>
              {ended && <div className="alert err small" style={{ marginBottom: 12 }}>{ended}</div>}
              <LogView tall text={text || (state === "open" ? "(no output yet)" : "Connecting…")} />
            </>
          ) : snapshot.isPending ? (
            <Loading />
          ) : snapshot.error ? (
            <ErrorBox error={snapshot.error} />
          ) : (
            <LogView tall text={snapshot.data.log || "(no output yet)"} />
          )}
        </div>
      </div>
      {app.domains.length > 0 && <AccessLogCard base={`/api/v1/apps/${app.id}`} />}
    </div>
  );
}

// The editor uses .env syntax: KEY=value per line, # comments.
function parseEnv(text: string): { env: Record<string, string>; error?: string } {
  const env: Record<string, string> = {};
  const lines = text.split("\n");
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i].trim();
    if (!line || line.startsWith("#")) continue;
    const eq = line.indexOf("=");
    if (eq <= 0) return { env, error: `Line ${i + 1}: expected KEY=value` };
    const key = line.slice(0, eq).trim().replace(/^export\s+/, "");
    let value = line.slice(eq + 1).trim();
    if (value.length >= 2 && (value[0] === '"' || value[0] === "'") && value.endsWith(value[0])) value = value.slice(1, -1);
    env[key] = value;
  }
  return { env };
}

const formatEnv = (env: Record<string, string>) =>
  Object.keys(env)
    .sort()
    .map((k) => `${k}=${env[k]}`)
    .join("\n");

function EnvTab({ app }: { app: App }) {
  const toast = useToast();
  const env = useQuery({ queryKey: ["app-env", app.id], queryFn: () => api.appEnv(app.id), gcTime: 0 });
  const [text, setText] = useState<string | null>(null);
  useEffect(() => {
    if (env.data && text === null) setText(formatEnv(env.data.env));
  }, [env.data, text]);
  const parsed = parseEnv(text ?? "");
  const save = useMutation({
    mutationFn: () => api.setAppEnv(app.id, parsed.env),
    onSuccess: () => toast("Saved. Deploy to apply the new environment."),
  });
  if (env.isPending) return <Loading />;
  if (env.error) return <ErrorBox error={env.error} />;
  return (
    <form
      className="card card-body form"
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <h2>Environment variables</h2>
      <ErrorBox error={save.error} />
      {parsed.error && <div className="alert err small">{parsed.error}</div>}
      <Field
        label="One per line, KEY=value"
        help={
          isCompose(app) ? (
            <>
              Stored encrypted. The compose file uses them as <code>${"{"}NAME{"}"}</code>; containers get only what the file
              passes them. Changes take effect on the next deployment.
            </>
          ) : (
            "Stored encrypted. Changes take effect on the next deployment."
          )
        }
      >
        <textarea
          className="input mono"
          rows={14}
          value={text ?? ""}
          onChange={(e) => setText(e.target.value)}
          spellCheck={false}
          placeholder={"DATABASE_URL=postgres://…\nSECRET_KEY=…"}
        />
      </Field>
      <div>
        <button className="btn primary" disabled={save.isPending || !!parsed.error}>
          Save
        </button>
      </div>
    </form>
  );
}

function DomainsTab({ app, user }: { app: App; user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [text, setText] = useState(app.domains.join("\n"));
  const save = useMutation({
    mutationFn: () => api.setAppDomains(app.id, text.split(/[\s,]+/).filter(Boolean)),
    onSuccess: (r) => {
      qc.setQueryData(["app", app.id], { app: r.app, sync: r.sync });
      qc.invalidateQueries({ queryKey: ["apps"] });
      toast(r.sync.in_sync ? "Domains updated" : "Saved; the server will apply it when it reconnects");
    },
  });
  return (
    <div className="grid-2">
      <form
        className="card card-body form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <h2>Domains</h2>
        <ErrorBox error={save.error} />
        <Field
          label="One per line"
          help={
            isCompose(app)
              ? app.service
                ? `Requests are proxied to port ${app.port} of service ${app.service}.`
                : "Choose the published service under Compose first."
              : app.port
                ? `Requests are proxied to port ${app.port} of the container.`
                : "Set an HTTP port in Settings first."
          }
        >
          <textarea className="input mono" rows={6} value={text} onChange={(e) => setText(e.target.value)} disabled={!can(user, "admin")} />
        </Field>
        {can(user, "admin") && (
          <div>
            <button className="btn primary" disabled={save.isPending}>
              Save domains
            </button>
          </div>
        )}
      </form>
      {isCompose(app) && <ServiceRoutesCard app={app} user={user} />}
      <div className="card card-body stack">
        <h2>How traffic reaches the app</h2>
        <p className="muted" style={{ margin: 0 }}>
          The server's proxy terminates HTTPS and forwards requests to the app's container over a private Docker network.
          No container port is published on the server.
        </p>
        <p className="faint small" style={{ margin: 0 }}>
          Certificate: {app.tls === "auto" ? "Let's Encrypt (falls back to ZeroSSL)" : "internal CA (browsers will warn)"}
        </p>
      </div>
    </div>
  );
}

// ServiceRoutesCard edits the other services of a compose app that have
// domains of their own.
function ServiceRoutesCard({ app, user }: { app: App; user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  type Row = { service: string; port: string; domains: string };
  const toRows = (a: App): Row[] =>
    (a.routes ?? []).map((r) => ({ service: r.service, port: String(r.port), domains: r.domains.join(" ") }));
  const [rows, setRows] = useState<Row[]>(() => toRows(app));
  const admin = can(user, "admin");
  const names = app.compose?.services.filter((s) => !s.job && s.name !== app.service).map((s) => s.name) ?? [];
  const set = (i: number, patch: Partial<Row>) => setRows(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const save = useMutation({
    mutationFn: () =>
      api.setAppRoutes(
        app.id,
        rows.map((r) => ({ service: r.service.trim(), port: Number(r.port) || 0, domains: r.domains.split(/[\s,]+/).filter(Boolean) })),
      ),
    onSuccess: (r) => {
      qc.setQueryData(["app", app.id], { app: r.app, sync: r.sync });
      qc.invalidateQueries({ queryKey: ["apps"] });
      setRows(toRows(r.app));
      toast("Saved. A new service or port is served after the next deployment.");
    },
  });
  if (!admin && rows.length === 0) return null;
  return (
    <form
      className="card card-body form"
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <h2>Other services</h2>
      <p className="muted small" style={{ margin: 0 }}>
        More services of the compose file with domains of their own (for example an API or an admin panel).
      </p>
      <ErrorBox error={save.error} />
      {rows.map((r, i) => (
        <div key={i} className="grid-2" style={{ gridTemplateColumns: "1fr 6rem" }}>
          <Field label="Service">
            <select className="input mono" value={r.service} onChange={(e) => set(i, { service: e.target.value, port: "" })} disabled={!admin}>
              <option value="" disabled>
                Choose…
              </option>
              {[...new Set([...names, r.service].filter(Boolean))].map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Port">
            <input
              className="input mono"
              type="number"
              min={0}
              max={65535}
              value={r.port}
              placeholder={String(app.compose?.services.find((s) => s.name === r.service)?.ports?.[0] ?? "")}
              onChange={(e) => set(i, { port: e.target.value })}
              disabled={!admin}
            />
          </Field>
          <div style={{ gridColumn: "1 / -1" }}>
            <Field label="Domains" help="Separated by spaces.">
              <div className="row" style={{ gap: 8 }}>
                <input className="input mono" value={r.domains} onChange={(e) => set(i, { domains: e.target.value })} disabled={!admin} />
                {admin && (
                  <button type="button" className="btn sm" onClick={() => setRows(rows.filter((_, j) => j !== i))}>
                    Remove
                  </button>
                )}
              </div>
            </Field>
          </div>
        </div>
      ))}
      {admin && (
        <div className="row" style={{ gap: 8 }}>
          <button
            type="button"
            className="btn"
            disabled={names.length === 0}
            onClick={() => setRows([...rows, { service: names.find((n) => !rows.some((r) => r.service === n)) ?? "", port: "", domains: "" }])}
          >
            Add service
          </button>
          <button className="btn primary" disabled={save.isPending}>
            Save
          </button>
        </div>
      )}
    </form>
  );
}

const formatVolumes = (v: AppVolume[]) => v.map((x) => `${x.name}:${x.path}`).join("\n");

function parseVolumes(text: string): AppVolume[] {
  return text
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean)
    .map((l) => {
      const i = l.indexOf(":");
      return i < 0 ? { name: l, path: "" } : { name: l.slice(0, i).trim(), path: l.slice(i + 1).trim() };
    });
}

function SettingsTab({ app, panelAdmin }: { app: App; panelAdmin: boolean }) {
  const move = panelAdmin && app.source !== "mail" && (
    <MoveProjectCard kind="apps" id={app.id} current={app.project_id} what={app.source === "database" ? "the database" : "the app"} />
  );
  const qc = useQueryClient();
  const toast = useToast();
  const [image, setImage] = useState(app.image);
  const [port, setPort] = useState(String(app.port));
  const [volumes, setVolumes] = useState(formatVolumes(app.volumes));
  const [memory, setMemory] = useState(app.memory_mb ? String(app.memory_mb) : "");
  const [cpus, setCpus] = useState(app.cpus ? String(app.cpus) : "");
  const [readOnly, setReadOnly] = useState(app.read_only);
  const save = useMutation({
    mutationFn: () =>
      api.updateApp(app.id, {
        image: image.trim(),
        git: app.git,
        port: Number(port) || 0,
        volumes: parseVolumes(volumes),
        memory_mb: Number(memory) || 0,
        cpus: Number(cpus) || 0,
        read_only: readOnly,
      }),
    onSuccess: (r) => {
      qc.setQueryData(["app", app.id], (old: { app: App } | undefined) => (old ? { ...old, app: r.app } : old));
      qc.invalidateQueries({ queryKey: ["apps"] });
      toast("Saved. Deploy to apply.");
    },
  });
  if (app.source === "database" || app.source === "mail") {
    return (
      <div className="stack" style={{ maxWidth: 680 }}>
        <form
          className="card card-body form"
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            save.mutate();
          }}
        >
          <h2>Resources</h2>
          <ErrorBox error={save.error} />
          <div className="grid-2">
            <Field label="Memory limit (MB)" help="Empty for no limit.">
              <input className="input mono" type="number" min={16} value={memory} onChange={(e) => setMemory(e.target.value)} />
            </Field>
            <Field label="CPU limit (cores)" help="Empty for no limit, e.g. 0.5.">
              <input className="input mono" type="number" min={0} step="0.1" value={cpus} onChange={(e) => setCpus(e.target.value)} />
            </Field>
          </div>
          <p className="faint small" style={{ margin: 0 }}>
            A deployment restarts the {app.source === "mail" ? "mail server" : "database"} with new limits: connections
            drop for a few seconds.
          </p>
          <div>
            <button className="btn primary" disabled={save.isPending}>
              Save
            </button>
          </div>
        </form>
        {move}
        <DeleteCard app={app} />
      </div>
    );
  }
  if (isCompose(app)) {
    return (
      <div className="stack" style={{ maxWidth: 680 }}>
        {move}
        <DeleteCard app={app} />
      </div>
    );
  }
  return (
    <div className="stack" style={{ maxWidth: 680 }}>
      <form
        className="card card-body form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <h2>Runtime</h2>
        <ErrorBox error={save.error} />
        {app.source === "image" && (
          <Field label="Image">
            <input className="input mono" value={image} onChange={(e) => setImage(e.target.value)} required />
          </Field>
        )}
        <Field label="HTTP port" help="0 if the app serves no HTTP (a worker).">
          <input className="input mono" type="number" min={0} max={65535} value={port} onChange={(e) => setPort(e.target.value)} />
        </Field>
        <Field label="Persistent volumes" help="One per line, name:/path/in/container. Data survives deployments.">
          <textarea className="input mono" rows={3} value={volumes} onChange={(e) => setVolumes(e.target.value)} placeholder="data:/data" />
        </Field>
        <div className="grid-2">
          <Field label="Memory limit (MB)" help="Empty for no limit.">
            <input className="input mono" type="number" min={16} value={memory} onChange={(e) => setMemory(e.target.value)} />
          </Field>
          <Field label="CPU limit (cores)" help="Empty for no limit, e.g. 0.5.">
            <input className="input mono" type="number" min={0} step="0.1" value={cpus} onChange={(e) => setCpus(e.target.value)} />
          </Field>
        </div>
        <Field
          label="Security"
          help="The container cannot change its own files, which stops many attacks from persisting. /tmp and /run stay writable in memory, volumes stay writable; apps that write elsewhere fail to start."
        >
          <label className="check">
            <input type="checkbox" checked={readOnly} onChange={(e) => setReadOnly(e.target.checked)} /> Read-only root filesystem
          </label>
        </Field>
        <div>
          <button className="btn primary" disabled={save.isPending}>
            Save
          </button>
        </div>
      </form>
      {move}
      <DeleteCard app={app} />
    </div>
  );
}

function DeleteCard({ app }: { app: App }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const toast = useToast();
  const [open, setOpen] = useState(false);
  const [confirmName, setConfirmName] = useState("");
  const [volumes, setVolumes] = useState(false);
  const isDB = app.source === "database";
  const isMail = app.source === "mail";
  const volumeNames = isMail
    ? ["etc", "data"]
    : isDB
      ? ["data"]
      : isCompose(app)
        ? (app.compose?.volumes ?? [])
        : app.volumes.map((v) => v.name);
  const del = useMutation({
    mutationFn: () => api.deleteApp(app.id, volumes),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["apps"] });
      const what = isMail ? "Mail server" : isDB ? "Database" : "App";
      toast(r.cleanup_error ? `${what} deleted; cleanup pending: ${r.cleanup_error}` : `${what} deleted`, !!r.cleanup_error);
      qc.invalidateQueries({ queryKey: ["mail"] });
      navigate(isMail ? "/mail" : isDB ? "/databases" : "/apps");
    },
  });
  return (
    <div className="card card-body stack">
      <h2>Delete {isDB ? "database" : "app"}</h2>
      <p className="muted" style={{ margin: 0 }}>
        {isDB
          ? "Its container is removed and linked apps lose access. The data volume is kept unless you choose to delete it."
          : "Its domains stop being served and its containers are removed. Persistent volumes are kept unless you choose to delete them."}
      </p>
      <div>
        <button className="btn danger" onClick={() => setOpen(true)}>
          <Icon name="trash" /> Delete {app.name}
        </button>
      </div>
      {open && (
        <Modal
          title={`Delete ${app.name}?`}
          onClose={() => setOpen(false)}
          footer={
            <>
              <button className="btn" onClick={() => setOpen(false)}>
                Cancel
              </button>
              <button className="btn danger solid" disabled={confirmName !== app.name || del.isPending} onClick={() => del.mutate()}>
                Delete {isDB ? "database" : "app"}
              </button>
            </>
          }
        >
          <div className="form">
            <ErrorBox error={del.error} />
            {volumeNames.length > 0 && (
              <label className="check">
                <input type="checkbox" checked={volumes} onChange={(e) => setVolumes(e.target.checked)} /> Also delete its data
                volumes ({volumeNames.join(", ")}) — this cannot be undone
              </label>
            )}
            <Field label={`Type ${app.name} to confirm`}>
              <input className="input" value={confirmName} onChange={(e) => setConfirmName(e.target.value)} autoFocus />
            </Field>
          </div>
        </Modal>
      )}
    </div>
  );
}

function SourceTab({ app }: { app: App }) {
  const qc = useQueryClient();
  const toast = useToast();
  const g = app.git!;
  const [url, setUrl] = useState(g.url);
  const [branch, setBranch] = useState(g.branch);
  const [dockerfile, setDockerfile] = useState(g.dockerfile);
  const [builder, setBuilder] = useState<"" | "railpack" | "compose">(g.builder ?? "");
  const [composeFile, setComposeFile] = useState(g.compose_file ?? "");
  const gitCompose = builder === "compose" ? { compose_file: composeFile.trim() } : {};
  const [buildEnv, setBuildEnv] = useState((g.build_env ?? []).join(" "));
  const buildEnvList = buildEnv.split(/[\s,]+/).filter(Boolean);
  const [context, setContext] = useState(g.context);
  const save = useMutation({
    mutationFn: () =>
      api.updateApp(app.id, {
        image: "",
        git: g.source_id
          ? { url: g.url, source_id: g.source_id, repo: g.repo, branch: branch.trim(), dockerfile: dockerfile.trim(), context: context.trim(), builder, build_env: buildEnvList, ...gitCompose }
          : { url: url.trim(), branch: branch.trim(), dockerfile: dockerfile.trim(), context: context.trim(), builder, build_env: buildEnvList, ...gitCompose },
        port: app.port,
        volumes: app.volumes,
        memory_mb: app.memory_mb,
        cpus: app.cpus,
        read_only: app.read_only,
      }),
    onSuccess: (r) => {
      qc.setQueryData(["app", app.id], (old: { app: App } | undefined) => (old ? { ...old, app: r.app } : old));
      toast("Saved. The next deployment uses it.");
    },
  });
  const ssh = !app.git!.url.startsWith("https://");
  return (
    <div className="stack" style={{ maxWidth: 760 }}>
      <form
        className="card card-body form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <h2>Repository</h2>
        <ErrorBox error={save.error} />
        {g.source_id ? (
          <p className="muted" style={{ margin: 0 }}>
            <span className="mono">{g.repo}</span> through a <Link to="/git-sources">git source</Link>: Basalt connected the
            repository (GitHub: a token for each build that reads it only; GitLab, Gitea: the app's read-only deploy key and a
            push webhook it added there).
          </p>
        ) : (
          <Field label="URL" help="https://… for public repositories, git@host:org/repo.git with the deploy key below for private ones.">
            <input className="input mono" value={url} onChange={(e) => setUrl(e.target.value)} required />
          </Field>
        )}
        <div className="grid-2">
          <Field label="Branch">
            <input className="input mono" value={branch} onChange={(e) => setBranch(e.target.value)} required />
          </Field>
          {builder === "compose" ? (
            <Field label="Compose file" help="Path in the repository; services with build: are built from the repository.">
              <input className="input mono" value={composeFile} onChange={(e) => setComposeFile(e.target.value)} required />
            </Field>
          ) : (
            <Field label="Build" help={builder ? "Railpack works out how to build it; the app listens on $PORT." : undefined}>
              <div className="row">
                <select className="input" style={{ width: "auto" }} value={builder} onChange={(e) => setBuilder(e.target.value as "" | "railpack")}>
                  <option value="">Dockerfile</option>
                  <option value="railpack">Automatic</option>
                </select>
                {!builder && <input className="input mono" value={dockerfile} onChange={(e) => setDockerfile(e.target.value)} required />}
              </div>
            </Field>
          )}
        </div>
        {builder !== "compose" && (
          <Field label="Build context" help="Directory sent to the build; . is the repository root.">
            <input className="input mono" value={context} onChange={(e) => setContext(e.target.value)} required />
          </Field>
        )}
        <Field
          label="Variables at build time"
          help={
            builder === "railpack" ? (
              <>Names of variables under Environment the build needs, e.g. NEXT_PUBLIC_API_URL. Its steps get them; they are not kept in the image.</>
            ) : (
              <>
                Names of variables under Environment the build{builder === "compose" ? "s need (every service built gets them)" : " needs"}. The
                Dockerfile reads each as a secret, e.g.{" "}
                <code>RUN --mount=type=secret,id=NPM_TOKEN,env=NPM_TOKEN npm ci</code>: they are not kept in the image.
              </>
            )
          }
        >
          <input className="input mono" value={buildEnv} onChange={(e) => setBuildEnv(e.target.value)} placeholder="NEXT_PUBLIC_API_URL NPM_TOKEN" />
        </Field>
        <div>
          <button className="btn primary" disabled={save.isPending}>
            Save
          </button>
        </div>
      </form>
      {!g.source_id && <DeployKeyCard app={app} ssh={ssh} />}
      <WebhookCard app={app} github={!!g.source_id} />
    </div>
  );
}

function DeployKeyCard({ app, ssh }: { app: App; ssh: boolean }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const regenerate = async () => {
    const ok = await confirm({
      title: "Replace the deploy key?",
      body: <p>Builds fail until the new key is added to the repository. Remove the old key there too.</p>,
      confirm: "Replace",
      danger: true,
    });
    if (!ok) return;
    try {
      const r = await api.regenerateDeployKey(app.id);
      qc.setQueryData(["app", app.id], (old: { app: App } | undefined) => (old ? { ...old, app: r.app } : old));
      toast("New deploy key created");
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  return (
    <div className="card card-body stack">
      <div className="row" style={{ justifyContent: "space-between" }}>
        <h2>Deploy key</h2>
        <button className="btn sm" onClick={regenerate}>
          <Icon name="refresh" size={14} /> Replace
        </button>
      </div>
      {!ssh && (
        <div className="alert small">
          The repository is fetched over HTTPS, so no key is used. For a private repository, switch to its SSH URL
          (git@…) and add this key.
        </div>
      )}
      <p className="muted" style={{ margin: 0 }}>
        Grants this app read access to a private repository. Add it as a <strong>read-only</strong> deploy key:
      </p>
      <ul className="steps muted small">
        <li>GitHub: repository → Settings → Deploy keys → Add deploy key</li>
        <li>GitLab: project → Settings → Repository → Deploy keys</li>
        <li>Gitea / Forgejo: repository → Settings → Deploy keys</li>
      </ul>
      <CodeBox text={app.deploy_key ?? ""} />
      <p className="faint small" style={{ margin: 0 }}>
        The private key is stored encrypted and only sent to the app's server for fetching the code. The git server's own
        key is remembered on first contact.
      </p>
    </div>
  );
}

function WebhookCard({ app, github }: { app: App; github: boolean }) {
  const qc = useQueryClient();
  const toast = useToast();
  const hook = useQuery({ queryKey: ["webhook", app.id], queryFn: () => api.appWebhook(app.id), gcTime: 0 });
  const update = useMutation({
    mutationFn: (b: { auto_deploy: boolean; regenerate?: boolean }) => api.setAppWebhook(app.id, b),
    onSuccess: (r, b) => {
      qc.setQueryData(["webhook", app.id], r);
      qc.invalidateQueries({ queryKey: ["app", app.id] });
      toast(b.regenerate ? "New secret created; update it at your git host" : "Saved");
    },
    onError: (e) => toast(errorMessage(e), true),
  });
  if (hook.isPending) return <Loading />;
  if (hook.error) return <ErrorBox error={hook.error} />;
  const w = hook.data.webhook;
  return (
    <div className="card card-body stack">
      <div className="row" style={{ justifyContent: "space-between" }}>
        <h2>Deploy on push</h2>
        <label className="check">
          <input type="checkbox" checked={w.auto_deploy} onChange={(e) => update.mutate({ auto_deploy: e.target.checked })} />{" "}
          enabled
        </label>
      </div>
      {github ? (
        <p className="muted" style={{ margin: 0 }}>
          The repository tells the panel of every push to <span className="mono">{app.git?.branch}</span> (Basalt set this up): that
          commit is deployed. A push during a deployment is deployed right after it.
        </p>
      ) : (
        <WebhookSetup app={app} secret={w.secret} path={w.path} onRegenerate={() => update.mutate({ auto_deploy: w.auto_deploy, regenerate: true })} pending={update.isPending} />
      )}
    </div>
  );
}

function WebhookSetup({ app, secret, path, onRegenerate, pending }: { app: App; secret: string; path: string; onRegenerate: () => void; pending: boolean }) {
  return (
    <>
      <p className="muted" style={{ margin: 0 }}>
        Add a webhook at your git host; every push to <span className="mono">{app.git?.branch}</span> deploys that commit.
        A push during a deployment is deployed right after it.
      </p>
      <Field label="Payload URL" help="Must be reachable from your git host.">
        <CodeBox text={window.location.origin + path} />
      </Field>
      <Field label="Secret">
        <CodeBox text={secret} />
      </Field>
      <ul className="steps muted small">
        <li>GitHub: Settings → Webhooks → Add webhook; content type application/json, the secret above, "Just the push event".</li>
        <li>GitLab: Settings → Webhooks; the secret as "Secret token", trigger "Push events".</li>
        <li>Gitea / Forgejo: Settings → Webhooks → Add webhook; POST, application/json, the secret above, push events.</li>
      </ul>
      <div>
        <button className="btn sm" onClick={onRegenerate} disabled={pending}>
          <Icon name="refresh" size={14} /> New secret
        </button>
      </div>
    </>
  );
}

function ServicesCard({ app, services }: { app: App; services?: ServiceStatus[] }) {
  const byName = new Map((services ?? []).map((s) => [s.name, s]));
  return (
    <div className="card">
      <div className="card-head">
        <h2>Services</h2>
      </div>
      <div className="table-wrap">
        <table className="table">
          <thead>
            <tr>
              <th>Service</th>
              <th>Image</th>
              <th>State</th>
              <th className="hide-sm">Deployment</th>
            </tr>
          </thead>
          <tbody>
            {(app.compose?.services ?? []).map((svc) => {
              const st = byName.get(svc.name);
              const done = st?.state === "exited" && !st.exit_code;
              return (
                <tr key={svc.name}>
                  <td>
                    <strong>{svc.name}</strong>
                    {svc.name === app.service && <span className="faint small"> · port {app.port}, domains</span>}
                    {app.routes
                      ?.filter((r) => r.service === svc.name)
                      .map((r) => (
                        <span key={r.service} className="faint small">
                          {" "}
                          · port {r.port}, {r.domains.join(", ")}
                        </span>
                      ))}
                    {svc.job && <span className="faint small"> · runs once per deployment</span>}
                  </td>
                  <td className="mono muted small">{st?.image ?? (svc.build ? `built from ${svc.build}` : svc.image)}</td>
                  <td>
                    {!st ? (
                      <span className="badge">not deployed</span>
                    ) : (
                      <span className="row wrap" style={{ gap: 4 }}>
                        <span className={`badge ${st.state === "running" || done ? "ok" : "err"}`}>
                          {done ? "finished" : st.state}
                        </span>
                        {st.health && <span className={`badge ${st.health === "healthy" ? "ok" : "warn"}`}>{st.health}</span>}
                        {!!st.restart_count && <span className="badge warn">{st.restart_count} restarts</span>}
                        {st.oom_killed && <span className="badge err">out of memory</span>}
                      </span>
                    )}
                  </td>
                  <td className="mono muted hide-sm">{st?.deployment_id ? `#${st.deployment_id}` : ""}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function ComposeTab({ app }: { app: App }) {
  const qc = useQueryClient();
  const toast = useToast();
  const file = useQuery({ queryKey: ["app-compose", app.id], queryFn: () => api.appCompose(app.id), gcTime: 0 });
  const [text, setText] = useState<string | null>(null);
  const [service, setService] = useState(app.service ?? "");
  const [port, setPort] = useState(app.port ? String(app.port) : "");
  const [warnings, setWarnings] = useState<string[] | null>(null);
  useEffect(() => {
    if (file.data && text === null) {
      setText(file.data.compose);
      setWarnings(file.data.warnings);
    }
  }, [file.data, text]);
  const fromRepo = app.source === "git";
  const saved = (r: { app: App; warnings: string[] }) => {
    qc.setQueryData(["app", app.id], (old: { app: App } | undefined) => (old ? { ...old, app: r.app } : old));
    qc.invalidateQueries({ queryKey: ["apps"] });
    setWarnings(r.warnings);
    setPort(r.app.port ? String(r.app.port) : "");
    setService(r.app.service ?? "");
  };
  const save = useMutation({
    mutationFn: () => api.setAppCompose(app.id, { compose: text ?? "", service: service.trim(), port: Number(port) || 0 }),
    onSuccess: (r) => {
      saved(r);
      toast("Saved. Deploy to apply.");
    },
  });
  // A repository's file is read again on every deployment; this shows it now.
  const load = useMutation({
    mutationFn: () => api.loadAppCompose(app.id),
    onSuccess: async (r) => {
      saved(r);
      const f = await file.refetch();
      setText(f.data?.compose ?? "");
      toast(`Read at commit ${r.commit.slice(0, 8)}`);
    },
  });
  if (file.isPending) return <Loading />;
  if (file.error) return <ErrorBox error={file.error} />;
  const names = app.compose?.services.filter((s) => !s.job).map((s) => s.name) ?? [];
  return (
    <form
      className="stack"
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <div className="card card-body form">
        <div className="row" style={{ justifyContent: "space-between" }}>
          <h2>Compose file</h2>
          {fromRepo && (
            <button type="button" className="btn sm" disabled={load.isPending} onClick={() => load.mutate()}>
              <Icon name="refresh" size={14} /> {load.isPending ? "Reading…" : "Load from repository"}
            </button>
          )}
        </div>
        <ErrorBox error={load.error} />
        {file.data.error && !save.isSuccess && !load.isSuccess && <div className="alert err small">{file.data.error}</div>}
        {warnings && warnings.length > 0 && (
          <div className="alert warn small">
            <ul className="steps">
              {warnings.map((w) => (
                <li key={w}>{w}</li>
              ))}
            </ul>
          </div>
        )}
        {fromRepo ? (
          file.data.compose === "" ? (
            <p className="muted" style={{ margin: 0 }}>
              <span className="mono">{app.git?.compose_file}</span> is read from branch{" "}
              <span className="mono">{app.git?.branch}</span> on every deployment. Load it to see its services and choose the
              one the app's domains point to.
            </p>
          ) : (
            <Field
              label={app.git?.compose_file ?? "compose.yaml"}
              help="As last read from the repository (change it there). Services with build: are built on the server; bind mounts, host networking and privileges are refused, ports are not published."
            >
              <textarea className="input mono" rows={22} value={text ?? ""} readOnly spellCheck={false} />
            </Field>
          )
        ) : (
          <Field
            label="docker-compose.yml"
            help="Stored encrypted. Bind mounts, host networking, privileges and builds are refused; ports are not published on the server."
          >
            <textarea className="input mono" rows={22} value={text ?? ""} onChange={(e) => setText(e.target.value)} spellCheck={false} />
          </Field>
        )}
      </div>
      <div className="card card-body form">
        <h2>Published service</h2>
        <div className="grid-2">
          <Field label="Service" help="The app's domains point to it. Empty: the first service with a port.">
            <input
              className="input mono"
              list="compose-services"
              value={service}
              onChange={(e) => {
                setService(e.target.value);
                setPort("");
              }}
            />
            <datalist id="compose-services">
              {names.map((n) => (
                <option key={n} value={n} />
              ))}
            </datalist>
          </Field>
          <Field label="Port" help="Inside the container; empty uses the one in the file.">
            <input className="input mono" type="number" min={0} max={65535} value={port} onChange={(e) => setPort(e.target.value)} />
          </Field>
        </div>
        <ErrorBox error={save.error} />
        <div>
          <button className="btn primary" disabled={save.isPending || (fromRepo && file.data.compose === "" && !load.isSuccess)}>
            Save
          </button>
        </div>
      </div>
    </form>
  );
}

// rate is bytes per second between two cumulative counters (0 after a reset).
const rate = (a: { at: string; v: number }, b: { at: string; v: number }) => {
  const dt = (new Date(b.at).getTime() - new Date(a.at).getTime()) / 1000;
  return dt > 0 && b.v >= a.v ? (b.v - a.v) / dt : 0;
};

function ResourcesCard({ app, running }: { app: App; running: boolean }) {
  const [period, setPeriod] = useState(60);
  const metrics = useQuery({
    queryKey: ["app-metrics", app.id, period],
    queryFn: () => api.appMetrics(app.id, period),
    placeholderData: (prev) => prev,
    refetchInterval: rangeRefresh(period),
  });
  const list = metrics.data?.metrics ?? [];
  if (list.length === 0) return null;
  const last = list[list.length - 1];
  // Samples stop when no container runs.
  const stale = !running || Date.now() - new Date(last.at).getTime() > 60_000;
  const prev = list.length > 1 ? list[list.length - 2] : last;
  const rx = rate({ at: prev.at, v: prev.net_rx }, { at: last.at, v: last.net_rx });
  const tx = rate({ at: prev.at, v: prev.net_tx }, { at: last.at, v: last.net_tx });
  const series = (f: (x: (typeof list)[number]) => number) => list.map((x) => ({ t: new Date(x.at).getTime(), v: f(x) }));
  const traffic = list.slice(1).map((x, i) => ({
    t: new Date(x.at).getTime(),
    v: rate({ at: list[i].at, v: list[i].net_rx + list[i].net_tx }, { at: x.at, v: x.net_rx + x.net_tx }),
  }));
  return (
    <div className="stack">
      <div className="grid-4">
        <Stat label="CPU" value={stale ? "—" : `${last.cpu_pct.toFixed(1)}%`} hint="of one core" />
        <Stat
          label="Memory"
          value={stale ? "—" : formatBytes(last.mem_used)}
          hint={app.memory_mb || app.source === "compose" ? `limit ${formatBytes(last.mem_limit)}` : `no limit (host ${formatBytes(last.mem_limit)})`}
        />
        <Stat label="Network in" value={stale ? "—" : `${formatBytes(rx)}/s`} hint={`${formatBytes(last.net_rx)} since start`} />
        <Stat label="Network out" value={stale ? "—" : `${formatBytes(tx)}/s`} hint={`${formatBytes(last.net_tx)} since start`} />
      </div>
      <div className="row" style={{ justifyContent: "flex-end" }}>
        <RangePicker value={period} onChange={setPeriod} />
      </div>
      <div className="grid-2">
        <div className="card card-body">
          <h3>CPU · {rangeTitle(period)}</h3>
          <Chart points={series((x) => x.cpu_pct)} format={(v) => `${v.toFixed(v < 10 ? 1 : 0)}%`} />
        </div>
        <div className="card card-body">
          <h3>Memory · {rangeTitle(period)}</h3>
          <Chart points={series((x) => x.mem_used)} format={(v) => formatBytes(v)} />
        </div>
      </div>
      <div className="card card-body">
        <h3>Network traffic · {rangeTitle(period)}</h3>
        <Chart points={traffic} format={(v) => `${formatBytes(v)}/s`} />
      </div>
    </div>
  );
}

const checkBadge: Record<string, string> = { up: "badge ok", down: "badge err", unknown: "badge plain" };

// ServersCard lists the servers an app runs on and lets admins add or
// remove replicas.
function ServersCard({ app, user, me, placements }: { app: App; user: User; me: User; placements: AppPlacement[] }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const [target, setTarget] = useState<number | "">("");
  const admin = can(user, "admin");
  const list = placements.length > 0 ? placements : [{ server_id: app.server_id, primary: true } as AppPlacement];
  const on = new Set(list.map((p) => p.server_id));
  const candidates = (servers.data?.servers ?? []).filter((s) => s.docker && !on.has(s.id));
  const name = (id: number) => servers.data?.servers.find((s) => s.id === id)?.name ?? `server #${id}`;
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["app", app.id] });
    qc.invalidateQueries({ queryKey: ["apps"] });
    qc.invalidateQueries({ queryKey: ["deployments", app.id] });
    qc.invalidateQueries({ queryKey: ["health-checks"] });
  };
  const add = useMutation({
    mutationFn: () => api.addReplica(app.id, Number(target || candidates[0]?.id)),
    onSuccess: (r) => {
      setTarget("");
      refresh();
      toast(
        r.deploy_error
          ? `Added; deploy to start it there (${r.deploy_error})`
          : r.deploying
            ? "Added; the current version is being deployed there"
            : "Added; the next deployment runs there too",
        !!r.deploy_error,
      );
    },
  });
  const remove = async (id: number) => {
    const ok = await confirm({
      title: `Stop running ${app.name} on ${name(id)}?`,
      body: <p>Its domains stop pointing at {name(id)} and its containers there are removed.</p>,
      confirm: "Remove",
      danger: true,
    });
    if (!ok) return;
    try {
      const r = await api.removeReplica(app.id, id);
      refresh();
      if (r.cleanup_error) toast(`Removed; cleanup pending: ${r.cleanup_error}`, true);
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  return (
    <div className="card">
      <div className="card-head">
        <h2>Servers</h2>
      </div>
      <div className="table-wrap">
        <table className="table">
          <tbody>
            {list.map((p) => (
              <tr key={p.server_id}>
                <td>
                  <strong>
                    <ServerLink id={p.server_id} name={name(p.server_id)} user={me} />
                  </strong>
                  {p.primary && list.length > 1 && <span className="faint small"> · primary</span>}
                </td>
                <td>
                  {p.health_check ? (
                    <Link
                      to="/monitoring"
                      className={checkBadge[p.status ?? "unknown"] ?? "badge"}
                      title="Its health check: DNS leaves out a server whose copy is down (unless every copy is)"
                    >
                      {p.status ?? "unknown"}
                    </Link>
                  ) : null}
                  {p.services?.map((c) => (
                    <Link
                      key={c.service}
                      to="/monitoring"
                      className={checkBadge[c.status] ?? "badge"}
                      style={{ marginLeft: 6 }}
                      title={`The health check of service ${c.service} there: DNS leaves out a server whose copy is down for its domains`}
                    >
                      {c.service}: {c.status}
                    </Link>
                  ))}
                </td>
                <td className="num">
                  {admin && !p.primary && (
                    <button className="btn sm icon ghost" title="Remove" onClick={() => remove(p.server_id)}>
                      <Icon name="trash" size={14} />
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="card-body stack">
        <p className="faint small" style={{ margin: 0 }}>
          {list.length > 1
            ? `Every deployment runs on each server in turn${app.source === "compose" ? " (all services, jobs included, on each)" : ""}. DNS answers with the servers whose copy passes its health check (HTTPS on /, editable under Monitoring); certificates come through DNS.`
            : `Run the app on more servers for failover: DNS then answers only with the servers whose copy is up. It needs domains in a zone under DNS, and no ${app.source === "compose" ? "volumes in its compose file" : "volumes"} or linked databases (each server keeps its own copy).`}
        </p>
        {admin &&
          (candidates.length === 0 ? (
            <p className="faint small" style={{ margin: 0 }}>No other server with Docker.</p>
          ) : (
            <form
              className="row wrap"
              onSubmit={(e: FormEvent) => {
                e.preventDefault();
                add.mutate();
              }}
            >
              <select className="input" style={{ width: "auto", minWidth: 180 }} value={target || candidates[0].id} onChange={(e) => setTarget(Number(e.target.value))}>
                {candidates.map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name} {s.status !== "online" ? `(${s.status})` : ""}
                  </option>
                ))}
              </select>
              <button className="btn sm" disabled={add.isPending}>
                <Icon name="plus" size={14} /> Run here too
              </button>
            </form>
          ))}
        <ErrorBox error={add.error} />
      </div>
    </div>
  );
}

// LinkedDatabases lists the databases this app reaches, if any.
function LinkedDatabases({ app }: { app: App }) {
  const apps = useQuery({ queryKey: ["apps"], queryFn: api.apps });
  const dbs = (apps.data?.apps ?? []).filter((a) => a.source === "database" && a.links?.includes(app.id));
  if (dbs.length === 0) return null;
  return (
    <div className="card card-body stack">
      <h2>Databases</h2>
      <div className="row wrap">
        {dbs.map((d) => (
          <Link key={d.id} to={appPath(d)} className="row" style={{ gap: 6 }}>
            <Icon name="database" size={14} />
            <strong>{d.name}</strong> <span className="faint small">{sourceLabel(d)}</span>
          </Link>
        ))}
      </div>
      <p className="faint small" style={{ margin: 0 }}>
        Reachable from the app's containers by these host names.
      </p>
    </div>
  );
}

function LinksCard({ app, user }: { app: App; user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const apps = useQuery({ queryKey: ["apps"], queryFn: api.apps });
  const engines = useQuery({ queryKey: ["database-engines"], queryFn: api.databaseEngines, staleTime: Infinity });
  const engine = engines.data?.engines.find((e) => e.id === app.engine);
  const links = app.links ?? [];
  const candidates = (apps.data?.apps ?? []).filter(
    (a) => a.source !== "database" && a.server_id === app.server_id && !links.includes(a.id),
  );
  const [target, setTarget] = useState<number | "">("");
  const [setEnv, setSetEnv] = useState(true);
  const [envVar, setEnvVar] = useState("");
  const done = (r: { app: App; attach_error?: string }, msg: string) => {
    qc.setQueryData(["app", app.id], (old: { app: App } | undefined) => (old ? { ...old, app: r.app } : old));
    qc.invalidateQueries({ queryKey: ["apps"] });
    if (r.attach_error) toast(`Saved, but not applied on the server yet: ${r.attach_error}`, true);
    else toast(msg);
  };
  const link = useMutation({
    mutationFn: () => api.linkApp(app.id, Number(target || candidates[0]?.id), setEnv ? envVar || engine?.env_var || "" : ""),
    onSuccess: (r) => {
      setTarget("");
      done(r, setEnv ? "Linked. Deploy the app to give it the new variable." : "Linked");
    },
  });
  const unlink = useMutation({
    mutationFn: (appId: number) => api.unlinkApp(app.id, appId),
    onSuccess: (r) => done(r, "Unlinked"),
    onError: (e) => toast(errorMessage(e), true),
  });
  const name = (id: number) => apps.data?.apps.find((a) => a.id === id)?.name ?? `#${id}`;
  return (
    <div className="card">
      <div className="card-head">
        <h2>Linked apps</h2>
      </div>
      <div className="card-body stack">
        <p className="muted" style={{ margin: 0 }}>
          Linked apps reach the database at host <span className="mono">{app.name}</span>
          {engine && (
            <>
              , port <span className="mono">{engine.port}</span>
            </>
          )}
          . Nothing else can connect to it: other apps, the server's network and the internet cannot.
        </p>
        {links.length > 0 && (
          <div className="table-wrap">
            <table className="table">
              <tbody>
                {links.map((id) => (
                  <tr key={id}>
                    <td>
                      <Link to={`/apps/${id}`}>
                        <strong>{name(id)}</strong>
                      </Link>
                    </td>
                    <td className="num">
                      {can(user, "admin") && (
                        <button className="btn sm" disabled={unlink.isPending} onClick={() => unlink.mutate(id)}>
                          Unlink
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {can(user, "admin") &&
          (candidates.length === 0 ? (
            <p className="faint small" style={{ margin: 0 }}>
              {links.length === 0 ? "No app on this server to link yet." : "Every app on this server is linked."}
            </p>
          ) : (
            <form
              className="form"
              onSubmit={(e: FormEvent) => {
                e.preventDefault();
                link.mutate();
              }}
            >
              <ErrorBox error={link.error} />
              <div className="row wrap">
                <select
                  className="input"
                  style={{ width: "auto", minWidth: 180 }}
                  value={target || candidates[0].id}
                  onChange={(e) => setTarget(Number(e.target.value))}
                >
                  {candidates.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.name}
                    </option>
                  ))}
                </select>
                <label className="check small">
                  <input type="checkbox" checked={setEnv} onChange={(e) => setSetEnv(e.target.checked)} /> set its
                </label>
                <input
                  className="input mono"
                  style={{ width: 160 }}
                  value={envVar}
                  placeholder={engine?.env_var}
                  disabled={!setEnv}
                  onChange={(e) => setEnvVar(e.target.value)}
                />
                <span className="small muted">to the connection URL</span>
                <button className="btn primary sm" disabled={link.isPending}>
                  <Icon name="link" size={14} /> Link
                </button>
              </div>
            </form>
          ))}
      </div>
    </div>
  );
}

function ConnectionTab({ app }: { app: App }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const conn = useQuery({ queryKey: ["db-connection", app.id], queryFn: () => api.databaseConnection(app.id), gcTime: 0 });
  const change = useMutation({
    mutationFn: () => api.databasePassword(app.id, true),
    onSuccess: (r) => {
      qc.setQueryData(["db-connection", app.id], { connection: r.connection });
      qc.invalidateQueries({ queryKey: ["app", app.id] });
      const apps = r.updated_apps.length ? ` Updated in ${r.updated_apps.join(", ")}${r.redeploying.length ? "; redeploying" : ""}.` : "";
      toast(`New password set.${apps}`);
    },
    onError: (e) => toast(errorMessage(e), true),
  });
  const ask = async () => {
    const ok = await confirm({
      title: "Give the database a new password?",
      body: (
        <p>
          {app.engine === "redis"
            ? "Redis restarts with it (a few seconds). "
            : "It changes at once; connections already open stay. "}
          Linked apps whose environment holds the connection URL get the new one and are redeployed. Anything else using the
          old password stops connecting.
        </p>
      ),
      confirm: "Change password",
    });
    if (ok) change.mutate();
  };
  if (conn.isPending) return <Loading />;
  if (conn.error) return <ErrorBox error={conn.error} />;
  const c = conn.data.connection;
  const row = (label: string, value?: string) =>
    value ? (
      <Field label={label}>
        <CodeBox text={value} />
      </Field>
    ) : null;
  return (
    <div className="stack" style={{ maxWidth: 760 }}>
      <VersionCard app={app} />
      <div className="card card-body form">
        <h2>Connection</h2>
        <p className="muted" style={{ margin: 0 }}>
          For linked apps (see Overview). Linking can put the URL into an app's environment as{" "}
          <span className="mono">{c.env_var}</span>.
        </p>
        {row("URL", c.url)}
        <div className="grid-2">
          {row("Host", c.host)}
          {row("Port", String(c.port))}
        </div>
        <div className="grid-2">
          {row("Database", c.credentials.database)}
          {row("User", c.credentials.user)}
        </div>
        {row("Password", c.credentials.password)}
        {row("Root password", c.credentials.root_password)}
        <p className="faint small" style={{ margin: 0 }}>Stored encrypted.</p>
        <div>
          <button className="btn" disabled={change.isPending} onClick={ask}>
            <Icon name="key" size={14} /> {change.isPending ? "Changing…" : "Change password"}
          </button>
        </div>
      </div>
    </div>
  );
}

// VersionCard upgrades a database to a newer major version.
function VersionCard({ app }: { app: App }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const info = useQuery({
    // Asked again once an upgrade has changed the version.
    queryKey: ["db-upgrade", app.id, app.version],
    queryFn: () => api.databaseUpgradeInfo(app.id),
    refetchInterval: (q) => (q.state.data?.upgrade?.state === "backing_up" ? 3000 : false),
  });
  const [target, setTarget] = useState("");
  const [backup, setBackup] = useState(true);
  const start = useMutation({
    mutationFn: (v: { version: string; backup: boolean }) => api.upgradeDatabase(app.id, v.version, v.backup),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["db-upgrade", app.id] });
      qc.invalidateQueries({ queryKey: ["app", app.id] });
      qc.invalidateQueries({ queryKey: ["deployments", app.id] });
      toast(r.deployment ? "Upgrade deploying: follow it on Overview" : "Backing up first; the upgrade deploys when the backup succeeds");
    },
  });
  if (info.isPending || info.error) return info.error ? <ErrorBox error={info.error} /> : null;
  const d = info.data;
  const to = target || d.targets[0] || "";
  const engine = app.engine === "postgres" ? "PostgreSQL" : app.engine === "mariadb" ? "MariaDB" : app.engine === "redis" ? "Redis" : "MySQL";
  const ask = async () => {
    const withBackup = backup && d.backups;
    const ok = await confirm({
      title: `Upgrade ${app.name} to ${engine} ${to}?`,
      body: (
        <>
          <p>
            {d.dump
              ? "The database stops; its data is dumped and loaded into a new cluster of the new version in the same volume. It is down for as long as that takes (seconds for small databases, longer for large ones). If anything fails, the old version starts again with its data as it was; once the new version runs, the old data is removed."
              : `The new version starts on the same data files and upgrades them${app.engine === "mariadb" ? " (mariadb-upgrade, a few seconds of downtime)" : ""}.`}
          </p>
          <p>
            There is no way back to {engine} {d.version} afterwards: rollbacks only go to deployments of the new version.
            {withBackup ? " A backup is taken first; the upgrade starts only if it succeeds." : " Without a backup, a failed upgrade may leave nothing to go back to."}
          </p>
        </>
      ),
      confirm: "Upgrade",
      danger: !withBackup,
    });
    if (ok) start.mutate({ version: to, backup: withBackup });
  };
  return (
    <div className="card card-body form">
      <h2>Version</h2>
      <p className="muted" style={{ margin: 0 }}>
        {engine} {d.version}: each deployment takes its latest minor release.
        {d.targets.length === 0 && " This is the newest major version offered."}
      </p>
      {d.upgrade?.state === "backing_up" && (
        <div className="alert">Backing up before the upgrade to {d.upgrade.version}…</div>
      )}
      {d.upgrade?.state === "failed" && d.upgrade.error && <div className="alert err">Upgrade to {d.upgrade.version} did not start: {d.upgrade.error}</div>}
      <ErrorBox error={start.error} />
      {d.targets.length > 0 && (
        <>
          <div className="row">
            <select className="input" style={{ width: "auto" }} value={to} onChange={(e) => setTarget(e.target.value)}>
              {d.targets.map((v) => (
                <option key={v} value={v}>
                  {engine} {v}
                </option>
              ))}
            </select>
            <button className="btn" onClick={ask} disabled={start.isPending || d.upgrade?.state === "backing_up"}>
              <Icon name="upload" size={14} /> Upgrade
            </button>
          </div>
          <label className="row small">
            <input type="checkbox" checked={backup && d.backups} disabled={!d.backups} onChange={(e) => setBackup(e.target.checked)} />
            {d.backups ? "Back up first (the upgrade starts only if the backup succeeds)" : "Back up first: set up backups on the Backups tab"}
          </label>
        </>
      )}
    </div>
  );
}

// PreviewBanner heads the page of a preview: what it is a copy of, and
// that it goes with its pull request.
function PreviewBanner({ app }: { app: App }) {
  const parent = useQuery({ queryKey: ["app", app.preview_of], queryFn: () => api.app(app.preview_of!), retry: false });
  return (
    <div className="alert" style={{ marginBottom: 16 }}>
      Preview of{" "}
      {parent.data ? <Link to={`/apps/${app.preview_of}`}>{parent.data.app.name}</Link> : `app #${app.preview_of}`} for pull request #
      {app.pr_number} (branch <span className="mono">{app.git?.branch}</span>). Every push to the pull request deploys it; it is
      deleted, with its data, when the pull request closes.
    </div>
  );
}

// PreviewsCard sets where the app's pull requests are deployed and lists
// their previews.
function PreviewsCard({ app, user }: { app: App; user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const data = useQuery({ queryKey: ["previews", app.id], queryFn: () => api.previews(app.id), refetchInterval: 30_000 });
  const [editing, setEditing] = useState(false);
  const [domain, setDomain] = useState("");
  const save = useMutation({
    mutationFn: (d: string) => api.setPreviews(app.id, d),
    onSuccess: (r) => {
      qc.setQueryData(["previews", app.id], r);
      qc.invalidateQueries({ queryKey: ["app", app.id] });
      setEditing(false);
      toast(r.domain ? `Pull requests into ${app.git?.branch} are deployed at pr-<number>.${r.domain}` : "Previews turned off");
    },
  });
  if (data.isPending) return null;
  if (data.error) return <ErrorBox error={data.error} />;
  const d = data.data;
  if (!d.supported && d.previews.length === 0) return null; // apps of a git source only
  const admin = can(user, "admin");
  return (
    <div className="card">
      <div className="card-head">
        <h2>Previews of pull requests</h2>
        {admin && !editing && (
          <button
            className="btn sm"
            onClick={() => {
              setDomain(d.domain);
              setEditing(true);
            }}
          >
            <Icon name="edit" size={14} /> {d.domain ? "Change" : "Set up"}
          </button>
        )}
      </div>
      <div className="card-body stack">
        {editing ? (
          <form
            className="form"
            onSubmit={(e: FormEvent) => {
              e.preventDefault();
              save.mutate(domain.trim());
            }}
          >
            <ErrorBox error={save.error} />
            <Field
              label="Domain for previews"
              help={
                <>
                  Each pull request (merge request on GitLab) into <span className="mono">{app.git?.branch}</span> runs at{" "}
                  <span className="mono">pr-&lt;number&gt;.{domain.trim() || "preview.example.com"}</span>. Point{" "}
                  <span className="mono">*.{domain.trim() || "preview.example.com"}</span> at this app's server (automatic if
                  the zone is under DNS).
                  {app.git?.builder === "compose" && (
                    <>
                      {" "}
                      There a preview publishes service <span className="mono">{app.service}</span>; other services get no domains.
                    </>
                  )}{" "}
                  Empty: no previews.
                </>
              }
            >
              <input className="input mono" value={domain} onChange={(e) => setDomain(e.target.value)} placeholder="preview.example.com" autoFocus />
            </Field>
            <div className="row">
              <button className="btn primary sm" disabled={save.isPending}>
                Save
              </button>
              <button type="button" className="btn sm" onClick={() => setEditing(false)}>
                Cancel
              </button>
            </div>
          </form>
        ) : !d.domain ? (
          <p className="muted" style={{ margin: 0 }}>
            Off. With a domain for previews, each pull request into <span className="mono">{app.git?.branch}</span> gets its
            own copy of the app, deployed at every push to it and deleted when it closes.
          </p>
        ) : d.previews.length === 0 ? (
          <p className="muted" style={{ margin: 0 }}>
            Pull requests into <span className="mono">{app.git?.branch}</span> are deployed at{" "}
            <span className="mono">pr-&lt;number&gt;.{d.domain}</span>. None is open.
          </p>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <tbody>
                {d.previews.map((pv) => (
                  <tr key={pv.id}>
                    <td>
                      <Link to={`/apps/${pv.id}`}>
                        <strong>#{pv.pr_number}</strong>
                      </Link>{" "}
                      <span className="mono faint small">{pv.git?.branch}</span>
                    </td>
                    <td>
                      {pv.domains[0] && (
                        <a href={`https://${pv.domains[0]}`} target="_blank" rel="noreferrer noopener" className="mono small">
                          {pv.domains[0]} ↗
                        </a>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {d.domain && !editing && (
          <p className="faint small" style={{ margin: 0 }}>
            Previews get the app's environment variables (not its linked databases or data), and at most {d.max} run at once.
            Pull requests from forks are not built: their code would run on your server.
          </p>
        )}
      </div>
    </div>
  );
}
