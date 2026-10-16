import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Passkey } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, timeAgo, useConfirm, useToast } from "../components/ui";
import { createPasskey, passkeysSupported } from "../components/passkey";

// guessName suggests a name for a new passkey from the browser.
function guessName(): string {
  const ua = navigator.userAgent;
  if (/iPhone/.test(ua)) return "iPhone";
  if (/iPad/.test(ua)) return "iPad";
  if (/Android/.test(ua)) return "Android phone";
  if (/Mac OS X/.test(ua)) return "Mac";
  if (/Windows/.test(ua)) return "Windows PC";
  return "Passkey";
}

// PasskeysCard lists the signed-in user's passkeys.
export function PasskeysCard() {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const list = useQuery({ queryKey: ["my-passkeys"], queryFn: api.myPasskeys });
  const [adding, setAdding] = useState(false);
  const [renaming, setRenaming] = useState<Passkey | null>(null);
  const refresh = () => qc.invalidateQueries({ queryKey: ["my-passkeys"] });
  const remove = async (k: Passkey) => {
    const ok = await confirm({
      title: `Remove passkey ${k.name}?`,
      body: <p>It no longer signs you in here. Also delete it from the device or password manager that holds it.</p>,
      confirm: "Remove",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deletePasskey(k.id);
      refresh();
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  const supported = passkeysSupported();
  return (
    <div className="card">
      <div className="card-head">
        <h2>Passkeys</h2>
        <button className="btn sm" onClick={() => setAdding(true)} disabled={!supported}>
          <Icon name="plus" size={14} /> Add passkey
        </button>
      </div>
      {list.isPending ? (
        <Loading />
      ) : list.error ? (
        <div className="card-body">
          <ErrorBox error={list.error} />
        </div>
      ) : list.data.passkeys.length === 0 ? (
        <div className="card-body muted">
          {supported
            ? "Sign in with your fingerprint, face or device PIN instead of a password and code. A passkey cannot be phished: it only works on this panel's address."
            : "This browser cannot use passkeys here: open the panel by its domain name over HTTPS."}
        </div>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <tbody>
              {list.data.passkeys.map((k) => (
                <tr key={k.id}>
                  <td>
                    <strong>{k.name}</strong>
                    {k.synced && (
                      <span className="badge plain" style={{ marginLeft: 6 }} title="Backed up by its password manager and available on your other devices">
                        synced
                      </span>
                    )}
                    <div className="faint small mono">{k.host}</div>
                  </td>
                  <td className="muted small">
                    {k.last_used_at ? `used ${timeAgo(k.last_used_at)}` : "never used"}
                    <div className="faint hide-sm">added {formatDate(k.created_at)}</div>
                  </td>
                  <td className="num">
                    <div className="row" style={{ justifyContent: "flex-end" }}>
                      <button className="btn sm icon ghost" title="Rename" onClick={() => setRenaming(k)}>
                        <Icon name="edit" size={14} />
                      </button>
                      <button className="btn sm icon ghost" title="Remove" onClick={() => remove(k)}>
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
      {adding && (
        <AddPasskeyModal
          onClose={() => setAdding(false)}
          onAdded={() => {
            refresh();
            toast("Passkey added: use it the next time you sign in");
          }}
        />
      )}
      {renaming && <RenameModal passkey={renaming} onClose={() => setRenaming(null)} onSaved={refresh} />}
    </div>
  );
}

function AddPasskeyModal({ onClose, onAdded }: { onClose: () => void; onAdded: () => void }) {
  const [name, setName] = useState(guessName());
  const [password, setPassword] = useState("");
  const add = useMutation({
    mutationFn: () => createPasskey(password, name.trim()),
    onSuccess: () => {
      onAdded();
      onClose();
    },
  });
  return (
    <Modal
      title="Add a passkey"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="passkey-form" disabled={add.isPending}>
            {add.isPending ? "Waiting for the device…" : "Continue"}
          </button>
        </>
      }
    >
      <form
        id="passkey-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          add.mutate();
        }}
      >
        <ErrorBox error={add.error} />
        <Field label="Name" help="To recognise it later, e.g. the device or password manager.">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} required maxLength={64} autoFocus />
        </Field>
        <Field label="Your password" help="To confirm it is you.">
          <input className="input" type="password" value={password} onChange={(e) => setPassword(e.target.value)} required autoComplete="current-password" />
        </Field>
        <p className="faint small" style={{ margin: 0 }}>
          Your browser then asks where to keep the passkey (this device, your phone, a security key or a password manager) and
          to unlock it with your fingerprint, face or PIN. Signing in with it skips the password and two-factor code.
        </p>
      </form>
    </Modal>
  );
}

function RenameModal({ passkey, onClose, onSaved }: { passkey: Passkey; onClose: () => void; onSaved: () => void }) {
  const [name, setName] = useState(passkey.name);
  const save = useMutation({
    mutationFn: () => api.renamePasskey(passkey.id, name.trim()),
    onSuccess: () => {
      onSaved();
      onClose();
    },
  });
  return (
    <Modal
      title="Rename passkey"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="rename-passkey" disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <form
        id="rename-passkey"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        <Field label="Name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} required maxLength={64} autoFocus />
        </Field>
      </form>
    </Modal>
  );
}
