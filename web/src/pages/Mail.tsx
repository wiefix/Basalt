import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import {
  api,
  can,
  type CheckStatus,
  type DNSRecordInput,
  type Forwarder,
  type MailCheckResult,
  type MailChecks,
  type MailDomain,
  type MailRelayInput,
  type MailSettings,
  type Mailbox,
  type MailboxInput,
  type User,
} from "../api";
import { CopyButton, ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, useConfirm, useToast } from "../components/ui";

export function MailPage({ user }: { user: User }) {
  const mail = useQuery({ queryKey: ["mail"], queryFn: api.mail });
  const [addingDomain, setAddingDomain] = useState(false);
  const admin = can(user, "admin");

  if (mail.isPending) return <Loading />;
  if (mail.error) return <ErrorBox error={mail.error} />;
  const m = mail.data;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Mail</h1>
          <div className="sub">Mailboxes for your domains, served by Stalwart on one of your servers.</div>
        </div>
        {m.server && admin && (
          <button className="btn primary" onClick={() => setAddingDomain(true)}>
            <Icon name="plus" /> Add domain
          </button>
        )}
      </div>
      {!m.server ? (
        admin ? (
          <SetupCard />
        ) : (
          <div className="card empty">
            <h3>No mail server yet</h3>
            <p>An administrator can set one up here.</p>
          </div>
        )
      ) : (
        <div className="stack">
          <ServerCard hostname={m.hostname ?? ""} serverId={m.server.server_id} appId={m.server.id} tls={m.tls} />
          <ChecksCard checks={m.checks ?? null} admin={admin} />
          {m.settings && <OutgoingCard settings={m.settings} admin={admin} />}
          {m.domains.length === 0 ? (
            <div className="card empty">
              <h3>No mail domains</h3>
              <p>Add a domain to create mailboxes for it.</p>
            </div>
          ) : (
            m.domains.map((d) => <DomainCard key={d.id} domain={d} hostname={m.hostname ?? ""} user={user} />)
          )}
        </div>
      )}
      {addingDomain && <AddDomainModal onClose={() => setAddingDomain(false)} />}
    </>
  );
}

function SetupCard() {
  const qc = useQueryClient();
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const withDocker = servers.data?.servers.filter((s) => s.docker) ?? [];
  const [serverId, setServerId] = useState<number | "">("");
  const [hostname, setHostname] = useState("");
  const [memory, setMemory] = useState("1024");
  const create = useMutation({
    mutationFn: () =>
      api.createMail({ server_id: Number(serverId || withDocker[0]?.id), hostname: hostname.trim(), memory_mb: Number(memory) || 0 }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["mail"] }),
  });
  return (
    <form
      className="card card-body form"
      style={{ maxWidth: 680 }}
      onSubmit={(e: FormEvent) => {
        e.preventDefault();
        create.mutate();
      }}
    >
      <h2>Set up the mail server</h2>
      <ErrorBox error={create.error} />
      {servers.data && withDocker.length === 0 && (
        <div className="alert warn">No server has Docker yet: install it on one first (installer option --with-docker).</div>
      )}
      <Field label="Server" help="Receives mail for all your domains. Ports 25, 465, 587 and 993 are opened on it.">
        <select className="input" value={serverId} onChange={(e) => setServerId(Number(e.target.value) || "")}>
          {withDocker.map((s) => (
            <option key={s.id} value={s.id}>
              {s.name}
              {s.dns_ips.length ? ` (${s.dns_ips.join(", ")})` : ""}
            </option>
          ))}
        </select>
      </Field>
      <Field
        label="Mail host name"
        help="The name clients and other mail servers connect to, e.g. mail.example.com. Point it at the server (automatic when its zone is under DNS)."
      >
        <input className="input mono" value={hostname} onChange={(e) => setHostname(e.target.value)} placeholder="mail.example.com" required />
      </Field>
      <Field label="Memory limit (MB)" help="At least 512; 1024 or more is comfortable.">
        <input className="input mono" type="number" min={512} value={memory} onChange={(e) => setMemory(e.target.value)} />
      </Field>
      <div className="faint small">
        Before sending mail, make sure your provider allows outgoing connections on port 25 and set the server's reverse DNS
        (PTR) to the mail host name.
      </div>
      <div>
        <button className="btn primary" disabled={create.isPending || withDocker.length === 0}>
          Set up
        </button>
      </div>
    </form>
  );
}

function ServerCard({
  hostname,
  serverId,
  appId,
  tls,
}: {
  hostname: string;
  serverId: number;
  appId: number;
  tls?: { not_after?: string; error?: string };
}) {
  const servers = useQuery({ queryKey: ["servers"], queryFn: api.servers });
  const status = useQuery({ queryKey: ["app-status", appId], queryFn: () => api.appStatus(appId), refetchInterval: 15_000 });
  const server = servers.data?.servers.find((s) => s.id === serverId);
  const state = status.data?.status.state;
  return (
    <div className="card card-body stack">
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <div className="row">
          <h2 className="mono">{hostname}</h2>
          {state === "running" ? (
            <span className="badge ok">running</span>
          ) : state ? (
            <span className="badge warn">{state}</span>
          ) : null}
        </div>
        <Link to={`/apps/${appId}`} className="btn sm">
          Deployments, logs, backups
        </Link>
      </div>
      <dl className="kv">
        <dt>Server</dt>
        <dd>
          <Link to={`/servers/${serverId}`}>{server?.name ?? `#${serverId}`}</Link>
          {server?.dns_ips.length ? <span className="faint"> · {server.dns_ips.join(", ")}</span> : null}
        </dd>
        <dt>TLS certificate</dt>
        <dd>
          {tls?.not_after ? (
            <>valid until {formatDate(tls.not_after)} (the proxy's certificate for {hostname}, renewed with it)</>
          ) : tls?.error ? (
            <span className="muted">{tls.error}</span>
          ) : (
            <span className="muted">waiting for the proxy to obtain one</span>
          )}
        </dd>
        <dt>Mail clients</dt>
        <dd>
          IMAP <span className="mono">{hostname}:993</span> (SSL/TLS) · SMTP <span className="mono">{hostname}:465</span>{" "}
          (SSL/TLS) or <span className="mono">587</span> (STARTTLS) · user name: the full address. Most clients find these
          settings themselves.
        </dd>
      </dl>
    </div>
  );
}

function OutgoingCard({ settings, admin }: { settings: MailSettings; admin: boolean }) {
  const [editing, setEditing] = useState(false);
  const r = settings.relay;
  return (
    <div className="card card-body stack">
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <h2>Outgoing mail</h2>
        {admin && (
          <button className="btn sm" onClick={() => setEditing(true)}>
            <Icon name="edit" size={14} /> Edit
          </button>
        )}
      </div>
      <dl className="kv">
        <dt>Delivery</dt>
        <dd>
          {r ? (
            <>
              Through the relay <span className="mono">{`${r.host}:${r.port}`}</span> ({r.tls === "implicit" ? "TLS" : "STARTTLS"}
              {r.username ? <>, as <span className="mono">{r.username}</span></> : ", without login"}
              {r.insecure ? ", certificate not verified" : ""})
            </>
          ) : (
            "Directly to the recipients' mail servers (port 25)"
          )}
        </dd>
        <dt>Send limit</dt>
        <dd>
          {settings.send_limit
            ? `${settings.send_limit} messages per mailbox per hour`
            : "No limit"}
          <span className="faint small"> · a brake on a mailbox whose password was stolen</span>
        </dd>
      </dl>
      {editing && <OutgoingModal settings={settings} onClose={() => setEditing(false)} />}
    </div>
  );
}

function OutgoingModal({ settings, onClose }: { settings: MailSettings; onClose: () => void }) {
  const qc = useQueryClient();
  const r = settings.relay;
  const [useRelay, setUseRelay] = useState(!!r);
  const [host, setHost] = useState(r?.host ?? "");
  const [tls, setTLS] = useState<"implicit" | "starttls">(r?.tls ?? "starttls");
  const [port, setPort] = useState(String(r?.port ?? 587));
  const [username, setUsername] = useState(r?.username ?? "");
  const [password, setPassword] = useState("");
  const [insecure, setInsecure] = useState(r?.insecure ?? false);
  const [limit, setLimit] = useState(String(settings.send_limit));
  const save = useMutation({
    mutationFn: () => {
      let relay: MailRelayInput | null = null;
      if (useRelay) {
        relay = { host: host.trim(), port: Number(port) || 0, tls, username: username.trim(), insecure };
        if (password) relay.password = password;
      }
      return api.updateMailSettings({ relay, send_limit: Number(limit) || 0 });
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["mail"] });
      onClose();
    },
  });
  return (
    <Modal
      title="Outgoing mail"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="outgoing-form" disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <form
        id="outgoing-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        <Field
          label="Delivery"
          help="A relay (Amazon SES, Mailgun, Postmark, your provider's SMTP…) helps when port 25 is blocked or the server's address has a poor reputation."
        >
          <div className="segmented">
            <button type="button" className={useRelay ? "" : "active"} onClick={() => setUseRelay(false)}>
              Direct
            </button>
            <button type="button" className={useRelay ? "active" : ""} onClick={() => setUseRelay(true)}>
              Through a relay
            </button>
          </div>
        </Field>
        {useRelay && (
          <>
            <Field label="Relay host">
              <input className="input mono" value={host} onChange={(e) => setHost(e.target.value)} placeholder="smtp.example.net" required />
            </Field>
            <Field label="Security" help="Credentials are only sent over TLS.">
              <select
                className="input"
                value={tls}
                onChange={(e) => {
                  const v = e.target.value as "implicit" | "starttls";
                  setTLS(v);
                  if (port === "587" || port === "465") setPort(v === "implicit" ? "465" : "587");
                }}
              >
                <option value="starttls">STARTTLS (usually port 587)</option>
                <option value="implicit">TLS (usually port 465)</option>
              </select>
            </Field>
            <Field label="Port">
              <input className="input mono" type="number" min={1} max={65535} value={port} onChange={(e) => setPort(e.target.value)} required />
            </Field>
            <Field label="User name" help="Empty if the relay accepts this server without a login.">
              <input className="input mono" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="off" />
            </Field>
            {username.trim() && (
              <Field label="Password" help={r?.has_password ? "Leave empty to keep the current one." : undefined}>
                <input
                  className="input mono"
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  autoComplete="new-password"
                  required={!r?.has_password}
                />
              </Field>
            )}
            <label className="row small">
              <input type="checkbox" checked={insecure} onChange={(e) => setInsecure(e.target.checked)} />
              Do not verify the relay's certificate (only for a relay on your own network)
            </label>
          </>
        )}
        <Field label="Send limit" help="Messages each mailbox may send per hour; 0 for no limit.">
          <input className="input mono" type="number" min={0} value={limit} onChange={(e) => setLimit(e.target.value)} />
        </Field>
      </form>
    </Modal>
  );
}

const checkLabels: Record<MailCheckResult["check"], string> = {
  outbound_smtp: "Outgoing port 25",
  hostname: "Host name",
  ptr: "Reverse DNS",
  blocklist: "Blocklists",
  mx: "MX",
  spf: "SPF",
  dkim: "DKIM",
  dmarc: "DMARC",
};

const statusBadges: Record<CheckStatus, [string, string]> = {
  ok: ["badge ok", "ok"],
  warn: ["badge warn", "warning"],
  fail: ["badge err", "problem"],
  unknown: ["badge plain", "unknown"],
};

function ChecksCard({ checks, admin }: { checks: MailChecks | null; admin: boolean }) {
  const qc = useQueryClient();
  const run = useMutation({
    mutationFn: api.mailChecks,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["mail"] }),
  });
  const results = checks?.results ?? [];
  const problems = results.filter((r) => r.status === "fail").length;
  const warnings = results.filter((r) => r.status === "warn").length;
  // Rows of the server first, then each domain under its name.
  const groups: [string, MailCheckResult[]][] = [];
  for (const r of results) {
    const g = r.domain ?? "";
    const last = groups[groups.length - 1];
    if (last && last[0] === g) last[1].push(r);
    else groups.push([g, [r]]);
  }
  return (
    <div className="card">
      <div className="card-body row wrap" style={{ justifyContent: "space-between" }}>
        <div className="row wrap">
          <h2>Deliverability</h2>
          {checks && !checks.error && results.length > 0 &&
            (problems > 0 ? (
              <span className="badge err">
                {problems} problem{problems > 1 ? "s" : ""}
              </span>
            ) : warnings > 0 ? (
              <span className="badge warn">
                {warnings} warning{warnings > 1 ? "s" : ""}
              </span>
            ) : (
              <span className="badge ok">no problems</span>
            ))}
          {checks && <span className="faint small">checked {formatDate(checks.at)}</span>}
        </div>
        {admin && (
          <button className="btn sm" onClick={() => run.mutate()} disabled={run.isPending}>
            {run.isPending ? "Checking…" : "Check now"}
          </button>
        )}
      </div>
      {run.error && (
        <div className="card-body" style={{ paddingTop: 0 }}>
          <ErrorBox error={run.error} />
        </div>
      )}
      {!checks ? (
        <div className="card-body muted" style={{ paddingTop: 0 }}>
          Not checked yet. The panel checks once a day whether other servers will accept this server's mail: outgoing port
          25, reverse DNS, blocklists, and the MX, SPF, DKIM and DMARC records of each domain.
        </div>
      ) : checks.error ? (
        <div className="card-body" style={{ paddingTop: 0 }}>
          <div className="alert err">The check failed: {checks.error}</div>
        </div>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <tbody>
              {groups.map(([domain, rows]) => [
                <tr key={`h-${domain}`}>
                  <th colSpan={3}>{domain || "Server"}</th>
                </tr>,
                ...rows.map((r, i) => (
                  <tr key={`${domain}-${i}`}>
                    <td style={{ width: 1 }}>
                      <span className={statusBadges[r.status][0]}>{statusBadges[r.status][1]}</span>
                    </td>
                    <td style={{ whiteSpace: "nowrap" }}>
                      {checkLabels[r.check] ?? r.check}
                      {r.target && !r.domain && r.check !== "hostname" && r.check !== "outbound_smtp" && (
                        <div className="mono faint small">{r.target}</div>
                      )}
                    </td>
                    <td className="small" style={{ overflowWrap: "anywhere" }}>
                      {r.detail}
                    </td>
                  </tr>
                )),
              ])}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function AddDomainModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const add = useMutation({
    mutationFn: () => api.createMailDomain(name.trim()),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["mail"] });
      qc.invalidateQueries({ queryKey: ["dns"] });
      onClose();
    },
  });
  return (
    <Modal
      title="Add mail domain"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="mail-domain-form" disabled={add.isPending}>
            Add domain
          </button>
        </>
      }
    >
      <form
        id="mail-domain-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          add.mutate();
        }}
      >
        <ErrorBox error={add.error} />
        <Field
          label="Domain"
          help="Addresses will be name@this domain. If its DNS zone is under DNS in Basalt, the MX, SPF, DKIM and DMARC records are published automatically."
        >
          <input className="input mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="example.com" required autoFocus />
        </Field>
      </form>
    </Modal>
  );
}

function recordData(r: DNSRecordInput): string {
  switch (r.type) {
    case "MX":
      return `${r.priority ?? 0} ${r.value}`;
    case "SRV":
      return `${r.priority ?? 0} ${r.weight ?? 0} ${r.port ?? 0} ${r.value}`;
    default:
      return r.value;
  }
}

function DomainCard({ domain, hostname, user }: { domain: MailDomain; hostname: string; user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const admin = can(user, "admin");
  const boxes = useQuery({ queryKey: ["mailboxes", domain.id], queryFn: () => api.mailboxes(domain.id) });
  const [editing, setEditing] = useState<Mailbox | "new" | null>(null);
  const fwds = useQuery({ queryKey: ["forwarders", domain.id], queryFn: () => api.forwarders(domain.id) });
  const [editingFwd, setEditingFwd] = useState<Forwarder | "new" | null>(null);
  const [editingCatchAll, setEditingCatchAll] = useState(false);
  const [showDNS, setShowDNS] = useState(false);
  const refresh = () => qc.invalidateQueries({ queryKey: ["mailboxes", domain.id] });

  const removeDomain = async () => {
    const ok = await confirm({
      title: `Remove ${domain.name}?`,
      body: <p>The mail server stops accepting mail for {domain.name}, and its DNS records are removed from Basalt's zones.</p>,
      confirm: "Remove",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteMailDomain(domain.id);
      qc.invalidateQueries({ queryKey: ["mail"] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };
  const removeBox = async (b: Mailbox) => {
    const ok = await confirm({
      title: `Delete ${b.address}?`,
      body: <p>The mailbox and all its mail are deleted. This cannot be undone.</p>,
      confirm: "Delete",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteMailbox(domain.id, b.id);
      refresh();
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };

  const removeFwd = async (f: Forwarder) => {
    const ok = await confirm({
      title: `Delete ${f.address}?`,
      body: <p>Mail to {f.address} is refused from now on.</p>,
      confirm: "Delete",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteForwarder(domain.id, f.id);
      qc.invalidateQueries({ queryKey: ["forwarders", domain.id] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };

  return (
    <div className="card">
      <div className="card-body row wrap" style={{ justifyContent: "space-between" }}>
        <div className="row">
          <h2>{domain.name}</h2>
          {domain.managed_dns ? <span className="badge ok">DNS automatic</span> : <span className="badge warn">DNS manual</span>}
        </div>
        <div className="row">
          <button className="btn sm" onClick={() => setShowDNS(!showDNS)}>
            DNS records
          </button>
          {admin && (
            <>
              <button className="btn sm primary" onClick={() => setEditing("new")}>
                <Icon name="plus" size={14} /> Mailbox
              </button>
              <button className="btn sm" onClick={() => setEditingFwd("new")}>
                <Icon name="plus" size={14} /> Forwarder
              </button>
              <button className="btn sm icon ghost" title="Remove domain" onClick={removeDomain}>
                <Icon name="trash" size={14} />
              </button>
            </>
          )}
        </div>
      </div>
      {showDNS && (
        <div className="card-body" style={{ paddingTop: 0 }}>
          <p className="faint small" style={{ marginTop: 0 }}>
            {domain.managed_dns
              ? "Published in your Basalt DNS zone (your own records at the same names take precedence). DKIM keys rotate; the panel keeps the records current."
              : `Add these records at the DNS provider of ${domain.name}, and an A record for ${hostname} pointing to the mail server. DKIM keys rotate: check back here.`}
          </p>
          <div className="table-wrap">
            <table className="table">
              <tbody>
                {domain.dns_records.map((r, i) => (
                  <tr key={i}>
                    <td className="mono small" style={{ overflowWrap: "anywhere" }}>
                      {r.name}
                    </td>
                    <td>{r.type}</td>
                    <td className="mono small" style={{ overflowWrap: "anywhere", maxWidth: 420 }}>
                      {recordData(r)}
                    </td>
                    <td className="num">
                      <CopyButton text={recordData(r)} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
      <div className="table-wrap">
        {boxes.isPending ? (
          <Loading />
        ) : boxes.error ? (
          <div className="card-body">
            <ErrorBox error={boxes.error} />
          </div>
        ) : boxes.data.mailboxes.length === 0 ? (
          <div className="card-body muted">No mailboxes yet.</div>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Address</th>
                <th>Aliases</th>
                <th>Storage</th>
                {admin && <th />}
              </tr>
            </thead>
            <tbody>
              {boxes.data.mailboxes.map((b) => (
                <tr key={b.id}>
                  <td>
                    <span className="mono">{b.address}</span>
                    {b.name && <div className="faint small">{b.name}</div>}
                  </td>
                  <td className="mono small">{b.aliases.length ? b.aliases.map((a) => `${a}@${domain.name}`).join(", ") : "—"}</td>
                  <td className="muted small">
                    {b.used_mb.toFixed(1)} MB{b.quota_mb ? ` of ${b.quota_mb} MB` : ""}
                  </td>
                  {admin && (
                    <td className="num">
                      <div className="row" style={{ justifyContent: "flex-end" }}>
                        <button className="btn sm icon ghost" title="Edit" onClick={() => setEditing(b)}>
                          <Icon name="edit" size={14} />
                        </button>
                        <button className="btn sm icon ghost" title="Delete" onClick={() => removeBox(b)}>
                          <Icon name="trash" size={14} />
                        </button>
                      </div>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {fwds.data && fwds.data.forwarders.length > 0 && (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Forwarder</th>
                <th>Delivered to</th>
                {admin && <th />}
              </tr>
            </thead>
            <tbody>
              {fwds.data.forwarders.map((f) => (
                <tr key={f.id}>
                  <td className="mono">{f.address}</td>
                  <td className="mono small" style={{ overflowWrap: "anywhere" }}>
                    {f.recipients.join(", ")}
                  </td>
                  {admin && (
                    <td className="num">
                      <div className="row" style={{ justifyContent: "flex-end" }}>
                        <button className="btn sm icon ghost" title="Edit" onClick={() => setEditingFwd(f)}>
                          <Icon name="edit" size={14} />
                        </button>
                        <button className="btn sm icon ghost" title="Delete" onClick={() => removeFwd(f)}>
                          <Icon name="trash" size={14} />
                        </button>
                      </div>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {fwds.error && (
        <div className="card-body">
          <ErrorBox error={fwds.error} />
        </div>
      )}
      <div className="card-body row wrap small" style={{ justifyContent: "space-between" }}>
        <span className="muted">
          Mail to unknown addresses:{" "}
          {domain.catch_all ? (
            <>
              delivered to <span className="mono">{`${domain.catch_all}@${domain.name}`}</span> (catch-all)
            </>
          ) : (
            "refused"
          )}
        </span>
        {admin && (
          <button className="btn sm ghost" onClick={() => setEditingCatchAll(true)}>
            Change
          </button>
        )}
      </div>
      {editingCatchAll && (
        <CatchAllModal domain={domain} mailboxes={boxes.data?.mailboxes ?? []} onClose={() => setEditingCatchAll(false)} />
      )}
      {editingFwd && (
        <ForwarderModal
          domain={domain}
          forwarder={editingFwd === "new" ? null : editingFwd}
          onClose={() => setEditingFwd(null)}
          onSaved={() => qc.invalidateQueries({ queryKey: ["forwarders", domain.id] })}
        />
      )}
      {editing && (
        <MailboxModal domain={domain} mailbox={editing === "new" ? null : editing} onClose={() => setEditing(null)} onSaved={refresh} />
      )}
    </div>
  );
}

// newPassword makes a 20-character random password.
function newPassword(): string {
  const chars = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789";
  const b = new Uint8Array(20);
  crypto.getRandomValues(b);
  return Array.from(b, (x) => chars[x % chars.length]).join("");
}

function MailboxModal({
  domain,
  mailbox,
  onClose,
  onSaved,
}: {
  domain: MailDomain;
  mailbox: Mailbox | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [local, setLocal] = useState("");
  const [name, setName] = useState(mailbox?.name ?? "");
  const [password, setPassword] = useState(mailbox ? "" : newPassword());
  const [aliases, setAliases] = useState((mailbox?.aliases ?? []).join(" "));
  const [quota, setQuota] = useState(mailbox?.quota_mb ? String(mailbox.quota_mb) : "");
  const [saved, setSaved] = useState<string | null>(null);
  const save = useMutation({
    mutationFn: async () => {
      const b: MailboxInput = {
        name: name.trim(),
        aliases: aliases.split(/[\s,]+/).filter(Boolean).map((a) => a.replace(/@.*$/, "")),
        quota_mb: Number(quota) || 0,
      };
      if (password) b.password = password;
      if (mailbox) await api.updateMailbox(domain.id, mailbox.id, b);
      else await api.createMailbox(domain.id, { ...b, local: local.trim() });
    },
    onSuccess: () => {
      onSaved();
      if (password) setSaved(password);
      else onClose();
    },
  });
  if (saved) {
    return (
      <Modal
        title={mailbox ? "Password changed" : "Mailbox created"}
        onClose={onClose}
        footer={
          <button className="btn primary" onClick={onClose}>
            Done
          </button>
        }
      >
        <div className="stack">
          <p style={{ margin: 0 }}>Give the password to its owner now: it is not shown again.</p>
          <dl className="kv">
            <dt>Address</dt>
            <dd className="mono">{mailbox?.address ?? `${local.trim()}@${domain.name}`}</dd>
            <dt>Password</dt>
            <dd>
              <div className="row">
                <code>{saved}</code>
                <CopyButton text={saved} />
              </div>
            </dd>
          </dl>
        </div>
      </Modal>
    );
  }
  return (
    <Modal
      title={mailbox ? `Edit ${mailbox.address}` : `New mailbox at ${domain.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="mailbox-form" disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <form
        id="mailbox-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        {!mailbox && (
          <Field label="Address">
            <div className="row">
              <input className="input mono" value={local} onChange={(e) => setLocal(e.target.value)} placeholder="alice" required autoFocus />
              <span className="mono muted">@{domain.name}</span>
            </div>
          </Field>
        )}
        <Field label="Display name" help="Optional.">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="Password" help={mailbox ? "Leave empty to keep the current one." : "At least 10 characters."}>
          <div className="row">
            <input className="input mono" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
            <button type="button" className="btn sm" onClick={() => setPassword(newPassword())}>
              Generate
            </button>
          </div>
        </Field>
        <Field label="Aliases" help={`Other addresses at ${domain.name} delivered here, e.g. info sales.`}>
          <input className="input mono" value={aliases} onChange={(e) => setAliases(e.target.value)} placeholder="info sales" />
        </Field>
        <Field label="Storage limit (MB)" help="Empty for no limit.">
          <input className="input mono" type="number" min={0} value={quota} onChange={(e) => setQuota(e.target.value)} />
        </Field>
      </form>
    </Modal>
  );
}

function CatchAllModal({ domain, mailboxes, onClose }: { domain: MailDomain; mailboxes: Mailbox[]; onClose: () => void }) {
  const qc = useQueryClient();
  const [local, setLocal] = useState(domain.catch_all);
  const save = useMutation({
    mutationFn: () => api.updateMailDomain(domain.id, { catch_all: local }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["mail"] });
      onClose();
    },
  });
  // Mailbox addresses and their aliases, as local parts.
  const locals = mailboxes.flatMap((b) => [b.address.replace(/@.*$/, ""), ...b.aliases]);
  if (domain.catch_all && !locals.includes(domain.catch_all)) locals.push(domain.catch_all);
  return (
    <Modal
      title={`Unknown addresses at ${domain.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" onClick={() => save.mutate()} disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <div className="form">
        <ErrorBox error={save.error} />
        <Field
          label="Catch-all"
          help="Mail to addresses that do not exist goes to this mailbox. Catch-all mailboxes receive a lot of spam: refusing is usually better."
        >
          <select className="input mono" value={local} onChange={(e) => setLocal(e.target.value)}>
            <option value="">Refuse the mail</option>
            {locals.map((l) => (
              <option key={l} value={l}>
                {l}@{domain.name}
              </option>
            ))}
          </select>
        </Field>
      </div>
    </Modal>
  );
}

function ForwarderModal({
  domain,
  forwarder,
  onClose,
  onSaved,
}: {
  domain: MailDomain;
  forwarder: Forwarder | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [local, setLocal] = useState("");
  const [recipients, setRecipients] = useState((forwarder?.recipients ?? []).join("\n"));
  const save = useMutation({
    mutationFn: async () => {
      const list = recipients.split(/[\s,;]+/).filter(Boolean);
      if (forwarder) await api.updateForwarder(domain.id, forwarder.id, list);
      else await api.createForwarder(domain.id, { local: local.trim(), recipients: list });
    },
    onSuccess: () => {
      onSaved();
      onClose();
    },
  });
  return (
    <Modal
      title={forwarder ? `Edit ${forwarder.address}` : `New forwarder at ${domain.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="forwarder-form" disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <form
        id="forwarder-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        {!forwarder && (
          <Field label="Address" help="Mail to it is passed on and not stored; it cannot be logged into.">
            <div className="row">
              <input className="input mono" value={local} onChange={(e) => setLocal(e.target.value)} placeholder="sales" required autoFocus />
              <span className="mono muted">@{domain.name}</span>
            </div>
          </Field>
        )}
        <Field
          label="Forward to"
          help="One address per line: mailboxes here or anywhere else (up to 50). Forwarding outside can fail the recipients' SPF checks; DKIM-signed mail still passes."
        >
          <textarea
            className="input mono"
            rows={4}
            value={recipients}
            onChange={(e) => setRecipients(e.target.value)}
            placeholder={"alice@" + domain.name + "\npartner@example.net"}
            required
          />
        </Field>
      </form>
    </Modal>
  );
}
