import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, can, type Registry, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, useConfirm, useToast } from "../components/ui";

export function RegistriesPage({ user }: { user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const list = useQuery({ queryKey: ["registries"], queryFn: api.registries, enabled: can(user, "admin") });
  const [editing, setEditing] = useState<Registry | "new" | null>(null);
  if (!can(user, "admin")) return <div className="empty">You do not have access to this page.</div>;

  const remove = async (r: Registry) => {
    const ok = await confirm({
      title: `Remove credentials for ${r.host}?`,
      body: <p>Apps with private images from {r.host} cannot be deployed until credentials are added again. Running apps keep running.</p>,
      confirm: "Remove",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteRegistry(r.id);
      qc.invalidateQueries({ queryKey: ["registries"] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Registries</h1>
          <div className="sub">Credentials for private images. They are used automatically for images from that registry.</div>
        </div>
        <button className="btn primary" onClick={() => setEditing("new")}>
          <Icon name="plus" /> Add registry
        </button>
      </div>
      {list.isPending ? (
        <Loading />
      ) : list.error ? (
        <ErrorBox error={list.error} />
      ) : list.data.registries.length === 0 ? (
        <div className="card empty">
          <h3>No registry credentials</h3>
          <p>Public images need none. Add credentials to deploy private images from Docker Hub, GitHub, GitLab or your own registry.</p>
        </div>
      ) : (
        <div className="card table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Registry</th>
                <th>Username</th>
                <th>Updated</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.data.registries.map((r) => (
                <tr key={r.id}>
                  <td className="mono">{r.host}</td>
                  <td className="mono">{r.username}</td>
                  <td className="muted">{formatDate(r.updated_at)}</td>
                  <td className="num">
                    <div className="row" style={{ justifyContent: "flex-end" }}>
                      <button className="btn sm" onClick={() => setEditing(r)}>
                        <Icon name="key" size={14} /> Change
                      </button>
                      <button className="btn sm icon ghost" title="Remove" onClick={() => remove(r)}>
                        <Icon name="trash" size={14} />
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <div className="card card-body stack" style={{ marginTop: 16 }}>
        <h2>How it works</h2>
        <p className="muted" style={{ margin: 0 }}>
          The registry is taken from the image name: <code>ghcr.io/org/app:1</code> uses <code>ghcr.io</code>, a name
          without a registry such as <code>org/app:1</code> uses Docker Hub (<code>docker.io</code>). Passwords and
          tokens are stored encrypted, sent to the server only for the pull, and never shown again.
        </p>
        <p className="faint small" style={{ margin: 0 }}>
          Prefer read-only tokens: a Docker Hub access token with read access, a GitHub token with{" "}
          <code>read:packages</code>, a GitLab deploy token with <code>read_registry</code>.
        </p>
      </div>
      {editing && <RegistryModal registry={editing === "new" ? null : editing} onClose={() => setEditing(null)} />}
    </>
  );
}

function RegistryModal({ registry, onClose }: { registry: Registry | null; onClose: () => void }) {
  const qc = useQueryClient();
  const [host, setHost] = useState(registry?.host ?? "");
  const [username, setUsername] = useState(registry?.username ?? "");
  const [password, setPassword] = useState("");
  const save = useMutation({
    mutationFn: () =>
      registry
        ? api.updateRegistry(registry.id, { username, password })
        : api.createRegistry({ host, username, password }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["registries"] });
      onClose();
    },
  });
  return (
    <Modal
      title={registry ? `Credentials for ${registry.host}` : "Add registry"}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="registry-form" disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <form
        id="registry-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        {!registry && (
          <Field label="Registry" help="Host name as in image names: docker.io, ghcr.io, registry.gitlab.com, registry.example.com:5000">
            <input className="input mono" value={host} onChange={(e) => setHost(e.target.value)} placeholder="ghcr.io" required autoFocus />
          </Field>
        )}
        <Field label="Username">
          <input className="input mono" value={username} onChange={(e) => setUsername(e.target.value)} required autoComplete="off" autoFocus={!!registry} />
        </Field>
        <Field label="Password or access token" help={registry ? "Enter it again to change the credentials." : "Stored encrypted; it is not shown again."}>
          <input className="input mono" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoComplete="new-password" />
        </Field>
      </form>
    </Modal>
  );
}
