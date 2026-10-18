import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { api, type RestoredCopy, type Snapshot, type SnapshotEntry } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatBytes, formatDate, useConfirm, useToast } from "../components/ui";

const join = (dir: string, name: string) => (dir ? `${dir}/${name}` : name);

// What a snapshot holds, for the list of backups.
const partLabel = (s: Snapshot) =>
  s.item.kind === "site_files"
    ? "Files"
    : s.item.kind === "volume"
      ? `Volume ${s.item.volume}`
      : `Database ${s.item.database} (dump)`;

const isDump = (s: Snapshot) => s.item.kind === "mariadb" || s.item.kind === "db_dump";

// SnapshotFilesModal browses the backups of a site or app: the site's files
// or an app's volumes (download a file, a folder as a zip, or restore a
// copy into a new folder, leaving the current files as they are), and the
// dumps of its databases (download them).
export function SnapshotFilesModal({
  base,
  siteId,
  canWrite,
  onClose,
}: {
  base: string; // API path of the site or app
  siteId?: number; // a site: restored copies open in its file manager
  canWrite: boolean;
  onClose: () => void;
}) {
  const toast = useToast();
  const confirm = useConfirm();
  const snaps = useQuery({ queryKey: ["snapshots", base], queryFn: () => api.snapshots(base), gcTime: 0 });
  const list = (snaps.data?.snapshots ?? []).filter((s) => ["site_files", "volume", "mariadb", "db_dump"].includes(s.item.kind));
  const [chosen, setChosen] = useState("");
  const snapshot = list.find((s) => s.id === chosen) ?? list[0];
  const snap = snapshot?.id ?? "";
  const volume = snapshot?.item.kind === "volume" ? (snapshot.item.volume ?? "") : "";
  const dump = !!snapshot && isDump(snapshot);
  const [path, setPath] = useState("");
  const [restored, setRestored] = useState<RestoredCopy | null>(null);
  const files = useQuery({
    queryKey: ["snapshot-files", base, snap, volume, path],
    queryFn: () => api.snapshotFiles(base, snap, path, volume),
    enabled: snap !== "" && !dump,
  });
  const copy = useMutation({
    mutationFn: (p: string) => api.restoreCopy(base, snap, p, volume),
    onSuccess: (r) => setRestored(r),
    onError: (e) => toast(errorMessage(e), true),
  });
  const where = volume ? `the volume ${volume}` : "the site";
  const restoreCopy = async (p: string, label: string) => {
    const ok = await confirm({
      title: `Restore a copy of ${label}?`,
      body: (
        <p>
          It is put into a new folder <span className="mono">restored-…</span> at the top of {where}. Current files stay as they
          are; move what you need from there. Symbolic links are left out
          {volume ? "; owners and permissions are kept" : ""}.
        </p>
      ),
      confirm: "Restore a copy",
    });
    if (ok) copy.mutate(p);
  };
  const crumbs = path.split("/").filter(Boolean);
  const label = path ? path.split("/").pop()! : volume ? `the whole volume ${volume}` : "the whole site";

  return (
    <Modal wide title="Browse backups" onClose={onClose}>
      {snaps.isPending ? (
        <Loading />
      ) : snaps.error ? (
        <ErrorBox error={snaps.error} />
      ) : list.length === 0 ? (
        <div className="empty small">No backups in the storage yet.</div>
      ) : (
        <div className="stack">
          <Field label="Backup">
            <select
              className="input"
              value={snap}
              onChange={(e) => {
                setChosen(e.target.value);
                setPath("");
                setRestored(null);
              }}
            >
              {list.map((s) => (
                <option key={s.id} value={s.id}>
                  {partLabel(s)} · {formatDate(s.time)} · {formatBytes(s.bytes)} · {s.id.slice(0, 8)}
                </option>
              ))}
            </select>
          </Field>
          {dump ? (
            <div className="stack">
              <p className="muted" style={{ margin: 0 }}>
                A consistent SQL dump of the database {snapshot.item.database}, {formatBytes(snapshot.bytes)}. It can be loaded
                into another database with the engine's client, or restored in place from the Restore dialog.
              </p>
              <div>
                <a className="btn primary" href={api.dumpDownloadUrl(base, snap, snapshot.item.database ?? "")}>
                  <Icon name="download" size={14} /> Download the dump
                </a>
              </div>
            </div>
          ) : (
            <>
              {restored && (
                <div className="alert ok small">
                  Restored {restored.files} file{restored.files === 1 ? "" : "s"} ({formatBytes(restored.bytes)}) into{" "}
                  {siteId !== undefined ? (
                    <Link to={`/sites/${siteId}?path=${encodeURIComponent("/" + restored.folder)}`} onClick={onClose}>
                      {restored.folder}
                    </Link>
                  ) : (
                    <>
                      <span className="mono">{restored.folder}</span> of the volume {volume}
                    </>
                  )}
                  {restored.skipped > 0 &&
                    `; ${restored.skipped} symbolic link${restored.skipped === 1 ? "" : "s"} or special file${restored.skipped === 1 ? "" : "s"} left out`}
                  .
                </div>
              )}
              <div className="row wrap" style={{ justifyContent: "space-between" }}>
                <div className="crumbs">
                  <button onClick={() => setPath("")} title={volume ? "Top of the volume" : "Site root"}>
                    <Icon name="folder" size={15} />
                  </button>
                  {crumbs.map((c, i) => (
                    <span key={i} className="row" style={{ gap: 2 }}>
                      <span className="sep">/</span>
                      <button onClick={() => setPath(crumbs.slice(0, i + 1).join("/"))}>{c}</button>
                    </span>
                  ))}
                </div>
                <div className="row wrap">
                  <a className="btn sm" href={api.snapshotDownloadUrl(base, snap, path, volume)} title="Download this folder as a zip archive">
                    <Icon name="download" size={14} /> Download
                  </a>
                  {canWrite && (
                    <button className="btn sm" disabled={copy.isPending} onClick={() => restoreCopy(path, label)}>
                      <Icon name="refresh" size={14} /> {copy.isPending ? "Restoring…" : "Restore a copy"}
                    </button>
                  )}
                </div>
              </div>
              {files.isPending ? (
                <Loading />
              ) : files.error ? (
                <ErrorBox error={files.error} />
              ) : files.data.entries.length === 0 ? (
                <div className="empty small">This folder was empty.</div>
              ) : (
                <div className="table-wrap">
                  <table className="table">
                    <thead>
                      <tr>
                        <th>Name</th>
                        <th className="num">Size</th>
                        <th className="hide-sm">Modified</th>
                        <th />
                      </tr>
                    </thead>
                    <tbody>
                      {files.data.entries.map((e) => (
                        <EntryRow
                          key={e.name}
                          e={e}
                          href={api.snapshotDownloadUrl(base, snap, join(path, e.name), volume)}
                          onOpen={() => setPath(join(path, e.name))}
                          onRestore={canWrite && !copy.isPending ? () => restoreCopy(join(path, e.name), e.name) : undefined}
                        />
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
              <p className="faint small" style={{ margin: 0 }}>
                A folder downloads as a zip archive; a large one takes a while to prepare before the download starts.
                {volume ? " The first look into a volume's backup reads all of it, which takes a while for a large volume." : ""}
              </p>
            </>
          )}
        </div>
      )}
    </Modal>
  );
}

function EntryRow({ e, href, onOpen, onRestore }: { e: SnapshotEntry; href: string; onOpen: () => void; onRestore?: () => void }) {
  const usable = e.type === "file" || e.type === "dir";
  return (
    <tr className={e.type === "dir" ? "clickable" : ""} onClick={e.type === "dir" ? onOpen : undefined}>
      <td>
        <div className="row">
          <Icon name={e.type === "dir" ? "folder" : e.type === "symlink" ? "link" : "file"} className={e.type === "dir" ? "file-icon dir" : "file-icon"} />
          <span>{e.name}</span>
        </div>
      </td>
      <td className="num muted">{e.type === "file" ? formatBytes(e.size) : ""}</td>
      <td className="muted hide-sm">{formatDate(e.mod_time)}</td>
      <td className="num" onClick={(ev) => ev.stopPropagation()}>
        {usable && (
          <div className="row" style={{ justifyContent: "flex-end", gap: 2 }}>
            <a className="btn sm icon ghost" title={e.type === "dir" ? "Download as a zip archive" : "Download"} href={href}>
              <Icon name="download" size={14} />
            </a>
            {onRestore && (
              <button className="btn sm icon ghost" title="Restore a copy" onClick={onRestore}>
                <Icon name="refresh" size={14} />
              </button>
            )}
          </div>
        )}
      </td>
    </tr>
  );
}
