ALTER TABLE servers ADD COLUMN proxy_revision INTEGER NOT NULL DEFAULT 0;
-- Outcome of the last attempt to deliver proxy configuration to the agent.
ALTER TABLE servers ADD COLUMN proxy_applied_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE servers ADD COLUMN proxy_error TEXT NOT NULL DEFAULT '';

CREATE TABLE proxy_routes (
    id         INTEGER PRIMARY KEY,
    server_id  INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL CHECK (kind IN ('static', 'reverse_proxy')),
    root       TEXT    NOT NULL DEFAULT '',
    upstream   TEXT    NOT NULL DEFAULT '',
    tls        TEXT    NOT NULL CHECK (tls IN ('auto', 'internal')),
    hsts       INTEGER NOT NULL DEFAULT 0 CHECK (hsts IN (0, 1)),
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
) STRICT;

CREATE INDEX proxy_routes_server ON proxy_routes (server_id);

-- A domain can be served by only one route per server.
CREATE TABLE proxy_route_domains (
    server_id INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    domain    TEXT    NOT NULL,
    route_id  INTEGER NOT NULL REFERENCES proxy_routes (id) ON DELETE CASCADE,
    PRIMARY KEY (server_id, domain)
) STRICT, WITHOUT ROWID;

CREATE INDEX proxy_route_domains_route ON proxy_route_domains (route_id);
