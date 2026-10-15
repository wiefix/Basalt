import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, can, roleRank, type Role, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, useConfirm, useToast } from "../components/ui";
import { AllTokensCard } from "./Tokens";

const ROLES: { role: Role; text: string }[] = [
  { role: "viewer", text: "Viewer — read-only access" },
  { role: "member", text: "Member — manage files and use servers" },
  { role: "admin", text: "Admin — manage servers, sites and users" },
  { role: "owner", text: "Owner — full control" },
];

export function UsersPage({ user }: { user: User }) {
  const users = useQuery({ queryKey: ["users"], queryFn: api.users, enabled: can(user, "admin") });
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<User | null>(null);
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const remove = async (u: User) => {
    const ok = await confirm({
      title: `Delete ${u.name}?`,
      body: <p>{u.email} can no longer sign in; their sessions and API tokens end. The audit log keeps what they did.</p>,
      confirm: "Delete",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteUser(u.id);
      qc.invalidateQueries({ queryKey: ["users"] });
      qc.invalidateQueries({ queryKey: ["all-tokens"] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  if (!can(user, "admin")) return <div className="empty">You do not have access to this page.</div>;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Users</h1>
          <div className="sub">People who can sign in to this panel.</div>
        </div>
        <button className="btn primary" onClick={() => setAdding(true)}>
          <Icon name="plus" /> Add user
        </button>
      </div>
      {users.isPending ? (
        <Loading />
      ) : users.error ? (
        <ErrorBox error={users.error} />
      ) : (
        <div className="card table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Email</th>
                <th>Role</th>
                <th>2FA</th>
                <th className="hide-sm">Added</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {users.data.users.map((u) => (
                <tr key={u.id}>
                  <td>
                    <strong>{u.name}</strong> {u.id === user.id && <span className="faint">(you)</span>}
                  </td>
                  <td>{u.email}</td>
                  <td>
                    <span className="badge plain">{u.role}</span>
                    {u.disabled && (
                      <span className="badge err" style={{ marginLeft: 6 }}>
                        disabled
                      </span>
                    )}
                  </td>
                  <td>{u.totp_enabled ? <span className="badge ok">on</span> : <span className="badge">off</span>}</td>
                  <td className="muted hide-sm">{formatDate(u.created_at)}</td>
                  <td className="num">
                    {roleRank[user.role] >= roleRank[u.role] && (
                      <div className="row" style={{ justifyContent: "flex-end" }}>
                        <button className="btn sm icon ghost" title="Edit" onClick={() => setEditing(u)}>
                          <Icon name="edit" size={14} />
                        </button>
                        {u.id !== user.id && (
                          <button className="btn sm icon ghost" title="Delete" onClick={() => remove(u)}>
                            <Icon name="trash" size={14} />
                          </button>
                        )}
                      </div>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <AllTokensCard />
      {editing && <EditUserModal me={user} target={editing} onClose={() => setEditing(null)} />}
      {adding && <AddUserModal me={user} onClose={() => setAdding(false)} />}
    </>
  );
}

function AddUserModal({ me, onClose }: { me: User; onClose: () => void }) {
  const qc = useQueryClient();
  const [form, setForm] = useState({ name: "", email: "", password: "", role: "member" as Role });
  const create = useMutation({
    mutationFn: () => api.createUser(form),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["users"] });
      onClose();
    },
  });
  return (
    <Modal
      title="Add user"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="add-user" disabled={create.isPending}>
            Add user
          </button>
        </>
      }
    >
      <form
        id="add-user"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <ErrorBox error={create.error} />
        <Field label="Name">
          <input className="input" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} required autoFocus />
        </Field>
        <Field label="Email">
          <input className="input" type="email" value={form.email} onChange={(e) => setForm({ ...form, email: e.target.value })} required />
        </Field>
        <Field label="Initial password" help="At least 12 characters. Share it securely; the user can change it after signing in.">
          <input
            className="input"
            type="password"
            value={form.password}
            onChange={(e) => setForm({ ...form, password: e.target.value })}
            minLength={12}
            required
            autoComplete="new-password"
          />
        </Field>
        <Field label="Role">
          <select className="input" value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value as Role })}>
            {ROLES.filter((r) => roleRank[r.role] <= roleRank[me.role]).map((r) => (
              <option key={r.role} value={r.role}>
                {r.text}
              </option>
            ))}
          </select>
        </Field>
      </form>
    </Modal>
  );
}

function EditUserModal({ me, target, onClose }: { me: User; target: User; onClose: () => void }) {
  const qc = useQueryClient();
  const toast = useToast();
  const self = me.id === target.id;
  const [name, setName] = useState(target.name);
  const [role, setRole] = useState<Role>(target.role);
  const [disabled, setDisabled] = useState(target.disabled);
  const [password, setPassword] = useState("");
  const [resetTOTP, setResetTOTP] = useState(false);
  const save = useMutation({
    mutationFn: async () => {
      await api.updateUser(target.id, self ? { name } : { name, role, disabled });
      if (password) await api.resetPassword(target.id, { password, reset_totp: resetTOTP });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["users"] });
      qc.invalidateQueries({ queryKey: ["me"] });
      if (password) toast(`Password set; ${target.name} was signed out everywhere`);
      onClose();
    },
  });
  return (
    <Modal
      title={`Edit ${target.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="edit-user" disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <form
        id="edit-user"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        <div className="muted small">{target.email}</div>
        <Field label="Name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} required />
        </Field>
        {self ? (
          <p className="faint small" style={{ margin: 0 }}>
            Your own role and access are changed by another administrator; your password under Account.
          </p>
        ) : (
          <>
            <Field label="Role">
              <select className="input" value={role} onChange={(e) => setRole(e.target.value as Role)}>
                {ROLES.filter((r) => roleRank[r.role] <= roleRank[me.role]).map((r) => (
                  <option key={r.role} value={r.role}>
                    {r.text}
                  </option>
                ))}
              </select>
            </Field>
            <label className="check">
              <input type="checkbox" checked={disabled} onChange={(e) => setDisabled(e.target.checked)} /> Disabled: cannot sign in,
              sessions and API tokens stop working
            </label>
            <Field label="New password" help="Optional. At least 12 characters; signs them out everywhere. Share it securely.">
              <input
                className="input"
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                minLength={12}
                autoComplete="new-password"
              />
            </Field>
            {password && target.totp_enabled && (
              <label className="check small">
                <input type="checkbox" checked={resetTOTP} onChange={(e) => setResetTOTP(e.target.checked)} /> Also turn off
                two-factor authentication (lost phone and recovery codes)
              </label>
            )}
          </>
        )}
      </form>
    </Modal>
  );
}
