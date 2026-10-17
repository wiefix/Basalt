import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { api, type RestoredCopy, type SnapshotEntry } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatBytes, formatDate, useConfirm, useToast } from "../components/ui";

const join = (dir: string, name: string) => (dir ? `${dir}/${name}` : name);

// SnapshotFilesModal browses the backups of a site's files: download a file
// or a folder (as a zip), or restore a copy of it into a new folder of the
// site, leaving the current files as they are.
export function SnapshotFilesModal({ siteId, base, canWrite, onClose }: { siteId: number; base: string; canWrite: boolean; onClose: () => void }) {
  const toast = useToast();
  const confirm = useConfirm();
  const snaps = useQuery({ queryKey: ["snapshots", base], queryFn: () => api.snapshots(base), gcTime: 0 });
  const list = (snaps.data?.snapshots ?? []).filter((s) => s.item.kind === "site_files");
  const [chosen, setChosen] = useState("");
  const snap = chosen || list[0]?.id || "";
  const [path, setPath] = useState("");
  const [restored, setRestored] = useState<RestoredCopy | null>(null);
  const files = useQuery({
    queryKey: ["snapshot-files", siteId, snap, path],
    queryFn: () => api.snapshotFiles(siteId, snap, path),
    enabled: snap !== "",
  });
  const copy = useMutation({
    mutationFn: (p: string) => api.restoreCopy(siteId, snap, p),
    onSuccess: (r) => setRestored(r),
    onError: (e) => toast(errorMessage(e), true),
  });
  const restoreCopy = async (p: string, label: string) => {
    const ok = await confirm({
      title: `Restore a copy of ${label}?`,
      body: (
        <p>
          It is put into a new folder <span className="mono">restored-…</span> at the top of the site. Current files stay as they
          are; move what you need from there. Symbolic links are left out.
        </p>
      ),
      confirm: "Restore a copy",
    });
    if (ok) copy.mutate(p);
  };
  const crumbs = path.split("/").filter(Boolean);
  const label = path ? path.split("/").pop()! : "the whole site";

  return (
    <Modal wide title="Files in backups" onClose={onClose}>
      {snaps.isPending ? (
        <Loading />
      ) : snaps.error ? (
        <ErrorBox error={snaps.error} />
      ) : list.length === 0 ? (
        <div className="empty small">No backups of the site's files in the storage yet.</div>
      ) : (
        <div className="stack">
          <Field label="Backup">
            <select
              className="input"
              value={snap}
              onChange={(e) => {
                setChosen(e.target.value);
                setRestored(null);
              }}
            >
              {list.map((s) => (
                <option key={s.id} value={s.id}>
                  {formatDate(s.time)} · {formatBytes(s.bytes)} · {s.id.slice(0, 8)}
                </option>
              ))}
            </select>
          </Field>
          {restored && (
            <div className="alert ok small">
              Restored {restored.files} file{restored.files === 1 ? "" : "s"} ({formatBytes(restored.bytes)}) into{" "}
              <Link to={`/sites/${siteId}?path=${encodeURIComponent("/" + restored.folder)}`} onClick={onClose}>
                {restored.folder}
              </Link>
              {restored.skipped > 0 && `; ${restored.skipped} symbolic link${restored.skipped === 1 ? "" : "s"} or special file${restored.skipped === 1 ? "" : "s"} left out`}.
            </div>
          )}
          <div className="row wrap" style={{ justifyContent: "space-between" }}>
            <div className="crumbs">
              <button onClick={() => setPath("")} title="Site root">
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
              <a className="btn sm" href={api.snapshotDownloadUrl(siteId, snap, path)} title="Download this folder as a zip archive">
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
                      href={api.snapshotDownloadUrl(siteId, snap, join(path, e.name))}
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
          </p>
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
