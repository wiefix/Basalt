import { useEffect, useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import { api, can, type BackupItem, type BackupPolicy, type BackupRun, type Snapshot, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatBytes, formatDate, useConfirm, useToast } from "../components/ui";
import { useEventStream } from "../components/stream";
import { LogView } from "../components/LogView";

const pad = (n: number) => String(n).padStart(2, "0");
const minuteText = (m: number) => `${pad(Math.floor(m / 60))}:${pad(m % 60)}`;
const parseMinute = (t: string) => {
  const [h, m] = t.split(":").map(Number);
  return (h || 0) * 60 + (m || 0);
};

function RunBadge({ run }: { run: BackupRun }) {
  const cls = run.status === "succeeded" ? "ok" : run.status === "running" ? "warn" : "err";
  return <span className={`badge ${cls}`}>{run.status === "running" ? (run.kind === "restore" ? "restoring" : "running") : run.status}</span>;
}

const itemLabel = (it: Pick<BackupItem, "kind" | "database" | "volume">) =>
  it.kind === "site_files" ? "files" : it.kind === "volume" ? `volume ${it.volume}` : `database ${it.database}`;

// What a backup holds, in a sentence.
function contents(items: BackupItem[]) {
  if (items.length === 0) return "Nothing to back up: it keeps no data (no volumes).";
  return `Holds ${items.map(itemLabel).join(", ")}${items.some((i) => i.kind === "mariadb" || i.kind === "db_dump") ? " (consistent dumps)" : ""}.`;
}

// BackupTarget is the site or app a Backups tab is about.
export interface BackupTarget {
  base: string; // API path, e.g. /api/v1/sites/1
  name: string;
  policyUrl?: string; // default base + "/backup-policy"
  queryKey?: unknown[]; // refreshed when the policy changes; default ["backups", base]
}

const policyUrl = (t: BackupTarget) => t.policyUrl ?? `${t.base}/backup-policy`;
const targetKey = (t: BackupTarget) => t.queryKey ?? ["backups", t.base];

export function BackupsTab({ target, user }: { target: BackupTarget; user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const [shown, setShown] = useState<number | null>(null);
  const [restoring, setRestoring] = useState(false);
  const key = ["backups", target.base];
  const data = useQuery({ queryKey: key, queryFn: () => api.backups(target.base), refetchInterval: 15_000 });
  const running = data.data?.backups.find((b) => b.status === "running");
  useEventStream(running ? api.backupStreamUrl(target.base, running.id) : null, {
    end: () => qc.invalidateQueries({ queryKey: key }),
  });
  const run = useMutation({
    mutationFn: () => api.runBackup(target.base),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: key });
      setShown(r.backup.id);
    },
    onError: (e) => toast(errorMessage(e), true),
  });
  if (data.isPending) return <Loading />;
  if (data.error) return <ErrorBox error={data.error} />;
  const { policy, backups, restic, items } = data.data;

  return (
    <div className="stack">
      {!restic && (
        <div className="alert warn">
          restic is not installed on this server, so it cannot make backups. Re-run the installer on it (it adds restic), or
          install restic 0.17 or newer.
        </div>
      )}
      <div className="card card-body">
        <div className="row wrap" style={{ justifyContent: "space-between" }}>
          <div>
            <h2>Backups</h2>
            <p className="muted small" style={{ margin: "4px 0 0" }}>
              {policy
                ? policy.schedule === "manual"
                  ? "Only when started by hand."
                  : `${policy.schedule === "daily" ? "Every day" : "Every hour"}; next ${policy.next_run ? formatDate(policy.next_run) : "—"}.`
                : "Not backed up yet: choose a storage below."}{" "}
              {contents(items)}
            </p>
          </div>
          <div className="row">
            {policy && can(user, "member") && (
              <button className="btn primary" onClick={() => run.mutate()} disabled={run.isPending || !!running || !restic || items.length === 0}>
                <Icon name="archive" /> {running ? "Running…" : "Back up now"}
              </button>
            )}
            {policy && can(user, "admin") && (
              <button className="btn" onClick={() => setRestoring(true)} disabled={!!running}>
                <Icon name="refresh" /> Restore
              </button>
            )}
          </div>
        </div>
      </div>

      {can(user, "admin") && <PolicyCard target={target} policy={policy} />}

      <div className="card">
        <div className="card-head">
          <h2>History</h2>
        </div>
        {backups.length === 0 ? (
          <div className="empty small">No backups yet.</div>
        ) : (
          <div className="table-wrap">
            <table className="table">
              <thead>
                <tr>
                  <th>#</th>
                  <th>Status</th>
                  <th>What</th>
                  <th className="hide-sm">Size</th>
                  <th>Started</th>
                </tr>
              </thead>
              <tbody>
                {backups.map((b) => (
                  <tr key={b.id} className={can(user, "member") ? "clickable" : ""} onClick={() => can(user, "member") && setShown(b.id)}>
                    <td className="mono">{b.id}</td>
                    <td>
                      <RunBadge run={b} />
                    </td>
                    <td className="muted small">
                      {b.kind === "restore" ? "restore: " : b.trigger === "schedule" ? "scheduled: " : ""}
                      {b.snapshots.map((s) => itemLabel(s.item)).join(", ") || "—"}
                    </td>
                    <td className="muted hide-sm small">
                      {b.kind === "backup" && b.status === "succeeded" ? `${formatBytes(b.bytes)} · ${formatBytes(b.data_added)} new` : ""}
                    </td>
                    <td className="muted">{formatDate(b.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {shown !== null && <RunModal target={target} id={shown} onClose={() => setShown(null)} />}
      {restoring && (
        <RestoreModal
          target={target}
          onClose={() => setRestoring(false)}
          onStarted={(id) => {
            setRestoring(false);
            qc.invalidateQueries({ queryKey: key });
            setShown(id);
          }}
        />
      )}
    </div>
  );
}

export function PolicyCard({ target, policy }: { target: BackupTarget; policy: BackupPolicy | null }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const storages = useQuery({ queryKey: ["backup-storages"], queryFn: api.backupStorages });
  const [storageId, setStorageId] = useState<number | "">(policy?.storage_id ?? "");
  const [schedule, setSchedule] = useState<BackupPolicy["schedule"]>(policy?.schedule ?? "daily");
  const [time, setTime] = useState(minuteText(policy?.at_minute ?? 180));
  const [keep, setKeep] = useState({
    last: policy?.keep_last ?? 3,
    daily: policy?.keep_daily ?? 7,
    weekly: policy?.keep_weekly ?? 4,
    monthly: policy?.keep_monthly ?? 6,
  });
  useEffect(() => {
    if (storageId === "" && storages.data?.storages.length) setStorageId(storages.data.storages[0].id);
  }, [storages.data, storageId]);
  const save = useMutation({
    mutationFn: () =>
      api.setBackupPolicy(policyUrl(target), {
        storage_id: Number(storageId),
        schedule,
        at_minute: schedule === "hourly" ? parseMinute(time) % 60 : parseMinute(time),
        keep_last: keep.last,
        keep_daily: keep.daily,
        keep_weekly: keep.weekly,
        keep_monthly: keep.monthly,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: targetKey(target) });
      toast("Backup settings saved");
    },
  });
  const off = async () => {
    const ok = await confirm({
      title: `Stop backing up ${target.name}?`,
      body: <p>Existing backups stay in the storage; the schedule stops.</p>,
      confirm: "Stop",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteBackupPolicy(policyUrl(target));
      qc.invalidateQueries({ queryKey: targetKey(target) });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  if (storages.isPending) return <Loading />;
  if (storages.error) return <ErrorBox error={storages.error} />;
  if (storages.data.storages.length === 0) {
    return (
      <div className="card card-body">
        <p className="muted" style={{ margin: 0 }}>
          Add a backup storage under <Link to="/backups">Backups</Link> first.
        </p>
      </div>
    );
  }
  const num = (k: keyof typeof keep, label: string) => (
    <Field label={label}>
      <input
        className="input mono"
        type="number"
        min={k === "last" ? 1 : 0}
        max={1000}
        value={keep[k]}
        onChange={(e) => setKeep({ ...keep, [k]: Number(e.target.value) })}
      />
    </Field>
  );
  const utcNow = new Date();
  return (
    <form
      className="card card-body form"
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        save.mutate();
      }}
    >
      <h2>Settings</h2>
      <ErrorBox error={save.error} />
      <div className="grid-2">
        <Field label="Storage">
          <select className="input" value={storageId} onChange={(e) => setStorageId(Number(e.target.value))}>
            {storages.data.storages.map((s) => (
              <option key={s.id} value={s.id}>
                {s.name}
              </option>
            ))}
          </select>
        </Field>
        <Field
          label="Schedule"
          help={
            schedule === "manual"
              ? "Backups run only when started."
              : `Time in UTC (now ${pad(utcNow.getUTCHours())}:${pad(utcNow.getUTCMinutes())} UTC). A backup missed while the server was offline runs when it is back.`
          }
        >
          <div className="row">
            <select className="input" style={{ width: "auto" }} value={schedule} onChange={(e) => setSchedule(e.target.value as BackupPolicy["schedule"])}>
              <option value="daily">Daily at</option>
              <option value="hourly">Hourly at minute</option>
              <option value="manual">Manual only</option>
            </select>
            {schedule === "daily" && <input className="input mono" style={{ width: 110 }} type="time" value={time} onChange={(e) => setTime(e.target.value)} />}
            {schedule === "hourly" && (
              <input
                className="input mono"
                style={{ width: 80 }}
                type="number"
                min={0}
                max={59}
                value={parseMinute(time) % 60}
                onChange={(e) => setTime(minuteText(Math.min(59, Math.max(0, Number(e.target.value)))))}
              />
            )}
          </div>
        </Field>
      </div>
      <div className="grid-4">
        {num("last", "Keep last")}
        {num("daily", "Daily")}
        {num("weekly", "Weekly")}
        {num("monthly", "Monthly")}
      </div>
      <p className="faint small" style={{ margin: 0 }}>
        After each backup, older ones are removed except the last N, and the newest of each of the last days, weeks and
        months.
      </p>
      <div className="row">
        <button className="btn primary" disabled={save.isPending || storageId === ""}>
          Save
        </button>
        {policy && (
          <button type="button" className="btn ghost" onClick={off}>
            Stop backups
          </button>
        )}
      </div>
    </form>
  );
}

function RunModal({ target, id, onClose }: { target: BackupTarget; id: number; onClose: () => void }) {
  const qc = useQueryClient();
  const d = useQuery({ queryKey: ["backup", target.base, id], queryFn: () => api.backup(target.base, id) });
  const run = d.data?.backup;
  const following = run?.status === "running";
  const [live, setLive] = useState("");
  useEventStream(following ? api.backupStreamUrl(target.base, id) : null, {
    open: () => setLive(""),
    log: (c: { text: string }) => setLive((t) => t + c.text),
    end: () => {
      qc.invalidateQueries({ queryKey: ["backup", target.base, id] });
      qc.invalidateQueries({ queryKey: ["backups", target.base] });
    },
  });
  const text = following ? live : run?.log;
  return (
    <Modal
      wide
      onClose={onClose}
      title={
        <span className="row">
          {run?.kind === "restore" ? "Restore" : "Backup"} #{id} {run && <RunBadge run={run} />}
        </span>
      }
    >
      {d.isPending ? (
        <Loading />
      ) : d.error ? (
        <ErrorBox error={d.error} />
      ) : (
        <div className="stack">
          {run?.error && <div className="alert err small">{run.error}</div>}
          <LogView text={text || (following ? "Waiting for the server…" : "(no output)")} />
        </div>
      )}
    </Modal>
  );
}

function RestoreModal({ target, onClose, onStarted }: { target: BackupTarget; onClose: () => void; onStarted: (id: number) => void }) {
  const snaps = useQuery({ queryKey: ["snapshots", target.base], queryFn: () => api.snapshots(target.base), gcTime: 0 });
  const [chosen, setChosen] = useState<Record<string, string>>({}); // part -> snapshot id
  const [confirmText, setConfirmText] = useState("");
  const restore = useMutation({
    mutationFn: () => api.restore(target.base, Object.values(chosen)),
    onSuccess: (r) => onStarted(r.backup.id),
  });
  const parts = new Map<string, Snapshot[]>();
  for (const s of snaps.data?.snapshots ?? []) {
    const key = itemLabel(s.item);
    parts.set(key, [...(parts.get(key) ?? []), s]);
  }
  const count = Object.keys(chosen).length;
  return (
    <Modal
      wide
      title="Restore from a backup"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn danger solid" disabled={count === 0 || confirmText !== target.name || restore.isPending} onClick={() => restore.mutate()}>
            {count ? `Restore ${count} part${count === 1 ? "" : "s"}` : "Restore"}
          </button>
        </>
      }
    >
      {snaps.isPending ? (
        <Loading />
      ) : snaps.error ? (
        <ErrorBox error={snaps.error} />
      ) : parts.size === 0 ? (
        <div className="empty small">No backups in the storage yet.</div>
      ) : (
        <div className="form">
          <ErrorBox error={restore.error} />
          <div className="alert warn small">
            Restored files and volumes replace the current ones exactly: files created since are deleted, and containers using
            a volume stop while it is restored. A restored database replaces the tables of the dump.
          </div>
          {[...parts.entries()].map(([part, list]) => (
            <Field key={part} label={part[0].toUpperCase() + part.slice(1)}>
              <select className="input" value={chosen[part] ?? ""} onChange={(e) => setChosen(cleanChoices({ ...chosen, [part]: e.target.value }))}>
                <option value="">Keep as it is</option>
                {list.map((s) => (
                  <option key={s.id} value={s.id}>
                    {formatDate(s.time)} · {formatBytes(s.bytes)} · {s.id.slice(0, 8)}
                  </option>
                ))}
              </select>
            </Field>
          ))}
          <Field label={`Type ${target.name} to confirm`}>
            <input className="input" value={confirmText} onChange={(e) => setConfirmText(e.target.value)} />
          </Field>
        </div>
      )}
    </Modal>
  );
}

// cleanChoices drops parts left as they are.
function cleanChoices(c: Record<string, string>) {
  return Object.fromEntries(Object.entries(c).filter(([, v]) => v));
}
