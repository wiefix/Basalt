import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import { api, can, type App, type AppTemplateInfo, type User } from "../api";
import { ErrorBox, Field, Icon, Modal } from "../components/ui";

// imageTag shortens an image reference to name:tag.
const imageTag = (ref?: string) => (ref ? ref.replace(/@sha256:.*/, "") : "");

// TemplateCard shows the catalog template an app was installed from and
// offers its updates.
export function TemplateCard({ app, user }: { app: App; user: User }) {
  const info = useQuery({
    queryKey: ["app-template", app.id],
    queryFn: () => api.appTemplate(app.id),
    refetchInterval: (q) => (q.state.data?.update?.state === "backing_up" ? 2_000 : 60_000),
  });
  const [updating, setUpdating] = useState(false);
  const d = info.data;
  if (!d) return null;
  const t = d.template;
  return (
    <div className="card card-body stack">
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <div className="row wrap">
          <h2>{t?.name ?? app.template}</h2>
          <span className="faint small">from the catalog · template revision {d.installed_revision}</span>
          {d.update_available && <span className="badge warn">update: {t?.version}</span>}
        </div>
        {d.update_available && can(user, "admin") && d.update?.state !== "backing_up" && (
          <button className="btn primary" onClick={() => setUpdating(true)}>
            <Icon name="up" /> Update to {t?.version}
          </button>
        )}
      </div>
      {!t && <div className="faint small">The catalog no longer has this app: it keeps running as it is.</div>}
      {d.update?.state === "backing_up" && (
        <div className="alert">
          <span className="spinner" /> Backing up before the update… The new version is deployed when the backup
          succeeds (see <Link to={`/apps/${app.id}/backups`}>Backups</Link>).
        </div>
      )}
      {d.update?.state === "failed" && <div className="alert err">The update did not complete: {d.update.error}</div>}
      {t?.notes && <div className="faint small" style={{ whiteSpace: "pre-line" }}>{t.notes}</div>}
      {updating && <UpdateModal app={app} info={d} onClose={() => setUpdating(false)} />}
    </div>
  );
}

function UpdateModal({ app, info, onClose }: { app: App; info: AppTemplateInfo; onClose: () => void }) {
  const qc = useQueryClient();
  const [inputs, setInputs] = useState<Record<string, string>>({});
  const [backup, setBackup] = useState(info.backups);
  const update = useMutation({
    mutationFn: () => api.updateTemplate(app.id, { inputs, backup }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["app-template", app.id] });
      qc.invalidateQueries({ queryKey: ["app", app.id] });
      qc.invalidateQueries({ queryKey: ["deployments", app.id] });
      qc.invalidateQueries({ queryKey: ["backups"] });
      onClose();
    },
  });
  const t = info.template!;
  return (
    <Modal
      title={`Update ${app.name} to ${t.name} ${t.version}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="template-update-form" disabled={update.isPending}>
            Update
          </button>
        </>
      }
    >
      <form
        id="template-update-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          update.mutate();
        }}
      >
        <ErrorBox error={update.error} />
        {info.images && info.images.length > 0 ? (
          <div className="table-wrap">
            <table className="table">
              <tbody>
                {info.images.map((c) => (
                  <tr key={c.service}>
                    <td>{c.service}</td>
                    <td className="mono small" style={{ overflowWrap: "anywhere" }}>
                      {c.from ? imageTag(c.from) : "new service"} → {c.to ? imageTag(c.to) : "removed"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="faint small">The images stay the same; the template's settings change.</div>
        )}
        {info.needs?.map((i) => (
          <Field key={i.key} label={i.label} help={i.help ?? "New in this version."}>
            <input
              className="input mono"
              type={i.type === "email" ? "email" : "text"}
              value={inputs[i.key] ?? ""}
              onChange={(e) => setInputs({ ...inputs, [i.key]: e.target.value })}
              required={i.required}
            />
          </Field>
        ))}
        <label className="row small">
          <input type="checkbox" checked={backup} disabled={!info.backups} onChange={(e) => setBackup(e.target.checked)} />
          Back up the app's volumes first, and update only if the backup succeeds
        </label>
        {!info.backups && (
          <div className="faint small">
            To back up first, choose where under <Link to={`/apps/${app.id}/backups`}>Backups</Link>.
          </div>
        )}
        <div className="faint small">
          Your settings and generated passwords are kept; new ones are generated. A newer version may change its data
          in a way the old one cannot read: rolling back the deployment then needs the backup restored.
        </div>
      </form>
    </Modal>
  );
}
