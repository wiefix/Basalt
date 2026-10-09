import { useCallback, useEffect, useRef, useState, type DragEvent, type FormEvent, type KeyboardEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useSearchParams } from "react-router";
import { api, upload, type FileEntry } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatBytes, formatDate, useConfirm, useToast } from "./ui";

const TEXT_EXT =
  /\.(html?|css|js|mjs|cjs|ts|tsx|jsx|json|xml|svg|txt|md|markdown|csv|ya?ml|toml|ini|conf|env|php|py|rb|go|rs|sh|sql|htaccess|vue|svelte|map|lock|log|gitignore|webmanifest)$/i;
const ARCHIVE_EXT = /\.(zip|tar|tar\.gz|tgz)$/i;
const MAX_EDIT = 1 << 20;

const join = (dir: string, name: string) => (dir === "/" ? `/${name}` : `${dir}/${name}`);
const parent = (p: string) => (p === "/" ? "/" : p.slice(0, p.lastIndexOf("/")) || "/");
const isText = (e: FileEntry) => e.type === "file" && e.size <= MAX_EDIT && (TEXT_EXT.test(e.name) || !e.name.includes("."));

type UploadItem = { id: number; name: string; progress: number; error?: string; done?: boolean };

export function FileManager({ siteId, canWrite }: { siteId: number; canWrite: boolean }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const [params, setParams] = useSearchParams();
  const path = params.get("path") || "/public";
  const setPath = (p: string) => {
    setSelected(new Set());
    setParams({ path: p });
  };

  const listing = useQuery({ queryKey: ["files", siteId, path], queryFn: () => api.files(siteId, path) });
  const refresh = () => qc.invalidateQueries({ queryKey: ["files", siteId, path] });

  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [dialog, setDialog] = useState<
    | { kind: "mkdir" | "newfile" }
    | { kind: "rename" | "chmod"; entry: FileEntry }
    | { kind: "edit"; path: string }
    | { kind: "compress"; names: string[] }
    | null
  >(null);
  const [uploads, setUploads] = useState<UploadItem[]>([]);
  const [dragOver, setDragOver] = useState(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const nextUpload = useRef(1);

  const entries = listing.data?.entries ?? [];
  const toggle = (name: string) =>
    setSelected((s) => {
      const n = new Set(s);
      n.has(name) ? n.delete(name) : n.add(name);
      return n;
    });
  const allSelected = entries.length > 0 && selected.size === entries.length;

  const run = async (label: string, f: () => Promise<unknown>) => {
    try {
      await f();
      refresh();
      toast(label);
      return true;
    } catch (e) {
      toast(errorMessage(e), true);
      return false;
    }
  };

  const startUploads = useCallback(
    async (files: File[]) => {
      if (!canWrite || files.length === 0) return;
      const dir = path;
      const items = files.map((f) => ({ id: nextUpload.current++, name: f.name, progress: 0, file: f }));
      setUploads((u) => [...u, ...items.map(({ file: _f, ...i }) => i)]);
      for (const it of items) {
        try {
          await upload(siteId, join(dir, it.file.name), it.file, (p) =>
            setUploads((u) => u.map((x) => (x.id === it.id ? { ...x, progress: p } : x))),
          );
          setUploads((u) => u.map((x) => (x.id === it.id ? { ...x, progress: 1, done: true } : x)));
        } catch (e) {
          setUploads((u) => u.map((x) => (x.id === it.id ? { ...x, error: errorMessage(e) } : x)));
        }
      }
      qc.invalidateQueries({ queryKey: ["files", siteId, dir] });
      setTimeout(() => setUploads((u) => u.filter((x) => !x.done)), 2500);
    },
    [canWrite, path, siteId, qc],
  );

  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    setDragOver(false);
    const items = Array.from(e.dataTransfer.items ?? []);
    if (items.some((i) => i.webkitGetAsEntry?.()?.isDirectory)) {
      toast("Folders cannot be dropped directly. Zip the folder, upload it, then extract it here.", true);
    }
    startUploads(Array.from(e.dataTransfer.files).filter((f) => f.size > 0 || f.type !== ""));
  };

  const open = (e: FileEntry) => {
    if (e.type === "dir") return setPath(join(path, e.name));
    if (isText(e)) return setDialog({ kind: "edit", path: join(path, e.name) });
    window.location.href = api.downloadUrl(siteId, join(path, e.name));
  };

  const deleteSelected = async (names: string[]) => {
    const ok = await confirm({
      title: names.length === 1 ? `Delete ${names[0]}?` : `Delete ${names.length} items?`,
      body: <p>Folders are deleted with everything inside. This cannot be undone.</p>,
      confirm: "Delete",
      danger: true,
    });
    if (!ok) return;
    if (await run("Deleted", () => api.fileOp(siteId, "delete", { paths: names.map((n) => join(path, n)) }))) {
      setSelected(new Set());
    }
  };

  const extract = async (e: FileEntry) => {
    try {
      const r = await api.fileOp(siteId, "extract", { path: join(path, e.name), to: path });
      refresh();
      const x = r.extract!;
      toast(`Extracted ${x.files} files${x.skipped?.length ? `, skipped ${x.skipped.length} existing or unsupported` : ""}`);
    } catch (err) {
      toast(errorMessage(err), true);
    }
  };

  const crumbs = path.split("/").filter(Boolean);
  const sel = entries.filter((e) => selected.has(e.name));
  const single = sel.length === 1 ? sel[0] : null;

  return (
    <div
      className={dragOver ? "card dropzone over" : "card dropzone"}
      onDragOver={(e) => {
        if (!canWrite) return;
        e.preventDefault();
        setDragOver(true);
      }}
      onDragLeave={(e) => e.currentTarget.contains(e.relatedTarget as Node) || setDragOver(false)}
      onDrop={onDrop}
    >
      <div className="card-head" style={{ flexWrap: "wrap" }}>
        <div className="crumbs">
          <button onClick={() => setPath("/")} title="Site root">
            <Icon name="folder" size={15} />
          </button>
          {crumbs.map((c, i) => (
            <span key={i} className="row" style={{ gap: 2 }}>
              <span className="sep">/</span>
              <button onClick={() => setPath("/" + crumbs.slice(0, i + 1).join("/"))}>{c}</button>
            </span>
          ))}
        </div>
        <div className="row wrap">
          {path !== "/" && (
            <button className="btn sm icon ghost" onClick={() => setPath(parent(path))} title="Up">
              <Icon name="up" size={14} />
            </button>
          )}
          <button className="btn sm icon ghost" onClick={refresh} title="Refresh">
            <Icon name="refresh" size={14} />
          </button>
          {canWrite && (
            <>
              <button className="btn sm" onClick={() => setDialog({ kind: "newfile" })}>
                <Icon name="filePlus" size={14} /> <span className="hide-sm">New file</span>
              </button>
              <button className="btn sm" onClick={() => setDialog({ kind: "mkdir" })}>
                <Icon name="folderPlus" size={14} /> <span className="hide-sm">New folder</span>
              </button>
              <button className="btn sm primary" onClick={() => fileInput.current?.click()}>
                <Icon name="upload" size={14} /> Upload
              </button>
              <input
                ref={fileInput}
                type="file"
                multiple
                hidden
                onChange={(e) => {
                  startUploads(Array.from(e.target.files ?? []));
                  e.target.value = "";
                }}
              />
            </>
          )}
        </div>
      </div>

      {sel.length > 0 && canWrite && (
        <div className="card-head" style={{ background: "var(--accent-soft)" }}>
          <div className="row">
            <strong>{sel.length} selected</strong>
            <button className="btn sm ghost" onClick={() => setSelected(new Set())}>
              Clear
            </button>
          </div>
          <div className="row wrap">
            {single && single.type === "file" && (
              <a className="btn sm" href={api.downloadUrl(siteId, join(path, single.name))}>
                <Icon name="download" size={14} /> Download
              </a>
            )}
            {single && (
              <>
                <button className="btn sm" onClick={() => setDialog({ kind: "rename", entry: single })}>
                  Rename
                </button>
                {single.type !== "symlink" && (
                  <button className="btn sm" onClick={() => setDialog({ kind: "chmod", entry: single })}>
                    Permissions
                  </button>
                )}
              </>
            )}
            {single && ARCHIVE_EXT.test(single.name) && (
              <button className="btn sm" onClick={() => extract(single)}>
                <Icon name="archive" size={14} /> Extract here
              </button>
            )}
            <button className="btn sm" onClick={() => setDialog({ kind: "compress", names: sel.map((e) => e.name) })}>
              <Icon name="archive" size={14} /> Compress
            </button>
            <button className="btn sm danger" onClick={() => deleteSelected(sel.map((e) => e.name))}>
              <Icon name="trash" size={14} /> Delete
            </button>
          </div>
        </div>
      )}

      {uploads.length > 0 && (
        <div className="card-body stack" style={{ gap: 8, borderBottom: "1px solid var(--border)" }}>
          {uploads.map((u) => (
            <div key={u.id}>
              <div className="row small">
                <span className="mono" style={{ overflow: "hidden", textOverflow: "ellipsis" }}>
                  {u.name}
                </span>
                <span className="spacer" />
                <span className={u.error ? "faint" : "muted"}>
                  {u.error ? "failed" : u.done ? "done" : `${Math.round(u.progress * 100)}%`}
                </span>
                {u.error && (
                  <button className="btn sm icon ghost" onClick={() => setUploads((x) => x.filter((y) => y.id !== u.id))}>
                    <Icon name="x" size={12} />
                  </button>
                )}
              </div>
              <div className={u.error ? "progress err" : "progress"}>
                <div style={{ width: `${(u.error ? 1 : u.progress) * 100}%` }} />
              </div>
              {u.error && <div className="small" style={{ color: "var(--err)" }}>{u.error}</div>}
            </div>
          ))}
        </div>
      )}

      {listing.isPending ? (
        <Loading />
      ) : listing.error ? (
        <div className="card-body">
          <ErrorBox error={listing.error} />
          <button className="btn sm" style={{ marginTop: 12 }} onClick={() => setPath("/")}>
            Go to site root
          </button>
        </div>
      ) : entries.length === 0 ? (
        <div className="empty">
          <h3>This folder is empty</h3>
          {canWrite && <p>Drop files here or use Upload.</p>}
        </div>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                {canWrite && (
                  <th style={{ width: 36 }}>
                    <input
                      type="checkbox"
                      checked={allSelected}
                      onChange={() => setSelected(allSelected ? new Set() : new Set(entries.map((e) => e.name)))}
                      aria-label="Select all"
                    />
                  </th>
                )}
                <th>Name</th>
                <th className="num">Size</th>
                <th className="hide-sm">Modified</th>
                <th className="hide-sm">Mode</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => (
                <tr key={e.name} className={selected.has(e.name) ? "clickable selected" : "clickable"} onDoubleClick={() => open(e)}>
                  {canWrite && (
                    <td onClick={(ev) => ev.stopPropagation()}>
                      <input type="checkbox" checked={selected.has(e.name)} onChange={() => toggle(e.name)} aria-label={`Select ${e.name}`} />
                    </td>
                  )}
                  <td onClick={() => open(e)}>
                    <div className="row">
                      <Icon
                        name={e.type === "dir" ? "folder" : e.type === "symlink" ? "link" : ARCHIVE_EXT.test(e.name) ? "archive" : "file"}
                        className={e.type === "dir" ? "file-icon dir" : "file-icon"}
                      />
                      <span>{e.name}</span>
                      {e.target && <span className="faint small mono">→ {e.target}</span>}
                    </div>
                  </td>
                  <td className="num muted">{e.type === "dir" ? "" : formatBytes(e.size)}</td>
                  <td className="muted hide-sm">{formatDate(e.mod_time)}</td>
                  <td className="mono muted hide-sm">{e.mode}</td>
                  <td className="num" onClick={(ev) => ev.stopPropagation()}>
                    <div className="row" style={{ justifyContent: "flex-end", gap: 2 }}>
                      {isText(e) && (
                        <button className="btn sm icon ghost" title="Edit" onClick={() => setDialog({ kind: "edit", path: join(path, e.name) })}>
                          <Icon name="edit" size={14} />
                        </button>
                      )}
                      {e.type === "file" && (
                        <a className="btn sm icon ghost" title="Download" href={api.downloadUrl(siteId, join(path, e.name))}>
                          <Icon name="download" size={14} />
                        </a>
                      )}
                      {canWrite && (
                        <button className="btn sm icon ghost" title="Delete" onClick={() => deleteSelected([e.name])}>
                          <Icon name="trash" size={14} />
                        </button>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {dialog?.kind === "mkdir" && (
        <NameDialog
          title="New folder"
          action="Create"
          onClose={() => setDialog(null)}
          onSubmit={(name) => run("Folder created", () => api.fileOp(siteId, "mkdir", { path: join(path, name) }))}
        />
      )}
      {dialog?.kind === "newfile" && (
        <NameDialog
          title="New file"
          action="Create"
          initial="index.html"
          onClose={() => setDialog(null)}
          onSubmit={async (name) => {
            const p = join(path, name);
            if (entries.some((e) => e.name === name)) {
              toast(`${name} already exists`, true);
              return false;
            }
            const ok = await run("File created", () => api.fileOp(siteId, "write", { path: p, content: "" }));
            if (ok && TEXT_EXT.test(name)) setTimeout(() => setDialog({ kind: "edit", path: p }));
            return ok;
          }}
        />
      )}
      {dialog?.kind === "rename" && (
        <NameDialog
          title={`Rename ${dialog.entry.name}`}
          action="Rename"
          initial={dialog.entry.name}
          onClose={() => setDialog(null)}
          onSubmit={(name) =>
            run("Renamed", async () => {
              await api.fileOp(siteId, "rename", { path: join(path, dialog.entry.name), to: join(path, name) });
              setSelected(new Set());
            })
          }
        />
      )}
      {dialog?.kind === "compress" && (
        <NameDialog
          title="Compress to zip"
          action="Compress"
          initial={dialog.names.length === 1 ? `${dialog.names[0]}.zip` : "archive.zip"}
          onClose={() => setDialog(null)}
          onSubmit={(name) =>
            run("Archive created", () =>
              api.fileOp(siteId, "compress", { paths: dialog.names.map((n) => join(path, n)), to: join(path, name) }),
            )
          }
        />
      )}
      {dialog?.kind === "chmod" && (
        <ChmodDialog
          entry={dialog.entry}
          onClose={() => setDialog(null)}
          onSubmit={(mode) => run("Permissions changed", () => api.fileOp(siteId, "chmod", { path: join(path, dialog.entry.name), mode }))}
        />
      )}
      {dialog?.kind === "edit" && (
        <Editor siteId={siteId} path={dialog.path} readOnly={!canWrite} onClose={() => setDialog(null)} onSaved={refresh} />
      )}
    </div>
  );
}

function NameDialog({
  title,
  action,
  initial = "",
  onClose,
  onSubmit,
}: {
  title: string;
  action: string;
  initial?: string;
  onClose: () => void;
  onSubmit: (name: string) => Promise<boolean>;
}) {
  const [name, setName] = useState(initial);
  const [busy, setBusy] = useState(false);
  const invalid = !name.trim() || name.includes("/") || name === "." || name === "..";
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (invalid) return;
    setBusy(true);
    const ok = await onSubmit(name.trim());
    setBusy(false);
    if (ok) onClose();
  };
  return (
    <Modal
      title={title}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="name-dialog" disabled={busy || invalid}>
            {action}
          </button>
        </>
      }
    >
      <form id="name-dialog" onSubmit={submit}>
        <Field label="Name">
          <input
            className="input"
            value={name}
            onChange={(e) => setName(e.target.value)}
            autoFocus
            onFocus={(e) => {
              const dot = e.target.value.lastIndexOf(".");
              e.target.setSelectionRange(0, dot > 0 ? dot : e.target.value.length);
            }}
          />
        </Field>
      </form>
    </Modal>
  );
}

const PERMS = [
  ["Owner", 6],
  ["Group", 3],
  ["Others", 0],
] as const;

function ChmodDialog({ entry, onClose, onSubmit }: { entry: FileEntry; onClose: () => void; onSubmit: (mode: string) => Promise<boolean> }) {
  const [mode, setMode] = useState(parseInt(entry.mode, 8) & 0o777);
  const bit = (shift: number, b: number) => (mode >> shift) & b;
  const flip = (shift: number, b: number) => setMode((m) => m ^ (b << shift));
  return (
    <Modal
      title={`Permissions · ${entry.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button
            className="btn primary"
            onClick={async () => (await onSubmit(mode.toString(8).padStart(3, "0"))) && onClose()}
          >
            Apply
          </button>
        </>
      }
    >
      <table className="table">
        <thead>
          <tr>
            <th />
            <th>Read</th>
            <th>Write</th>
            <th>{entry.type === "dir" ? "Enter" : "Execute"}</th>
          </tr>
        </thead>
        <tbody>
          {PERMS.map(([label, shift]) => (
            <tr key={label}>
              <td>{label}</td>
              {[4, 2, 1].map((b) => (
                <td key={b}>
                  <input type="checkbox" checked={!!bit(shift, b)} onChange={() => flip(shift, b)} />
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      <p className="muted" style={{ marginBottom: 0 }}>
        Octal: <code>{mode.toString(8).padStart(3, "0")}</code>
      </p>
    </Modal>
  );
}

function Editor({
  siteId,
  path,
  readOnly,
  onClose,
  onSaved,
}: {
  siteId: number;
  path: string;
  readOnly: boolean;
  onClose: () => void;
  onSaved: () => void;
}) {
  const toast = useToast();
  const confirm = useConfirm();
  const file = useQuery({ queryKey: ["text", siteId, path], queryFn: () => api.readText(siteId, path), gcTime: 0 });
  const [content, setContent] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (file.data && content === null) {
      setContent(file.data.content);
      setSaved(file.data.content);
    }
  }, [file.data, content]);

  const dirty = content !== null && content !== saved;
  const save = async () => {
    if (readOnly || content === null) return;
    setSaving(true);
    try {
      await api.fileOp(siteId, "write", { path, content });
      setSaved(content);
      onSaved();
      toast("Saved");
    } catch (e) {
      toast(errorMessage(e), true);
    } finally {
      setSaving(false);
    }
  };
  const close = async () => {
    if (dirty && !(await confirm({ title: "Discard changes?", body: <p>Your unsaved changes will be lost.</p>, confirm: "Discard", danger: true })))
      return;
    onClose();
  };
  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if ((e.metaKey || e.ctrlKey) && e.key === "s") {
      e.preventDefault();
      save();
    }
    if (e.key === "Tab" && !e.shiftKey) {
      e.preventDefault();
      const t = e.currentTarget;
      const { selectionStart: a, selectionEnd: b } = t;
      const v = t.value.slice(0, a) + "\t" + t.value.slice(b);
      setContent(v);
      requestAnimationFrame(() => t.setSelectionRange(a + 1, a + 1));
    }
  };

  return (
    <Modal
      title={
        <span className="row">
          <span className="mono">{path}</span>
          {dirty && <span className="badge warn plain">unsaved</span>}
          {readOnly && <span className="badge plain">read-only</span>}
        </span>
      }
      onClose={close}
      wide
      footer={
        <>
          <span className="faint small spacer">{readOnly ? "" : "Ctrl/⌘ + S to save"}</span>
          <button className="btn" onClick={close}>
            Close
          </button>
          {!readOnly && (
            <button className="btn primary" onClick={save} disabled={!dirty || saving}>
              {saving ? "Saving…" : "Save"}
            </button>
          )}
        </>
      }
    >
      {file.isPending ? (
        <Loading />
      ) : file.error ? (
        <ErrorBox error={file.error} />
      ) : (
        <div className="card" style={{ overflow: "hidden", margin: "-4px 0" }}>
          <textarea
            className="editor"
            value={content ?? ""}
            onChange={(e) => setContent(e.target.value)}
            onKeyDown={onKey}
            readOnly={readOnly}
            spellCheck={false}
            autoFocus
          />
        </div>
      )}
    </Modal>
  );
}
