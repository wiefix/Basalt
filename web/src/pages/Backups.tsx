import { useEffect, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, can, type BackupCheck, type BackupStorage, type CheckRepository, type StorageInput, type User } from "../api";
import { CodeBox, ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatBytes, formatDate, timeAgo, useConfirm, useToast } from "../components/ui";
import { LogView } from "../components/LogView";
import { PolicyCard } from "./BackupsTab";

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
                  <td className="muted hide-sm">{s.policies ? `${s.policies} site${s.policies > 1 ? "s or apps" : " or app"}` : "—"}</td>
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
      {list.data && list.data.storages.length > 0 && <ChecksCard storages={list.data.storages} />}
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
      <PanelBackup user={user} />
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

const checkBadge: Record<BackupCheck["status"], [string, string]> = {
  running: ["badge warn", "checking"],
  succeeded: ["badge ok", "no errors"],
  failed: ["badge err", "failed"],
};

// ChecksCard lists the repositories holding backups with their integrity
// checks (restic check).
function ChecksCard({ storages }: { storages: BackupStorage[] }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [shown, setShown] = useState<number | null>(null);
  const repos = useQuery({
    queryKey: ["backup-checks"],
    queryFn: api.backupChecks,
    refetchInterval: (q) => (q.state.data?.repositories.some((r) => r.last?.status === "running") ? 3000 : 30_000),
  });
  const start = async (r: CheckRepository) => {
    try {
      const c = await api.startBackupCheck(r.storage_id, r.server_id);
      qc.invalidateQueries({ queryKey: ["backup-checks"] });
      setShown(c.check.id);
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  const storageName = (id: number) => storages.find((s) => s.id === id)?.name ?? `#${id}`;
  const list = repos.data?.repositories ?? [];
  return (
    <div className="card" style={{ marginTop: 16 }}>
      <div className="card-head">
        <h2>Integrity checks</h2>
      </div>
      {repos.isPending ? (
        <Loading />
      ) : repos.error ? (
        <div className="card-body">
          <ErrorBox error={repos.error} />
        </div>
      ) : list.length === 0 ? (
        <div className="card-body muted">Nothing to check yet: repositories are checked once backups were made to them.</div>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Storage</th>
                <th>Backups of</th>
                <th>Last check</th>
                <th className="hide-sm">Next</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.map((r) => (
                <tr key={`${r.storage_id}-${r.server_id}`}>
                  <td>
                    <strong>{storageName(r.storage_id)}</strong>
                  </td>
                  <td>{r.server_id ? r.server : <span className="muted">the panel</span>}</td>
                  <td className="small">
                    {r.last ? (
                      <button className="btn sm ghost" style={{ padding: 0, height: "auto" }} onClick={() => setShown(r.last!.id)}>
                        <span className={checkBadge[r.last.status][0]} title={r.last.error}>
                          {checkBadge[r.last.status][1]}
                        </span>{" "}
                        <span className="faint">{timeAgo(r.last.created_at)}</span>
                      </button>
                    ) : (
                      <span className="faint">never</span>
                    )}
                    {r.last?.status === "failed" && r.last.error && (
                      <div className="small" style={{ color: "var(--err)", overflowWrap: "anywhere" }}>
                        {r.last.error}
                      </div>
                    )}
                  </td>
                  <td className="muted small hide-sm">{r.next_at ? formatDate(r.next_at) : ""}</td>
                  <td className="num">
                    <button className="btn sm" disabled={r.last?.status === "running"} onClick={() => start(r)}>
                      <Icon name="check" size={14} /> Check now
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <div className="card-body">
        <p className="faint small" style={{ margin: 0 }}>
          Every week each repository's structure is checked and a fiftieth of its data is read back and decrypted, a different part
          each time, so all of it is read within a year. A failed check is tried again the next day and notified like a failed
          backup (see Notifications). Checks wait for backups of the same repository, and backups wait for them.
        </p>
      </div>
      {shown !== null && <IntegrityModal id={shown} onClose={() => setShown(null)} />}
    </div>
  );
}

function IntegrityModal({ id, onClose }: { id: number; onClose: () => void }) {
  const qc = useQueryClient();
  const d = useQuery({
    queryKey: ["backup-check", id],
    queryFn: () => api.backupCheck(id),
    refetchInterval: (q) => (q.state.data?.check.status === "running" ? 1500 : false),
  });
  const c = d.data?.check;
  const done = !!c && c.status !== "running";
  useEffect(() => {
    if (done) qc.invalidateQueries({ queryKey: ["backup-checks"] });
  }, [done, qc]);
  return (
    <Modal
      wide
      title={
        <span className="row">
          Integrity check #{id} {c && <span className={checkBadge[c.status][0]}>{checkBadge[c.status][1]}</span>}
        </span>
      }
      onClose={onClose}
    >
      {d.isPending ? (
        <Loading />
      ) : d.error ? (
        <ErrorBox error={d.error} />
      ) : (
        <div className="stack">
          {c?.error && <div className="alert err small">{c.error}</div>}
          <LogView text={c?.log || (c?.status === "running" ? "Running…" : "(no output)")} />
          {c?.read_data && <div className="faint small">Data part read: {c.read_data}</div>}
        </div>
      )}
    </Modal>
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

const panelTarget = { base: "/api/v1/panel-backup", name: "the panel", policyUrl: "/api/v1/panel-backup/policy", queryKey: ["panel-backup"] };

// PanelBackup backs up the panel's own database with its master key,
// encrypted with the owner's recovery passphrase.
function PanelBackup({ user }: { user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [shown, setShown] = useState<number | null>(null);
  const data = useQuery({
    queryKey: ["panel-backup"],
    queryFn: api.panelBackup,
    refetchInterval: (q) => (q.state.data?.backups.some((b) => b.status === "running") ? 2000 : 30_000),
  });
  const run = useMutation({
    mutationFn: api.runPanelBackup,
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["panel-backup"] });
      setShown(r.backup.id);
    },
    onError: (e) => toast(errorMessage(e), true),
  });
  if (data.isPending) return <Loading />;
  if (data.error) return <ErrorBox error={data.error} />;
  const d = data.data;
  const running = d.backups.some((b) => b.status === "running");
  return (
    <div className="stack" style={{ marginTop: 24 }}>
      <div className="page-head" style={{ marginBottom: 0 }}>
        <div>
          <h2>Panel</h2>
          <div className="sub">The panel's own database (users, servers, settings, secrets) and its encrypted master key.</div>
        </div>
        <button className="btn primary" onClick={() => run.mutate()} disabled={run.isPending || running || !d.policy || !d.recovery.set}>
          <Icon name="archive" /> {running ? "Running…" : "Back up now"}
        </button>
      </div>
      {d.restic_error && <div className="alert warn">{d.restic_error}</div>}
      <RecoveryCard user={user} recovery={d.recovery} />
      <PolicyCard target={panelTarget} policy={d.policy} />
      {d.backups.length > 0 && (
        <div className="card table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>#</th>
                <th>Status</th>
                <th className="hide-sm">Size</th>
                <th>Started</th>
              </tr>
            </thead>
            <tbody>
              {d.backups.map((b) => (
                <tr key={b.id} className="clickable" onClick={() => setShown(b.id)}>
                  <td className="mono">{b.id}</td>
                  <td>
                    <span className={`badge ${b.status === "succeeded" ? "ok" : b.status === "running" ? "warn" : "err"}`}>{b.status}</span>
                    {b.trigger === "schedule" && <span className="faint small"> · scheduled</span>}
                  </td>
                  <td className="muted hide-sm">{b.status === "succeeded" ? formatBytes(b.bytes) : ""}</td>
                  <td className="muted">{formatDate(b.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <div className="card card-body stack">
        <h2>Restoring the panel</h2>
        <ol className="steps muted small">
          <li>
            Install the panel on a new server: <code>sh install.sh panel --domain …</code>, then{" "}
            <code>systemctl stop basalt-server</code>.
          </li>
          <li>
            Fetch the latest backup: <code>restic -r &lt;storage&gt;/panel restore latest --target /root/restore</code> (with the
            storage's password).
          </li>
          <li>
            <code>basalt server recover --from /root/restore</code> — it asks for the recovery passphrase — then{" "}
            <code>systemctl start basalt-server</code>. Servers reconnect by themselves.
          </li>
        </ol>
        <p className="faint small" style={{ margin: 0 }}>
          Keep the storage's password and the recovery passphrase outside the panel: both are needed. A local storage for the
          panel must be under <code>/var/backups/basalt</code> on the panel's server.
        </p>
      </div>
      {shown !== null && <PanelRunModal id={shown} onClose={() => setShown(null)} />}
    </div>
  );
}

function RecoveryCard({ user, recovery }: { user: User; recovery: { set: boolean; set_at?: string } }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [editing, setEditing] = useState(false);
  const [password, setPassword] = useState("");
  const [phrase, setPhrase] = useState("");
  const [again, setAgain] = useState("");
  const save = useMutation({
    mutationFn: () => api.setRecoveryPassphrase(password, phrase),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["panel-backup"] });
      setEditing(false);
      setPassword("");
      setPhrase("");
      setAgain("");
      toast("Recovery passphrase set; it applies to the next backups");
    },
  });
  const owner = user.role === "owner";
  return (
    <div className="card card-body stack">
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <div>
          <h2>Recovery passphrase</h2>
          <p className="muted small" style={{ margin: "4px 0 0" }}>
            {recovery.set
              ? `Set ${recovery.set_at ? formatDate(recovery.set_at) : ""}. Backups carry the master key encrypted with it.`
              : "Not set: panel backups need it."}
          </p>
        </div>
        {owner && !editing && (
          <button className="btn" onClick={() => setEditing(true)}>
            <Icon name="key" /> {recovery.set ? "Change" : "Set passphrase"}
          </button>
        )}
      </div>
      {!owner && !recovery.set && <div className="alert warn small">Only the owner can set it.</div>}
      {editing && (
        <form
          className="form"
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            save.mutate();
          }}
        >
          <ErrorBox error={save.error} />
          <div className="alert warn small">
            The panel does not keep the passphrase. Write it down somewhere safe: without it, a panel backup cannot be restored.
            Changing it applies to new backups only.
          </div>
          <div className="grid-2">
            <Field label="Recovery passphrase" help="At least 12 characters; a few random words work well.">
              <input className="input" type="password" value={phrase} onChange={(e) => setPhrase(e.target.value)} required minLength={12} autoComplete="new-password" />
            </Field>
            <Field label="Repeat it">
              <input className="input" type="password" value={again} onChange={(e) => setAgain(e.target.value)} required autoComplete="new-password" />
            </Field>
          </div>
          <Field label="Your password" help="To confirm it is you.">
            <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoComplete="current-password" />
          </Field>
          <div className="row">
            <button className="btn primary" disabled={save.isPending || phrase !== again || phrase.length < 12}>
              Save
            </button>
            <button type="button" className="btn ghost" onClick={() => setEditing(false)}>
              Cancel
            </button>
            {again && phrase !== again && <span className="small" style={{ color: "var(--err)" }}>The passphrases differ.</span>}
          </div>
        </form>
      )}
    </div>
  );
}

function PanelRunModal({ id, onClose }: { id: number; onClose: () => void }) {
  const d = useQuery({
    queryKey: ["panel-backup-run", id],
    queryFn: () => api.panelBackupRun(id),
    refetchInterval: (q) => (q.state.data?.backup.status === "running" ? 1500 : false),
  });
  const run = d.data?.backup;
  return (
    <Modal wide title={`Panel backup #${id}`} onClose={onClose}>
      {d.isPending ? (
        <Loading />
      ) : d.error ? (
        <ErrorBox error={d.error} />
      ) : (
        <div className="stack">
          {run?.error && <div className="alert err small">{run.error}</div>}
          <LogView text={run?.log || (run?.status === "running" ? "Running…" : "(no output)")} />
        </div>
      )}
    </Modal>
  );
}
