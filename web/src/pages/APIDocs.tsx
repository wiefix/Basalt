import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";
import { api, type OpenAPISpec } from "../api";
import { ErrorBox, Loading } from "../components/ui";

type Schema = {
  type?: string | string[];
  $ref?: string;
  oneOf?: Schema[];
  items?: Schema;
  properties?: Record<string, Schema>;
  additionalProperties?: Schema | boolean;
  format?: string;
  required?: string[];
};

// typeText shows a schema briefly: string, integer[], Record{…}, object name.
function typeText(s: Schema | undefined): string {
  if (!s) return "any";
  if (s.oneOf) return s.oneOf.map(typeText).join(" or ");
  if (Array.isArray(s.type)) return s.type.map((t) => typeText({ ...s, type: t })).join(" or ");
  if (s.$ref) return s.$ref.replace("#/components/schemas/", "");
  if (s.type === "array") return `${typeText(s.items)}[]`;
  if (s.type === "object" && s.additionalProperties && typeof s.additionalProperties === "object")
    return `map of ${typeText(s.additionalProperties)}`;
  return s.format ? `${s.type} (${s.format})` : s.type ?? "any";
}

function Fields({ schema, spec, depth = 0 }: { schema: Schema; spec: OpenAPISpec; depth?: number }) {
  const resolved: Schema = schema.$ref ? (spec.components.schemas[schema.$ref.replace("#/components/schemas/", "")] as Schema) : schema;
  const props = Object.entries(resolved?.properties ?? {});
  if (props.length === 0) return <span className="faint">{typeText(schema)}</span>;
  // Responses list the fields always present; request fields are all optional.
  const required = resolved?.required;
  return (
    <ul className="steps" style={{ listStyle: "none", paddingLeft: depth ? 16 : 0 }}>
      {props.map(([name, s]) => {
        const nested = s.$ref ?? s.items?.$ref ?? s.oneOf?.find((x) => x.$ref)?.$ref;
        const inline = !nested && (s.properties ? s : s.items?.properties ? s.items : undefined);
        return (
          <li key={name}>
            <code>{name}</code> <span className="faint small">{typeText(s)}</span>
            {required && !required.includes(name) && <span className="faint small"> · optional</span>}
            {nested && depth < 2 && <Fields schema={{ $ref: nested }} spec={spec} depth={depth + 1} />}
            {inline && depth < 2 && <Fields schema={inline} spec={spec} depth={depth + 1} />}
          </li>
        );
      })}
    </ul>
  );
}

const methodBadge: Record<string, string> = { get: "badge ok", post: "badge warn", put: "badge warn", delete: "badge err" };

export function APIDocsPage() {
  const spec = useQuery({ queryKey: ["openapi"], queryFn: api.openapi, staleTime: Infinity });
  const [filter, setFilter] = useState("");
  const [open, setOpen] = useState<string | null>(null);
  if (spec.isPending) return <Loading />;
  if (spec.error) return <ErrorBox error={spec.error} />;
  const s = spec.data;
  const f = filter.trim().toLowerCase();
  const ops = Object.entries(s.paths)
    .flatMap(([path, methods]) => Object.entries(methods).map(([method, op]) => ({ path, method, op })))
    .filter((x) => !f || x.path.includes(f) || x.op.summary.toLowerCase().includes(f) || x.op.tags[0].includes(f));
  const tags = [...new Set(ops.map((x) => x.op.tags[0]))].sort();
  return (
    <>
      <div className="page-head">
        <div>
          <h1>API</h1>
          <div className="sub">
            The API this panel uses, version {s.info.version}. Authenticate with an API token from{" "}
            <Link to="/account">Account</Link>: <code>Authorization: Bearer bst_…</code>
          </div>
        </div>
        <a className="btn" href="/api/v1/openapi.json" download="basalt-openapi.json">
          OpenAPI file
        </a>
      </div>
      <input className="input" style={{ maxWidth: 320, marginBottom: 16 }} placeholder="Filter" value={filter} onChange={(e) => setFilter(e.target.value)} />
      <div className="stack">
        {tags.map((tag) => (
          <div key={tag} className="card">
            <div className="card-head">
              <h2>{tag}</h2>
            </div>
            <div className="table-wrap">
              <table className="table">
                <tbody>
                  {ops
                    .filter((x) => x.op.tags[0] === tag)
                    .sort((a, b) => a.path.localeCompare(b.path) || a.method.localeCompare(b.method))
                    .map(({ path, method, op }) => {
                      const key = `${method} ${path}`;
                      const body = op.requestBody?.content?.["application/json"]?.schema as Schema | undefined;
                      const raw = op.requestBody?.content?.["application/octet-stream"];
                      const ok = op.responses?.["2XX"];
                      const respSchema = ok?.content?.["application/json"]?.schema as Schema | undefined;
                      return [
                        <tr key={key} className="clickable" onClick={() => setOpen(open === key ? null : key)}>
                          <td style={{ width: 1 }}>
                            <span className={`${methodBadge[method] ?? "badge"} plain`}>{method.toUpperCase()}</span>
                          </td>
                          <td className="mono small" style={{ overflowWrap: "anywhere" }}>
                            {path}
                          </td>
                          <td className="small">
                            {op.summary}
                            {op["x-basalt-role"] && <span className="faint"> · {op["x-basalt-role"]}</span>}
                          </td>
                        </tr>,
                        open === key && (
                          <tr key={`${key}-detail`}>
                            <td colSpan={3}>
                              <div className="stack small" style={{ gap: 8 }}>
                                <div className="muted">{op.description}</div>
                                {op.parameters?.some((p) => p.in === "query") && (
                                  <div>
                                    <strong>Query</strong>
                                    <ul className="steps" style={{ listStyle: "none", paddingLeft: 0 }}>
                                      {op.parameters
                                        .filter((p) => p.in === "query")
                                        .map((p) => (
                                          <li key={p.name}>
                                            <code>{p.name}</code> <span className="faint">{p.description}</span>
                                          </li>
                                        ))}
                                    </ul>
                                  </div>
                                )}
                                {body && (
                                  <div>
                                    <strong>JSON body</strong>
                                    <Fields schema={body} spec={s} />
                                  </div>
                                )}
                                {raw && <div><strong>Body</strong> <span className="faint">the file's bytes</span></div>}
                                <div>
                                  <strong>Response</strong>{" "}
                                  {respSchema ? (
                                    <Fields schema={respSchema} spec={s} />
                                  ) : (
                                    <span className="faint">{op.responses?.["204"] ? "no content (204)" : ok?.description}</span>
                                  )}
                                </div>
                              </div>
                            </td>
                          </tr>
                        ),
                      ];
                    })}
                </tbody>
              </table>
            </div>
          </div>
        ))}
      </div>
    </>
  );
}
