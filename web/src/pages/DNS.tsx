import { useState, type FormEvent } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "react-router";
import { api, can, type DNSRecord, type DNSRecordInput, type DNSServer, type DNSZone, type HealthCheck, type User } from "../api";
import { CopyButton, ErrorBox, Field, Icon, Loading, Modal, errorMessage, formatDate, useConfirm, useToast } from "../components/ui";

export function DNSPage({ user }: { user: User }) {
  const dns = useQuery({ queryKey: ["dns"], queryFn: api.dns });
  const [adding, setAdding] = useState(false);
  const [editingNS, setEditingNS] = useState(false);

  return (
    <>
      <div className="page-head">
        <div>
          <h1>DNS</h1>
          <div className="sub">Authoritative DNS for your domains, served by your own servers.</div>
        </div>
        {can(user, "admin") && (
          <button className="btn primary" onClick={() => setAdding(true)}>
            <Icon name="plus" /> Add zone
          </button>
        )}
      </div>
      {dns.isPending ? (
        <Loading />
      ) : dns.error ? (
        <ErrorBox error={dns.error} />
      ) : (
        <div className="stack">
          {dns.data.zones.length === 0 ? (
            <div className="card empty">
              <h3>No zones yet</h3>
              <p>Add a domain, create its records here, then point the domain to your nameservers at the registrar.</p>
            </div>
          ) : (
            <div className="card table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>Zone</th>
                    <th className="num">Records</th>
                    <th>Serial</th>
                    <th>Updated</th>
                  </tr>
                </thead>
                <tbody>
                  {dns.data.zones.map((z) => (
                    <tr key={z.id}>
                      <td>
                        <Link to={`/dns/${z.id}`}>
                          <strong>{z.name}</strong>
                        </Link>
                        {z.dnssec && (
                          <span className="badge ok" style={{ marginLeft: 8 }}>
                            DNSSEC
                          </span>
                        )}
                      </td>
                      <td className="num">{z.records}</td>
                      <td className="mono muted">{z.serial}</td>
                      <td className="muted">{formatDate(z.updated_at)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <div className="grid-2">
            <div className="card card-body stack">
              <div className="row" style={{ justifyContent: "space-between" }}>
                <h2>Nameservers</h2>
                {can(user, "admin") && (
                  <button className="btn sm" onClick={() => setEditingNS(true)}>
                    <Icon name="edit" size={14} /> Change
                  </button>
                )}
              </div>
              {dns.data.nameservers.length === 0 ? (
                <p className="muted" style={{ margin: 0 }}>
                  Not set. Choose host names such as <code>ns1.example.com</code> and <code>ns2.example.com</code> that
                  point to your DNS servers; new zones get NS records for them.
                </p>
              ) : (
                <>
                  <div className="stack" style={{ gap: 4 }}>
                    {dns.data.nameservers.map((n) => (
                      <code key={n}>{n}</code>
                    ))}
                  </div>
                  <p className="faint small" style={{ margin: 0 }}>
                    New zones get NS records for these names. Each name needs an A (and AAAA) record with a DNS server's
                    IP; if it lies inside one of the zones, also register it as a glue record at the registrar.
                  </p>
                </>
              )}
            </div>
            <ServersCard servers={dns.data.servers} />
          </div>
        </div>
      )}
      {adding && <NewZoneModal onClose={() => setAdding(false)} />}
      {editingNS && dns.data && <NameserversModal current={dns.data.nameservers} onClose={() => setEditingNS(false)} />}
    </>
  );
}

function ServersCard({ servers }: { servers: DNSServer[] }) {
  const serving = servers.filter((s) => s.port > 0);
  return (
    <div className="card card-body stack">
      <h2>DNS servers</h2>
      {servers.length === 0 && <p className="muted" style={{ margin: 0 }}>No servers yet.</p>}
      {servers.map((s) => (
        <div key={s.server_id} className="row wrap" style={{ justifyContent: "space-between" }}>
          <Link to={`/servers/${s.server_id}`}>{s.name}</Link>
          <span className="row">
            {s.port > 0 && s.ip && <code className="small">{s.ip}</code>}
            {s.listen_error ? (
              <span className="badge err" title={s.listen_error}>
                cannot listen
              </span>
            ) : s.port === 0 ? (
              <span className="badge plain">not serving</span>
            ) : !s.online ? (
              <span className="badge warn">offline</span>
            ) : s.error ? (
              <span className="badge err" title={s.error}>
                error
              </span>
            ) : s.in_sync ? (
              <span className="badge ok">serving</span>
            ) : (
              <span className="badge warn">updating</span>
            )}
          </span>
        </div>
      ))}
      {serving.length < 2 && (
        <p className="faint small" style={{ margin: 0 }}>
          Registrars want at least two nameservers, best on different servers. A server serves DNS when its agent runs
          with <code>--dns-listen :53</code> (the installer's <code>--with-dns</code>); every server keeps a copy of the
          zones.
        </p>
      )}
    </div>
  );
}

function NewZoneModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const create = useMutation({
    mutationFn: () => api.createZone(name.trim()),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ["dns"] });
      navigate(`/dns/${r.zone.id}`);
    },
  });
  return (
    <Modal
      title="Add zone"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="zone-form" disabled={create.isPending}>
            Add zone
          </button>
        </>
      }
    >
      <form
        id="zone-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <ErrorBox error={create.error} />
        <Field label="Domain" help="The zone answers for this domain and everything below it. Use punycode (xn--…) for international names.">
          <input className="input mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="example.com" required autoFocus />
        </Field>
      </form>
    </Modal>
  );
}

function NameserversModal({ current, onClose }: { current: string[]; onClose: () => void }) {
  const qc = useQueryClient();
  const [text, setText] = useState(current.join("\n"));
  const save = useMutation({
    mutationFn: () => api.setNameservers(text.split(/[\s,]+/).filter(Boolean)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["dns"] });
      onClose();
    },
  });
  return (
    <Modal
      title="Nameservers"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="ns-form" disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <form
        id="ns-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        <Field label="Host names, one per line" help="Used for the NS records of new zones; existing zones keep theirs.">
          <textarea className="input mono" rows={4} value={text} onChange={(e) => setText(e.target.value)} placeholder={"ns1.example.com\nns2.example.com"} autoFocus />
        </Field>
      </form>
    </Modal>
  );
}

// recordData shows a record's data the way zone files do.
function recordData(r: DNSRecordInput): string {
  switch (r.type) {
    case "MX":
      return `${r.priority ?? 0} ${r.value}`;
    case "SRV":
      return `${r.priority ?? 0} ${r.weight ?? 0} ${r.port ?? 0} ${r.value}`;
    case "CAA":
      return `${r.flags ?? 0} ${r.tag} "${r.value}"`;
    default:
      return r.value;
  }
}

function formatTTL(s: number): string {
  if (s % 86400 === 0) return `${s / 86400}d`;
  if (s % 3600 === 0) return `${s / 3600}h`;
  if (s % 60 === 0) return `${s / 60}m`;
  return `${s}s`;
}

export function ZonePage({ user }: { user: User }) {
  const id = Number(useParams().id);
  const qc = useQueryClient();
  const toast = useToast();
  const confirm = useConfirm();
  const navigate = useNavigate();
  const zone = useQuery({ queryKey: ["dns-zone", id], queryFn: () => api.zone(id) });
  const checks = useQuery({ queryKey: ["health-checks"], queryFn: api.healthChecks, refetchInterval: 15_000 });
  const [editing, setEditing] = useState<DNSRecord | "new" | null>(null);
  const [filter, setFilter] = useState("");
  const admin = can(user, "admin");

  if (zone.isPending) return <Loading />;
  if (zone.error) return <ErrorBox error={zone.error} />;
  const z = zone.data.zone;
  const fqdn = (name: string) => (name === "@" ? z.name : `${name}.${z.name}`);
  const f = filter.trim().toLowerCase();
  const match = (r: DNSRecordInput) =>
    !f || fqdn(r.name).includes(f) || r.type.toLowerCase() === f || recordData(r).toLowerCase().includes(f);
  const records = zone.data.records.filter(match);
  const checkByID = new Map((checks.data?.checks ?? []).map((c) => [c.id, c]));
  const isDown = (r: DNSRecord) => !!r.health_check && checkByID.get(r.health_check)?.status === "down";
  // As the panel does (dns.Failover): a down address is left out unless
  // every address of its name and type is down.
  const notServed = (r: DNSRecord) =>
    isDown(r) && zone.data.records.some((o) => o.name === r.name && o.type === r.type && !isDown(o));
  const auto = zone.data.auto.filter(match);
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ["dns-zone", id] });
    qc.invalidateQueries({ queryKey: ["dns"] });
  };

  const removeRecord = async (r: DNSRecord) => {
    const ok = await confirm({
      title: `Delete ${r.type} record?`,
      body: (
        <p>
          <code>{fqdn(r.name)}</code> {r.type} <code>{recordData(r)}</code>
        </p>
      ),
      confirm: "Delete",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteRecord(id, r.id);
      refresh();
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };

  const removeZone = async () => {
    const ok = await confirm({
      title: `Delete zone ${z.name}?`,
      body: <p>All its records are deleted and your servers stop answering for {z.name}. Sites and apps are not affected otherwise.</p>,
      confirm: "Delete zone",
      danger: true,
    });
    if (!ok) return;
    try {
      await api.deleteZone(id);
      qc.invalidateQueries({ queryKey: ["dns"] });
      navigate("/dns");
    } catch (e) {
      toast(errorMessage(e), true);
    }
  };

  return (
    <>
      <div className="page-head">
        <div>
          <h1>{z.name}</h1>
          <div className="sub row wrap">
            <Link to="/dns">DNS</Link>
            <span className="faint">
              serial <span className="mono">{z.serial}</span> · updated {formatDate(z.updated_at)}
            </span>
          </div>
        </div>
        {admin && (
          <div className="row">
            <button className="btn primary" onClick={() => setEditing("new")}>
              <Icon name="plus" /> Add record
            </button>
            <button className="btn icon ghost" title="Delete zone" onClick={removeZone}>
              <Icon name="trash" />
            </button>
          </div>
        )}
      </div>
      <div className="card">
        <div className="card-body" style={{ paddingBottom: 0 }}>
          <input className="input" value={filter} onChange={(e) => setFilter(e.target.value)} placeholder="Filter by name, type or value" />
        </div>
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Type</th>
                <th>Data</th>
                <th>TTL</th>
                {admin && <th />}
              </tr>
            </thead>
            <tbody>
              <tr>
                <td className="mono">{z.name}</td>
                <td>SOA</td>
                <td className="mono muted small">managed by Basalt</td>
                <td className="muted">1h</td>
                {admin && <td />}
              </tr>
              {auto.map((r) => (
                <tr key={`auto ${r.name} ${r.type} ${r.value}`}>
                  <td className="mono" style={{ overflowWrap: "anywhere" }}>
                    {fqdn(r.name)}
                  </td>
                  <td>{r.type}</td>
                  <td className="mono small" style={{ overflowWrap: "anywhere", maxWidth: 420 }}>
                    {r.value}{" "}
                    <span className="badge plain" title="Created for this domain automatically; add your own A, AAAA, CNAME or ALIAS record at this name to replace it">
                      auto · {r.source}
                    </span>
                  </td>
                  <td className="muted">{formatTTL(r.ttl)}</td>
                  {admin && <td />}
                </tr>
              ))}
              {records.map((r) => (
                <tr key={r.id}>
                  <td className="mono" style={{ overflowWrap: "anywhere" }}>
                    {fqdn(r.name)}
                  </td>
                  <td>{r.type}</td>
                  <td className="mono small" style={{ overflowWrap: "anywhere", maxWidth: 420 }}>
                    {recordData(r)}
                    {r.health_check ? (
                      <FollowsBadge check={checkByID.get(r.health_check)} notServed={notServed(r)} />
                    ) : null}
                  </td>
                  <td className="muted">{formatTTL(r.ttl)}</td>
                  {admin && (
                    <td className="num">
                      <div className="row" style={{ justifyContent: "flex-end" }}>
                        <button className="btn sm icon ghost" title="Edit" onClick={() => setEditing(r)}>
                          <Icon name="edit" size={14} />
                        </button>
                        <button className="btn sm icon ghost" title="Delete" onClick={() => removeRecord(r)}>
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
      </div>
      <DNSSECCard zone={z} admin={admin} onChanged={refresh} />
      <p className="faint small">
        Sites and apps whose domains are in this zone get A/AAAA records for their server automatically (TTL 5
        minutes). A record of your own at the same name replaces them. An A or AAAA record can follow a health check
        (see <Link to="/monitoring">Monitoring</Link>): while the check is down, its address is left out of the
        answers, unless every address of that name is down.
      </p>
      {editing && (
        <RecordModal
          zone={z.name}
          zoneId={id}
          types={zone.data.types}
          checks={checks.data?.checks ?? []}
          record={editing === "new" ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={refresh}
        />
      )}
    </>
  );
}

function FollowsBadge({ check, notServed }: { check?: HealthCheck; notServed: boolean }) {
  if (!check) return null;
  const cls = notServed ? "badge err" : check.status === "down" ? "badge warn" : check.status === "up" ? "badge ok" : "badge";
  const title = notServed
    ? `The check ${check.name} is down: this address is left out of the answers.`
    : check.status === "down"
      ? `The check ${check.name} is down, but so are the other addresses of this name: all of them are answered.`
      : `Answered while the check ${check.name} is not down.`;
  return (
    <span className={cls} style={{ marginLeft: 8 }} title={title}>
      {check.name} · {notServed ? "down, not served" : check.status}
    </span>
  );
}

function DNSSECCard({ zone, admin, onChanged }: { zone: DNSZone; admin: boolean; onChanged: () => void }) {
  const toast = useToast();
  const confirm = useConfirm();
  const [busy, setBusy] = useState(false);
  const sec = zone.dnssec;
  const set = async (enabled: boolean) => {
    if (!enabled) {
      const ok = await confirm({
        title: "Turn off DNSSEC?",
        body: (
          <>
            <p>
              First remove the DS record at your registrar and wait a day or two for it to expire from caches.
              Otherwise resolvers that validate will treat <strong>{zone.name}</strong> as broken and stop resolving it.
            </p>
            <p className="muted">Turning it on again creates a new key with a new DS record.</p>
          </>
        ),
        confirm: "Turn off",
        danger: true,
      });
      if (!ok) return;
    }
    setBusy(true);
    try {
      await api.setDNSSEC(zone.id, enabled);
      onChanged();
    } catch (e) {
      toast(errorMessage(e), true);
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="card card-body stack" style={{ marginTop: 16 }}>
      <div className="row" style={{ justifyContent: "space-between" }}>
        <div className="row">
          <h2>DNSSEC</h2>
          {sec ? <span className="badge ok">signed</span> : <span className="badge plain">off</span>}
        </div>
        {admin && (
          <button className={sec ? "btn sm ghost" : "btn sm primary"} disabled={busy} onClick={() => set(!sec)}>
            {sec ? "Turn off" : "Sign this zone"}
          </button>
        )}
      </div>
      {sec ? (
        <>
          <p className="muted" style={{ margin: 0 }}>
            Answers are signed (ECDSA P-256). To make resolvers check the signatures, add this DS record at your
            registrar. Registrars that read CDS records pick it up by themselves.
          </p>
          <dl className="kv">
            <dt>DS record</dt>
            <dd>
              {sec.ds.map((d) => (
                <div key={d} className="row" style={{ alignItems: "flex-start" }}>
                  <code style={{ overflowWrap: "anywhere" }}>{d}</code>
                  <CopyButton text={d} />
                </div>
              ))}
            </dd>
            <dt>Key tag</dt>
            <dd className="mono">{sec.key_tag}</dd>
            <dt>Algorithm</dt>
            <dd>13 (ECDSA P-256 with SHA-256)</dd>
            <dt>Digest type</dt>
            <dd>2 (SHA-256)</dd>
            <dt>DNSKEY</dt>
            <dd>
              <div className="row" style={{ alignItems: "flex-start" }}>
                <code className="small" style={{ overflowWrap: "anywhere" }}>{sec.dnskey}</code>
                <CopyButton text={sec.dnskey} />
              </div>
            </dd>
          </dl>
        </>
      ) : (
        <p className="muted" style={{ margin: 0 }}>
          Signing protects visitors from forged answers. Once on, you add one DS record at your registrar.
        </p>
      )}
    </div>
  );
}

const ttlChoices = [
  [60, "1 minute"],
  [300, "5 minutes"],
  [3600, "1 hour"],
  [86400, "1 day"],
] as const;

const valueHelp: Record<string, string> = {
  A: "IPv4 address",
  AAAA: "IPv6 address",
  CNAME: "Full host name this name is an alias of",
  ALIAS: "Full host name; A/AAAA queries get its current addresses (works at the apex)",
  MX: "Mail server host name, or . for a domain that receives no mail",
  TXT: "Text, e.g. v=spf1 mx -all; long values are split automatically",
  SRV: "Target host name",
  CAA: "e.g. letsencrypt.org, or mailto:security@example.com for iodef",
  NS: "Nameserver host name (below the apex this delegates the subdomain)",
  PTR: "Host name",
};

function RecordModal({
  zone,
  zoneId,
  types,
  checks,
  record,
  onClose,
  onSaved,
}: {
  zone: string;
  zoneId: number;
  types: string[];
  checks: HealthCheck[];
  record: DNSRecord | null;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [type, setType] = useState(record?.type ?? "A");
  const [name, setName] = useState(record?.name ?? "@");
  const [value, setValue] = useState(record?.value ?? "");
  const [ttl, setTTL] = useState(String(record?.ttl ?? 3600));
  const [priority, setPriority] = useState(String(record?.priority ?? (type === "MX" ? 10 : 0)));
  const [weight, setWeight] = useState(String(record?.weight ?? 0));
  const [port, setPort] = useState(String(record?.port ?? ""));
  const [tag, setTag] = useState(record?.tag ?? "issue");
  const [critical, setCritical] = useState((record?.flags ?? 0) === 128);
  const [check, setCheck] = useState(record?.health_check ?? 0);
  const address = type === "A" || type === "AAAA";
  const save = useMutation({
    mutationFn: () => {
      const r: DNSRecordInput = { name: name.trim() || "@", type, ttl: Number(ttl), value };
      if (type === "MX" || type === "SRV") r.priority = Number(priority) || 0;
      if (type === "SRV") {
        r.weight = Number(weight) || 0;
        r.port = Number(port) || 0;
      }
      if (type === "CAA") {
        r.tag = tag;
        r.flags = critical ? 128 : 0;
      }
      if (address && check) r.health_check = check;
      return record ? api.updateRecord(zoneId, record.id, r) : api.createRecord(zoneId, r);
    },
    onSuccess: () => {
      onSaved();
      onClose();
    },
  });
  const custom = !ttlChoices.some(([s]) => String(s) === ttl);
  return (
    <Modal
      title={record ? "Edit record" : "Add record"}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" form="record-form" disabled={save.isPending}>
            Save
          </button>
        </>
      }
    >
      <form
        id="record-form"
        className="form"
        onSubmit={(e: FormEvent) => {
          e.preventDefault();
          save.mutate();
        }}
      >
        <ErrorBox error={save.error} />
        <div className="grid-2">
          <Field label="Type">
            <select
              className="input"
              value={type}
              onChange={(e) => {
                setType(e.target.value);
                if (e.target.value === "MX" && priority === "0") setPriority("10");
              }}
            >
              {types.map((t) => (
                <option key={t}>{t}</option>
              ))}
            </select>
          </Field>
          <Field label="TTL">
            <select className="input" value={ttl} onChange={(e) => setTTL(e.target.value)}>
              {ttlChoices.map(([s, label]) => (
                <option key={s} value={s}>
                  {label}
                </option>
              ))}
              {custom && <option value={ttl}>{formatTTL(Number(ttl))}</option>}
            </select>
          </Field>
        </div>
        <Field label="Name" help={<>@ is {zone} itself; * matches any name. Full name: <code>{name.trim() === "@" || !name.trim() ? zone : `${name.trim()}.${zone}`}</code></>}>
          <input className="input mono" value={name} onChange={(e) => setName(e.target.value)} placeholder="@" autoFocus={!record} />
        </Field>
        {(type === "MX" || type === "SRV") && (
          <div className="grid-2">
            <Field label="Priority" help="Lower is preferred">
              <input className="input" type="number" min={0} max={65535} value={priority} onChange={(e) => setPriority(e.target.value)} />
            </Field>
            {type === "SRV" && (
              <Field label="Weight">
                <input className="input" type="number" min={0} max={65535} value={weight} onChange={(e) => setWeight(e.target.value)} />
              </Field>
            )}
          </div>
        )}
        {type === "SRV" && (
          <Field label="Port">
            <input className="input" type="number" min={0} max={65535} value={port} onChange={(e) => setPort(e.target.value)} required />
          </Field>
        )}
        {type === "CAA" && (
          <div className="grid-2">
            <Field label="Tag">
              <select className="input" value={tag} onChange={(e) => setTag(e.target.value)}>
                {["issue", "issuewild", "iodef", "issuemail", "issuevmc"].map((t) => (
                  <option key={t}>{t}</option>
                ))}
              </select>
            </Field>
            <Field label="Critical">
              <label className="row">
                <input type="checkbox" checked={critical} onChange={(e) => setCritical(e.target.checked)} /> flag 128
              </label>
            </Field>
          </div>
        )}
        {address && (
          <Field
            label="Health check"
            help={
              checks.length === 0 ? (
                <>Add one under <Link to="/monitoring">Monitoring</Link> to serve this address only while it answers.</>
              ) : (
                "While the check is down, this address is left out of the answers (unless every address of this name is down). Use a short TTL."
              )
            }
          >
            <select
              className="input"
              value={check}
              onChange={(e) => {
                const v = Number(e.target.value);
                setCheck(v);
                if (v && Number(ttl) > 300) setTTL("60");
              }}
            >
              <option value={0}>None: always served</option>
              {checks.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name} ({c.status})
                </option>
              ))}
            </select>
          </Field>
        )}
        <Field label={type === "TXT" ? "Text" : "Value"} help={valueHelp[type]}>
          {type === "TXT" ? (
            <textarea className="input mono" rows={3} value={value} onChange={(e) => setValue(e.target.value)} required />
          ) : (
            <input className="input mono" value={value} onChange={(e) => setValue(e.target.value)} required />
          )}
        </Field>
      </form>
    </Modal>
  );
}
