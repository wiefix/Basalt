import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, can, roleRank, type Role, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, formatDate } from "../components/ui";

const ROLES: { role: Role; text: string }[] = [
  { role: "viewer", text: "Viewer — read-only access" },
  { role: "member", text: "Member — manage files and use servers" },
  { role: "admin", text: "Admin — manage servers, sites and users" },
  { role: "owner", text: "Owner — full control" },
];

export function UsersPage({ user }: { user: User }) {
  const users = useQuery({ queryKey: ["users"], queryFn: api.users, enabled: can(user, "admin") });
  const [adding, setAdding] = useState(false);
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
                <th>Added</th>
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
                  </td>
                  <td>{u.totp_enabled ? <span className="badge ok">on</span> : <span className="badge">off</span>}</td>
                  <td className="muted">{formatDate(u.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
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
