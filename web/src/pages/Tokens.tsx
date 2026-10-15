import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import { api, roleRank, type APIToken, type Role, type User } from "../api";
import { CodeBox, ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, timeAgo, useConfirm, useToast } from "../components/ui";

const roleText: Record<Role, string> = {
  viewer: "Read only (viewer)",
  member: "Deploy and manage files (member)",
  admin: "Manage everything (admin)",
  owner: "Full control (owner)",
};

const expiryChoices = [
  [30, "30 days"],
  [90, "90 days"],
  [365, "1 year"],
  [0, "Never"],
] as const;

function Expiry({ t }: { t: APIToken }) {
  if (t.expired) return <span className="badge err">expired</span>;
  if (!t.expires_at) return <span className="faint">never</span>;
  return <span className="muted">{formatDate(t.expires_at)}</span>;
}

function TokenTable({ tokens, showUser, onRevoke }: { tokens: APIToken[]; showUser?: boolean; onRevoke: (t: APIToken) => void }) {
  return (
    <div className="table-wrap">
      <table className="table">
        <thead>
          <tr>
            <th>Token</th>
            {showUser && <th>User</th>}
            <th>Access</th>
            <th className="hide-sm">Last used</th>
            <th>Expires</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {tokens.map((t) => (
            <tr key={t.id}>
              <td>
                <strong>{t.name}</strong>
                <div className="mono faint small">{t.prefix}…</div>
              </td>
              {showUser && <td className="small">{t.user?.email}</td>}
              <td>
                <span className="badge plain">{t.role}</span>
              </td>
              <td className="muted small hide-sm">
                {t.last_used_at ? (
                  <>
                    {timeAgo(t.last_used_at)}
                    {t.last_used_ip && <div className="mono faint">{t.last_used_ip}</div>}
                  </>
                ) : (
                  "never"
                )}
              </td>
              <td className="small">
                <Expiry t={t} />
              </td>
              <td className="num">
                <button className="btn sm icon ghost" title="Revoke" onClick={() => onRevoke(t)}>
                  <Icon name="trash" size={14} />
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// TokensCard lists the signed-in user's API tokens.
export function TokensCard({ user }: { user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const tokens = useQuery({ queryKey: ["my-tokens"], queryFn: api.myTokens });
  const [creating, setCreating] = useState(false);
  const revoke = async (t: APIToken) => {
    const ok = await confirm({
      title: `Delete token ${t.name}?`,
      body: <p>Scripts using it stop working at once.</p>,
      confirm: "Delete",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteMyToken(t.id);
      qc.invalidateQueries({ queryKey: ["my-tokens"] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  return (
    <div className="card">
      <div className="card-head">
        <h2>API tokens</h2>
        <div className="row">
          <Link className="btn sm ghost" to="/api-reference">
            API reference
          </Link>
          <button className="btn sm" onClick={() => setCreating(true)}>
            <Icon name="plus" size={14} /> New token
          </button>
        </div>
      </div>
      {tokens.isPending ? (
        <Loading />
      ) : tokens.error ? (
        <div className="card-body">
          <ErrorBox error={tokens.error} />
        </div>
      ) : tokens.data.tokens.length === 0 ? (
        <div className="card-body muted">
          For scripts and CI: a token calls the same API as this panel, as you, with the access you choose. Send it as{" "}
          <code>Authorization: Bearer …</code>. See the <Link to="/api-reference">API reference</Link>.
        </div>
      ) : (
        <TokenTable tokens={tokens.data.tokens} onRevoke={revoke} />
      )}
      {creating && <NewTokenModal user={user} onClose={() => setCreating(false)} />}
    </div>
  );
}

function NewTokenModal({ user, onClose }: { user: User; onClose: () => void }) {
  const qc = useQueryClient();
  const roles = (Object.keys(roleText) as Role[]).filter((r) => roleRank[r] <= roleRank[user.role]);
  const [name, setName] = useState("");
  const [role, setRole] = useState<Role>(roleRank[user.role] >= roleRank.member ? "member" : "viewer");
  const [days, setDays] = useState(90);
  const [password, setPassword] = useState("");
  const [secret, setSecret] = useState<string | null>(null);
  const create = useMutation({
    mutationFn: () => api.createToken({ name: name.trim(), role, expires_days: days, password }),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["my-tokens"] });
      setSecret(r.secret);
    },
  });
  if (secret) {
    return (
      <Modal
        title={`Token ${name.trim()} created`}
        onClose={onClose}
        footer={
          <button className="btn primary" onClick={onClose}>
            Done
          </button>
        }
      >
        <div className="stack">
          <div className="alert warn small">Copy it now: it is not stored and will not be shown again.</div>
          <CodeBox text={secret} />
          <Field label="Example">
            <CodeBox text={`curl -H "Authorization: Bearer ${secret}" ${window.location.origin}/api/v1/servers`} />
          </Field>
        </div>
      </Modal>
    );
  }
  return (
    <Modal
      title="New API token"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="token-form" disabled={create.isPending}>
            Create token
          </button>
        </>
      }
    >
      <form
        id="token-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <ErrorBox error={create.error} />
        <Field label="Name" help="What uses it, e.g. github-actions.">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} required maxLength={64} autoFocus />
        </Field>
        <Field label="Access" help="Never more than your own role. Tokens cannot change passwords, two-factor settings, tokens or users.">
          <select className="input" value={role} onChange={(e) => setRole(e.target.value as Role)}>
            {roles.map((r) => (
              <option key={r} value={r}>
                {roleText[r]}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Expires">
          <select className="input" value={days} onChange={(e) => setDays(Number(e.target.value))}>
            {expiryChoices.map(([d, label]) => (
              <option key={d} value={d}>
                {label}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Your password" help="To confirm it is you.">
          <input
            className="input"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            autoComplete="current-password"
          />
        </Field>
      </form>
    </Modal>
  );
}

// AllTokensCard lets admins see and revoke every user's tokens.
export function AllTokensCard() {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const tokens = useQuery({ queryKey: ["all-tokens"], queryFn: api.allTokens });
  const revoke = async (t: APIToken) => {
    const ok = await confirm({
      title: `Revoke ${t.user?.email}'s token ${t.name}?`,
      body: <p>Scripts using it stop working at once.</p>,
      confirm: "Revoke",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.revokeToken(t.id);
      qc.invalidateQueries({ queryKey: ["all-tokens"] });
      qc.invalidateQueries({ queryKey: ["my-tokens"] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  if (tokens.isPending) return <Loading />;
  if (tokens.error) return <ErrorBox error={tokens.error} />;
  if (tokens.data.tokens.length === 0) return null;
  return (
    <div className="card" style={{ marginTop: 16 }}>
      <div className="card-head">
        <h2>API tokens of all users</h2>
      </div>
      <TokenTable tokens={tokens.data.tokens} showUser onRevoke={revoke} />
    </div>
  );
}

// browserName shortens a user agent to something readable.
function browserName(ua: string): string {
  const browser = /Edg\//.test(ua) ? "Edge" : /Firefox\//.test(ua) ? "Firefox" : /Chrome\//.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "";
  const os = /iPhone|iPad/.test(ua) ? "iOS" : /Android/.test(ua) ? "Android" : /Mac OS X/.test(ua) ? "macOS" : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "";
  return [browser, os].filter(Boolean).join(" on ") || ua || "unknown client";
}

// SessionsCard lists where the user is signed in.
export function SessionsCard() {
  const qc = useQueryClient();
  const toast = useToast();
  const sessions = useQuery({ queryKey: ["my-sessions"], queryFn: api.mySessions });
  const run = async (f: () => Promise<unknown>) => {
    try {
      await f();
      qc.invalidateQueries({ queryKey: ["my-sessions"] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  const list = sessions.data?.sessions ?? [];
  return (
    <div className="card">
      <div className="card-head">
        <h2>Sessions</h2>
        {list.length > 1 && (
          <button className="btn sm" onClick={() => run(api.endOtherSessions)}>
            Sign out other sessions
          </button>
        )}
      </div>
      {sessions.isPending ? (
        <Loading />
      ) : sessions.error ? (
        <div className="card-body">
          <ErrorBox error={sessions.error} />
        </div>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <tbody>
              {list.map((x) => (
                <tr key={x.id}>
                  <td>
                    <span title={x.user_agent}>{browserName(x.user_agent)}</span>
                    {x.current && (
                      <span className="badge ok" style={{ marginLeft: 6 }}>
                        this one
                      </span>
                    )}
                    <div className="mono faint small">{x.ip}</div>
                  </td>
                  <td className="muted small">
                    active {timeAgo(x.last_seen_at)}
                    <div className="faint hide-sm">signed in {formatDate(x.created_at)}</div>
                  </td>
                  <td className="num">
                    {!x.current && (
                      <button className="btn sm" onClick={() => run(() => api.endSession(x.id))}>
                        Sign out
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
