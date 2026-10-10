import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, can, type BackupStorage, type StorageInput, type User } from "../api";
import { CodeBox, ErrorBox, Field, Icon, Loading, Modal, errorMessage, useConfirm, useToast } from "../components/ui";

const kindLabel = { s3: "S3", sftp: "SFTP", local: "Local directory" } as const;

export function storageWhere(s: BackupStorage) {
  if (s.kind === "s3") return `${s.endpoint}/${s.bucket}${s.path ? "/" + s.path : ""}`;
  if (s.kind === "sftp") return `${s.user}@${s.host}${s.port && s.port !== 22 ? ":" + s.port : ""}:${s.path}`;
  return s.path;
}

export function BackupsPage({ user }: { user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const list = useQuery({ queryKey: ["backup-storages"], queryFn: api.backupStorages, enabled: can(user, "admin") });
  const [editing, setEditing] = useState<BackupStorage | "new" | null>(null);
  const [checking, setChecking] = useState<BackupStorage | null>(null);
  const [password, setPassword] = useState<{ storage: BackupStorage; password: string } | null>(null);
  if (!can(user, "admin")) return <div className="empty">You do not have access to this page.</div>;

  const remove = async (s: BackupStorage) => {
    const ok = await confirm({
      title: `Remove storage ${s.name}?`,
      body: <p>The backups already in it are left there. Keep its password if you may need them.</p>,
      confirm: "Remove",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteBackupStorage(s.id);
      qc.invalidateQueries({ queryKey: ["backup-storages"] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  const reveal = async (s: BackupStorage) => {
    try {
      setPassword({ storage: s, password: (await api.backupStoragePassword(s.id)).password });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Backups</h1>
          <div className="sub">Storages for encrypted, deduplicated backups (restic). Choose what to back up on each site.</div>
        </div>
        <button className="btn primary" onClick={() => setEditing("new")}>
          <Icon name="plus" /> Add storage
        </button>
      </div>
      {list.isPending ? (
        <Loading />
      ) : list.error ? (
        <ErrorBox error={list.error} />
      ) : list.data.storages.length === 0 ? (
        <div className="card empty">
          <h3>No backup storage yet</h3>
          <p>Add an S3 bucket (AWS, Backblaze B2, Cloudflare R2, MinIO…), an SFTP server or a local directory.</p>
          <button className="btn primary" onClick={() => setEditing("new")}>
            <Icon name="plus" /> Add storage
          </button>
        </div>
      ) : (
        <div className="card table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Storage</th>
                <th>Kind</th>
                <th>Location</th>
                <th className="hide-sm">Used by</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.data.storages.map((s) => (
                <tr key={s.id}>
                  <td>
                    <strong>{s.name}</strong>
                  </td>
                  <td className="muted">{kindLabel[s.kind]}</td>
                  <td className="mono muted small">{storageWhere(s)}</td>
                  <td className="muted hide-sm">{s.policies ? `${s.policies} site${s.policies > 1 ? "s" : ""}` : "—"}</td>
                  <td className="num">
                    <div className="row" style={{ justifyContent: "flex-end" }}>
                      <button className="btn sm" onClick={() => setChecking(s)}>
                        <Icon name="check" size={14} /> Test
                      </button>
                      <button className="btn sm" onClick={() => reveal(s)} title="Needed to read the backups without the panel">
                        <Icon name="key" size={14} /> Password
                      </button>
                      <button className="btn sm icon ghost" title="Edit" onClick={() => setEditing(s)}>
                        <Icon name="edit" size={14} />
                      </button>
                      <button className="btn sm icon ghost" title="Remove" onClick={() => remove(s)}>
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
          Each server writes its own restic repository in the storage, under <code>server-&lt;id&gt;</code>. Backups are
          encrypted on the server before they leave it, and only changed data is uploaded. Site backups hold the files and
          a consistent dump of each database.
        </p>
        <p className="faint small" style={{ margin: 0 }}>
          Keep each storage's password somewhere safe outside the panel: with it, the backups can be restored with the
          restic command even if the panel is lost. A local directory protects against mistakes, not against losing the
          server.
        </p>
      </div>
      {editing && <StorageModal storage={editing === "new" ? null : editing} onClose={() => setEditing(null)} />}
      {checking && <CheckModal storage={checking} onClose={() => setChecking(null)} />}
      {password && (
        <Modal title={`Password of ${password.storage.name}`} onClose={() => setPassword(null)}>
          <div className="stack">
            <CodeBox text={password.password} />
            <p className="faint small" style={{ margin: 0 }}>
              To read a server's backups without the panel: <code>restic -r {"<location>"}/server-{"<id>"} snapshots</code>{" "}
              with this password.
            </p>
          </div>
        </Modal>
      )}
    </>
  );
}

function StorageModal({ storage, onClose }: { storage: BackupStorage | null; onClose: () => void }) {
  const qc = useQueryClient();
  const [kind, setKind] = useState<BackupStorage["kind"]>(storage?.kind ?? "s3");
  const [name, setName] = useState(storage?.name ?? "");
  const [endpoint, setEndpoint] = useState(storage?.endpoint ?? "https://");
  const [bucket, setBucket] = useState(storage?.bucket ?? "");
  const [region, setRegion] = useState(storage?.region ?? "");
  const [accessKey, setAccessKey] = useState(storage?.access_key ?? "");
  const [secretKey, setSecretKey] = useState("");
  const [host, setHost] = useState(storage?.host ?? "");
  const [port, setPort] = useState(String(storage?.port ?? 22));
  const [sftpUser, setSftpUser] = useState(storage?.user ?? "");
  const [path, setPath] = useState(storage?.path ?? (kind === "local" ? "/var/backups/basalt" : ""));
  const [created, setCreated] = useState<BackupStorage | null>(null);
  const save = useMutation({
    mutationFn: () => {
      const b: StorageInput = {
        name: name.trim(),
        kind,
        endpoint,
        bucket,
        region,
        access_key: accessKey,
        secret_key: secretKey,
        host,
        port: Number(port) || 22,
        user: sftpUser,
        path,
      };
      return storage ? api.updateBackupStorage(storage.id, b) : api.createBackupStorage(b);
    },
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["backup-storages"] });
      if (!storage && r.storage.kind === "sftp") setCreated(r.storage);
      else onClose();
    },
  });
  if (created) {
    return (
      <Modal title="Authorize the key" onClose={onClose} footer={<button className="btn primary" onClick={onClose}>Done</button>}>
        <div className="stack">
          <p className="muted" style={{ margin: 0 }}>
            Add this key to <code>~/.ssh/authorized_keys</code> of <span className="mono">{created.user}</span> on{" "}
            <span className="mono">{created.host}</span>, then test the storage. Its host key is accepted on the first
            connection.
          </p>
          <CodeBox text={created.ssh_public_key ?? ""} />
        </div>
      </Modal>
    );
  }
  return (
    <Modal
      title={storage ? `Edit ${storage.name}` : "Add storage"}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="storage-form" disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <form
        id="storage-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        {!storage && (
          <div className="segmented">
            {(["s3", "sftp", "local"] as const).map((k) => (
              <button
                key={k}
                type="button"
                className={kind === k ? "active" : ""}
                onClick={() => {
                  setKind(k);
                  if (k === "local" && !path) setPath("/var/backups/basalt");
                }}
              >
                {kindLabel[k]}
              </button>
            ))}
          </div>
        )}
        <Field label="Name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="offsite" required autoFocus />
        </Field>
        {kind === "s3" && (
          <>
            <Field label="Endpoint" help="e.g. https://s3.eu-central-1.amazonaws.com, https://s3.us-west-002.backblazeb2.com, https://<account>.r2.cloudflarestorage.com">
              <input className="input mono" value={endpoint} onChange={(e) => setEndpoint(e.target.value)} required />
            </Field>
            <div className="grid-2">
              <Field label="Bucket">
                <input className="input mono" value={bucket} onChange={(e) => setBucket(e.target.value)} required />
              </Field>
              <Field label="Region" help="Optional for most services.">
                <input className="input mono" value={region} onChange={(e) => setRegion(e.target.value)} placeholder="us-east-1" />
              </Field>
            </div>
            <div className="grid-2">
              <Field label="Access key">
                <input className="input mono" value={accessKey} onChange={(e) => setAccessKey(e.target.value)} required autoComplete="off" />
              </Field>
              <Field label="Secret key" help={storage ? "Leave empty to keep the current one." : "Stored encrypted."}>
                <input
                  className="input mono"
                  type="password"
                  value={secretKey}
                  onChange={(e) => setSecretKey(e.target.value)}
                  required={!storage}
                  autoComplete="new-password"
                />
              </Field>
            </div>
            <Field label="Path in the bucket" help="Optional prefix.">
              <input className="input mono" value={path} onChange={(e) => setPath(e.target.value)} placeholder="basalt" />
            </Field>
          </>
        )}
        {kind === "sftp" && (
          <>
            <div className="grid-2">
              <Field label="Host">
                <input className="input mono" value={host} onChange={(e) => setHost(e.target.value)} placeholder="backup.example.com" required />
              </Field>
              <Field label="Port">
                <input className="input mono" type="number" min={1} max={65535} value={port} onChange={(e) => setPort(e.target.value)} />
              </Field>
            </div>
            <div className="grid-2">
              <Field label="User">
                <input className="input mono" value={sftpUser} onChange={(e) => setSftpUser(e.target.value)} required />
              </Field>
              <Field label="Directory" help="Relative to the user's home unless it starts with /.">
                <input className="input mono" value={path} onChange={(e) => setPath(e.target.value)} placeholder="basalt" required />
              </Field>
            </div>
            {!storage && <p className="faint small" style={{ margin: 0 }}>A key is generated for this storage; you authorize it next.</p>}
          </>
        )}
        {kind === "local" && (
          <Field label="Directory on each server" help="Outside the sites and Basalt's own directories; created with access for root only.">
            <input className="input mono" value={path} onChange={(e) => setPath(e.target.value)} required />
          </Field>
        )}
        {storage && <p className="faint small" style={{ margin: 0 }}>Changing the location does not move backups already made.</p>}
      </form>
    </Modal>
  );
}

function CheckModal({ storage, onClose }: { storage: BackupStorage; onClose: () => void }) {
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const usable = servers.data?.servers.filter((s) => s.restic) ?? [];
  const [serverId, setServerId] = useState<number | "">("");
  const check = useMutation({ mutationFn: () => api.checkBackupStorage(storage.id, Number(serverId || usable[0]?.id)) });
  return (
    <Modal
      title={`Test ${storage.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Close
          </button>
          <button className="btn primary" onClick={() => check.mutate()} disabled={check.isPending || usable.length === 0}>
            {check.isPending ? "Testing…" : "Test"}
          </button>
        </>
      }
    >
      <div className="form">
        {servers.isPending ? (
          <Loading />
        ) : usable.length === 0 ? (
          <div className="alert warn">No server has restic. Re-run the installer on a server to add it.</div>
        ) : (
          <Field label="From server" help="The server connects to the storage and opens (or creates) its repository.">
            <select className="input" value={serverId || usable[0].id} onChange={(e) => setServerId(Number(e.target.value))}>
              {usable.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name} · restic {s.restic} {s.status !== "online" ? `(${s.status})` : ""}
                </option>
              ))}
            </select>
          </Field>
        )}
        <ErrorBox error={check.error} />
        {check.data && (check.data.ok ? <div className="alert ok">The repository is ready.</div> : <div className="alert err">{check.data.error}</div>)}
      </div>
    </Modal>
  );
}
