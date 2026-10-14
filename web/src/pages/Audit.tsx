import { useState } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { api, can, type AuditEntry, type User } from "../api";
import { ErrorBox, Icon, Loading, formatDate } from "../components/ui";

// detailText shows an entry's details as key: value pairs.
const detailText = (d: Record<string, unknown>) =>
  Object.entries(d)
    .filter(([, v]) => v !== "" && v !== null)
    .map(([k, v]) => `${k}: ${typeof v === "object" ? JSON.stringify(v) : String(v)}`)
    .join(" · ");

export function AuditPage({ user }: { user: User }) {
  const admin = can(user, "admin");
  const [action, setAction] = useState("");
  const [actor, setActor] = useState("");
  const users = useQuery({ queryKey: ["users"], queryFn: api.users, enabled: admin });
  const log = useInfiniteQuery({
    queryKey: ["audit", action, actor],
    queryFn: ({ pageParam }) => api.audit({ before: pageParam, action, actor }),
    initialPageParam: 0,
    getNextPageParam: (last) => (last.more && last.entries.length ? last.entries[last.entries.length - 1].id : undefined),
    enabled: admin,
  });
  const check = useQuery({ queryKey: ["audit-verify"], queryFn: api.verifyAudit, enabled: admin, staleTime: 60_000 });
  if (!admin) return <div className="empty">You do not have access to this page.</div>;

  const actions = log.data?.pages[0]?.actions ?? [];
  const groups = [...new Set(actions.map((a) => a.split(".")[0]))];
  const entries: AuditEntry[] = log.data?.pages.flatMap((p) => p.entries) ?? [];
  const broken = check.data?.intact === false ? check.data.broken_at : undefined;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Audit log</h1>
          <div className="sub">Who changed what, and when. Secrets are never written here.</div>
        </div>
      </div>
      <div className="stack">
        <ChainCard check={check.data} error={check.error} pending={check.isFetching} onCheck={() => check.refetch()} />
        <div className="card">
          <div className="card-body row wrap">
            <select className="input" style={{ width: "auto", minWidth: 200 }} value={action} onChange={(e) => setAction(e.target.value)}>
              <option value="">All actions</option>
              {groups.map((g) => (
                <optgroup key={g} label={g}>
                  <option value={`${g}.`}>{g}.* (all)</option>
                  {actions
                    .filter((a) => a.startsWith(`${g}.`))
                    .map((a) => (
                      <option key={a} value={a}>
                        {a}
                      </option>
                    ))}
                </optgroup>
              ))}
            </select>
            <select className="input" style={{ width: "auto", minWidth: 200 }} value={actor} onChange={(e) => setActor(e.target.value)}>
              <option value="">Anyone</option>
              <option value="system">The panel itself (schedules, webhooks)</option>
              {users.data?.users.map((u) => (
                <option key={u.id} value={String(u.id)}>
                  {u.name} · {u.email}
                </option>
              ))}
            </select>
          </div>
          {log.isPending ? (
            <Loading />
          ) : log.error ? (
            <div className="card-body">
              <ErrorBox error={log.error} />
            </div>
          ) : entries.length === 0 ? (
            <div className="empty small">Nothing recorded{action || actor ? " for this filter" : " yet"}.</div>
          ) : (
            <div className="table-wrap">
              <table className="table">
                <thead>
                  <tr>
                    <th>When</th>
                    <th>Who</th>
                    <th>Action</th>
                    <th>Details</th>
                    <th className="hide-sm">IP</th>
                  </tr>
                </thead>
                <tbody>
                  {entries.map((e) => (
                    <tr key={e.id} className={e.id === broken ? "selected" : undefined}>
                      <td className="muted small" style={{ whiteSpace: "nowrap" }} title={`#${e.id}`}>
                        {formatDate(e.at)}
                      </td>
                      <td className="small">
                        {e.actor ? (
                          e.actor.name ? (
                            <span title={e.actor.email}>{e.actor.name}</span>
                          ) : (
                            <span className="faint">user #{e.actor.id} (deleted)</span>
                          )
                        ) : (
                          <span className="faint">panel</span>
                        )}
                      </td>
                      <td className="mono small" style={{ whiteSpace: "nowrap" }}>
                        {e.action}
                        {e.id === broken && (
                          <span className="badge err" style={{ marginLeft: 6 }}>
                            changed
                          </span>
                        )}
                      </td>
                      <td className="small muted" style={{ overflowWrap: "anywhere" }}>
                        {detailText(e.details)}
                      </td>
                      <td className="mono small faint hide-sm">{e.ip}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {log.hasNextPage && (
            <div className="card-body" style={{ textAlign: "center" }}>
              <button className="btn sm" onClick={() => log.fetchNextPage()} disabled={log.isFetchingNextPage}>
                {log.isFetchingNextPage ? "Loading…" : "Load older"}
              </button>
            </div>
          )}
        </div>
      </div>
    </>
  );
}

function ChainCard({
  check,
  error,
  pending,
  onCheck,
}: {
  check?: { entries: number; legacy: number; intact: boolean; broken_at?: number; at: string };
  error: unknown;
  pending: boolean;
  onCheck: () => void;
}) {
  return (
    <div className="card card-body stack" style={{ gap: 10 }}>
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <div className="row wrap">
          <Icon name="shield" />
          <h2>Integrity</h2>
          {check &&
            (check.intact && check.entries > 0 && check.legacy === check.entries ? (
              <span className="badge plain">nothing signed yet</span>
            ) : check.intact ? (
              <span className="badge ok">intact</span>
            ) : (
              <span className="badge err">changed at entry #{check.broken_at}</span>
            ))}
          {check && <span className="faint small">checked {formatDate(check.at)}</span>}
        </div>
        <button className="btn sm" onClick={onCheck} disabled={pending}>
          {pending ? "Checking…" : "Check now"}
        </button>
      </div>
      <ErrorBox error={error} />
      {check && !check.intact && (
        <div className="alert err small">
          Entry #{check.broken_at} does not match its signature: it, or an entry before it, was changed or removed in the
          database outside the panel. Find out who has access to the panel's server and its database.
        </div>
      )}
      <p className="faint small" style={{ margin: 0 }}>
        Each entry is signed together with the one before it, with a key derived from the panel's master key (which is
        not in the database). Changing or removing an entry breaks the chain; the panel checks it daily and notifies
        (event “The audit log was changed”).
        {check && check.legacy > 0
          ? check.legacy === check.entries
            ? ` The ${check.entries} entries so far were written before signing existed and are not protected; new ones are.`
            : ` Of ${check.entries} entries, the first ${check.legacy} were written before signing existed and are not protected.`
          : check
            ? ` ${check.entries} entries, all signed.`
            : ""}
      </p>
    </div>
  );
}
