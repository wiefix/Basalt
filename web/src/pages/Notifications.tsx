import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, can, type ChannelInput, type ChannelKind, type NotificationChannel, type User } from "../api";
import { CodeBox, ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, timeAgo, useConfirm, useToast } from "../components/ui";

const kindLabel: Record<ChannelKind, string> = {
  webhook: "Webhook",
  slack: "Slack",
  discord: "Discord",
  telegram: "Telegram",
  email: "Email",
};

// where tells where a channel delivers, without secrets.
function where(c: NotificationChannel): string {
  switch (c.kind) {
    case "webhook":
      return c.url ?? "";
    case "slack":
    case "discord":
      return c.host ? `${c.host} (webhook)` : "";
    case "telegram":
      return `chat ${c.chat_id}`;
    case "email":
      return `${(c.to ?? []).join(", ")} via ${c.smtp_host}:${c.smtp_port}`;
  }
}

export function NotificationsPage({ user }: { user: User }) {
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const data = useQuery({ queryKey: ["notifications"], queryFn: api.notifications, enabled: can(user, "admin"), refetchInterval: 15_000 });
  const [editing, setEditing] = useState<NotificationChannel | "new" | null>(null);
  const [testing, setTesting] = useState<number | null>(null);
  if (!can(user, "admin")) return <div className="empty">You do not have access to this page.</div>;
  if (data.isPending) return <Loading />;
  if (data.error) return <ErrorBox error={data.error} />;
  const { channels, events, panel_url } = data.data;
  const eventLabel = (id: string) => events.find((e) => e.id === id)?.label ?? id;

  const test = async (c: NotificationChannel) => {
    setTesting(c.id);
    try {
      const r = await api.testChannel(c.id);
      toast(r.ok ? `Test message sent to ${c.name}` : `${c.name}: ${r.error}`, !r.ok);
    } catch (e) {
      toast(errorMessage(e), true);
    } finally {
      setTesting(null);
      qc.invalidateQueries({ queryKey: ["notifications"] });
    }
  };
  const remove = async (c: NotificationChannel) => {
    const ok = await confirm({ title: `Delete ${c.name}?`, body: <p>It stops receiving notifications.</p>, confirm: "Delete", danger: true });
    if (!ok) return;
    try {
      await api.deleteChannel(c.id);
      qc.invalidateQueries({ queryKey: ["notifications"] });
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Notifications</h1>
          <div className="sub">Where the panel tells you about failures: health checks, deployments, backups.</div>
        </div>
        <button className="btn primary" onClick={() => setEditing("new")}>
          <Icon name="plus" /> Add channel
        </button>
      </div>
      {channels.length === 0 ? (
        <div className="card empty">
          <h3>No channels</h3>
          <p>Send alerts to email, Telegram, Slack, Discord or any webhook.</p>
        </div>
      ) : (
        <div className="card table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Channel</th>
                <th className="hide-sm">Events</th>
                <th>Last delivery</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {channels.map((c) => (
                <tr key={c.id}>
                  <td>
                    <strong>{c.name}</strong> <span className="badge plain">{kindLabel[c.kind]}</span>
                    <div className="mono faint small" style={{ overflowWrap: "anywhere" }}>
                      {where(c)}
                    </div>
                  </td>
                  <td className="hide-sm small muted">
                    {c.events.length ? c.events.map(eventLabel).join("; ") : <span className="faint">none: only tests</span>}
                  </td>
                  <td className="small">
                    {c.last_error ? (
                      <span className="badge err" title={c.last_error}>
                        failed {timeAgo(c.last_error_at)}
                      </span>
                    ) : c.last_sent_at ? (
                      <span className="badge ok">sent {timeAgo(c.last_sent_at)}</span>
                    ) : (
                      <span className="faint">never</span>
                    )}
                    {c.last_error && (
                      <div className="faint small" style={{ overflowWrap: "anywhere" }}>
                        {c.last_error}
                      </div>
                    )}
                    {c.queued > 0 && (
                      <div className="small muted" title="Tried again until about 2.5 hours after the first attempt">
                        {c.queued === 1 ? "1 message waits" : `${c.queued} messages wait`} to be tried again
                      </div>
                    )}
                  </td>
                  <td className="num">
                    <div className="row" style={{ justifyContent: "flex-end" }}>
                      <button className="btn sm" disabled={testing === c.id} onClick={() => test(c)}>
                        {testing === c.id ? "Sending…" : "Test"}
                      </button>
                      <button className="btn sm icon ghost" title="Edit" onClick={() => setEditing(c)}>
                        <Icon name="edit" size={14} />
                      </button>
                      <button className="btn sm icon ghost" title="Delete" onClick={() => remove(c)}>
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
      <PanelURLCard url={panel_url} />
      <p className="faint small">
        A failed delivery is tried again after 10 seconds, a minute, 5 and 30 minutes and 2 hours, also after a restart of the panel. Three or more messages of one kind within 10 seconds (a server down fails many health checks) arrive as one. A health check notifies when it goes down
        and when it comes back.
      </p>
      {editing && (
        <ChannelModal channel={editing === "new" ? null : editing} events={events} onClose={() => setEditing(null)} />
      )}
    </>
  );
}

function ChannelModal({
  channel,
  events,
  onClose,
}: {
  channel: NotificationChannel | null;
  events: { id: string; label: string }[];
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const c = channel;
  const [kind, setKind] = useState<ChannelKind>(c?.kind ?? "email");
  const [name, setName] = useState(c?.name ?? "");
  const [chosen, setChosen] = useState<string[]>(c?.events ?? events.map((e) => e.id));
  const [url, setURL] = useState(c?.kind === "webhook" ? (c.url ?? "") : "");
  const [secret, setSecret] = useState(c?.secret ?? "");
  const [allowPrivate, setAllowPrivate] = useState(c?.allow_private ?? false);
  const [token, setToken] = useState("");
  const [chatID, setChatID] = useState(c?.chat_id ?? "");
  const [host, setHost] = useState(c?.smtp_host ?? "");
  const [port, setPort] = useState(String(c?.smtp_port ?? 587));
  const [tls, setTLS] = useState<string>(c?.smtp_tls ?? "starttls");
  const [smtpUser, setSMTPUser] = useState(c?.smtp_user ?? "");
  const [password, setPassword] = useState("");
  const [insecure, setInsecure] = useState(c?.smtp_insecure ?? false);
  const [from, setFrom] = useState(c?.from ?? "");
  const [to, setTo] = useState((c?.to ?? []).join("\n"));
  const save = useMutation({
    mutationFn: () => {
      const b: ChannelInput = { name: name.trim(), kind, events: chosen };
      if (kind === "webhook") {
        Object.assign(b, { url: url.trim(), allow_private: allowPrivate });
        // A new webhook without a secret gets a generated one.
        if (c || secret) b.secret = secret;
      }
      if (kind === "slack" || kind === "discord") b.url = url.trim();
      if (kind === "telegram") Object.assign(b, { token: token.trim(), chat_id: chatID.trim() });
      if (kind === "email")
        Object.assign(b, {
          smtp_host: host.trim(),
          smtp_port: Number(port) || 0,
          smtp_tls: tls,
          smtp_user: smtpUser.trim(),
          smtp_password: password,
          smtp_insecure: insecure,
          from: from.trim(),
          to: to.split(/[\s,;]+/).filter(Boolean),
        });
      return c ? api.updateChannel(c.id, b) : api.createChannel(b);
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["notifications"] });
      onClose();
    },
  });
  const keep = c ? "Leave empty to keep the current one." : undefined;
  return (
    <Modal
      title={c ? `Edit ${c.name}` : "New notification channel"}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="channel-form" disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <form
        id="channel-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        {!c && (
          <div className="segmented" style={{ flexWrap: "wrap" }}>
            {(Object.keys(kindLabel) as ChannelKind[]).map((k) => (
              <button type="button" key={k} className={kind === k ? "active" : ""} onClick={() => setKind(k)}>
                {kindLabel[k]}
              </button>
            ))}
          </div>
        )}
        <Field label="Name">
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="ops" required autoFocus />
        </Field>
        {kind === "webhook" && (
          <>
            <Field label="URL" help="Receives a POST with JSON: event, title, text, at.">
              <input className="input mono" value={url} onChange={(e) => setURL(e.target.value)} placeholder="https://example.com/hooks/basalt" required />
            </Field>
            <Field
              label="Signing secret"
              help={
                <>
                  The body is signed: <code>X-Basalt-Signature: sha256=&lt;HMAC-SHA256 of the body&gt;</code>.{" "}
                  {c ? "Empty: no signature." : "Generated when left empty."}
                </>
              }
            >
              <input className="input mono" value={secret} onChange={(e) => setSecret(e.target.value)} autoComplete="off" />
            </Field>
            {c?.secret && <CodeBox text={c.secret} />}
            <label className="row small">
              <input type="checkbox" checked={allowPrivate} onChange={(e) => setAllowPrivate(e.target.checked)} />
              The receiver is on a private network (allows private addresses and plain http)
            </label>
          </>
        )}
        {(kind === "slack" || kind === "discord") && (
          <Field
            label="Webhook URL"
            help={
              (kind === "slack"
                ? "Slack: an app's Incoming Webhooks, https://hooks.slack.com/services/…"
                : "Discord: channel settings → Integrations → Webhooks, https://discord.com/api/webhooks/…") +
              (c ? ` It is secret and not shown. ${keep}` : " It is stored encrypted.")
            }
          >
            <input className="input mono" value={url} onChange={(e) => setURL(e.target.value)} required={!c} autoComplete="off" />
          </Field>
        )}
        {kind === "telegram" && (
          <>
            <Field label="Bot token" help={`From @BotFather. ${keep ?? "Stored encrypted."}`}>
              <input className="input mono" value={token} onChange={(e) => setToken(e.target.value)} placeholder="123456789:AA…" required={!c} autoComplete="off" />
            </Field>
            <Field label="Chat ID" help="Add the bot to the chat; a group's ID looks like -1001234567890, a public channel's like @name.">
              <input className="input mono" value={chatID} onChange={(e) => setChatID(e.target.value)} required />
            </Field>
          </>
        )}
        {kind === "email" && (
          <>
            <div className="grid-2">
              <Field label="SMTP server">
                <input className="input mono" value={host} onChange={(e) => setHost(e.target.value)} placeholder="smtp.example.com" required />
              </Field>
              <Field label="Security">
                <select
                  className="input"
                  value={tls}
                  onChange={(e) => {
                    setTLS(e.target.value);
                    setPort(e.target.value === "implicit" ? "465" : e.target.value === "starttls" ? "587" : "25");
                  }}
                >
                  <option value="starttls">STARTTLS (port 587)</option>
                  <option value="implicit">TLS (port 465)</option>
                  <option value="none">None (local relay, no login)</option>
                </select>
              </Field>
            </div>
            <div className="grid-2">
              <Field label="Port">
                <input className="input mono" type="number" min={1} max={65535} value={port} onChange={(e) => setPort(e.target.value)} />
              </Field>
              <Field label="User name" help="Empty: no login.">
                <input className="input mono" value={smtpUser} onChange={(e) => setSMTPUser(e.target.value)} autoComplete="off" />
              </Field>
            </div>
            {smtpUser.trim() && (
              <Field label="Password" help={keep}>
                <input className="input mono" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="new-password" />
              </Field>
            )}
            <Field label="From">
              <input className="input mono" value={from} onChange={(e) => setFrom(e.target.value)} placeholder="basalt@example.com" required />
            </Field>
            <Field label="To" help="One address per line, up to 20.">
              <textarea className="input mono" rows={2} value={to} onChange={(e) => setTo(e.target.value)} required />
            </Field>
            {tls !== "none" && (
              <label className="row small">
                <input type="checkbox" checked={insecure} onChange={(e) => setInsecure(e.target.checked)} />
                Do not verify the server's certificate
              </label>
            )}
          </>
        )}
        <Field label="Send">
          <div className="stack" style={{ gap: 6 }}>
            {events.map((e) => (
              <label key={e.id} className="row small">
                <input
                  type="checkbox"
                  checked={chosen.includes(e.id)}
                  onChange={(ev) => setChosen(ev.target.checked ? [...chosen, e.id] : chosen.filter((x) => x !== e.id))}
                />
                {e.label}
              </label>
            ))}
          </div>
        </Field>
        {c?.last_error && (
          <div className="alert err small">
            Last delivery failed {formatDate(c.last_error_at)}: {c.last_error}
          </div>
        )}
      </form>
    </Modal>
  );
}

// PanelURLCard sets the address links in messages point to.
function PanelURLCard({ url }: { url: string }) {
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(url || window.location.origin);
  const save = useMutation({
    mutationFn: () => api.setPanelURL(value.trim()),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["notifications"] });
      setEditing(false);
    },
  });
  return (
    <div className="card card-body stack" style={{ marginTop: 16 }}>
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <div>
          <h2>Links in messages</h2>
          <p className="muted small" style={{ margin: "4px 0 0" }}>
            {url ? (
              <>
                Messages link to the page concerned at <span className="mono">{url}</span>.
              </>
            ) : (
              "Set the panel's address to put a link to the page concerned in each message."
            )}
          </p>
        </div>
        {!editing && (
          <button className="btn sm" onClick={() => setEditing(true)}>
            <Icon name="edit" size={14} /> {url ? "Change" : "Set"}
          </button>
        )}
      </div>
      {editing && (
        <form
          className="row wrap"
          onSubmit={(e: FormEvent) => {
            e.preventDefault();
            save.mutate();
          }}
        >
          <input
            className="input mono"
            style={{ flex: 1, minWidth: 220 }}
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder="https://panel.example.com"
            autoFocus
          />
          <button className="btn sm primary" disabled={save.isPending}>
            Save
          </button>
          <button type="button" className="btn sm" onClick={() => setEditing(false)}>
            Cancel
          </button>
        </form>
      )}
      <ErrorBox error={save.error} />
    </div>
  );
}
