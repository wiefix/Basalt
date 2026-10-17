import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { ErrorBox, Icon, Loading, formatBytes } from "./ui";

const statusClass = (s: number) => (s >= 500 ? "err" : s >= 400 ? "warn" : "ok");

// AccessLogCard shows the latest requests the proxy served for a site or
// an app, newest first.
export function AccessLogCard({ base }: { base: string }) {
  const log = useQuery({ queryKey: ["access-log", base], queryFn: () => api.accessLog(base), refetchInterval: 15_000 });
  return (
    <div className="card">
      <div className="card-head">
        <h2>Requests</h2>
        <button className="btn sm" onClick={() => log.refetch()} disabled={log.isFetching}>
          <Icon name="refresh" size={14} /> Refresh
        </button>
      </div>
      {log.isPending ? (
        <Loading />
      ) : log.error ? (
        <div className="card-body">
          <ErrorBox error={log.error} />
        </div>
      ) : log.data.entries.length === 0 ? (
        <div className="empty small">No requests logged yet.</div>
      ) : (
        <div className="table-wrap" style={{ maxHeight: 420, overflowY: "auto" }}>
          <table className="table">
            <thead>
              <tr>
                <th>Time</th>
                <th>Request</th>
                <th className="num">Status</th>
                <th className="num hide-sm">Size</th>
                <th className="num hide-sm">Time, ms</th>
                <th className="hide-sm">Client</th>
              </tr>
            </thead>
            <tbody>
              {log.data.entries.map((e, i) => (
                <tr key={i}>
                  <td className="muted small mono" style={{ whiteSpace: "nowrap" }}>
                    {new Date(e.at).toLocaleTimeString()}
                  </td>
                  <td className="small" style={{ overflowWrap: "anywhere" }}>
                    <span className="mono">{e.method}</span> <span className="mono">{e.uri}</span>
                    <div className="faint">{e.host}</div>
                  </td>
                  <td className="num">
                    <span className={`badge ${statusClass(e.status)}`}>{e.status}</span>
                  </td>
                  <td className="num muted small hide-sm">{formatBytes(e.bytes)}</td>
                  <td className="num muted small hide-sm">{e.duration_ms.toFixed(1)}</td>
                  <td className="muted small hide-sm" title={e.user_agent}>
                    <span className="mono">{e.ip}</span>
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
