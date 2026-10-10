import { useEffect, useLayoutEffect, useRef, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, NavLink, Route, Routes, useNavigate, useParams } from "react-router";
import { api, can, type App, type AppVolume, type Deployment, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, timeAgo, useConfirm, useToast } from "../components/ui";
import { useEventStream, type StreamState } from "../components/stream";
import { SyncBadge } from "./Server";

export function AppPage({ user }: { user: User }) {
  const id = Number(useParams().id);
  const app = useQuery({ queryKey: ["app", id], queryFn: () => api.app(id) });
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });

  if (app.isPending) return <Loading />;
  if (app.error) return <ErrorBox error={app.error} />;
  const a = app.data.app;
  const server = servers.data?.servers.find((x) => x.id === a.server_id);
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
            <SyncBadge sync={app.data.sync} />
          </div>
          <div className="sub row wrap">
            <span className="mono">{a.image}</span>
            {a.domains.map((d) => (
              <a key={d} href={`https://${d}`} target="_blank" rel="noreferrer noopener">
                {d} ↗
              </a>
            ))}
            <span className="faint">
              on <Link to={`/servers/${a.server_id}`}>{server?.name ?? `server #${a.server_id}`}</Link>
            </span>
          </div>
        </div>
      </div>
      {server && server.status !== "online" && (
        <div className="alert warn" style={{ marginBottom: 16 }}>
          The server is {server.status}. The app keeps running; it cannot be deployed or inspected until the agent
          reconnects.
        </div>
      )}
      <div className="tabs">
        {tab(`/apps/${id}`, "Overview")}
        {can(user, "member") && tab(`/apps/${id}/logs`, "Logs")}
        {can(user, "admin") && tab(`/apps/${id}/env`, "Environment")}
        {tab(`/apps/${id}/domains`, "Domains")}
        {can(user, "admin") && tab(`/apps/${id}/settings`, "Settings")}
      </div>
      <Routes>
        <Route index element={<Overview app={a} user={user} />} />
        <Route path="logs" element={<LogsTab app={a} />} />
        <Route path="env" element={<EnvTab app={a} />} />
        <Route path="domains" element={<DomainsTab app={a} user={user} />} />
        <Route path="settings" element={<SettingsTab app={a} />} />
      </Routes>
    </>
  );
}

function DeployBadge({ status }: { status: Deployment["status"] }) {
  const cls = status === "succeeded" ? "ok" : status === "running" ? "warn" : "err";
  return <span className={`badge ${cls}`}>{status === "running" ? "deploying" : status}</span>;
}

function Overview({ app, user }: { app: App; user: User }) {
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
            <button className="btn primary" onClick={() => deploy.mutate(undefined)} disabled={deploy.isPending || running}>
              <Icon name="rocket" /> {running ? "Deploying…" : "Deploy"}
            </button>
          )}
        </div>
        <p className="faint small" style={{ marginBottom: 0 }}>
          A deployment pulls <span className="mono">{app.image}</span>, starts a new container and switches traffic once
          it is ready. If the new container fails, the current one keeps serving.
        </p>
      </div>

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
                  <th>Image</th>
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
                    <td className="mono muted small" title={d.image_digest}>
                      {d.image}
                    </td>
                    <td className="muted">{formatDate(d.created_at)}</td>
                    <td className="num" onClick={(e) => e.stopPropagation()}>
                      {can(user, "member") && d.status === "succeeded" && d.id !== current && (
                        <button
                          className="btn sm"
                          disabled={running}
                          onClick={async () => {
                            const ok = await confirm({
                              title: `Roll back to #${d.id}?`,
                              body: <p>The image of deployment #{d.id} is deployed again with the app's current settings and environment.</p>,
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
      {shown !== null && <DeploymentModal appId={app.id} id={shown} onClose={() => setShown(null)} />}
    </div>
  );
}

function DeploymentModal({ appId, id, onClose }: { appId: number; id: number; onClose: () => void }) {
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
        </div>
      )}
    </Modal>
  );
}

// LogView shows log text and keeps it scrolled to the end unless the user
// scrolled up to read.
function LogView({ text, tall }: { text: string; tall?: boolean }) {
  const ref = useRef<HTMLPreElement>(null);
  const stick = useRef(true);
  useLayoutEffect(() => {
    const el = ref.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [text]);
  return (
    <pre
      ref={ref}
      className={tall ? "logbox tall" : "logbox"}
      onScroll={(e) => {
        const el = e.currentTarget;
        stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
      }}
    >
      {text}
    </pre>
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
  const [tail, setTail] = useState(200);
  const [live, setLive] = useState(true);
  const [text, setText] = useState("");
  const [ended, setEnded] = useState<string | null>(null);
  const snapshot = useQuery({
    queryKey: ["app-logs", app.id, tail],
    queryFn: () => api.appLogs(app.id, tail),
    enabled: !live,
    retry: false,
  });
  const state = useEventStream(live ? api.appLogsStreamUrl(app.id, tail) : null, {
    open: () => {
      setText(""); // a (re)connected stream starts with the last lines again
      setEnded(null);
    },
    log: (c: { text: string }) => setText((t) => trimLog(t + c.text)),
    end: (d: { error?: string } | null) => setEnded(d?.error || "the stream ended"),
  });
  return (
    <div className="card">
      <div className="card-head">
        <div className="row">
          <h2>Container output</h2>
          {live && <StreamBadge state={state} ended={ended} />}
        </div>
        <div className="row">
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
      <Field label="One per line, KEY=value" help="Stored encrypted. Changes take effect on the next deployment.">
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
        <Field label="One per line" help={app.port ? `Requests are proxied to port ${app.port} of the container.` : "Set an HTTP port in Settings first."}>
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

function SettingsTab({ app }: { app: App }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [image, setImage] = useState(app.image);
  const [port, setPort] = useState(String(app.port));
  const [volumes, setVolumes] = useState(formatVolumes(app.volumes));
  const [memory, setMemory] = useState(app.memory_mb ? String(app.memory_mb) : "");
  const [cpus, setCpus] = useState(app.cpus ? String(app.cpus) : "");
  const save = useMutation({
    mutationFn: () =>
      api.updateApp(app.id, {
        image: image.trim(),
        port: Number(port) || 0,
        volumes: parseVolumes(volumes),
        memory_mb: Number(memory) || 0,
        cpus: Number(cpus) || 0,
      }),
    onSuccess: (r) => {
      qc.setQueryData(["app", app.id], (old: { app: App } | undefined) => (old ? { ...old, app: r.app } : old));
      qc.invalidateQueries({ queryKey: ["apps"] });
      toast("Saved. Deploy to apply.");
    },
  });
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
        <Field label="Image">
          <input className="input mono" value={image} onChange={(e) => setImage(e.target.value)} required />
        </Field>
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
        <div>
          <button className="btn primary" disabled={save.isPending}>
            Save
          </button>
        </div>
      </form>
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
  const del = useMutation({
    mutationFn: () => api.deleteApp(app.id, volumes),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["apps"] });
      toast(r.cleanup_error ? `App deleted; cleanup pending: ${r.cleanup_error}` : "App deleted", !!r.cleanup_error);
      navigate("/apps");
    },
  });
  return (
    <div className="card card-body stack">
      <h2>Delete app</h2>
      <p className="muted" style={{ margin: 0 }}>
        Its domains stop being served and its containers are removed. Persistent volumes are kept unless you choose to
        delete them.
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
                Delete app
              </button>
            </>
          }
        >
          <div className="form">
            <ErrorBox error={del.error} />
            {app.volumes.length > 0 && (
              <label className="check">
                <input type="checkbox" checked={volumes} onChange={(e) => setVolumes(e.target.checked)} /> Also delete its data
                volumes ({app.volumes.map((v) => v.name).join(", ")}) — this cannot be undone
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
