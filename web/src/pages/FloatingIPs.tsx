import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, can, type CloudAccount, type FloatingIP, type User } from "../api";
import { ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, timeAgo, useConfirm, useToast } from "../components/ui";

export function FloatingIPsPage({ user }: { user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const admin = can(user, "admin");
  const ips = useQuery({ queryKey: ["floating-ips"], queryFn: api.floatingIPs, enabled: admin, refetchInterval: 15000 });
  const accounts = useQuery({ queryKey: ["cloud-accounts"], queryFn: api.cloudAccounts, enabled: admin });
  const [account, setAccount] = useState<CloudAccount | "new" | null>(null);
  const [adding, setAdding] = useState(false);
  const [group, setGroup] = useState<FloatingIP | null>(null);
  const [busy, setBusy] = useState(0);
  if (!admin) return <div className="empty">You do not have access to this page.</div>;

  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["floating-ips"] });
    qc.invalidateQueries({ queryKey: ["cloud-accounts"] });
  };
  const act = async (f: FloatingIP, fn: () => Promise<unknown>, done?: string) => {
    setBusy(f.id);
    try {
      await fn();
      if (done) toast(done);
    } catch (e) {
      toast(errorMessage(e), true);
    } finally {
      setBusy(0);
      refresh();
    }
  };
  const move = async (f: FloatingIP, serverID: number) => {
    const to = f.servers.find((s) => s.server_id === serverID)?.name ?? "";
    const ok = await confirm({
      title: `Move ${f.address} to ${to}?`,
      body: <p>The provider routes the address to {to} within seconds; connections open on the other server break.</p>,
      confirm: "Move",
    });
    if (ok) act(f, () => api.assignFloatingIP(f.id, serverID), `${f.address} now goes to ${to}`);
  };
  const remove = async (f: FloatingIP) => {
    const ok = await confirm({
      title: `Remove ${f.address} from Basalt?`,
      body: (
        <p>
          The servers take the address off their interface. The floating IP stays in your {f.account_name} account, and
          the provider keeps routing it where it does now.
        </p>
      ),
      confirm: "Remove",
      danger: true,
    });
    if (ok) act(f, () => api.deleteFloatingIP(f.id));
  };
  const removeAccount = async (a: CloudAccount) => {
    const ok = await confirm({ title: `Remove the account ${a.name}?`, body: <p>Its API token is deleted from Basalt.</p>, confirm: "Remove", danger: true });
    if (!ok) return;
    try {
      await api.deleteCloudAccount(a.id);
      refresh();
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };

  const hasAccounts = (accounts.data?.accounts.length ?? 0) > 0;
  return (
    <>
      <div className="page-head">
        <div>
          <h1>Floating IPs</h1>
          <div className="sub">Addresses of your cloud provider that can move between servers: every server of the group has the address, the provider sends its traffic to one.</div>
        </div>
        <button className="btn primary" onClick={() => setAdding(true)} disabled={!hasAccounts} title={hasAccounts ? undefined : "Add a cloud account first"}>
          <Icon name="plus" /> Add floating IP
        </button>
      </div>
      {ips.isPending ? (
        <Loading />
      ) : ips.error ? (
        <ErrorBox error={ips.error} />
      ) : ips.data.floating_ips.length === 0 ? (
        <div className="card empty">
          <h3>No floating IPs</h3>
          <p>
            {hasAccounts
              ? "Add a floating IP of your account and choose the servers it may go to."
              : "Add your Hetzner Cloud account below, then one of its floating IPs."}
          </p>
        </div>
      ) : (
        <div className="card table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Address</th>
                <th>Goes to</th>
                <th>Servers</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {ips.data.floating_ips.map((f) => {
                const current = f.servers.find((s) => s.server_id === f.server_id);
                return (
                  <tr key={f.id}>
                    <td>
                      <div className="mono">{f.address}</div>
                      <div className="faint small">
                        {[f.name, f.location, f.account_name].filter(Boolean).join(" · ")}
                        {f.network !== f.address && <> · {f.network}</>}
                      </div>
                    </td>
                    <td>
                      {current ? (
                        <>
                          <span className="badge ok">{current.name}</span>
                          {f.assigned_at && <div className="faint small">since {timeAgo(f.assigned_at)}</div>}
                        </>
                      ) : (
                        <span className="badge warn" title="Not routed to a server of the group, as far as Basalt knows. Refresh reads it from the provider.">
                          none of them
                        </span>
                      )}
                    </td>
                    <td>
                      <div className="row" style={{ flexWrap: "wrap", gap: 6 }}>
                        {f.servers.map((s) => (
                          <span
                            key={s.server_id}
                            className={s.error ? "badge err" : s.online ? "badge plain" : "badge warn"}
                            title={s.error ? `Cannot put the address on its interface: ${s.error}` : s.online ? "Online" : "Offline: it gets the address when it connects"}
                          >
                            {s.name}
                          </span>
                        ))}
                      </div>
                    </td>
                    <td className="num">
                      <div className="row" style={{ justifyContent: "flex-end" }}>
                        {f.servers.length > 1 && (
                          <select
                            className="input"
                            style={{ width: "auto" }}
                            value=""
                            disabled={busy === f.id}
                            onChange={(e) => e.target.value && move(f, Number(e.target.value))}
                            aria-label="Move to server"
                          >
                            <option value="">{busy === f.id ? "Working…" : "Move to…"}</option>
                            {f.servers
                              .filter((s) => s.server_id !== f.server_id)
                              .map((s) => (
                                <option key={s.server_id} value={s.server_id}>
                                  {s.name}
                                </option>
                              ))}
                          </select>
                        )}
                        <button className="btn sm" onClick={() => setGroup(f)}>
                          <Icon name="server" size={14} /> Servers
                        </button>
                        <button className="btn sm icon ghost" title="Read where the provider routes it" disabled={busy === f.id} onClick={() => act(f, () => api.refreshFloatingIP(f.id))}>
                          <Icon name="refresh" size={14} />
                        </button>
                        <button className="btn sm icon ghost" title="Remove" onClick={() => remove(f)}>
                          <Icon name="trash" size={14} />
                        </button>
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      <div className="page-head" style={{ marginTop: 24 }}>
        <div>
          <h2>Cloud accounts</h2>
          <div className="sub">API tokens Basalt moves floating IPs with. Stored encrypted.</div>
        </div>
        <button className="btn" onClick={() => setAccount("new")}>
          <Icon name="plus" /> Add account
        </button>
      </div>
      {accounts.isPending ? (
        <Loading />
      ) : accounts.error ? (
        <ErrorBox error={accounts.error} />
      ) : accounts.data.accounts.length === 0 ? (
        <div className="card card-body muted">
          No accounts yet. In the Hetzner Cloud Console, open your project → Security → API tokens and create a token
          with Read &amp; Write permission.
        </div>
      ) : (
        <div className="card table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Provider</th>
                <th>Floating IPs</th>
                <th>Updated</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {accounts.data.accounts.map((a) => (
                <tr key={a.id}>
                  <td>{a.name}</td>
                  <td>Hetzner Cloud</td>
                  <td className="num">{a.floating_ips}</td>
                  <td className="muted">{formatDate(a.updated_at)}</td>
                  <td className="num">
                    <div className="row" style={{ justifyContent: "flex-end" }}>
                      <button className="btn sm" onClick={() => setAccount(a)}>
                        <Icon name="key" size={14} /> Change
                      </button>
                      <button className="btn sm icon ghost" title="Remove" onClick={() => removeAccount(a)}>
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
          Each server of a floating IP's group puts the address on its network interface (and again after a reboot, without
          the panel). Moving the address is then only a call to the provider: the traffic arrives at the other server within
          seconds. Point DNS records at the floating IP; automatic DNS records keep using each server's own address.
        </p>
        <p className="faint small" style={{ margin: 0 }}>
          Basalt finds the provider's server by its addresses. For an IPv6 floating IP the servers use the first address of
          its block.
        </p>
      </div>
      {account && <AccountModal account={account === "new" ? null : account} onClose={() => setAccount(null)} />}
      {adding && accounts.data && <AddModal accounts={accounts.data.accounts} onClose={() => setAdding(false)} />}
      {group && <GroupModal ip={group} onClose={() => setGroup(null)} />}
    </>
  );
}

function AccountModal({ account, onClose }: { account: CloudAccount | null; onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState(account?.name ?? "");
  const [token, setToken] = useState("");
  const save = useMutation({
    mutationFn: () => (account ? api.updateCloudAccount(account.id, { name, token }) : api.createCloudAccount({ provider: "hetzner", name, token })),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["cloud-accounts"] });
      qc.invalidateQueries({ queryKey: ["floating-ips"] });
      onClose();
    },
  });
  return (
    <Modal
      title={account ? `Account ${account.name}` : "Add Hetzner Cloud account"}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="cloud-account-form" disabled={save.isPending}>
            {save.isPending ? "Checking…" : "Save"}
          </button>
        </>
      }
    >
      <form
        id="cloud-account-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        <Field label="Name" help="Your name for it, e.g. the project's.">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} required autoFocus maxLength={64} />
        </Field>
        <Field
          label="API token"
          help={account ? "Leave empty to keep the current token." : "Project → Security → API tokens, with Read & Write permission. Checked with the provider, stored encrypted."}
        >
          <input className="input mono" type="password" value={token} onChange={(e) => setToken(e.target.value)} required={!account} autoComplete="new-password" />
        </Field>
      </form>
    </Modal>
  );
}

// ServerPicker lists the servers to choose, in the order they were picked.
function ServerPicker({ servers, picked, onChange }: { servers: { id: number; name: string }[]; picked: number[]; onChange: (ids: number[]) => void }) {
  if (servers.length === 0) return <p className="muted">None of your Basalt servers is in this account (Basalt matches them by their addresses).</p>;
  return (
    <div className="stack" style={{ gap: 6 }}>
      {servers.map((s) => (
        <label key={s.id} className="check">
          <input
            type="checkbox"
            checked={picked.includes(s.id)}
            onChange={(e) => onChange(e.target.checked ? [...picked, s.id] : picked.filter((id) => id !== s.id))}
          />{" "}
          {s.name}
          {picked.includes(s.id) && <span className="faint small"> #{picked.indexOf(s.id) + 1}</span>}
        </label>
      ))}
    </div>
  );
}

function AddModal({ accounts, onClose }: { accounts: CloudAccount[]; onClose: () => void }) {
  const qc = useQueryClient();
  const [accountID, setAccountID] = useState(accounts[0]?.id ?? 0);
  const [providerID, setProviderID] = useState("");
  const [picked, setPicked] = useState<number[]>([]);
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const at = useQuery({ queryKey: ["provider-floating-ips", accountID], queryFn: () => api.providerFloatingIPs(accountID), enabled: accountID > 0 });
  const save = useMutation({
    mutationFn: () => api.addFloatingIP({ account_id: accountID, provider_id: providerID, server_ids: picked }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["floating-ips"] });
      qc.invalidateQueries({ queryKey: ["cloud-accounts"] });
      onClose();
    },
  });
  const names = new Map((servers.data?.servers ?? []).map((s) => [s.id, s.name]));
  const matched = (at.data?.servers ?? []).filter((s) => s.server_id > 0).map((s) => ({ id: s.server_id, name: `${names.get(s.server_id) ?? s.server_id} (${s.name})` }));
  const free = (at.data?.floating_ips ?? []).filter((f) => f.floating_ip_id === 0);
  const providerName = (id: string) => at.data?.servers.find((s) => s.provider_id === id)?.name;
  return (
    <Modal
      title="Add floating IP"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="floating-ip-form" disabled={save.isPending || !providerID || picked.length === 0}>
            Add
          </button>
        </>
      }
    >
      <form
        id="floating-ip-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        {accounts.length > 1 && (
          <Field label="Account">
            <select className="input" value={accountID} onChange={(e) => { setAccountID(Number(e.target.value)); setProviderID(""); setPicked([]); }}>
              {accounts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name}
                </option>
              ))}
            </select>
          </Field>
        )}
        {at.isPending ? (
          <Loading />
        ) : at.error ? (
          <ErrorBox error={at.error} />
        ) : (
          <>
            <Field label="Floating IP" help={free.length === 0 ? "The account has no floating IP that is not added yet. Create one in the provider's console." : undefined}>
              <select className="input mono" value={providerID} onChange={(e) => setProviderID(e.target.value)} required>
                <option value="">Choose…</option>
                {free.map((f) => (
                  <option key={f.provider_id} value={f.provider_id}>
                    {f.network}
                    {f.name && ` — ${f.name}`}
                    {f.location && ` (${f.location})`}
                    {f.assigned_to && ` → ${providerName(f.assigned_to) ?? f.assigned_to}`}
                  </option>
                ))}
              </select>
            </Field>
            <Field label="Servers" help="Each of them puts the address on its interface. The order is the one failover will try.">
              <ServerPicker servers={matched} picked={picked} onChange={setPicked} />
            </Field>
          </>
        )}
      </form>
    </Modal>
  );
}

function GroupModal({ ip, onClose }: { ip: FloatingIP; onClose: () => void }) {
  const qc = useQueryClient();
  const [picked, setPicked] = useState<number[]>(ip.servers.map((s) => s.server_id));
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const at = useQuery({ queryKey: ["provider-floating-ips", ip.account_id], queryFn: () => api.providerFloatingIPs(ip.account_id) });
  const save = useMutation({
    mutationFn: () => api.setFloatingIPServers(ip.id, picked),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["floating-ips"] });
      onClose();
    },
  });
  const names = new Map((servers.data?.servers ?? []).map((s) => [s.id, s.name]));
  const matched = (at.data?.servers ?? []).filter((s) => s.server_id > 0).map((s) => ({ id: s.server_id, name: names.get(s.server_id) ?? String(s.server_id) }));
  return (
    <Modal
      title={`Servers of ${ip.address}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="floating-group-form" disabled={save.isPending || picked.length === 0}>
            Save
          </button>
        </>
      }
    >
      <form
        id="floating-group-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        {at.isPending ? <Loading /> : at.error ? <ErrorBox error={at.error} /> : <ServerPicker servers={matched} picked={picked} onChange={setPicked} />}
        <p className="faint small" style={{ margin: 0 }}>
          A server taken out of the group takes the address off its interface. If the provider routes the address to it, move it first.
        </p>
      </form>
    </Modal>
  );
}
