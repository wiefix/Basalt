import { useMemo, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { api, can, type App, type Template, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal } from "../components/ui";

export function CatalogPage({ user }: { user: User }) {
  const catalog = useQuery({ queryKey: ["catalog"], queryFn: api.catalog });
  const [filter, setFilter] = useState("");
  const [category, setCategory] = useState("");
  const [installing, setInstalling] = useState<Template | null>(null);
  const admin = can(user, "admin");
  const templates = catalog.data?.templates ?? [];
  const categories = useMemo(() => [...new Set(templates.map((t) => t.category))].sort(), [templates]);
  const shown = templates.filter(
    (t) =>
      (!category || t.category === category) &&
      (t.name + " " + t.description).toLowerCase().includes(filter.trim().toLowerCase()),
  );

  if (catalog.isPending) return <Loading />;
  if (catalog.error) return <ErrorBox error={catalog.error} />;
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Catalog</h1>
          <div className="sub">Apps installed in one click, each with its own network, volumes and HTTPS domain.</div>
        </div>
      </div>
      <div className="row wrap" style={{ marginBottom: 16 }}>
        <input className="input" style={{ maxWidth: 280 }} placeholder="Search" value={filter} onChange={(e) => setFilter(e.target.value)} />
        <div className="segmented" style={{ flexWrap: "wrap" }}>
          <button className={category ? "" : "active"} onClick={() => setCategory("")}>
            All
          </button>
          {categories.map((c) => (
            <button key={c} className={category === c ? "active" : ""} onClick={() => setCategory(c)}>
              {c}
            </button>
          ))}
        </div>
      </div>
      {shown.length === 0 ? (
        <div className="card empty">
          <h3>Nothing found</h3>
        </div>
      ) : (
        <div className="catalog-grid">
          {shown.map((t) => (
            <div key={t.id} className="card card-body stack catalog-card">
              <div className="row">
                <div className="catalog-logo" aria-hidden>
                  {t.name.slice(0, 1)}
                </div>
                <div style={{ minWidth: 0 }}>
                  <h3 style={{ margin: 0 }}>{t.name}</h3>
                  <div className="faint small">
                    {t.category} · {t.version}
                  </div>
                </div>
              </div>
              <p className="muted small" style={{ margin: 0, flex: 1 }}>
                {t.description}
              </p>
              <div className="row wrap" style={{ justifyContent: "space-between" }}>
                <span className="faint small">
                  {t.services.length > 1 ? `${t.services.length} services` : "1 service"}
                  {t.memory_mb ? ` · ${t.memory_mb >= 1024 ? `${t.memory_mb / 1024} GB` : `${t.memory_mb} MB`} RAM` : ""}
                </span>
                {admin && (
                  <button className="btn sm primary" onClick={() => setInstalling(t)}>
                    Install
                  </button>
                )}
              </div>
            </div>
          ))}
        </div>
      )}
      {installing && <InstallModal template={installing} onClose={() => setInstalling(null)} />}
    </>
  );
}

function InstallModal({ template: t, onClose }: { template: Template; onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const withDocker = servers.data?.servers.filter((s) => s.docker) ?? [];
  const [serverId, setServerId] = useState<number | "">("");
  const [name, setName] = useState(t.id);
  const [inputs, setInputs] = useState<Record<string, string>>(() =>
    Object.fromEntries(t.inputs.map((i) => [i.key, i.default ?? ""])),
  );
  const [showCompose, setShowCompose] = useState(false);
  const [done, setDone] = useState<{ app: App; notes: string; deploy_error?: string } | null>(null);
  const install = useMutation({
    mutationFn: () =>
      api.installTemplate(t.id, { server_id: Number(serverId || withDocker[0]?.id), name: name.trim(), inputs }),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["apps"] });
      setDone(r);
    },
  });

  if (done) {
    return (
      <Modal
        title={`${t.name} is being installed`}
        onClose={onClose}
        footer={
          <button className="btn primary" onClick={() => navigate(`/apps/${done.app.id}`)}>
            Open the app
          </button>
        }
      >
        <div className="stack">
          {done.deploy_error && <div className="alert err">The deployment did not start: {done.deploy_error}</div>}
          <p style={{ margin: 0 }}>
            The images are being pulled and the app started; follow it on the app's page. It will answer at{" "}
            <a href={`https://${done.app.domains[0]}`} target="_blank" rel="noreferrer" className="mono">
              {done.app.domains[0]}
            </a>{" "}
            once its domain points to the server.
          </p>
          {done.notes && <div className="alert">{done.notes}</div>}
          <p className="faint small" style={{ margin: 0 }}>
            Generated passwords and keys are under the app's Environment.
          </p>
        </div>
      </Modal>
    );
  }

  return (
    <Modal
      title={`Install ${t.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="install-form" disabled={install.isPending || withDocker.length === 0}>
            Install
          </button>
        </>
      }
    >
      <form
        id="install-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          install.mutate();
        }}
      >
        <ErrorBox error={install.error} />
        {servers.data && withDocker.length === 0 && (
          <div className="alert warn">No server has Docker yet: install it on one first (installer option --with-docker).</div>
        )}
        <Field label="Server">
          <select className="input" value={serverId} onChange={(e) => setServerId(Number(e.target.value) || "")}>
            {withDocker.map((s) => (
              <option key={s.id} value={s.id}>
                {s.name}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Name" help="The app's name in Basalt.">
          <input className="input mono" value={name} onChange={(e) => setName(e.target.value)} required />
        </Field>
        {t.inputs.map((i) => (
          <Field
            key={i.key}
            label={i.label}
            help={
              i.help ??
              (i.type === "domain" ? "HTTPS is set up automatically; point the domain to the server (automatic if its zone is under DNS)." : undefined)
            }
          >
            <input
              className="input mono"
              type={i.type === "email" ? "email" : "text"}
              value={inputs[i.key] ?? ""}
              onChange={(e) => setInputs({ ...inputs, [i.key]: e.target.value })}
              placeholder={i.type === "domain" ? `${t.id}.example.com` : undefined}
              required={i.required || i.type === "domain"}
            />
          </Field>
        ))}
        <div className="faint small">
          Runs {t.services.map((s) => s.image.replace(/@sha256:.*/, "")).join(", ")}
          {t.volumes?.length ? `, data in volume${t.volumes.length > 1 ? "s" : ""} ${t.volumes.join(", ")}` : ""}.
          {t.secrets?.length ? ` Passwords are generated (${t.secrets.map((s) => s.key).join(", ")}).` : ""}{" "}
          {t.memory_mb ? `Needs about ${t.memory_mb} MB of memory.` : ""}{" "}
          <button type="button" className="btn sm ghost" onClick={() => setShowCompose(!showCompose)}>
            <Icon name="file" size={14} /> {showCompose ? "Hide" : "Show"} the compose file
          </button>
          {t.website && (
            <a href={t.website} target="_blank" rel="noreferrer" className="btn sm ghost">
              Website
            </a>
          )}
        </div>
        {showCompose && <pre className="logbox">{t.compose}</pre>}
        <div className="faint small">
          Installed apps appear under <Link to="/apps">Apps</Link>, with deployments, logs and backups.
        </div>
      </form>
    </Modal>
  );
}
